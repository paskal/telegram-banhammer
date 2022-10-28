package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	log "github.com/go-pkgz/lgr"
	"github.com/gotd/td/tg"
)

const participantsRequestLimit = 100 // should be between 1 and 100

// banUserInfo stores all the information about a user to ban
type banUserInfo struct {
	userID    int64
	joined    time.Time
	message   string
	username  string
	firstName string
	lastName  string
	langCode  string
}

// retrieves users by for given period and write them to file in ./ban directory
func searchAndStoreUsersToBan(ctx context.Context, api *tg.Client, channel *tg.Channel, banToUnixtime int64, banSearchDuration time.Duration) {
	banTo := time.Unix(banToUnixtime, 0)
	banFrom := banTo.Add(-banSearchDuration)
	log.Printf("[INFO] Looking for users to ban who joined in %s between %s and %s", banSearchDuration, banFrom, banTo)

	// Buffered channel with users to ban
	nottyList := make(chan *tg.ChannelParticipant, participantsRequestLimit)

	go func() {
		err := getChannelMembersWithinTimeframe(ctx, api, channel, banFrom, banTo, nottyList)
		close(nottyList)
		if err != nil {
			log.Printf("[ERROR] Error getting channel members: %v", err)
		}
	}()

	fileName := fmt.Sprintf("./ban/telegram-banhammer-%s.users.csv", time.Now().Format("2006-01-02T15-04-05"))

	usersToBan := getUsersInfo(ctx, api, channel, nottyList)
	if err := writeUsersToFile(usersToBan, fileName); err != nil {
		log.Printf("[ERROR] Error writing users to ban to file: %v", err)
	} else {
		log.Printf("[INFO] Success, users to ban written to %s", fileName)
	}
}

// getSingleUserStoreInfo retrieves userID and joined date for users in given period and pushes them to users channel,
// supposed to be run in goroutine
func getChannelMembersWithinTimeframe(ctx context.Context, api *tg.Client, channel *tg.Channel, banFrom, banTo time.Time, users chan<- *tg.ChannelParticipant) error {
	var offset int
	for {
		participants, err := api.ChannelsGetParticipants(ctx,
			&tg.ChannelsGetParticipantsRequest{
				Channel: channel.AsInput(),
				Filter:  &tg.ChannelParticipantsRecent{},
				Limit:   participantsRequestLimit,
				Offset:  offset,
			})
		offset += participantsRequestLimit
		if err != nil {
			return fmt.Errorf("error getting list of channel participants: %w", err)
		}
		if len(participants.(*tg.ChannelsChannelParticipants).Participants) == 0 {
			log.Printf("[INFO] No more users to process")
			break
		}
		for _, participant := range participants.(*tg.ChannelsChannelParticipants).Participants {
			switch v := participant.(type) {
			case *tg.ChannelParticipant:
				p := v
				joinTime := time.Unix(int64(p.Date), 0)
				if joinTime.After(banFrom) && joinTime.Before(banTo) {
					users <- p
				}
			case *tg.ChannelParticipantSelf:
			case *tg.ChannelParticipantCreator:
			case *tg.ChannelParticipantAdmin:
			case *tg.ChannelParticipantBanned:
			case *tg.ChannelParticipantLeft:
			default:
				log.Printf("[WARN] Unknown participant type: %T, %v", v, participant)
			}
		}
		log.Printf("[INFO] Processed %d users", offset)
	}
	return nil
}

// writeUsersToFile writes users to tab-separated csv file
func writeUsersToFile(users []banUserInfo, fileName string) error {
	file, err := os.Create(fileName)
	if err != nil {
		log.Printf("[ERROR] Error creating file %s: %v", fileName, err)
		log.Printf("[INFO] Writing results to stdout instead")
		file = os.Stdout
	} else {
		defer func() {
			e := file.Close()
			if e != nil {
				log.Printf("[ERROR] Error closing file %s: %v", fileName, e)
			}
		}()
	}

	data := [][]string{{"joined", "userID", "username", "firstName", "lastName", "langCode", "message"}}

	for _, user := range users {
		data = append(data, []string{
			user.joined.Format(time.RFC3339),              // joined
			fmt.Sprintf("%d", user.userID),                // userID
			user.username,                                 // username
			strings.ReplaceAll(user.firstName, "\t", " "), // firstName
			strings.ReplaceAll(user.lastName, "\t", " "),  // lastName
			user.langCode,                                 // langCode
			strings.ReplaceAll(user.message, "\t", " "),   // message
		})
	}

	writer := csv.NewWriter(file)
	writer.Comma = '\t' // use tab as separator
	defer writer.Flush()
	for _, value := range data {
		err = writer.Write(value)
		if err != nil {
			return fmt.Errorf("error writing row to csv: %v", err)
		}
	}
	return nil
}

