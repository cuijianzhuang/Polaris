#!/usr/bin/env bash
# Dry-run tests for the HA commands (ha-init / ha-join / ha-join --finish /
# witness-init).
#
# Run from any directory: bash scripts/test-ha-dryrun.sh
#
# The installer is sourced up to (not including) the entry point, with the
# globals pointed at a fixture directory and the few commands the dry-run
# path can reach mocked out. Each case asserts:
#   - the runbook steps print in the right order (design §6.1/§6.3/§13),
#   - secrets are masked (the fixture's real secret key never appears),
#   - dry-run changes nothing (fixture snapshot identical afterwards),
#   - the embedded watchdog copy matches scripts/cf-failover.sh, and the
#     embedded fence copy matches scripts/polaris-ha-fence.sh.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALLER="${INSTALLER_UNDER_TEST:-$SCRIPT_DIR/install-panel.sh}"
work="$(mktemp -d "${TMPDIR:-/tmp}/polaris-ha-dryrun-test.XXXXXXXX")"
work="$(cd "$work" && pwd -P)"
# Only remove the test directory created above, never an empty/root path.
trap '[[ "$work" = /*/polaris-ha-dryrun-test.* ]] && rm -rf "$work"' EXIT

# Everything above the entry point: globals + all function definitions.
awk '/^# --- entry point ---/ {exit} {print}' "$INSTALLER" > "$work/functions.sh"
grep -q '^do_ha_init()' "$work/functions.sh" || { echo "FAIL: no HA functions extracted" >&2; exit 1; }

cat > "$work/mocks.sh" <<'MOCKS'
# The dry-run path never installs, starts or connects to anything; these
# mocks only keep read-only probes from touching the real system.
apt-get() { return 0; }
curl() { return 1; }
install() { printf 'install %s\n' "$*" >> "$CASE_DIR/commands"; return 0; }
pg_su() { return 1; }
MOCKS

# The driver runs as a separate bash process so `set -e` semantics are real.
# Mocks are sourced after the functions so they win over the real pg_su.
driver='
set -euo pipefail
source "$FUNCTIONS"
source "$MOCKS"
# Point the installer globals at the fixture (fresh machine by default).
BIN="$CASE_DIR/bin/polaris-server"
ENV_FILE="$CASE_DIR/etc/polaris.env"
DATA="$CASE_DIR/data"
ETC="$CASE_DIR/etc"
HA_DRYRUN=1
case "$HA_CASE" in
  init) do_ha_init ;;
  join) HA_TOKEN="test-join-token" HA_PANEL_URL="https://polaris.cuijianzhuang.com" do_ha_join ;;
  join-already-secondary) HA_TOKEN="test-join-token" HA_PANEL_URL="https://polaris.cuijianzhuang.com" do_ha_join ;;
  join-openresty) HA_PROXY_MODE=openresty HA_TOKEN="test-join-token" HA_PANEL_URL="https://polaris.cuijianzhuang.com" do_ha_join ;;
  finish) HA_FINISH=1 CF_API_TOKEN="test-cf-token" CF_PRIMARY_IP="203.0.113.1" do_ha_join ;;
  finish-openresty) HA_FINISH=1 CF_API_TOKEN="test-cf-token" CF_PRIMARY_IP="203.0.113.1" do_ha_join ;;
  witness) do_witness_init ;;
  pg-upgrade) PG_UPGRADE_TO=17 do_pg_upgrade ;;
  init-retrofit) do_ha_init ;;
  *) echo "unknown HA_CASE: $HA_CASE" >&2; exit 2 ;;
esac
'

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# assert_order FILE TITLE...: every title appears, in the given order.
assert_order() {
  local f="$1" prev=0 n
  shift
  for t in "$@"; do
    n="$(grep -nF "$t" "$f" | head -n1 | cut -d: -f1)"
    [ -n "$n" ] || fail "missing step: $t (see $f)"
    [ "$n" -gt "$prev" ] || fail "step out of order: $t (see $f)"
    prev="$n"
  done
}

