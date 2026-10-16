#!/usr/bin/env bash
# Polaris panel installer and management tool.
#
# Install (then manage any time by typing: polaris):
#   bash <(curl -fsSL https://raw.githubusercontent.com/cuijianzhuang/Polaris/main/scripts/install-panel.sh)
#
# With no arguments an interactive menu opens. Non-interactive commands:
#   install [--domain NAME] [--no-caddy] [--version TAG] [--port N] [--agent-port N] [--admin-path P]
#   update [--version TAG] [--force]  upgrade binaries, keep config and data (database backed up first)
#   check-update                  compare the installed version with the latest release
#   update-request                run an update the panel asked for (used by polaris-update.service)
#   uninstall [--purge]           remove; --purge also deletes config and data
#   start | stop | restart | status | log
#   info                          admin URL, ports and config summary
#   reset-password [-u NAME]      new random admin password, unlocks the account, signs out sessions
#   reset-2fa [-u NAME]           turn off two-factor authentication
#   set-path PATH | set-port N    change the admin path (/ turns it off) / panel port
#   set-domain NAME               switch to HTTPS on a domain via Caddy
#   set-url URL                   public address used in subscription / install links
#   backup                        config + consistent database copy to /root/polaris-backup-*.tar.gz
#   node [-n NAME]                install a node agent on this server (connects over 127.0.0.1)
#   self-update                   update this management tool to the latest release
#   join LINK                     install the node agent from the panel's one-click link (or move to another panel)
#   agent [status|log|start|stop|restart|upgrade|uninstall [--purge]]   manage the node agent
#   doctor                        check service, ports, firewall, DNS/HTTPS, clock and disk
#
# Options for install/update: --mirror URL (download prefix for github.com)
set -euo pipefail

REPO="cuijianzhuang/Polaris"
BIN=/usr/local/bin/polaris-server   # the panel program (run by systemd)
CTL=/usr/local/bin/polaris          # this tool: the management menu
CTL_ALIAS=/usr/local/bin/polaris-ctl
ETC=/etc/polaris
ENV_FILE=$ETC/polaris.env
DATA=/var/lib/polaris
# Updates requested from the panel (web, Telegram bot, auto update): the panel
# runs unprivileged, so it drops a request file that a root path unit acts on.
UPD_REQ=$DATA/update-request
UPD_RES=$DATA/update-result
UPD_SVC=/etc/systemd/system/polaris-update.service
UPD_PATH=/etc/systemd/system/polaris-update.path
UNIT=/etc/systemd/system/polaris.service
CADDYFILE=/etc/caddy/Caddyfile
RAW_SCRIPT="https://raw.githubusercontent.com/$REPO/main/scripts/install-panel.sh"

FORCE=0 DOMAIN="" CADDY=1 VERSION="" PORT=8080 AGENT_PORT=9443 ADMIN_PATH="" MIRROR="" PURGE=0 ADMIN_USER="" NODE_NAME="" AGENT_SUB="" AGENT_ARG=""

shopt -s extglob

# --- output ----------------------------------------------------------------------

if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  C_R=$'\e[31m' C_G=$'\e[32m' C_Y=$'\e[33m' C_C=$'\e[36m' C_D=$'\e[2m' C_BD=$'\e[1m' C_0=$'\e[0m'
else
  C_R="" C_G="" C_Y="" C_C="" C_D="" C_BD="" C_0=""
fi
red() { printf '%s%s%s\n' "$C_R" "$*" "$C_0"; }
green() { printf '%s%s%s\n' "$C_G" "$*" "$C_0"; }
yellow() { printf '%s%s%s\n' "$C_Y" "$*" "$C_0"; }
dim() { printf '%s%s%s\n' "$C_D" "$*" "$C_0"; }
ok() { printf '  %s✓%s %s\n' "$C_G" "$C_0" "$*"; }
warn() { printf '  %s!%s %s\n' "$C_Y" "$C_0" "$*"; }
bad() { printf '  %s✗%s %s\n' "$C_R" "$C_0" "$*"; }
die() { printf '  %s✗ 错误：%s%s\n' "$C_R" "$*" "$C_0" >&2; exit 1; }
# step prints a numbered progress line once STEP_TOTAL is set, a plain one otherwise.
STEP_N=0 STEP_TOTAL=0
step() {
  STEP_N=$((STEP_N + 1))
  if [ "$STEP_TOTAL" -gt 0 ]; then
    printf '  %s[%d/%d]%s %s%s%s\n' "$C_C" "$STEP_N" "$STEP_TOTAL" "$C_0" "$C_BD" "$*" "$C_0"
  else
    printf '  %s==>%s %s%s%s\n' "$C_C" "$C_0" "$C_BD" "$*" "$C_0"
  fi
}

