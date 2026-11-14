#!/bin/bash
# Deploys torrmedia to a server. Idempotent. Run: sudo bash deploy.sh
#
# It installs EXACTLY the requested version, and only if its checksum matches
# SHA256SUMS. The script used to install an unversioned deploy/torrmedia that
# lagged several versions behind the sources, so a fresh install got old
# code.
#
#   TORRMEDIA_VERSION=2026-07-27.1 sudo -E bash deploy.sh   # explicit version
#   sudo bash deploy.sh                              # latest in SHA256SUMS
#
# The firewall, reverse proxy and token are the installer's responsibility,
# see README. The script does not expose anything to the outside by itself.
set -euo pipefail
SRC="$(cd "$(dirname "$0")" && pwd)"

echo "== 0. pick the artifact =="
if [ ! -f "$SRC/SHA256SUMS" ]; then
  echo "SHA256SUMS not found next to install.sh, nothing to verify" >&2
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
( cd "$SRC" && grep " $BINARY\$" SHA256SUMS | sha256sum -c - ) \
  || { echo "checksum of $BINARY does not match, installation aborted" >&2; exit 1; }

echo "== 1. system user =="
if ! id -u torrmedia &>/dev/null; then
  useradd -r -M -d /nonexistent -s /usr/sbin/nologin torrmedia
fi
# Sparse trim needs access to qBittorrent's files: torrmedia opens an
# existing inode for writing, so it is a member of qBittorrent's group.
usermod -aG qbittorrent torrmedia

echo "== 2. config (created only if missing, so a filled-in password is never overwritten) =="
install -d -m 0750 -o root -g torrmedia /etc/torrmedia
if [ ! -f /etc/torrmedia/config.env ]; then
  cat > /etc/torrmedia/config.env <<'EOF'
TORRMEDIA_LISTEN=:8091
TORRMEDIA_ROOT=/var/media/library
QBIT_URL=http://127.0.0.1:8080
QBIT_USER=admin
QBIT_PASS=CHANGE_ME

# TORRMEDIA_TOKEN enables the Authorization: Bearer <token> check on all
# endpoints except /health. EMPTY by default: the API is open to anyone who
# can reach the port. Either set a token or keep the port behind a
# firewall/VPN. Details in README.
# TORRMEDIA_TOKEN=
# Host names clients use besides IP addresses and *.local, comma-separated:
# TORRMEDIA_HOSTS=
EOF
fi
chown root:torrmedia /etc/torrmedia/config.env
chmod 0640 /etc/torrmedia/config.env

echo "== 3. binary =="
install -m 0755 -o root -g root "$SRC/$BINARY" /usr/local/bin/torrmedia

echo "== 4. unit + hardening drop-in =="
install -m 0644 "$SRC/torrmedia.service" /etc/systemd/system/torrmedia.service
install -d /etc/systemd/system/torrmedia.service.d
install -m 0644 "$SRC/hardening.conf" /etc/systemd/system/torrmedia.service.d/hardening.conf

echo "== 5. permissions for sparse trim: g+w on the library, UMask 0002 for qBittorrent =="
TORRMEDIA_ROOT_DIR="$(. /etc/torrmedia/config.env; echo "${TORRMEDIA_ROOT:-/var/media/library}")"
MEDIA_PARENT="$(dirname "$TORRMEDIA_ROOT_DIR")"
find "$MEDIA_PARENT" -type d -exec chmod g+w {} +
# UMask applies only to files created after qBittorrent restarts. Existing
# files get g+w here, otherwise torrmedia cannot open them O_RDWR.
find "$TORRMEDIA_ROOT_DIR" -type f \
  -user qbittorrent -group qbittorrent ! -perm -g+w \
  -exec chmod g+w {} +

# A drop-in instead of editing the unit with sed: a qBittorrent upgrade will
# not wipe the setting, and it stays visible apart from the packaged file.
install -d /etc/systemd/system/qbittorrent.service.d
QBIT_DROPIN=/etc/systemd/system/qbittorrent.service.d/10-torrmedia.conf
QBIT_RESTART=0
if ! grep -qs '^UMask=0002' "$QBIT_DROPIN"; then
  cat > "$QBIT_DROPIN" <<'EOF'
# Installed by torrmedia: new files must be group-writable, otherwise
# sparse trim cannot open them for writing.
[Service]
UMask=0002
EOF
  chmod 0644 "$QBIT_DROPIN"
  QBIT_RESTART=1
fi

systemctl daemon-reload
if [ "$QBIT_RESTART" = 1 ]; then
  echo "== restart qbittorrent (UMask drop-in added) =="
  systemctl restart qbittorrent
fi
systemctl enable --now torrmedia
systemctl restart torrmedia

sleep 1
echo "== status =="
systemctl --no-pager --lines=5 status torrmedia | grep -E "●|Active:|Main PID" || true
PORT="$(. /etc/torrmedia/config.env; echo "${TORRMEDIA_LISTEN:-:8091}")"
ss -tln | grep -q ":${PORT##*:}" && echo "listening on port ${PORT##*:}" || echo "WARN: port ${PORT##*:} is not listening"
echo
echo "Done. No port was opened to the outside: set up the firewall or VPN yourself (see README)."
echo "Check: curl -fsS http://127.0.0.1:8091/health"
