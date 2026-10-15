#!/usr/bin/env bash
# Polaris panel installer / updater / uninstaller.
#
#   bash <(curl -fsSL https://raw.githubusercontent.com/cuijianzhuang/Polaris/main/scripts/install-panel.sh)
#   bash <(curl -fsSL ...) --domain panel.example.com      # HTTPS via Caddy (recommended)
#   bash <(curl -fsSL ...) update                           # upgrade binary, keep config & data
#   bash <(curl -fsSL ...) uninstall [--purge]              # remove; --purge also deletes data
#
# Options:
#   --domain NAME     panel domain; installs Caddy for automatic HTTPS (DNS must already point here)
#   --no-caddy        with --domain, do not install Caddy (you run your own reverse proxy)
#   --version TAG     release tag to install (default: newest release, pre-releases included)
#   --port N          panel HTTP port (default 8080; loopback-only when Caddy fronts it)
#   --agent-port N    mTLS port nodes connect to (default 9443)
#   --admin-path P    admin UI path (default: random)
#   --mirror URL      download prefix for github.com (e.g. https://ghproxy.example.com/)
set -euo pipefail

REPO="cuijianzhuang/Polaris"
BIN=/usr/local/bin/polaris
ETC=/etc/polaris
ENV_FILE=$ETC/polaris.env
DATA=/var/lib/polaris
UNIT=/etc/systemd/system/polaris.service

ACTION=install DOMAIN="" CADDY=1 VERSION="" PORT=8080 AGENT_PORT=9443 ADMIN_PATH="" MIRROR="" PURGE=0
while [ $# -gt 0 ]; do
  case "$1" in
    install|update|uninstall) ACTION="$1"; shift ;;
    --domain) DOMAIN="$2"; shift 2 ;;
    --no-caddy) CADDY=0; shift ;;
    --version) VERSION="$2"; shift 2 ;;
    --port) PORT="$2"; shift 2 ;;
    --agent-port) AGENT_PORT="$2"; shift 2 ;;
    --admin-path) ADMIN_PATH="$2"; shift 2 ;;
    --mirror) MIRROR="$2"; shift 2 ;;
    --purge) PURGE=1; shift ;;
    -h|--help) sed -n '2,19p' "$0" 2>/dev/null || true; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

red() { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
yellow() { printf '\033[33m%s\033[0m\n' "$*"; }
die() { red "错误：$*" >&2; exit 1; }
step() { printf '\033[36m==>\033[0m %s\n' "$*"; }

[ "$(id -u)" = 0 ] || die "请以 root 运行"
command -v systemctl >/dev/null || die "需要 systemd"
for c in curl tar sha256sum awk sed; do command -v "$c" >/dev/null || die "缺少命令 $c"; done

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "不支持的架构 $(uname -m)" ;;
esac

# --- uninstall ---------------------------------------------------------------
if [ "$ACTION" = uninstall ]; then
  step "停止并移除 polaris 服务"
  systemctl disable --now polaris 2>/dev/null || true
  rm -f "$UNIT" "$BIN"
  systemctl daemon-reload
  if [ "$PURGE" = 1 ]; then
    yellow "删除配置与数据：$ETC $DATA"
    rm -rf "$ETC" "$DATA"
    id polaris >/dev/null 2>&1 && userdel polaris 2>/dev/null || true
  else
    echo "已保留配置 $ETC 与数据 $DATA（加 --purge 一并删除）"
  fi
  green "已卸载"
  exit 0
fi

# --- download helpers --------------------------------------------------------
gh() { printf '%s%s' "$MIRROR" "$1"; }
fetch() { curl -fsSL --retry 3 --connect-timeout 15 -o "$2" "$1"; }

if [ -z "$VERSION" ]; then
  step "查询最新版本"
  # /releases/latest skips pre-releases, so take the newest entry of the list.
  VERSION="$(curl -fsSL --retry 3 "https://api.github.com/repos/$REPO/releases?per_page=1" |
    sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n1)"
  [ -n "$VERSION" ] || die "无法获取版本号（GitHub API 不可达？可用 --version 指定）"
