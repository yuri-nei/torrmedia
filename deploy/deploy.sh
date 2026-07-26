#!/bin/bash
set -euo pipefail
SRC="$(cd "$(dirname "$0")" && pwd)"

echo "== 1. system user =="
if ! id -u torrmedia &>/dev/null; then
  useradd -r -M -d /nonexistent -s /usr/sbin/nologin -G qbittorrent torrmedia
fi
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
EOF
fi
chown root:torrmedia /etc/torrmedia/config.env
chmod 0640 /etc/torrmedia/config.env

echo "== 3. binary =="
install -m 0755 -o root -g root "$SRC/torrmedia" /usr/local/bin/torrmedia

echo "== 4. unit + hardening drop-in =="
install -m 0644 "$SRC/torrmedia.service" /etc/systemd/system/torrmedia.service
install -d /etc/systemd/system/torrmedia.service.d
install -m 0644 "$SRC/hardening.conf" /etc/systemd/system/torrmedia.service.d/hardening.conf

echo "== 5. permissions for DELETE: g+w on directories and existing qBit files, UMask 0002 =="
find /var/media -type d -exec chmod g+w {} +
# UMask affects only files created after the qBittorrent restart. Retrofit
# existing media owned by qBittorrent so torrmedia (SupplementaryGroups=
# qbittorrent) can open the inode O_RDWR for the sparse-trim workflow.
find /var/media/library -type f \
  -user qbittorrent -group qbittorrent ! -perm -g+w \
  -exec chmod g+w {} +
QBIT_RESTART=0
if grep -q '^UMask=0022' /etc/systemd/system/qbittorrent.service; then
  sed -i 's/^UMask=0022/UMask=0002/' /etc/systemd/system/qbittorrent.service
  QBIT_RESTART=1
fi

systemctl daemon-reload
if [ "$QBIT_RESTART" = 1 ]; then
  echo "== restart qbittorrent (UMask changed) =="
  systemctl restart qbittorrent
fi
systemctl enable --now torrmedia
systemctl restart torrmedia

sleep 1
echo "== status =="
systemctl --no-pager --lines=5 status torrmedia qbittorrent | grep -E "●|Active:|Main PID" || true
ss -tln | grep 8091 || echo "WARN: 8091 not listening"
