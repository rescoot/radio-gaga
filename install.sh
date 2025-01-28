#!/bin/bash
set -e

error_exit() {
  echo "Error: $1" >&2
  exit 1
}

FORCE=false
for arg in "$@"; do
  [[ "$arg" == "--force" ]] && FORCE=true
done

if [[ $EUID -ne 0 ]] && [[ "$FORCE" != true ]]; then
  error_exit "This script must be run as root."
fi

if [[ ! "$(hostname)" =~ ^mdb-.*[0-9]+$ ]] && [[ "$FORCE" != true ]]; then
  error_exit "This script must be run on an unu Scooter Pro MDB."
fi

if ! grep -q "scooterOS" /etc/issue && [[ "$FORCE" != true ]]; then
  error_exit "This script must be run on an unu Scooter Pro (scooterOS)."
fi

if [[ "$FORCE" != true ]]; then
  cd || error_exit "Failed to change to home directory."
fi

if [[ "$(pwd)" != "/var/rootdirs/home/root" ]] && [[ "$FORCE" != true ]]; then
  error_exit "Current working directory is not '/var/rootdirs/home/root'. This does not look like an unu Scooter Pro MDB environment."
fi

if [[ -z "$USER_TOKEN" ]]; then
  echo "Please enter a user-scope token from the Sunshine cloud to generate the scooter config."
  echo "You can create a token at https://sunshine.rescoot.org/account"
  read -rsp "Token: " USER_TOKEN
  echo
fi

if [[ -z "$USER_TOKEN" ]]; then
  error_exit "Token cannot be empty."
fi

if [[ -z "$VIN" ]]; then

  echo "Fetching scooter information..."
  SCOOTERS_RESPONSE=$(curl -s -H "Authorization: Bearer ${USER_TOKEN}" "https://sunshine.rescoot.org/api/v1/scooters/")

  if [[ "$SCOOTERS_RESPONSE" == "[]" || -z "$SCOOTERS_RESPONSE" ]]; then
    error_exit "No scooters found. Please create a scooter at https://sunshine.rescoot.org first."
  fi

  SCOOTER_COUNT=$(echo "$SCOOTERS_RESPONSE" | grep -o '"vin"' | wc -l)

  if [[ "$SCOOTER_COUNT" -eq 1 ]]; then
    VIN=$(echo "$SCOOTERS_RESPONSE" | grep -o '"vin":"[^"]*"' | cut -d'"' -f4)
    NAME=$(echo "$SCOOTERS_RESPONSE" | grep -o '"name":"[^"]*"' | cut -d'"' -f4)
    echo "Found scooter: $NAME (VIN: $VIN)"
    read -rp "Continue with this scooter? [Y/n] " CONFIRM
    if [[ "$CONFIRM" =~ ^[Nn] ]]; then
      error_exit "Installation cancelled by user."
    fi
  else
    echo "Multiple scooters found. Please choose a VIN from the following list:"
    VIN_ENTRY=""
    NAME_ENTRY=""
    echo "$SCOOTERS_RESPONSE" | grep -oE '"(vin|name)":"[^"]*"' | while read -r line; do
      if [[ "$line" == *'"name":"'* ]]; then NAME_ENTRY=$(echo $line | cut -d'"' -f4); fi
      if [[ "$line" == *'"vin":"'* ]]; then VIN_ENTRY=$(echo $line | cut -d'"' -f4); fi
      if [[ -n "$VIN_ENTRY" && -n "$NAME_ENTRY" ]]; then
        echo "- $NAME_ENTRY: $VIN_ENTRY"
        VIN_ENTRY=""
        NAME_ENTRY=""
      fi
    done
    
    read -rp "Enter VIN: " VIN
    
    if ! echo "$SCOOTERS_RESPONSE" | grep -q "\"vin\":\"$VIN\""; then
      error_exit "Invalid VIN. Please enter a VIN from the list above."
    fi
  fi
fi

echo "Creating 'radio-gaga' directory..."
mkdir -p radio-gaga
cd radio-gaga

echo "Downloading config for scooter ${VIN}..."
curl -s -H "Authorization: Bearer ${USER_TOKEN}" "https://sunshine.rescoot.org/api/v1/scooters/${VIN}/config.yml" > config.yml

echo "Downloading supplemental files..."
curl -s -L -o radio-whats-new.sh https://raw.githubusercontent.com/teal-bauer/reunu-radio-gaga/refs/heads/main/radio-whats-new.sh
curl -s -L -o rescoot-radio-gaga.service https://raw.githubusercontent.com/teal-bauer/reunu-radio-gaga/refs/heads/main/rescoot-radio-gaga.service

echo "Creating systemd service unit..."
SYSTEMD_EDITOR=tee systemctl edit rescoot-radio-gaga.service --force --full < ./rescoot-radio-gaga.service

echo "Fetching telemetry client..."
chmod +x radio-whats-new.sh
bash -e radio-whats-new.sh

echo "Enabling the service unit..."
systemctl enable rescoot-radio-gaga

echo "Checking the service..."
systemctl restart rescoot-radio-gaga
if systemctl is-active --quiet rescoot-radio-gaga; then
  echo "Service 'rescoot-radio-gaga' started successfully."
else
  error_exit "Failed to start the service 'rescoot-radio-gaga'."
fi

echo "Installation completed successfully."
