#!/usr/bin/env bash
# Focused tests for ha_wg_start: a re-run of ha-join must not die silently
# when wg-quick fails (set -e kills the script with no message), and an
# already-up wg0 must be accepted instead of re-started.
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
# WG_UP=1: wg0 already up. WG_UP=0: wg0 down; systemctl decides the rest.
# SYSTEMCTL_OK=1: the start succeeds (wg0 comes up). SYSTEMCTL_OK=0: it fails.
driver='
set -euo pipefail
source "$FUNCTIONS"
wg() {
  if [ "$1" = show ] && [ "$2" = wg0 ]; then
    [ "$WG_UP" = 1 ] && return 0
    [ "${WG_AFTER_START:-0}" = 1 ] && return 0
    return 1
  fi
  return 1
}
systemctl() {
  printf "systemctl %s\n" "$*" >> "$CASE_DIR/systemctl"
  if [ "$SYSTEMCTL_OK" = 1 ]; then WG_AFTER_START=1; return 0; fi
  return 1
}
HA_DRYRUN=0
ha_wg_start
'

run_case() { # run_case NAME WG_UP SYSTEMCTL_OK EXPECT_RC EXPECT_SYSTEMCTL
  local name="$1" wg_up="$2" sys_ok="$3" want_rc="$4" want_sys="$5"
  local dir="$work/$name"
  mkdir -p "$dir"
  : > "$dir/systemctl"
  if CASE_DIR="$dir" WG_UP="$wg_up" SYSTEMCTL_OK="$sys_ok" FUNCTIONS="$work/functions.sh" bash -c "$driver" > "$dir/out" 2>&1; then rc=0; else rc=$?; fi
  [ "$rc" = "$want_rc" ] || fail "$name exited $rc, want $want_rc (see $dir/out)"
  if [ "$want_sys" = 0 ]; then
    [ ! -s "$dir/systemctl" ] || fail "$name called systemctl but should not: $(cat "$dir/systemctl")"
  else
    [ -s "$dir/systemctl" ] || fail "$name did not call systemctl"
  fi
  echo "ok: $name"
}

# wg0 already up: succeeds without touching systemctl.
run_case already-up 1 0 0 0

# wg0 down, start succeeds: succeeds and calls systemctl once.
run_case start-ok 0 1 0 1

# wg0 down, start fails: must die with the clear message, not silently.
dir="$work/start-fail"
mkdir -p "$dir"
: > "$dir/systemctl"
if CASE_DIR="$dir" WG_UP=0 SYSTEMCTL_OK=0 FUNCTIONS="$work/functions.sh" bash -c "$driver" > "$dir/out" 2>&1; then
  fail "start-fail should have failed"
fi
grep -q "WireGuard 未能启动" "$dir/out" || fail "start-fail printed no clear error (see $dir/out)"
echo "ok: start-fail"

echo "all ha_wg_start tests passed"
