package sync

import (
	"context"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type LogUploader struct {
	apiBaseURL string
	scooterID  string
	token      string
	httpClient *http.Client
}

// UploadRejectedError is returned when sunshine rejects the upload with a 4xx
// status. These are deterministic (auth, size limit, malformed request) — a
// retry would fail the same way, so callers should not retry.
type UploadRejectedError struct {
	StatusCode int
	Body       string
}

func (e *UploadRejectedError) Error() string {
	return fmt.Sprintf("upload rejected with status %d: %s", e.StatusCode, e.Body)
}

func NewLogUploader(apiBaseURL, scooterID, token string, timeout time.Duration) *LogUploader {
	return &LogUploader{
		apiBaseURL: strings.TrimRight(apiBaseURL, "/"),
		scooterID:  scooterID,
		token:      token,
		httpClient: &http.Client{Timeout: timeout},
	}
}

func (u *LogUploader) Upload(ctx context.Context, archivePath, requestID string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("opening archive: %w", err)
	}
	defer file.Close()

	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)

	go func() {
		defer pw.Close()
		defer writer.Close()

		if requestID != "" {
			if err := writer.WriteField("request_id", requestID); err != nil {
				pw.CloseWithError(err)
				return
			}
		}

		part, err := writer.CreateFormFile("file", filepath.Base(archivePath))
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(part, file); err != nil {
			pw.CloseWithError(err)
			return
		}
	}()

	url := fmt.Sprintf("%s/api/v1/scooters/%s/log_bundles", u.apiBaseURL, u.scooterID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, pr)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+u.token)

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return &UploadRejectedError{StatusCode: resp.StatusCode, Body: string(body)}
		}
		return fmt.Errorf("upload failed with status %d: %s", resp.StatusCode, string(body))
	}

	log.Printf("Log archive uploaded to %s (status %d)", url, resp.StatusCode)
	return nil
}
