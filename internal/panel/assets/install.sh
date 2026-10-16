#!/usr/bin/env bash
# Polaris agent installer, served by the panel at /install.sh. The panel fills
# in its own agent endpoint and CA fingerprint, so a node only needs a token:
#
#   curl -fsSL https://panel.example.com/install.sh | bash -s -- --token <one-time>
#   curl -fsSL https://panel.example.com/j/<one-time> | bash     (one-click link)
#
# Other uses:
#   … | bash -s -- --upgrade              replace the binary, keep the enrolment
#   … | bash -s -- --uninstall [--purge]  remove the agent (--purge: also its data)
# Options: --mirror <prefix> (GitHub mirror for agent and cores), --data <dir>,
#          --panel <url> / --ca-sha256 <hex> (override the embedded values),
#          --local (connect over 127.0.0.1; automatic on the panel server).
set -euo pipefail

PANEL_HTTP="__PANEL_HTTP__"
PANEL="__PANEL_AGENT__"
CA="__PANEL_CA__"
PANEL_VERSION="__PANEL_VERSION__"
SHA_amd64="__SHA_AMD64__"
SHA_arm64="__SHA_ARM64__"
TOKEN="__PANEL_TOKEN__" DATA="" MIRROR="" MODE="install" PURGE="" LOCAL=""
BIN="/usr/local/bin/polaris-agent"
UNIT="/etc/systemd/system/polaris-agent.service"

if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  R=$'\e[31m' G=$'\e[32m' Y=$'\e[33m' C=$'\e[36m' D=$'\e[2m' B=$'\e[1m' N=$'\e[0m'
else
  R="" G="" Y="" C="" D="" B="" N=""
fi
STEP_N=0 STEP_TOTAL=0
step() {
  STEP_N=$((STEP_N + 1))
  if [ "$STEP_TOTAL" -gt 0 ]; then echo "  ${C}[$STEP_N/$STEP_TOTAL]${N} ${B}$*${N}"; else echo "  ${C}==>${N} ${B}$*${N}"; fi
}
ok() { echo "  ${G}✓${N} $*"; }
info() { echo "    ${D}$*${N}"; }
warn() { echo "  ${Y}!${N} $*" >&2; }
die() { echo "  ${R}✗ $*${N}" >&2; exit 1; }
line() { printf '  %s%s%s\n' "$1" "────────────────────────────────────────────────────────────────" "$N"; }
# kv LABEL VALUE: aligned line; CJK labels are 2 columns per character
# (width = chars + (bytes - chars) / 2 in UTF-8, whatever the locale).
kv() {
  local LC_ALL=C l="$1" b c
  b=${#l}; c=${l//[$'\x80'-$'\xbf']/}; c=${#c}
  printf '  %s%s%*s%s  %s\n' "$D" "$1" $((8 - c - (b - c) / 2)) '' "$N" "$2"
}

while [ $# -gt 0 ]; do
  case "$1" in
    --token) TOKEN="${2:-}"; shift 2 ;;
    --panel) PANEL="${2:-}"; shift 2 ;;
    --ca-sha256) CA="${2:-}"; shift 2 ;;
    --data) DATA="${2:-}"; shift 2 ;;
    --mirror) MIRROR="${2:-}"; shift 2 ;;
    --upgrade) MODE="upgrade"; shift ;;
    --uninstall) MODE="uninstall"; shift ;;
    --purge) PURGE=1; shift ;;
    --local) LOCAL=1; shift ;;
    -h|--help) sed -n '2,13p' "$0" 2>/dev/null || true; exit 0 ;;
    *) die "未知参数：$1" ;;
  esac
done

# Agent data lives apart from the panel's /var/lib/polaris, so a node on the
# panel server can never touch the panel database. Agents installed before
# this layout keep their old directory.
if [ -z "$DATA" ]; then
  if [ -f /var/lib/polaris/agent/panel-url ]; then DATA=/var/lib/polaris; else DATA=/var/lib/polaris-agent; fi
fi

[ "$(id -u)" = 0 ] || die "请以 root 运行（sudo -i 后再执行）"
command -v systemctl >/dev/null || die "需要 systemd"