# assert_contains FILE NEEDLE / assert_absent FILE NEEDLE
assert_contains() { grep -qF "$2" "$1" || fail "missing '$2' (see $1)"; }
assert_absent() { if grep -qF "$2" "$1"; then fail "forbidden '$2' found (see $1)"; fi; }

snapshot() { (cd "$1" && find . -type f | LC_ALL=C sort | xargs -r sha256sum 2>/dev/null); }

run_case() {
  local ha_case="$1"
  local d="$work/case-$ha_case"
  mkdir -p "$d/bin" "$d/etc" "$d/data"
  case "$ha_case" in
    init|finish|finish-openresty|init-retrofit)
      # Fake installed panel: the binary prints a version and carries the
      # usage strings the installer greps for (ha-token / ha-seal-secret /
      # "ha sync-wg").
      cat > "$d/bin/polaris-server" <<'FAKEBIN'
#!/usr/bin/env bash
# usage: polaris ha-token / polaris ha-seal-secret / polaris ha sync-wg
echo "polaris v0.1.260 pro"
FAKEBIN
      chmod +x "$d/bin/polaris-server"
      ;;
  esac
  case "$ha_case" in
    init)
      cat > "$d/etc/polaris.env" <<'ENV'
POLARIS_SECRET_KEY=fixture-secret-key-123
POLARIS_DB_DSN=postgres://polaris:fixture-db-pass@127.0.0.1:5432/polaris
POLARIS_PUBLIC_URL=https://polaris.cuijianzhuang.com
POLARIS_ADMIN_PATH=/
POLARIS_LISTEN=127.0.0.1:8080
ENV
      ;;
    finish)
      cat > "$d/etc/polaris.env" <<'ENV'
POLARIS_SECRET_KEY=fixture-secret-key-123
POLARIS_PUBLIC_URL=https://polaris.cuijianzhuang.com
POLARIS_ADMIN_PATH=/
POLARIS_HA_ROLE=secondary
ENV
      ;;
    finish-openresty)
      cat > "$d/etc/polaris.env" <<'ENV'
POLARIS_SECRET_KEY=fixture-secret-key-123
POLARIS_PUBLIC_URL=https://polaris.cuijianzhuang.com
POLARIS_ADMIN_PATH=/
POLARIS_HA_ROLE=secondary
POLARIS_HA_PROXY_MODE=openresty
ENV
      ;;
    join-already-secondary)
      cat > "$d/etc/polaris.env" <<'ENV'
POLARIS_SECRET_KEY=fixture-secret-key-123
POLARIS_PUBLIC_URL=https://polaris.cuijianzhuang.com
POLARIS_ADMIN_PATH=/
POLARIS_HA_ROLE=secondary
POLARIS_HA_WG_IP=10.8.0.2
POLARIS_HA_WG_PUBKEY=fixture-wg-pubkey-standby
POLARIS_HA_PEER_WG_IP=10.8.0.1
POLARIS_HA_ENDPOINT=45.78.33.200
ENV
      ;;
    init-retrofit)
      cat > "$d/etc/polaris.env" <<'ENV'
