package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	log "github.com/go-pkgz/lgr"
	"github.com/gotd/td/tg"
)

const requestLimit = 100 // should be between 1 and 100

// banUserInfo stores all the information about a user to ban
type banUserInfo struct {
	userID     int64
	accessHash int64
	joined     time.Time
	message    string
	username   string
	firstName  string
	lastName   string
}

type channelParticipantInfo struct {
	participantInfo *tg.ChannelParticipant
	info            *tg.User
}

type searchParams struct {
	endUnixTime    int64
	duration       time.Duration
	offset         int
	limit          int
	ignoreMessages bool
}

// retrieves users by for given period and write them to file in ./ban directory
func searchAndStoreUsersToBan(ctx context.Context, api *tg.Client, channel *tg.Channel, params searchParams) {
	banTo := time.Unix(params.endUnixTime, 0)
	banFrom := banTo.Add(-params.duration)
	log.Printf("[INFO] Looking for users to ban who joined in %s between %s and %s", params.duration, banFrom, banTo)

	// Buffered channel with users to ban
	nottyList := make(chan channelParticipantInfo, requestLimit)
	go getChannelMembersWithinTimeframe(ctx, api, channel, banFrom, banTo, params.offset, params.limit, nottyList)

	fileName := fmt.Sprintf("./ban/%s.users.csv", time.Now().Format("2006-01-02T15-04-05"))

	usersToBan := getUsersInfo(ctx, api, channel, nottyList, params.ignoreMessages)
	if len(usersToBan) == 0 {
		log.Printf("[INFO] No users to ban found")
		return
	}
	if err := writeUsersToFile(usersToBan, fileName); err != nil {
		log.Printf("[ERROR] Error writing users to ban to file: %v", err)
	} else {
		log.Printf("[INFO] Success, users to ban written to %s", fileName)
		log.Printf("[INFO] Please review, and to ban run same command with the following flag:")
		log.Printf("[INFO] --ban-and-kick-filepath %s", fileName)
	}
}

// getSingleUserStoreInfo retrieves userID and joined date for users in given period and pushes them to users channel,
// closes provided channel before returning, supposed to be run in goroutine.
// Uses provided offset: Telegram sort seems to be stable so once you established there are no droids here,
// you can just add offset to always start from the point after the filtered users.
func getChannelMembersWithinTimeframe(ctx context.Context, api *tg.Client, channel *tg.Channel, banFrom, banTo time.Time, offset, searchLimit int, users chan<- channelParticipantInfo) {
	defer close(users)
	for {
		if searchLimit != 0 && offset >= searchLimit {
			break
		}
		participants, err := api.ChannelsGetParticipants(ctx,
			&tg.ChannelsGetParticipantsRequest{
				Channel: channel.AsInput(),
				Filter:  &tg.ChannelParticipantsRecent{},
				Limit:   requestLimit,
				Offset:  offset,
			})
		offset += requestLimit
		if err != nil {
			log.Printf("[ERROR] Error getting channel participants: %v", err)
			break
		}
		if len(participants.(*tg.ChannelsChannelParticipants).Participants) == 0 {
			log.Printf("[INFO] No more users to process")
			break
		}
		for _, participant := range participants.(*tg.ChannelsChannelParticipants).Participants {
			if p, ok := participant.(*tg.ChannelParticipant); ok {
				joinTime := time.Unix(int64(p.Date), 0)
				if joinTime.After(banFrom) && joinTime.Before(banTo) {
					// retrieve user info searches over all retrieved users in the latest bunch
					// O(N^2) but N is small (100)
					for _, u := range participants.(*tg.ChannelsChannelParticipants).GetUsers() {
						if u.GetID() == p.GetUserID() {
							// ignore error as then we couldn't do anything about it anyway
							if user, ok := u.(*tg.User); ok {
								// there is no point in writing to channel if we can't get user info
								// as without access hash we can't ban user
								users <- channelParticipantInfo{participantInfo: p, info: user}
							}
							break
						}
					}
				}
			}
		}
		log.Printf("[INFO] Processed %d users", offset)
	}
}