fi
RELEASE="${POLARIS_RELEASE_URL:-$(gh "https://github.com/$REPO/releases/download/$VERSION")}"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
step "下载 Polaris $VERSION ($ARCH)"
PANEL_TGZ="polaris-panel-$VERSION-linux-$ARCH.tar.gz"
fetch "$RELEASE/checksums.txt" "$TMP/checksums.txt" || die "下载 checksums.txt 失败：$RELEASE"
for f in "$PANEL_TGZ" polaris-agent-linux-amd64 polaris-agent-linux-arm64; do
  fetch "$RELEASE/$f" "$TMP/$f" || die "下载 $f 失败"
  want="$(awk -v f="$f" '$2 == f || $2 == "*"f {print $1}' "$TMP/checksums.txt")"
  got="$(sha256sum "$TMP/$f" | awk '{print $1}')"
  [ -n "$want" ] && [ "$want" = "$got" ] || die "$f 校验失败（期望 ${want:-无}，实际 $got）"
done
green "SHA-256 校验通过"
tar -xzf "$TMP/$PANEL_TGZ" -C "$TMP"

install_files() {
  install -m 0755 "$TMP/polaris" "$BIN"
  # Agent binaries served at /download/ so node installs pull them from this panel.
  install -d -m 0755 -o polaris -g polaris "$DATA" "$DATA/downloads"
  install -m 0755 -o polaris -g polaris "$TMP/polaris-agent-linux-amd64" "$TMP/polaris-agent-linux-arm64" "$DATA/downloads/"
}

# --- update ------------------------------------------------------------------
if [ "$ACTION" = update ]; then
  [ -f "$ENV_FILE" ] || die "未找到 $ENV_FILE，请先安装"
  cur="$("$BIN" --version 2>/dev/null || echo unknown)"
  step "升级 $cur -> $VERSION（配置与数据保持不变）"
  backup="$DATA/backup-$(date +%Y%m%d%H%M%S)"
  if ls "$DATA"/*.db >/dev/null 2>&1; then
    mkdir -p "$backup" && cp -a "$DATA"/*.db* "$backup"/ && echo "数据库已备份到 $backup"
  fi
  systemctl stop polaris
  install_files
  systemctl start polaris
  sleep 2
  systemctl is-active --quiet polaris || { journalctl -u polaris -n 30 --no-pager; die "启动失败，旧数据库备份在 $backup"; }
  green "已升级到 $("$BIN" --version)"
  exit 0
fi

# --- install -----------------------------------------------------------------
[ -f "$ENV_FILE" ] && die "$ENV_FILE 已存在：升级请用 update，重装请先 uninstall"

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

if [ -n "$DOMAIN" ]; then
  HOST="$DOMAIN"
else
  HOST="${POLARIS_PUBLIC_IP:-$(public_ip)}"
  [ -n "$HOST" ] || die "无法探测公网 IP：请用 --domain 指定域名，或设置 POLARIS_PUBLIC_IP"
fi
[ -n "$ADMIN_PATH" ] || ADMIN_PATH="/$(rand 12)"
ADMIN_PATH="/${ADMIN_PATH#/}"
SECRET="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"

USE_CADDY=0
if [ -n "$DOMAIN" ] && [ "$CADDY" = 1 ]; then USE_CADDY=1; fi
if [ "$USE_CADDY" = 1 ] || { [ -n "$DOMAIN" ] && [ "$CADDY" = 0 ]; }; then
  LISTEN="127.0.0.1:$PORT"; PUBLIC_URL="https://$DOMAIN"
else
  LISTEN=":$PORT"; PUBLIC_URL="http://$HOST:$PORT"
fi

step "创建用户与目录"
id polaris >/dev/null 2>&1 || useradd --system --home-dir "$DATA" --shell /usr/sbin/nologin polaris
install -d -m 0750 -o root -g polaris "$ETC"
install_files

cat > "$ENV_FILE" <<EOF
# Generated by install-panel.sh on $(date -u +%Y-%m-%dT%H:%M:%SZ)
# 务必备份 POLARIS_SECRET_KEY：丢失后已加密的数据无法解密
POLARIS_SECRET_KEY=$SECRET
POLARIS_ADMIN_PATH=$ADMIN_PATH
POLARIS_PUBLIC_URL=$PUBLIC_URL
POLARIS_LISTEN=$LISTEN
POLARIS_AGENT_LISTEN=:$AGENT_PORT
POLARIS_AGENT_HOSTS=$HOST
POLARIS_DATA_DIR=$DATA
POLARIS_INITIAL_ADMIN_USERNAME=admin
EOF
chmod 0640 "$ENV_FILE"; chown root:polaris "$ENV_FILE"

install -m 0644 "$TMP/polaris.service" "$UNIT"

if [ "$USE_CADDY" = 1 ]; then
  if ! command -v caddy >/dev/null; then
    step "安装 Caddy（自动申请 HTTPS 证书）"
    if command -v apt-get >/dev/null; then
      apt-get update -qq && apt-get install -y -qq debian-keyring debian-archive-keyring apt-transport-https gnupg >/dev/null
      curl -fsSL https://dl.cloudsmith.io/public/caddy/stable/gpg.key | gpg --dearmor --yes -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
      curl -fsSL https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt > /etc/apt/sources.list.d/caddy-stable.list
      apt-get update -qq && apt-get install -y -qq caddy >/dev/null
    elif command -v dnf >/dev/null; then
      dnf install -y -q 'dnf-command(copr)' && dnf copr enable -y -q @caddy/caddy && dnf install -y -q caddy
    else
      die "无法自动安装 Caddy，请手动安装或改用 --no-caddy"
    fi
  fi
  step "配置 Caddy 站点 $DOMAIN"
  CF=/etc/caddy/Caddyfile
  mkdir -p /etc/caddy
  if [ -f "$CF" ] && grep -q "^$DOMAIN {" "$CF"; then
    yellow "Caddyfile 中已有 $DOMAIN 站点，未修改"
  else
    # The distro's stock Caddyfile only serves a welcome page on :80; replace it.
    # Any other existing config is kept and our site is appended.
    if [ -f "$CF" ] && grep -q '/usr/share/caddy' "$CF"; then cp "$CF" "$CF.orig"; : > "$CF"; fi
    printf '\n%s {\n\tencode zstd gzip\n\treverse_proxy 127.0.0.1:%s\n}\n' "$DOMAIN" "$PORT" >> "$CF"
  fi
  systemctl enable --now caddy >/dev/null 2>&1 || true
  systemctl reload caddy || systemctl restart caddy
fi

step "启动 Polaris"
systemctl daemon-reload
systemctl enable --now polaris >/dev/null 2>&1
PASS=""
for _ in $(seq 1 30); do
  PASS="$(journalctl -u polaris --no-pager -o cat 2>/dev/null | sed -n 's/.*created initial admin.* password=\([^ ]*\).*/\1/p' | tail -n1)"
  [ -n "$PASS" ] && break
  systemctl is-active --quiet polaris || break
  sleep 1
