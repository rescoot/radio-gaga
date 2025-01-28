#!/bin/bash

set -e

BINARY_URL="https://sunshine.rescoot.org/radio-gaga/radio-gaga-arm"
BINARY_PATH="/var/rootdirs/home/root/radio-gaga/radio-gaga"
BINARY_NEW="${BINARY_PATH}.new"
SERVICE_NAME="rescoot-radio-gaga"

# Get remote last-modified time using just headers
REMOTE_MODIFIED=$(curl -sI "$BINARY_URL" | grep -i "last-modified" | cut -d' ' -f2- | sed 's/GMT/+0000/' | tr -d '\r\n')

if [ -f "$BINARY_PATH" ]; then
    # Store current local file's modification time in same format as HTTP header
    LOCAL_MODIFIED=$(date -u -r "$BINARY_PATH" --rfc-2822)
   
    if [ "$REMOTE_MODIFIED" = "$LOCAL_MODIFIED" ]; then
        echo "Binary is up to date"
        exit 0
    fi
fi

echo "Local file: $LOCAL_MODIFIED"
echo "Remote file: $REMOTE_MODIFIED"

# Download to new file
echo "Downloading new binary..."
curl -fsSL -C - -o "$BINARY_NEW" "$BINARY_URL"

# Set permissions on new file
chmod 755 "$BINARY_NEW"

# Preserve the modification time from server
touch -d "$REMOTE_MODIFIED" "$BINARY_NEW"

# Atomic move
mv -f "$BINARY_NEW" "$BINARY_PATH"

echo "Restarting service..."
systemctl restart "$SERVICE_NAME"

echo "Update complete"
