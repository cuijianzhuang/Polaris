#!/usr/bin/env bash
# Polaris panel installer and management tool.
#
# Install (then manage any time with: polaris-ctl):
#   bash <(curl -fsSL https://raw.githubusercontent.com/cuijianzhuang/Polaris/main/scripts/install-panel.sh)
#
# With no arguments an interactive menu opens. Non-interactive commands:
#   install [--domain NAME] [--no-caddy] [--version TAG] [--port N] [--agent-port N] [--admin-path P]
#   update [--version TAG]        upgrade binaries, keep config and data (database backed up first)
#   uninstall [--purge]           remove; --purge also deletes config and data
#   start | stop | restart | status | log
#   info                          admin URL, ports and config summary
#   reset-password [-u NAME]      new random admin password, unlocks the account, signs out sessions
#   reset-2fa [-u NAME]           turn off two-factor authentication
#   set-path PATH | set-port N    change the admin path / panel port
#   set-domain NAME               switch to HTTPS on a domain via Caddy
#   backup                        config + consistent database copy to /root/polaris-backup-*.tar.gz
#
# Options for install/update: --mirror URL (download prefix for github.com)
set -euo pipefail

REPO="cuijianzhuang/Polaris"
BIN=/usr/local/bin/polaris
CTL=/usr/local/bin/polaris-ctl
ETC=/etc/polaris
ENV_FILE=$ETC/polaris.env
DATA=/var/lib/polaris
UNIT=/etc/systemd/system/polaris.service
CADDYFILE=/etc/caddy/Caddyfile
RAW_SCRIPT="https://raw.githubusercontent.com/$REPO/main/scripts/install-panel.sh"

DOMAIN="" CADDY=1 VERSION="" PORT=8080 AGENT_PORT=9443 ADMIN_PATH="" MIRROR="" PURGE=0 ADMIN_USER=""

red() { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
yellow() { printf '\033[33m%s\033[0m\n' "$*"; }
die() { red "错误：$*" >&2; exit 1; }
step() { printf '\033[36m==>\033[0m %s\n' "$*"; }

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

# polaris_cmd runs a panel maintenance subcommand as the service user with
# the service's environment (so SQLite files keep the right owner).
polaris_cmd() {
  # shellcheck source=/dev/null
  (cd "$DATA" && set -a && . "$ENV_FILE" && set +a && runuser -u polaris -- "$BIN" "$@")
}

wait_active() {
  local _
  for _ in $(seq 1 15); do running && return 0; sleep 1; done
  journalctl -u polaris -n 30 --no-pager
  return 1
}

# --- download ------------------------------------------------------------------

gh() { printf '%s%s' "$MIRROR" "$1"; }
fetch() { curl -fsSL --retry 3 --connect-timeout 15 -o "$2" "$1"; }

resolve_version() {
  if [ -z "$VERSION" ]; then
    step "查询最新版本"
    # /releases/latest skips pre-releases, so take the newest entry of the list.
    VERSION="$(curl -fsSL --retry 3 "https://api.github.com/repos/$REPO/releases?per_page=1" |
      sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n1 || true)"
    [ -n "$VERSION" ] || die "无法获取版本号（GitHub API 不可达？可用 --version 指定）"
  fi
  RELEASE="${POLARIS_RELEASE_URL:-$(gh "https://github.com/$REPO/releases/download/$VERSION")}"
}

# download_release fetches and verifies the panel, both agents and this script.
download_release() {
  resolve_version
  TMP="$(mktemp -d)"
  trap 'rm -rf "$TMP"' EXIT
  step "下载 Polaris $VERSION ($ARCH)"
  PANEL_TGZ="polaris-panel-$VERSION-linux-$ARCH.tar.gz"
  fetch "$RELEASE/checksums.txt" "$TMP/checksums.txt" || die "下载 checksums.txt 失败：$RELEASE"
  local f want got
  for f in "$PANEL_TGZ" polaris-agent-linux-amd64 polaris-agent-linux-arm64 install-panel.sh; do
    if ! fetch "$RELEASE/$f" "$TMP/$f"; then
      [ "$f" = install-panel.sh ] && { rm -f "$TMP/$f"; continue; } # older releases do not ship it
      die "下载 $f 失败"
    fi
    want="$(awk -v f="$f" '$2 == f || $2 == "*"f {print $1}' "$TMP/checksums.txt")"
    got="$(sha256sum "$TMP/$f" | awk '{print $1}')"
    [ -n "$want" ] && [ "$want" = "$got" ] || die "$f 校验失败（期望 ${want:-无}，实际 $got）"
  done
  green "SHA-256 校验通过"
  tar -xzf "$TMP/$PANEL_TGZ" -C "$TMP"
}

