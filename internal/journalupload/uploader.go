package journalupload

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

var ErrSessionInvalid = errors.New("journalupload: session invalidated (401)")

type Uploader struct {
	URL      string
	Username string
	Password string
	Client   *http.Client
	Backoff  []time.Duration
}

func DefaultBackoff() []time.Duration {
	return []time.Duration{
		1 * time.Second, 2 * time.Second, 4 * time.Second,
		8 * time.Second, 16 * time.Second, 32 * time.Second,
		60 * time.Second,
	}
}

// Upload POSTs body with content-type application/vnd.fdo.journal.
// On 401 returns ErrSessionInvalid (caller should stop the tailer).
// On 5xx / network errors, retries per the Backoff schedule.
func (u *Uploader) Upload(ctx context.Context, body []byte) error {
	backoff := u.Backoff
	if backoff == nil {
		backoff = DefaultBackoff()
	}
	client := u.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	var lastErr error
	for attempt := 0; attempt <= len(backoff); attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff[attempt-1]):
			}
		}

		req, err := http.NewRequestWithContext(ctx, "POST", u.URL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/vnd.fdo.journal")
		req.SetBasicAuth(u.Username, u.Password)

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		switch {
		case resp.StatusCode == http.StatusNoContent, resp.StatusCode == http.StatusOK:
			return nil
		case resp.StatusCode == http.StatusUnauthorized:
			return ErrSessionInvalid
		case resp.StatusCode >= 500:
			lastErr = fmt.Errorf("server error %d", resp.StatusCode)
			continue
		default:
			return fmt.Errorf("upload failed: status %d", resp.StatusCode)
		}
	}
	return lastErr
}
