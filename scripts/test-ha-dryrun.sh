#!/usr/bin/env bash
# Dry-run tests for the HA commands (ha-init / ha-join / ha-join --finish).
#
# Run from any directory: bash scripts/test-ha-dryrun.sh
#
# The installer is sourced up to (not including) the entry point, with the
# globals pointed at a fixture directory and the few commands the dry-run
# path can reach mocked out. Each case asserts:
#   - the runbook steps print in the right order (design §6.1/§6.3),
#   - secrets are masked (the fixture's real secret key never appears),
#   - dry-run changes nothing (fixture snapshot identical afterwards),
#   - the embedded watchdog copy matches scripts/cf-failover.sh.
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
  finish) HA_FINISH=1 CF_API_TOKEN="test-cf-token" CF_PRIMARY_IP="203.0.113.1" do_ha_join ;;
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
    init|finish)
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
  esac
  export CASE_DIR="$d" HA_CASE="$ha_case" MOCKS="$work/mocks.sh" FUNCTIONS="$work/functions.sh"
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
        "生成加入 token"
      assert_contains "$d/output" "备机加入命令"
      assert_contains "$d/output" "ha-join <加入TOKEN>"
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
        "自检"
      assert_contains "$d/output" "GET https://polaris.cuijianzhuang.com/api/v1/ha/join-config"
      assert_contains "$d/output" "POST https://polaris.cuijianzhuang.com/api/v1/ha/peers"
      ;;
    finish)
      assert_order "$d/output" "申请证书" "启用 nginx" "安装看门狗"
      assert_absent "$d/output" "搭建 PostgreSQL 从库"
      assert_absent "$d/output" "安装面板（同版本、同库、同 Redis）"
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

run_case init
run_case join
run_case finish

# Watchdog scenarios with a fake curl: primary up, counting failures,
# flipping on the third failure, already on the standby, and local panel
# unhealthy (must not count or flip).
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
  *"--resolve"*) printf '%s\n' "${FAKE_PRIMARY_CODE:-000}"; exit 0 ;;
  *"127.0.0.1:8080"*) printf '%s\n' "${FAKE_LOCAL_CODE:-000}"; exit 0 ;;
  *"dns_records"*) printf '{"result":{"content":"%s"}}\n' "${FAKE_CURRENT_DNS:-}"; exit 0 ;;
esac
exit 1
FAKECURL
  chmod +x "$d/bin/curl"
  export FAKE_PRIMARY_CODE="$2" FAKE_LOCAL_CODE="$3" FAKE_CURRENT_DNS="$5" FAKE_PUT_LOG="$d/put.log"
  if [ -n "$4" ]; then printf '%s\n' "$4" > "$d/state"; fi
  if CF_TOKEN=test-cf-token CF_ZONE_ID=test-zone CF_RECORD_ID=test-record \
     CF_DOMAIN=polaris.cuijianzhuang.com CF_PRIMARY_IP=203.0.113.1 CF_STANDBY_IP=45.78.33.200 \
     CF_STATE_FILE="$d/state" CF_LOG_FILE="$d/log" \
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
  esac
  echo "ok: watchdog $name"
}

run_watchdog_case primary-up 200 200 "" 203.0.113.1
run_watchdog_case counting 000 200 "" 203.0.113.1
run_watchdog_case flip 000 200 2 203.0.113.1
run_watchdog_case already-on-standby 000 200 2 45.78.33.200
run_watchdog_case local-unhealthy 000 000 "" 203.0.113.1
echo "all ha dry-run tests passed"