POLARIS_SECRET_KEY=fixture-secret-key-123
POLARIS_DB_DSN=postgres://polaris:fixture-db-pass@127.0.0.1:5433/polaris
POLARIS_PUBLIC_URL=https://polaris.cuijianzhuang.com
POLARIS_ADMIN_PATH=/
POLARIS_LISTEN=127.0.0.1:8080
POLARIS_HA_ROLE=primary
ENV
      ;;
  esac
  export CASE_DIR="$d" HA_CASE="$ha_case" MOCKS="$work/mocks.sh" FUNCTIONS="$work/functions.sh" WITNESS_URL="http://203.0.113.9:8443"
  snapshot "$d" > "$d.before"
  if bash -c "$driver" > "$d/output" 2>&1; then rc=0; else rc=$?; fi
  [ "$rc" = 0 ] || fail "$ha_case dry-run exited $rc (see $d/output)"
  snapshot "$d" > "$d.after"
  # The snapshots include before/after/output themselves; compare only the
  # fixture files (bin + etc + data).
  grep -v -e '\./before$' -e '\./after$' -e '\./output$' -e '\./commands$' "$d.before" > "$d.before.clean" || true
  grep -v -e '\./before$' -e '\./after$' -e '\./output$' -e '\./commands$' "$d.after" > "$d.after.clean" || true
  diff "$d.before.clean" "$d.after.clean" >/dev/null || fail "$ha_case dry-run changed files (diff $d.before.clean $d.after.clean)"
  # Nothing was installed/created by a mocked command either.
  [ ! -s "$d/commands" ] || fail "$ha_case dry-run ran a mutating command: $(cat "$d/commands")"
  # Secrets never leak into dry-run output.
  assert_absent "$d/output" "fixture-secret-key-123"
  assert_absent "$d/output" "fixture-db-pass"
  assert_absent "$d/output" "test-join-token"
  assert_absent "$d/output" "test-cf-token"
  case "$ha_case" in
    init|join)
      # Masked values do appear where a secret would be used.
      assert_contains "$d/output" "********"
      ;;
  esac
  case "$ha_case" in
    init)
      assert_order "$d/output" \
        "前置检查" \
        "备份（面板 + 数据库 + 配置）" \
        "安装 WireGuard 并生成密钥" \
        "写隧道配置并启动" \
        "生成复制密码并密封保存" \
        "配置 PostgreSQL 主库" \
        "配置 Redis 主库" \
        "面板改走隧道连库" \
        "安装 WireGuard 同步定时器" \
        "安装见证心跳定时器" \
        "安装围栏检查" \
        "生成加入 token"
      assert_contains "$d/output" "备机加入命令"
      assert_contains "$d/output" "ha-join <加入TOKEN>"
      assert_contains "$d/output" "WITNESS_URL=http://203.0.113.9:8443"
      ;;
    join)
      assert_order "$d/output" \
        "前置检查" \
        "安装 WireGuard 并生成密钥" \
        "取配置包并注册对端" \
        "写隧道配置并验证" \
        "搭建 PostgreSQL 从库" \
        "配置 Redis 从库" \
        "安装面板（同版本、同库、同 Redis）" \
        "同步密钥与配置" \
        "配置 nginx 与证书" \
        "安装看门狗与备份同步" \
        "安装见证心跳定时器" \
        "自检"
      assert_contains "$d/output" "GET https://polaris.cuijianzhuang.com/api/v1/ha/join-config"
      assert_contains "$d/output" "POST https://polaris.cuijianzhuang.com/api/v1/ha/peers"
      # The primary's collation is copied by pg_basebackup, and a standby
      # without that locale cannot start ("database locale is incompatible
      # with operating system"). The check has to be planned before the
      # copy, not discovered after an 80 GB transfer.
      assert_contains "$d/output" "确认本机 locale 包含"
      # The bundle carries the whole cluster's collations (template1's
      # en_US.UTF-8 as well as the polaris database's C): each one gets
      # its own ensure step before the copy.
      assert_contains "$d/output" "确认本机 locale 包含 C（"
      assert_contains "$d/output" "确认本机 locale 包含 en_US.UTF-8（"
      assert_contains "$d/output" "只有残留的 standby.signal 则清空重建"
      assert_contains "$d/output" "WITNESS_URL=http://203.0.113.9:8443"
      ;;
    join-openresty)
      assert_order "$d/output" \
        "前置检查" \
        "安装 WireGuard 并生成密钥" \
        "取配置包并注册对端" \
        "写隧道配置并验证" \
        "搭建 PostgreSQL 从库" \
        "配置 Redis 从库" \
        "安装面板（同版本、同库、同 Redis）" \
        "同步密钥与配置" \
        "复用 OpenResty 反代" \
        "安装看门狗与备份同步" \
        "安装见证心跳定时器" \
        "自检"
      assert_contains "$d/output" "GET https://polaris.cuijianzhuang.com/api/v1/ha/join-config"
      assert_contains "$d/output" "POST https://polaris.cuijianzhuang.com/api/v1/ha/peers"
      assert_contains "$d/output" "WITNESS_URL=http://203.0.113.9:8443"
      assert_contains "$d/output" "POLARIS_HA_PROXY_MODE=openresty"
      # Same multi-locale ensure steps as the plain join (shared code path).
      assert_contains "$d/output" "确认本机 locale 包含 C（"
      assert_contains "$d/output" "确认本机 locale 包含 en_US.UTF-8（"
      assert_contains "$d/output" "proxy_pass http://127.0.0.1:8080"
      assert_contains "$d/output" "listen 9443"
      assert_contains "$d/output" "1Panel"
      assert_absent "$d/output" "apt install -y nginx-full"
      assert_absent "$d/output" "certbot certonly"
      assert_absent "$d/output" "配置 nginx 与证书"
      ;;
    join-already-secondary)
      # Re-running ha-join on an existing standby must not redo the join,
      # but it must re-register the peer: the ha_peers row on the primary
      # can be lost (DB reset, re-init), and without it sync-wg would
      # eventually drop this standby from the primary's WireGuard config.
      # The mocked curl fails, so the re-registration warns instead of
      # dying; the standby keeps working on its existing config.
      assert_contains "$d/output" "本机已是备节点"
      assert_contains "$d/output" "对端重新注册失败"
      assert_absent "$d/output" "搭建 PostgreSQL 从库"
      assert_absent "$d/output" "取配置包并注册对端"
      assert_absent "$d/output" "安装面板（同版本、同库、同 Redis）"
      ;;
    finish)
      assert_order "$d/output" "申请证书" "启用 nginx" "安装看门狗"
      assert_absent "$d/output" "搭建 PostgreSQL 从库"
      assert_absent "$d/output" "安装面板（同版本、同库、同 Redis）"
      assert_contains "$d/output" "WITNESS_URL=http://203.0.113.9:8443"
      ;;
    finish-openresty)
      assert_order "$d/output" "复用 OpenResty 反代" "安装看门狗"
      assert_absent "$d/output" "申请证书"
      assert_absent "$d/output" "启用 nginx"
      assert_absent "$d/output" "搭建 PostgreSQL 从库"
      assert_contains "$d/output" "proxy_pass http://127.0.0.1:8080"
      assert_contains "$d/output" "WITNESS_URL=http://203.0.113.9:8443"
      ;;
    witness)
      assert_order "$d/output" \
        "安装面板二进制（含 witness serve）" \
        "写 systemd unit 并启动" \
        "验证"
      assert_contains "$d/output" "ufw allow 8443/tcp"
      assert_contains "$d/output" "WITNESS_URL=http://<见证IP>:8443"
      ;;
    pg-upgrade)
      assert_order "$d/output" \
        "确认当前集群" \
        "读当前 PG 大版本与端口" \
        "目标版本：17" \
        "检查：无表空间、无正在连接的从库、磁盘剩余" \
        "备份：pg_dumpall 到 /root" \
        "装 postgresql-<目标版本>" \
        "pg_upgradecluster -v <目标版本> -m upgrade <当前版本> main" \
        "验证：新集群在原端口、polaris 库在、SELECT 1 通" \
        "旧集群保留在原地"
      ;;
    init-retrofit)
      # Re-running ha-init on an existing primary only installs the pieces
      # the first run skipped (notably the sync-wg timer) and prints a
      # fresh join command; it must not redo the full init.
      assert_contains "$d/output" "本机已是主节点"
      assert_contains "$d/output" "安装 WireGuard 同步定时器"
      assert_contains "$d/output" "备机加入命令"
      assert_absent "$d/output" "配置 PostgreSQL 主库"
      assert_absent "$d/output" "生成加入 token"
      ;;
  esac
  echo "ok: $ha_case"
}

