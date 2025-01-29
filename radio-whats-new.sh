#!/bin/bash

set -e

BINARY_URL="https://sunshine.rescoot.org/radio-gaga/radio-gaga-arm"
BINARY_PATH="/var/rootdirs/home/root/radio-gaga/radio-gaga"
BINARY_NEW="${BINARY_PATH}.new"
SERVICE_NAME="rescoot-radio-gaga"

REMOTE_MODIFIED=$(curl -sI "$BINARY_URL" | grep -i "last-modified" | cut -d' ' -f2- | sed 's/GMT/+0000/' | tr -d '\r\n')

if [ -f "$BINARY_PATH" ]; then
    LOCAL_MODIFIED=$(date -u -r "$BINARY_PATH" --rfc-2822)
   
    if [ "$REMOTE_MODIFIED" = "$LOCAL_MODIFIED" ]; then
        echo "Binary is up to date"
        exit 0
    fi
fi

echo "Local file: $LOCAL_MODIFIED"
echo "Remote file: $REMOTE_MODIFIED"

echo "Downloading new binary..."
curl -fsSL -C - -o "$BINARY_NEW" "$BINARY_URL"

chmod 755 "$BINARY_NEW"

touch -d "$REMOTE_MODIFIED" "$BINARY_NEW"

mv -f "$BINARY_NEW" "$BINARY_PATH"

echo "Restarting service..."
systemctl restart "$SERVICE_NAME"

echo "Update complete"
