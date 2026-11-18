#!/usr/bin/env bash
# Polaris panel installer and management tool.
#
# Install (then manage any time by typing: polaris):
#   bash <(curl -fsSL https://raw.githubusercontent.com/cuijianzhuang/Polaris/main/scripts/install-panel.sh)
#
# With no arguments an interactive menu opens. Non-interactive commands:
#   install [--pro] [--domain NAME] [--no-caddy] [--version TAG] [--port N] [--agent-port N] [--admin-path P] [--restore FILE]
#           [--db sqlite|local|DSN] [--redis local|URL]   database (asked on a terminal; default sqlite) and optional Redis
#   migrate sqlite|local|DSN      move the data to another database (local = PostgreSQL installed on this server)
#   update [--version TAG] [--force] [--pro|--community]  upgrade binaries, keep config and data (database backed up first)
#   check-update                  compare the installed version with the latest release
#   update-request                run an update the panel asked for (used by polaris-update.service)
#   switch [pro|community]        switch between the community and professional editions (same version)
#   uninstall [--purge]           remove; --purge also deletes config and data
#   start | stop | restart | status | log
#   info                          admin URL, ports and config summary
#   reset-password [-u NAME]      new random admin password, unlocks the account, signs out sessions
#   reset-2fa [-u NAME]           turn off two-factor authentication
#   set-path PATH | set-port N    change the admin path (/ turns it off) / panel port
#   set-domain NAME               switch to HTTPS on a domain via Caddy
#   set-sub-domain NAME           serve subscriptions on their own HTTPS domain via Caddy
#   set-url URL                   public address used in subscription / install links
#   backup [--passphrase P]       backup to /root (with a passphrase: encrypted, restores on any server)
#   restore FILE [--passphrase P] replace the data with a backup (made here or on another server)
#   node [-n NAME]                install a node agent on this server (connects over 127.0.0.1)
#   self-update                   update this management tool to the latest release
#   join LINK                     install the node agent from the panel's one-click link (or move to another panel)
#   agent [status|log|start|stop|restart|upgrade|uninstall [--purge]]   manage the node agent
#   doctor                        check service, ports, firewall, DNS/HTTPS, clock and disk
#   ha-init [--dry-run] [--endpoint IP]   turn this machine into the HA primary (WireGuard + PG/Redis primary + token)
#   ha-join <token> <panel-url> [--dry-run] [--finish]   join this machine as the HA standby (panel-url includes the admin path when one is set)
#   HA_PROXY_MODE=auto|nginx|openresty (env for ha-join): which reverse proxy
#   serves the standby on 443. auto (default) installs nginx when 443 is free
#   and reuses OpenResty (e.g. installed by 1Panel) when it already owns 443.
#   ha-status                     local HA state: role, WireGuard, PostgreSQL, Redis, panel
#   witness-init [--dry-run] [--listen ADDR]   install the HA witness arbiter on this machine (third host; default listen 0.0.0.0:8443)
#   pg-upgrade [--to VER] [--dry-run]   upgrade the local PostgreSQL cluster to a newer major version (Debian pg_upgradecluster; the upgraded cluster keeps the original port)
#   ha-token | ha-seal-secret NAME | ha sync-wg | witness serve   panel-binary maintenance commands (forwarded to polaris-server)
#
# Options for install/update: --mirror URL (download prefix for github.com)
#   --pro / --community: the professional edition (paid features, needs a
#   license) or the community edition; updates keep the installed one.
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

FORCE=0 EDITION="" DOMAIN="" CADDY=1 VERSION="" PORT=8080 AGENT_PORT=9443 ADMIN_PATH="" MIRROR="" PURGE=0 ADMIN_USER="" NODE_NAME="" AGENT_SUB="" AGENT_ARG=""
HA_DRYRUN=0 HA_FINISH=0 HA_TOKEN="" HA_PANEL_URL="" HA_API_URL="" HA_JOIN=0 HA_ENDPOINT="" SECRET_OVERRIDE="" HA_WITNESS_LISTEN="" HA_PROXY_MODE="auto" HA_PG_CLUSTER="" PG_UPGRADE_TO=""
BK_PASS="${POLARIS_BACKUP_PASSPHRASE:-}" RESTORE_FILE=""
DB_CHOICE="" REDIS_CHOICE="" DB_DSN="" REDIS_URL=""
# A PostgreSQL on this server, reached over its Unix socket as the polaris
# system user (peer authentication: no password to keep).
# (postgresql://polaris@/polaris?host=SOCKET_DIR[&port=N]: setup_local_pg
# writes the cluster's own socket directory and port, which Debian moves off
# 5432 when that port is taken.)
is_local_pg() { case "$1" in "postgresql://polaris@/polaris?host="*) return 0 ;; esac; return 1; }
DB_DROPIN=/etc/systemd/system/polaris.service.d/database.conf

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
env_get() { sed -n "s/^$1=//p" "$ENV_FILE" 2>/dev/null | tail -n1 | sed "s/^'\\(.*\\)'$/\\1/" || true; }
env_set() { # key value
  local v="$2"
  # Values with shell-special characters (a DSN's & or spaces) are single-
  # quoted: systemd and the shell both read that form.
  case "$v" in *[!A-Za-z0-9_./:@%+=,-]*) v="'$v'" ;; esac
  if grep -q "^$1=" "$ENV_FILE"; then
    local e="${v//\\/\\\\}"; e="${e//&/\\&}"; e="${e//|/\\|}"
    sed -i "s|^$1=.*|$1=$e|" "$ENV_FILE"
  else
    printf '%s=%s\n' "$1" "$v" >> "$ENV_FILE"
  fi
}
env_del() { sed -i "/^$1=/d" "$ENV_FILE"; }
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
    "$(pad 地址 8)${C_BD}$(admin_url)${C_0}" \
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
# installed_edition: the edition of the installed panel — "pro", "community",
# or "legacy" for builds from before the split (they print no edition and
# contained the paid features).
installed_edition() {
  local e=""
  [ -x "$BIN" ] && e="$("$BIN" --version 2>/dev/null | awk '{print $3}')"
  case "$e" in pro | community) echo "$e" ;; "") if [ -x "$BIN" ]; then echo legacy; else echo community; fi ;; *) echo community ;; esac
}
edition_label() { case "$1" in pro) echo 专业版 ;; legacy) echo 旧版 ;; *) echo 社区版 ;; esac; }

# resolve_edition: --pro / --community, else POLARIS_EDITION, else what is
# installed (a pre-split build moves to pro, which keeps its features), else
# community. EP is the release asset infix ("pro-" or ""). EDITION_GUESSED
# marks a choice nobody made, which may fall back to community.
resolve_edition() {
  EDITION_GUESSED=""
  [ -n "$EDITION" ] || EDITION="${POLARIS_EDITION:-}"
  if [ -z "$EDITION" ] && [ -x "$BIN" ]; then
    EDITION="$(installed_edition)"
    [ "$EDITION" = legacy ] && EDITION=pro EDITION_GUESSED=1
  fi
  [ -n "$EDITION" ] || EDITION=community
  case "$EDITION" in
    pro) EP="pro-" ;;
    community) EP="" ;;
    *) die "未知版本：${EDITION}（可选 pro / community）" ;;
  esac
}

# choose_edition asks which edition to install when nobody said and someone
# is at the terminal (piped installs default to community).
choose_edition() {
  [ -z "$EDITION" ] && [ -z "${POLARIS_EDITION:-}" ] && [ -t 0 ] || return 0
  echo
  printf '  %s选择版本%s（之后可以随时切换：polaris switch）\n' "$C_BD" "$C_0"
  printf '  %s1.%s 社区版  开源免费，包含全部基础功能\n' "$C_C" "$C_0"
  printf '  %s2.%s 专业版  另含客户独立限速、订阅转换、DDNS、Telegram 管理菜单、备份推送；需要许可证（面板「许可证」页面激活）\n' "$C_C" "$C_0"
  local c
  read -r -p "  请选择 [1-2，默认 1]：" c || true
  case "$c" in 2) EDITION=pro ;; *) EDITION=community ;; esac
  ok "安装$(edition_label "$EDITION")"
}

# --- database ----------------------------------------------------------------------

# choose_database picks the database (and optional Redis) for a new install:
# --db / --redis when given, a question on a terminal, SQLite otherwise.
choose_database() {
  if [ -z "$DB_CHOICE" ] && [ -t 0 ]; then
    echo
    printf '  %s选择数据库%s（之后可以切换：polaris migrate）\n' "$C_BD" "$C_0"
    printf '  %s1.%s SQLite              免安装，单个主控、几千用户以内足够（推荐）\n' "$C_C" "$C_0"
    printf '  %s2.%s PostgreSQL（本机）  自动安装到这台服务器；用户、节点很多或流量记录量大时选它\n' "$C_C" "$C_0"
    printf '  %s3.%s PostgreSQL（已有）  云数据库或另一台服务器，准备运行多个主控实例时选它\n' "$C_C" "$C_0"
    local c
    read -r -p "  请选择 [1-3，默认 1]：" c || true
    case "$c" in
      2) DB_CHOICE=local ;;
      3) while [ -z "$DB_CHOICE" ]; do
           read -r -p "  连接地址（postgres://用户:密码@主机:5432/库名）：" DB_CHOICE || true
         done ;;
      *) DB_CHOICE=sqlite ;;
    esac
  fi
  [ -n "$DB_CHOICE" ] || DB_CHOICE=sqlite
  case "$DB_CHOICE" in
    sqlite | local) ;;
    postgres://* | postgresql://* | host=* | dbname=* | user=*) DB_DSN="$DB_CHOICE" ;;
    *) die "数据库参数不正确：${DB_CHOICE}（可选 sqlite / local / postgres://…）" ;;
  esac
  case "$DB_CHOICE" in *"'"*) die "数据库地址不能包含单引号" ;; esac

  # Redis only helps several panel instances share state.
  if [ -z "$REDIS_CHOICE" ] && [ "$DB_CHOICE" != sqlite ] && [ -t 0 ]; then
    echo
    printf '  %sRedis%s（可选：多个主控实例共享在线 IP、限流计数和告警去重）\n' "$C_BD" "$C_0"
    printf '  %s1.%s 不使用       只运行一个主控时不需要（推荐）\n' "$C_C" "$C_0"
    printf '  %s2.%s 本机安装     自动安装到这台服务器\n' "$C_C" "$C_0"
    printf '  %s3.%s 已有 Redis   输入连接地址\n' "$C_C" "$C_0"
    local c
    read -r -p "  请选择 [1-3，默认 1]：" c || true
    case "$c" in
      2) REDIS_CHOICE=local ;;
      3) while [ -z "$REDIS_CHOICE" ]; do
           read -r -p "  连接地址（redis://:密码@主机:6379/0）：" REDIS_CHOICE || true
         done ;;
      *) REDIS_CHOICE=none ;;
    esac
  fi
  case "${REDIS_CHOICE:-none}" in
    none | local) ;;
    redis://* | rediss://*) REDIS_URL="$REDIS_CHOICE" ;;
    *) die "Redis 参数不正确：${REDIS_CHOICE}（可选 local / redis://…）" ;;
  esac
  case "$REDIS_CHOICE" in *"'"*) die "Redis 地址不能包含单引号" ;; esac
}

db_label() { # DSN -> what the user sees
  case "$1" in
    "") echo "SQLite" ;;
    "postgresql://polaris@/polaris?host="*) echo "PostgreSQL（本机）" ;;
    *) echo "PostgreSQL（$(printf '%s' "$1" | sed -E 's#^[a-z]+://([^@/]*@)?##; s#[/?].*##; s#^.*host=([^ ]*).*#\1#')）" ;;
  esac
}

pkg_install() { # packages...
  if command -v apt-get >/dev/null; then
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$@" >/dev/null 2>&1 \
      || { apt-get update -qq >/dev/null 2>&1; DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$@" >/dev/null; }
  elif command -v dnf >/dev/null; then dnf install -y -q "$@" >/dev/null
  elif command -v yum >/dev/null; then yum install -y -q "$@" >/dev/null
  else return 1; fi
}

pg_su() { (cd / && runuser -u postgres -- psql -X -q -v ON_ERROR_STOP=1 "$@"); }

# setup_local_pg installs PostgreSQL on this server if needed and creates the
# polaris role and database (owned by it, peer authentication).
setup_local_pg() {
  if ! command -v pg_isready >/dev/null && ! ls /usr/lib/postgresql/*/bin/pg_isready >/dev/null 2>&1; then
    step "安装 PostgreSQL"
    if command -v apt-get >/dev/null; then pkg_install postgresql || die "PostgreSQL 安装失败"
    else
      pkg_install postgresql-server postgresql || die "PostgreSQL 安装失败：请手动安装后用连接地址安装（--db postgres://…）"
      [ -f /var/lib/pgsql/data/PG_VERSION ] || postgresql-setup --initdb >/dev/null 2>&1 || true
    fi
  else
    step "使用本机 PostgreSQL"
  fi
  systemctl enable --now postgresql >/dev/null 2>&1 || true
  local i
  for i in $(seq 1 20); do
    pg_su -tAc 'SELECT 1' >/dev/null 2>&1 && break
    [ "$i" = 20 ] && die "PostgreSQL 未能启动（systemctl status postgresql 查看）"
    sleep 1
  done
  id polaris >/dev/null 2>&1 || useradd --system --home-dir "$DATA" --shell /usr/sbin/nologin polaris
  [ "$(pg_su -tAc "SELECT 1 FROM pg_roles WHERE rolname = 'polaris'")" = 1 ] || pg_su -c "CREATE ROLE polaris LOGIN"
  # UTF8 whatever the server's locale: a cluster made under LANG=C defaults to
  # SQL_ASCII, which the panel cannot use. An empty database from an earlier
  # attempt with another encoding is recreated.
  local enc
  enc="$(pg_su -tAc "SELECT pg_encoding_to_char(encoding) FROM pg_database WHERE datname = 'polaris'")"
  if [ -n "$enc" ] && [ "$enc" != UTF8 ]; then
    if [ "$(pg_su -d polaris -tAc "SELECT to_regclass('public.admins') IS NOT NULL")" = t ] &&
      [ "$(pg_su -d polaris -tAc "SELECT EXISTS (SELECT 1 FROM public.admins)")" = t ]; then
      die "本机数据库 polaris 的编码是 ${enc}（需要 UTF8）且已有数据：请先备份并删除它（sudo -u postgres dropdb polaris）后重试"
    fi
    pg_su -c "DROP DATABASE polaris"
    enc=""
  fi
  [ -n "$enc" ] || pg_su -c "CREATE DATABASE polaris OWNER polaris ENCODING 'UTF8' TEMPLATE template0 LC_COLLATE 'C' LC_CTYPE 'C'"
  # The cluster's real socket directory and port: psql finds a cluster on
  # another port by itself (Debian's wrapper), the panel does not.
  local port sock
  port="$(pg_su -tAc 'SHOW port')"
  sock="$(pg_su -tAc 'SHOW unix_socket_directories' | cut -d, -f1 | tr -d ' ')"
  [[ "$port" =~ ^[0-9]+$ ]] || die "无法读取 PostgreSQL 端口"
  case "$sock" in /*) ;; *) sock=/var/run/postgresql ;; esac
  DB_DSN="postgresql://polaris@/polaris?host=$sock"
  [ "$port" = 5432 ] || DB_DSN="$DB_DSN&port=$port"
  ok "数据库 polaris（本机，Unix socket 免密码连接）"
}

# check_pg_target: the database answers and, for a new install, holds no
# Polaris data (it would be encrypted with another install's key).
check_pg_target() { # dsn fresh(0/1)
  command -v psql >/dev/null || { dim "  未安装 psql，跳过连接检查（启动时检查）"; return 0; }
  local q=(psql -X -tA "$1")
  # A local database is reached as the polaris user (peer authentication),
  # with the port pinned so psql cannot pick another cluster.
  if is_local_pg "$1"; then
    local port
    port="$(printf '%s' "$1" | sed -n 's/.*[?&]port=\([0-9]*\).*/\1/p')"
    q=(env -C / PGPORT="${port:-5432}" runuser -u polaris -- psql -X -tA "$1")
  fi
  "${q[@]}" -c 'SELECT 1' >/dev/null 2>&1 || die "无法连接数据库：$(db_label "$1")，请检查地址、账号和网络"
  local enc
  enc="$("${q[@]}" -c "SELECT pg_encoding_to_char(encoding) FROM pg_database WHERE datname = current_database()" 2>/dev/null)"
  [ -z "$enc" ] || [ "$enc" = UTF8 ] || die "数据库编码是 ${enc}，需要 UTF8：请用 CREATE DATABASE … ENCODING 'UTF8' TEMPLATE template0 重新建库"
  # Empty tables (left by a migration that failed after creating them) are
  # fine; an admin account means real data.
  if [ "$2" = 1 ] && [ "$("${q[@]}" -c "SELECT to_regclass('admins') IS NOT NULL" 2>/dev/null)" = t ] &&
    [ "$("${q[@]}" -c "SELECT EXISTS (SELECT 1 FROM admins)" 2>/dev/null)" = t ]; then
    die "数据库里已有 Polaris 数据（可能来自之前的安装，用的是另一把密钥）。请换一个空数据库，或先清空它"
  fi
}

setup_local_redis() {
  if ! command -v redis-server >/dev/null; then
    step "安装 Redis"
    if command -v apt-get >/dev/null; then pkg_install redis-server || die "Redis 安装失败"
    else pkg_install redis || die "Redis 安装失败"; fi
  fi
  systemctl enable --now redis-server >/dev/null 2>&1 || systemctl enable --now redis >/dev/null 2>&1 || true
  REDIS_URL="redis://127.0.0.1:6379/0"
  ok "Redis（本机 127.0.0.1:6379）"
}

# db_dropin starts the panel after a database or Redis on this server.
db_dropin() {
  if is_local_pg "$(env_get POLARIS_DB_DSN)" || [ "$(env_get POLARIS_REDIS_URL)" = "redis://127.0.0.1:6379/0" ]; then
    mkdir -p "${DB_DROPIN%/*}"
    printf '[Unit]\nAfter=postgresql.service redis-server.service redis.service\n' > "$DB_DROPIN"
  else
    rm -f "$DB_DROPIN"
  fi
  systemctl daemon-reload
}

# do_migrate moves the data to another database: sqlite, local (PostgreSQL
# on this server) or a PostgreSQL DSN. The old database is left untouched.
do_migrate() {
  need_installed
  grep -qaF 'polaris migrate' "$BIN" || die "当前版本不支持迁移，请先执行 polaris update 升级"
  local cur target="$ARG"
  cur="$(env_get POLARIS_DB_DSN)"
  case "$target" in
    sqlite) target="" ;;
    local) ;;
    postgres://* | postgresql://* | host=* | dbname=* | user=*) ;;
    *) die "用法：polaris migrate sqlite|local|postgres://…" ;;
  esac
  case "$target" in *"'"*) die "数据库地址不能包含单引号" ;; esac
  banner "迁移数据库"
  kv 当前 "$(db_label "$cur")"
  if [ "$target" = local ]; then setup_local_pg; target="$DB_DSN"; fi
  kv 目标 "$(db_label "$target")"
  [ "$cur" != "$target" ] || die "目标与当前数据库相同"
  local to="$target"
  if [ -z "$to" ]; then
    to="$DATA/polaris.db"
    [ ! -e "$to" ] || die "$to 已存在：请先移走它再迁移回 SQLite"
  else
    check_pg_target "$to" 1
  fi
  [ "$FORCE" = 1 ] || confirm "迁移期间面板会停止（通常不到一分钟），继续？" || die "已取消（非交互运行请加 --force）"
  step "停止面板并复制数据"
  systemctl stop polaris 2>/dev/null || true
  if ! polaris_cmd migrate -to "$to"; then
    systemctl start polaris 2>/dev/null || true
    die "迁移失败，面板已用原数据库重新启动，数据未改动"
  fi
  step "切换到新数据库并启动"
  if [ -z "$target" ]; then env_del POLARIS_DB_DSN; else env_set POLARIS_DB_DSN "$target"; fi
  db_dropin
  systemctl start polaris && wait_active || die "面板启动失败（polaris log 查看）；改回原数据库：编辑 $ENV_FILE 的 POLARIS_DB_DSN"
  ok "已切换到 $(db_label "$target")"
  dim "  原数据库保持原样，确认一切正常后可以自行删除"
}

