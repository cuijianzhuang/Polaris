#!/usr/bin/env bash
# Polaris agent installer. Served by the panel at /install.sh:
#   curl -fsSL https://panel.example.com/install.sh | bash -s -- \
#     --panel https://panel.example.com:9443 --token <one-time> --ca-sha256 <fingerprint>
set -euo pipefail

PANEL_HTTP="__PANEL_HTTP__"
PANEL="" TOKEN="" CA="" DATA="/var/lib/polaris" MIRROR="" BIN="/usr/local/bin/polaris-agent"
RELEASE_URL="${POLARIS_AGENT_URL:-https://github.com/cuijianzhuang/polaris/releases/latest/download}"

while [ $# -gt 0 ]; do
  case "$1" in
    --panel) PANEL="$2"; shift 2 ;;
    --token) TOKEN="$2"; shift 2 ;;
    --ca-sha256) CA="$2"; shift 2 ;;
    --data) DATA="$2"; shift 2 ;;
    --mirror) MIRROR="$2"; shift 2 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

[ "$(id -u)" = 0 ] || { echo "please run as root" >&2; exit 1; }
[ -n "$PANEL" ] && [ -n "$TOKEN" ] && [ -n "$CA" ] || { echo "--panel, --token and --ca-sha256 are required" >&2; exit 2; }
command -v systemctl >/dev/null || { echo "systemd is required" >&2; exit 1; }

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac

fetch() { # url dest
  if command -v curl >/dev/null; then curl -fsSL --retry 3 -o "$2" "$1"; else wget -qO "$2" "$1"; fi
}

TMP="$(mktemp)"
trap 'rm -f "$TMP"' EXIT
echo "==> downloading polaris-agent ($ARCH)"
if ! fetch "$PANEL_HTTP/download/polaris-agent-linux-$ARCH" "$TMP" 2>/dev/null; then
  fetch "$RELEASE_URL/polaris-agent-linux-$ARCH" "$TMP"
fi
install -m 0755 "$TMP" "$BIN"
mkdir -p "$DATA"
chmod 700 "$DATA"

echo "==> enrolling with $PANEL"
"$BIN" --panel "$PANEL" --token "$TOKEN" --ca-sha256 "$CA" --data "$DATA" --enroll-only

EXTRA=""
[ -n "$MIRROR" ] && EXTRA="--mirror $MIRROR"
cat > /etc/systemd/system/polaris-agent.service <<UNIT
[Unit]
Description=Polaris node agent
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=$BIN --data $DATA $EXTRA
Restart=always
RestartSec=3
LimitNOFILE=1048576
AmbientCapabilities=CAP_NET_BIND_SERVICE CAP_NET_ADMIN
KillMode=mixed

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable --now polaris-agent
echo "==> polaris-agent is running. Logs: journalctl -u polaris-agent -f"