done
systemctl is-active --quiet polaris || { journalctl -u polaris -n 40 --no-pager; die "Polaris 启动失败"; }

echo
green "Polaris $VERSION 安装完成"
echo "  后台地址：  $PUBLIC_URL$ADMIN_PATH/"
echo "  用户名：    admin"
echo "  初始密码：  ${PASS:-（未能从日志读取，运行 journalctl -u polaris | grep password 查看）}"
echo "  配置文件：  $ENV_FILE（含加密密钥，请备份）"
echo "  数据目录：  $DATA"
echo
yellow "请在防火墙放行："
if [ "$USE_CADDY" = 1 ]; then echo "  80/tcp、443/tcp  （HTTPS 与证书申请）"; else echo "  $PORT/tcp        （后台与订阅）"; fi
echo "  $AGENT_PORT/tcp      （节点 Agent 接入，mTLS，不能经过反代）"
if [ -z "$DOMAIN" ]; then
  echo
  yellow "当前为 HTTP 明文访问。正式使用建议绑定域名重装：--domain panel.example.com"
fi
if [ -n "$DOMAIN" ] && [ "$CADDY" = 0 ]; then
  echo
  yellow "未安装 Caddy：请让你的反向代理把 https://$DOMAIN 转发到 http://127.0.0.1:$PORT"
fi
echo
echo "登录后：修改密码 → 开启二次验证 → 「节点与内核」添加节点"
echo "升级：bash <(curl -fsSL https://raw.githubusercontent.com/$REPO/main/scripts/install-panel.sh) update"