# do_switch moves an installed panel to the other edition at its current
# version (configuration and data are shared; nothing else changes).
do_switch() {
  need_installed
  local cur target
  cur="$(installed_edition)"
  case "${ARG:-}" in
    pro | community) target="$ARG" ;;
    "") if [ "$cur" = pro ]; then target=community; else target=pro; fi ;;
    *) die "用法：polaris switch [pro|community]" ;;
  esac
  if [ "$cur" = "$target" ]; then ok "已经是$(edition_label "$target")"; return 0; fi
  banner "切换版本"
  kv 当前 "$(panel_version) $(edition_label "$cur")"
  kv 切换到 "${C_BD}$(edition_label "$target")${C_0}"
  if [ "$target" = community ]; then
    warn "社区版不含客户独立限速、订阅转换、DDNS、Telegram 管理菜单、备份推送：这些设置会保留，但不再生效"
  else
    dim "    专业版功能需要许可证：切换后在面板「许可证」页面激活"
  fi
  if [ -t 0 ] && [ "$FORCE" = 0 ] && ! confirm "  确定切换？"; then return 0; fi
  EDITION="$target"
  # Same version, other edition (pre-split builds go to the latest release).
  [ "$cur" = legacy ] || VERSION="$(panel_version)"
  do_update
}
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
# admin_url: the admin address (no double slash when the path is "/").
admin_url() {
  local p
  p="$(env_get POLARIS_ADMIN_PATH)"
  printf '%s%s/' "$(env_get POLARIS_PUBLIC_URL)" "${p%/}"
}

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

# wait_healthy: the service is up AND answers HTTP on its port (any status
# counts: the admin path may 404 by design) and still does a few seconds
# later, so a crash loop that is briefly "active" does not pass.
wait_healthy() {
  local listen host port _ hits=0
  listen="$(env_get POLARIS_LISTEN)"; port="${listen##*:}"; port="${port:-8080}"
  host="${listen%:*}"
  case "$host" in ""|0.0.0.0|"[::]"|::|"$listen") host=127.0.0.1 ;; esac
  for _ in $(seq 1 20); do
    if running && curl -s -o /dev/null -m 2 "http://$host:$port/"; then
      hits=$((hits + 1))
      [ "$hits" -ge 2 ] && { scrub_initial_password; return 0; }
    else
      hits=0
    fi
    sleep 1
  done
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

# fetch_origin URL OUT [progress]: download straight from GitHub, never through
# a mirror. Checksums are read this way: a mirror relaying both a binary and its
# checksum could pair a backdoored binary with a matching hash.
fetch_origin() {
  dl "$1" "$2" "${3:-}"
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
  if ! fetch_origin "$RELEASE/checksums.txt" "$d/checksums.txt" || ! grep -q ' \*\?install-panel.sh$' "$d/checksums.txt" ||
    ! fetch "$RELEASE/install-panel.sh" "$d/install-panel.sh"; then
    rm -rf "$d"; warn "无法下载 $VERSION 的管理脚本，脚本未更新"; return 0
  fi
  want="$(awk '$2 == "install-panel.sh" || $2 == "*install-panel.sh" {print $1}' "$d/checksums.txt")"
  got="$(sha256sum "$d/install-panel.sh" | awk '{print $1}')"
  if [ -z "$want" ] || [ "$want" != "$got" ]; then
    rm -rf "$d"; warn "管理脚本校验失败，未更新"; return 0
  fi
  if cmp -s "$d/install-panel.sh" "$CTL"; then
    ok "管理脚本已是最新（${VERSION}）"
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
  resolve_edition
  step "下载 Polaris $VERSION $(edition_label "$EDITION") ($ARCH)${MIRROR:+ · 镜像 $MIRROR}"
  PANEL_TGZ="polaris-panel-$EP$VERSION-linux-$ARCH.tar.gz"
  AGENT_ASSET="polaris-agent-${EP}linux-$ARCH"
  fetch_origin "$RELEASE/checksums.txt" "$TMP/checksums.txt" ||
    die "下载 checksums.txt 失败（${RELEASE}）。校验和只从 GitHub 官方下载，镜像仅用于加速软件包；请检查服务器到 github.com 的连通性。"
  if ! grep -q " \*\?$PANEL_TGZ\$" "$TMP/checksums.txt"; then
    [ -n "$EDITION_GUESSED" ] || die "$VERSION 没有$(edition_label "$EDITION")的发布包（${PANEL_TGZ}）"
    warn "$VERSION 没有专业版发布包，改为安装社区版（不含客户独立限速、订阅转换、DDNS、Telegram 管理菜单、备份推送）"
    EDITION=community EP="" EDITION_GUESSED=""
    PANEL_TGZ="polaris-panel-$VERSION-linux-$ARCH.tar.gz"
    AGENT_ASSET="polaris-agent-linux-$ARCH"
  elif [ -n "$EDITION_GUESSED" ]; then
    ok "当前版本包含高级功能，升级为专业版以保留它们（需要许可证，有 30 天试用；改用社区版：polaris update --community）"
  fi
  local f want got
  # Only this machine's architecture: the panel fetches another agent build
  # itself the first time a node of that architecture needs it.
  local files=("$PANEL_TGZ" "$AGENT_ASSET")
  # Releases before the installer existed do not ship it; only fetch what is listed.
  grep -q ' \*\?install-panel.sh$' "$TMP/checksums.txt" && files+=(install-panel.sh)
  for f in "${files[@]}"; do
    dim "    $f"
    fetch "$RELEASE/$f" "$TMP/$f" progress || die "下载 $f 失败（直连与镜像均不可用；可用 --mirror 指定其他镜像）"
    want="$(awk -v f="$f" '$2 == f || $2 == "*"f {print $1}' "$TMP/checksums.txt")"
    got="$(sha256sum "$TMP/$f" | awk '{print $1}')"
    [ -n "$want" ] && [ "$want" = "$got" ] || die "$f 校验失败（期望 ${want:-无}，实际 ${got}）"
  done
  ok "SHA-256 校验通过（${#files[@]} 个文件）"
  tar -xzf "$TMP/$PANEL_TGZ" -C "$TMP"
}

install_files() {
  install -m 0755 "$TMP/polaris" "$BIN"
  # Agent binaries served at /download/ so node installs pull them from this panel.
  install -d -m 0755 -o polaris -g polaris "$DATA" "$DATA/downloads"
  # Nodes always ask for polaris-agent-linux-<arch>: the edition's build goes there.
  install -m 0755 -o polaris -g polaris "$TMP/$AGENT_ASSET" "$DATA/downloads/polaris-agent-linux-$ARCH"
  local mark="$VERSION${EP:+ pro}" # the panel compares this with its own version and edition
  printf '%s\n' "$mark" > "$DATA/downloads/.version-$ARCH"
  chown polaris:polaris "$DATA/downloads/.version-$ARCH"
  # Builds of the other architecture from an older version would be served
  # with a stale checksum: drop them (re-fetched on demand), and the old
  # shared marker with them.
  local a
  for a in amd64 arm64; do
    [ "$a" = "$ARCH" ] && continue
    if [ "$(cat "$DATA/downloads/.version-$a" 2>/dev/null)" != "$mark" ]; then
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
  local src="" d="" rc=0
  if [ -f "${TMP:-}/install-panel.sh" ]; then
    src="$TMP/install-panel.sh"
  elif [ -f "$0" ] && [ "$(readlink -f "$0")" != "$(readlink -f "$CTL" 2>/dev/null)" ]; then
    src="$0"
  elif ! is_script "$CTL"; then
    # The running copy came from a pipe (bash <(curl …)), so there is no file
    # to install. Take the script of the release being installed, verified
    # against its checksums: tracking main would put an unreviewed and
    # unverified script behind the root "polaris" command.
    if [ -z "$VERSION" ]; then
      VERSION="$(latest_version 15)" && RELEASE="${POLARIS_RELEASE_URL:-https://github.com/$REPO/releases/download/$VERSION}"
    fi
    local want got
    d="$(mktemp -d "${TMP:-${TMPDIR:-/tmp}}/ctl.XXXXXXXX")" || return $?
    if [ -n "$VERSION" ] && fetch_origin "$RELEASE/checksums.txt" "$d/checksums.txt" 2>/dev/null &&
      fetch "$RELEASE/install-panel.sh" "$d/install-panel.sh" 2>/dev/null; then
      if want="$(awk '$2 == "install-panel.sh" || $2 == "*install-panel.sh" {print $1}' "$d/checksums.txt")" &&
        got="$(sha256sum "$d/install-panel.sh" | awk '{print $1}')" && [ -n "$want" ] && [ "$want" = "$got" ]; then
        src="$d/install-panel.sh"
      else
        warn "管理脚本校验失败，未安装 polaris 命令"
      fi
    else
      warn "无法下载 $VERSION 的管理脚本，未安装 polaris 命令"
    fi
    if [ -z "$src" ]; then
      rm -rf "$d"
      return 0 # Old/offline releases may lack the script; do not register a dangling alias.
    fi
  fi
  if [ -n "$src" ]; then
    install -m 0755 "$src" "$CTL" || { rc=$?; [ -z "$d" ] || rm -rf "$d"; return "$rc"; }
  fi
  ln -sf "$CTL" "$CTL_ALIAS" || rc=$?
  [ -z "$d" ] || rm -rf "$d"
  return "$rc"
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
    set-path|set-domain|set-port|set-url|set-sub-domain) ;;
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
    set-sub-domain) ( set -e; do_set_sub_domain "$arg" ) >"$log" 2>&1 ;;
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
port_msg() { local o; o="$(port_owner "$1")"; echo "端口 $1 已被占用${o:+（${o}）}"; }
valid_domain() { [[ "$1" =~ ^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)+$ ]]; }

# proxied_to_panel DOMAIN PORT: does https://DOMAIN reach this panel? The same
# unknown subscription path is fetched through the domain and from the panel
# directly; identical answers mean the existing proxy forwards to the panel.
# Sets PROXY_PROBE to the reason when it does not.
proxied_to_panel() {
  local path a b
  path="/sub/polaris-probe-$RANDOM$RANDOM"
  b="$(curl -s -m 5 -w '|%{http_code}' "http://127.0.0.1:$2$path" 2>/dev/null)" || b=""
  a="$(curl -s -m 10 -w '|%{http_code}' "https://$1$path" 2>/dev/null)" || {
    PROXY_PROBE="https://$1 无法访问：检查 DNS 解析和 HTTPS 证书"; return 1; }
  [ -n "$b" ] && [ "$a" = "$b" ] && return 0
  PROXY_PROBE="https://$1 能访问，但返回的不是面板（HTTP ${a##*|}）：检查反代目标是否为 http://127.0.0.1:$2"
  return 1
}

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
  kv 版本 "$(panel_version) $(edition_label "$(installed_edition)")"
  kv 面板 "$(svc_line polaris)"
  if systemctl is-enabled --quiet polaris 2>/dev/null; then kv 开机自启 "是"; else kv 开机自启 "${C_Y}否${C_0}"; fi
  systemctl cat polaris-agent >/dev/null 2>&1 && kv 本机节点 "$(svc_line polaris-agent)"
  if [ -n "$domain" ] && [ -f "$CADDYFILE" ] && grep -q "^$domain {" "$CADDYFILE"; then
    if ! systemctl is-active --quiet caddy && port_busy 443; then
      # Another web server (nginx / OpenResty …) serves HTTPS: Caddy is not needed.
      kv Caddy "$(dot) ${C_D}未运行（443 由其他反代占用；不用 Caddy 可执行 systemctl disable caddy）${C_0}"
    else
      kv Caddy "$(svc_line caddy)"
    fi
  fi

  section "访问"
  kv 后台地址 "${C_BD}$url${path%/}/${C_0}"
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
  if [ -n "$(env_get POLARIS_DB_DSN)" ]; then kv 数据库 "$(db_label "$(env_get POLARIS_DB_DSN)")"
  elif [ -n "$db" ]; then size="$(human_bytes "$(stat -c %s "$db")")"; kv 数据库 "${db}（${size}）"; fi
  [ -z "$(env_get POLARIS_REDIS_URL)" ] || kv Redis "$(env_get POLARIS_REDIS_URL | sed -E 's#//[^@]*@#//#')"
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

  choose_edition
  choose_database
  local use_caddy=0
  [ -n "$DOMAIN" ] && [ "$CADDY" = 1 ] && use_caddy=1
  SECONDS=0 STEP_N=0 STEP_TOTAL=$((3 + 2 * use_caddy))
  [ -n "$VERSION" ] || STEP_TOTAL=$((STEP_TOTAL + 1))
  banner "安装面板"
  kv 系统 "$(sys_line)"
  kv 访问方式 "$(if [ -n "$DOMAIN" ]; then echo "https://$DOMAIN$([ "$use_caddy" = 1 ] && echo '（自动安装 Caddy 申请证书）' || echo '（自备反向代理）')"; else echo "IP + HTTP"; fi)"
  kv 面板端口 "$PORT"
  kv 节点接入 "$AGENT_PORT"
  kv 版本 "${VERSION:-最新} $(edition_label "${EDITION:-community}")"
  kv 数据库 "$(case "$DB_CHOICE" in local) echo "PostgreSQL（本机，自动安装）" ;; sqlite) echo SQLite ;; *) db_label "$DB_DSN" ;; esac)$([ -n "$REDIS_URL$( [ "$REDIS_CHOICE" = local ] && echo 1)" ] && echo " + Redis")"
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
  # A join reuses the primary's secret key (SECRET_OVERRIDE): generating a
  # fresh one would seal the CA with a key the primary does not have.
  secret="${SECRET_OVERRIDE:-$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')}"
  if [ -n "$DOMAIN" ]; then
    listen="127.0.0.1:$PORT"; public_url="https://$DOMAIN"
  else
    listen=":$PORT"; public_url="http://$host:$PORT"
  fi

  step "创建用户与目录"
  id polaris >/dev/null 2>&1 || useradd --system --home-dir "$DATA" --shell /usr/sbin/nologin polaris
  install -d -m 0750 -o root -g polaris "$ETC"
  install_files
  # A join installs onto the primary's existing database, which already has
  # an admin: the fresh-database check must not run (fresh=0 only checks
  # connectivity and encoding).
  local fresh=1
  [ "$HA_JOIN" = 1 ] && fresh=0
  case "$DB_CHOICE" in
    local) setup_local_pg; check_pg_target "$DB_DSN" "$fresh" ;;
    sqlite) ;;
    *) check_pg_target "$DB_DSN" "$fresh" ;;
  esac
  [ "$REDIS_CHOICE" = local ] && setup_local_redis
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
  [ -z "$DB_DSN" ] || env_set POLARIS_DB_DSN "$DB_DSN"
  [ -z "$REDIS_URL" ] || env_set POLARIS_REDIS_URL "$REDIS_URL"
  chmod 0640 "$ENV_FILE"; chown root:polaris "$ENV_FILE"
  install_unit
  db_dropin

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
    # A join reuses the primary's admin, so there is nothing to print.
    [ "$HA_JOIN" = 1 ] || print_credentials "$pass" # after the log wait_active printed, so it stays in view
    if journalctl -u polaris -n 40 --no-pager 2>/dev/null | grep -q 'address already in use'; then
      die "Polaris 启动失败：端口被占用。可执行 polaris set-port <新端口> 修改后会自动重启"
    fi
    die "Polaris 启动失败（修正后执行 polaris restart）"
  fi

  open_port "$AGENT_PORT"
  if [ "$use_caddy" = 1 ]; then open_port 80; open_port 443; elif [ -z "$DOMAIN" ]; then open_port "$PORT"; fi

  echo
  ok "${C_BD}Polaris $VERSION 安装完成${C_0}（用时 $(human_dur "$SECONDS")）"
  [ "$HA_JOIN" = 1 ] || print_credentials "$pass"
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
  if [ -n "$RESTORE_FILE" ]; then
    section "从备份恢复"
    do_restore "$RESTORE_FILE"
    warn "已恢复备份：上面显示的初始管理员不再有效，请用备份里的管理员登录"
  fi
}