if [ "$MODE" = uninstall ]; then
  step "卸载 polaris-agent"
  systemctl disable --now polaris-agent 2>/dev/null || true
  rm -f "$UNIT" "$BIN"
  systemctl daemon-reload
  if [ -n "$PURGE" ]; then
    if ls "$DATA"/*.db >/dev/null 2>&1; then
      rm -rf "$DATA/agent" "$DATA/cores" "$DATA/run" # shared with the panel: agent parts only
      ok "已卸载并删除 agent 数据（$DATA 中的主控数据保持不动）"
    else
      rm -rf "$DATA"
      ok "已卸载并删除数据目录 $DATA"
    fi
  else
    ok "已卸载；数据保留在 $DATA（加 --purge 一并删除）"
  fi
  info "记得在面板中删除该节点。"
  exit 0
fi

ENROLLED=""
[ -f "$DATA/agent/panel-url" ] && ENROLLED=1
if [ "$MODE" = install ] && [ -z "$TOKEN" ]; then
  if [ -n "$ENROLLED" ]; then
    MODE="upgrade" # re-running without a token on an enrolled node: just upgrade
  else
    die "缺少 --token（在面板「节点」中生成安装命令）"
  fi
fi
[ "$MODE" = upgrade ] && [ -z "$ENROLLED" ] && die "本机尚未注册，无法升级；请使用带 --token 的安装命令"

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "不支持的架构 $(uname -m)（仅 amd64 / arm64）" ;;
esac
command -v sha256sum >/dev/null || die "缺少 sha256sum（coreutils）"
if command -v curl >/dev/null; then
  fetch() { curl -fsSL --retry 3 --connect-timeout 10 -o "$2" "$1"; }
elif command -v wget >/dev/null; then
  fetch() { wget -q -T 15 -t 3 -O "$2" "$1"; }
else
  die "需要 curl 或 wget"
fi

# install: check panel, download, enrol, start; upgrade: download, start.
if [ "$MODE" = install ]; then STEP_TOTAL=4; else STEP_TOTAL=2; fi
echo
echo "  ${C}${B}Polaris 节点 Agent${N}  ${D}$([ "$MODE" = install ] && echo 安装 || echo 升级) · $ARCH · 版本 $PANEL_VERSION${N}"
line "$C"
kv 主机 "$(hostname 2>/dev/null) · $(. /etc/os-release 2>/dev/null && echo "${PRETTY_NAME:-Linux}")"
kv 主控 "${PANEL:-未指定}"
kv 数据目录 "$DATA"
echo

# The agent dials the panel; check the endpoint before touching anything so
# a closed firewall port is reported plainly instead of as an enrol error.
if [ "$MODE" = install ]; then
  [ -n "$PANEL" ] && [ -n "$CA" ] || die "缺少主控地址或 CA 指纹（--panel / --ca-sha256）"
  hp="${PANEL#*://}"; hp="${hp%%/*}"
  host="${hp%:*}"; port="${hp##*:}"
  [ "$port" = "$hp" ] && port=443
  host="${host#[}"; host="${host%]}"
  # On the panel server itself, talk to it over loopback: no firewall rule or
  # public address needed (the panel's agent certificate covers 127.0.0.1).
  if [ -z "$LOCAL" ] && [ -f /etc/polaris/polaris.env ] && command -v timeout >/dev/null &&
    timeout 3 bash -c ">/dev/tcp/127.0.0.1/$port" 2>/dev/null; then
    LOCAL=1
    info "检测到本机运行着 Polaris 主控，节点将通过 127.0.0.1 连接"
  fi
  if [ -n "$LOCAL" ]; then
    host=127.0.0.1
    PANEL="https://127.0.0.1:$port"
  fi
  step "检查与主控的连接 $host:$port"
  if command -v timeout >/dev/null && ! timeout 8 bash -c ">/dev/tcp/$host/$port" 2>/dev/null; then
    die "无法连接主控 $host:$port。请确认：1) 主控服务器防火墙 / 安全组已放行 TCP $port；2) 地址正确（面板设置 POLARIS_AGENT_HOSTS / POLARIS_PUBLIC_URL）"
  fi
  ok "主控可达"
fi

if [ "$PANEL_VERSION" = dev ]; then
  RELEASE_URL="${POLARIS_AGENT_URL:-https://github.com/cuijianzhuang/polaris/releases/latest/download}"
else # same version as the panel
  RELEASE_URL="${POLARIS_AGENT_URL:-https://github.com/cuijianzhuang/polaris/releases/download/$PANEL_VERSION}"
fi
[ -n "$MIRROR" ] && case "$RELEASE_URL" in https://github.com/*) RELEASE_URL="${MIRROR%/}/$RELEASE_URL" ;; esac

TMP="$(mktemp)" SUMS="$(mktemp)"
trap 'rm -f "$TMP" "$SUMS"' EXIT
case "$ARCH" in amd64) WANT="$SHA_amd64" ;; arm64) WANT="$SHA_arm64" ;; esac
step "下载 polaris-agent（$ARCH，版本 $PANEL_VERSION）"
# The panel caches only its own architecture up front; ask it to fetch this
# one (it may reach GitHub when the node cannot).
if [ -z "$WANT" ] && fetch "$PANEL_HTTP/download/polaris-agent-linux-$ARCH.sha256" "$SUMS" 2>/dev/null; then
  WANT="$(tr -dc '0-9a-f' < "$SUMS" | head -c 64)"
  [ ${#WANT} = 64 ] || WANT=""
fi
if [ -n "$WANT" ] && fetch "$PANEL_HTTP/download/polaris-agent-linux-$ARCH" "$TMP" 2>/dev/null; then
  info "来源：主控 $PANEL_HTTP"
else
  info "来源：$RELEASE_URL"
  fetch "$RELEASE_URL/polaris-agent-linux-$ARCH" "$TMP" || die "下载失败；国内服务器可加 --mirror https://ghfast.top/ 等 GitHub 镜像"
  fetch "$RELEASE_URL/checksums.txt" "$SUMS" || die "下载 checksums.txt 失败"
  WANT="$(awk -v f="polaris-agent-linux-$ARCH" '$2 == f || $2 == "*"f {print $1}' "$SUMS")"
fi
GOT="$(sha256sum "$TMP" | awk '{print $1}')"
[ -n "$WANT" ] && [ "$GOT" = "$WANT" ] || die "SHA256 校验失败（期望 ${WANT:-无}，实际 $GOT），已中止"
ok "SHA256 校验通过"

systemctl stop polaris-agent 2>/dev/null || true
install -m 0755 "$TMP" "$BIN"
if [ ! -d "$DATA" ]; then
  mkdir -p "$DATA"
  chmod 700 "$DATA"
fi

if [ "$MODE" = install ]; then
  step "向主控注册"
  "$BIN" --panel "$PANEL" --token "$TOKEN" --ca-sha256 "$CA" --data "$DATA" --enroll-only ||
    die "注册失败：令牌可能已使用或过期（24 小时有效、一次性），请在面板重新生成安装命令"
  ok "注册成功"
fi
# Remember the panel's HTTP address: `polaris` upgrades the agent from it.
mkdir -p "$DATA/agent" && printf '%s\n' "$PANEL_HTTP" > "$DATA/agent/panel-http" 2>/dev/null || true

# Install the polaris management command (the same tool as on the panel) so
# the node can be managed with `polaris`; never replace an existing copy.
CTL=/usr/local/bin/polaris
if ! { [ -f "$CTL" ] && head -c 2 "$CTL" | grep -q '#!'; }; then
  CTL_TMP="$(mktemp)"
  if fetch "$PANEL_HTTP/download/install-panel.sh" "$CTL_TMP" 2>/dev/null || fetch "$RELEASE_URL/install-panel.sh" "$CTL_TMP" 2>/dev/null; then
    if head -c 2 "$CTL_TMP" | grep -q '#!' && grep -q 'polaris' "$CTL_TMP"; then
      install -m 0755 "$CTL_TMP" "$CTL" && ln -sf "$CTL" /usr/local/bin/polaris-ctl
    fi
  fi
  rm -f "$CTL_TMP"
fi

EXTRA=""
if [ -n "$MIRROR" ]; then
  EXTRA="--mirror $MIRROR"
elif [ "$MODE" = upgrade ] && [ -f "$UNIT" ]; then
  EXTRA="$(sed -n 's/^ExecStart=.* \(--mirror [^ ]*\).*/\1/p' "$UNIT")" # keep the mirror chosen at install
