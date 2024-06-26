package main

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"os/signal"
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
	AppID                int           `long:"appid" description:"AppID, https://core.telegram.org/api/obtaining_api_id" required:"true"`
	AppHash              string        `long:"apphash" description:"AppHash, https://core.telegram.org/api/obtaining_api_id" required:"true"`
	Phone                string        `long:"phone" description:"Telegram phone of the channel admin" required:"true"`
	Password             string        `long:"password" description:"password, if set for the admin"`
	ChannelID            int64         `long:"channel-id" description:"channel or supergroup id, without -100 part, https://gist.github.com/mraaroncruz/e76d19f7d61d59419002db54030ebe35" required:"true"`
	BanToTimestamp       int64         `long:"ban-to-timestamp" description:"the end of the time from which newly joined users will be banned, Unix timestamp"`
	BanToTime            string        `long:"ban-to-time" description:"the end of the time from which newly joined users will be banned, dd-mm-yyThh:mm:ss format (like 31-10-22T19:30:15), in your timezone"`
	BanSearchDuration    time.Duration `long:"ban-search-duration" description:"amount of time before the ban-to-timestamp for which we need to ban users"`
	BanSearchOffset      int           `long:"ban-search-offset" description:"starting offset of search, useful if you banned the offenders in first N users already"`
	BanSearchLimit       int           `long:"ban-search-limit" description:"limit of users to check for a ban, 0 is unlimited"`
	SearchIgnoreMessages bool          `long:"search-ignore-messages" description:"do not retrieve messages when searching for users to ban"`
	BanAndKickFilePath   string        `long:"ban-and-kick-filepath" description:"set this option to a path to a text file with users clean up their messages, ban and kick them"`

	Dbg bool `long:"dbg" description:"debug mode"`
}

var revision = "local"

// settings for Telegram API floodwait
const maxRetries = 3

// after banning 300 users in a row, Telegram API gives you cooldown timeout of ~12 minutes
const maxWait = time.Minute * 15

// that tick value allows ban not being painfully slow after 300 users
const tick = time.Second

func main() {
	var opts options
	if _, err := flags.Parse(&opts); err != nil {
		os.Exit(1)
	}
	setupLog(opts.Dbg)
	log.Printf("[INFO] Starting telegram-banhammer %s", revision)

	if err := ensureDirectoryExists("./ban"); err != nil {
		log.Fatalf("[FATAL] %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	// catch CTRL+C, will cancel the context and cause program to write what's processed to disk
	// https://medium.com/@matryer/make-ctrl-c-cancel-the-context-context-bd006a8ad6ff
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	defer func() {
		signal.Stop(c)
		cancel()
	}()
	go func() {
		select {
		case <-c:
			cancel()
		case <-ctx.Done():
		}
	}()
	// prevent getting banned by floodwait
	waiter := floodwait.NewWaiter().WithMaxRetries(maxRetries).WithMaxWait(maxWait).WithTick(tick)

	// credentials are stored per phone number
	telegramOptions := telegram.Options{
		Middlewares:    []telegram.Middleware{waiter},
		SessionStorage: &telegram.FileSessionStorage{Path: fmt.Sprintf("./ban/%s.json", opts.Phone)},
	}

	// logging for Telegram library
	if opts.Dbg {
		if logger, err := zap.NewProduction(); err == nil {
			defer func() {
				e := logger.Sync()
				if err != nil {
					log.Printf("[WARN] Logger sync failed: %v", e)
				}
			}()
			telegramOptions.Logger = logger
		}
	}

	client := telegram.NewClient(
		opts.AppID,
		opts.AppHash,
		telegramOptions,
	)

	handler := func(ctx context.Context) error {
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

		// ban users case
		if opts.BanAndKickFilePath != "" {
			banAndKickUsers(ctx, api, channel, opts.BanAndKickFilePath)
			return nil
		}

		// retrieve users to ban case
		banTo := time.Time{}
		if opts.BanToTimestamp != 0 {
			banTo = time.Unix(opts.BanToTimestamp, 0)
		}
		if opts.BanToTime != "" {
			banTo, err = time.ParseInLocation("02-01-06T15:04:05", opts.BanToTime, time.Local)
			if err != nil {
				log.Printf("[ERROR] can't parse ban-to-time: %v", err)
				return nil
			}
		}
		if banTo.IsZero() {
			log.Printf("[ERROR] ban-to-timestamp or ban-to-time must be set when searching for users")
			return nil
		}
		if opts.BanSearchDuration.Seconds() <= 0 {
			log.Printf("[ERROR] ban-search-duration must be non-zero when searching for users")
			return nil
		}
		searchAndStoreUsersToBan(ctx, api, channel, searchParams{
			banTo:          banTo,
			duration:       opts.BanSearchDuration,
			offset:         opts.BanSearchOffset,
			limit:          opts.BanSearchLimit,
			ignoreMessages: opts.SearchIgnoreMessages,
		})

		return nil
	}

	err := waiter.Run(ctx, func(ctx context.Context) error {
		return client.Run(ctx, handler)
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("[ERROR] Error running the Telegram Banhammer: %s", err)
	}
}

// ensureDirectoryExists ensures the directory exists, creates it if it doesn't,
// and returns error in case of problem creating it or if specified path is not a directory
func ensureDirectoryExists(dir string) error {
	if s, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		e := os.Mkdir(dir, os.ModePerm)
		if e != nil {
			return fmt.Errorf("error creating %s directory: %w", dir, e)
		}
	} else if !s.IsDir() {
		return fmt.Errorf("%s is not a directory, please remove or rename that file", dir)
	}
	return nil
}

// authenticate the user. If password is not empty, it will be used.
// Second factor code would be requested in any case.
func authenticate(ctx context.Context, phone, password string, client *telegram.Client) error {
	// Function for getting second factor code from stdin
	codePrompt := func(_ context.Context, _ *tg.AuthSentCode) (string, error) {
		fmt.Print("Enter code received from Telegram for login:")
		//nolint:unconvert // int conversion is in place to enable builds for Windows
		code, err := term.ReadPassword(int(syscall.Stdin))
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
	if err := client.Auth().IfNecessary(ctx, auth.NewFlow(userAuth, auth.SendCodeOptions{})); err != nil {
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

	data := [][]string{{"joined", "userID", "access_hash", "username", "firstName", "lastName", "message"}}

	for _, user := range users {
		data = append(data, []string{
			user.joined.Format(time.RFC3339),              // joined
			fmt.Sprintf("%d", user.userID),                // userID
			fmt.Sprintf("%d", user.accessHash),            // accessHash
			user.username,                                 // username
			strings.ReplaceAll(user.firstName, "\t", " "), // firstName
			strings.ReplaceAll(user.lastName, "\t", " "),  // lastName
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

func setupLog(dbg bool) {
	if dbg {
		log.Setup(log.Debug, log.CallerFile, log.CallerFunc, log.Msec, log.LevelBraces)
		return
	}
	log.Setup(log.Msec, log.LevelBraces)
}
