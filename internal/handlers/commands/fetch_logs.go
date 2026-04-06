package commands

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"time"

	"radio-gaga/internal/models"
	"radio-gaga/internal/sync"
)

func HandleFetchLogsCommand(client CommandHandlerClient, config *models.Config, params map[string]interface{}, requestID string) error {
	since := "24h"
	if s, ok := params["since"].(string); ok && s != "" {
		since = s
	}

	outputDir := fmt.Sprintf("/tmp/scooter-logs-%s", requestID)
	defer os.RemoveAll(outputDir)
	defer os.Remove(outputDir + ".tar.gz")

	log.Printf("Collecting logs (since=%s) to %s", since, outputDir)

	cmd := exec.Command("lsc", "logs", "--since", since, "--output", outputDir, "--json")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("lsc logs failed: %w (output: %s)", err, string(output))
	}

	archivePath := outputDir + ".tar.gz"
	if _, err := os.Stat(archivePath); os.IsNotExist(err) {
		return fmt.Errorf("archive not found at %s after lsc logs", archivePath)
	}

	timeout := 60 * time.Second
	if config.API.Timeout != "" {
		if d, err := time.ParseDuration(config.API.Timeout); err == nil {
			timeout = d
		}
	}

	uploader := sync.NewLogUploader(config.API.BaseURL, config.API.ScooterID, config.Scooter.Token, timeout)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if err := uploader.Upload(ctx, archivePath, requestID); err != nil {
		log.Printf("Log upload failed, retrying: %v", err)
		time.Sleep(2 * time.Second)
		if err := uploader.Upload(ctx, archivePath, requestID); err != nil {
			return fmt.Errorf("log upload failed after retry: %w", err)
		}
	}

	client.SendCommandResponse(requestID, "success", "")
	return nil
}
