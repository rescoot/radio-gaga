#!/bin/sh
# Radio-Gaga out-of-band update script
# This script downloads and installs a new version of the radio-gaga binary

set -e
LOG_FILE="/tmp/radio-gaga-oob-update.log"

log() {
    echo "$(date '+%Y-%m-%d %H:%M:%S') - $1"
    echo "$(date '+%Y-%m-%d %H:%M:%S') - $1" >> "$LOG_FILE"
}

log "Starting Radio-Gaga out-of-band update"

# Configuration
DOWNLOAD_URL="https://sunshine.rescoot.org/api/v1/radio_gaga/download/5"
EXPECTED_CHECKSUM="b93efa56f974f54bbf99f70b8a807d7093475006c30b4e9c503b4171f3edab58"
SERVICE_NAME="radio-gaga"
TEMP_BINARY="/tmp/radio-gaga-$$.new"

# Determine current binary path using multiple strategies
log "Determining current binary path"

# 1. First check if service is running and get the path from the process
SERVICE_PID=$(systemctl show -p MainPID ${SERVICE_NAME} 2>/dev/null | cut -d= -f2)
if [ -n "$SERVICE_PID" ] && [ "$SERVICE_PID" -ne 0 ]; then
    log "Service is running with PID $SERVICE_PID, trying to get path from process"
    PROC_PATH=$(readlink -e /proc/$SERVICE_PID/exe 2>/dev/null)
    if [ -n "$PROC_PATH" ]; then
        CURRENT_PATH="$PROC_PATH"
        log "Found path from running process: $CURRENT_PATH"
    fi
fi

# 2. If not found, try to get from service file (for any service that matches)
if [ -z "$CURRENT_PATH" ]; then
    log "Trying to find path from service files"
    SERVICE_FILES=$(find /etc/systemd/system -name "*radio-gaga*.service" 2>/dev/null)
    for SERVICE_FILE in $SERVICE_FILES; do
        log "Checking service file: $SERVICE_FILE"
        SVC_PATH=$(grep -o "ExecStart=.*" "$SERVICE_FILE" | sed 's/ExecStart=//' | awk '{print $1}')
        if [ -n "$SVC_PATH" ] && [ -e "$SVC_PATH" ]; then
            CURRENT_PATH="$SVC_PATH"
            SERVICE_NAME=$(basename "$SERVICE_FILE" .service)
            log "Found path from service file $SERVICE_FILE: $CURRENT_PATH"
            break
        fi
    done
fi

# 3. Try with which and readlink -e (only resolves if file exists)
if [ -z "$CURRENT_PATH" ]; then
    log "Trying common binary locations"
    for BIN_PATH in $(which radio-gaga 2>/dev/null) "/usr/local/bin/radio-gaga" "/usr/bin/radio-gaga" "/var/usrlocal/bin/radio-gaga"; do
        if [ -e "$BIN_PATH" ]; then
            CURRENT_PATH=$(readlink -e "$BIN_PATH" 2>/dev/null || echo "$BIN_PATH")
            log "Found binary at common location: $CURRENT_PATH"
            break
        fi
    done
fi

if [ -z "$CURRENT_PATH" ]; then
    log "ERROR: Could not determine current binary path"
    log "Please ensure radio-gaga is installed and/or the service is properly configured"
    exit 1
fi

# Verify that the path actually exists
if [ ! -e "$CURRENT_PATH" ]; then
    log "ERROR: Found path '$CURRENT_PATH' does not exist"
    exit 1
fi

log "Found current binary at $CURRENT_PATH"
BACKUP_PATH="${CURRENT_PATH}.old"

# Download new binary
log "Downloading new binary from $DOWNLOAD_URL"
if ! curl -Lks "$DOWNLOAD_URL" -o "$TEMP_BINARY"; then
    log "ERROR: Failed to download new binary"
    exit 1
fi

# Verify checksum
log "Verifying checksum"
CALCULATED_CHECKSUM=$(sha256sum "$TEMP_BINARY" | awk '{print $1}')
if [ "$CALCULATED_CHECKSUM" != "$EXPECTED_CHECKSUM" ]; then
    log "ERROR: Checksum verification failed"
    log "Expected: $EXPECTED_CHECKSUM"
    log "Got:      $CALCULATED_CHECKSUM"
    rm -f "$TEMP_BINARY"
    exit 1
fi
log "Checksum verified successfully"

# Make new binary executable
log "Making new binary executable"
chmod 755 "$TEMP_BINARY"

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
        rm -f "$TEMP_BINARY"
        exit 1
    fi
    log "Filesystem remounted successfully"
fi

# Create backup of current binary
log "Creating backup of current binary"
if ! cp "$CURRENT_PATH" "$BACKUP_PATH"; then
    log "ERROR: Failed to create backup"
    if [ $NEEDS_REMOUNT -eq 1 ]; then
        mount -o remount,ro /
    fi
    rm -f "$TEMP_BINARY"
    exit 1
fi

# Replace binary
log "Replacing binary"
if ! mv "$TEMP_BINARY" "$CURRENT_PATH"; then
    log "ERROR: Failed to replace binary"
    log "Restoring from backup"
    mv "$BACKUP_PATH" "$CURRENT_PATH" 
    if [ $NEEDS_REMOUNT -eq 1 ]; then
        mount -o remount,ro /
    fi
    exit 1
fi

# Restart service
log "Restarting service: $SERVICE_NAME"
if ! systemctl restart "$SERVICE_NAME"; then
    log "ERROR: Failed to restart service"
    log "Rolling back to previous version"
    mv "$BACKUP_PATH" "$CURRENT_PATH"
    systemctl restart "$SERVICE_NAME"
    if [ $NEEDS_REMOUNT -eq 1 ]; then
        mount -o remount,ro /
    fi
    exit 1
fi

# Verify service is running
log "Waiting to verify service is running properly"
sleep 10
if ! systemctl is-active "$SERVICE_NAME" > /dev/null; then
    log "ERROR: Service failed to start after update"
    log "Rolling back to previous version"
    mv "$BACKUP_PATH" "$CURRENT_PATH"
    systemctl restart "$SERVICE_NAME"
    if [ $NEEDS_REMOUNT -eq 1 ]; then
        mount -o remount,ro /
    fi
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
log "New version is now running"
exit 0