do_update() {
  migrate_layout
  # Config is what matters: after "uninstall" without --purge the binary and
  # unit are gone but config and data remain, and update restores them.
  [ -f "$ENV_FILE" ] || die "Polaris 尚未安装（未找到 ${ENV_FILE}）"
  local cur backup=""
  cur="$(panel_version)"
  SECONDS=0 STEP_N=0 STEP_TOTAL=3
  [ -n "$VERSION" ] || STEP_TOTAL=4
  banner "升级面板"
  resolve_version
  resolve_edition
  if [ -n "$cur" ] && [ "$cur" = "$VERSION" ] && [ "$EDITION" = "$(installed_edition)" ] && [ "$FORCE" = 0 ] && [ -f "$UNIT" ]; then
    ok "已是最新版本 ${cur}（加 --force 可重新安装）"
    # A tool older than the updater replaced itself during the update but
    # never set it up: do it now.
    if [ ! -f "$UPD_PATH" ]; then install_updater && ok "已安装在线升级 / 配置组件"; fi
    update_self
    return 0
  fi
  kv 当前版本 "${cur:-未安装}${cur:+ $(edition_label "$(installed_edition)")}"
  kv 目标版本 "${C_BD}$VERSION $(edition_label "$EDITION")${C_0}"
  kv 更新说明 "https://github.com/$REPO/releases/tag/$VERSION"
  echo
  id polaris >/dev/null 2>&1 || useradd --system --home-dir "$DATA" --shell /usr/sbin/nologin polaris
  download_release
  # Trial run: the new build must start on this machine before anything changes.
  if ! "$TMP/polaris" --version 2>/dev/null | grep -qF "$VERSION"; then
    die "新版本无法在本机运行（试运行失败：$("$TMP/polaris" --version 2>&1 | head -n1)），未做任何改动"
  fi
  ok "试运行通过：$("$TMP/polaris" --version 2>/dev/null)"
  step "备份数据库并替换程序（配置与数据保持不变）"
  if [ -n "$(env_get POLARIS_DB_DSN)" ]; then
    : # PostgreSQL: a logical backup with the current build, below
  elif ls "$DATA"/*.db >/dev/null 2>&1; then
    backup="$DATA/backup-$(date +%Y%m%d%H%M%S)"
  fi
  systemctl stop polaris 2>/dev/null || true
  if [ -n "$backup" ]; then
    mkdir -p "$backup" && cp -a "$DATA"/*.db* "$backup"/ && ok "数据库已备份到 $backup"
  elif [ -n "$(env_get POLARIS_DB_DSN)" ]; then
    backup="$DATA/backup-$(date +%Y%m%d%H%M%S).polaris"
    if polaris_cmd backup "$backup" >/dev/null 2>&1; then ok "数据库已备份到 $backup"
    else warn "PostgreSQL 数据库备份失败，继续升级（建议先用 polaris backup 手动备份）"; fi
  fi
  local prev=""
  if [ -x "$BIN" ]; then prev="$BIN.prev"; cp -a "$BIN" "$prev"; fi
  install_files
  install_unit
  ok "程序已替换"
  step "启动 Polaris"
  systemctl daemon-reload
  systemctl enable polaris >/dev/null 2>&1 || true
  systemctl start polaris
  if ! wait_healthy; then
    [ -n "$prev" ] || die "启动失败${backup:+，升级前的数据库备份在 $backup}"
    warn "新版本启动后未能正常响应，自动回滚到 ${cur:-旧版本}"
    systemctl stop polaris 2>/dev/null || true
    install -m 0755 "$prev" "$BIN"
    systemctl start polaris
    if wait_healthy; then
      die "已自动回滚到 ${cur:-旧版本}，面板正常运行；新版本的启动日志见上方${backup:+（升级前的数据库备份在 ${backup}）}"
    fi
    die "回滚后仍无法启动${backup:+；升级前的数据库备份在 $backup}，请执行 polaris log 查看"
  fi
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
    warn "有新版本：${latest}，执行 polaris update 升级"
    dim "  更新说明：https://github.com/$REPO/releases/tag/$latest"
  fi
}

do_uninstall() {
  step "停止并移除 polaris 服务"
  systemctl disable --now polaris 2>/dev/null || true
  systemctl disable --now polaris-update.path 2>/dev/null || true
  rm -f "$UNIT" "$BIN" "$UPD_SVC" "$UPD_PATH" "$DB_DROPIN"
  systemctl daemon-reload
  if [ "$PURGE" = 1 ]; then
    warn "删除配置与数据：$ETC $DATA"
    if is_local_pg "$(env_get POLARIS_DB_DSN)" && command -v psql >/dev/null; then
      pg_su -c "DROP DATABASE IF EXISTS polaris" 2>/dev/null && pg_su -c "DROP ROLE IF EXISTS polaris" 2>/dev/null \
        && warn "已删除本机 PostgreSQL 数据库 polaris（PostgreSQL 本身保留）"
    elif [ -n "$(env_get POLARIS_DB_DSN)" ]; then
      warn "外部数据库 $(db_label "$(env_get POLARIS_DB_DSN)") 中的数据未删除"
    fi
    rm -rf "$ETC" "$DATA"
    rm -f "$DB_DROPIN"
    id polaris >/dev/null 2>&1 && userdel polaris 2>/dev/null || true
    rm -f "$CTL" "$CTL_ALIAS"
  else
    dim "  已保留配置 $ETC 与数据 ${DATA}（加 --purge 一并删除）"
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
    "$(pad 地址 8)${C_BD}$(admin_url)${C_0}" \
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
  # Another web server on 80/443 (nginx, OpenResty, a control panel) would keep
  # Caddy from starting and leave the panel unreachable: stop before changing anything.
  local p owner
  for p in 80 443; do
    if port_busy "$p" && ! systemctl is-active --quiet caddy; then
      owner="$(port_owner "$p")"
      die "端口 $p 已被 ${owner:-其他程序} 占用，无法使用 Caddy。请在 ${owner:-该 Web 服务} 里把 $domain 反向代理到 127.0.0.1:${port}，再执行 polaris set-url https://${domain}（面板配置未做任何修改）"
    fi
  done
  install_caddy
  step "配置 Caddy 站点 $domain"
  caddy_site "$domain" "$port"
  systemctl is-active --quiet caddy || die "Caddy 未能启动（journalctl -u caddy 查看原因），面板配置未做修改"
  env_set POLARIS_PUBLIC_URL "https://$domain"
  env_set POLARIS_LISTEN "127.0.0.1:$port"
  hosts="$(env_get POLARIS_AGENT_HOSTS)"
  # Keep the old host so already-enrolled nodes still match the server certificate.
  case ",$hosts," in *",$domain,"*) ;; *) env_set POLARIS_AGENT_HOSTS "$domain${hosts:+,$hosts}" ;; esac
  systemctl restart polaris && wait_active || die "重启失败"
  open_port 80; open_port 443
  ok "已切换到 ${C_BD}$(admin_url)${C_0}"
  warn "订阅地址已变为 https://$domain/…，旧的 http 订阅链接失效，用户需要重新导入；可在防火墙关闭 $port/tcp，放行 80、443"
}

# do_set_sub_domain DOMAIN: a separate HTTPS domain for subscription links.
# Caddy proxies it to the panel like the admin domain; the panel serves only
# subscriptions and the user center there. The admin address is unchanged.
do_set_sub_domain() {
  need_installed
  local domain="$1" port p owner
  valid_domain "$domain" || die "域名格式不正确：$domain"
  port="$(env_get POLARIS_LISTEN)"; port="${port##*:}"
  for p in 80 443; do
    if port_busy "$p" && ! systemctl is-active --quiet caddy; then
      owner="$(port_owner "$p")"
      # Another web server owns the port. If it already proxies the domain
      # to the panel, there is nothing for Caddy to do: accept the domain.
      if proxied_to_panel "$domain" "$port"; then
        ok "https://$domain 已由 ${owner:-现有 Web 服务} 反代到面板，无需 Caddy"
        dim "  订阅链接改用 https://${domain}；该域名只开放订阅和用户中心"
        return 0
      fi
      die "端口 $p 已被 ${owner:-其他程序} 占用，无法使用 Caddy；且 https://$domain 还没有反代到面板（${PROXY_PROBE:-无法访问}）。请在 ${owner:-该 Web 服务} 里把 $domain 反向代理到 http://127.0.0.1:$port 并配置 HTTPS 证书，然后再点一次「配置」，或直接到后台「订阅设置」填写订阅域名 https://$domain"
    fi
  done
  install_caddy
  step "配置 Caddy 站点 $domain"
  caddy_site "$domain" "$port"
  systemctl is-active --quiet caddy || die "Caddy 未能启动（journalctl -u caddy 查看原因）"
  open_port 80; open_port 443
  ok "订阅域名 ${C_BD}https://$domain${C_0} 已就绪（证书由 Caddy 自动签发，首次访问可能需要几秒）"
  dim "  后台「订阅设置 → 订阅域名」填 https://$domain 后，订阅链接改用这个域名；该域名只开放订阅和用户中心"
}

do_backup() {
  need_installed
  local ts out file
  ts="$(date +%Y%m%d-%H%M%S)"
  if [ -z "$BK_PASS" ] && [ -t 0 ]; then
    read -r -s -p "备份口令（加密后可恢复到任何服务器；留空不加密）：" BK_PASS; echo
  fi
  file="$DATA/polaris-$ts.tar.gz"; [ -n "$BK_PASS" ] && file="$file.enc"
  out="/root/$(basename "$file")"
  POLARIS_BACKUP_PASSPHRASE="$BK_PASS" polaris_cmd backup "$file" >/dev/null || die "备份失败"
  mv -f "$file" "$out"
  chmod 600 "$out"
  ok "已备份到 ${out}（$(human_bytes "$(stat -c %s "$out")")）"
  if [ -n "$BK_PASS" ]; then
    dim "  已加密并附带密钥：新服务器上安装后执行 polaris restore $out 即可（需要口令）"
  else
    warn "未加密：只能恢复到使用同一密钥（$ENV_FILE 中的 POLARIS_SECRET_KEY）的面板"
  fi
}

# do_restore replaces the panel's data with a backup (this server's or
# another's): the panel stops, the data is swapped (a snapshot of the current
# data is kept in $DATA/backups) and the panel starts again.
do_restore() {
  need_installed
  local src="$1" tmp
  [ -f "$src" ] || die "找不到备份文件：$src"
  if [ -z "$BK_PASS" ] && [ "$(head -c 8 "$src")" = "PLRSENC1" ]; then
    [ -t 0 ] || die "这是加密备份，请加 --passphrase 口令"
    read -r -s -p "备份口令：" BK_PASS; echo
  fi
  tmp="$DATA/restore-upload-$$"
  install -m 600 -o polaris -g polaris "$src" "$tmp"
  dim "  停止面板并恢复数据…"
  systemctl stop polaris 2>/dev/null || true
  if ! POLARIS_BACKUP_PASSPHRASE="$BK_PASS" polaris_cmd restore "$tmp"; then
    rm -f "$tmp"
    systemctl start polaris 2>/dev/null || true
    die "恢复失败，原数据未改动"
  fi
  rm -f "$tmp"
  systemctl start polaris && wait_active || die "面板启动失败（polaris log 查看）"
  ok "已恢复：管理员账号、节点与用户均来自备份；已接入的节点无需重装"
  dim "  恢复前的数据已另存在 $DATA/backups（恢复前快照）"
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
    code="$(curl -s -o /dev/null -m 5 -w '%{http_code}' "http://127.0.0.1:$port${path%/}/" 2>/dev/null || true)"; code="${code#000}"
    case "$code" in 2??|3??) chk_ok "面板端口 $port 响应正常（HTTP ${code}）" ;; *) chk_bad "面板端口 $port 无正常响应（HTTP ${code:-无}）" ;; esac
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
    elif [ -n "$ip" ] && [ "$resolved" != "$ip" ]; then chk_warn "$domain 解析到 ${resolved}，本机公网 IP 为 ${ip}（使用 CDN 时可忽略）"
    else chk_ok "$domain 解析到 ${resolved}"; fi
    if [ -f "$CADDYFILE" ] && grep -q "^$domain {" "$CADDYFILE"; then
      systemctl is-active --quiet caddy && chk_ok "Caddy 运行中" || chk_bad "Caddy 未运行：systemctl status caddy"
    fi
    code="$(curl -s -o /dev/null -m 8 -w '%{http_code}' "https://$domain${path%/}/" 2>/dev/null || true)"; code="${code#000}"
    case "$code" in 2??|3??) chk_ok "https://$domain 可访问（HTTP ${code}）" ;; *) chk_bad "https://$domain 访问失败（HTTP ${code:-无}），证书可能尚未签发或 80/443 未放行" ;; esac
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

# --- high availability -----------------------------------------------------

# One-click HA (PR2): ha-init turns this machine into the primary, ha-join
# turns a fresh machine into the standby, ha-status prints the local HA
# state. The manual runbook is docs/design/ha-one-click-deployment.md §6 and
# the hand-written runbook it mirrors; the --dry-run output prints the same
# steps in the same order, so the two can be checked against each other.
#
# HA only supports Debian/Ubuntu (apt): the standby may need the same
# PostgreSQL major version, installed from the PGDG repository when the
# distro's default differs.
HA_WG_PRIMARY_IP="10.8.0.1"
HA_WG_SECONDARY_IP="10.8.0.2"
HA_WG_PORT=51820
# HA_SYNC_APPNAME must match haSyncAppName in internal/panel/api_ha.go: the
# primary writes synchronous_standby_names from that Go constant and the
# standby registers itself under this name here. tests/test-ha-dryrun.sh
# asserts they are equal.
HA_SYNC_APPNAME=polaris_standby
HA_SYNCWG_UNIT=/etc/systemd/system/polaris-ha-sync-wg.service
HA_SYNCWG_TIMER=/etc/systemd/system/polaris-ha-sync.timer
HA_WATCHDOG=/usr/local/bin/cf-failover.sh
HA_WATCHDOG_ENV=/etc/cf-failover.env
HA_HB_UNIT=/etc/systemd/system/polaris-ha-heartbeat.service
HA_HB_TIMER=/etc/systemd/system/polaris-ha-heartbeat.timer
HA_HB_ENV=/etc/polaris/witness.env
HA_FENCE_SCRIPT=/usr/local/bin/polaris-ha-fence.sh
HA_FENCE_UNIT=/etc/systemd/system/polaris-ha-fence.service
HA_FENCE_DROPIN=/etc/systemd/system/polaris.service.d/ha-fence.conf
HA_WITNESS_UNIT=/etc/systemd/system/polaris-witness.service
HA_WITNESS_STATE=/var/lib/polaris-witness/state.json

# ha_require_apt: HA is Debian/Ubuntu only (PGDG, wireguard, redis-server
# and nginx all come from apt here).
ha_require_apt() {
  command -v apt-get >/dev/null || die "高可用只支持 Debian/Ubuntu（需要 apt）"
}

# ha_plan prints a planned action in dry-run mode. Secrets are never passed
# here: callers write the masked form (********) themselves.
ha_plan() { echo "    计划：$*"; }

# ha_conf_set FILE KEY VALUE SEP: set one key in a config file (SEP is "="
# for postgresql.conf, " " for redis.conf). An existing line — commented or
# not — is replaced in place, otherwise the key is appended.
ha_conf_set() {
  local f="$1" k="$2" v="$3" sep="$4"
  if grep -qE "^[# ]*${k}[[:space:]]*${sep}" "$f"; then
    sed -i -E "s|^[# ]*${k}[[:space:]]*${sep}.*|${k}${sep}${v}|" "$f"
  else
    printf '%s%s%s\n' "$k" "$sep" "$v" >> "$f"
  fi
}

# ha_pg_conf_file prints the running cluster's postgresql.conf path ("" when
# no cluster answers; never fails, so callers under set -e stay alive).
ha_pg_conf_file() {
  local out
  out="$(pg_su -tAc 'SHOW config_file' 2>/dev/null | tr -d '[:space:]')" || out=""
  printf '%s' "$out"
}

# ha_pg_major prints the running cluster's PostgreSQL major version (e.g.
# 15), or "" when no cluster answers.
ha_pg_major() {
  local v
  v="$(pg_su -tAc 'SHOW server_version' 2>/dev/null | tr -d '[:space:]')" || v=""
  if [ -z "$v" ]; then
    v="$(psql --version 2>/dev/null | awk '{print $3}')" || v=""
  fi
  printf '%s' "${v%%.*}"
}

# ha_pg_data_dir prints the running cluster's data directory ("" when no
# cluster answers).
ha_pg_data_dir() {
  local out
  out="$(pg_su -tAc 'SHOW data_directory' 2>/dev/null | tr -d '[:space:]')" || out=""
  printf '%s' "$out"
}

# ha_pg_port prints the TCP port of the cluster pg_su reaches (the panel's
# database). Debian moves a cluster off 5432 when that port is taken, so the
# DSN must use the real port, not a constant (5432 may be another instance
# entirely). Falls back to 5432 when no cluster answers (dry-run).
ha_pg_port() {
  local p
  p="$(pg_su -tAc 'SHOW port' 2>/dev/null | tr -d '[:space:]')" || p=""
  case "$p" in ''|*[!0-9]*) p=5432 ;; esac
  printf '%s' "$p"
}

# ha_dsn_port DSN: the TCP port in a postgres:// DSN (host:port), or 5432
# when the DSN carries no port. IPv4 hosts only; the HA bundle uses them.
ha_dsn_port() {
  local port
  port="$(printf '%s' "$1" | sed -nE 's|^[a-z]+://[^/@]*@[^:/]+:([0-9]+)/.*$|\1|p')" || port=""
  case "$port" in ''|*[!0-9]*) port=5432 ;; esac
  printf '%s' "$port"
}

# ha_pg_cluster_for_ver VER prints "VER/name" of the first local cluster of
# that PostgreSQL major version (from pg_lsclusters), or "" when none.
ha_pg_cluster_for_ver() {
  pg_lsclusters --no-header 2>/dev/null | awk -v v="$1" '$1==v {print $1"/"$2; exit}'
}

# ha_pg_cluster_datadir CLUSTER prints the data directory of one Debian
# cluster ("VER/name"), or "" when it is not listed.
ha_pg_cluster_datadir() {
  pg_lsclusters --no-header 2>/dev/null | awk -v c="$1" '$1"/"$2==c {print $6; exit}'
}

# pg_cluster_su CLUSTER [PSQL-ARGS...]: psql as postgres against one Debian
# cluster ("15/main"). Unlike pg_su, this does not depend on which cluster
# happens to sit on the default port.
pg_cluster_su() { local c="$1"; shift; (cd / && runuser -u postgres -- psql --cluster "$c" -X -q -v ON_ERROR_STOP=1 "$@"); }

# ha_replica_su [PSQL-ARGS...]: psql against the replica cluster
# (HA_PG_CLUSTER, set by ha_setup_pg_replica), or the pg_su default when
# the join did not set one.
ha_replica_su() {
  if [ -n "${HA_PG_CLUSTER:-}" ]; then pg_cluster_su "$HA_PG_CLUSTER" "$@"; else pg_su "$@"; fi
}

# ha_replica_is_standby: true when the replica cluster answers and reports
# pg_is_in_recovery() = t. A standby.signal on disk is not enough: it
# survives an interrupted basebackup or a cluster that refused to start, so
# only an answering standby means an earlier join actually finished.
ha_replica_is_standby() {
  [ "$(ha_replica_su -tAc 'SELECT pg_is_in_recovery()' 2>/dev/null)" = t ]
}

# ha_ensure_locale COLLATE: make sure the standby's operating system knows
# the primary's collation before pg_basebackup copies it over.
#
# A basebackup is a byte-for-byte copy of the primary's cluster, so the
# standby inherits its LC_COLLATE. PostgreSQL calls setlocale() with that
# value at startup and refuses to run when the OS does not provide it:
#   FATAL: database locale is incompatible with operating system
# The standby is a different machine, so "it works on the primary" proves
# nothing. Ubuntu ships C, C.UTF-8 and POSIX only until locales is
# configured, which is exactly the failure seen in the field with
# en_US.UTF-8. Generate the locale (or die pointing at the manual command)
# before the copy, not after, so a long basebackup does not end in a
# cluster that cannot start.
ha_ensure_locale() {
  local want="$1" norm
  [ -n "$want" ] || return 0
  if [ "$HA_DRYRUN" = 1 ]; then
    ha_plan "确认本机 locale 包含 ${want}（缺了就 locale-gen；主库的 collation 会被 pg_basebackup 带过来，系统不认识会导致从库起不来）"
    return 0
  fi
  # "en_US.UTF-8" and "en_US.utf8" are the same locale; locale -a prints
  # the normalised lowercase form.
  norm="$(printf '%s' "$want" | tr 'A-Z' 'a-z' | tr -d '-')"
  if locale -a 2>/dev/null | tr 'A-Z' 'a-z' | tr -d '-' | grep -qx "$norm"; then
    ok "本机 locale 支持主库的 $want"
    return 0
  fi
  warn "本机缺少 locale ${want}（主库的 collation）：不装上从库会以「database locale is incompatible with operating system」启动失败"
  # "en_US.UTF-8" -> "en_US" for the /etc/locale.gen line.
  local base="${want%%.*}"
  if [ "$base" != "$want" ]; then
    if command -v locale-gen >/dev/null; then
      if [ -f /etc/locale.gen ] && ! grep -qE "^ *${base}\.UTF-8 UTF-8" /etc/locale.gen; then
        printf '%s.UTF-8 UTF-8\n' "$base" >> /etc/locale.gen
      fi
      locale-gen >/dev/null 2>&1 || true
      update-locale >/dev/null 2>&1 || true
      if locale -a 2>/dev/null | tr 'A-Z' 'a-z' | tr -d '-' | grep -qx "$norm"; then
        ok "已生成 locale $want"
        return 0
      fi
    fi
  fi
  die "无法自动生成 locale ${want}：请在备机执行「apt-get install -y locales && sed -i 's/^# *${base}\.UTF-8 UTF-8/${base}.UTF-8 UTF-8/' /etc/locale.gen && locale-gen」，或在主库上把它改成从库已有的 collation 后重建备机"
}

# ha_url_password URL: the password in redis://:pass@host/... ("" when the
# URL carries no password).
ha_url_password() {
  case "$1" in *://:*@*) local u="${1#*://:}"; printf '%s' "${u%%@*}" ;; *) printf '' ;; esac
}

# ha_url_host URL: the host in any scheme://[user:pass@]host[:port]/... URL.
ha_url_host() {
  local u="${1#*://}"
  u="${u#*@}"
  printf '%s' "${u%%[:/]*}"
}

# ha_witness_url prints WITNESS_URL without a trailing slash, or nothing
# when it is unset. The witness is optional: callers treat empty as "no
# witness configured" and keep the old behaviour. A malformed URL is not
# validated here — it fails safe at runtime (the heartbeat misses, the
# watchdog treats the witness as unreachable and never flips, the fence
# lets the panel start), and the dry-run plan prints the URL for eyeballing.
ha_witness_url() {
  local u="${WITNESS_URL:-}"
  printf '%s' "${u%/}"
}

# ha_cli_supports CMD: true when the installed panel binary knows the
# maintenance subcommand (its usage text is compiled into the binary).
# ha-init needs ha-token / ha-seal-secret / "ha sync-wg", which only exist
# in releases containing PR1+PR2; older binaries would silently start a
# second panel server instead, so never call them blindly.
ha_cli_supports() {
  [ -x "$BIN" ] && grep -qaF "polaris $1" "$BIN"
}

# ha_cf_ids TOKEN DOMAIN: resolve the Cloudflare zone id and the polaris A
# record id; sets CF_ZONE_ID / CF_RECORD_ID. Returns 1 on failure.
ha_cf_ids() {
  local token="$1" domain="$2" zone="${2#*.}" zj rj
  command -v python3 >/dev/null || return 1
  zj="$(curl -fsS -H "Authorization: Bearer $token" --max-time 10 "https://api.cloudflare.com/client/v4/zones?name=$zone" 2>/dev/null)" || return 1
  CF_ZONE_ID="$(printf '%s' "$zj" | python3 -c "import sys,json;print(json.load(sys.stdin)['result'][0]['id'])" 2>/dev/null)" || return 1
  [ -n "$CF_ZONE_ID" ] || return 1
  rj="$(curl -fsS -H "Authorization: Bearer $token" --max-time 10 "https://api.cloudflare.com/client/v4/zones/$CF_ZONE_ID/dns_records?name=$domain&type=A" 2>/dev/null)" || return 1
  CF_RECORD_ID="$(printf '%s' "$rj" | python3 -c "import sys,json;print(json.load(sys.stdin)['result'][0]['id'])" 2>/dev/null)" || return 1
  [ -n "$CF_RECORD_ID" ] || return 1
}

# ha_check_dns: the polaris A record should be TTL <= 60 and DNS-only
# (proxied=false); returns 1 when it is not. Only called when a CF token
# is available; without one ha-init just warns.
ha_check_dns() {
  local domain zone rec ttl proxied
  domain="$(panel_domain)"
  [ -n "$domain" ] || { warn "未配域名：跳过 DNS 检查"; return 0; }
  command -v python3 >/dev/null || { warn "未装 python3：跳过 DNS 检查"; return 0; }
  ha_cf_ids "${CF_API_TOKEN:-}" "$domain" || { warn "Cloudflare API 查不到 $domain 的记录：跳过 DNS 检查"; return 0; }
  zone="$CF_ZONE_ID"
  rec="$(curl -fsS -H "Authorization: Bearer ${CF_API_TOKEN:-}" --max-time 10 "https://api.cloudflare.com/client/v4/zones/$zone/dns_records/$CF_RECORD_ID" 2>/dev/null)" || return 1
  ttl="$(printf '%s' "$rec" | python3 -c "import sys,json;print(json.load(sys.stdin)['result']['ttl'])" 2>/dev/null)" || ttl=""
  proxied="$(printf '%s' "$rec" | python3 -c "import sys,json;print(json.load(sys.stdin)['result']['proxied'])" 2>/dev/null)" || proxied=""
  if [ "$ttl" != 1 ] && [ -n "$ttl" ] && [ "$ttl" -gt 60 ] 2>/dev/null; then
    warn "polaris A 记录 TTL 是 ${ttl}（应 ≤ 60，否则切换后生效慢）"
  fi
  if [ "$proxied" = True ]; then
    warn "polaris A 记录是橙云代理（应为 DNS-only，否则 9443 不通）"
    return 1
  fi
  return 0
}

# ha_wg_keypair generates the local WireGuard keypair when missing and sets
# WG_PUB to the public key (a placeholder in dry-run). Nothing is touched
# on disk in dry-run.
ha_wg_keypair() {
  if [ "$HA_DRYRUN" = 1 ]; then
    ha_plan "生成 WireGuard 密钥对（/etc/wireguard/privatekey，0600；公钥写入 publickey）"
    WG_PUB="<本机WG公钥>"
    return 0
  fi
  install -d -m 0700 /etc/wireguard
  if [ ! -f /etc/wireguard/privatekey ]; then
    umask 077
    wg genkey | tee /etc/wireguard/privatekey | wg pubkey > /etc/wireguard/publickey
    chmod 0600 /etc/wireguard/privatekey /etc/wireguard/publickey
  fi
  WG_PUB="$(cat /etc/wireguard/publickey)"
}

# ha_wg_write_conf MY_IP [PEER_PUB PEER_IP PEER_ENDPOINT]: write wg0.conf.
# The primary has no peer yet (the standby registers later and sync-wg adds
# it), so the peer block is omitted when no peer pubkey is given.
ha_wg_write_conf() {
  local my_ip="$1" peer_pub="${2:-}" peer_ip="${3:-}" peer_ep="${4:-}"
  if [ "$HA_DRYRUN" = 1 ]; then
    if [ -n "$peer_pub" ]; then
      ha_plan "写 /etc/wireguard/wg0.conf（本机 $my_ip/24，端口 ${HA_WG_PORT}；对端 $peer_ip/32，endpoint $peer_ep:${HA_WG_PORT}）"
    else
      ha_plan "写 /etc/wireguard/wg0.conf（本机 $my_ip/24，端口 ${HA_WG_PORT}；对端待加入后由 sync-wg 补上）"
    fi
    return 0
  fi
  {
    printf '[Interface]\nPrivateKey = %s\nAddress = %s/24\nListenPort = %s\n' "$(cat /etc/wireguard/privatekey)" "$my_ip" "$HA_WG_PORT"
    if [ -n "$peer_pub" ]; then
      printf '\n[Peer]\nPublicKey = %s\nAllowedIPs = %s/32\nEndpoint = %s:%s\nPersistentKeepalive = 25\n' "$peer_pub" "$peer_ip" "$peer_ep" "$HA_WG_PORT"
    fi
  } > /etc/wireguard/wg0.conf
  chmod 0600 /etc/wireguard/wg0.conf
}

# ha_wg_start enables and starts the tunnel; dies unless wg0 answers.
ha_wg_start() {
  if [ "$HA_DRYRUN" = 1 ]; then
    ha_plan "systemctl enable --now wg-quick@wg0，wg show 应列出 wg0"
    return 0
  fi
  # A stale wg0 can linger from an earlier ha-join attempt: the interface
  # name exists (so wg-quick refuses to create it again, "already exists")
  # but it is not a working WireGuard interface (so `wg show` sees nothing).
  # Remove that stale interface before starting fresh; a working interface
  # is never touched because `wg show` succeeds on it.
  if ! wg show wg0 >/dev/null 2>&1 && ip link show wg0 >/dev/null 2>&1; then
    ip link delete wg0 >/dev/null 2>&1 || true
  fi
  # wg-quick refuses to start an interface that is already up (a re-run of
  # ha-join after an earlier attempt), and a bare `systemctl enable --now`
  # failing under `set -e` kills the whole script without any message, so
  # the step looked stuck with no error. Only start when the interface is
  # down, tolerate the start failing, and let the explicit check below
  # report the failure with a clear message.
  if ! wg show wg0 >/dev/null 2>&1; then
    systemctl enable --now wg-quick@wg0 >/dev/null 2>&1 || true
  fi
  wg show wg0 >/dev/null 2>&1 || die "WireGuard 未能启动（wg show wg0 无输出）"
  ok "隧道已启动（wg0）"
}

# ha_wg_ping PEER_IP: the tunnel must carry traffic before anything else is
# built on it.
ha_wg_ping() {
  local peer_ip="$1" i
  if [ "$HA_DRYRUN" = 1 ]; then
    ha_plan "ping -c 3 $peer_ip 应通（不通就停下，先修 51820/udp 防火墙）"
    return 0
  fi
  for i in $(seq 1 10); do
    ping -c 1 -W 2 "$peer_ip" >/dev/null 2>&1 && { ok "隧道通：ping $peer_ip 成功"; return 0; }
    sleep 1
  done
  die "隧道不通：ping $peer_ip 失败，请检查 51820/udp 防火墙与对端公钥"
}

# ha_seal NAME VALUE: seal a secret into cluster_secrets through the panel
# CLI. The value travels over stdin, never an argument list, and is never
# printed; in dry-run only the masked plan line appears.
ha_seal() {
  local name="$1" value="$2"
  if [ "$HA_DRYRUN" = 1 ]; then
    ha_plan "密封保存 $name 到 cluster_secrets（不打印明文）"
    return 0
  fi
  printf '%s' "$value" | polaris_cmd ha-seal-secret "$name" >/dev/null || die "密封 $name 失败"
  ok "已密封保存 $name"
}

# ha_watchdog_script prints the failover watchdog (scripts/cf-failover.sh).
# It is embedded here instead of downloaded so ha-join works from the
# installer alone; scripts/test-ha-dryrun.sh asserts the embedded copy is
# byte-identical to scripts/cf-failover.sh.
ha_watchdog_script() {
  cat <<'WATCHDOG'
#!/bin/bash
# cf-failover.sh — Cloudflare DNS failover watchdog (HA standby only).
#
# Runs every minute from cron on the standby. It checks the primary over its
# public IP (bypassing current DNS); if the primary fails 3 checks in a row
# and the local panel is healthy, it moves the polaris A record to the
# standby. One-way only: it never switches back (failback is manual, to
# avoid flapping). Do NOT run this on the primary.
#
# Configuration comes from the environment or /etc/cf-failover.env
# (KEY=VALUE, 0600). Nothing is hardcoded: the installer writes the env
# file from the join config.
#   CF_TOKEN       Cloudflare API token (Zone:DNS:Edit for the zone)
#   CF_ZONE_ID     Cloudflare zone id for the domain's zone
#   CF_RECORD_ID   Cloudflare DNS record id of the polaris A record
#   CF_DOMAIN      e.g. polaris.cuijianzhuang.com
#   CF_ADMIN_PATH  admin path prefix, "/" when none (the API lives under
#                  <admin-path>/api/v1; default "/" keeps /api/v1/info)
#   CF_PRIMARY_IP  primary public IP (dmit)
#   CF_STANDBY_IP  standby public IP (this machine, HK)
#   WITNESS_URL    witness arbiter base URL, e.g. http://<WITNESS_IP>:8443
#                  (optional). When set, the DNS flip needs quorum: this
#                  machine must fail 3 checks in a row AND the witness must
#                  also report the primary unreachable AND the witness must
#                  answer at all. A witness that cannot be reached, or one
#                  that still sees the primary, blocks the flip — never flip
#                  on a single observer. When unset, the old local-only
#                  behaviour applies.
set -u
[ -f /etc/cf-failover.env ] && set -a && . /etc/cf-failover.env && set +a
CF_TOKEN="${CF_TOKEN:-}"
CF_ZONE_ID="${CF_ZONE_ID:-}"
CF_RECORD_ID="${CF_RECORD_ID:-}"
CF_DOMAIN="${CF_DOMAIN:-}"
CF_ADMIN_PATH="${CF_ADMIN_PATH:-/}"
CF_PRIMARY_IP="${CF_PRIMARY_IP:-}"
CF_STANDBY_IP="${CF_STANDBY_IP:-}"
WITNESS_URL="${WITNESS_URL:-}"
# Test hooks (the installer always uses the defaults above).
STATE="${CF_STATE_FILE:-/var/lib/cf-failover.state}"
LOG="${CF_LOG_FILE:-/var/log/cf-failover.log}"
FAIL_N=3

log() { echo "$(date '+%F %T') $*" >> "$LOG"; }

[ -n "$CF_TOKEN" ] && [ -n "$CF_ZONE_ID" ] && [ -n "$CF_RECORD_ID" ] && \
[ -n "$CF_DOMAIN" ] && [ -n "$CF_PRIMARY_IP" ] && [ -n "$CF_STANDBY_IP" ] || {
  echo "cf-failover: CF_TOKEN/CF_ZONE_ID/CF_RECORD_ID/CF_DOMAIN/CF_PRIMARY_IP/CF_STANDBY_IP 未配全（/etc/cf-failover.env 或环境变量）" >&2
  exit 1
}

# Check the primary over its public IP directly (bypasses current DNS).
code=$(curl -sk -o /dev/null -w "%{http_code}" --resolve "$CF_DOMAIN:443:$CF_PRIMARY_IP" --max-time 8 "https://$CF_DOMAIN${CF_ADMIN_PATH%/}/api/v1/info" || echo 000)
if [ "$code" = "200" ]; then echo 0 > "$STATE"; exit 0; fi

# The local panel must be healthy before switching, or the DNS would point
# at a broken standby.
local_code=$(curl -s -o /dev/null -w "%{http_code}" --max-time 5 "http://127.0.0.1:8080${CF_ADMIN_PATH%/}/api/v1/info" || echo 000)
if [ "$local_code" != "200" ]; then log "primary fail but local panel unhealthy, skip"; exit 0; fi

n=$(cat "$STATE" 2>/dev/null || echo 0); n=$((n+1)); echo "$n" > "$STATE"
log "primary check failed ($n/$FAIL_N)"
[ "$n" -lt "$FAIL_N" ] && exit 0

# Quorum (three-condition rule): the witness must agree the primary is
# down, and answer at all — asking it is itself the reachability proof.
# Without a witness the old local-only behaviour applies; a witness that
# cannot be reached, or one that still sees the primary, blocks the flip.
# The witness emits compact JSON ({"reachable":false,...}), so grep for the
# exact token; anything else is "unclear" and also blocks the flip.
if [ -n "$WITNESS_URL" ]; then
  verdict="$(curl -fsS --max-time 5 "$WITNESS_URL/verdict?node=dmit" 2>/dev/null)" || { log "witness unreachable, skip flip"; exit 0; }
  if printf '%s' "$verdict" | grep -q '"reachable":false'; then
    log "witness agrees primary is down, proceeding"
  elif printf '%s' "$verdict" | grep -q '"reachable":true'; then
    log "witness says primary reachable, skip flip"
    echo 0 > "$STATE"
    exit 0
  else
    log "witness verdict unclear, skip flip"
    exit 0
  fi
else
  log "no witness configured, flipping on local checks only"
fi

cur=$(curl -s -H "Authorization: Bearer $CF_TOKEN" \
  "https://api.cloudflare.com/client/v4/zones/$CF_ZONE_ID/dns_records/$CF_RECORD_ID" \
  | python3 -c "import sys,json;print(json.load(sys.stdin)['result']['content'])" 2>/dev/null || echo "")
[ "$cur" = "$CF_STANDBY_IP" ] && { log "already on standby"; exit 0; }

curl -s -X PUT -H "Authorization: Bearer $CF_TOKEN" -H "Content-Type: application/json" \
  "https://api.cloudflare.com/client/v4/zones/$CF_ZONE_ID/dns_records/$CF_RECORD_ID" \
  --data "{\"type\":\"A\",\"name\":\"${CF_DOMAIN%%.*}\",\"content\":\"$CF_STANDBY_IP\",\"ttl\":60,\"proxied\":false}" >> "$LOG"
log "DNS flipped to $CF_STANDBY_IP"

# Report the promotion to the witness so the fencing check on the primary
# (polaris-ha-fence.sh) blocks it from starting as primary again. Best
# effort: the flip already happened, a witness outage must not fail it.
if [ -n "$WITNESS_URL" ]; then
  if curl -fsS --max-time 5 -X POST "$WITNESS_URL/promoted" -H "Content-Type: application/json" -d '{"node":"hk"}' >/dev/null 2>&1; then
    log "witness recorded promotion (hk)"
  else
    log "witness promote report failed (ignored)"
  fi
fi
WATCHDOG
}

# ha_fence_script prints the fencing check (scripts/polaris-ha-fence.sh).
# It is embedded here instead of downloaded so ha-init works from the
# installer alone; scripts/test-ha-dryrun.sh asserts the embedded copy is
# byte-identical to scripts/polaris-ha-fence.sh.
ha_fence_script() {
  cat <<'FENCE'
#!/bin/bash
# polaris-ha-fence.sh — fencing check before the panel starts (primary only).
#
# Runs as a systemd oneshot before polaris.service (see
# polaris-ha-fence.service, installed by install-panel.sh ha-init). It asks
# the witness arbiter who the current primary is: if HK was promoted, this
# machine must not start as primary again — exit non-zero so the panel does
# not start, and print what to do instead (rebuild this machine as a
# replica). When the witness cannot be reached the check warns and lets the
# panel start (availability first); when no witness is configured it skips
# silently. A failback needs a manual reset: after pointing DNS back at
# dmit, report dmit as primary again with
#   curl -X POST $WITNESS_URL/promoted -d '{"node":"dmit"}'
# and then start the panel.
#
# Configuration: WITNESS_URL from the environment or /etc/polaris/witness.env
# (written by ha-init). Nothing is hardcoded.
set -u
[ -f /etc/polaris/witness.env ] && set -a && . /etc/polaris/witness.env && set +a
WITNESS_URL="${WITNESS_URL:-}"
[ -n "$WITNESS_URL" ] || exit 0

resp="$(curl -fsS --max-time 5 "$WITNESS_URL/primary" 2>/dev/null)" || {
  echo "polaris-ha-fence: 见证节点不可达，放行启动（可用性优先）" >&2
  exit 0
}
# The witness emits compact JSON ({"primary":"hk",...}); grep the token.
# A missing token means "no promotion recorded", which is the same as
# primary=null: both let the panel start.
primary=""
case "$resp" in
  *'"primary":"hk"'*) primary="hk" ;;
esac

if [ "$primary" = "hk" ]; then
  echo "polaris-ha-fence: HK 已提主，本机不得以主库身份启动；按文档把本机重建成从库后再启动面板" >&2
  exit 1
fi
exit 0
FENCE
}

# ha_install_syncwg_timer installs the timer that runs `polaris ha sync-wg`
# every 60 seconds (primary only). The service reads the panel env file so
# the CLI finds the database.
ha_install_syncwg_timer() {
  step "安装 WireGuard 同步定时器（每 60 秒）"
  if [ "$HA_DRYRUN" = 1 ]; then
    ha_plan "写 $HA_SYNCWG_UNIT 与 ${HA_SYNCWG_TIMER}，systemctl enable --now polaris-ha-sync.timer"
    return 0
  fi
  cat > "$HA_SYNCWG_UNIT" <<UNIT
[Unit]
Description=Reconcile WireGuard peers with the Polaris ha_peers table
After=network-online.target

[Service]
Type=oneshot
EnvironmentFile=$ENV_FILE
ExecStart=$BIN ha sync-wg
UNIT
  cat > "$HA_SYNCWG_TIMER" <<UNIT
[Unit]
Description=Reconcile WireGuard peers every minute

[Timer]
OnBootSec=1min
OnUnitActiveSec=60s
Unit=polaris-ha-sync-wg.service

[Install]
WantedBy=timers.target
UNIT
  systemctl daemon-reload
  systemctl enable --now polaris-ha-sync.timer >/dev/null 2>&1
  ok "同步定时器已启用（每 60 秒）"
}

# ha_install_heartbeat NODE installs the timer that POSTs a heartbeat to
# the witness every 10 seconds (dmit on the primary, hk on the standby).
# The witness URL comes from WITNESS_URL at install time and is written to
# /etc/polaris/witness.env, which the timer service reads; without it,
# warn and skip (re-run ha-init/ha-join with WITNESS_URL set to retrofit).
ha_install_heartbeat() {
  local node="$1" url
  url="$(ha_witness_url)"
  step "安装见证心跳定时器（每 10 秒）"
  if [ -z "$url" ]; then
    warn "未配 WITNESS_URL：跳过见证心跳安装（见证节点就绪后，用 WITNESS_URL=http://<见证IP>:8443 重跑 ha-init/ha-join 补装）"
    return 0
  fi
  if [ "$HA_DRYRUN" = 1 ]; then
    ha_plan "写 ${HA_HB_ENV}（WITNESS_URL=${url}），写 $HA_HB_UNIT 与 ${HA_HB_TIMER}，systemctl enable --now polaris-ha-heartbeat.timer"
    return 0
  fi
  printf 'WITNESS_URL=%s\n' "$url" > "$HA_HB_ENV"
  chmod 644 "$HA_HB_ENV"
  cat > "$HA_HB_UNIT" <<UNIT
[Unit]
Description=Send a heartbeat to the HA witness arbiter
After=network-online.target

[Service]
Type=oneshot
EnvironmentFile=$HA_HB_ENV
ExecStart=/usr/bin/curl -fsS -X POST --max-time 5 \${WITNESS_URL}/heartbeat -H Content-Type:application/json -d '{"node":"$node"}'
UNIT
  cat > "$HA_HB_TIMER" <<UNIT
[Unit]
Description=Send a heartbeat to the HA witness every 10 seconds

[Timer]
OnBootSec=10s
OnUnitActiveSec=10s
Unit=polaris-ha-heartbeat.service

[Install]
WantedBy=timers.target
UNIT
  systemctl daemon-reload
  systemctl enable --now polaris-ha-heartbeat.timer >/dev/null 2>&1
  ok "见证心跳定时器已启用（每 10 秒，节点 ${node}）"
}

# ha_install_fence installs the fencing oneshot that runs before
# polaris.service: it asks the witness who the current primary is and
# blocks the panel from starting when HK was promoted. Same WITNESS_URL
# rule as ha_install_heartbeat: unset means warn and skip.
ha_install_fence() {
  local url
  url="$(ha_witness_url)"
  step "安装围栏检查（面板启动前）"
  if [ -z "$url" ]; then
    warn "未配 WITNESS_URL：跳过围栏安装（见证节点就绪后，用 WITNESS_URL=http://<见证IP>:8443 重跑 ha-init 补装）"
    return 0
  fi
  if [ "$HA_DRYRUN" = 1 ]; then
    ha_plan "写 ${HA_HB_ENV}（WITNESS_URL=${url}），写 ${HA_FENCE_SCRIPT}，写 ${HA_FENCE_UNIT}（Before=polaris.service）与 ${HA_FENCE_DROPIN}（polaris.service 依赖围栏检查），systemctl daemon-reload"
    return 0
  fi
  printf 'WITNESS_URL=%s\n' "$url" > "$HA_HB_ENV"
  chmod 644 "$HA_HB_ENV"
  ha_fence_script > "$HA_FENCE_SCRIPT"
  chmod +x "$HA_FENCE_SCRIPT"
  cat > "$HA_FENCE_UNIT" <<UNIT
[Unit]
Description=Fencing check before the Polaris panel starts (HA primary)
After=network-online.target
Before=polaris.service

[Service]
Type=oneshot
ExecStart=$HA_FENCE_SCRIPT

[Install]
WantedBy=multi-user.target
UNIT
  mkdir -p "${HA_FENCE_DROPIN%/*}"
  printf '[Unit]\nAfter=polaris-ha-fence.service\nRequires=polaris-ha-fence.service\n' > "$HA_FENCE_DROPIN"
  systemctl daemon-reload
  systemctl enable polaris-ha-fence.service >/dev/null 2>&1
  ok "围栏检查已安装（面板启动前问见证节点）"
}

# ha_print_join_command mints a join token and prints the command to run on
# the standby. The token is one-time and 10 minutes; printing it here is the
# point (the operator copies it to the other machine). The URL includes the
# admin path when one is set: the join API lives under <admin-path>/api/v1.
ha_print_join_command() {
  local tok url ap
  url="$(env_get POLARIS_PUBLIC_URL)"
  ap="$(env_get POLARIS_ADMIN_PATH)"
  case "$ap" in ""|"/") ;; *) url="${url%/}$ap" ;; esac
  if [ "$HA_DRYRUN" = 1 ]; then
    tok="<加入TOKEN>"
  else
    tok="$(polaris_cmd ha-token)" || die "生成加入 token 失败"
  fi
  echo
  box "$C_G" "备机加入命令（10 分钟内有效，一次性）" \
    "  bash <(curl -fsSL $RAW_SCRIPT) ha-join $tok $url"
  dim "  在备机（HK）上以 root 执行上面一行；token 只能用一次，过期重跑 polaris ha-token"
}

do_ha_init() {
  ha_require_apt
  need_installed
  [ -n "$(panel_version)" ] || die "无法读取面板版本：请先正常安装面板"
  # The HA CLI only exists in releases containing PR1+PR2; an older binary
  # would silently start a second panel server instead of minting a token.
  ha_cli_supports ha-token || die "当前面板版本不支持 HA 命令：请先执行 polaris update 升级到最新版"
  ha_cli_supports ha-seal-secret || die "当前面板版本不支持 HA 命令：请先执行 polaris update 升级到最新版"
  ha_cli_supports "ha sync-wg" || die "当前面板版本不支持 HA 命令：请先执行 polaris update 升级到最新版"
  local role
  role="$(env_get POLARIS_HA_ROLE)"
  if [ "$role" = primary ]; then
    echo
    ok "本机已是主节点（HA 已初始化）"
    # A first run that died before step 9 never installed the sync-wg
    # timer, so a registered standby is never added as a WireGuard peer
    # and ha-join stalls at the tunnel check. Re-running ha-init installs
    # any piece the first run skipped.
    STEP_TOTAL=0
    ha_install_syncwg_timer
    # Retrofitting a witness onto an existing primary: re-running ha-init
    # with WITNESS_URL installs the witness pieces the first run skipped.
    if [ -n "${WITNESS_URL:-}" ]; then
      ha_install_heartbeat dmit
      ha_install_fence
    fi
    ha_print_join_command
    return 0
  fi
  [ -z "$role" ] || die "本机角色是 ${role}：ha-init 只能在未初始化的机器上做主节点"
  SECONDS=0 STEP_N=0 STEP_TOTAL=12
  banner "高可用：初始化主节点"
  kv 隧道 "WireGuard $HA_WG_PRIMARY_IP/24，端口 $HA_WG_PORT/udp"
  kv 复制 "PostgreSQL 流复制 + Redis 主从"
  echo

  local repl_pass redis_pass pg_pass my_ip cf_token
  cf_token="${CF_API_TOKEN:-}"
  my_ip="${HA_ENDPOINT:-$(public_ip)}"
  if [ -z "$my_ip" ]; then
    if [ "$HA_DRYRUN" = 1 ]; then my_ip="<本机公网IP>"; else die "无法探测公网 IP：请用 --endpoint <公网IP> 再跑 ha-init"; fi
  fi

  step "前置检查"
  if [ "$HA_DRYRUN" = 1 ]; then
    ha_plan "检查：面板已安装且版本已知、库是 PostgreSQL、PG 大版本可读、51820/udp 与 6379 未被无关进程占用、磁盘剩余 ≥ 库大小 × 1.2、公网 IP 可探测、时间同步、DNS（TTL ≤ 60、DNS-only；无 CF token 时只警告）"
  else
    local dsn
    dsn="$(env_get POLARIS_DB_DSN)"
    case "$dsn" in
      postgres://*|postgresql://*|host=*|dbname=*|user=*) ;;
      *) die "高可用需要 PostgreSQL：当前面板用的是 SQLite，请先 polaris migrate local 再做 HA" ;;
    esac
    [ -n "$(ha_pg_major)" ] || die "PG 大版本读不到：本机 PostgreSQL 未运行（systemctl status postgresql 查看）"
    pg_su -tAc 'SELECT 1' >/dev/null 2>&1 || die "本机 PostgreSQL 未运行（systemctl status postgresql 查看）"
    if command -v ss >/dev/null && ! wg show wg0 >/dev/null 2>&1 && ss -Huln "sport = :$HA_WG_PORT" 2>/dev/null | grep -q .; then
      die "端口 $HA_WG_PORT/udp 已被占用"
    fi
    if command -v redis-cli >/dev/null; then
      if ! redis-cli -h 127.0.0.1 ping 2>/dev/null | grep -qE 'PONG|NOAUTH'; then
        if command -v ss >/dev/null && ss -Hltn "sport = :6379" 2>/dev/null | grep -q .; then
          die "端口 6379 已被非 Redis 进程占用"
        fi
      fi
    elif command -v ss >/dev/null && ss -Hltn "sport = :6379" 2>/dev/null | grep -q .; then
      die "端口 6379 已被占用"
    fi
    local db_bytes free_kb
    db_bytes="$(pg_su -d polaris -tAc 'SELECT pg_database_size(current_database())' 2>/dev/null | tr -d '[:space:]')" || db_bytes=""
    free_kb="$(df -k / 2>/dev/null | awk 'NR==2{print $4}')" || free_kb=""
    if printf '%s' "$db_bytes" | grep -qE '^[0-9]+$' && [ -n "$free_kb" ] && [ "$free_kb" -lt $((db_bytes * 6 / 5 / 1024)) ]; then
      warn "磁盘剩余不足库大小 × 1.2（库约 $(human_bytes "$db_bytes")，/ 剩余 $(human_bytes "$((free_kb * 1024))")）：备机也要装得下整个库"
    fi
    if command -v timedatectl >/dev/null; then
      [ "$(timedatectl show -p NTPSynchronized --value 2>/dev/null)" = yes ] || warn "时间同步未开启（timedatectl set-ntp true）；租约时间以库为准，偏一点没事，别差出分钟级"
    fi
    if [ -n "$cf_token" ]; then
      ha_check_dns || warn "DNS 检查未通过：请确认 polaris A 记录 TTL ≤ 60、DNS-only（灰云）"
    else
      warn "未提供 CF_API_TOKEN：跳过 DNS 检查，请确认 polaris A 记录 TTL ≤ 60、DNS-only（灰云）"
    fi
    ok "前置检查通过"
  fi

  step "备份（面板 + 数据库 + 配置）"
  if [ "$HA_DRYRUN" = 1 ]; then
    ha_plan "polaris backup 到 /root；pg_dumpall 到 /root/pg-full-<日期>.sql；cp $ENV_FILE /root/polaris.env.bak"
  else
    BK_PASS="" do_backup </dev/null
    runuser -u postgres -- pg_dumpall > "/root/pg-full-$(date +%F).sql"
    cp "$ENV_FILE" /root/polaris.env.bak
    chmod 600 /root/polaris.env.bak
    ok "备份完成"
  fi

  step "安装 WireGuard 并生成密钥"
  if ! command -v wg >/dev/null; then
    if [ "$HA_DRYRUN" = 1 ]; then ha_plan "apt install -y wireguard"; else pkg_install wireguard || die "wireguard 安装失败"; fi
  fi
  ha_wg_keypair
  [ "$HA_DRYRUN" = 1 ] || ok "WireGuard 密钥就绪"

  step "写隧道配置并启动"
  ha_wg_write_conf "$HA_WG_PRIMARY_IP"
  ha_wg_start

  step "生成复制密码并密封保存"
  repl_pass="$(rand 32)"
  redis_pass="$(rand 32)"
  pg_pass="$(rand 32)"
  if [ "$HA_DRYRUN" = 1 ]; then
    ha_plan "生成三组随机密码（复制账号 / Redis / polaris 业务账号）：********（不打印明文）"
  else
    ok "三组密码已生成（只存配置与密封库，不打印）"
  fi
  ha_seal ha_repl_password "$repl_pass"
  ha_seal ha_redis_password "$redis_pass"
  if [ -n "$cf_token" ]; then ha_seal cf_api_token "$cf_token"; fi

  step "配置 PostgreSQL 主库"
  local conf hba
  conf="$(ha_pg_conf_file)"
  if [ "$HA_DRYRUN" = 1 ]; then
    ha_plan "postgresql.conf：listen_addresses='localhost,10.8.0.1' wal_level=replica max_wal_senders=5 max_replication_slots=5 wal_keep_size='1GB' hot_standby=on"
    ha_plan "pg_hba.conf：放行 10.8.0.2/32 的 replication 与 polaris 连接（scram-sha-256）"
    ha_plan "建复制账号 replicator（密码 ********）与复制槽 hk_slot；给 polaris 业务账号设 TCP 密码（********）"
    ha_plan "systemctl restart postgresql（面板会短暂连不上库，几秒后恢复）"
  else
    [ -n "$conf" ] || die "找不到 postgresql.conf"
    hba="$(dirname "$conf")/pg_hba.conf"
    ha_conf_set "$conf" listen_addresses "'localhost,10.8.0.1'" "="
    ha_conf_set "$conf" wal_level replica "="
    ha_conf_set "$conf" max_wal_senders 5 "="
    ha_conf_set "$conf" max_replication_slots 5 "="
    ha_conf_set "$conf" wal_keep_size "'1GB'" "="
    ha_conf_set "$conf" hot_standby on "="
    grep -qE '^host[[:space:]]+replication[[:space:]]+replicator[[:space:]]+10\.8\.0\.2/32' "$hba" || printf '%s\n' "host replication replicator 10.8.0.2/32 scram-sha-256" >> "$hba"
    grep -qE '^host[[:space:]]+polaris[[:space:]]+polaris[[:space:]]+10\.8\.0\.2/32' "$hba" || printf '%s\n' "host polaris   polaris     10.8.0.2/32 scram-sha-256" >> "$hba"
    grep -qE '^host[[:space:]]+polaris[[:space:]]+polaris[[:space:]]+10\.8\.0\.1/32' "$hba" || printf '%s\n' "host polaris   polaris     10.8.0.1/32 scram-sha-256" >> "$hba"
    if [ "$(pg_su -tAc "SELECT 1 FROM pg_roles WHERE rolname = 'replicator'")" = 1 ]; then
      pg_su -c "ALTER USER replicator WITH ENCRYPTED PASSWORD '$repl_pass'"
    else
      pg_su -c "CREATE USER replicator WITH REPLICATION ENCRYPTED PASSWORD '$repl_pass'"
    fi
    if [ "$(pg_su -tAc "SELECT 1 FROM pg_replication_slots WHERE slot_name = 'hk_slot'")" != 1 ]; then
      pg_su -c "SELECT pg_create_physical_replication_slot('hk_slot')"
    fi
    # dmit used Unix-socket peer auth, so the polaris role has no password
    # yet; TCP over the tunnel needs one. It is the same password the
    # join-config bundle hands to the standby (the design's "existing
    # business password" only exists once the DSN is TCP).
    pg_su -c "ALTER USER polaris WITH ENCRYPTED PASSWORD '$pg_pass'"
    systemctl restart postgresql
    pg_su -tAc 'SELECT 1' >/dev/null 2>&1 || die "PostgreSQL 重启后未恢复"
    ok "主库配置完成"
  fi

  step "配置 Redis 主库"
  if ! command -v redis-server >/dev/null; then
    if [ "$HA_DRYRUN" = 1 ]; then ha_plan "apt install -y redis-server"; else pkg_install redis-server || die "Redis 安装失败"; fi
  fi
  if [ "$HA_DRYRUN" = 1 ]; then
    ha_plan "redis.conf：bind 127.0.0.1 10.8.0.1，requirepass ********，appendonly yes"
    ha_plan "systemctl restart redis-server；redis-cli info replication 应为 role:master"
  else
    [ -f /etc/redis/redis.conf ] || die "找不到 /etc/redis/redis.conf"
    ha_conf_set /etc/redis/redis.conf bind "127.0.0.1 10.8.0.1" " "
    ha_conf_set /etc/redis/redis.conf protected-mode yes " "
    ha_conf_set /etc/redis/redis.conf requirepass "$redis_pass" " "
    ha_conf_set /etc/redis/redis.conf appendonly yes " "
    systemctl restart redis-server 2>/dev/null || systemctl restart redis 2>/dev/null || die "Redis 重启失败"
    local rrole
    rrole="$(redis-cli -h 10.8.0.1 -a "$redis_pass" info replication 2>/dev/null | awk -F: '/^role:/{print $2}' | tr -d '[:space:]')" || rrole=""
    [ "$rrole" = master ] || die "Redis 未成为主库（role=${rrole:-未知}）"
    ok "Redis 主库就绪"
  fi

  step "面板改走隧道连库"
  if [ "$HA_DRYRUN" = 1 ]; then
    ha_plan "POLARIS_DB_DSN=postgres://polaris:********@10.8.0.1:$(ha_pg_port)/polaris"
    ha_plan "POLARIS_REDIS_URL=redis://:********@10.8.0.1:6379/0"
    ha_plan "POLARIS_HA_ROLE=primary，POLARIS_HA_WG_PUBKEY=<本机WG公钥>，POLARIS_HA_WG_IP=10.8.0.1，POLARIS_HA_ENDPOINT=$my_ip"
    ha_plan "systemctl restart polaris，wait_healthy 确认"
  else
    local pg_port
    pg_port="$(ha_pg_port)"
    # The DSN uses this cluster's real port: Debian moves a cluster off
    # 5432 when that port is taken, and 5432 may belong to another
    # PostgreSQL instance with a different polaris password.
    env_set POLARIS_DB_DSN "postgres://polaris:$pg_pass@10.8.0.1:$pg_port/polaris"
    env_set POLARIS_REDIS_URL "redis://:$redis_pass@10.8.0.1:6379/0"
    env_set POLARIS_HA_ROLE primary
    env_set POLARIS_HA_WG_PUBKEY "$WG_PUB"
    env_set POLARIS_HA_WG_IP "$HA_WG_PRIMARY_IP"
    env_set POLARIS_HA_ENDPOINT "$my_ip"
    # The DSN now looks remote, but the database is still local: keep the
    # After=postgresql/redis ordering so the panel never starts first.
    mkdir -p "${DB_DROPIN%/*}"
    printf '[Unit]\nAfter=postgresql.service redis-server.service redis.service\n' > "$DB_DROPIN"
    systemctl daemon-reload
    systemctl restart polaris
    wait_healthy || die "面板重启后未恢复（polaris log 查看）"
    ok "面板已改走隧道连库"
  fi

  ha_install_syncwg_timer

  ha_install_heartbeat dmit
  ha_install_fence

  step "生成加入 token"
  ha_print_join_command

  echo
  ok "${C_BD}主节点初始化完成${C_0}（用时 $(human_dur "$SECONDS")）"
  echo
  dim "  备机加入后：wg show 应看到对端；主库 pg_stat_replication 应有 streaming 连接"
}

# ha_parse_bundle JSON: parse a join-config bundle into B_* shell variables.
# Values are quoted with Python's shlex.quote, so eval is safe here; the
# bundle holds secrets, which are never printed (dry-run uses placeholders
# instead of calling this).
ha_parse_bundle() {
  eval "$(printf '%s' "$1" | python3 -c '
import sys, json, shlex
b = json.load(sys.stdin)
wg = b.get("wg") or {}
def out(k, v):
    print("B_%s=%s" % (k, shlex.quote(str(v) if v is not None else "")))
out("PANEL_VERSION", b.get("panel_version"))
out("EDITION", b.get("edition"))
out("PUBLIC_URL", b.get("public_url"))
out("ADMIN_PATH", b.get("admin_path"))
out("TIMEZONE", b.get("timezone"))
out("SECRET_KEY", b.get("secret_key"))
out("SECRET_KEY_PREVIOUS", b.get("secret_key_previous"))
out("DB_DSN", b.get("db_dsn"))
out("REDIS_URL", b.get("redis_url"))
out("PG_VERSION", b.get("pg_version"))
out("PG_COLLATE", b.get("pg_collate"))
out("REPL_PASSWORD", b.get("repl_password"))
out("DB_SIZE_BYTES", b.get("db_size_bytes"))
out("WG_PUBKEY", wg.get("pubkey"))
out("WG_ENDPOINT", wg.get("endpoint"))
out("WG_IP", wg.get("ip"))
out("WG_IP_PEER", b.get("wg_ip"))
out("CF_TOKEN", b.get("cf_api_token"))
out("DOMAIN", b.get("domain"))
')"
}

# ha_register_peer MY_PUBLIC_IP: register this standby with the primary.
# This consumes the join token, so it runs only after the config was fetched
# and the version/disk checks passed.
ha_register_peer() {
  local my_ip="$1"
  curl -fsS --max-time 15 -H "Authorization: Bearer $HA_TOKEN" -H "Content-Type: application/json" \
    -d "{\"wg_ip\":\"$HA_WG_SECONDARY_IP\",\"pubkey\":\"$WG_PUB\",\"endpoint\":\"$my_ip\",\"role\":\"secondary\"}" \
    "$HA_API_URL/ha/peers" >/dev/null || die "注册对端失败（token 可能已被用过：主节点重跑 polaris ha-token 再试）"
  ok "对端已注册（wg_ip ${HA_WG_SECONDARY_IP}）"
}

# ha_setup_pg_replica: install the primary's PostgreSQL major version when
# it is missing or different (PGDG), then rebuild this machine as a
# streaming replica in that version's cluster. The replica is addressed
# explicitly by version (HA_PG_CLUSTER): another major may be installed
# alongside, so the pg_su default (port 5432) is not the replica. The
# replica listens on the primary's port (from the bundle's db_dsn):
# promotion only rewrites the host (10.8.0.1 -> 10.8.0.2), never the port.
ha_setup_pg_replica() {
  local want_ver="$B_PG_VERSION" want_port have_ver target_ver target_name data_dir conf basebackup i
  local -a clusters
  [ -n "$want_ver" ] || die "主节点未报告 PG 版本，无法安装对应版本"
  want_port="$(ha_dsn_port "$B_DB_DSN")"
  # Before anything is copied: a basebackup brings the primary's collation
  # with it, and the standby cannot start without the matching locale.
  ha_ensure_locale "${B_PG_COLLATE:-}"
  have_ver="$(ha_pg_major)"
  if [ "$have_ver" != "$want_ver" ] && [ -z "$(ha_pg_cluster_for_ver "$want_ver")" ]; then
    pkg_install postgresql-common || die "postgresql-common 安装失败"
    /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh -y >/dev/null 2>&1 || die "PGDG 源添加失败"
    apt-get update -qq >/dev/null 2>&1
    pkg_install "postgresql-$want_ver" || die "PostgreSQL $want_ver 安装失败"
  fi
  HA_PG_CLUSTER="$(ha_pg_cluster_for_ver "$want_ver")"
  [ -n "$HA_PG_CLUSTER" ] || die "找不到 PostgreSQL $want_ver 集群"
  target_ver="${HA_PG_CLUSTER%%/*}"; target_name="${HA_PG_CLUSTER##*/}"
  data_dir="$(ha_pg_cluster_datadir "$HA_PG_CLUSTER")"
  [ -n "$data_dir" ] || die "找不到 PostgreSQL $want_ver 数据目录"
  conf="/etc/postgresql/$target_ver/$target_name/postgresql.conf"
  if [ ! -f "$conf" ]; then
    conf="$(pg_cluster_su "$HA_PG_CLUSTER" -tAc 'SHOW config_file' 2>/dev/null | tr -d '[:space:]')" || conf=""
  fi
  [ -n "$conf" ] && [ -f "$conf" ] || die "找不到 postgresql.conf"
  # A standby.signal only says an earlier ha-join started building a replica;
  # it does not say that attempt finished. An interrupted basebackup or a
  # cluster that refused to start leaves the file behind, and trusting it
  # skipped the rebuild and failed later at "still not in recovery" with no
  # clue why. Treat a cluster as done only when it actually answers as a
  # standby.
  if [ -f "$data_dir/standby.signal" ]; then
    if ha_replica_is_standby; then
      ok "本机已是从库（standby.signal 存在且运行中），跳过重建"
    else
      warn "本机残留 standby.signal 但从库并未正常运行：清空数据目录后重建（原数据只是上次失败留下的）"
      rm -rf "${data_dir:?}/"*
    fi
  fi
  if [ ! -f "$data_dir/standby.signal" ]; then
    # A cluster holding real polaris data must never be wiped or abandoned
    # blindly: check every local cluster, not just the pg_su default.
    systemctl start postgresql >/dev/null 2>&1 || true
    mapfile -t clusters < <(pg_lsclusters --no-header 2>/dev/null | awk '{print $1"/"$2}')
    for c in "${clusters[@]}"; do
      if pg_cluster_su "$c" -tAc "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'polaris')" 2>/dev/null | grep -q t; then
        die "本机已有 polaris 数据库（集群 ${c}）：ha-join 只接管全新空库，请先备份并清空"
      fi
    done
    # The replica must listen on the primary's port. If another local
    # cluster holds that port, stop it (its panel is being replaced; its
    # data stays on disk) so the replica can bind.
    mapfile -t clusters < <(pg_lsclusters --no-header 2>/dev/null | awk -v want="$want_port" -v self="$HA_PG_CLUSTER" '$1"/"$2 != self && $3 == want {print $1"/"$2}')
    for c in "${clusters[@]}"; do
      warn "端口 $want_port 被本机 PostgreSQL $c 占用：先停掉它（数据保留在磁盘上），再建从库"
      pg_ctlcluster "${c%%/*}" "${c##*/}" stop >/dev/null 2>&1 || true
      systemctl disable "postgresql@${c%%/*}-${c##*/}" >/dev/null 2>&1 || true
    done
    pg_ctlcluster "$target_ver" "$target_name" stop >/dev/null 2>&1 || true
    rm -rf "${data_dir:?}/"*
    # -R does not store the password in primary_conninfo, so the replica
    # reconnects with this file; it must stay on the standby.
    install -d -m 0700 -o postgres -g postgres /var/lib/postgresql
    printf '10.8.0.1:%s:*:replicator:%s\n' "$want_port" "$B_REPL_PASSWORD" > /var/lib/postgresql/.pgpass
    chmod 0600 /var/lib/postgresql/.pgpass
    chown postgres:postgres /var/lib/postgresql/.pgpass
    basebackup="/usr/lib/postgresql/$want_ver/bin/pg_basebackup"
    [ -x "$basebackup" ] || basebackup="pg_basebackup"
    runuser -u postgres -- env PGPASSFILE=/var/lib/postgresql/.pgpass \
      "$basebackup" -h "$HA_WG_PRIMARY_IP" -p "$want_port" -U replicator -D "$data_dir" -Fp -Xs -P -R -S hk_slot \
      || die "pg_basebackup 失败（检查复制账号、密码与隧道；主库端口 ${want_port}）"
    # pg_basebackup -R should have written standby.signal and
    # primary_conninfo, but if either is missing the replica starts as a
    # standalone primary and never enters recovery mode. Recreate them
    # explicitly so the join heals itself instead of failing later with
    # "not in recovery mode" and no further clue. application_name uses
    # HA_SYNC_APPNAME (polaris_standby) so synchronous replication keeps
    # working when this fallback fires.
    if [ ! -f "$data_dir/standby.signal" ]; then
      warn "pg_basebackup 未生成 standby.signal：手动补建"
      : > "$data_dir/standby.signal"
      chown postgres:postgres "$data_dir/standby.signal"
    fi
    if ! grep -q "primary_conninfo" "$data_dir/postgresql.auto.conf" 2>/dev/null; then
      warn "pg_basebackup 未写 primary_conninfo：手动补写"
      printf "primary_conninfo = 'host=%s port=%s user=replicator password=%s application_name=%s'\n" \
        "$HA_WG_PRIMARY_IP" "$want_port" "$B_REPL_PASSWORD" "$HA_SYNC_APPNAME" >> "$data_dir/postgresql.auto.conf"
      chown postgres:postgres "$data_dir/postgresql.auto.conf"
    fi
  fi
  # Give the walreceiver a stable application_name so the primary can name
  # it in synchronous_standby_names. pg_basebackup -R writes
  # application_name=walreceiver, which no operator would guess and which
  # makes the HA page's switch look like it did nothing. Only that word is
  # replaced: -R has already put the password and the primary's address in
  # the line, and rewriting the whole line would drop them. The value goes
  # in through HA_CONNINFO_APPNAME, never interpolated into the perl program,
  # so no shell quoting can alter the file.
  if [ "$HA_DRYRUN" != 1 ] && grep -qE '^[# ]*primary_conninfo[[:space:]]*=' "$conf"; then
    if grep -q 'application_name=' "$conf"; then
      HA_CONNINFO_APPNAME="$HA_SYNC_APPNAME" perl -i -pe "s/(application_name=)[^ '\"]*/\${1}\$ENV{HA_CONNINFO_APPNAME}/g" "$conf"
    else
      HA_CONNINFO_APPNAME="$HA_SYNC_APPNAME" perl -i -pe "s/(^[# ]*primary_conninfo[[:space:]]*=.*[^ ])'\$/\${1} application_name=\$ENV{HA_CONNINFO_APPNAME}'/" "$conf"
    fi
  fi
  ha_conf_set "$conf" listen_addresses "'localhost,10.8.0.2'" "="
  ha_conf_set "$conf" hot_standby on "="
  ha_conf_set "$conf" port "$want_port" "="
  pg_ctlcluster "$target_ver" "$target_name" start >/dev/null 2>&1 || systemctl start "postgresql@$target_ver-$target_name" >/dev/null 2>&1 || die "从库启动失败"
  systemctl enable "postgresql@$target_ver-$target_name" >/dev/null 2>&1 || true
  for i in $(seq 1 30); do
    [ "$(ha_replica_su -tAc 'SELECT pg_is_in_recovery()' 2>/dev/null)" = t ] && break
    if [ "$i" = 30 ]; then
      if [ -f "$data_dir/standby.signal" ]; then dim "standby.signal 存在" >&2; else red "standby.signal 缺失" >&2; fi
      red "最近的 PostgreSQL 日志（journalctl）："
      journalctl -u "postgresql@$target_ver-$target_name" -n 20 --no-pager >&2 || true
      die "从库启动后仍不在恢复模式"
    fi
    sleep 1
  done
  ok "从库已启动（恢复模式）"
}

