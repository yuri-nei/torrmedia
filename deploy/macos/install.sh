#!/bin/bash
# Installs torrmedia on macOS as a LaunchAgent of the current user, so it runs
# in the same session and with the same file access as qBittorrent.app. No
# root, no extra user, no umask changes.
#
#   bash install.sh             install or update to the newest version in SHA256SUMS
#   bash install.sh uninstall   stop and remove the agent; config, viewed marks
#                               and the binary stay in place
#
# Everything lives in ~/Library/Application Support/torrmedia, the log in
# ~/Library/Logs/torrmedia.log. Firewall and network exposure are yours to
# decide, see README.
set -euo pipefail
SRC="$(cd "$(dirname "$0")" && pwd)"
LABEL=io.github.yuri-nei.torrmedia
APP="$HOME/Library/Application Support/torrmedia"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
LOG="$HOME/Library/Logs/torrmedia.log"
DOMAIN="gui/$(id -u)"

[ "$(uname -s)" = Darwin ] || { echo "this script is for macOS; on Linux use deploy/linux/install.sh" >&2; exit 1; }
[ "$(id -u)" != 0 ] || { echo "run it as your own user, not with sudo" >&2; exit 1; }

if [ "${1:-}" = uninstall ]; then
  launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true
  rm -f "$PLIST"
  echo "agent removed; $APP is left in place"
  exit 0
fi

# setting KEY prints the value of KEY from config.env without its quotes.
setting() {
  sed -n "s/^[[:space:]]*$1[[:space:]]*=[[:space:]]*//p" "$APP/config.env" | tail -1 |
    sed -e 's/[[:space:]]*$//' -e 's/^"\(.*\)"$/\1/' -e "s/^'\(.*\)'\$/\1/"
}

echo "== 0. pick the artifact =="
if [ ! -f "$SRC/SHA256SUMS" ]; then
  echo "SHA256SUMS not found next to install.sh, nothing to verify (build with deploy/build.sh macos)" >&2
  exit 1
fi
if [ -z "${TORRMEDIA_VERSION:-}" ]; then
  TORRMEDIA_VERSION="$(awk '{print $2}' "$SRC/SHA256SUMS" | sed 's/^torrmedia-//' | sort -V | tail -1)"
fi
BINARY="torrmedia-$TORRMEDIA_VERSION"
if [ ! -f "$SRC/$BINARY" ]; then
  echo "no artifact $BINARY (see SHA256SUMS)" >&2
  exit 1
fi
echo "version: $TORRMEDIA_VERSION"
( cd "$SRC" && grep " $BINARY\$" SHA256SUMS | shasum -a 256 -c - ) \
  || { echo "checksum of $BINARY does not match, installation aborted" >&2; exit 1; }
ARCH="$(uname -m)"
file "$SRC/$BINARY" | grep -q "Mach-O.*$ARCH" \
  || { echo "$BINARY is not a macOS $ARCH binary (build with deploy/build.sh macos)" >&2; exit 1; }

echo "== 1. config (created only if missing, so a filled-in password is never overwritten) =="
install -d -m 0700 "$APP"
if [ ! -f "$APP/config.env" ]; then
  cat > "$APP/config.env" <<EOF
TORRMEDIA_LISTEN=:8091
TORRMEDIA_ROOT="$HOME/Movies/torrmedia"
QBIT_URL=http://127.0.0.1:8080
QBIT_USER=admin
QBIT_PASS=CHANGE_ME

# TORRMEDIA_TOKEN=
EOF
  chmod 0600 "$APP/config.env"
  echo "created $APP/config.env"
  echo "set QBIT_USER/QBIT_PASS (qBittorrent Web UI) and TORRMEDIA_ROOT (qBittorrent's download folder), then run this script again"
  exit 0
fi
if [ "$(setting QBIT_PASS)" = CHANGE_ME ]; then
  echo "set QBIT_PASS in $APP/config.env first" >&2
  exit 1
fi
ROOT_DIR="$(setting TORRMEDIA_ROOT)"
if [ ! -d "$ROOT_DIR" ]; then
  echo "TORRMEDIA_ROOT=$ROOT_DIR does not exist" >&2
  exit 1
fi
case "$ROOT_DIR" in
  "$HOME/Desktop"*|"$HOME/Documents"*|"$HOME/Downloads"*|/Volumes/*)
    echo "WARN: macOS privacy protection may block a background agent from $ROOT_DIR."
    echo "      Either keep the library elsewhere (e.g. ~/Movies) or allow Full Disk Access for"
    echo "      $APP/torrmedia in System Settings > Privacy & Security."
    ;;
esac

echo "== 2. binary =="
install -m 0755 "$SRC/$BINARY" "$APP/torrmedia"

echo "== 3. launch agent =="
install -d "$HOME/Library/LaunchAgents" "$HOME/Library/Logs"
cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>$LABEL</string>
	<key>ProgramArguments</key>
	<array>
		<string>$APP/torrmedia</string>
		<string>-config</string>
		<string>$APP/config.env</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>StandardOutPath</key>
	<string>$LOG</string>
	<key>StandardErrorPath</key>
	<string>$LOG</string>
</dict>
</plist>
EOF
plutil -lint -s "$PLIST"

launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true
# bootout returns before the old instance is fully gone; bootstrap can fail
# with "Input/output error" for a moment after it.
for attempt in 1 2 3 4 5; do
  if launchctl bootstrap "$DOMAIN" "$PLIST" 2>/dev/null; then
    break
  fi
  [ "$attempt" = 5 ] && { echo "launchctl bootstrap failed, see: launchctl print $DOMAIN/$LABEL" >&2; exit 1; }
  sleep 1
done

sleep 1
echo "== status =="
LISTEN="$(setting TORRMEDIA_LISTEN)"
PORT="${LISTEN##*:}"
PORT="${PORT:-8091}"
HEALTH="$(curl -sS --max-time 3 "http://127.0.0.1:$PORT/health" 2>/dev/null || true)"
if [ -n "$HEALTH" ]; then
  echo "port $PORT: $HEALTH"
else
  echo "WARN: nothing answers on port $PORT yet, see $LOG"
fi
echo
echo "Done. Log: $LOG. Remove with: bash install.sh uninstall"
echo "Keep the Mac awake (System Settings > Energy) if other devices stream from it."
