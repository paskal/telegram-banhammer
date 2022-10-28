package main

import (
	"context"
	"fmt"
	"os"
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
const maxWait = time.Minute
const participantsRequestLimit = 100

func main() {
	var opts options
	if _, err := flags.Parse(&opts); err != nil {
		os.Exit(1)
	}
	setupLog(opts.Dbg)
	log.Printf("[DEBUG] starting telegram-banhammer %s", revision)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// prevent getting banned by floodwait
	waiter := floodwait.NewWaiter().WithMaxRetries(maxRetries).WithMaxWait(maxWait)
	go waiter.Run(ctx)

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

		log.Printf("[INFO] logging in")
		if err := authenticate(ctx, opts.Phone, opts.Password, client); err != nil {
			return err
		}

		log.Printf("[INFO] retrieving the channel information")
		channel, err := getChannel(ctx, api, opts.ChannelID)
		if err != nil {
			return err
		}

		banTo := time.Unix(opts.BanTo, 0)
		banFrom := banTo.Add(-opts.BanSearchDuration)
		log.Printf("[INFO] looking for users to ban who joined in %s between %s and %s", opts.BanSearchDuration, banFrom, banTo)

		// Buffered channel with users to ban
		nottyList := make(chan *tg.ChannelParticipant, 10)

		go func() {
			e := getChannelMembersWithinTimeframe(ctx, api, channel, banFrom, banTo, nottyList)
			close(nottyList)
			if e != nil {
				log.Printf("[ERROR] error getting channel members: %v", err)
			}
		}()

		banUsers(ctx, api, channel, nottyList, opts.NotDryRun)

		return nil
	}); err != nil {
		log.Printf("[ERROR] Error running the Telegram Banhammer: %s", err)
	}
}

func banUsers(ctx context.Context, api *tg.Client, channel *tg.InputChannel, users <-chan *tg.ChannelParticipant, notDryRun bool) {
	for {
		select {
		case userToBan, ok := <-users:
			if !ok {
				return
			}
			log.Printf("[INFO] user to ban: %d", userToBan.UserID)
			// TODO: search for the user messages only in the channel
			// messages, err := api.MessagesSearch(ctx, &tg.MessagesSearchRequest{
			// 	FromID: &tg.InputPeerUser{UserID: userToBan.UserID},
			// })
			// if err != nil {
			// 	log.Printf("[ERROR] error retrieving user %d messages: %v", userToBan.UserID, err)
			// }
			if notDryRun {
				// TODO delete found messages
				_, err := api.ChannelsEditBanned(ctx, &tg.ChannelsEditBannedRequest{
					Channel:     channel,
					Participant: &tg.InputPeerUser{UserID: userToBan.UserID},
					BannedRights: tg.ChatBannedRights{
						ViewMessages: true,
						InviteUsers:  true,
						UntilDate:    0, // forever
					},
				})
				if err != nil {
					log.Printf("[ERROR] error banning user %d: %v", userToBan.UserID, err)
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

func getChannelMembersWithinTimeframe(ctx context.Context, api *tg.Client, channel *tg.InputChannel, banFrom, banTo time.Time, users chan<- *tg.ChannelParticipant) error {
	var offset int
	for {
		participants, err := api.ChannelsGetParticipants(ctx,
			&tg.ChannelsGetParticipantsRequest{
				Channel: channel,
				Filter:  &tg.ChannelParticipantsRecent{},
				Limit:   participantsRequestLimit,
				Offset:  offset,
			})
		offset += participantsRequestLimit
		if err != nil {
			return fmt.Errorf("error getting list of channel participants: %w", err)
		}
		if len(participants.(*tg.ChannelsChannelParticipants).Participants) == 0 {
			log.Printf("[INFO] no more users to process")
			break
		}
		for _, participant := range participants.(*tg.ChannelsChannelParticipants).Participants {
			switch v := participant.(type) {
			case *tg.ChannelParticipant:
				p := participant.(*tg.ChannelParticipant)
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
		log.Printf("[INFO] processed %d users", offset)
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
func getChannel(ctx context.Context, api *tg.Client, channelID int64) (*tg.InputChannel, error) {
	channelInfo, err := api.ChannelsGetChannels(ctx, []tg.InputChannelClass{&tg.InputChannel{ChannelID: channelID}})
	if err != nil {
		return nil, fmt.Errorf("error retrieving channel by id: %w", err)
	}
	if len(channelInfo.GetChats()) != 1 {
		return nil, fmt.Errorf("couldn't get the chat info, got %v", channelInfo.GetChats())
	}
	var chat *tg.InputChannel
	switch v := channelInfo.GetChats()[0].(type) {
	case *tg.Channel:
		chat = &tg.InputChannel{
			ChannelID:  channelInfo.GetChats()[0].(*tg.Channel).ID,
			AccessHash: channelInfo.GetChats()[0].(*tg.Channel).AccessHash,
		}
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