# ha_proxy_mode_resolve: decide which reverse proxy serves the standby on
# 443 and record it in HA_PROXY_MODE (nginx|openresty). HA_PROXY_MODE=auto
# (default) detects from who owns 443: free (or nginx, e.g. a previous join)
# means nginx, OpenResty means reuse it; anything else (e.g. a node inbound)
# aborts with a hint to move it. Explicit nginx/openresty is validated the
# same way; in a dry run auto plans the nginx path (pass
# HA_PROXY_MODE=openresty to plan the reuse path instead).
ha_proxy_mode_resolve() {
  case "${HA_PROXY_MODE:-auto}" in
    auto)
      if [ "$HA_DRYRUN" = 1 ]; then HA_PROXY_MODE=nginx; return 0; fi
      if port_busy 443; then
        local o
        o="$(port_owner 443)"
        case "$o" in
          *nginx*) warn "443 已被 nginx 占用（可能是上次 join 留下的），继续用 nginx 模式"; HA_PROXY_MODE=nginx ;;
          *openresty*) warn "443 已被 OpenResty 占用：复用 OpenResty 当反代，不安装 nginx（反代需自行在 1Panel 里添加）"; HA_PROXY_MODE=openresty ;;
          *) die "端口 443 已被占用${o:+（${o}）}：若是节点入站，先在面板里把它改到别的端口，再重跑 ha-join" ;;
        esac
      else
        HA_PROXY_MODE=nginx
      fi
      ;;
    nginx)
      if [ "$HA_DRYRUN" = 1 ]; then return 0; fi
      if port_busy 443; then
        local o
        o="$(port_owner 443)"
        case "$o" in
          *nginx*) warn "443 已被 nginx 占用（可能是上次 join 留下的），继续" ;;
          *) die "端口 443 已被占用${o:+（${o}）}：HA_PROXY_MODE=nginx 需要 443 空闲（或已被 nginx 占用）" ;;
        esac
      fi
      ;;
    openresty)
      if [ "$HA_DRYRUN" = 1 ]; then return 0; fi
      if port_busy 443; then
        local o
        o="$(port_owner 443)"
        case "$o" in
          *openresty*) ;;
          *) die "HA_PROXY_MODE=openresty，但 443 不是 OpenResty 占用${o:+（${o}）}：先腾出 443 或换模式" ;;
        esac
      else
        die "HA_PROXY_MODE=openresty，但 443 空闲：请先确认 OpenResty 在监听 443，或去掉该设置走自动模式"
      fi
      ;;
    *) die "HA_PROXY_MODE 非法：${HA_PROXY_MODE}（auto/nginx/openresty）" ;;
  esac
}

