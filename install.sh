#!/bin/sh
# pylon-beacon installer (Linux) — https://pylonmon.com/docs#beacon
# Usage:  curl -fsSL https://pylonmon.com/beacon.sh | sh
#         PYLON_KEY=pm_xxx curl -fsSL https://pylonmon.com/beacon.sh | sh   (non-interactive)
set -e

REPO="joshuaGlass808/pylon-beacon"
BIN="/usr/local/bin/pylon-beacon"
CONF="/etc/pylon-beacon.conf"
UNIT="/etc/systemd/system/pylon-beacon.service"

if [ "$(id -u)" != "0" ]; then
  echo "please run as root:  curl -fsSL https://pylonmon.com/beacon.sh | sudo sh"
  exit 1
fi

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "unsupported arch: $(uname -m) — build from source: go build github.com/$REPO"; exit 1 ;;
esac

echo "-> downloading pylon-beacon (linux/$ARCH)…"
curl -fsSL -o "$BIN" "https://github.com/$REPO/releases/latest/download/pylon-beacon-linux-$ARCH"
chmod +x "$BIN"

if [ ! -f "$CONF" ]; then
  KEY="${PYLON_KEY:-}"
  if [ -z "$KEY" ]; then
    printf "PylonMon API key (ingest-scoped; Settings -> Admin -> Status page & API): "
    read -r KEY </dev/tty
  fi
  cat > "$CONF" <<EOF
# pylon-beacon — https://pylonmon.com/docs#beacon
key      = $KEY
url      = ${PYLON_URL:-https://pylonmon.com}
# node   = $(hostname)        # uncomment to override the monitor name
interval = 20

[custom]
# name = command   (first number in the output becomes the metric)
# gpu_temp_c = nvidia-smi --query-gpu=temperature.gpu --format=csv,noheader
EOF
  chmod 600 "$CONF"
  echo "-> wrote $CONF"
else
  echo "-> keeping existing $CONF"
fi

cat > "$UNIT" <<EOF
[Unit]
Description=pylon-beacon — pushes node vitals to PylonMon
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=$BIN
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now pylon-beacon
echo ""
echo "✓ pylon-beacon is running. Your node appears in PylonMon within a minute."
echo "  status:  systemctl status pylon-beacon"
echo "  logs:    journalctl -u pylon-beacon -f"
echo "  config:  $CONF"