# dwidth TEXT sets REPLY to the display width of TEXT (CJK counts 2, colour
# codes 0), whatever the locale: width = chars + (bytes - chars) / 2 in UTF-8.
dwidth() {
  local LC_ALL=C s="$1"
  s=${s//$'\e['*([0-9;])m/}
  local b=${#s}
  s=${s//[$'\x80'-$'\xbf']/}
  REPLY=$((${#s} + (b - ${#s}) / 2))
}
# pad TEXT WIDTH prints TEXT followed by spaces up to WIDTH columns.
pad() { dwidth "$1"; local n=$(($2 - REPLY)); [ $n -gt 0 ] || n=0; printf '%s%*s' "$1" "$n" ''; }
rep() { local s; printf -v s '%*s' "$2" ''; printf '%s' "${s// /$1}"; }
# UI_W: width of dividers and frames, fitted to the terminal so nothing wraps
# on a phone SSH client (at most 60 columns, at least 32).
term_cols() {
  local c=""
  c="$(stty size 2>/dev/null </dev/tty | awk '{print $2}')" || true
  [ -n "$c" ] || c="$(tput cols 2>/dev/null)" || true
  [[ "$c" =~ ^[0-9]+$ ]] || c=80
  echo "$c"
}
UI_W=$(($(term_cols) - 4))
[ "$UI_W" -gt 60 ] && UI_W=60
[ "$UI_W" -lt 32 ] && UI_W=32
# section TITLE: a "── title ─────" divider.
section() { dwidth "$1"; printf '\n  %s── %s%s%s %s%s\n' "$C_D" "$C_0$C_BD" "$1" "$C_0$C_D" "$(rep ─ $((UI_W - 6 - REPLY)))" "$C_0"; }
# kv LABEL VALUE: an aligned "label   value" line.
kv() { printf '  %s%s%s  %s\n' "$C_D" "$(pad "$1" 8)" "$C_0" "$2"; }
# box COLOUR TITLE LINE...: a left-bordered panel (no right border, so long
# URLs never break the frame).
box() {
  local col="$1" title="$2" line; shift 2
  dwidth "$title"
  printf '  %s╭─ %s%s%s %s%s\n' "$col" "$C_BD" "$title" "$C_0$col" "$(rep ─ $((UI_W - 6 - REPLY)))" "$C_0"
  for line; do printf '  %s│%s  %s\n' "$col" "$C_0" "$line"; done
  printf '  %s╰%s%s\n' "$col" "$(rep ─ $((UI_W - 3)))" "$C_0"
}
dot() { case "${1:-}" in on) printf '%s●%s' "$C_G" "$C_0" ;; off) printf '%s●%s' "$C_R" "$C_0" ;; *) printf '%s○%s' "$C_D" "$C_0" ;; esac; }

# human_dur SECONDS / human_bytes N: short Chinese durations and sizes.
human_dur() {
  local s=$1
  if [ "$s" -ge 86400 ]; then printf '%d天%d小时' $((s / 86400)) $((s % 86400 / 3600))
  elif [ "$s" -ge 3600 ]; then printf '%d小时%d分' $((s / 3600)) $((s % 3600 / 60))
  elif [ "$s" -ge 60 ]; then printf '%d分钟' $((s / 60))
  else printf '%d秒' "$s"; fi
}
human_bytes() { awk -v b="$1" 'BEGIN { split("B K M G T", u); i = 1; while (b >= 1024 && i < 5) { b /= 1024; i++ } printf (i == 1 ? "%d%s" : "%.1f%s"), b, u[i] }'; }

preflight() {
  [ "$(id -u)" = 0 ] || die "请以 root 运行"
  command -v systemctl >/dev/null || die "需要 systemd"
  for c in curl tar sha256sum awk sed; do command -v "$c" >/dev/null || die "缺少命令 $c"; done
  case "$(uname -m)" in
    x86_64|amd64) ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) die "不支持的架构 $(uname -m)" ;;
  esac
}

installed() { [ -x "$BIN" ] && [ -f "$ENV_FILE" ]; }
need_installed() { installed || die "Polaris 尚未安装"; }
env_get() { sed -n "s/^$1=//p" "$ENV_FILE" 2>/dev/null | tail -n1; }
env_set() { # key value
  if grep -q "^$1=" "$ENV_FILE"; then
    sed -i "s|^$1=.*|$1=$2|" "$ENV_FILE"
  else
    echo "$1=$2" >> "$ENV_FILE"
  fi
}
running() { systemctl is-active --quiet polaris; }

# The installer hands the generated admin password to the first start via the
# env file; once the panel runs the account exists, so drop it from disk.
scrub_initial_password() {
  [ -f "$ENV_FILE" ] && grep -q '^POLARIS_INITIAL_ADMIN_PASSWORD=' "$ENV_FILE" &&
    sed -i '/^POLARIS_INITIAL_ADMIN_PASSWORD=/d' "$ENV_FILE"
  return 0
}

print_credentials() { # password
  echo
  box "$C_G" "后台登录信息（请立即保存）" \
    "$(pad 地址 8)${C_BD}$(env_get POLARIS_PUBLIC_URL)$(env_get POLARIS_ADMIN_PATH)/${C_0}" \
    "$(pad 用户名 8)${C_BD}admin${C_0}" \
    "$(pad 密码 8)${C_BD}$1${C_0}" \
    "${C_D}密码只显示这一次（双击可整段选中复制，区分大小写）；忘记后执行 polaris reset-password 重置${C_0}"
}

# banner TITLE: the script header with the installed version on the right.
banner() {
  local left="Polaris  ${1:-面板管理}" right; right="$(panel_version)"
  dwidth "$left"; local n=$((UI_W - 2 - REPLY - ${#right})); [ $n -gt 1 ] || n=1
  echo
  printf '  %s%s%s%*s%s%s%s\n' "$C_C$C_BD" "$left" "$C_0" "$n" '' "$C_D" "$right" "$C_0"
  printf '  %s%s%s\n' "$C_C" "$(rep ═ $((UI_W - 2)))" "$C_0"
}

# --- status ----------------------------------------------------------------------

panel_version() { [ -x "$BIN" ] && "$BIN" --version 2>/dev/null | awk '{print $2}'; return 0; }
# svc_uptime UNIT: seconds since the unit became active (empty if not running).
svc_uptime() {
  local mono now
  mono="$(systemctl show "$1" -p ActiveEnterTimestampMonotonic --value 2>/dev/null || true)"
  [[ "$mono" =~ ^[0-9]+$ ]] && [ "$mono" -gt 0 ] || return 0
  now="$(cut -d. -f1 /proc/uptime)"
  echo $((now - mono / 1000000))
}
svc_mem() { # human-readable memory of a running unit, if systemd accounts it
  local m; m="$(systemctl show "$1" -p MemoryCurrent --value 2>/dev/null || true)"
  [[ "$m" =~ ^[0-9]+$ ]] && human_bytes "$m"
  return 0
}
# svc_line UNIT: "● 运行中 · 已运行 3天2小时 · 内存 41.2M" style summary.
svc_line() {
  local up mem
  if systemctl is-active --quiet "$1"; then
    up="$(svc_uptime "$1")"; mem="$(svc_mem "$1")"
    printf '%s %s运行中%s%s%s' "$(dot on)" "$C_G" "$C_0" "${up:+ · 已运行 $(human_dur "$up")}" "${mem:+ · 内存 $mem}"
  elif systemctl cat "$1" >/dev/null 2>&1; then
    printf '%s %s已停止%s' "$(dot off)" "$C_R" "$C_0"
  else
    printf '%s %s未安装%s' "$(dot)" "$C_D" "$C_0"
  fi
}
sys_line() {
  local os mem load
  os="$(. /etc/os-release 2>/dev/null && echo "${NAME%% *} ${VERSION_ID:-}")" # "Debian 13": short enough for phone terminals
  mem="$(awk '/^MemTotal/ {t=$2} /^MemAvailable/ {a=$2} END {printf "%.1f/%.1fG", (t-a)/1048576, t/1048576}' /proc/meminfo 2>/dev/null)"
  load="$(cut -d' ' -f1 /proc/loadavg 2>/dev/null)"
  printf '%s · %s · %s核 · 内存 %s · 负载 %s' "${os:-Linux}" "$(uname -m)" "$(nproc 2>/dev/null || echo '?')" "$mem" "$load"
}
disk_line() { df -h "$1" 2>/dev/null | awk 'NR == 2 {printf "已用 %s / %s（%s），剩余 %s", $3, $2, $5, $4}'; }

# latest_version [TIMEOUT]: newest release tag (pre-releases included), empty
# when offline. If api.github.com is blocked or rate-limited, fall back to the
# github.com/…/releases/latest redirect (stable releases), direct or via mirrors.
latest_version() {
  local t="${1:-8}" v m
  # Only "v…" tags are panel releases ("sing-box-v…" hold core builds).
  v="$(curl -fsSL --max-time "$t" "https://api.github.com/repos/$REPO/releases?per_page=20" 2>/dev/null |
    sed -n 's/.*"tag_name": *"\(v[0-9][^"]*\)".*/\1/p' | head -n1 || true)"
  if [ -z "$v" ]; then
    for m in "${MIRROR:-}" "" "${MIRRORS[@]}"; do
      v="$(curl -fsSIL -o /dev/null -w '%{url_effective}' --max-time "$t" "${m:+${m%/}/}https://github.com/$REPO/releases/latest" 2>/dev/null |
        sed -n 's|.*/releases/tag/\([^/?#[:space:]]*\).*|\1|p' || true)"
      [ -n "$v" ] && break
    done
  fi
  echo "$v"
}

# open_port PORT: allow PORT/tcp in an active ufw or firewalld. Cloud security
# groups are outside the server and still need opening by hand.
open_port() {
  if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q '^Status: active'; then
    ufw allow "$1/tcp" >/dev/null 2>&1 && ok "ufw 已放行 $1/tcp"
  elif command -v firewall-cmd >/dev/null && firewall-cmd --state >/dev/null 2>&1; then
    firewall-cmd -q --permanent --add-port="$1/tcp" && firewall-cmd -q --reload && ok "firewalld 已放行 $1/tcp"
  fi
  return 0
}
# fw_state PORT: describes whether the local firewall lets PORT/tcp in.
fw_state() {
  if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q '^Status: active'; then
    if ufw status 2>/dev/null | grep -Eq "^$1(/tcp)?[[:space:]]+ALLOW"; then echo open; else echo closed; fi
  elif command -v firewall-cmd >/dev/null && firewall-cmd --state >/dev/null 2>&1; then
    if firewall-cmd --list-ports 2>/dev/null | grep -qw "$1/tcp"; then echo open; else echo closed; fi
  else
    echo none
  fi
}

# polaris_cmd runs a panel maintenance subcommand as the service user with
# the service's environment (so SQLite files keep the right owner).
polaris_cmd() {
  # Releases before v0.1.0-beta.2 lack these subcommands and would ignore the
  # arguments and start a second panel instead, so never run them blindly.
  # (Detected from the usage text compiled into the binary.)
  if ! grep -qaF 'polaris admin reset-password' "$BIN"; then
    red "当前安装的版本（$("$BIN" --version 2>/dev/null | awk '{print $2}')）不支持此功能，请先执行 polaris update 升级" >&2
    return 1
  fi
  # shellcheck source=/dev/null
  (cd "$DATA" && set -a && . "$ENV_FILE" && set +a && runuser -u polaris -- "$BIN" "$@")
}

wait_active() {
  local _
  for _ in $(seq 1 15); do running && { scrub_initial_password; return 0; }; sleep 1; done
  journalctl -u polaris -n 30 --no-pager
  return 1
}

# --- download ------------------------------------------------------------------

# Public GitHub download mirrors, tried in order when github.com is blocked or
# too slow (common on servers in mainland China). --mirror puts one first.
MIRRORS=(https://ghfast.top/ https://gh-proxy.com/ https://ghproxy.net/)
MIRROR_FILE=$ETC/mirror # the mirror that last worked, reused by later updates

is_github() { case "$1" in https://github.com/*|https://raw.githubusercontent.com/*) return 0 ;; esac; return 1; }
gh() { if [ -n "$MIRROR" ] && is_github "$1"; then printf '%s%s' "${MIRROR%/}/" "$1"; else printf '%s' "$1"; fi; }
# dl URL OUT [progress]: one download attempt; a transfer that stays under
# 10 KB/s for 30 s is aborted instead of hanging, so the next source is tried.
dl() {
  local show=(-sS)
  [ "${3:-}" = progress ] && [ -t 2 ] && show=(--progress-bar)
  curl -fL "${show[@]}" --retry 1 --connect-timeout 10 --speed-limit 10240 --speed-time 30 -o "$2" "$1"
}
# fetch URL OUT [progress]: direct (or via --mirror) first; GitHub URLs then
# fall back to the public mirrors, and a mirror that works is kept for the rest.
fetch() {
  local url="$1" out="$2" m
  dl "$(gh "$url")" "$out" "${3:-}" && return 0
  is_github "$url" || return 1
  for m in "" "${MIRRORS[@]}"; do
    [ "${m%/}" = "${MIRROR%/}" ] && continue
    if [ -n "$m" ]; then warn "下载失败，改用镜像 $m 重试"; else warn "下载失败，改用 GitHub 直连重试"; fi
    if dl "${m:+${m%/}/}$url" "$out" "${3:-}"; then
      MIRROR="$m"
      [ -z "$m" ] && rm -f "$MIRROR_FILE" 2>/dev/null
      [ -n "$m" ] && [ -d "$ETC" ] && { echo "$m" > "$MIRROR_FILE"; dim "    之后的升级会优先使用该镜像（polaris update --mirror none 恢复直连）"; }
      return 0
    fi
  done
  return 1
}

resolve_version() {
  if [ -z "$VERSION" ]; then
    step "查询最新版本"
    VERSION="$(latest_version 15)"
    [ -n "$VERSION" ] || die "无法获取最新版本号（GitHub 不可达）。可指定版本与镜像重试：polaris update --version v0.1.6 --mirror https://ghfast.top/"
    ok "最新版本 $VERSION"
  fi
  RELEASE="${POLARIS_RELEASE_URL:-https://github.com/$REPO/releases/download/$VERSION}"
}

# update_self installs this tool from release $VERSION (checksum-verified)
# when it differs from the installed copy. Used when the panel itself is
# already current, so script-only fixes still arrive.
update_self() {
  resolve_version
  local d want got
  d="$(mktemp -d)"
  if ! fetch "$RELEASE/checksums.txt" "$d/checksums.txt" || ! grep -q ' \*\?install-panel.sh$' "$d/checksums.txt" ||
    ! fetch "$RELEASE/install-panel.sh" "$d/install-panel.sh"; then
    rm -rf "$d"; warn "无法下载 $VERSION 的管理脚本，脚本未更新"; return 0
  fi
  want="$(awk '$2 == "install-panel.sh" || $2 == "*install-panel.sh" {print $1}' "$d/checksums.txt")"
  got="$(sha256sum "$d/install-panel.sh" | awk '{print $1}')"
  if [ -z "$want" ] || [ "$want" != "$got" ]; then
    rm -rf "$d"; warn "管理脚本校验失败，未更新"; return 0
  fi
  if cmp -s "$d/install-panel.sh" "$CTL"; then
    ok "管理脚本已是最新（$VERSION）"
  else
    install -m 0755 "$d/install-panel.sh" "$CTL" && ln -sf "$CTL" "$CTL_ALIAS"
    installed && [ -d "$DATA/downloads" ] && install -m 0644 -o polaris -g polaris "$CTL" "$DATA/downloads/install-panel.sh"
    ok "管理脚本已更新到 $VERSION"
  fi
  rm -rf "$d"
}

# download_release fetches and verifies the panel, this machine's agent and this script.
download_release() {
  resolve_version
  TMP="$(mktemp -d)"
  trap 'rm -rf "$TMP"' EXIT
  step "下载 Polaris $VERSION ($ARCH)${MIRROR:+ · 镜像 $MIRROR}"
  PANEL_TGZ="polaris-panel-$VERSION-linux-$ARCH.tar.gz"
  fetch "$RELEASE/checksums.txt" "$TMP/checksums.txt" ||
    die "下载 checksums.txt 失败（$RELEASE）。服务器无法访问 GitHub 时可指定镜像：--mirror https://你的镜像/"
  local f want got
  # Only this machine's architecture: the panel fetches another agent build
  # itself the first time a node of that architecture needs it.
  local files=("$PANEL_TGZ" "polaris-agent-linux-$ARCH")
  # Releases before the installer existed do not ship it; only fetch what is listed.
  grep -q ' \*\?install-panel.sh$' "$TMP/checksums.txt" && files+=(install-panel.sh)
  for f in "${files[@]}"; do
    dim "    $f"
    fetch "$RELEASE/$f" "$TMP/$f" progress || die "下载 $f 失败（直连与镜像均不可用；可用 --mirror 指定其他镜像）"
    want="$(awk -v f="$f" '$2 == f || $2 == "*"f {print $1}' "$TMP/checksums.txt")"
    got="$(sha256sum "$TMP/$f" | awk '{print $1}')"
    [ -n "$want" ] && [ "$want" = "$got" ] || die "$f 校验失败（期望 ${want:-无}，实际 $got）"
  done
  ok "SHA-256 校验通过（${#files[@]} 个文件）"
  tar -xzf "$TMP/$PANEL_TGZ" -C "$TMP"
}

install_files() {
  install -m 0755 "$TMP/polaris" "$BIN"
  # Agent binaries served at /download/ so node installs pull them from this panel.
  install -d -m 0755 -o polaris -g polaris "$DATA" "$DATA/downloads"
  install -m 0755 -o polaris -g polaris "$TMP/polaris-agent-linux-$ARCH" "$DATA/downloads/"
  printf '%s\n' "$VERSION" > "$DATA/downloads/.version-$ARCH"
  chown polaris:polaris "$DATA/downloads/.version-$ARCH"
  # Builds of the other architecture from an older version would be served
  # with a stale checksum: drop them (re-fetched on demand), and the old
  # shared marker with them.
  local a
  for a in amd64 arm64; do
    [ "$a" = "$ARCH" ] && continue
    if [ "$(cat "$DATA/downloads/.version-$a" 2>/dev/null)" != "$VERSION" ]; then
      rm -f "$DATA/downloads/polaris-agent-linux-$a" "$DATA/downloads/.version-$a"
    fi
  done
  rm -f "$DATA/downloads/.version"
  install_ctl
  # Nodes install the same tool as their `polaris` command from this panel.
  [ -f "$CTL" ] && install -m 0644 -o polaris -g polaris "$CTL" "$DATA/downloads/install-panel.sh"
  return 0
}

# install_ctl registers this tool as the "polaris" command (and polaris-ctl).
install_ctl() {
  local src=""
  if [ -f "${TMP:-}/install-panel.sh" ]; then
    src="$TMP/install-panel.sh"
  elif [ -f "$0" ] && [ "$(readlink -f "$0")" != "$(readlink -f "$CTL" 2>/dev/null)" ]; then
    src="$0"
  elif ! is_script "$CTL"; then
    src="$(mktemp)"
    fetch "$RAW_SCRIPT" "$src" || { yellow "未能注册 polaris 命令"; return 0; }
  fi
  [ -n "$src" ] && install -m 0755 "$src" "$CTL"
  ln -sf "$CTL" "$CTL_ALIAS"
}
is_script() { [ -f "$1" ] && head -c 2 "$1" | grep -q '#!'; }

# migrate_layout moves pre-rename installs (panel binary at /usr/local/bin/polaris)
# to polaris-server, freeing the name for the menu.
migrate_layout() {
  if [ -f "$CTL" ] && ! is_script "$CTL"; then
    [ -x "$BIN" ] || install -m 0755 "$CTL" "$BIN"
    rm -f "$CTL"
  fi
  if [ -f "$UNIT" ] && grep -q '^ExecStart=/usr/local/bin/polaris$' "$UNIT"; then
    sed -i "s|^ExecStart=/usr/local/bin/polaris$|ExecStart=$BIN|" "$UNIT"
    systemctl daemon-reload
  fi
}

# install_unit installs the systemd unit, pointing it at polaris-server.
install_unit() {
  install -m 0644 "$TMP/polaris.service" "$UNIT"
  sed -i "s|^ExecStart=/usr/local/bin/polaris$|ExecStart=$BIN|" "$UNIT"
  install_updater
}

# install_updater: the root side of updates requested from the panel.
install_updater() {
  cat > "$UPD_SVC" <<UNIT
[Unit]
Description=Polaris update requested from the panel

[Service]
Type=oneshot
ExecStart=$CTL update-request
UNIT
  cat > "$UPD_PATH" <<UNIT
[Unit]
Description=Watch for Polaris update requests

[Path]
PathExists=$UPD_REQ
Unit=polaris-update.service

[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload
  systemctl enable --now polaris-update.path >/dev/null 2>&1 || true
}

# do_update_request: run by polaris-update.service for requests the panel
# writes (it cannot change its own install): "latest" or a version tag
# updates; "set-path P", "set-domain D", "set-port N" and "set-url U" run the
# matching command. The outcome goes to update-result for the panel.
do_update_request() {
  [ -f "$UPD_REQ" ] || return 0
  local line action arg cur rc=0 log="$DATA/update.log"
  line="$(head -n1 "$UPD_REQ" | tr -cd 'A-Za-z0-9.:/+_ -')"
  rm -f "$UPD_REQ" # first: the path unit would fire again while it exists
  action="${line%% *}" arg=""
  [ "$action" = "$line" ] || arg="${line#* }"
  case "$action" in
    set-path|set-domain|set-port|set-url) ;;
    ""|latest) action=update VERSION="" ;;
    v*) arg="$action" action=update VERSION="$arg" ;;
    *) arg="$action" action=update VERSION="v$arg" ;;
  esac
  cur="$(panel_version)"
  # set -e must stay on inside the command ("cmd || x" would switch it off).
  set +e
  case "$action" in
    update) ( set -e; do_update ) >"$log" 2>&1 ;;
    set-path) ( set -e; do_set_path "$arg" ) >"$log" 2>&1 ;;
    set-domain) ( set -e; do_set_domain "$arg" ) >"$log" 2>&1 ;;
    set-port) ( set -e; do_set_port "$arg" ) >"$log" 2>&1 ;;
    set-url) ( set -e; do_set_url "$arg" ) >"$log" 2>&1 ;;
  esac
  rc=$?
  set -e
  {
    printf 'ok=%s\naction=%s\narg=%s\nfrom=%s\nto=%s\n' "$([ "$rc" = 0 ] && echo 1 || echo 0)" "$action" "$arg" "$cur" "$(panel_version)"
    echo "log:"
    sed 's/\x1b\[[0-9;]*m//g' "$log" | tail -n 12
  } > "$UPD_RES.tmp"
  mv -f "$UPD_RES.tmp" "$UPD_RES"
  chown polaris:polaris "$UPD_RES" "$log" 2>/dev/null || true
  return "$rc"
}