# ha_openresty_guide DOMAIN: print the reverse-proxy config the operator
# adds themselves in 1Panel's OpenResty (the script installs nothing here:
# no nginx, no certbot — 1Panel manages the site's certificate). 443
# proxies to the panel on 127.0.0.1:8080; the stream block passes 9443
# through to the agent port without terminating TLS.
ha_openresty_guide() {
  local domain="$1"
  echo
  echo "  在 1Panel 的 OpenResty 里给 $domain 建站（开启 HTTPS，证书由 1Panel 申请/管理），再添加以下反代："
  echo
  echo "  # HTTP：443 反代面板"
  echo "  server {"
  echo "      listen 443 ssl;"
  echo "      server_name $domain;"
  echo "      ssl_certificate <1Panel 里该站点的证书路径>;"
  echo "      ssl_certificate_key <1Panel 里该站点的私钥路径>;"
  echo "      location / {"
  echo "          proxy_pass http://127.0.0.1:8080;"
  echo "          proxy_set_header Host \$host;"
  echo "          proxy_set_header X-Real-IP \$remote_addr;"
  echo "          proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;"
  echo "          proxy_set_header X-Forwarded-Proto \$scheme;"
  echo "      }"
  echo "  }"
  echo
  echo "  # TCP 直通：9443 节点接入（不终结 TLS，加到 OpenResty 主配置的 stream 段）"
  echo "  stream {"
  echo "      upstream panel_agent {"
  echo "          server 127.0.0.1:9443;"
  echo "      }"
  echo "      server {"
  echo "          listen 9443;"
  echo "          proxy_pass panel_agent;"
  echo "          proxy_timeout 1h;"
  echo "      }"
  echo "  }"
  echo
  echo "  加完后验证：curl -sk --resolve $domain:443:<本机公网IP> https://$domain/api/v1/info 应返回 200"
  echo "  （1Panel 的 OpenResty 若跑在容器里且 127.0.0.1 不通，把 127.0.0.1 换成宿主机 IP）"
}