# The watchdog embedded in install-panel.sh must stay byte-identical to
# scripts/cf-failover.sh (ha-join installs the embedded copy).
awk '/cat <<.WATCHDOG./ {f=1; next} /^WATCHDOG$/ {f=0} f' "$INSTALLER" > "$work/embedded.sh"
diff "$work/embedded.sh" "$SCRIPT_DIR/cf-failover.sh" >/dev/null \
  || fail "embedded watchdog drifted from scripts/cf-failover.sh (diff $work/embedded.sh $SCRIPT_DIR/cf-failover.sh)"
echo "ok: embedded watchdog matches scripts/cf-failover.sh"

# The fence script embedded in install-panel.sh must stay byte-identical
# to scripts/polaris-ha-fence.sh (ha-init installs the embedded copy).
awk '/cat <<.FENCE./ {f=1; next} /^FENCE$/ {f=0} f' "$INSTALLER" > "$work/embedded-fence.sh"
diff "$work/embedded-fence.sh" "$SCRIPT_DIR/polaris-ha-fence.sh" >/dev/null \
  || fail "embedded fence drifted from scripts/polaris-ha-fence.sh (diff $work/embedded-fence.sh $SCRIPT_DIR/polaris-ha-fence.sh)"
echo "ok: embedded fence matches scripts/polaris-ha-fence.sh"