// getUsersInfo retrieves extended user info for every user in given channel, as well as single message sent by such user
func getUsersInfo(ctx context.Context, api *tg.Client, channel *tg.Channel, users <-chan channelParticipantInfo, ignoreMessages bool) []banUserInfo {
	var members []banUserInfo
	// Do not check for ctx.Done() because then we could store existing data about the user as-is and write it to a file
	// instead of dropping the information which we already retrieved. That is achieved by closing users channel.
	for {
		userToBan, ok := <-users
		if !ok {
			break
		}
		userInfoToStore := getSingleUserStoreInfo(ctx, api, channel, userToBan, ignoreMessages)
		members = append(members, userInfoToStore)
	}
	log.Printf("[INFO] %d users found", len(members))
	// sort members by joined date
	sort.Slice(members, func(i, j int) bool {
		return members[i].joined.Before(members[j].joined)
	})
	return members
}

// getSingleUserStoreInfo retrieves extended user information for given user and returns filled banUserInfo
func getSingleUserStoreInfo(ctx context.Context, api *tg.Client, channel *tg.Channel, userToBan channelParticipantInfo, ignoreMessages bool) banUserInfo {
	joined := time.Unix(int64(userToBan.participantInfo.Date), 0)
	userInfoToStore := banUserInfo{
		userID: userToBan.participantInfo.UserID,
		joined: joined,
	}
	userInfoStr := "user to ban"
	if userToBan.info.Username != "" {
		userInfoStr += fmt.Sprintf(" @%s (%s %s) joined %s",
			userToBan.info.Username,
			userToBan.info.FirstName,
			userToBan.info.LastName,
			joined)
	} else {
		userInfoStr += fmt.Sprintf(" %s %s joined %s",
			userToBan.info.FirstName,
			userToBan.info.LastName,
			joined)
	}
	userInfoToStore.username = userToBan.info.Username
	userInfoToStore.firstName = userToBan.info.FirstName
	userInfoToStore.lastName = userToBan.info.LastName
	userInfoToStore.accessHash = userToBan.info.AccessHash

	var message string
	if !ignoreMessages {
		message = getSingeUserMessage(ctx, api, channel, userToBan.info.AsInputPeer())
		userInfoToStore.message = message
		if len([]rune(message)) > 50 {
			message = string([]rune(message)[:45]) + "... (truncated)"
		}
	}
	if message != "" {
		userInfoStr += fmt.Sprintf(", last message: %s", strings.ReplaceAll(message, "\n", " "))
	}
	if message == "" && !ignoreMessages {
		userInfoStr += ", no message found"
	}
	log.Printf("[INFO] %s", userInfoStr)
	return userInfoToStore
}

// getSingeUserMessage retrieves single user (last?) message from given channel from Telegram API
func getSingeUserMessage(ctx context.Context, api *tg.Client, channel *tg.Channel, user tg.InputPeerClass) string {
	var message string
	messages, err := api.MessagesSearch(ctx, &tg.MessagesSearchRequest{
		FromID: user,
		Peer:   channel.AsInputPeer(),
		Filter: &tg.InputMessagesFilterEmpty{},
		Limit:  1,
	})
	if err != nil {
		log.Printf("[ERROR] Error retrieving user %s message: %v", user.String(), err)
		return ""
	}
	if messages.Zero() {
		return ""
	}
	var rawMessages []tg.MessageClass
	switch v := messages.(type) {
	case *tg.MessagesMessages:
		rawMessages = v.Messages
	case *tg.MessagesMessagesSlice:
		rawMessages = v.Messages
	case *tg.MessagesChannelMessages:
		rawMessages = v.Messages
	}
	if len(rawMessages) == 1 {
		switch v := rawMessages[0].(type) {
		case *tg.Message:
			message = v.GetMessage()
		case *tg.MessageService:
			message = v.String()
			if _, ok := v.GetAction().(*tg.MessageActionChatAddUser); ok {
				message = "[system] joining the channel"
			}
		}
	}

	return message
}