# ha_certbot_dns TOKEN DOMAIN: request a certificate with the Cloudflare
# DNS-01 plugin. The token lives in /root/.cloudflare.ini (0600), the
# standard certbot layout.
ha_certbot_dns() {
  local token="$1" domain="$2"
  if [ "$HA_DRYRUN" = 1 ]; then
    ha_plan "certbot certonly --dns-cloudflare -d ${domain}（CF token 不打印）"
    return 0
  fi
  pkg_install certbot python3-certbot-dns-cloudflare || die "certbot 安装失败"
  install -d -m 0700 /root
  printf 'dns_cloudflare_api_token = %s\n' "$token" > /root/.cloudflare.ini
  chmod 600 /root/.cloudflare.ini
  certbot certonly --dns-cloudflare --dns-cloudflare-credentials /root/.cloudflare.ini \
    --non-interactive --agree-tos --register-unsafely-without-email -d "$domain" \
    || die "证书申请失败（检查 CF token 权限 Zone:DNS:Edit）"
  certbot renew --dry-run >/dev/null 2>&1 || warn "证书续期演练未通过（certbot renew --dry-run 查看）"
  ok "证书已申请（DNS-01）：$domain"
}

# ha_nginx_enable DOMAIN: nginx -t + enable once the certificate exists.
ha_nginx_enable() {
  local domain="$1"
  if [ "$HA_DRYRUN" = 1 ]; then
    ha_plan "nginx -t 通过后 systemctl enable --now nginx"
    return 0
  fi
  [ -f "/etc/letsencrypt/live/$domain/fullchain.pem" ] || { warn "证书尚未就绪，nginx 暂不启动"; return 0; }
  nginx -t || die "nginx 配置检查失败"
  systemctl enable --now nginx >/dev/null 2>&1
  ok "nginx 已启动（443 反代 + 9443 直通）"
}

# ha_setup_nginx_cert: nginx reverse proxy (443) + stream passthrough (9443)
# + certbot DNS-01 certificate. Without a Cloudflare token the configs are
# written but nginx stays stopped; ha-join --finish completes it.
ha_setup_nginx_cert() {
  local cf_token="${CF_API_TOKEN:-$B_CF_TOKEN}" domain="$B_DOMAIN"
  if [ "${HA_PROXY_MODE:-auto}" = openresty ]; then
    if [ "$HA_DRYRUN" = 1 ]; then
      ha_plan "复用 OpenResty：不安装 nginx、不写 nginx 配置、不申请证书（证书由 1Panel 管理）"
    else
      warn "复用 OpenResty 模式：跳过 nginx 安装、证书申请与 nginx 配置"
    fi
    ha_openresty_guide "$domain"
    return 0
  fi
  if [ "$HA_DRYRUN" = 1 ]; then
    ha_plan "apt install -y nginx-full"
    if [ -n "$cf_token" ]; then
      ha_plan "certbot certonly --dns-cloudflare -d ${domain}（CF token 密封保存，不打印）"
    else
      ha_plan "无 CF token：跳过证书申请（留给 ha-join --finish），nginx 配置先写好、暂不启用"
    fi
    ha_plan "写 /etc/nginx/sites-available/polaris（443 反代 127.0.0.1:8080）与 nginx.conf stream 段（9443 直通）"
    ha_nginx_enable "$domain"
    return 0
  fi
  pkg_install nginx-full || die "nginx 安装失败"
  if [ -n "$cf_token" ]; then
    ha_certbot_dns "$cf_token" "$domain"
  else
    warn "未提供 CF token：跳过证书申请；拿到 token 后执行 ha-join --finish 补做"
  fi
  cat > /etc/nginx/sites-available/polaris <<NGINX
server {
    listen 80;
    server_name $domain;
    return 301 https://\$host\$request_uri;
}

server {
    listen 443 ssl;
    server_name $domain;

    ssl_certificate /etc/letsencrypt/live/$domain/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/$domain/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;
    }
}
NGINX
  ln -sf /etc/nginx/sites-available/polaris /etc/nginx/sites-enabled/polaris
  if ! grep -q '^stream' /etc/nginx/nginx.conf; then
    cat >> /etc/nginx/nginx.conf <<NGINX

stream {
    upstream panel_agent {
        server 127.0.0.1:9443;
    }
    server {
        listen 9443;
        proxy_pass panel_agent;
        proxy_timeout 1h;
    }
}
NGINX
  fi
  ha_nginx_enable "$domain"
}

# ha_setup_watchdog installs the DNS failover watchdog (standby only) and
# the hourly backup-sync cron. The watchdog needs the Cloudflare zone and
# record ids, looked up live; the backup sync only when passwordless SSH to
# the primary already works.
ha_setup_watchdog() {
  local cf_token="${CF_API_TOKEN:-$B_CF_TOKEN}" domain="$B_DOMAIN"
  if [ "$HA_DRYRUN" = 1 ]; then
    ha_plan "拷看门狗到 /usr/local/bin/cf-failover.sh，写 /etc/cf-failover.env（CF token 打码），cron 每分钟跑一次"
    [ -n "$(ha_witness_url)" ] && ha_plan "看门狗配 WITNESS_URL=$(ha_witness_url)（切 DNS 前要见证节点也确认主节点挂了）"
    ha_plan "备机有免密 SSH 到主节点时，加每小时备份同步 cron（rsync 到 backups-peer）"
    return 0
  fi
  if [ -n "$cf_token" ]; then
    if ha_cf_ids "$cf_token" "$domain"; then
      ha_watchdog_script > "$HA_WATCHDOG"
      chmod +x "$HA_WATCHDOG"
      {
        printf 'CF_TOKEN=%s\n' "$cf_token"
        printf 'CF_ZONE_ID=%s\n' "$CF_ZONE_ID"
        printf 'CF_RECORD_ID=%s\n' "$CF_RECORD_ID"
        printf 'CF_DOMAIN=%s\n' "$domain"
        printf 'CF_ADMIN_PATH=%s\n' "${B_ADMIN_PATH:-/}"
        printf 'CF_PRIMARY_IP=%s\n' "$B_WG_ENDPOINT"
        printf 'CF_STANDBY_IP=%s\n' "$(public_ip)"
        [ -n "$(ha_witness_url)" ] && printf 'WITNESS_URL=%s\n' "$(ha_witness_url)"
      } > "$HA_WATCHDOG_ENV"
      chmod 600 "$HA_WATCHDOG_ENV"
      (crontab -l 2>/dev/null | grep -v 'cf-failover.sh'; echo "* * * * * $HA_WATCHDOG") | crontab -
      ok "看门狗已安装（每分钟）"
    else
      warn "Cloudflare zone/record 查不到：看门狗暂不安装，ha-join --finish 时重试"
    fi
  else
    warn "未提供 CF token：跳过看门狗，ha-join --finish 时补装"
  fi
  if ssh -o BatchMode=yes -o ConnectTimeout=5 "root@$HA_WG_PRIMARY_IP" true 2>/dev/null; then
    (crontab -l 2>/dev/null | grep -v 'backups-peer'; echo "0 * * * * rsync -a --delete /var/lib/polaris/backups/ root@$HA_WG_PRIMARY_IP:/var/lib/polaris/backups-peer/") | crontab -
    ok "备份同步 cron 已安装（每小时）"
  else
    warn "到主节点的免密 SSH 未通：先执行 ssh-copy-id root@${HA_WG_PRIMARY_IP}，再手动加备份同步 cron（见部署文档 10.3）"
  fi
}

