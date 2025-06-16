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

// handleSelfUpdateCommand handles the self_update command with comprehensive error handling
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

	// Validate service name
	serviceName := config.ServiceName
	if serviceName == "" {
		serviceName = "rescoot-radio-gaga.service" // Default fallback
		log.Printf("Warning: No service name configured, using default: %s", serviceName)
	}

	// Download new binary to temporary location
	tempFile, err := os.CreateTemp("/tmp", "radio-gaga-*.new")
	if err != nil {
		return fmt.Errorf("failed to create temporary file: %v", err)
	}
	tempFileName := tempFile.Name()

	// Ensure cleanup of temporary file on error
	defer func() {
		if tempFile != nil {
			tempFile.Close()
		}
		// Only clean up temp file if we haven't passed it to the update script
		if _, statErr := os.Stat(tempFileName); statErr == nil {
			os.Remove(tempFileName)
		}
	}()

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

	// Check HTTP status
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to download new binary: HTTP %d", resp.StatusCode)
	}

	// Calculate checksum while downloading
	hasher, err := utils.CreateHash(algorithm)
	if err != nil {
		return fmt.Errorf("failed to create hash: %v", err)
	}

	writer := io.MultiWriter(tempFile, hasher)
	if _, err := io.Copy(writer, resp.Body); err != nil {
		return fmt.Errorf("failed to save new binary: %v", err)
	}
	tempFile.Close()
	tempFile = nil // Prevent defer from closing again

	// Verify checksum
	calculatedChecksum := fmt.Sprintf("%x", hasher.Sum(nil))
	if calculatedChecksum != expectedChecksum {
		return fmt.Errorf("checksum mismatch. Expected: %s, got: %s", expectedChecksum, calculatedChecksum)
	}
	log.Printf("Checksum verification successful: %s", calculatedChecksum)

	// Make new binary executable
	if err := os.Chmod(tempFileName, 0755); err != nil {
		return fmt.Errorf("failed to make new binary executable: %v", err)
	}

	// Get current executable path
	currentExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get current executable path: %v", err)
	}

	// Verify current executable exists
	if _, err := os.Stat(currentExe); err != nil {
		return fmt.Errorf("current executable not found at %s: %v", currentExe, err)
	}

	// Create the update helper script
	scriptPath, err := createUpdateHelperScript(tempFileName, currentExe, serviceName)
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
	log.Printf("Update script: %s", scriptPath)
	log.Printf("Update log will be written to: /tmp/radio-gaga-update.log")

	cmd := exec.Command("/bin/sh", "-c", fmt.Sprintf("nohup %s > /tmp/radio-gaga-update.log 2>&1 &", scriptPath))
	if err := cmd.Start(); err != nil {
		os.Remove(scriptPath)
		return fmt.Errorf("failed to start update helper script: %v", err)
	}

	log.Printf("Update process initiated successfully. The service will restart shortly.")
	return nil
}

