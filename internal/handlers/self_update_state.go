package handlers

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"radio-gaga/internal/models"
	"radio-gaga/internal/utils"
)

type selfUpdateFlight struct {
	key  string
	done chan struct{}
	err  error
}

var selfUpdateFlights struct {
	sync.Mutex
	active *selfUpdateFlight
}

type selfUpdateResult struct {
	Request  string    `json:"request"`
	Identity string    `json:"identity"`
	Error    string    `json:"error,omitempty"`
	Finished time.Time `json:"finished"`
}

// Only self_update is deduplicated. An explicit new request ID retries a failed
// probe using the verified cached payload, while QoS redelivery replays its
// completed result. The file lock also excludes another updater process.
func coordinatedSelfUpdate(ctx context.Context, dir, requestID, identity string, run func() error) error {
	request := fmt.Sprintf("%x", sha256.Sum256([]byte(requestID)))
	if requestID == "" {
		request = "anonymous-" + identity
	}
	key := dir + "\x00" + request + "\x00" + identity
	selfUpdateFlights.Lock()
	if active := selfUpdateFlights.active; active != nil {
		selfUpdateFlights.Unlock()
		if active.key != key {
			return fmt.Errorf("another self-update is in progress; retry with a new request ID after it completes")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-active.done:
			return active.err
		}
	}
	flight := &selfUpdateFlight{key: key, done: make(chan struct{})}
	selfUpdateFlights.active = flight
	selfUpdateFlights.Unlock()
	var result error
	defer func() {
		selfUpdateFlights.Lock()
		flight.err = result
		close(flight.done)
		selfUpdateFlights.active = nil
		selfUpdateFlights.Unlock()
	}()
	result = runCoordinatedSelfUpdate(ctx, dir, request, identity, run)
	return result
}

func runCoordinatedSelfUpdate(ctx context.Context, dir, request, identity string, run func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("self-update state is locked by another process: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck
	path := filepath.Join(dir, "results.json")
	var results []selfUpdateResult
	if err := readUpdateJSON(path, &results, 256*1024); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read self-update results: %w", err)
	}
	if len(results) > 64 {
		return fmt.Errorf("self-update result history exceeds 64 entries")
	}
	now := time.Now()
	kept := results[:0]
	for _, prior := range results {
		if now.Sub(prior.Finished) > selfUpdateCacheTTL {
			continue
		}
		kept = append(kept, prior)
		if prior.Request == request {
			if prior.Identity != identity {
				return fmt.Errorf("self-update request ID reused with different URL or checksum")
			}
			if prior.Error != "" {
				return errors.New(prior.Error)
			}
			return nil
		}
	}
	results = kept
	err = run()
	message := ""
	if err != nil {
		message = err.Error()
		if len(message) > 2048 {
			message = message[:2048]
		}
	}
	if len(results) >= 64 {
		results = results[len(results)-63:]
	}
	results = append(results, selfUpdateResult{Request: request, Identity: identity, Error: message, Finished: time.Now()})
	if saveErr := writeUpdateJSON(path, results); saveErr != nil {
		return fmt.Errorf("self-update result %v; persist result: %w", err, saveErr)
	}
	return err
}

func selfUpdateDownloadPolicy(client CommandHandlerClient) (updateDownloadPolicy, error) {
	policy := defaultUpdateDownloadPolicy()
	for _, setting := range []struct {
		name  string
		value *time.Duration
		max   time.Duration
	}{
		{"download_budget", &policy.budget, 24 * time.Hour},
		{"stall_timeout", &policy.stall, 30 * time.Minute},
	} {
		value := client.GetCommandParam("self_update", setting.name, setting.value.String())
		duration, err := utils.ParseDuration(value)
		if err != nil || duration < time.Second || duration > setting.max {
			return policy, fmt.Errorf("self_update %s must be between 1s and %s", setting.name, setting.max)
		}
		*setting.value = duration
	}
	return policy, nil
}

func selfUpdateStateDir(client CommandHandlerClient, config *models.Config) (string, error) {
	if config != nil && config.StateDir != "" {
		return filepath.Join(config.StateDir, "self-update"), nil
	}
	if client.GetConfigPath() == "" {
		return "", fmt.Errorf("config path unavailable; cannot test candidate")
	}
	return filepath.Join(filepath.Dir(client.GetConfigPath()), ".self-update"), nil
}