# The standby's application_name is written by this script and read by the
# panel when it builds synchronous_standby_names. If the two drift, the HA
# page reports "已设置但未生效" while writes are actually asynchronous.
script_app="$(grep -E '^HA_SYNC_APPNAME=' "$INSTALLER" | head -n1 | cut -d= -f2-)"
go_app="$(grep -E '^const haSyncAppName = ' "$SCRIPT_DIR/../internal/panel/api_ha.go" | head -n1 | cut -d'"' -f2)"
[ -n "$script_app" ] || fail "HA_SYNC_APPNAME missing from install-panel.sh"
[ -n "$go_app" ] || fail "haSyncAppName missing from internal/panel/api_ha.go"
[ "$script_app" = "$go_app" ] \
  || fail "application_name drifted: install-panel.sh=$script_app api_ha.go=$go_app"
echo "ok: application_name matches the panel constant ($script_app)"

run_case init
run_case join
run_case join-already-secondary
run_case join-openresty
run_case finish
run_case finish-openresty
run_case witness
run_case pg-upgrade
run_case init-retrofit

# Watchdog scenarios with a fake curl: primary up, counting failures,
# flipping on the third failure, already on the standby, and local panel
# unhealthy (must not count or flip). Plus the witness quorum cases:
# witness agrees the primary is down (flip + report promotion), witness
# says the primary is up (no flip, counter reset), witness unreachable
# (no flip, counter kept), verdict unparseable (no flip, counter kept).
# Plus the sync-upgrade suppression cases: an in-window suppress file
# skips the whole check (counter untouched, no flip), an expired or
# unreadable one falls back to the normal rules.
run_watchdog_case() {
  local name="$1" d="$work/watchdog-$1"
  mkdir -p "$d/bin"
  cat > "$d/bin/curl" <<'FAKECURL'
#!/usr/bin/env bash
# Fake curl for the watchdog test: answers from scenario env vars.
args="$*"
case "$args" in
  *"-X PUT"*) printf 'PUT %s\n' "$args" >> "$FAKE_PUT_LOG"; printf '{}\n'; exit 0 ;;
esac
case "$args" in
  *"/verdict"*)
    if [ -n "${FAKE_WITNESS_DOWN:-}" ]; then exit 1; fi
    printf '%s\n' "${FAKE_WITNESS_BODY:-{\"reachable\":false}}"; exit 0 ;;
  *"/promoted"*) printf 'PROMOTED %s\n' "$args" >> "$FAKE_PROMOTED_LOG"; printf '{"ok":true}\n'; exit 0 ;;
esac
case "$args" in
  *"--resolve"*) printf '%s\n' "${FAKE_PRIMARY_CODE:-000}"; exit 0 ;;
  *"127.0.0.1:8080"*) printf '%s\n' "${FAKE_LOCAL_CODE:-000}"; exit 0 ;;
  *"dns_records"*) printf '{"result":{"content":"%s"}}\n' "${FAKE_CURRENT_DNS:-}"; exit 0 ;;
