#!/bin/bash

# Script to download, verify, and install a new radio-gaga binary.
# Designed for Yocto Linux environment.

set -e # Exit immediately if a command exits with a non-zero status.

UPDATE_URL="$1"
CHECKSUM="$2"
NEW_BINARY_PATH="/tmp/radio-gaga.new"
RUNNING_BINARY_PATH="/usr/bin/radio-gaga"
OLD_BINARY_PATH="/usr/bin/radio-gaga.old"
SERVICE_NAME="rescoot-radio-gaga.service"

log() {
    echo "$(date '+%Y-%m-%d %H:%M:%S') - $1"
}

error_exit() {
    log "Error: $1"
    exit 1
}

if [ -z "$UPDATE_URL" ] || [ -z "$CHECKSUM" ]; then
    error_exit "Usage: $0 <update_url> <checksum>"
fi

log "Starting self-update process for Radio Gaga..."
log "Update URL: $UPDATE_URL"
log "Checksum: $CHECKSUM"

# 1. Check and remount root filesystem as writable
log "Checking filesystem writability..."
if mount | grep ' on / ' | grep -q rw; then
    log "Root filesystem is already writable."
else
    log "Root filesystem is read-only, attempting to remount as writable..."
    if ! mount -o rw,remount /; then
        error_exit "Failed to remount root filesystem as writable."
    fi
    log "Root filesystem remounted as writable."
fi

# 2. Download the new binary
log "Downloading new binary from $UPDATE_URL..."
if ! curl -fLk -o "$NEW_BINARY_PATH" "$UPDATE_URL"; then
    error_exit "Failed to download new binary."
fi
log "Download complete."

# 3. Verify the checksum
log "Verifying checksum..."
# Parse algorithm and value from checksum string (e.g., sha256:...)
CHECKSUM_ALGORITHM=$(echo "$CHECKSUM" | cut -d':' -f1)
EXPECTED_CHECKSUM=$(echo "$CHECKSUM" | cut -d':' -f2)

if [ -z "$CHECKSUM_ALGORITHM" ] || [ -z "$EXPECTED_CHECKSUM" ]; then
    error_exit "Invalid checksum format. Expected format: algorithm:value"
fi

CALCULATED_CHECKSUM=""
case "$CHECKSUM_ALGORITHM" in
    sha256)
        CALCULATED_CHECKSUM=$(sha256sum "$NEW_BINARY_PATH" | cut -d' ' -f1)
        ;;
    sha1)
        CALCULATED_CHECKSUM=$(sha1sum "$NEW_BINARY_PATH" | cut -d' ' -f1)
        ;;
    md5)
        CALCULATED_CHECKSUM=$(md5sum "$NEW_BINARY_PATH" | cut -d' ' -f1)
        ;;
    *)
        error_exit "Unsupported checksum algorithm: $CHECKSUM_ALGORITHM. Supported: sha256, sha1, md5"
        ;;
esac

if [ "$CALCULATED_CHECKSUM" != "$EXPECTED_CHECKSUM" ]; then
    error_exit "Checksum mismatch. Calculated: $CALCULATED_CHECKSUM, Expected: $EXPECTED_CHECKSUM"
fi
log "Checksum verification successful."

# 4. Backup the old binary
log "Backing up old binary to $OLD_BINARY_PATH..."
if ! cp "$RUNNING_BINARY_PATH" "$OLD_BINARY_PATH"; then
    error_exit "Failed to backup old binary."
fi
log "Backup complete."

# 5. Replace the running binary
log "Replacing running binary with new version..."
if ! mv "$NEW_BINARY_PATH" "$RUNNING_BINARY_PATH"; then
    log "Failed to replace binary. Attempting to restore backup..."
    if ! mv "$OLD_BINARY_PATH" "$RUNNING_BINARY_PATH"; then
        error_exit "Failed to replace binary and failed to restore backup. Manual intervention required!"
    else
        error_exit "Failed to replace binary, but backup restored."
    fi
fi
log "Binary replaced successfully."

# 6. Set executable permission
log "Setting executable permission on new binary..."
if ! chmod +x "$RUNNING_BINARY_PATH"; then
    error_exit "Failed to set executable permission."
fi
log "Executable permission set."

# 7. Restart the service
log "Restarting $SERVICE_NAME service..."
if ! systemctl restart "$SERVICE_NAME"; then
    error_exit "Failed to restart service. Manual intervention required!"
fi
log "$SERVICE_NAME service restarted."

log "Self-update process completed successfully."
exit 0