# --- helpers -------------------------------------------------------------------

public_ip() {
  local u ip
  for u in https://api.ipify.org https://ifconfig.me/ip https://ipv4.icanhazip.com; do
    ip="$(curl -4 -fsS --max-time 5 "$u" 2>/dev/null | tr -d '[:space:]' || true)"
    if printf '%s' "$ip" | grep -Eq '^[0-9]+(\.[0-9]+){3}$'; then echo "$ip"; return 0; fi
  done
  return 0 # empty output = not found; never trip set -e
}
# rand N: N random lowercase hex characters (no pipes that can SIGPIPE under pipefail).
rand() { od -An -N32 -tx1 /dev/urandom | tr -d ' \n' | cut -c1-"$1"; }
valid_port() { [[ "$1" =~ ^[0-9]+$ ]] && [ "$1" -ge 1 ] && [ "$1" -le 65535 ]; }
# port_busy PORT: true if something already listens on the TCP port.
port_busy() {
  if command -v ss >/dev/null; then
    ss -Hltn "sport = :$1" 2>/dev/null | grep -q .
  else # /proc fallback: state 0A = LISTEN, port in hex; tcp6 is absent without IPv6
    local f files=()
    for f in /proc/net/tcp /proc/net/tcp6; do [ -r "$f" ] && files+=("$f"); done
    [ ${#files[@]} -gt 0 ] || return 1
    awk -v p="$(printf ':%04X' "$1")" '$4 == "0A" && substr($2, length($2) - 4) == p' "${files[@]}" | grep -q .
  fi
}
port_owner() { ss -Hltnp "sport = :$1" 2>/dev/null | sed -n 's/.*users:(("\([^"]*\)".*/\1/p' | head -n1; }
port_msg() { local o; o="$(port_owner "$1")"; echo "端口 $1 已被占用${o:+（$o）}"; }
valid_domain() { [[ "$1" =~ ^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)+$ ]]; }

install_caddy() {
  step "安装 Caddy（自动申请 HTTPS 证书）"
  if command -v caddy >/dev/null; then ok "已安装 $(caddy version 2>/dev/null | awk '{print $1}')，跳过"; return 0; fi
  if command -v apt-get >/dev/null; then
    apt-get update -qq && apt-get install -y -qq debian-keyring debian-archive-keyring apt-transport-https gnupg >/dev/null
    curl -fsSL https://dl.cloudsmith.io/public/caddy/stable/gpg.key | gpg --dearmor --yes -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
    curl -fsSL https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt > /etc/apt/sources.list.d/caddy-stable.list
    apt-get update -qq && apt-get install -y -qq caddy >/dev/null
  elif command -v dnf >/dev/null; then
    dnf install -y -q 'dnf-command(copr)' && dnf copr enable -y -q @caddy/caddy && dnf install -y -q caddy
  else
    die "无法自动安装 Caddy，请手动安装后重试"
  fi
}

# caddy_site DOMAIN PORT adds (or repoints) a reverse-proxy site.
caddy_site() {
  local domain="$1" port="$2"
  mkdir -p /etc/caddy
  # The distro's stock Caddyfile only serves a welcome page on :80; replace it.
  if [ -f "$CADDYFILE" ] && grep -q '/usr/share/caddy' "$CADDYFILE"; then cp "$CADDYFILE" "$CADDYFILE.orig"; : > "$CADDYFILE"; fi
  if [ -f "$CADDYFILE" ] && grep -q "^$domain {" "$CADDYFILE"; then
    sed -i "/^$domain {/,/^}/ s|reverse_proxy 127.0.0.1:[0-9]*|reverse_proxy 127.0.0.1:$port|" "$CADDYFILE"
  else
    printf '\n%s {\n\tencode zstd gzip\n\treverse_proxy 127.0.0.1:%s\n}\n' "$domain" "$port" >> "$CADDYFILE"
  fi
  systemctl enable --now caddy >/dev/null 2>&1 || true
  systemctl reload caddy 2>/dev/null || systemctl restart caddy
}

panel_domain() { # domain the panel is served on via HTTPS, if any
  local u; u="$(env_get POLARIS_PUBLIC_URL)"
  case "$u" in https://*) u="${u#https://}"; echo "${u%%[:/]*}" ;; esac
}

# port_line PORT [fw]: listening state (and local firewall state) of a TCP port.
port_line() {
  local s
  if port_busy "$1"; then s="$(dot on) 监听中"; else s="$(dot off) 未监听"; fi
  if [ "${2:-}" = fw ]; then
    case "$(fw_state "$1")" in
      open) s="$s · 防火墙已放行" ;;
      closed) s="$s · ${C_Y}防火墙未放行${C_0}" ;;
    esac
  fi
  printf '%s' "$s"
}

