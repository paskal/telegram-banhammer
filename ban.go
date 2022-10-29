package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strconv"

	log "github.com/go-pkgz/lgr"
	"github.com/gotd/td/tg"
)

// bans users from given file, cleans up their messages and kicks them afterwards
func banAndKickUsers(ctx context.Context, api *tg.Client, channel *tg.Channel, filePath string) {

	users, err := readUserIDsFromCSV(filePath)
	if err != nil {
		log.Printf("[ERROR] error reading users from the file %s: %v", filePath, err)
		return
	}

	for i, user := range users {
		log.Printf("[DEBUG] Banning and kicking user %d forever", user.UserID)
		log.Printf("[DEBUG] Deleting messages by the user %d", user.UserID)
		_, err = api.ChannelsDeleteParticipantHistory(ctx, &tg.ChannelsDeleteParticipantHistoryRequest{
			Channel:     channel.AsInput(),
			Participant: user,
		})
		if err != nil {
			log.Printf("[ERROR] error deleting messages by the user %d: %v", user.UserID, err)
		}
		_, err = api.ChannelsEditBanned(ctx, &tg.ChannelsEditBannedRequest{
			Channel:     channel.AsInput(),
			Participant: user,
			BannedRights: tg.ChatBannedRights{
				ViewMessages: true,
				SendMessages: true,
				SendMedia:    true,
				SendStickers: true,
				SendGifs:     true,
				SendGames:    true,
				SendInline:   true,
				EmbedLinks:   true,
				SendPolls:    true,
				ChangeInfo:   true,
				InviteUsers:  true,
				PinMessages:  true,
				UntilDate:    0, // forever
			},
		})
		if err != nil {
			log.Printf("[ERROR] error banning user %d: %v", user.UserID, err)
		}
		log.Printf("[INFO] Done processing user #%d out of %d", i+1, len(users))
	}
}

// readUserIDsFromCSV reads user IDs from the second column of tab-separated CSV file
func readUserIDsFromCSV(filePath string) ([]*tg.InputPeerUser, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("error opening %s: %w", filePath, err)
	}

	var ids []*tg.InputPeerUser
	var sawFirstRow bool
	r := csv.NewReader(f)
	r.Comma = '\t'
	for {
		record, e := r.Read()

		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, fmt.Errorf("error reading %s: %w", filePath, e)
		}

		if !sawFirstRow {
			sawFirstRow = true
			continue
		}

		// user ID should be second column and accessHash is third
		if len(record) < 3 {
			continue
		}

		id, convErr := strconv.Atoi(record[1])
		if convErr != nil {
			log.Printf("[WARN] error converting %s id to int: %v", record[1], convErr)
			continue
		}
		accessHash, accessConvErr := strconv.Atoi(record[2])
		if convErr != nil {
			log.Printf("[WARN] error converting %s accessHash to int: %v", record[1], accessConvErr)
			continue
		}
		ids = append(ids, &tg.InputPeerUser{
			UserID:     int64(id),
			AccessHash: int64(accessHash),
		})
	}
	return ids, nil
}
