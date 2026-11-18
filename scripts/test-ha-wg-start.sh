#!/usr/bin/env bash
# Focused tests for ha_wg_start: a re-run of ha-join must not die silently
# when wg-quick fails (set -e kills the script with no message), an
# already-up wg0 must be accepted instead of re-started, and a stale wg0
# (interface exists but `wg show` sees nothing) must be removed and
# rebuilt instead of failing with "already exists".
# Run from any directory: bash scripts/test-ha-wg-start.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALLER="${INSTALLER_UNDER_TEST:-$SCRIPT_DIR/install-panel.sh}"
work="$(mktemp -d "${TMPDIR:-/tmp}/polaris-ha-wg-start-test.XXXXXXXX")"
work="$(cd "$work" && pwd -P)"
# Only remove the test directory created above, never an empty/root path.
trap '[[ "$work" = /*/polaris-ha-wg-start-test.* ]] && rm -rf "$work"' EXIT

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# Everything above the entry point: globals + all function definitions.
awk '/^# --- entry point ---/ {exit} {print}' "$INSTALLER" > "$work/functions.sh"
grep -q '^ha_wg_start()' "$work/functions.sh" || fail "no ha_wg_start extracted"

# The driver runs as a separate bash process so `set -e` semantics are real.
# State lives in files under CASE_DIR: wg_exists (an interface named wg0
# exists) and wg_up (`wg show wg0` works). SYSTEMCTL_OK=1 makes the start
# bring the interface up; 0 makes it fail.
driver='
set -euo pipefail
source "$FUNCTIONS"
wg() {
  if [ "$1" = show ] && [ "$2" = wg0 ]; then
    [ "$(cat "$CASE_DIR/wg_up")" = 1 ] && return 0
    return 1
  fi
  return 1
}
ip() {
  if [ "$1" = link ] && [ "$2" = show ] && [ "$3" = wg0 ]; then
    [ "$(cat "$CASE_DIR/wg_exists")" = 1 ] && return 0
    return 1
  fi
  if [ "$1" = link ] && [ "$2" = delete ] && [ "$3" = wg0 ]; then
    printf "ip %s\n" "$*" >> "$CASE_DIR/ip_calls"
    echo 0 > "$CASE_DIR/wg_exists"
    return 0
  fi
  return 1
}
systemctl() {
  printf "systemctl %s\n" "$*" >> "$CASE_DIR/systemctl"
  if [ "$SYSTEMCTL_OK" = 1 ]; then echo 1 > "$CASE_DIR/wg_up"; echo 1 > "$CASE_DIR/wg_exists"; return 0; fi
  return 1
}
HA_DRYRUN=0
ha_wg_start
'

# run_case NAME EXISTS UP SYS_OK WANT_RC WANT_SYS WANT_IP_DELETE
# EXISTS/UP: initial wg0 state. SYS_OK: systemctl succeeds. WANT_RC: exit code.
# WANT_SYS/WANT_IP_DELETE: 1 if the call must happen, 0 if it must not.
run_case() {
  local name="$1" exists="$2" up="$3" sys_ok="$4" want_rc="$5" want_sys="$6" want_ip="$7"
  local dir="$work/$name"
  mkdir -p "$dir"
  echo "$exists" > "$dir/wg_exists"
  echo "$up" > "$dir/wg_up"
  : > "$dir/systemctl"; : > "$dir/ip_calls"
  if CASE_DIR="$dir" SYSTEMCTL_OK="$sys_ok" FUNCTIONS="$work/functions.sh" bash -c "$driver" > "$dir/out" 2>&1; then rc=0; else rc=$?; fi
  [ "$rc" = "$want_rc" ] || fail "$name exited $rc, want $want_rc (see $dir/out)"
  if [ "$want_sys" = 0 ]; then
    [ ! -s "$dir/systemctl" ] || fail "$name called systemctl but should not: $(cat "$dir/systemctl")"
  else
    [ -s "$dir/systemctl" ] || fail "$name did not call systemctl"
  fi
  if [ "$want_ip" = 0 ]; then
    [ ! -s "$dir/ip_calls" ] || fail "$name deleted wg0 but should not: $(cat "$dir/ip_calls")"
  else
    grep -q "ip link delete wg0" "$dir/ip_calls" || fail "$name did not delete stale wg0"
  fi
  echo "ok: $name"
}

# wg0 already up: succeeds without touching systemctl or ip.
run_case already-up 1 1 0 0 0 0

# wg0 absent, start succeeds: succeeds, calls systemctl, no ip delete.
run_case start-ok 0 0 1 0 1 0

# wg0 stale (exists but `wg show` fails), start succeeds: deletes the stale
# interface, then starts fresh and succeeds.
run_case stale-rebuild 1 0 1 0 1 1

# wg0 absent, start fails: must die with the clear message, not silently.
dir="$work/start-fail"
mkdir -p "$dir"
echo 0 > "$dir/wg_exists"; echo 0 > "$dir/wg_up"
: > "$dir/systemctl"; : > "$dir/ip_calls"
if CASE_DIR="$dir" SYSTEMCTL_OK=0 FUNCTIONS="$work/functions.sh" bash -c "$driver" > "$dir/out" 2>&1; then
  fail "start-fail should have failed"
fi
grep -q "WireGuard 未能启动" "$dir/out" || fail "start-fail printed no clear error (see $dir/out)"
echo "ok: start-fail"

echo "all ha_wg_start tests passed"
