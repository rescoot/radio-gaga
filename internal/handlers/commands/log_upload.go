package commands

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/go-redis/redis/v8"

	"radio-gaga/internal/journalupload"
	"radio-gaga/internal/models"
)

// JournalUploadStateDir is where session + cursor files live on the scooter.
// Defaults to /data/radio-gaga (LibreScoot's writable partition); overridden
// from main.go when -state-dir is passed (e.g. /var/lib/radio-gaga on stock).
var JournalUploadStateDir = "/data/radio-gaga"

var (
	managerMu     sync.Mutex
	managerCancel context.CancelFunc
)

// StartManager spawns the tailer manager if one isn't already running.
// Safe to call from both the Start command handler and from the radio-gaga
// startup path (re-arm after reboot).
func StartManager(state *journalupload.State) {
	managerMu.Lock()
	defer managerMu.Unlock()
	if managerCancel != nil {
		// already running
		return
	}
	runCtx, cancel := context.WithCancel(context.Background())
	managerCancel = cancel
	go func() {
		m := &journalupload.Manager{
			State:     state,
			NewReader: journalupload.NewJournalctlReader,
		}
		m.Run(runCtx)
		managerMu.Lock()
		managerCancel = nil
		managerMu.Unlock()
	}()
}

// HandleStartLogUploadCommand persists a session file and spawns the tailer.
func HandleStartLogUploadCommand(client CommandHandlerClient, redisClient *redis.Client, ctx context.Context, config *models.Config, params map[string]any, requestID string) error {
	url, _ := params["url"].(string)
	username, _ := params["username"].(string)
	password, _ := params["password"].(string)
	if url == "" || username == "" || password == "" {
		return fmt.Errorf("start_log_upload: url, username, password all required")
	}

	state := &journalupload.State{Dir: JournalUploadStateDir}
	if err := state.Clear(); err != nil {
		log.Printf("start_log_upload: clear old state: %v", err)
	}
	if err := state.WriteSession(journalupload.Session{URL: url, Username: username, Password: password}); err != nil {
		return fmt.Errorf("start_log_upload: write session: %w", err)
	}

	StartManager(state)
	log.Printf("start_log_upload: streaming started")
	return nil
}

// HandleStopLogUploadCommand stops the tailer and wipes persisted state.
func HandleStopLogUploadCommand(client CommandHandlerClient, redisClient *redis.Client, ctx context.Context, config *models.Config, params map[string]any, requestID string) error {
	managerMu.Lock()
	if managerCancel != nil {
		managerCancel()
		managerCancel = nil
	}
	managerMu.Unlock()

	state := &journalupload.State{Dir: JournalUploadStateDir}
	if err := state.Clear(); err != nil {
		log.Printf("stop_log_upload: clear state: %v", err)
	}
	log.Printf("stop_log_upload: streaming stopped")
	return nil
}
