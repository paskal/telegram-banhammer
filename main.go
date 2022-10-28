package main

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	log "github.com/go-pkgz/lgr"
	"github.com/gotd/contrib/middleware/floodwait"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"github.com/jessevdk/go-flags"
	"go.uber.org/zap"
	"golang.org/x/term"
)

type options struct {
	AppID             int           `long:"appid" description:"AppID, https://core.telegram.org/api/obtaining_api_id" required:"true"`
	AppHash           string        `long:"apphash" description:"AppHash, https://core.telegram.org/api/obtaining_api_id" required:"true"`
	Phone             string        `long:"phone" description:"Telegram phone of the channel admin" required:"true"`
	Password          string        `long:"password" description:"password, if set for the admin"`
	ChannelID         int64         `long:"channel_id" description:"channel or supergroup id, without -100 part, https://gist.github.com/mraaroncruz/e76d19f7d61d59419002db54030ebe35" required:"true"`
	BanTo             int64         `long:"ban_to" description:"the end of the time from which newly joined users will be banned, unix timestamp" required:"true"`
	BanSearchDuration time.Duration `long:"ban_search_duration" description:"amount of time before the ban_to for which we need to ban users" required:"true"`

	NotDryRun bool `long:"not_dry_run" description:"unless this is set, only show what would be done, but don't actually do anything"`
	Dbg       bool `long:"dbg" description:"debug mode"`
}

var revision = "local"

const maxRetries = 3
const maxWait = time.Minute * 5
const participantsRequestLimit = 100

func main() {
	var opts options
	if _, err := flags.Parse(&opts); err != nil {
		os.Exit(1)
	}
	setupLog(opts.Dbg)
	log.Printf("[DEBUG] Starting telegram-banhammer %s", revision)

	// create "ban" directory if not exists
	if s, dirErr := os.Stat("./ban"); errors.Is(dirErr, os.ErrNotExist) {
		e := os.Mkdir("./ban", os.ModePerm)
		if e != nil {
			log.Fatalf("[ERROR] Error creating ./ban directory: %v", e)
		}
	} else if !s.IsDir() {
		log.Fatalf("[ERROR] ./ban is not a directory, please remove or rename that file")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// prevent getting banned by floodwait
	waiter := floodwait.NewWaiter().WithMaxRetries(maxRetries).WithMaxWait(maxWait)
	go func() {
		err := waiter.Run(ctx)
		if err != nil {
			log.Printf("[WARN] Waiter middleware failed: %v", err)
		}
	}()

	telegramOptions := telegram.Options{Middlewares: []telegram.Middleware{waiter}}

	// logging for Telegram library
	if opts.Dbg {
		if logger, err := zap.NewProduction(); err == nil {
			defer logger.Sync()
			telegramOptions.Logger = logger
		}
	}

	client := telegram.NewClient(
		opts.AppID,
		opts.AppHash,
		telegramOptions,
	)
	if err := client.Run(ctx, func(ctx context.Context) error {
		api := client.API()

		log.Printf("[INFO] Logging in")
		if err := authenticate(ctx, opts.Phone, opts.Password, client); err != nil {
			return err
		}

		log.Printf("[INFO] Retrieving the channel information")
		channel, err := getChannel(ctx, api, opts.ChannelID)
		if err != nil {
			return err
		}

		banTo := time.Unix(opts.BanTo, 0)
		banFrom := banTo.Add(-opts.BanSearchDuration)
		log.Printf("[INFO] Looking for users to ban who joined in %s between %s and %s", opts.BanSearchDuration, banFrom, banTo)

		// Buffered channel with users to ban
		nottyList := make(chan *tg.ChannelParticipant, 10)

		go func() {
			e := getChannelMembersWithinTimeframe(ctx, api, channel, banFrom, banTo, nottyList)
			close(nottyList)
			if e != nil {
				log.Printf("[ERROR] Error getting channel members: %v", err)
			}
		}()

		fileName := fmt.Sprintf("./ban/telegram-banhammer-%s.users.csv", time.Now().Format("2006-01-02T15-04-05"))

		usersToBan := getUsersInfo(ctx, api, channel, nottyList)
		err = writeUsersToFile(usersToBan, fileName)
		if err != nil {
			log.Printf("[ERROR] Error writing users to ban to file: %v", err)
		} else {
			log.Printf("[INFO] Success, users to ban written to %s", fileName)
		}

		return nil
	}); err != nil {
		log.Printf("[ERROR] Error running the Telegram Banhammer: %s", err)
	}

}

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

func getUsersInfo(ctx context.Context, api *tg.Client, channel *tg.Channel, users <-chan *tg.ChannelParticipant) []banUserInfo {
	var members []banUserInfo
	var done bool
	for {
		select {
		case userToBan, ok := <-users:
			if !ok {
				done = true
				break
			}
			userInfoToStore := getSingleUserStoreInfo(ctx, api, channel, userToBan)
			members = append(members, userInfoToStore)
		case <-ctx.Done():
			done = true
		}
		if done {
			break
		}
	}
	log.Printf("[INFO] %d users found", len(members))
	// sort members by joined date
	sort.Slice(members, func(i, j int) bool {
		return members[i].joined.Before(members[j].joined)
	})
	return members
}

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

func getSingeUserMessage(ctx context.Context, api *tg.Client, channel *tg.Channel, userToBan *tg.ChannelParticipant) string {
	var message string
	var rawMessages []tg.MessageClass
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

func authenticate(ctx context.Context, phone, password string, client *telegram.Client) error {
	// Function for getting second factor code from stdin
	codePrompt := func(ctx context.Context, sentCode *tg.AuthSentCode) (string, error) {
		fmt.Print("Enter code received from Telegram for login:")
		code, err := term.ReadPassword(syscall.Stdin)
		fmt.Print("\n")
		if err != nil {
			return "", fmt.Errorf("error reading code from the terminal: %w", err)
		}
		return string(code), nil
	}

	// If password is set, use it, otherwise rely only on the second factor code.
	userAuth := auth.CodeOnly(phone, auth.CodeAuthenticatorFunc(codePrompt))
	if password != "" {
		userAuth = auth.Constant(phone, password, auth.CodeAuthenticatorFunc(codePrompt))
	}
	// This will set up and perform authentication flow.
	if err := auth.NewFlow(userAuth, auth.SendCodeOptions{}).Run(ctx, client.Auth()); err != nil {
		return fmt.Errorf("error authenticating with the user: %w", err)
	}
	return nil
}

// getChannel returns tg.InputChannel with AccessHash doing search by ID
func getChannel(ctx context.Context, api *tg.Client, channelID int64) (*tg.Channel, error) {
	channelInfo, err := api.ChannelsGetChannels(ctx, []tg.InputChannelClass{&tg.InputChannel{ChannelID: channelID}})
	if err != nil {
		return nil, fmt.Errorf("error retrieving channel by id: %w", err)
	}
	if len(channelInfo.GetChats()) != 1 {
		return nil, fmt.Errorf("couldn't get the chat info, got %v", channelInfo.GetChats())
	}
	var chat *tg.Channel
	switch v := channelInfo.GetChats()[0].(type) {
	case *tg.Channel:
		chat = v
	default:
		return nil, fmt.Errorf("unknown chat type received: %T (expected Channel), %v", v, channelInfo.GetChats()[0])
	}
	return chat, nil
}

func setupLog(dbg bool) {
	if dbg {
		log.Setup(log.Debug, log.CallerFile, log.CallerFunc, log.Msec, log.LevelBraces, log.StackTraceOnError)
		return
	}
	log.Setup(log.Msec, log.LevelBraces, log.StackTraceOnError)
}