show_info() {
  need_installed
  local url path listen agent domain db size backups
  url="$(env_get POLARIS_PUBLIC_URL)"; path="$(env_get POLARIS_ADMIN_PATH)"
  listen="$(env_get POLARIS_LISTEN)"; agent="$(env_get POLARIS_AGENT_LISTEN)"
  domain="$(panel_domain)"

  section "服务"
  kv 版本 "$(panel_version)"
  kv 面板 "$(svc_line polaris)"
  if systemctl is-enabled --quiet polaris 2>/dev/null; then kv 开机自启 "是"; else kv 开机自启 "${C_Y}否${C_0}"; fi
  systemctl cat polaris-agent >/dev/null 2>&1 && kv 本机节点 "$(svc_line polaris-agent)"
  [ -n "$domain" ] && [ -f "$CADDYFILE" ] && grep -q "^$domain {" "$CADDYFILE" && kv Caddy "$(svc_line caddy)"

  section "访问"
  kv 后台地址 "${C_BD}$url$path/${C_0}"
  kv 订阅域名 "$url"
  if [ -n "$domain" ]; then
    kv 面板监听 "$listen · $(port_line "${listen##*:}")（经 HTTPS 反代）"
  else
    kv 面板监听 "$listen · $(port_line "${listen##*:}" fw)"
  fi
  kv 节点接入 "$agent · $(port_line "${agent##*:}" fw)"
  kv 接入地址 "$(env_get POLARIS_AGENT_HOSTS)"

  section "文件"
  kv 配置文件 "$ENV_FILE"
  db="$(ls "$DATA"/*.db 2>/dev/null | head -n1 || true)"
  if [ -n "$db" ]; then size="$(human_bytes "$(stat -c %s "$db")")"; kv 数据库 "$db（$size）"
  elif env_get POLARIS_DB_DSN | grep -q '^postgres'; then kv 数据库 "PostgreSQL"; fi
  backups="$(find "$DATA" /root -maxdepth 1 \( -name 'backup-*' -o -name 'polaris-backup-*' \) 2>/dev/null | wc -l || true)"
  kv 数据目录 "$DATA · 备份 ${backups} 份"
  kv 磁盘 "$(disk_line "$DATA")"

  section "系统"
  kv 主机 "$(sys_line)"
  kv 开机时长 "$(human_dur "$(cut -d. -f1 /proc/uptime)")"
}

# --- actions -------------------------------------------------------------------

do_install() {
  installed && die "已安装：升级用 update，重装请先 uninstall"
  [ -f "$ENV_FILE" ] && die "$ENV_FILE 已存在：恢复或升级请用 polaris update，全新安装请先 polaris uninstall --purge"
  # Data left by an earlier install is encrypted with that install's secret
  # key; a fresh key cannot read it, so do not start on top of it.
  if ls "$DATA"/*.db >/dev/null 2>&1; then
    die "$DATA 中有旧安装留下的数据，但配置文件（含解密密钥）已不存在。请恢复原来的 $ENV_FILE 后执行 polaris update，或执行 polaris uninstall --purge 清除旧数据后重新安装"
  fi
  [ -z "$DOMAIN" ] || valid_domain "$DOMAIN" || die "域名格式不正确：$DOMAIN"
  valid_port "$PORT" && valid_port "$AGENT_PORT" || die "端口不正确"
  [ "$PORT" != "$AGENT_PORT" ] || die "面板端口与节点接入端口不能相同"
  port_busy "$PORT" && die "$(port_msg "$PORT")：请换一个面板端口（--port）"
  port_busy "$AGENT_PORT" && die "$(port_msg "$AGENT_PORT")：请换一个节点接入端口（--agent-port）"

  local use_caddy=0
  [ -n "$DOMAIN" ] && [ "$CADDY" = 1 ] && use_caddy=1
  SECONDS=0 STEP_N=0 STEP_TOTAL=$((3 + 2 * use_caddy))
  [ -n "$VERSION" ] || STEP_TOTAL=$((STEP_TOTAL + 1))
  banner "安装面板"
  kv 系统 "$(sys_line)"
  kv 访问方式 "$(if [ -n "$DOMAIN" ]; then echo "https://$DOMAIN$([ "$use_caddy" = 1 ] && echo '（自动安装 Caddy 申请证书）' || echo '（自备反向代理）')"; else echo "IP + HTTP"; fi)"
  kv 面板端口 "$PORT"
  kv 节点接入 "$AGENT_PORT"
  kv 版本 "${VERSION:-最新}"
  echo
  download_release

  local host listen public_url secret pass=""
  # Generate the first admin's password here and print it ourselves: reading it
  # back from the journal can pick up an older install's line after a reinstall.
  pass="$(head -c 48 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | cut -c1-16)"
  if [ -n "$DOMAIN" ]; then
    host="$DOMAIN"
  else
    host="${POLARIS_PUBLIC_IP:-$(public_ip)}"
    [ -n "$host" ] || die "无法探测公网 IP：请用 --domain 指定域名，或设置 POLARIS_PUBLIC_IP"
  fi
  [ -n "$ADMIN_PATH" ] || ADMIN_PATH="/$(rand 12)"
  ADMIN_PATH="/${ADMIN_PATH#/}"
  secret="$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')"
  if [ -n "$DOMAIN" ]; then
    listen="127.0.0.1:$PORT"; public_url="https://$DOMAIN"
  else
    listen=":$PORT"; public_url="http://$host:$PORT"
  fi

  step "创建用户与目录"
  id polaris >/dev/null 2>&1 || useradd --system --home-dir "$DATA" --shell /usr/sbin/nologin polaris
  install -d -m 0750 -o root -g polaris "$ETC"
  install_files
  cat > "$ENV_FILE" <<EOF
# Generated by the polaris installer on $(date -u +%Y-%m-%dT%H:%M:%SZ)
# 务必备份 POLARIS_SECRET_KEY：丢失后已加密的数据无法解密
POLARIS_SECRET_KEY=$secret
POLARIS_ADMIN_PATH=$ADMIN_PATH
POLARIS_PUBLIC_URL=$public_url
POLARIS_LISTEN=$listen
POLARIS_AGENT_LISTEN=:$AGENT_PORT
POLARIS_AGENT_HOSTS=$host
POLARIS_DATA_DIR=$DATA
POLARIS_INITIAL_ADMIN_USERNAME=admin
POLARIS_INITIAL_ADMIN_PASSWORD=$pass
EOF
  chmod 0640 "$ENV_FILE"; chown root:polaris "$ENV_FILE"
  install_unit

  if [ "$use_caddy" = 1 ]; then
    install_caddy
    step "配置 Caddy 站点 $DOMAIN"
    caddy_site "$DOMAIN" "$PORT"
  fi

  step "启动 Polaris"
  systemctl daemon-reload
  systemctl enable --now polaris >/dev/null 2>&1
  if ! wait_active; then
    # The admin account is created before the listeners start, so the
    # credentials are valid even when startup fails afterwards (e.g. port busy).
    print_credentials "$pass" # after the log wait_active printed, so it stays in view
    if journalctl -u polaris -n 40 --no-pager 2>/dev/null | grep -q 'address already in use'; then
      die "Polaris 启动失败：端口被占用。可执行 polaris set-port <新端口> 修改后会自动重启"
    fi
    die "Polaris 启动失败（修正后执行 polaris restart）"
  fi

  open_port "$AGENT_PORT"
  if [ "$use_caddy" = 1 ]; then open_port 80; open_port 443; elif [ -z "$DOMAIN" ]; then open_port "$PORT"; fi

  echo
  ok "${C_BD}Polaris $VERSION 安装完成${C_0}（用时 $(human_dur "$SECONDS")）"
  print_credentials "$pass"
  echo
  local fw=()
  if [ "$use_caddy" = 1 ]; then fw+=("$(pad "80, 443/tcp" 14)HTTPS 访问与证书申请")
  elif [ -z "$DOMAIN" ]; then fw+=("$(pad "$PORT/tcp" 14)后台与订阅"); fi
  fw+=("$(pad "$AGENT_PORT/tcp" 14)节点 Agent 接入（mTLS，不能经过反代）")
  fw+=("${C_D}云服务器还需在控制台的安全组 / 防火墙中放行${C_0}")
  box "$C_Y" "需要放行的端口" "${fw[@]}"
  [ -z "$DOMAIN" ] && { echo; warn "当前为 HTTP 明文访问，正式使用建议绑定域名：polaris set-domain panel.example.com"; }
  [ -n "$DOMAIN" ] && [ "$CADDY" = 0 ] && { echo; warn "未安装 Caddy：请让你的反向代理把 https://$DOMAIN 转发到 http://127.0.0.1:$PORT"; }
  section "下一步"
  echo "  1. 登录后台，修改密码并开启二次验证"
  echo "  2. 「节点」中添加节点；或执行 ${C_C}polaris node${C_0} 把本机作为节点"
  echo "  3. 随时输入 ${C_C}polaris${C_0} 打开管理菜单（polaris help 查看全部命令）"
  echo
}

do_update() {
  migrate_layout
  # Config is what matters: after "uninstall" without --purge the binary and
  # unit are gone but config and data remain, and update restores them.
  [ -f "$ENV_FILE" ] || die "Polaris 尚未安装（未找到 $ENV_FILE）"
  local cur backup=""
  cur="$(panel_version)"
  SECONDS=0 STEP_N=0 STEP_TOTAL=3
  [ -n "$VERSION" ] || STEP_TOTAL=4
  banner "升级面板"
  resolve_version
  if [ -n "$cur" ] && [ "$cur" = "$VERSION" ] && [ "$FORCE" = 0 ] && [ -f "$UNIT" ]; then
    ok "已是最新版本 $cur（加 --force 可重新安装）"
    update_self
    return 0
  fi
  kv 当前版本 "${cur:-未安装}"
  kv 目标版本 "${C_BD}$VERSION${C_0}"
  kv 更新说明 "https://github.com/$REPO/releases/tag/$VERSION"
  echo
  id polaris >/dev/null 2>&1 || useradd --system --home-dir "$DATA" --shell /usr/sbin/nologin polaris
  download_release
  step "备份数据库并替换程序（配置与数据保持不变）"
  if ls "$DATA"/*.db >/dev/null 2>&1; then
    backup="$DATA/backup-$(date +%Y%m%d%H%M%S)"
  fi
  systemctl stop polaris 2>/dev/null || true
  if [ -n "$backup" ]; then
    mkdir -p "$backup" && cp -a "$DATA"/*.db* "$backup"/ && ok "数据库已备份到 $backup"
  fi
  install_files
  install_unit
  ok "程序已替换"
  step "启动 Polaris"
  systemctl daemon-reload
  systemctl enable polaris >/dev/null 2>&1 || true
  systemctl start polaris
  wait_active || die "启动失败${backup:+，升级前的数据库备份在 $backup}"
  echo
  ok "${C_BD}已升级：${cur:-未安装} → $(panel_version)${C_0}（用时 $(human_dur "$SECONDS")）"
  if systemctl cat polaris-agent >/dev/null 2>&1; then
    dim "  本机节点 agent 可执行 polaris node 升级到相同版本"
  fi
}

do_check_update() {
  local cur latest
  cur="$(panel_version)"
  latest="$(latest_version 10)"
  [ -n "$latest" ] || die "无法获取最新版本（GitHub API 不可达）"
  kv 当前版本 "${cur:-未安装}"
  kv 最新版本 "$latest"
  if [ "$cur" = "$latest" ]; then ok "已是最新版本"
  else
    warn "有新版本：$latest，执行 polaris update 升级"
    dim "  更新说明：https://github.com/$REPO/releases/tag/$latest"
  fi
}

do_uninstall() {
  step "停止并移除 polaris 服务"
  systemctl disable --now polaris 2>/dev/null || true
  systemctl disable --now polaris-update.path 2>/dev/null || true
  rm -f "$UNIT" "$BIN" "$UPD_SVC" "$UPD_PATH"
  systemctl daemon-reload
  if [ "$PURGE" = 1 ]; then
    warn "删除配置与数据：$ETC $DATA"
    rm -rf "$ETC" "$DATA"
    id polaris >/dev/null 2>&1 && userdel polaris 2>/dev/null || true
    rm -f "$CTL" "$CTL_ALIAS"
  else
    dim "  已保留配置 $ETC 与数据 $DATA（加 --purge 一并删除）"
  fi
  ok "已卸载"
}

do_reset_password() {
  need_installed
  local args=(admin reset-password)
  [ -n "$ADMIN_USER" ] && args+=(-u "$ADMIN_USER")
  local out user pass
  out="$(polaris_cmd "${args[@]}")" || die "重置失败"
  user="$(printf '%s\n' "$out" | sed -n 's/^username: //p')"
  pass="$(printf '%s\n' "$out" | sed -n 's/^password: //p')"
  [ -n "$pass" ] || { printf '%s\n' "$out"; die "无法读取新密码"; }
  echo
  box "$C_G" "新的登录信息（请立即保存）" \
    "$(pad 地址 8)${C_BD}$(env_get POLARIS_PUBLIC_URL)$(env_get POLARIS_ADMIN_PATH)/${C_0}" \
    "$(pad 用户名 8)${C_BD}${user}${C_0}" \
    "$(pad 密码 8)${C_BD}${pass}${C_0}" \
    "${C_D}双击密码即可整段选中复制；只含字母和数字，区分大小写${C_0}"
  warn "已解除登录锁定，所有已登录会话已失效；浏览器若自动填入了旧密码，请手动改成上面的新密码"
}

do_reset_2fa() {
  need_installed
  local args=(admin reset-2fa)
  [ -n "$ADMIN_USER" ] && args+=(-u "$ADMIN_USER")
  polaris_cmd "${args[@]}" || die "操作失败"
}

# do_node installs an agent on the panel server itself: the panel issues a
# token locally (polaris-server node-token) and the agent connects over
# loopback, so no firewall rule is needed.
do_node() {
  need_installed
  running || die "面板未运行，请先启动"
  local port; port="$(env_get POLARIS_LISTEN)"; port="${port##*:}"
  local script=/tmp/polaris-agent-install.$$.sh
  if systemctl cat polaris-agent >/dev/null 2>&1; then
    warn "本机已安装节点 agent，将升级到与面板相同的版本"
    curl -fsSL "http://127.0.0.1:$port/install.sh" -o "$script" || die "无法从面板获取安装脚本"
    local rc=0
    bash "$script" --upgrade ${MIRROR:+--mirror "$MIRROR"} || rc=$?
    rm -f "$script"; return $rc
  fi
  if ! grep -qaF 'polaris node-token' "$BIN"; then
    die "当前面板版本不支持本机节点一键安装，请先执行 polaris update 升级"
  fi
  local tok; tok="$(polaris_cmd node-token ${NODE_NAME:+-n "$NODE_NAME"})" || die "生成安装令牌失败"
  curl -fsSL "http://127.0.0.1:$port/install.sh" -o "$script" || die "无法从面板获取安装脚本"
  local rc=0
  bash "$script" --token "$tok" --local ${MIRROR:+--mirror "$MIRROR"} || rc=$?
  rm -f "$script"
  [ $rc = 0 ] || die "本机节点安装失败"
  ok "本机节点「${NODE_NAME:-本机}」已接入，可在后台「节点」中管理"
}

do_set_path() {
  need_installed
  local p="/${1#/}"
  # "/" (or "none") turns the secret path off: the admin UI answers at the root.
  case "$p" in /none|/off) p=/ ;; esac
  if [ "$p" != / ]; then
    [[ "$p" =~ ^/[A-Za-z0-9_-]{4,64}$ ]] || die "路径只能包含字母、数字、- 和 _，长度 4–64；关闭路径用 /"
  fi
  env_set POLARIS_ADMIN_PATH "$p"
  systemctl restart polaris && wait_active || die "重启失败"
  ok "后台地址：${C_BD}$(env_get POLARIS_PUBLIC_URL)${p%/}/${C_0}"
  if [ "$p" = / ]; then
    warn "已关闭后台路径：任何人打开这个地址都能看到登录页，请使用强密码并开启二次验证"
  fi
}

# do_set_url: the public address used in subscription and install links
# (behind your own reverse proxy, or to switch back to http://IP:port).
do_set_url() {
  need_installed
  local url="${1%/}"
  [[ "$url" =~ ^https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?$ ]] || die "地址格式应为 http(s)://主机[:端口]"
  env_set POLARIS_PUBLIC_URL "$url"
  systemctl restart polaris && wait_active || die "重启失败"
  ok "公开地址：${C_BD}$url${C_0}"
  warn "订阅与安装链接随之改变；用户需要重新导入订阅（或在订阅设置里单独指定订阅域名）"
}

do_set_port() {
  need_installed
  local port="$1" listen url domain
  valid_port "$port" || die "端口不正确"
  listen="$(env_get POLARIS_LISTEN)"
  if [ "${listen##*:}" != "$port" ] && port_busy "$port"; then die "$(port_msg "$port")"; fi
  env_set POLARIS_LISTEN "${listen%:*}:$port"
  url="$(env_get POLARIS_PUBLIC_URL)"
  domain="$(panel_domain)"
  if [ -n "$domain" ]; then
    if [ -f "$CADDYFILE" ] && grep -q "^$domain {" "$CADDYFILE"; then caddy_site "$domain" "$port"; else
      warn "HTTPS 由你自己的反向代理提供：记得把上游改为 127.0.0.1:$port"
    fi
  else
    case "$url" in http://*:*) env_set POLARIS_PUBLIC_URL "${url%:*}:$port" ;; esac
    open_port "$port"
    warn "订阅链接中的端口已变化，用户需要重新导入订阅；云服务器还需在安全组放行 $port/tcp"
  fi
  systemctl restart polaris && wait_active || die "重启失败"
  ok "面板端口已改为 $port"
}

do_set_domain() {
  need_installed
  local domain="$1" port hosts
  valid_domain "$domain" || die "域名格式不正确：$domain"
  port="$(env_get POLARIS_LISTEN)"; port="${port##*:}"
  install_caddy
  step "配置 Caddy 站点 $domain"
  caddy_site "$domain" "$port"
  env_set POLARIS_PUBLIC_URL "https://$domain"
  env_set POLARIS_LISTEN "127.0.0.1:$port"
  hosts="$(env_get POLARIS_AGENT_HOSTS)"
  # Keep the old host so already-enrolled nodes still match the server certificate.
  case ",$hosts," in *",$domain,"*) ;; *) env_set POLARIS_AGENT_HOSTS "$domain${hosts:+,$hosts}" ;; esac
  systemctl restart polaris && wait_active || die "重启失败"
  open_port 80; open_port 443
  ok "已切换到 ${C_BD}https://$domain$(env_get POLARIS_ADMIN_PATH)/${C_0}"
  warn "订阅地址已变为 https://$domain/…，旧的 http 订阅链接失效，用户需要重新导入；可在防火墙关闭 $port/tcp，放行 80、443"
}

do_backup() {
  need_installed
  local ts out tmpdb
  ts="$(date +%Y%m%d-%H%M%S)"
  out="/root/polaris-backup-$ts.tar.gz"
  tmpdb="$DATA/backup-$ts.db"
  if [ -n "$(env_get POLARIS_DB_DSN)" ] && env_get POLARIS_DB_DSN | grep -q '^postgres'; then
    warn "使用 PostgreSQL：请另行用 pg_dump 备份数据库，这里只备份配置"
    tar -czf "$out" -C / "${ETC#/}"
  else
    polaris_cmd backup "$tmpdb" >/dev/null || die "数据库备份失败"
    tar -czf "$out" -C / "${ETC#/}" -C "$DATA" "$(basename "$tmpdb")"
    rm -f "$tmpdb"
  fi
  chmod 600 "$out"
  ok "已备份到 $out（$(human_bytes "$(stat -c %s "$out")")）"
  warn "备份包含加密密钥，请妥善保管；恢复时把 $ENV_FILE 与数据库放回原处后执行 polaris update"
}

# do_doctor checks the things that usually break a panel: the service, its
# ports, the local firewall, the domain's DNS, the clock, disk and recent errors.
do_doctor() {
  need_installed
  local n_bad=0 n_warn=0 listen port agent path domain code ip resolved free sync errs
  chk_ok() { ok "$*"; }
  chk_warn() { warn "$*"; n_warn=$((n_warn + 1)); }
  chk_bad() { bad "$*"; n_bad=$((n_bad + 1)); }
  listen="$(env_get POLARIS_LISTEN)"; port="${listen##*:}"
  agent="$(env_get POLARIS_AGENT_LISTEN)"; agent="${agent##*:}"
  path="$(env_get POLARIS_ADMIN_PATH)"; domain="$(panel_domain)"
  banner "系统诊断"

  section "服务"
  if running; then chk_ok "polaris 运行中（$(panel_version)）"; else chk_bad "polaris 未运行：polaris restart，或 polaris log 查看原因"; fi
  systemctl is-enabled --quiet polaris 2>/dev/null && chk_ok "已设置开机自启" || chk_warn "未设置开机自启"
  errs="$(journalctl -u polaris --since '-1h' -p err --no-pager -q 2>/dev/null | wc -l || true)"
  [ "$errs" = 0 ] && chk_ok "最近 1 小时无错误日志" || chk_warn "最近 1 小时有 $errs 条错误日志（polaris log 查看）"
  if systemctl cat polaris-agent >/dev/null 2>&1; then
    systemctl is-active --quiet polaris-agent && chk_ok "本机节点 agent 运行中" || chk_bad "本机节点 agent 未运行：journalctl -u polaris-agent -n 50"
  fi

  section "端口与防火墙"
  if port_busy "$port"; then
    code="$(curl -s -o /dev/null -m 5 -w '%{http_code}' "http://127.0.0.1:$port$path/" 2>/dev/null || true)"; code="${code#000}"
    case "$code" in 2??|3??) chk_ok "面板端口 $port 响应正常（HTTP $code）" ;; *) chk_bad "面板端口 $port 无正常响应（HTTP ${code:-无}）" ;; esac
  else chk_bad "面板端口 $port 未监听"; fi
  port_busy "$agent" && chk_ok "节点接入端口 $agent 监听中" || chk_bad "节点接入端口 $agent 未监听"
  local p need=("$agent")
  if [ -n "$domain" ]; then need+=(80 443); else need+=("$port"); fi
  for p in "${need[@]}"; do
    case "$(fw_state "$p")" in
      open) chk_ok "本机防火墙已放行 $p/tcp" ;;
      closed) chk_bad "本机防火墙未放行 $p/tcp（ufw allow $p/tcp 或 firewall-cmd --add-port=$p/tcp --permanent）" ;;
    esac
  done
  [ "$(fw_state "$agent")" = none ] && chk_ok "未启用 ufw / firewalld"
  dim "  云服务器的安全组在服务器之外，无法在此检测"

  if [ -n "$domain" ]; then
    section "域名与 HTTPS"
    ip="$(public_ip)"
    resolved="$(getent ahostsv4 "$domain" 2>/dev/null | awk '{print $1; exit}' || true)"
    if [ -z "$resolved" ]; then chk_bad "$domain 无法解析"
    elif [ -n "$ip" ] && [ "$resolved" != "$ip" ]; then chk_warn "$domain 解析到 $resolved，本机公网 IP 为 $ip（使用 CDN 时可忽略）"
    else chk_ok "$domain 解析到 ${resolved}"; fi
    if [ -f "$CADDYFILE" ] && grep -q "^$domain {" "$CADDYFILE"; then
      systemctl is-active --quiet caddy && chk_ok "Caddy 运行中" || chk_bad "Caddy 未运行：systemctl status caddy"
    fi
    code="$(curl -s -o /dev/null -m 8 -w '%{http_code}' "https://$domain$path/" 2>/dev/null || true)"; code="${code#000}"
    case "$code" in 2??|3??) chk_ok "https://$domain 可访问（HTTP $code）" ;; *) chk_bad "https://$domain 访问失败（HTTP ${code:-无}），证书可能尚未签发或 80/443 未放行" ;; esac
  fi

  section "系统"
  free="$(df -Pk "$DATA" 2>/dev/null | awk 'NR == 2 {print $4}' || true)"
  if [ -n "$free" ] && [ "$free" -lt 524288 ]; then chk_bad "磁盘剩余不足 512M（$(disk_line "$DATA")）"
  else chk_ok "磁盘 $(disk_line "$DATA")"; fi
  sync="$(timedatectl show -p NTPSynchronized --value 2>/dev/null || true)"
  case "$sync" in
    yes) chk_ok "系统时间已同步" ;;
    no) chk_warn "系统时间未同步：时间偏差会导致节点 TLS 连接和二次验证失败（timedatectl set-ntp true）" ;;
  esac
  [ -f "$ENV_FILE" ] && [ "$(stat -c %a "$ENV_FILE")" = 640 ] && chk_ok "配置文件权限正确（640）" || chk_warn "配置文件权限不是 640：chmod 640 $ENV_FILE"

  echo
  if [ $n_bad = 0 ] && [ $n_warn = 0 ]; then ok "${C_BD}全部检查通过${C_0}"
  else printf '  %s%d 项异常%s · %s%d 项提醒%s\n' "$C_R" "$n_bad" "$C_0" "$C_Y" "$n_warn" "$C_0"; fi
}

do_log() {
  trap 'echo' INT
  journalctl -u polaris -n 50 -f --no-pager || true
  trap - INT
}

# --- node agent --------------------------------------------------------------
# The same tool manages a node: the agent is installed by the panel's
# install.sh (one-click link), which also drops this script in as `polaris`.

AGENT_BIN=/usr/local/bin/polaris-agent
AGENT_UNIT=/etc/systemd/system/polaris-agent.service
agent_installed() { [ -x "$AGENT_BIN" ] && [ -f "$AGENT_UNIT" ]; }
or_dash() { if [ -n "$1" ]; then echo "$1"; else echo "未知"; fi; }
need_agent() { agent_installed || die "本机未安装节点 Agent（在面板「节点」中生成一键安装链接后执行）"; }
agent_data() {
  local d; d="$(sed -n 's/^ExecStart=.* --data \([^ ]*\).*/\1/p' "$AGENT_UNIT" 2>/dev/null | head -n1)"
  echo "${d:-/var/lib/polaris-agent}"
}
agent_version() { [ -x "$AGENT_BIN" ] && "$AGENT_BIN" --version 2>/dev/null | awk '{print $2}'; return 0; }
agent_panel() { cat "$(agent_data)/agent/panel-url" 2>/dev/null || true; }
# agent_panel_http: the panel's web address, for fetching install.sh. Saved by
# install.sh; on the panel server itself the local listener works too.
agent_panel_http() {
  local f; f="$(agent_data)/agent/panel-http"
  if [ -s "$f" ]; then head -n1 "$f"; return; fi
  if installed; then local p; p="$(env_get POLARIS_LISTEN)"; echo "http://127.0.0.1:${p##*:}"; fi
}

show_agent_info() {
  need_agent
  section "节点 Agent"
  kv 服务 "$(svc_line polaris-agent)"
  kv 版本 "$(agent_version)"
  kv 主控 "$(or_dash "$(agent_panel)")"
  kv 面板地址 "$(or_dash "$(agent_panel_http)")"
  kv 数据目录 "$(agent_data)"
  section "最近日志"
  journalctl -u polaris-agent -n 8 --no-pager -o cat 2>/dev/null | sed 's/^/  /' || true
}

# run_panel_script URL ARGS...: download a panel-served script and run it.
run_panel_script() {
  local url="$1" f; shift
  f="$(mktemp)"
  local code msg
  code="$(curl -sSL --connect-timeout 10 -o "$f" -w '%{http_code}' "$url")" || { rm -f "$f"; die "无法连接 $url"; }
  if [ "$code" != 200 ]; then
    # Rejected links come back as a tiny script whose only line explains why.
    msg="$(sed -n "s/^echo '✗ \(.*\)' >&2$/\1/p" "$f" | head -n1)"
    rm -f "$f"; die "${msg:-下载 $url 失败（HTTP $code）}"
  fi
  head -c 2 "$f" | grep -q '#!' || { rm -f "$f"; die "$url 返回的不是安装脚本"; }
  local rc=0
  bash "$f" "$@" || rc=$?
  rm -f "$f"
  return $rc
}

do_agent_upgrade() {
  need_agent
  local http; http="$(agent_panel_http)"
  if [ -z "$http" ]; then
    http="$(ask "主控面板地址（如 http://1.2.3.4:8080）：")"
    [ -n "$http" ] || die "未提供主控地址"
  fi
  step "从 ${http%/} 升级 Agent"
  run_panel_script "${http%/}/install.sh" --upgrade ${MIRROR:+--mirror "$MIRROR"}
  # On a node without a panel, take the management tool from the panel too.
  if ! installed; then
    local f; f="$(mktemp)"
    if curl -fsSL --connect-timeout 10 "${http%/}/download/install-panel.sh" -o "$f" 2>/dev/null && is_script "$f" && ! cmp -s "$f" "$CTL"; then
      install -m 0755 "$f" "$CTL" && ln -sf "$CTL" "$CTL_ALIAS" && ok "管理脚本已更新为主控上的版本"
    fi
    rm -f "$f"
  fi
}

# do_agent_join LINK: install the agent, or move it to another panel, from the
# one-click link the panel shows (the URL or the whole command both work).
do_agent_join() {
  local link="$1" url tok
  [ -n "$link" ] || die "请提供面板「节点」页生成的一键安装链接"
  url="$(printf '%s' "$link" | grep -oE 'https?://[^ |"'"'"']+' | head -n1)"
  [ -n "$url" ] || die "没有找到链接：请粘贴类似 curl -fsSL http://主控/j/xxxx | bash 的命令或其中的地址"
  if agent_installed; then
    warn "本机已接入主控 $(agent_panel)；继续将改为接入新的主控（旧主控中的该节点会离线）"
  fi
  case "$url" in
    */j/*) run_panel_script "$url" ${MIRROR:+--mirror "$MIRROR"} ;;
    */install.sh)
      tok="$(printf '%s' "$link" | sed -n 's/.*--token[ =]\([A-Za-z0-9_-]*\).*/\1/p')"
      [ -n "$tok" ] || die "命令中没有 --token"
      run_panel_script "$url" --token "$tok" ${MIRROR:+--mirror "$MIRROR"} ;;
    *) run_panel_script "$url" ${MIRROR:+--mirror "$MIRROR"} ;;
  esac
}

do_agent_uninstall() {
  need_agent
  local data; data="$(agent_data)"
  step "卸载节点 Agent"
  systemctl disable --now polaris-agent >/dev/null 2>&1 || true
  rm -f "$AGENT_UNIT" "$AGENT_BIN"
  systemctl daemon-reload
  if [ "$PURGE" = 1 ]; then
    if ls "$data"/*.db >/dev/null 2>&1; then rm -rf "$data/agent" "$data/cores" "$data/run"
    else rm -rf "$data"; fi
    ok "已卸载并删除 Agent 数据"
  else
    ok "已卸载；数据保留在 $data（加 --purge 一并删除）"
  fi
  dim "  记得在面板中删除该节点"
}

agent_cli() { # agent SUBCOMMAND [ARG]
  case "${1:-menu}" in
    menu) agent_menu ;;
    info|status) banner "节点 Agent"; show_agent_info ;;
    log) need_agent; trap 'echo' INT; journalctl -u polaris-agent -n 50 -f --no-pager || true ;;
    start|stop|restart) need_agent; systemctl "$1" polaris-agent && ok "已执行 $1" ;;
    upgrade|update) do_agent_upgrade ;;
    join) do_agent_join "${2:-}" ;;
    uninstall) [ "${2:-}" = --purge ] && PURGE=1; do_agent_uninstall ;;
    *) die "未知的 agent 子命令：$1（status / log / start / stop / restart / upgrade / join 链接 / uninstall）" ;;
  esac
}

agent_menu() {
  set +e
  while true; do
    clear 2>/dev/null || true
    banner "节点 Agent · 管理脚本"
    if agent_installed; then
      kv Agent "$(svc_line polaris-agent)"
      kv 主控 "$(or_dash "$(agent_panel)")"
      kv 版本 "$(agent_version)"
    else
      kv Agent "$(dot) ${C_Y}未安装${C_0}（选 1，粘贴面板生成的一键安装链接）"
    fi
    installed && kv 主控面板 "$(svc_line polaris)（本机）"
    kv 系统 "$(sys_line)"
    section "节点 Agent"
    if agent_installed; then
      menu_row "1|运行状态" "2|实时日志" "3|重启"
      menu_row "4|停止" "5|启动" "6|升级 Agent"
      menu_row "7|更换主控" "8|卸载 Agent"
    else
      menu_row "1|用一键链接安装"
    fi
    echo
    if installed; then menu_row "m|切换到主控面板菜单" "0|退出"; else menu_row "p|安装主控面板" "0|退出"; fi
    echo
    local c; c="$(ask "  请选择：")"
    echo
    if ! agent_installed; then
      case "$c" in
        1) local l; l="$(ask "粘贴一键安装链接或命令：")"; [ -n "$l" ] && run_action do_agent_join "$l"; pause ;;
        m|M) installed && { menu; return; } ;;
        p|P) menu; return ;;
        0|q|Q) exit 0 ;;
      esac
      continue
    fi
    case "$c" in
      1) (show_agent_info) || true; pause ;;
      2) dim "按 Ctrl+C 返回菜单"; trap 'echo' INT; journalctl -u polaris-agent -n 50 -f --no-pager; trap - INT ;;
      3) systemctl restart polaris-agent && ok "已重启"; pause ;;
      4) systemctl stop polaris-agent && ok "已停止"; pause ;;
      5) systemctl start polaris-agent && ok "已启动"; pause ;;
      6)
        local before; before="$(sha256sum "$CTL" 2>/dev/null)"
        run_action do_agent_upgrade; pause
        if [ "$(sha256sum "$CTL" 2>/dev/null)" != "$before" ] && is_script "$CTL"; then exec "$CTL" agent menu; fi ;;
      7) local l; l="$(ask "粘贴新主控的一键安装链接或命令：")"
         if [ -n "$l" ] && confirm "改为接入新的主控？"; then run_action do_agent_join "$l"; fi; pause ;;
      8) if confirm "确定卸载节点 Agent？"; then confirm "同时删除 Agent 数据？" && PURGE=1; run_action do_agent_uninstall; PURGE=0; fi; pause ;;
      m|M) installed && { menu; return; } ;;
      0|q|Q) exit 0 ;;
    esac
  done
}

# role_menu: first run on a bare server: panel or node?
role_menu() {
  clear 2>/dev/null || true
  banner "选择本机角色"
  kv 系统 "$(sys_line)"
  section "这台服务器要做什么"
  menu_row "1|主控面板" "2|节点 Agent"
  dim "  主控：管理节点、用户和订阅的后台"
  dim "  节点：运行代理内核，由主控远程管理"
  echo
  local c; c="$(ask "  请选择 [1-2]：")"
  case "$c" in
    1) menu ;;
    2) agent_menu ;;
    *) exit 0 ;;
  esac
}

# --- interactive menu --------------------------------------------------------

pause() { echo; read -r -p "按回车返回菜单…" _ || true; }
ask() { local v; read -r -p "$1" v || true; echo "$v"; }
confirm() { local v; read -r -p "$1 [y/N] " v || true; [[ "$v" =~ ^[Yy]$ ]]; }
# ask_port LABEL DEFAULT [TAKEN]: prompt until a valid, free port is given.
ask_port() {
  local v def="$2"
  while port_busy "$def" || [ "$def" = "${3:-}" ]; do def=$((def + 1)); done
  while true; do
    v="$(ask "$1 [$def]：")"; v="${v:-$def}"
    if ! valid_port "$v"; then red "端口不正确" >&2
    elif [ "$v" = "${3:-}" ]; then red "不能与面板端口相同" >&2
    elif port_busy "$v"; then red "$(port_msg "$v")" >&2
    else echo "$v"; return; fi
  done
}

# run_action runs an action in a subshell with errexit on. It must not be
# called as part of && / || lists: bash ignores set -e in such contexts.
run_action() { ( set -e; "$@" ); }

# menu_row "N|label" ...: numbered items, one per line (columns break apart
# when a narrow terminal wraps them).
menu_row() {
  local it
  for it; do printf '  %s%3s.%s %s\n' "$C_C" "${it%%|*}" "$C_0" "${it#*|}"; done
}

menu_header() {
  clear 2>/dev/null || true
  banner "多内核代理面板 · 管理脚本"
  if installed; then
    kv 面板 "$(svc_line polaris)"
    kv 后台 "${C_BD}$(env_get POLARIS_PUBLIC_URL)$(env_get POLARIS_ADMIN_PATH)/${C_0}"
    systemctl cat polaris-agent >/dev/null 2>&1 && kv 本机节点 "$(svc_line polaris-agent)"
  else
    kv 面板 "$(dot) ${C_Y}未安装${C_0}（选 1 安装）"
  fi
  kv 系统 "$(sys_line)"
  local latest=""
  [ -s "${LATEST_FILE:-}" ] && latest="$(cat "$LATEST_FILE")"
  if [ -n "$latest" ] && installed && [ "$latest" != "$(panel_version)" ]; then
    kv 更新 "${C_Y}发现新版本 $latest${C_0}，选 2 升级"
  fi
}

menu() {
  set +e # a failed menu step must not exit the menu
  # Look up the newest release in the background so the menu opens at once.
  LATEST_FILE="$(mktemp)"
  trap 'rm -f "$LATEST_FILE"' EXIT
  (latest_version 8 > "$LATEST_FILE") &
  local auto node
  while true; do
    menu_header
    if systemctl is-enabled --quiet polaris 2>/dev/null; then auto="开"; else auto="关"; fi
    if systemctl cat polaris-agent >/dev/null 2>&1; then node="升级本机节点"; else node="安装本机节点"; fi
    section "安装与升级"
    menu_row "1|安装面板" "2|升级面板" "3|检查更新"
    menu_row "4|卸载面板"
    section "服务管理"
    menu_row "5|启动" "6|停止" "7|重启"
    menu_row "8|运行状态" "9|实时日志" "10|开机自启 [$auto]"
    section "面板设置"
    menu_row "11|面板信息" "12|修改后台路径" "13|修改面板端口"
    menu_row "14|绑定域名 HTTPS" "15|重置管理员密码" "16|关闭二次验证"
    section "工具"
    menu_row "17|备份配置与数据" "18|$node" "19|系统诊断"
    echo
    if agent_installed; then menu_row "a|切换到节点 Agent 菜单" "0|退出"; else menu_row "0|退出"; fi
    echo
    local c; c="$(ask "  请选择 [0-19]：")"
    echo
    case "$c" in
      1)
        if installed; then warn "已安装：升级请选 2，重装请先卸载"; pause; continue; fi
        DOMAIN="$(ask "域名（已解析到本机，留空则使用 IP + HTTP）：")"
        if [ -n "$DOMAIN" ] && ! confirm "自动安装 Caddy 申请 HTTPS 证书？（选 N 表示你自备反向代理）"; then CADDY=0; else CADDY=1; fi
        PORT="$(ask_port "面板端口" 8080)"
        AGENT_PORT="$(ask_port "节点接入端口" 9443 "$PORT")"
        run_action do_install; pause ;;
      2)
        local before; before="$(sha256sum "$CTL" 2>/dev/null)"
        run_action do_update; pause
        # The menu runs the old code from memory: restart it on the new script.
        if [ "$(sha256sum "$CTL" 2>/dev/null)" != "$before" ] && is_script "$CTL"; then exec "$CTL" menu; fi ;;
      3) run_action do_check_update; pause ;;
      4)
        if confirm "确定卸载 Polaris？"; then
          confirm "同时删除配置与全部数据（不可恢复）？" && PURGE=1
          run_action do_uninstall; PURGE=0
        fi
        pause ;;
      5) need_installed_soft && { systemctl start polaris && wait_active && ok "已启动"; }; pause ;;
      6) need_installed_soft && { systemctl stop polaris && ok "已停止"; }; pause ;;
      7) need_installed_soft && { systemctl restart polaris && wait_active && ok "已重启"; }; pause ;;
      8) need_installed_soft && systemctl status polaris --no-pager -l | head -n 20; pause ;;
      9) need_installed_soft && { dim "按 Ctrl+C 返回菜单"; do_log; }; ;;
      10)
        if need_installed_soft; then
          if systemctl is-enabled --quiet polaris; then systemctl disable polaris >/dev/null 2>&1 && ok "已关闭开机自启"
          else systemctl enable polaris >/dev/null 2>&1 && ok "已开启开机自启"; fi
        fi
        pause ;;
      11) (show_info) || true; pause ;;
      12)
        local np; np="$(ask "新的后台路径（留空随机生成，输入 / 关闭路径）：")"
        run_action do_set_path "${np:-$(rand 12)}"; pause ;;
      13)
        local nport; nport="$(ask "新的面板端口：")"
        if [ -n "$nport" ]; then run_action do_set_port "$nport"; fi; pause ;;
      14)
        local nd; nd="$(ask "域名（需已解析到本机）：")"
        if [ -n "$nd" ] && confirm "将安装 / 配置 Caddy 并切换到 https://$nd，旧的 http 订阅链接会失效，继续？"; then run_action do_set_domain "$nd"; fi
        pause ;;
      15)
        ADMIN_USER="$(ask "管理员用户名（留空为第一个管理员）：")"
        run_action do_reset_password; ADMIN_USER=""; pause ;;
      16)
        ADMIN_USER="$(ask "管理员用户名（留空为第一个管理员）：")"
        if confirm "关闭该管理员的二次验证？"; then run_action do_reset_2fa; fi
        ADMIN_USER=""; pause ;;
      17) run_action do_backup; pause ;;
      18)
        if need_installed_soft; then
          if ! systemctl cat polaris-agent >/dev/null 2>&1; then NODE_NAME="$(ask "节点名称（留空为「本机」）：")"; fi
          run_action do_node; NODE_NAME=""
        fi
        pause ;;
      19) run_action do_doctor; pause ;;
      a|A) agent_installed && { agent_menu; return; } ;;
      0|q|Q) exit 0 ;;
      *) ;;
    esac
  done
}
need_installed_soft() { installed || { bad "Polaris 尚未安装"; return 1; }; }

# --- entry point ----------------------------------------------------------------

ACTION=""
while [ $# -gt 0 ]; do
  case "$1" in
    install|update|uninstall|start|stop|restart|status|log|info|reset-password|reset-2fa|backup|menu|node|doctor|check-update) ACTION="$1"; shift ;;
    self-update) ACTION=self-update; shift ;;
    update-request) ACTION=update-request; shift ;;
    agent) ACTION=agent; shift; AGENT_SUB="${1:-menu}"; [ $# -gt 0 ] && shift; AGENT_ARG="${1:-}"; [ $# -gt 0 ] && shift ;;
    join) ACTION=agent; AGENT_SUB="join"; AGENT_ARG="${2:-}"; shift; [ $# -gt 0 ] && shift ;;
    --force) FORCE=1; shift ;;
    -n) NODE_NAME="$2"; shift 2 ;;
    set-path|set-port|set-domain|set-url) ACTION="$1"; [ $# -ge 2 ] || die "$1 需要一个参数"; ARG="$2"; shift 2 ;;
    --domain) DOMAIN="$2"; shift 2 ;;
    --no-caddy) CADDY=0; shift ;;
    --version) if [ $# -ge 2 ] && [ "${2#-}" = "$2" ]; then VERSION="$2"; shift 2; else ACTION=version; shift; fi ;;
    version|-v) ACTION=version; shift ;;
    help) sed -n '2,22p' "$CTL" 2>/dev/null || sed -n '2,22p' "$0"; exit 0 ;;
    --port) PORT="$2"; shift 2 ;;
    --agent-port) AGENT_PORT="$2"; shift 2 ;;
    --admin-path) ADMIN_PATH="$2"; shift 2 ;;
    --mirror) MIRROR="$2"; shift 2 ;;
    --purge) PURGE=1; shift ;;
    -u) ADMIN_USER="$2"; shift 2 ;;
    -h|--help) sed -n '2,22p' "$0" 2>/dev/null || echo "see $RAW_SCRIPT"; exit 0 ;;
    *) die "未知参数：$1（--help 查看用法）" ;;
  esac
done

# --mirror none forgets a saved mirror; otherwise reuse the one that last worked.
if [ "$MIRROR" = none ]; then MIRROR=""; rm -f "$MIRROR_FILE" 2>/dev/null || true
elif [ -z "$MIRROR" ] && [ -s "$MIRROR_FILE" ]; then MIRROR="$(head -n1 "$MIRROR_FILE")"; fi

[ "$ACTION" = version ] && { echo "polaris $( [ -x "$BIN" ] && "$BIN" --version | awk '{print $2}' || echo 未安装)"; exit 0; }
preflight
if [ -z "$ACTION" ]; then
  # Flags without a command (e.g. --domain x) keep the old "install" meaning;
  # no arguments at all on a terminal opens the menu.
  if [ -n "$DOMAIN" ] || [ "$PORT" != 8080 ] || [ -n "$VERSION" ] || [ -n "$ADMIN_PATH" ]; then ACTION=install
  elif [ -t 0 ]; then ACTION=menu
  elif installed; then ACTION=info
  elif agent_installed; then ACTION=agent AGENT_SUB=info
  else ACTION=install; fi
fi

case "$ACTION" in
  menu)
    migrate_layout
    # The panel menu on a panel server, the node menu on a node, and a role
    # choice on a bare server.
    if installed; then menu
    elif agent_installed; then agent_menu
    else role_menu; fi ;;
  agent) agent_cli "$AGENT_SUB" "$AGENT_ARG" ;;
  self-update) banner "更新管理脚本"; update_self ;;
  install) do_install ;;
  update) do_update ;;
  update-request) do_update_request ;;
  uninstall) do_uninstall ;;
  start) need_installed; systemctl start polaris; wait_active && ok "已启动" ;;
  stop) need_installed; systemctl stop polaris; ok "已停止" ;;
  restart) need_installed; systemctl restart polaris; wait_active && ok "已重启" ;;
  status) need_installed; systemctl status polaris --no-pager -l ;;
  log) need_installed; do_log ;;
  info) banner "面板信息"; show_info ;;
  doctor) do_doctor ;;
  check-update) do_check_update ;;
  reset-password) do_reset_password ;;
  reset-2fa) do_reset_2fa ;;
  set-path) do_set_path "$ARG" ;;
  set-port) do_set_port "$ARG" ;;
  set-domain) do_set_domain "$ARG" ;;
  set-url) do_set_url "$ARG" ;;
  backup) do_backup ;;
  node) do_node ;;
esac
