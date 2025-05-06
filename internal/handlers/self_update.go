package handlers

import (
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"

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
	tempFile, err := os.CreateTemp("/tmp", "radio-gaga-*.new")
	if err != nil {
		return fmt.Errorf("failed to create temporary file: %v", err)
	}

	log.Printf("Downloading new binary from %s", updateURL)
	// Create HTTP client with TLS verification skipping as system time might be unreliable
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	httpClient := &http.Client{Transport: transport}

	resp, err := httpClient.Get(updateURL)
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

	// Create the update helper script
	serviceName := config.ServiceName
	scriptPath, err := createUpdateHelperScript(tempFile.Name(), currentExe, serviceName)
	if err != nil {
		return fmt.Errorf("failed to create update helper script: %v", err)
	}

	// Make script executable
	if err := os.Chmod(scriptPath, 0755); err != nil {
		os.Remove(scriptPath)
		return fmt.Errorf("failed to make helper script executable: %v", err)
	}

	// Execute the update helper script in background
	log.Printf("Starting update helper script to replace binary and restart service")
	cmd := exec.Command("/bin/sh", "-c", fmt.Sprintf("nohup %s > /tmp/radio-gaga-update.log 2>&1 &", scriptPath))
	if err := cmd.Start(); err != nil {
		os.Remove(scriptPath)
		return fmt.Errorf("failed to start update helper script: %v", err)
	}

	return nil
}

// createUpdateHelperScript creates a minimal shell script to handle binary replacement and service restart
func createUpdateHelperScript(newBinaryPath, currentBinaryPath, serviceName string) (string, error) {
	scriptContent := fmt.Sprintf(`#!/bin/sh
# Radio-Gaga update helper script
# This script handles replacing the binary and restarting the service

set -e
LOG_FILE="/tmp/radio-gaga-update.log"

log() {
    echo "$(date '+%%Y-%%m-%%d %%H:%%M:%%S') - $1" >> "$LOG_FILE"
}

log "Starting update helper script"

NEW_BINARY="%s"
CURRENT_PATH="%s"
SERVICE_NAME="%s"
BACKUP_PATH="${CURRENT_PATH}.old"

log "New binary: $NEW_BINARY"
log "Current binary: $CURRENT_PATH"
log "Service name: $SERVICE_NAME"

# Check if filesystem needs to be remounted
NEEDS_REMOUNT=0
CURRENT_DIR=$(dirname "$CURRENT_PATH")
log "Checking if $CURRENT_DIR is writable"

if ! touch "$CURRENT_DIR/.write_test" 2>/dev/null; then
    log "Filesystem is not writable, need to remount"
    NEEDS_REMOUNT=1
fi

if [ -f "$CURRENT_DIR/.write_test" ]; then
    rm -f "$CURRENT_DIR/.write_test"
fi

# Remount filesystem if needed
if [ $NEEDS_REMOUNT -eq 1 ]; then
    log "Remounting root filesystem as read-write"
    if ! mount -o remount,rw /; then
        log "ERROR: Failed to remount filesystem as read-write"
        exit 1
    fi
    log "Filesystem remounted successfully"
fi

# Create backup of current binary
log "Creating backup of current binary"
mv "$CURRENT_PATH" "$BACKUP_PATH"
if [ $? -ne 0 ]; then
    log "ERROR: Failed to create backup"
    exit 1
fi

# Replace binary
log "Replacing binary"
mv "$NEW_BINARY" "$CURRENT_PATH"
if [ $? -ne 0 ]; then
    log "ERROR: Failed to replace binary"
    log "Restoring from backup"
    mv "$BACKUP_PATH" "$CURRENT_PATH" 
    exit 1
fi

# Clean up temporary binary
rm -f "$NEW_BINARY"

# Restart service
log "Restarting service: $SERVICE_NAME"
systemctl restart "$SERVICE_NAME"
if [ $? -ne 0 ]; then
    log "ERROR: Failed to restart service"
    log "Rolling back to previous version"
    mv "$BACKUP_PATH" "$CURRENT_PATH"
    systemctl restart "$SERVICE_NAME"
    exit 1
fi

# Verify service is running
log "Waiting to verify service is running properly"
sleep 10
systemctl is-active "$SERVICE_NAME" > /dev/null
if [ $? -ne 0 ]; then
    log "ERROR: Service failed to start after update"
    log "Rolling back to previous version"
    mv "$BACKUP_PATH" "$CURRENT_PATH"
    systemctl restart "$SERVICE_NAME"
    exit 1
fi

# Remount filesystem back to read-only if we changed it
if [ $NEEDS_REMOUNT -eq 1 ]; then
    log "Remounting filesystem back to read-only"
    if ! mount -o remount,ro /; then
        log "WARNING: Failed to remount filesystem back to read-only"
    else
        log "Filesystem remounted to read-only successfully"
    fi
fi

# Update successful, clean up
log "Update completed successfully"
rm -f "$BACKUP_PATH"
exit 0
`, newBinaryPath, currentBinaryPath, serviceName)

	// Write script to temporary file
	scriptFile, err := os.CreateTemp("/tmp", "radio-gaga-updater-*.sh")
	if err != nil {
		return "", fmt.Errorf("failed to create update script file: %v", err)
	}

	if _, err := scriptFile.WriteString(scriptContent); err != nil {
		scriptFile.Close()
		os.Remove(scriptFile.Name())
		return "", fmt.Errorf("failed to write update script content: %v", err)
	}

	scriptFile.Close()
	return scriptFile.Name(), nil
}
