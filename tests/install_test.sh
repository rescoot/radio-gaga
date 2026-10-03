#!/bin/bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
mkdir "$TMP/bin"

cat > "$TMP/bin/curl" <<'MOCK'
#!/bin/bash
set -euo pipefail
output=
url=
while (( $# )); do
  case "$1" in
    -o) output=$2; shift 2 ;;
    https://*) url=$1; shift ;;
    *) shift ;;
  esac
done
printf '%s\n' "$url" > "$CURL_CALL"
if [[ "${DOWNLOAD_FAIL:-0}" == 1 ]]; then
  exit 22
fi
printf '%s\n' '#!/bin/sh' 'echo ran > "$INSTALL_MARKER"' 'exit "${INSTALL_STATUS:-0}"' > "$output"
MOCK
chmod +x "$TMP/bin/curl"
export PATH="$TMP/bin:$PATH" CURL_CALL="$TMP/curl-call" INSTALL_MARKER="$TMP/installed"
export USER_TOKEN= BOOTSTRAP_TOKEN=

TOKEN=abcdefghijklmnopqrstuvwxyz0123456789_-ABCDE
BOOTSTRAP_TOKEN="$TOKEN" bash "$ROOT/install.sh" --force > "$TMP/output"
[[ "$(<"$CURL_CALL")" == "https://sunshine.rescoot.org/install/u/$TOKEN" ]]
[[ -f "$INSTALL_MARKER" ]]
grep -q 'Accept any pending scooter claim' "$TMP/output"

rm "$CURL_CALL" "$INSTALL_MARKER"
SUNSHINE_URL=https://sunshine.example.org/ BOOTSTRAP_TOKEN=FGUHG1 bash "$ROOT/install.sh" --force > "$TMP/output"
[[ "$(<"$CURL_CALL")" == https://sunshine.example.org/install/u/FGUHG1 ]]
[[ -f "$INSTALL_MARKER" ]]

for token in 'not-a-token' 'FGUHG1/extra' 'secret token'; do
  rm -f "$CURL_CALL" "$INSTALL_MARKER"
  if BOOTSTRAP_TOKEN="$token" bash "$ROOT/install.sh" --force > "$TMP/output" 2>&1; then
    echo "Invalid token was accepted" >&2
    exit 1
  fi
  [[ ! -f "$CURL_CALL" && ! -f "$INSTALL_MARKER" ]]
done

if USER_TOKEN=api-token bash "$ROOT/install.sh" --force > "$TMP/output" 2>&1; then
  echo "User API token was accepted" >&2
  exit 1
fi
grep -q 'Use BOOTSTRAP_TOKEN' "$TMP/output"
[[ ! -f "$CURL_CALL" ]]

if SUNSHINE_URL=http://sunshine.example.org BOOTSTRAP_TOKEN=FGUHG1 bash "$ROOT/install.sh" --force > "$TMP/output" 2>&1; then
  echo "Insecure URL was accepted" >&2
  exit 1
fi
[[ ! -f "$CURL_CALL" ]]

if DOWNLOAD_FAIL=1 BOOTSTRAP_TOKEN=FGUHG1 bash "$ROOT/install.sh" --force > "$TMP/output" 2>&1; then
  echo "Download failure was ignored" >&2
  exit 1
fi
[[ ! -f "$INSTALL_MARKER" ]]

if INSTALL_STATUS=7 BOOTSTRAP_TOKEN=FGUHG1 bash "$ROOT/install.sh" --force > "$TMP/output" 2>&1; then
  echo "Installer failure was ignored" >&2
  exit 1
fi

bash "$ROOT/install.sh" --help > "$TMP/output"
grep -q BOOTSTRAP_TOKEN "$TMP/output"
echo 'Installer tests passed (no scooter commands or network requests executed).'
