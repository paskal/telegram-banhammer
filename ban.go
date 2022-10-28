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

	userIDs, err := readUserIDsFromCSV(filePath)
	if err != nil {
		log.Printf("[ERROR] error reading users from the file %s: %v", filePath, err)
		return
	}

	for i, userID := range userIDs {
		log.Printf("[DEBUG] Banning and kicking user %d forever", userID)
		_, err = api.ChannelsEditBanned(ctx, &tg.ChannelsEditBannedRequest{
			Channel:     channel.AsInput(),
			Participant: &tg.InputPeerUser{UserID: userID},
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
			log.Printf("[ERROR] error banning user %d: %v", userID, err)
		}
		log.Printf("[DEBUG] Deleting messages by the user %d", userID)
		_, err = api.ChannelsDeleteParticipantHistory(ctx, &tg.ChannelsDeleteParticipantHistoryRequest{
			Channel:     channel.AsInput(),
			Participant: &tg.InputPeerUser{UserID: userID},
		})
		if err != nil {
			log.Printf("[ERROR] error deleting messages by the user %d: %v", userID, err)
		}
		log.Printf("[INFO] Done processing user #%d out of %d", i+1, len(userIDs))
	}
}

// readUserIDsFromCSV reads user IDs from the second column of tab-separated CSV file
func readUserIDsFromCSV(filePath string) ([]int64, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("error opening %s: %w", filePath, err)
	}

	ids := make([]int64, 0)
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

		// user ID should be second column
		if len(record) < 2 {
			continue
		}

		id, convErr := strconv.Atoi(record[1])
		if convErr != nil {
			log.Printf("[WARN] error converting %s to int: %v", record[1], convErr)
			continue
		}
		ids = append(ids, int64(id))
	}
	return ids, nil
}