fi
cat > "$UNIT" <<EOF
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
EOF
step "启动 polaris-agent 服务"
systemctl daemon-reload
systemctl enable polaris-agent >/dev/null 2>&1
systemctl restart polaris-agent

for _ in 1 2 3 4 5 6; do
  sleep 1
  systemctl is-active --quiet polaris-agent || break
done
if ! systemctl is-active --quiet polaris-agent; then
  journalctl -u polaris-agent -n 20 --no-pager >&2 || true
  die "polaris-agent 启动失败（日志见上）"
fi
ok "服务已启动"
echo
ok "${B}polaris-agent $("$BIN" --version | awk '{print $2}') 运行中${N}"
line "$G"
if [ "$MODE" = install ]; then
  kv 下一步 "回到面板，节点会在几秒内显示为在线；内核按需自动下载"
fi
kv 主控 "$(cat "$DATA/agent/panel-url" 2>/dev/null || echo "$PANEL")"
kv 数据目录 "$DATA"
if [ -x /usr/local/bin/polaris ]; then
  kv 管理 "输入 ${B}polaris${N} 打开管理菜单（状态、日志、升级、更换主控、卸载）"
else
  kv 查看日志 "journalctl -u polaris-agent -f"
  kv 升级 "重新执行安装命令并加 --upgrade"
  kv 卸载 "重新执行安装命令并加 --uninstall [--purge]"
fi
echo