esac
exit 1
FAKECURL
  chmod +x "$d/bin/curl"
  export FAKE_PRIMARY_CODE="$2" FAKE_LOCAL_CODE="$3" FAKE_CURRENT_DNS="$5" FAKE_PUT_LOG="$d/put.log" FAKE_PROMOTED_LOG="$d/promoted.log"
  # $6: witness mode — "" (WITNESS_URL unset), agrees (verdict reachable:false),
  # says-up (verdict reachable:true), down (verdict curl fails), unclear
  # (verdict JSON has no reachable field).
  unset FAKE_WITNESS_BODY FAKE_WITNESS_DOWN WITNESS_URL
  case "$6" in
    agrees) export WITNESS_URL="http://203.0.113.9:8443" FAKE_WITNESS_BODY='{"reachable":false,"lastSeen":"2026-11-18T00:00:00Z","secondsSince":42}' ;;
    says-up) export WITNESS_URL="http://203.0.113.9:8443" FAKE_WITNESS_BODY='{"reachable":true,"lastSeen":"2026-11-18T00:00:00Z","secondsSince":2}' ;;
    down) export WITNESS_URL="http://203.0.113.9:8443" FAKE_WITNESS_DOWN=1 ;;
    unclear) export WITNESS_URL="http://203.0.113.9:8443" FAKE_WITNESS_BODY='{"weird":1}' ;;
  esac
  if [ -n "$4" ]; then printf '%s\n' "$4" > "$d/state"; fi
  # $7: suppress mode — "" (no file), active (until an hour ahead),
  # expired (until a minute ago), garbage (until not a number).
  case "$7" in
    active) printf 'until=%s\nop=test-op-1\n' "$(( $(date +%s) + 3600 ))" > "$d/suppress" ;;
    expired) printf 'until=%s\nop=test-op-1\n' "$(( $(date +%s) - 60 ))" > "$d/suppress" ;;
    garbage) printf 'until=soon\nop=test-op-1\n' > "$d/suppress" ;;
  esac
  if CF_TOKEN=test-cf-token CF_ZONE_ID=test-zone CF_RECORD_ID=test-record \
     CF_DOMAIN=polaris.cuijianzhuang.com CF_PRIMARY_IP=203.0.113.1 CF_STANDBY_IP=45.78.33.200 \
     CF_STATE_FILE="$d/state" CF_LOG_FILE="$d/log" CF_SUPPRESS_FILE="$d/suppress" \
     PATH="$d/bin:$PATH" bash "$SCRIPT_DIR/cf-failover.sh" > "$d/output" 2>&1; then rc=0; else rc=$?; fi
  [ "$rc" = 0 ] || fail "watchdog $name exited $rc (see $d/output)"
  case "$name" in
    primary-up)
      [ "$(cat "$d/state")" = 0 ] || fail "watchdog $name: state should reset to 0"
      [ ! -s "$d/put.log" ] || fail "watchdog $name: must not PUT while primary is up"
      ;;
    counting)
      [ "$(cat "$d/state")" = 1 ] || fail "watchdog $name: state should count to 1"
      [ ! -s "$d/put.log" ] || fail "watchdog $name: must not PUT before 3 failures"
      grep -q "primary check failed (1/3)" "$d/log" || fail "watchdog $name: missing count log line"
      ;;
    flip)
      [ "$(cat "$d/state")" = 3 ] || fail "watchdog $name: state should count to 3"
      grep -q "PUT" "$d/put.log" || fail "watchdog $name: PUT missing"
      grep -q "DNS flipped to 45.78.33.200" "$d/log" || fail "watchdog $name: missing flip log line"
      grep -q "no witness configured, flipping on local checks only" "$d/log" || fail "watchdog $name: missing no-witness log line"
      [ ! -s "$d/promoted.log" ] || fail "watchdog $name: must not report promotion without a witness"
      ;;
    witness-agrees)
      [ "$(cat "$d/state")" = 3 ] || fail "watchdog $name: state should count to 3"
      grep -q "PUT" "$d/put.log" || fail "watchdog $name: PUT missing"
      grep -q "DNS flipped to 45.78.33.200" "$d/log" || fail "watchdog $name: missing flip log line"
      grep -q "witness agrees primary is down" "$d/log" || fail "watchdog $name: missing witness-agrees log line"
      grep -q "PROMOTED" "$d/promoted.log" || fail "watchdog $name: promotion not reported to witness"
      grep -q "witness recorded promotion (hk)" "$d/log" || fail "watchdog $name: missing promotion-recorded log line"
      ;;
    witness-says-up)
      [ "$(cat "$d/state")" = 0 ] || fail "watchdog $name: state should reset to 0 when the witness still sees the primary"
      [ ! -s "$d/put.log" ] || fail "watchdog $name: must not PUT when the witness says the primary is up"
      grep -q "witness says primary reachable" "$d/log" || fail "watchdog $name: missing witness-says-up log line"
      [ ! -s "$d/promoted.log" ] || fail "watchdog $name: must not report promotion without a flip"
      ;;
    witness-down)
      [ "$(cat "$d/state")" = 3 ] || fail "watchdog $name: state must stay at 3 (retry next minute) when the witness is unreachable"
      [ ! -s "$d/put.log" ] || fail "watchdog $name: must not PUT when the witness is unreachable"
      grep -q "witness unreachable, skip flip" "$d/log" || fail "watchdog $name: missing witness-unreachable log line"
      ;;
    witness-unclear)
      [ "$(cat "$d/state")" = 3 ] || fail "watchdog $name: state must stay at 3 when the verdict cannot be parsed"
      [ ! -s "$d/put.log" ] || fail "watchdog $name: must not PUT on an unclear verdict"
      grep -q "witness verdict unclear, skip flip" "$d/log" || fail "watchdog $name: missing verdict-unclear log line"
      ;;
    already-on-standby)
      grep -q "already on standby" "$d/log" || fail "watchdog $name: missing already-on-standby line"
      [ ! -s "$d/put.log" ] || fail "watchdog $name: must not PUT when already on standby"
      ;;
    local-unhealthy)
      [ ! -e "$d/state" ] || fail "watchdog $name: must not count when local panel is unhealthy"
      [ ! -s "$d/put.log" ] || fail "watchdog $name: must not PUT when local panel is unhealthy"
      grep -q "local panel unhealthy" "$d/log" || fail "watchdog $name: missing unhealthy log line"
      ;;
    suppress-active)
      [ "$(cat "$d/state")" = 2 ] || fail "watchdog $name: failure counter must stay untouched while suppressed"
      [ ! -s "$d/put.log" ] || fail "watchdog $name: must not PUT while suppressed"
      grep -q "suppressed by sync upgrade op test-op-1" "$d/log" || fail "watchdog $name: missing suppressed log line"
      ;;
    suppress-expired)
      [ "$(cat "$d/state")" = 3 ] || fail "watchdog $name: expired suppression must resume counting"
      grep -q "PUT" "$d/put.log" || fail "watchdog $name: expired suppression must flip normally"
      grep -q "DNS flipped to 45.78.33.200" "$d/log" || fail "watchdog $name: missing flip log line after expiry"
      ;;
    suppress-garbage)
      [ "$(cat "$d/state")" = 3 ] || fail "watchdog $name: unreadable suppression must resume counting"
      grep -q "PUT" "$d/put.log" || fail "watchdog $name: unreadable suppression must flip normally"
      grep -q "suppress file unreadable" "$d/log" || fail "watchdog $name: missing unreadable log line"
      ;;
  esac
  echo "ok: watchdog $name"
}

run_watchdog_case primary-up 200 200 "" 203.0.113.1 "" ""
run_watchdog_case counting 000 200 "" 203.0.113.1 "" ""
run_watchdog_case flip 000 200 2 203.0.113.1 "" ""
run_watchdog_case already-on-standby 000 200 2 45.78.33.200 "" ""
run_watchdog_case local-unhealthy 000 000 "" 203.0.113.1 "" ""
run_watchdog_case witness-agrees 000 200 2 203.0.113.1 agrees ""
run_watchdog_case witness-says-up 000 200 2 203.0.113.1 says-up ""
run_watchdog_case witness-down 000 200 2 203.0.113.1 down ""
run_watchdog_case witness-unclear 000 200 2 203.0.113.1 unclear ""
run_watchdog_case suppress-active 000 200 2 203.0.113.1 "" active
run_watchdog_case suppress-expired 000 200 2 203.0.113.1 "" expired
run_watchdog_case suppress-garbage 000 200 2 203.0.113.1 "" garbage
echo "all ha dry-run tests passed"