# ha_self_check runs the runbook §8 checklist and prints one ok/bad line
# per item; any failure aborts the join at the end (never silently passed).
ha_self_check() {
  local fails=0 redis_pass admin_path
  if [ "$HA_DRYRUN" = 1 ]; then
    if [ "${HA_PROXY_MODE:-auto}" = openresty ]; then
      ha_plan "自检：wg show 有对端、ping 10.8.0.1 通、pg_is_in_recovery()=t、回放追上、Redis role:slave、cluster_members 两行、/api/v1/info 200、备机 443 直验跳过（反代自行添加后手动验）"
    else
      ha_plan "自检：wg show 有对端、ping 10.8.0.1 通、pg_is_in_recovery()=t、回放追上、Redis role:slave、cluster_members 两行、/api/v1/info 200、--resolve 验备机 443"
    fi
    return 0
  fi
  admin_path="${B_ADMIN_PATH:-/}"
  if wg show wg0 2>/dev/null | grep -q "peer: $B_WG_PUBKEY"; then ok "隧道：对端已连接"; else bad "隧道：wg show 未见主节点对端"; fails=$((fails+1)); fi
  if ping -c 2 -W 2 "$HA_WG_PRIMARY_IP" >/dev/null 2>&1; then ok "隧道：ping $HA_WG_PRIMARY_IP 通"; else bad "隧道：ping $HA_WG_PRIMARY_IP 不通"; fails=$((fails+1)); fi
  if [ "$(ha_replica_su -tAc 'SELECT pg_is_in_recovery()' 2>/dev/null)" = t ]; then ok "PostgreSQL：从库（恢复模式）"; else bad "PostgreSQL：不在恢复模式"; fails=$((fails+1)); fi
  if [ "$(ha_replica_su -tAc 'SELECT pg_last_wal_receive_lsn() = pg_last_wal_replay_lsn()' 2>/dev/null)" = t ]; then ok "PostgreSQL：回放已追上"; else bad "PostgreSQL：回放未追上（稍等再查）"; fails=$((fails+1)); fi
  redis_pass="$(ha_url_password "$B_REDIS_URL")"
  if [ -n "$redis_pass" ] && redis-cli -h 10.8.0.2 -a "$redis_pass" info replication 2>/dev/null | grep -q '^role:slave' &&
    redis-cli -h 10.8.0.2 -a "$redis_pass" info replication 2>/dev/null | grep -q '^master_link_status:up'; then
    ok "Redis：从库，主链接 up"
  else
    bad "Redis：不是从库或主链接未通"
    fails=$((fails+1))
  fi
  local i members
  members=""
  for i in $(seq 1 15); do
    members="$(ha_replica_su -d polaris -tAc 'SELECT count(*) FROM cluster_members' 2>/dev/null | tr -d '[:space:]')" || members=""
    [ "$members" = 2 ] && break
    sleep 2
  done
  if [ "$members" = 2 ]; then ok "双实例：cluster_members 两行"; else bad "双实例：cluster_members 只有 ${members:-0} 行（面板可能还未注册）"; fails=$((fails+1)); fi
  if curl -sf --max-time 5 "http://127.0.0.1:8080${admin_path%/}/api/v1/info" >/dev/null 2>&1; then ok "面板：/api/v1/info 200"; else bad "面板：/api/v1/info 不通"; fails=$((fails+1)); fi
  if [ -f "/etc/letsencrypt/live/$B_DOMAIN/fullchain.pem" ]; then
    if curl -sk --resolve "$B_DOMAIN:443:$(public_ip)" --max-time 8 "https://$B_DOMAIN${admin_path%/}/api/v1/info" >/dev/null 2>&1; then
      ok "备机直验：https://$B_DOMAIN/api/v1/info 200"
    else
      bad "备机直验：https://$B_DOMAIN/api/v1/info 不通"
      fails=$((fails+1))
    fi
  else
    dim "  备机直验跳过（证书尚未就绪，ha-join --finish 后再验）"
  fi
  [ "$fails" = 0 ] || die "自检有 $fails 项未通过（上面标 ✗ 的项），先修好再投入使用"
}

# do_pg_upgrade [--to VER] [--dry-run]: upgrade the local PostgreSQL
# cluster (the panel's database) to a newer major version, in place, with
# Debian's pg_upgradecluster (-m upgrade: pg_upgrade copies the data
# files). The upgraded cluster keeps the original port (the old one is
# moved aside, left stopped on manual startup, and not removed); the panel
# DSN needs no change. Use it to align versions before ha-join when the
# two machines disagree (e.g. primary on 15, standby on 17: upgrade the
# primary to 17, then join). Replication slots are not preserved, and a
# streaming standby cannot replay WAL across major versions: while one is
# connected this refuses to run (rebuild the standby after the upgrade).
do_pg_upgrade() {
  local cur_cluster cur_ver cur_name cur_port target_ver data_dir db_bytes free_kb bk_file panel_running=0
  local -a clusters
  step "确认当前集群"
  if [ "$HA_DRYRUN" = 1 ]; then
    ha_plan "读当前 PG 大版本与端口（pg_lsclusters / SHOW port）"
    ha_plan "目标版本：${PG_UPGRADE_TO:-最新可用大版本}（须大于当前版本；不支持降级）"
    ha_plan "检查：无表空间、无正在连接的从库、磁盘剩余 ≥ 库大小 × 1.5"
    ha_plan "备份：pg_dumpall 到 /root；停面板（防写入）"
    ha_plan "装 postgresql-<目标版本>（PGDG）；空的目标集群先 pg_dropcluster 删掉"
    ha_plan "pg_upgradecluster -v <目标版本> -m upgrade <当前版本> main"
    ha_plan "验证：新集群在原端口、polaris 库在、SELECT 1 通；重启面板并 wait_healthy"
    ha_plan "旧集群保留在原地（已停、manual 启动）：验证无误后用 pg_dropcluster <旧版本> main 删除"
    return 0
  fi
  # The panel's cluster: the one pg_su reaches (matched by data directory).
  data_dir="$(ha_pg_data_dir)"
  [ -n "$data_dir" ] || die "本机 PostgreSQL 未运行（systemctl status postgresql 查看）"
  cur_cluster="$(pg_lsclusters --no-header 2>/dev/null | awk -v d="$data_dir" '$6==d {print $1"/"$2; exit}')"
  [ -n "$cur_cluster" ] || die "找不到本机 PostgreSQL 集群（pg_lsclusters 查看）"
  cur_ver="${cur_cluster%%/*}"; cur_name="${cur_cluster##*/}"
  cur_port="$(ha_pg_port)"
  # The target version is needed before installing it, so the PGDG source
  # goes in first (idempotent when already present).
  pkg_install postgresql-common || die "postgresql-common 安装失败"
  /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh -y >/dev/null 2>&1 || die "PGDG 源添加失败"
  apt-get update -qq >/dev/null 2>&1
  if [ -n "${PG_UPGRADE_TO:-}" ]; then
    target_ver="$PG_UPGRADE_TO"
    case "$target_ver" in ''|*[!0-9]*) die "--to 须是 PG 大版本号，如 --to 17" ;; esac
  else
    target_ver="$(apt-cache search --names-only '^postgresql-[0-9]+$' 2>/dev/null | sed -E 's/^postgresql-([0-9]+).*/\1/' | sort -n | tail -1)"
    [ -n "$target_ver" ] || die "查不到可用的 PG 大版本：用 --to <版本> 指定，如 --to 17"
  fi
  if [ "$target_ver" = "$cur_ver" ]; then die "本机已是 PostgreSQL ${cur_ver}，无需升级"; fi
  if [ "$target_ver" -lt "$cur_ver" ]; then die "不支持降级：本机是 ${cur_ver}，目标 $target_ver 更低"; fi

  step "升级前检查"
  # pg_upgradecluster cannot upgrade clusters that use tablespaces.
  local tbspace standbys
  tbspace="$(pg_su -tAc "SELECT count(*) FROM pg_tablespace WHERE spcname NOT IN ('pg_default','pg_global')" 2>/dev/null | tr -d '[:space:]')" || tbspace=""
  [ "$tbspace" = 0 ] || die "本机 PG 用了表空间（$tbspace 个），pg_upgradecluster 不支持升级带表空间的集群"
  # A streaming standby cannot replay WAL across major versions.
  standbys="$(pg_su -tAc "SELECT count(*) FROM pg_stat_replication" 2>/dev/null | tr -d '[:space:]')" || standbys=""
  if [ -n "$standbys" ] && [ "$standbys" != 0 ]; then
    die "有从库正在流复制（$standbys 个）：大版本升级后从库无法继续回放，请先停掉备机复制，升级完再重建备机"
  fi
  if pg_su -tAc "SELECT count(*) FROM pg_replication_slots" 2>/dev/null | grep -qE '^[1-9]'; then
    warn "复制槽升级后不保留（如 hk_slot）：备机加入时会重建，正在复制的备机需重建"
  fi
  db_bytes="$(pg_su -tAc 'SELECT coalesce(sum(pg_database_size(datname)),0) FROM pg_database' 2>/dev/null | tr -d '[:space:]')" || db_bytes=""
  free_kb="$(df -k / 2>/dev/null | awk 'NR==2{print $4}')" || free_kb=""
  if printf '%s' "$db_bytes" | grep -qE '^[0-9]+$' && [ -n "$free_kb" ] && [ "$free_kb" -lt $((db_bytes * 3 / 2 / 1024)) ]; then
    die "磁盘剩余不足：全库约 $(human_bytes "$db_bytes")，升级复制数据约需 1.5 倍，/ 剩余 $(human_bytes "$((free_kb * 1024))")"
  fi
  ok "当前 PostgreSQL ${cur_ver}（集群 ${cur_cluster}，端口 ${cur_port}），目标 $target_ver"

  step "备份"
  bk_file="/root/pg-full-$(date +%F-%H%M)-pre-upgrade.sql"
  runuser -u postgres -- pg_dumpall > "$bk_file" || die "pg_dumpall 备份失败"
  chmod 600 "$bk_file"
  if installed; then cp "$ENV_FILE" /root/polaris.env.pre-upgrade.bak 2>/dev/null && chmod 600 /root/polaris.env.pre-upgrade.bak 2>/dev/null || true; fi
  ok "备份完成：$bk_file"

  step "停面板并装新版本"
  if installed && systemctl is-active --quiet polaris 2>/dev/null; then
    panel_running=1
    systemctl stop polaris || die "面板停止失败"
  fi
  pkg_install "postgresql-$target_ver" || die "PostgreSQL $target_ver 安装失败"
  # Installing the new version may auto-create an empty cluster of that
  # version; pg_upgradecluster refuses to create its target while one
  # exists, so drop the empty one first (a non-empty one is never touched).
  local target_cluster target_dbs
  target_cluster="$(ha_pg_cluster_for_ver "$target_ver")"
  if [ -n "$target_cluster" ]; then
    target_dbs="$(pg_cluster_su "$target_cluster" -tAc "SELECT count(*) FROM pg_database WHERE datname NOT IN ('template0','template1','postgres')" 2>/dev/null | tr -d '[:space:]')" || target_dbs=""
    if [ "$target_dbs" = 0 ]; then
      warn "删掉安装时自动建的空集群 ${target_cluster}，再升级"
      pg_dropcluster "${target_cluster%%/*}" "${target_cluster##*/}" --stop >/dev/null 2>&1 || die "删空集群 $target_cluster 失败"
    else
      die "目标版本已有非空集群 ${target_cluster}：先确认它的数据是否还要，再手动处理"
    fi
  fi

  step "升级集群"
  if pg_upgradecluster -v "$target_ver" -m upgrade "$cur_ver" "$cur_name"; then
    ok "集群已升级到 PostgreSQL $target_ver"
  else
    warn "升级失败：尝试恢复旧集群"
    pg_ctlcluster "$cur_ver" "$cur_name" start >/dev/null 2>&1 || true
    [ "$panel_running" = 1 ] && systemctl start polaris >/dev/null 2>&1
    die "pg_upgradecluster 失败（旧集群已尝试恢复，面板已尝试重启；备份在 ${bk_file}）：查上面的输出与日志后重试"
  fi

  step "验证"
  local new_cluster new_port
  new_cluster="$(ha_pg_cluster_for_ver "$target_ver")"
  [ -n "$new_cluster" ] || die "升级后找不到 PostgreSQL $target_ver 集群"
  new_port="$(pg_cluster_su "$new_cluster" -tAc 'SHOW port' 2>/dev/null | tr -d '[:space:]')" || new_port=""
  [ "$new_port" = "$cur_port" ] || die "升级后端口变了（原 ${cur_port}，现 ${new_port:-未知}）：检查 pg_lsclusters"
  [ "$(pg_cluster_su "$new_cluster" -tAc "SELECT 1 FROM pg_database WHERE datname = 'polaris'")" = 1 ] || die "升级后 polaris 库不见了（备份在 ${bk_file}）"
  ok "PostgreSQL $target_ver 在端口 ${cur_port}，polaris 库完好"
  if [ "$panel_running" = 1 ]; then
    systemctl start polaris
    wait_healthy || die "面板重启后未恢复（polaris log 查看；数据库备份在 ${bk_file}）"
    ok "面板已恢复"
  fi

  echo
  ok "${C_BD}PostgreSQL 已从 $cur_ver 升级到 ${target_ver}（端口 $cur_port 不变）${C_0}"
  echo
  dim "  旧集群 $cur_cluster 保留在原地（已停、manual 启动，不会自动启动）"
  dim "  验证面板正常后，删除旧集群释放空间：pg_dropcluster $cur_ver $cur_name"
  dim "  备份在 $bk_file"
}

do_ha_join() {
  ha_require_apt
  # --finish needs no token or URL: it only needs the local env + CF token.
  if [ "$HA_FINISH" = 1 ]; then
    do_ha_finish
    return 0
  fi
  [ -n "$HA_TOKEN" ] && [ -n "$HA_PANEL_URL" ] || die "用法：bash <(curl -fsSL $RAW_SCRIPT) ha-join <token> <panel-url> [--dry-run]"
  HA_PANEL_URL="${HA_PANEL_URL%/}"
  HA_API_URL="$HA_PANEL_URL/api/v1"
  local role
  role="$(env_get POLARIS_HA_ROLE)"
  if [ "$role" = secondary ]; then
    echo
    ok "本机已是备节点（HA 已加入）"
    # Retrofitting a witness onto an existing standby: re-running ha-join
    # with WITNESS_URL installs the heartbeat the first run skipped.
    if [ -n "${WITNESS_URL:-}" ]; then
      STEP_TOTAL=0
      ha_install_heartbeat hk
    fi
    do_ha_status
    return 0
  fi
  [ -z "$role" ] || die "本机角色是 ${role}：ha-join 只能在未初始化的备机上执行"
  SECONDS=0 STEP_N=0 STEP_TOTAL=12
  banner "高可用：加入备节点"
  kv 主节点 "$HA_PANEL_URL"
  echo

  step "前置检查"
  if [ "$HA_DRYRUN" = 1 ]; then
    ha_proxy_mode_resolve
    if [ "$HA_PROXY_MODE" = openresty ]; then
      ha_plan "检查：Debian/Ubuntu、python3、端口 8080/9443/51820 空闲、443 由 OpenResty 占用（复用模式，不装 nginx）、磁盘剩余 ≥ 库大小 × 1.2、PG 大版本与主节点一致、时间同步"
    else
      ha_plan "检查：Debian/Ubuntu、python3、端口 8080/9443/51820 空闲、443 未被节点入站占用（被 OpenResty 占用时自动切复用模式）、磁盘剩余 ≥ 库大小 × 1.2、PG 大版本与主节点一致、时间同步"
    fi
  else
    command -v python3 >/dev/null || { pkg_install python3 || die "python3 安装失败"; }
    if ! installed; then
      port_busy 8080 && die "$(port_msg 8080)：请先腾出面板端口"
      port_busy 9443 && die "$(port_msg 9443)：请先腾出节点接入端口"
    fi
    if command -v ss >/dev/null && ! wg show wg0 >/dev/null 2>&1 && ss -Huln "sport = :$HA_WG_PORT" 2>/dev/null | grep -q .; then
      die "端口 $HA_WG_PORT/udp 已被占用"
    fi
    ha_proxy_mode_resolve
    if command -v timedatectl >/dev/null; then
      [ "$(timedatectl show -p NTPSynchronized --value 2>/dev/null)" = yes ] || warn "时间同步未开启（timedatectl set-ntp true）；租约时间以库为准，偏一点没事，别差出分钟级"
    fi
    ok "前置检查通过"
  fi

  step "安装 WireGuard 并生成密钥"
  if ! command -v wg >/dev/null; then
    if [ "$HA_DRYRUN" = 1 ]; then ha_plan "apt install -y wireguard"; else pkg_install wireguard || die "wireguard 安装失败"; fi
  fi
  ha_wg_keypair
  [ "$HA_DRYRUN" = 1 ] || ok "WireGuard 密钥就绪"

  step "取配置包并注册对端"
  # The token authorises both calls; the GET comes first because the POST
  # consumes the token (a failed fetch must not burn it).
  local bundle_json my_ip
  if [ "$HA_DRYRUN" = 1 ]; then
    ha_plan "GET $HA_API_URL/ha/join-config（Bearer <加入TOKEN>，校验证书，不提供 --insecure）"
    ha_plan "检查：磁盘剩余 ≥ 库大小 × 1.2（PG 大版本不同时，搭从库那步装主库版本）"
    ha_plan "POST $HA_API_URL/ha/peers（wg_ip=10.8.0.2，公钥，endpoint=<本机公网IP>）"
    B_PANEL_VERSION="v0.1.258"; B_EDITION="pro"; B_DOMAIN="polaris.cuijianzhuang.com"
    B_SECRET_KEY="<SECRET_KEY>"; B_SECRET_KEY_PREVIOUS=""; B_ADMIN_PATH="/"
    B_TIMEZONE="Asia/Shanghai"; B_DB_DSN="postgres://polaris:********@10.8.0.1:5432/polaris"
    B_REDIS_URL="redis://:********@10.8.0.1:6379/0"; B_PG_VERSION="<主库PG大版本>"
    B_PG_COLLATE="<主库collation>"
    B_REPL_PASSWORD="********"; B_DB_SIZE_BYTES=""
    B_WG_PUBKEY="<主节点WG公钥>"; B_WG_IP="10.8.0.1"; B_WG_ENDPOINT="<主节点公网IP>"
    B_WG_IP_PEER="10.8.0.2"; B_CF_TOKEN=""
  else
    bundle_json="$(curl -fsS --max-time 15 -H "Authorization: Bearer $HA_TOKEN" "$HA_API_URL/ha/join-config")" || die "取配置包失败：请检查 token 是否过期（10 分钟）、主节点是否可达，以及 panel-url 是否带上了管理路径"
    ha_parse_bundle "$bundle_json"
    # The primary assigns the standby's WireGuard address (10.8.0.2 unless
    # the operator set ha_peer_wg_ip on the primary); honour it.
    HA_WG_SECONDARY_IP="${B_WG_IP_PEER:-$HA_WG_SECONDARY_IP}"
    # A different local PostgreSQL major is not fatal: ha_setup_pg_replica
    # installs the primary's major (PGDG) and builds the replica in that
    # version's cluster.
    if printf '%s' "$B_DB_SIZE_BYTES" | grep -qE '^[0-9]+$' && [ "$B_DB_SIZE_BYTES" -gt 0 ]; then
      local free_kb need_kb
      free_kb="$(df -k / 2>/dev/null | awk 'NR==2{print $4}')" || free_kb=""
      need_kb=$((B_DB_SIZE_BYTES * 6 / 5 / 1024 + 102400))
      if [ -n "$free_kb" ] && [ "$free_kb" -lt "$need_kb" ]; then
        die "磁盘空间不足：数据库约 $(human_bytes "$B_DB_SIZE_BYTES")，需要至少 $(human_bytes "$((need_kb * 1024))")，/ 剩余 $(human_bytes "$((free_kb * 1024))")"
      fi
    fi
    my_ip="$(public_ip)"
    [ -n "$my_ip" ] || die "无法探测本机公网 IP"
    ha_register_peer "$my_ip"
  fi

  step "写隧道配置并验证"
  ha_wg_write_conf "$HA_WG_SECONDARY_IP" "$B_WG_PUBKEY" "$B_WG_IP" "$B_WG_ENDPOINT"
  ha_wg_start
  ha_wg_ping "$HA_WG_PRIMARY_IP"

  step "搭建 PostgreSQL 从库"
  if [ "$HA_DRYRUN" = 1 ]; then
    ha_plan "装 PostgreSQL ${B_PG_VERSION}（未装或大版本不同时走 PGDG；从库用该版本集群，端口取主库配置包）"
    ha_ensure_locale "${B_PG_COLLATE:-}"
    ha_plan "本机已是从库且真的在恢复模式时跳过重建；只有残留的 standby.signal 则清空重建"
    ha_plan "停 postgresql，清空本机空数据目录，pg_basebackup -h 10.8.0.1 -U replicator -S hk_slot"
    ha_plan ".pgpass 留在备机（0600，postgres 所有）：从库重连靠它"
    ha_plan "postgresql.conf：listen_addresses='localhost,10.8.0.2' hot_standby=on port=$(ha_dsn_port "$B_DB_DSN")；启动后验证 pg_is_in_recovery()=t"
  else
    ha_setup_pg_replica
  fi

  step "配置 Redis 从库"
  if ! command -v redis-server >/dev/null; then
    if [ "$HA_DRYRUN" = 1 ]; then ha_plan "apt install -y redis-server"; else pkg_install redis-server || die "Redis 安装失败"; fi
  fi
  if [ "$HA_DRYRUN" = 1 ]; then
    ha_plan "redis.conf：bind 127.0.0.1 10.8.0.2，requirepass ********，masterauth ********，replicaof 10.8.0.1 6379"
    ha_plan "systemctl restart redis-server；info replication 应为 role:slave，master_link_status:up"
  else
    [ -f /etc/redis/redis.conf ] || die "找不到 /etc/redis/redis.conf"
    local redis_pass
    redis_pass="$(ha_url_password "$B_REDIS_URL")"
    [ -n "$redis_pass" ] || die "配置包里的 redis_url 没有密码"
    ha_conf_set /etc/redis/redis.conf bind "127.0.0.1 10.8.0.2" " "
    ha_conf_set /etc/redis/redis.conf protected-mode yes " "
    ha_conf_set /etc/redis/redis.conf requirepass "$redis_pass" " "
    ha_conf_set /etc/redis/redis.conf masterauth "$redis_pass" " "
    ha_conf_set /etc/redis/redis.conf replicaof "10.8.0.1 6379" " "
    ha_conf_set /etc/redis/redis.conf appendonly yes " "
    systemctl restart redis-server 2>/dev/null || systemctl restart redis 2>/dev/null || die "Redis 重启失败"
    if redis-cli -h 10.8.0.2 -a "$redis_pass" info replication 2>/dev/null | grep -q '^role:slave'; then
      ok "Redis 从库就绪"
    else
      die "Redis 未成为从库（info replication 查看）"
    fi
  fi

  step "安装面板（同版本、同库、同 Redis）"
  if installed; then
    ok "面板已安装，跳过安装（继续同步配置）"
  elif [ "$HA_DRYRUN" = 1 ]; then
    ha_plan "install --pro --version $B_PANEL_VERSION --domain $B_DOMAIN --no-caddy --db 'postgres://polaris:********@10.8.0.1:5432/polaris' --redis 'redis://:********@10.8.0.1:6379/0' --port 8080 --agent-port 9443"
    ha_plan "密钥用配置包的 SECRET_KEY（不重新生成），管理路径用配置包的 ADMIN_PATH；跳过全新库检查与首次登录信息打印"
  else
    EDITION="$B_EDITION"; VERSION="$B_PANEL_VERSION"; DOMAIN="$B_DOMAIN"; CADDY=0
    DB_CHOICE="$B_DB_DSN"; REDIS_CHOICE="$B_REDIS_URL"; PORT=8080; AGENT_PORT=9443
    ADMIN_PATH="$B_ADMIN_PATH"; SECRET_OVERRIDE="$B_SECRET_KEY"; HA_JOIN=1
    do_install
  fi

  step "同步密钥与配置"
  if [ "$HA_DRYRUN" = 1 ]; then
    ha_plan "POLARIS_SECRET_KEY=<配置包的值>，POLARIS_SECRET_KEY_PREVIOUS=<配置包的值>"
    ha_plan "POLARIS_DB_DSN、POLARIS_REDIS_URL、POLARIS_ADMIN_PATH、POLARIS_PUBLIC_URL 按配置包改好"
    ha_plan "POLARIS_TIMEZONE=${B_TIMEZONE}，POLARIS_HA_ROLE=secondary，POLARIS_HA_WG_IP=10.8.0.2，POLARIS_HA_ENDPOINT=<本机公网IP>"
    ha_plan "POLARIS_HA_PROXY_MODE=${HA_PROXY_MODE}（反代模式：nginx 或 openresty）"
    ha_plan "systemctl restart polaris，wait_healthy 确认"
  else
    env_set POLARIS_SECRET_KEY "$B_SECRET_KEY"
    env_set POLARIS_SECRET_KEY_PREVIOUS "$B_SECRET_KEY_PREVIOUS"
    env_set POLARIS_DB_DSN "$B_DB_DSN"
    env_set POLARIS_REDIS_URL "$B_REDIS_URL"
    env_set POLARIS_ADMIN_PATH "$B_ADMIN_PATH"
    env_set POLARIS_PUBLIC_URL "$B_PUBLIC_URL"
    env_set POLARIS_TIMEZONE "$B_TIMEZONE"
    env_set POLARIS_HA_ROLE secondary
    env_set POLARIS_HA_PROXY_MODE "$HA_PROXY_MODE"
    env_set POLARIS_HA_WG_PUBKEY "$WG_PUB"
    env_set POLARIS_HA_WG_IP "$HA_WG_SECONDARY_IP"
    local my_ip
    my_ip="$(public_ip)"
    [ -n "$my_ip" ] || die "无法探测本机公网 IP"
    env_set POLARIS_HA_ENDPOINT "$my_ip"
    env_set POLARIS_HA_PEER_WG_IP "$HA_WG_PRIMARY_IP"
    systemctl restart polaris
    wait_healthy || die "面板重启后未恢复（polaris log 查看）"
    ok "面板已同步配置并重启"
  fi

  if [ "$HA_PROXY_MODE" = openresty ]; then step "复用 OpenResty 反代"; else step "配置 nginx 与证书"; fi
  ha_setup_nginx_cert

  step "安装看门狗与备份同步"
  ha_setup_watchdog

  ha_install_heartbeat hk

  step "自检"
  ha_self_check

  echo
  ok "${C_BD}备节点加入完成${C_0}（用时 $(human_dur "$SECONDS")）"
  echo
  if [ "$HA_PROXY_MODE" = openresty ]; then
    dim "  复用 OpenResty：记得在 1Panel 里给 $B_DOMAIN 建站并加上面的反代（443→8080、stream 9443），再用 --resolve 验一次"
  fi
  dim "  主节点上：wg show 应看到备机对端；pg_stat_replication 应有 streaming 连接"
}

