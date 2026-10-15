#!/usr/bin/env bash
# Polaris agent installer. Served by the panel at /install.sh:
#   curl -fsSL https://panel.example.com/install.sh | bash -s -- \
#     --panel https://panel.example.com:9443 --token <one-time> --ca-sha256 <fingerprint>
set -euo pipefail

PANEL_HTTP="__PANEL_HTTP__"
PANEL="" TOKEN="" CA="" DATA="/var/lib/polaris" MIRROR="" BIN="/usr/local/bin/polaris-agent"
PANEL_VERSION="__PANEL_VERSION__"
SHA_amd64="__SHA_AMD64__"
SHA_arm64="__SHA_ARM64__"
if [ "$PANEL_VERSION" = "dev" ]; then
  RELEASE_URL="${POLARIS_AGENT_URL:-https://github.com/cuijianzhuang/polaris/releases/latest/download}"
else # same version as the panel
  RELEASE_URL="${POLARIS_AGENT_URL:-https://github.com/cuijianzhuang/polaris/releases/download/$PANEL_VERSION}"
fi

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

command -v sha256sum >/dev/null || { echo "sha256sum is required" >&2; exit 1; }
TMP="$(mktemp)"
SUMS="$(mktemp)"
trap 'rm -f "$TMP" "$SUMS"' EXIT
eval "WANT=\$SHA_$ARCH"
echo "==> downloading polaris-agent ($ARCH)"
if [ -n "$WANT" ] && fetch "$PANEL_HTTP/download/polaris-agent-linux-$ARCH" "$TMP" 2>/dev/null; then
  : # checksum published by the panel together with this script
else
  fetch "$RELEASE_URL/polaris-agent-linux-$ARCH" "$TMP"
  fetch "$RELEASE_URL/checksums.txt" "$SUMS"
  WANT="$(awk -v f="polaris-agent-linux-$ARCH" '$2 == f || $2 == "*"f {print $1}' "$SUMS")"
fi
GOT="$(sha256sum "$TMP" | awk '{print $1}')"
if [ -z "$WANT" ] || [ "$GOT" != "$WANT" ]; then
  echo "checksum mismatch for polaris-agent-linux-$ARCH (want ${WANT:-none}, got $GOT); aborting" >&2
  exit 1
fi
echo "==> sha256 verified ($GOT)"
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