install_files() {
  install -m 0755 "$TMP/polaris" "$BIN"
  # Agent binaries served at /download/ so node installs pull them from this panel.
  install -d -m 0755 -o polaris -g polaris "$DATA" "$DATA/downloads"
  install -m 0755 -o polaris -g polaris "$TMP/polaris-agent-linux-amd64" "$TMP/polaris-agent-linux-arm64" "$DATA/downloads/"
  install_ctl
}

# install_ctl puts this tool at /usr/local/bin/polaris-ctl.
install_ctl() {
  if [ -f "${TMP:-}/install-panel.sh" ]; then
    install -m 0755 "$TMP/install-panel.sh" "$CTL"
  elif [ -f "$0" ] && [ "$(readlink -f "$0")" != "$CTL" ]; then
    install -m 0755 "$0" "$CTL"
  elif [ ! -f "$CTL" ]; then
    fetch "$RAW_SCRIPT" "$CTL" && chmod 0755 "$CTL" || yellow "未能安装 polaris-ctl 命令"
  fi
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
valid_domain() { [[ "$1" =~ ^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)+$ ]]; }

install_caddy() {
  command -v caddy >/dev/null && return 0
  step "安装 Caddy（自动申请 HTTPS 证书）"
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

show_info() {
  need_installed
  local url path ver listen agent
  url="$(env_get POLARIS_PUBLIC_URL)"; path="$(env_get POLARIS_ADMIN_PATH)"
  listen="$(env_get POLARIS_LISTEN)"; agent="$(env_get POLARIS_AGENT_LISTEN)"
  ver="$("$BIN" --version 2>/dev/null | awk '{print $2}')"
  echo "  版本：      ${ver:-未知}"
  if running; then echo "  状态：      $(green 运行中)"; else echo "  状态：      $(red 已停止)"; fi
  if systemctl is-enabled --quiet polaris 2>/dev/null; then echo "  开机自启：  是"; else echo "  开机自启：  否"; fi
  echo "  后台地址：  $url$path/"
  echo "  面板监听：  $listen"
  echo "  节点接入：  ${agent}（mTLS，需在防火墙放行）"
  echo "  配置文件：  $ENV_FILE"
  echo "  数据目录：  $DATA"
}

# --- actions -------------------------------------------------------------------

do_install() {
  installed && die "已安装：升级用 update，重装请先 uninstall"
  [ -f "$ENV_FILE" ] && die "$ENV_FILE 已存在：升级用 update，重装请先 uninstall"
  [ -z "$DOMAIN" ] || valid_domain "$DOMAIN" || die "域名格式不正确：$DOMAIN"
  valid_port "$PORT" && valid_port "$AGENT_PORT" || die "端口不正确"
  download_release

  local host listen public_url secret use_caddy=0 pass=""
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
    [ "$CADDY" = 1 ] && use_caddy=1
    listen="127.0.0.1:$PORT"; public_url="https://$DOMAIN"
  else
    listen=":$PORT"; public_url="http://$host:$PORT"
  fi

  step "创建用户与目录"
  id polaris >/dev/null 2>&1 || useradd --system --home-dir "$DATA" --shell /usr/sbin/nologin polaris
  install -d -m 0750 -o root -g polaris "$ETC"
  install_files
  cat > "$ENV_FILE" <<EOF
# Generated by polaris-ctl on $(date -u +%Y-%m-%dT%H:%M:%SZ)
# 务必备份 POLARIS_SECRET_KEY：丢失后已加密的数据无法解密
POLARIS_SECRET_KEY=$secret
POLARIS_ADMIN_PATH=$ADMIN_PATH
POLARIS_PUBLIC_URL=$public_url
POLARIS_LISTEN=$listen
POLARIS_AGENT_LISTEN=:$AGENT_PORT
POLARIS_AGENT_HOSTS=$host
POLARIS_DATA_DIR=$DATA
POLARIS_INITIAL_ADMIN_USERNAME=admin
EOF
  chmod 0640 "$ENV_FILE"; chown root:polaris "$ENV_FILE"
  install -m 0644 "$TMP/polaris.service" "$UNIT"

  if [ "$use_caddy" = 1 ]; then
    install_caddy
    step "配置 Caddy 站点 $DOMAIN"
    caddy_site "$DOMAIN" "$PORT"
  fi

  step "启动 Polaris"
  systemctl daemon-reload
  systemctl enable --now polaris >/dev/null 2>&1
  local _
  for _ in $(seq 1 30); do
    pass="$(journalctl -u polaris --no-pager -o cat 2>/dev/null | sed -n 's/.*created initial admin.* password=\([^ ]*\).*/\1/p' | tail -n1)"
    [ -n "$pass" ] && break
    running || break
    sleep 1
  done
  running || { journalctl -u polaris -n 40 --no-pager; die "Polaris 启动失败"; }

  echo
  green "Polaris $VERSION 安装完成"
  show_info
  echo "  用户名：    admin"
  echo "  初始密码：  ${pass:-（未能从日志读取，可执行 polaris-ctl reset-password 重新生成）}"
  echo
  yellow "请在防火墙放行："
  if [ "$use_caddy" = 1 ]; then echo "  80/tcp、443/tcp（HTTPS 与证书申请）"; elif [ -z "$DOMAIN" ]; then echo "  $PORT/tcp（后台与订阅）"; fi
  echo "  $AGENT_PORT/tcp（节点 Agent 接入，mTLS，不能经过反代）"
  [ -z "$DOMAIN" ] && { echo; yellow "当前为 HTTP 明文访问，正式使用建议绑定域名：polaris-ctl set-domain panel.example.com"; }
  [ -n "$DOMAIN" ] && [ "$CADDY" = 0 ] && { echo; yellow "未安装 Caddy：请让你的反向代理把 https://$DOMAIN 转发到 http://127.0.0.1:$PORT"; }
  echo
  echo "登录后：修改密码 → 开启二次验证 → 「节点与内核」添加节点"
  echo "管理命令：polaris-ctl（打开菜单）"
}

do_update() {
  need_installed
  download_release
  local cur backup=""
  cur="$("$BIN" --version 2>/dev/null || echo unknown)"
  step "升级 $cur -> $VERSION（配置与数据保持不变）"
  if ls "$DATA"/*.db >/dev/null 2>&1; then
    backup="$DATA/backup-$(date +%Y%m%d%H%M%S)"
  fi
  systemctl stop polaris
  if [ -n "$backup" ]; then
    mkdir -p "$backup" && cp -a "$DATA"/*.db* "$backup"/ && echo "数据库已备份到 $backup"
  fi
  install_files
  install -m 0644 "$TMP/polaris.service" "$UNIT"
  systemctl daemon-reload
  systemctl start polaris
  wait_active || die "启动失败${backup:+，升级前的数据库备份在 $backup}"
  green "已升级到 $("$BIN" --version)"
}

do_uninstall() {
  step "停止并移除 polaris 服务"
  systemctl disable --now polaris 2>/dev/null || true
  rm -f "$UNIT" "$BIN"
  systemctl daemon-reload
  if [ "$PURGE" = 1 ]; then
    yellow "删除配置与数据：$ETC $DATA"
    rm -rf "$ETC" "$DATA"
    id polaris >/dev/null 2>&1 && userdel polaris 2>/dev/null || true
    rm -f "$CTL"
  else
    echo "已保留配置 $ETC 与数据 $DATA（加 --purge 一并删除）"
  fi
  green "已卸载"
}

do_reset_password() {
  need_installed
  local args=(admin reset-password)
  [ -n "$ADMIN_USER" ] && args+=(-u "$ADMIN_USER")
  polaris_cmd "${args[@]}" || die "重置失败"
  yellow "已解除登录锁定，所有已登录会话已失效"
}

do_reset_2fa() {
  need_installed
  local args=(admin reset-2fa)
  [ -n "$ADMIN_USER" ] && args+=(-u "$ADMIN_USER")
  polaris_cmd "${args[@]}" || die "操作失败"
}

do_set_path() {
  need_installed
  local p="/${1#/}"
  [[ "$p" =~ ^/[A-Za-z0-9_-]{4,64}$ ]] || die "路径只能包含字母、数字、- 和 _，长度 4–64"
  env_set POLARIS_ADMIN_PATH "$p"
  systemctl restart polaris && wait_active || die "重启失败"
  green "后台地址：$(env_get POLARIS_PUBLIC_URL)$p/"
}

do_set_port() {
  need_installed
  local port="$1" listen url domain
  valid_port "$port" || die "端口不正确"
  listen="$(env_get POLARIS_LISTEN)"
  env_set POLARIS_LISTEN "${listen%:*}:$port"
  url="$(env_get POLARIS_PUBLIC_URL)"
  domain="$(panel_domain)"
  if [ -n "$domain" ]; then
    if [ -f "$CADDYFILE" ] && grep -q "^$domain {" "$CADDYFILE"; then caddy_site "$domain" "$port"; else
      yellow "HTTPS 由你自己的反向代理提供：记得把上游改为 127.0.0.1:$port"
    fi
  else
    case "$url" in http://*:*) env_set POLARIS_PUBLIC_URL "${url%:*}:$port" ;; esac
    yellow "订阅链接中的端口已变化，用户需要重新导入订阅；记得在防火墙放行 $port/tcp"
  fi
  systemctl restart polaris && wait_active || die "重启失败"
  green "面板端口已改为 $port"
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
  green "已切换到 https://$domain$(env_get POLARIS_ADMIN_PATH)/"
  yellow "订阅地址已变为 https://$domain/…，旧的 http 订阅链接失效，用户需要重新导入；可在防火墙关闭 $port/tcp，放行 80、443"
}

do_backup() {
  need_installed
  local ts out tmpdb
  ts="$(date +%Y%m%d-%H%M%S)"
  out="/root/polaris-backup-$ts.tar.gz"
  tmpdb="$DATA/backup-$ts.db"
  if [ -n "$(env_get POLARIS_DB_DSN)" ] && env_get POLARIS_DB_DSN | grep -q '^postgres'; then
    yellow "使用 PostgreSQL：请另行用 pg_dump 备份数据库，这里只备份配置"
    tar -czf "$out" -C / "${ETC#/}"
  else
    polaris_cmd backup "$tmpdb" >/dev/null || die "数据库备份失败"
    tar -czf "$out" -C / "${ETC#/}" -C "$DATA" "$(basename "$tmpdb")"
    rm -f "$tmpdb"
  fi
  chmod 600 "$out"
  green "已备份到 $out"
  yellow "备份包含加密密钥，请妥善保管"
}

do_log() {
  trap 'echo' INT
  journalctl -u polaris -n 50 -f --no-pager || true
  trap - INT
}

# --- interactive menu --------------------------------------------------------

pause() { echo; read -r -p "按回车返回菜单…" _ || true; }
ask() { local v; read -r -p "$1" v || true; echo "$v"; }
confirm() { local v; read -r -p "$1 [y/N] " v || true; [[ "$v" =~ ^[Yy]$ ]]; }

# run_action runs an action in a subshell with errexit on. It must not be
# called as part of && / || lists: bash ignores set -e in such contexts.
run_action() { ( set -e; "$@" ); }

menu() {
  set +e # a failed menu step must not exit the menu
  while true; do
    clear 2>/dev/null || true
    printf '\033[36m%s\033[0m\n' "Polaris 面板管理"
    echo "──────────────────────────────────────────"
    if installed; then
      local v; v="$("$BIN" --version 2>/dev/null | awk '{print $2}')"
      if running; then echo "  状态：$(green 运行中)   版本：$v"; else echo "  状态：$(red 已停止)   版本：$v"; fi
      echo "  后台：$(env_get POLARIS_PUBLIC_URL)$(env_get POLARIS_ADMIN_PATH)/"
    else
      echo "  状态：$(yellow 未安装)"
    fi
    echo "──────────────────────────────────────────"
    echo "   1. 安装              2. 升级              3. 卸载"
    echo "   4. 启动              5. 停止              6. 重启"
    echo "   7. 运行状态          8. 实时日志          9. 查看后台地址与配置"
    echo "  10. 重置管理员密码   11. 关闭二次验证"
    echo "  12. 修改后台路径     13. 修改面板端口     14. 绑定域名（HTTPS）"
    echo "  15. 备份配置与数据   16. 开机自启开关"
    echo "   0. 退出"
    echo "──────────────────────────────────────────"
    local c; c="$(ask "请选择 [0-16]：")"
    echo
    case "$c" in
      1)
        DOMAIN="$(ask "域名（已解析到本机，留空则使用 IP + HTTP）：")"
        if [ -n "$DOMAIN" ] && ! confirm "自动安装 Caddy 申请 HTTPS 证书？（选 N 表示你自备反向代理）"; then CADDY=0; else CADDY=1; fi
        local p; p="$(ask "面板端口 [8080]：")"; PORT="${p:-8080}"
        p="$(ask "节点接入端口 [9443]：")"; AGENT_PORT="${p:-9443}"
        run_action do_install; pause ;;
      2) run_action do_update; pause ;;
      3)
        if confirm "确定卸载 Polaris？"; then
          confirm "同时删除配置与全部数据（不可恢复）？" && PURGE=1
          run_action do_uninstall; PURGE=0
        fi
        pause ;;
      4) need_installed_soft && { systemctl start polaris && wait_active && green "已启动"; }; pause ;;
      5) need_installed_soft && { systemctl stop polaris && green "已停止"; }; pause ;;
      6) need_installed_soft && { systemctl restart polaris && wait_active && green "已重启"; }; pause ;;
      7) need_installed_soft && systemctl status polaris --no-pager -l | head -n 20; pause ;;
      8) need_installed_soft && { echo "按 Ctrl+C 返回菜单"; do_log; }; ;;
      9) (show_info) || true; pause ;;
      10)
        ADMIN_USER="$(ask "管理员用户名（留空为第一个管理员）：")"
        run_action do_reset_password; ADMIN_USER=""; pause ;;
      11)
        ADMIN_USER="$(ask "管理员用户名（留空为第一个管理员）：")"
        if confirm "关闭该管理员的二次验证？"; then run_action do_reset_2fa; fi
        ADMIN_USER=""; pause ;;
      12)
        local np; np="$(ask "新的后台路径（留空随机生成）：")"
        run_action do_set_path "${np:-$(rand 12)}"; pause ;;
      13)
        local nport; nport="$(ask "新的面板端口：")"
        if [ -n "$nport" ]; then run_action do_set_port "$nport"; fi; pause ;;
      14)
        local nd; nd="$(ask "域名（需已解析到本机）：")"
        if [ -n "$nd" ] && confirm "将安装 / 配置 Caddy 并切换到 https://$nd，旧的 http 订阅链接会失效，继续？"; then run_action do_set_domain "$nd"; fi
        pause ;;
      15) run_action do_backup; pause ;;
      16)
        if need_installed_soft; then
          if systemctl is-enabled --quiet polaris; then systemctl disable polaris >/dev/null 2>&1 && green "已关闭开机自启"
          else systemctl enable polaris >/dev/null 2>&1 && green "已开启开机自启"; fi
        fi
        pause ;;
      0|q|Q) exit 0 ;;
      *) ;;
    esac
  done
}
need_installed_soft() { installed || { red "Polaris 尚未安装"; return 1; }; }

# --- entry point ----------------------------------------------------------------

ACTION=""
while [ $# -gt 0 ]; do
  case "$1" in
    install|update|uninstall|start|stop|restart|status|log|info|reset-password|reset-2fa|backup|menu) ACTION="$1"; shift ;;
    set-path|set-port|set-domain) ACTION="$1"; [ $# -ge 2 ] || die "$1 需要一个参数"; ARG="$2"; shift 2 ;;
    --domain) DOMAIN="$2"; shift 2 ;;
    --no-caddy) CADDY=0; shift ;;
    --version) VERSION="$2"; shift 2 ;;
    --port) PORT="$2"; shift 2 ;;
    --agent-port) AGENT_PORT="$2"; shift 2 ;;
    --admin-path) ADMIN_PATH="$2"; shift 2 ;;
    --mirror) MIRROR="$2"; shift 2 ;;
    --purge) PURGE=1; shift ;;
    -u) ADMIN_USER="$2"; shift 2 ;;
    -h|--help) sed -n '2,20p' "$0" 2>/dev/null || echo "see $RAW_SCRIPT"; exit 0 ;;
    *) die "未知参数：$1（--help 查看用法）" ;;
  esac
done

preflight
if [ -z "$ACTION" ]; then
  # Flags without a command (e.g. --domain x) keep the old "install" meaning;
  # no arguments at all on a terminal opens the menu.
  if [ -n "$DOMAIN" ] || [ "$PORT" != 8080 ] || [ -n "$VERSION" ] || [ -n "$ADMIN_PATH" ]; then ACTION=install
  elif [ -t 0 ]; then ACTION=menu
  elif installed; then ACTION=info
  else ACTION=install; fi
fi

case "$ACTION" in
  menu) menu ;;
  install) do_install ;;
  update) do_update ;;
  uninstall) do_uninstall ;;
  start) need_installed; systemctl start polaris; wait_active && green "已启动" ;;
  stop) need_installed; systemctl stop polaris; green "已停止" ;;
  restart) need_installed; systemctl restart polaris; wait_active && green "已重启" ;;
  status) need_installed; systemctl status polaris --no-pager -l ;;
  log) need_installed; do_log ;;
  info) show_info ;;
  reset-password) do_reset_password ;;
  reset-2fa) do_reset_2fa ;;
  set-path) do_set_path "$ARG" ;;
  set-port) do_set_port "$ARG" ;;
  set-domain) do_set_domain "$ARG" ;;
  backup) do_backup ;;
esac