// getUsersInfo retrieves extended user info for every user in given channel, as well as single message sent by such user
func getUsersInfo(ctx context.Context, api *tg.Client, channel *tg.Channel, users <-chan *tg.ChannelParticipant) []banUserInfo {
	var members []banUserInfo
	// Do not check for ctx.Done() because then we could store existing data about the user as-is and write it to a file
	// instead of dropping the information which we already retrieved. That is achieved by closing users channel.
	for {
		userToBan, ok := <-users
		if !ok {
			break
		}
		userInfoToStore := getSingleUserStoreInfo(ctx, api, channel, userToBan)
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
func getSingleUserStoreInfo(ctx context.Context, api *tg.Client, channel *tg.Channel, userToBan *tg.ChannelParticipant) banUserInfo {
	joined := time.Unix(int64(userToBan.Date), 0)
	userInfoToStore := banUserInfo{
		userID: userToBan.UserID,
		joined: joined,
	}
	userInfoStr := fmt.Sprintf("user to ban %d, joined %s", userToBan.UserID, joined)
	telegramUser := getTelegramUser(ctx, api, userToBan.UserID)
	if telegramUser != nil {
		userInfoStr = fmt.Sprintf("user to ban @%s (%s %s), joined %s",
			telegramUser.Username,
			telegramUser.FirstName,
			telegramUser.LastName,
			joined)
		userInfoToStore.username = telegramUser.Username
		userInfoToStore.firstName = telegramUser.FirstName
		userInfoToStore.lastName = telegramUser.LastName
		userInfoToStore.langCode = telegramUser.LangCode
	}

	message := getSingeUserMessage(ctx, api, channel, userToBan)
	if message != "" {
		userInfoToStore.message = message
		if len(message) > 80 {
			message = string([]rune(message)[:65]) + "... (truncated)"
		}
		userInfoStr += fmt.Sprintf(", last message: %s", strings.ReplaceAll(message, "\n", " "))
	} else {
		userInfoStr += ", no message found"
	}
	log.Printf("[INFO] %s", userInfoStr)
	return userInfoToStore
}

// getTelegramUser retrieves single user extended information from Telegram API
func getTelegramUser(ctx context.Context, api *tg.Client, userID int64) *tg.User {
	// TODO: doesn't work without User's AccessHash
	// userInfo, err := api.UsersGetUsers(ctx, []tg.InputUserClass{&tg.InputUser{UserID: userID}})
	// if err != nil {
	// 	log.Printf("[DEBUG] Error retrieving info for user %d: %v", userID, err)
	// 	return nil
	// }
	// if len(userInfo) != 1 {
	// 	return nil
	// }
	// switch v := userInfo[0].(type) {
	// case *tg.User:
	// 	return v
	// }
	return nil
}

// getSingeUserMessage retrieves single user (last?) message from given channel from Telegram API
func getSingeUserMessage(ctx context.Context, api *tg.Client, channel *tg.Channel, userToBan *tg.ChannelParticipant) string {
	var message string
	messages, err := api.MessagesSearch(ctx, &tg.MessagesSearchRequest{
		FromID: &tg.InputPeerUser{UserID: userToBan.UserID},
		Peer:   channel.AsInputPeer(),
		Filter: &tg.InputMessagesFilterEmpty{},
		Limit:  1,
	})
	if err != nil {
		log.Printf("[ERROR] Error retrieving user %d message: %v", userToBan.UserID, err)
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
		}
	}

	return message
}