# do_ha_finish completes a join that skipped the certificate and watchdog
# for lack of a Cloudflare token. The token comes from the environment now:
# the sealed copy on the primary cannot be read back from this machine.
do_ha_finish() {
  need_installed
  local role
  role="$(env_get POLARIS_HA_ROLE)"
  [ "$role" = secondary ] || die "ha-join --finish 只在备节点上执行（本机角色：${role:-none}）"
  # The proxy mode was decided by ha-join (persisted in the env file); an
  # explicit HA_PROXY_MODE wins over the persisted value. Joins from before
  # this mode existed have no value and default to nginx.
  local proxy_mode
  if [ "${HA_PROXY_MODE:-auto}" != auto ]; then proxy_mode="$HA_PROXY_MODE"; else proxy_mode="$(env_get POLARIS_HA_PROXY_MODE)"; fi
  [ -n "$proxy_mode" ] || proxy_mode=nginx
  case "$proxy_mode" in nginx|openresty) ;; *) die "反代模式非法：${proxy_mode}（nginx/openresty）" ;; esac
  SECONDS=0 STEP_N=0
  if [ "$proxy_mode" = openresty ]; then STEP_TOTAL=2; else STEP_TOTAL=3; fi
  if [ "$proxy_mode" = openresty ]; then banner "高可用：补做看门狗（复用 OpenResty）"; else banner "高可用：补做证书与看门狗"; fi
  local cf_token url domain primary_ip
  cf_token="${CF_API_TOKEN:-}"
  [ -n "$cf_token" ] || die "需要 CF_API_TOKEN：CF_API_TOKEN=<token> $0 ha-join --finish（token 需 Zone:DNS:Edit 权限）"
  url="$(env_get POLARIS_PUBLIC_URL)"
  domain="${url#https://}"; domain="${domain#http://}"; domain="${domain%%/*}"
  [ -n "$domain" ] || die "POLARIS_PUBLIC_URL 未配域名"
  primary_ip="${CF_PRIMARY_IP:-}"
  if [ -z "$primary_ip" ]; then
    primary_ip="$(getent hosts "$domain" | awk '{print $1}' | head -n1)" || primary_ip=""
  fi
  [ -n "$primary_ip" ] || die "无法确定主节点公网 IP：设置 CF_PRIMARY_IP 再重试"
  B_DOMAIN="$domain"; B_WG_ENDPOINT="$primary_ip"; B_CF_TOKEN="$cf_token"
  B_ADMIN_PATH="$(env_get POLARIS_ADMIN_PATH)"

  if [ "$proxy_mode" = openresty ]; then
    step "复用 OpenResty 反代"
    ha_openresty_guide "$domain"
  else
    step "申请证书"
    ha_certbot_dns "$cf_token" "$domain"

    step "启用 nginx"
    ha_nginx_enable "$domain"
  fi

  step "安装看门狗"
  ha_setup_watchdog

  echo
  if [ "$proxy_mode" = openresty ]; then
    ok "${C_BD}看门狗补做完成${C_0}（用时 $(human_dur "$SECONDS")；反代配置见上，证书由 1Panel 管理）"
  else
    ok "${C_BD}证书与看门狗补做完成${C_0}（用时 $(human_dur "$SECONDS")）"
  fi
}

do_ha_status() {
  banner "高可用状态"
  local role rurl rpass rhost
  role="$(env_get POLARIS_HA_ROLE)"; [ -n "$role" ] || role="none"
  kv 角色 "$role"
  if [ "$role" != none ]; then
    kv "WG 地址" "$(env_get POLARIS_HA_WG_IP)"
    kv "WG 公钥" "$(env_get POLARIS_HA_WG_PUBKEY)"
    kv "公网地址" "$(env_get POLARIS_HA_ENDPOINT)"
    kv "对端 WG" "$(env_get POLARIS_HA_PEER_WG_IP)"
  fi
  rurl="$(env_get POLARIS_REDIS_URL)"
  [ -n "$rurl" ] && kv Redis "$(printf '%s' "$rurl" | sed -E 's|://[^@]*@|://***@|')"
  echo
  if command -v wg >/dev/null; then
    echo "  WireGuard："
    wg show 2>/dev/null | sed 's/^/    /' || echo "    未启动"
  else
    echo "  WireGuard：未安装"
  fi
  echo "  PostgreSQL："
  if command -v psql >/dev/null && pg_su -tAc 'SELECT 1' >/dev/null 2>&1; then
    local rec
    rec="$(pg_su -tAc 'SELECT pg_is_in_recovery()' 2>/dev/null)" || rec=""
    case "$rec" in
      t) printf '    从库（恢复模式），回放落后：%s\n' "$(pg_su -tAc "SELECT now() - pg_last_xact_replay_timestamp()" 2>/dev/null)" ;;
      f) printf '    主库，复制连接：%s\n' "$(pg_su -tAc 'SELECT count(*) FROM pg_stat_replication' 2>/dev/null)" ;;
      *) echo "    状态未知" ;;
    esac
  else
    echo "    未运行或未安装"
  fi
  echo "  Redis："
  rpass="$(ha_url_password "$rurl")"; rhost="$(ha_url_host "$rurl")"
  if [ -n "$rpass" ] && [ -n "$rhost" ] && command -v redis-cli >/dev/null; then
    redis-cli -h "$rhost" -a "$rpass" info replication 2>/dev/null | grep -E '^(role|master_link_status|master_host):' | sed 's/^/    /' || echo "    连不上"
  else
    echo "    未配置"
  fi
  echo "  面板："
  if running; then printf '    运行中（%s %s）\n' "$(panel_version)" "$(installed_edition)"; else echo "    未运行"; fi
}

# do_witness_init installs the witness arbiter on the third host (Ubuntu):
# the panel binary (which contains `polaris witness serve`; community
# edition is enough — the witness has no edition-gated code), a systemd
# unit, and prints the firewall + verification commands. The witness has
# no database and no panel config; it only serves heartbeats and verdicts.
do_witness_init() {
  ha_require_apt
  local listen="${HA_WITNESS_LISTEN:-0.0.0.0:8443}"
  SECONDS=0 STEP_N=0 STEP_TOTAL=0
  banner "高可用：安装见证节点"
  kv 监听 "$listen"
  echo

  step "安装面板二进制（含 witness serve）"
  if [ "$HA_DRYRUN" = 1 ]; then
    ha_plan "下载最新版 panel 包（checksum 校验），装到 $BIN"
  else
    if [ -x "$BIN" ] && ha_cli_supports "witness serve"; then
      ok "面板二进制已安装且支持 witness serve（跳过下载）"
    else
      EDITION=community download_release
      install -m 0755 "$TMP/polaris" "$BIN"
      ha_cli_supports "witness serve" || die "下载的面板版本不支持 witness serve：请等含 PR5 的发版"
      ok "面板二进制已安装"
    fi
  fi

  step "写 systemd unit 并启动"
  if [ "$HA_DRYRUN" = 1 ]; then
    ha_plan "写 ${HA_WITNESS_UNIT}（ExecStart=$BIN witness serve --listen $listen --state-file ${HA_WITNESS_STATE}），systemctl enable --now polaris-witness"
    ha_plan "放行端口：ufw allow ${listen##*:}/tcp（或云厂商安全组放行 ${listen##*:}/tcp）"
  else
    mkdir -p "${HA_WITNESS_STATE%/*}"
    cat > "$HA_WITNESS_UNIT" <<UNIT
[Unit]
Description=Polaris HA witness arbiter
After=network-online.target

[Service]
Type=simple
ExecStart=$BIN witness serve --listen $listen --state-file $HA_WITNESS_STATE
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
UNIT
    systemctl daemon-reload
    systemctl enable --now polaris-witness >/dev/null 2>&1
    ok "见证服务已启动（${listen}）"
  fi

  step "验证"
  if [ "$HA_DRYRUN" = 1 ]; then
    ha_plan "curl http://127.0.0.1:${listen##*:}/health 应返回 {\"ok\":true}"
    ha_plan "dmit/HK 上用 WITNESS_URL=http://<见证IP>:${listen##*:} 重跑 ha-init/ha-join 补装心跳与围栏"
  else
    sleep 1
    curl -fsS --max-time 5 "http://127.0.0.1:${listen##*:}/health" >/dev/null || die "见证服务未响应（systemctl status polaris-witness 查看）"
    ok "见证服务正常"
  fi

  echo
  ok "${C_BD}见证节点安装完成${C_0}（用时 $(human_dur "$SECONDS")）"
  echo
  dim "  放行端口：ufw allow ${listen##*:}/tcp（或云厂商安全组）"
  dim "  验证：curl http://<见证IP>:${listen##*:}/health"
  dim "  dmit/HK：WITNESS_URL=http://<见证IP>:${listen##*:} ha-init/ha-join 补装心跳与围栏"
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
    rm -f "$f"; die "${msg:-下载 $url 失败（HTTP ${code}）}"
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
    ok "已卸载；数据保留在 ${data}（加 --purge 一并删除）"
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
    kv 后台 "${C_BD}$(admin_url)${C_0}"
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
    menu_row "4|卸载面板" "20|切换版本 [$(edition_label "$(installed_edition)")]"
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
    local c; c="$(ask "  请选择 [0-20]：")"
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
      20) ARG=""; run_action do_switch; pause ;;
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
        if [ -n "$nd" ] && confirm "将安装 / 配置 Caddy 并切换到 https://${nd}，旧的 http 订阅链接会失效，继续？"; then run_action do_set_domain "$nd"; fi
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
    ha-init|ha-status) ACTION="$1"; shift ;;
    pg-upgrade) ACTION=pg-upgrade; shift ;;
    witness-init) ACTION=witness-init; shift ;;
    ha-join) ACTION=ha-join; shift
      # Flags may come before the token (--dry-run / --finish in any order);
      # --finish takes no token: ha-join --finish [--dry-run]
      while [ $# -gt 0 ]; do
        case "$1" in
          --dry-run) HA_DRYRUN=1; shift ;;
          --finish) HA_FINISH=1; shift ;;
          *) break ;;
        esac
      done
      if [ "$HA_FINISH" = 1 ]; then :
      else
        [ $# -ge 2 ] || die "用法：$0 ha-join <token> <panel-url> [--dry-run]（panel-url 带上管理路径，如 https://域名/管理路径）；补做证书与看门狗用：$0 ha-join --finish [--dry-run]"
        HA_TOKEN="$1"; HA_PANEL_URL="$2"; shift 2
      fi ;;
    --dry-run) HA_DRYRUN=1; shift ;;
    --finish) HA_FINISH=1; shift ;;
    --endpoint) HA_ENDPOINT="$2"; shift 2 ;;
    --to) PG_UPGRADE_TO="$2"; shift 2 ;;
    --listen) HA_WITNESS_LISTEN="$2"; shift 2 ;;
    self-update) ACTION=self-update; shift ;;
    update-request) ACTION=update-request; shift ;;
    agent) ACTION=agent; shift; AGENT_SUB="${1:-menu}"; [ $# -gt 0 ] && shift; AGENT_ARG="${1:-}"; [ $# -gt 0 ] && shift ;;
    join) ACTION=agent; AGENT_SUB="join"; AGENT_ARG="${2:-}"; shift; [ $# -gt 0 ] && shift ;;
    --force) FORCE=1; shift ;;
    -n) NODE_NAME="$2"; shift 2 ;;
    set-path|set-port|set-domain|set-url|set-sub-domain|restore|migrate) ACTION="$1"; [ $# -ge 2 ] || die "$1 需要一个参数"; ARG="$2"; shift 2 ;;
    switch) ACTION=switch; shift; if [ $# -gt 0 ] && [ "${1#-}" = "$1" ]; then ARG="$1"; shift; fi ;;
    --passphrase) BK_PASS="$2"; shift 2 ;;
    --restore) RESTORE_FILE="$2"; shift 2 ;;
    --db) DB_CHOICE="$2"; shift 2 ;;
    --redis) REDIS_CHOICE="$2"; shift 2 ;;
    --domain) DOMAIN="$2"; shift 2 ;;
    --no-caddy) CADDY=0; shift ;;
    --pro) EDITION=pro; shift ;;
    --community) EDITION=community; shift ;;
    --version) if [ $# -ge 2 ] && [ "${2#-}" = "$2" ]; then VERSION="$2"; shift 2; else ACTION=version; shift; fi ;;
    version|-v) ACTION=version; shift ;;
    help) sed -n '2,35p' "$CTL" 2>/dev/null || sed -n '2,35p' "$0"; exit 0 ;;
    --port) PORT="$2"; shift 2 ;;
    --agent-port) AGENT_PORT="$2"; shift 2 ;;
    --admin-path) ADMIN_PATH="$2"; shift 2 ;;
    --mirror) MIRROR="$2"; shift 2 ;;
    --purge) PURGE=1; shift ;;
    -u) ADMIN_USER="$2"; shift 2 ;;
    -h|--help) sed -n '2,35p' "$0" 2>/dev/null || echo "see $RAW_SCRIPT"; exit 0 ;;
    ha-token|ha-seal-secret|ha|witness)
      # Panel-binary maintenance commands. This tool (polaris) is the
      # management script, not the panel binary (polaris-server), so
      # forward them instead of rejecting them as unknown. The binary
      # reads its config from the panel env file (the sync-wg timer
      # does the same via EnvironmentFile), so load it first; without
      # it the binary dies on "secret_key is required".
      [ -x "$BIN" ] || die "面板尚未安装（$BIN 不存在）"
      [ -f "$ENV_FILE" ] || die "找不到面板配置文件（${ENV_FILE}）"
      set -a
      # shellcheck source=/dev/null
      . "$ENV_FILE"
      set +a
      exec "$BIN" "$@" ;;
    *) die "未知参数：$1（--help 查看用法）" ;;
  esac
done

# --mirror none forgets a saved mirror; otherwise reuse the one that last worked.
if [ "$MIRROR" = none ]; then MIRROR=""; rm -f "$MIRROR_FILE" 2>/dev/null || true
elif [ -z "$MIRROR" ] && [ -s "$MIRROR_FILE" ]; then MIRROR="$(head -n1 "$MIRROR_FILE")"; fi
# A mirror relays release bytes from an untrusted third party, so plain http
# would let anyone on the path swap them: only https is accepted.
case "$MIRROR" in
  ""|https://*) ;;
  *) die "镜像必须使用 https://（收到 ${MIRROR}）。镜像仅加速软件包下载，校验和始终从 GitHub 官方获取。" ;;
esac

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
  migrate) do_migrate ;;
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
  switch) do_switch ;;
  reset-password) do_reset_password ;;
  reset-2fa) do_reset_2fa ;;
  set-path) do_set_path "$ARG" ;;
  set-port) do_set_port "$ARG" ;;
  set-domain) do_set_domain "$ARG" ;;
  set-url) do_set_url "$ARG" ;;
  set-sub-domain) do_set_sub_domain "$ARG" ;;
  backup) do_backup ;;
  restore) do_restore "$ARG" ;;
  node) do_node ;;
  ha-init) do_ha_init ;;
  pg-upgrade) do_pg_upgrade ;;
  ha-join) do_ha_join ;;
  ha-status) do_ha_status ;;
  witness-init) do_witness_init ;;
esac
