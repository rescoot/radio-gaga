package handlers

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"radio-gaga/internal/models"
	"radio-gaga/internal/utils"
)

// handleSelfUpdateCommand handles the self_update command
func handleSelfUpdateCommand(client CommandHandlerClient, params map[string]interface{}, requestID string, config *models.Config) error {
	updateURL, ok := params["url"].(string)
	if !ok || updateURL == "" {
		return fmt.Errorf("update URL not specified or invalid")
	}

	checksum, ok := params["checksum"].(string)
	if !ok || checksum == "" {
		return fmt.Errorf("checksum not specified or invalid")
	}

	// Parse checksum algorithm and value
	parts := strings.SplitN(checksum, ":", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid checksum format. Expected format: algorithm:value")
	}
	algorithm, expectedChecksum := parts[0], parts[1]

	// Download new binary to temporary location
	tempFile, err := os.CreateTemp("", "radio-gaga-*.new")
	if err != nil {
		return fmt.Errorf("failed to create temporary file: %v", err)
	}
	defer os.Remove(tempFile.Name())

	log.Printf("Downloading new binary from %s", updateURL)
	resp, err := http.Get(updateURL)
	if err != nil {
		return fmt.Errorf("failed to download new binary: %v", err)
	}
	defer resp.Body.Close()

	// Calculate checksum while downloading
	hasher, err := utils.CreateHash(algorithm)
	if err != nil {
		return err
	}

	writer := io.MultiWriter(tempFile, hasher)
	if _, err := io.Copy(writer, resp.Body); err != nil {
		return fmt.Errorf("failed to save new binary: %v", err)
	}
	tempFile.Close()

	// Verify checksum
	calculatedChecksum := fmt.Sprintf("%x", hasher.Sum(nil))
	if calculatedChecksum != expectedChecksum {
		return fmt.Errorf("checksum mismatch. Expected: %s, got: %s", expectedChecksum, calculatedChecksum)
	}

	// Make new binary executable
	if err := os.Chmod(tempFile.Name(), 0755); err != nil {
		return fmt.Errorf("failed to make new binary executable: %v", err)
	}

	// Get current executable path
	currentExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get current executable path: %v", err)
	}

	// Create backup of current binary
	backupPath := currentExe + ".old"
	if err := os.Rename(currentExe, backupPath); err != nil {
		return fmt.Errorf("failed to create backup: %v", err)
	}

	// Move new binary into place
	if err := os.Rename(tempFile.Name(), currentExe); err != nil {
		// Try to restore backup if moving new binary fails
		if restoreErr := os.Rename(backupPath, currentExe); restoreErr != nil {
			return fmt.Errorf("failed to move new binary and restore backup: %v (original error: %v)", restoreErr, err)
		}
		return fmt.Errorf("failed to move new binary into place: %v", err)
	}

	// Use service name from config (which should now be auto-detected if not specified)
	serviceName := config.ServiceName
	log.Printf("Using systemd service name for restart: %s", serviceName)

	// Start verification process in a goroutine
	go func() {
		// Restart the service
		cmd := exec.Command("systemctl", "restart", serviceName)
		if err := cmd.Run(); err != nil {
			log.Printf("Failed to restart service: %v", err)
			rollbackUpdate(currentExe, backupPath, serviceName)
			return
		}

		// Wait 10 seconds and verify the service is still running
		time.Sleep(10 * time.Second)

		cmd = exec.Command("systemctl", "is-active", serviceName)
		output, err := cmd.Output()
		if err != nil || strings.TrimSpace(string(output)) != "active" {
			log.Printf("New version failed verification: %v", err)
			rollbackUpdate(currentExe, backupPath, serviceName)
			return
		}

		// If we get here, update was successful - remove backup
		os.Remove(backupPath)
		log.Printf("Update successfully completed and verified")
	}()

	return nil
}

// rollbackUpdate rolls back a failed update
func rollbackUpdate(currentExe, backupPath, serviceName string) {
	log.Printf("Rolling back update...")

	// Stop the service
	exec.Command("systemctl", "stop", serviceName).Run()

	// Remove failed binary
	os.Remove(currentExe)

	// Restore backup
	if err := os.Rename(backupPath, currentExe); err != nil {
		log.Printf("Failed to restore backup: %v", err)
		return
	}

	// Restart service with old binary
	if err := exec.Command("systemctl", "restart", serviceName).Run(); err != nil {
		log.Printf("Failed to restart service after rollback: %v", err)
		return
	}

	log.Printf("Rollback completed successfully")
}
