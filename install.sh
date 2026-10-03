#!/bin/bash
set -euo pipefail

error_exit() {
  echo "Error: $1" >&2
  exit 1
}

SUNSHINE_URL="${SUNSHINE_URL:-https://sunshine.rescoot.org}"
SUNSHINE_URL="${SUNSHINE_URL%/}"
FORCE=false
for arg in "$@"; do
  case "$arg" in
    --force) FORCE=true ;;
    --help|-h)
      echo "Usage: BOOTSTRAP_TOKEN=<token> [SUNSHINE_URL=https://sunshine.rescoot.org] bash install.sh [--force]"
      echo "Generate an installer link in Sunshine Account settings; developer mode is not required."
      echo "If BOOTSTRAP_TOKEN is unset, the script prompts for it."
      echo "--force skips root and platform checks; the installer still needs permission to write system files."
      exit 0
      ;;
    *) error_exit "Unknown option: $arg" ;;
  esac
done

if [[ $EUID -ne 0 && "$FORCE" != true ]]; then
  error_exit "This script must be run as root."
fi

if [[ "$FORCE" != true ]] && ! grep -Eq '^ID="?librescoot' /etc/os-release && ! grep -q 'scooterOS' /etc/issue; then
  error_exit "Run this installer on a Librescoot or ScooterOS scooter."
fi

[[ "$SUNSHINE_URL" == https://* ]] || error_exit "SUNSHINE_URL must use HTTPS."

if [[ -z "${BOOTSTRAP_TOKEN:-}" ]]; then
  if [[ -n "${USER_TOKEN:-}" ]]; then
    error_exit "Use BOOTSTRAP_TOKEN from Sunshine Account settings, not a user API token."
  fi
  echo "Generate an installer link at ${SUNSHINE_URL}/account/security#bootstrap-tokens."
  echo "Copy its bootstrap token. Developer mode is not required."
  read -rsp "Bootstrap token: " BOOTSTRAP_TOKEN </dev/tty || error_exit "Set BOOTSTRAP_TOKEN when no interactive terminal is available."
  echo
fi

[[ "$BOOTSTRAP_TOKEN" =~ ^[A-Za-z0-9_-]{43}$ || "$BOOTSTRAP_TOKEN" =~ ^[A-Za-z0-9]{6}$ ]] ||
  error_exit "Paste only the bootstrap token or six-character installer code."

SCRIPT=$(mktemp)
trap 'rm -f "$SCRIPT"' EXIT

# Sunshine owns distro detection, binary installation and bootstrap configuration.
curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
  "${SUNSHINE_URL}/install/u/${BOOTSTRAP_TOKEN}" -o "$SCRIPT" ||
  error_exit "Could not download the Sunshine installer."

sh "$SCRIPT"
echo "Installation completed. Accept any pending scooter claim in Sunshine to finish setup."