// createUpdateHelperScript creates a robust shell script to handle binary replacement and service restart
func createUpdateHelperScript(newBinaryPath, currentBinaryPath, serviceName string) (string, error) {
	scriptContent := fmt.Sprintf(`#!/bin/sh
# Radio-Gaga update helper script
# This script handles replacing the binary and restarting the service with comprehensive error handling

set -e
LOG_FILE="/tmp/radio-gaga-update.log"

log() {
    echo "$(date '+%%Y-%%m-%%d %%H:%%M:%%S') - $1" | tee -a "$LOG_FILE"
}

error_cleanup() {
    local error_msg="$1"
    log "ERROR: $error_msg"

    # Clean up temporary files
    [ -f "$NEW_BINARY" ] && rm -f "$NEW_BINARY"
    [ -f "$SCRIPT_PATH" ] && rm -f "$SCRIPT_PATH"

    # Remount filesystem back to read-only if we changed it
    if [ $NEEDS_REMOUNT -eq 1 ]; then
        log "Remounting filesystem back to read-only due to error"
        mount -o remount,ro / 2>/dev/null || log "WARNING: Failed to remount filesystem back to read-only"
    fi

    exit 1
}

# Trap to ensure cleanup on script exit
trap 'error_cleanup "Script interrupted"' INT TERM

log "Starting update helper script"

NEW_BINARY="%s"
CURRENT_PATH="%s"
SERVICE_NAME="%s"
BACKUP_PATH="${CURRENT_PATH}.old"
SCRIPT_PATH="$0"

log "New binary: $NEW_BINARY"
log "Current binary: $CURRENT_PATH"
log "Service name: $SERVICE_NAME"

# Verify new binary exists and is executable
if [ ! -f "$NEW_BINARY" ]; then
    error_cleanup "New binary file does not exist: $NEW_BINARY"
fi

if [ ! -x "$NEW_BINARY" ]; then
    log "Making new binary executable"
    chmod +x "$NEW_BINARY" || error_cleanup "Failed to make new binary executable"
fi

# Verify current binary exists
if [ ! -f "$CURRENT_PATH" ]; then
    error_cleanup "Current binary does not exist: $CURRENT_PATH"
fi

# Check if filesystem needs to be remounted
NEEDS_REMOUNT=0
CURRENT_DIR=$(dirname "$CURRENT_PATH")
log "Checking if $CURRENT_DIR is writable"

if ! touch "$CURRENT_DIR/.write_test" 2>/dev/null; then
    log "Filesystem is not writable, need to remount"
    NEEDS_REMOUNT=1
else
    rm -f "$CURRENT_DIR/.write_test"
fi

# Remount filesystem if needed
if [ $NEEDS_REMOUNT -eq 1 ]; then
    log "Remounting root filesystem as read-write"
    if ! mount -o remount,rw /; then
        error_cleanup "Failed to remount filesystem as read-write"
    fi
    log "Filesystem remounted successfully"
fi

# Create backup of current binary using cp to preserve original
log "Creating backup of current binary"
if ! cp "$CURRENT_PATH" "$BACKUP_PATH"; then
    error_cleanup "Failed to create backup of current binary"
fi
log "Backup created successfully"

# Replace binary
log "Replacing binary"
if ! mv "$NEW_BINARY" "$CURRENT_PATH"; then
    log "Failed to replace binary, restoring from backup"
    if ! mv "$BACKUP_PATH" "$CURRENT_PATH"; then
        error_cleanup "Failed to replace binary AND failed to restore backup - manual intervention required!"
    fi
    error_cleanup "Failed to replace binary, but backup restored successfully"
fi
log "Binary replaced successfully"

# Restart service
log "Restarting service: $SERVICE_NAME"
if ! systemctl restart "$SERVICE_NAME"; then
    log "Failed to restart service, rolling back to previous version"
    if mv "$BACKUP_PATH" "$CURRENT_PATH" && systemctl restart "$SERVICE_NAME"; then
        error_cleanup "Service restart failed, but rollback completed successfully"
    else
        error_cleanup "Service restart failed AND rollback failed - manual intervention required!"
    fi
fi
log "Service restarted successfully"

# Verify service is running with multiple checks
log "Verifying service is running properly"
sleep 5

# Check 1: Service is active
if ! systemctl is-active "$SERVICE_NAME" >/dev/null 2>&1; then
    log "Service is not active, rolling back"
    if mv "$BACKUP_PATH" "$CURRENT_PATH" && systemctl restart "$SERVICE_NAME"; then
        error_cleanup "Service failed to start, but rollback completed successfully"
    else
        error_cleanup "Service failed to start AND rollback failed - manual intervention required!"
    fi
fi

# Check 2: Wait a bit more and verify again
log "Waiting additional time for service stabilization"
sleep 10

if ! systemctl is-active "$SERVICE_NAME" >/dev/null 2>&1; then
    log "Service became inactive after initial check, rolling back"
    if mv "$BACKUP_PATH" "$CURRENT_PATH" && systemctl restart "$SERVICE_NAME"; then
        error_cleanup "Service became unstable, but rollback completed successfully"
    else
        error_cleanup "Service became unstable AND rollback failed - manual intervention required!"
    fi
fi

log "Service verification completed successfully"

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
rm -f "$SCRIPT_PATH"
log "Cleanup completed"
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
