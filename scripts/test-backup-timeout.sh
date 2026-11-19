#!/usr/bin/env bash
# Focused tests for the upgrade backup timeout: a backup that cannot reach
# its database (e.g. a standby connecting to its primary over a flaky
# tunnel) used to hang the upgrade at its backup step forever, because the
# backup has no connect timeout and neither succeeds nor fails. polaris_cmd
# must cap a call at POLARIS_CMD_TIMEOUT seconds (killing the stuck
# process) and the upgrade backup step must set that cap, so the existing
# warn-and-continue branch runs instead of waiting indefinitely.
# Run from any directory: bash scripts/test-backup-timeout.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALLER="${INSTALLER_UNDER_TEST:-$SCRIPT_DIR/install-panel.sh}"
work="$(mktemp -d "${TMPDIR:-/tmp}/polaris-backup-timeout-test.XXXXXXXX")"
work="$(cd "$work" && pwd -P)"
# Only remove the test directory created above, never an empty/root path.
trap '[[ "$work" = /*/polaris-backup-timeout-test.* ]] && rm -rf "$work"' EXIT
export INSTALLER

REAL_TIMEOUT="$(command -v timeout || true)"
if [ -z "$REAL_TIMEOUT" ]; then
  printf 'SKIP: GNU timeout is not installed on this system\n'
  exit 0
fi

mkdir -p "$work/mockbin" "$work/data"
cat > "$work/mocks.sh" <<'MOCKS'
red() { printf 'red: %s\n' "$*" >&2; }
MOCKS

# Mock runuser: strip "-u <user> --" and exec the rest, like the real one.
# The mock is an executable on PATH so the real `timeout` can kill it.
cat > "$work/mockbin/runuser" <<MOCKRUNUSER
#!/usr/bin/env bash
printf 'runuser %s\n' "\$*" >> "$work/runuser.log"
[ "\$1" = "-u" ] && shift 2
[ "\$1" = "--" ] && shift
exec "\$@"
MOCKRUNUSER
# Mock timeout: log the invocation, then run the real one.
cat > "$work/mockbin/timeout" <<MOCKTIMEOUT
#!/usr/bin/env bash
printf 'timeout %s\n' "\$*" >> "$work/timeout.log"
exec "$REAL_TIMEOUT" "\$@"
MOCKTIMEOUT
chmod +x "$work/mockbin/runuser" "$work/mockbin/timeout"

# Fake panel binary: carries the marker polaris_cmd greps for, then either
# exits at once (ok) or hangs forever (hang) like a backup stuck connecting.
cat > "$work/polaris-server" <<'MOCKBIN'
#!/usr/bin/env bash
# polaris admin reset-password (fixture marker for the polaris_cmd check)
case "${BIN_SCENARIO:-ok}" in
  hang) printf '%s\n' "$$" > "$BIN_PID_FILE"; exec sleep 301 ;;
  *) exit 0 ;;
esac
MOCKBIN
chmod +x "$work/polaris-server"
: > "$work/polaris.env"

# Extract only the production function definition into a plain file.
awk '/^polaris_cmd\(\) \{/ {copy=1} copy {print} copy && /^}/ {exit}' "$INSTALLER" > "$work/functions.sh"
export MOCKS="$work/mocks.sh" FUNCTIONS="$work/functions.sh"

# The driver runs as a separate bash process so `set -e` semantics are real.
driver='
set -euo pipefail
source "$MOCKS"
source "$FUNCTIONS"
declare -F polaris_cmd >/dev/null
start=$SECONDS
if polaris_cmd backup "$CASE_DIR/backup.polaris" >/dev/null 2>&1; then rc=0; else rc=$?; fi
printf "rc=%s elapsed=%s\n" "$rc" "$((SECONDS - start))" > "$CASE_DIR/result"
'

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
count=0
run_case() {
  local scenario="$1" timeout_secs="$2" expected_rc="$3" expect_timeout_call="$4"
  local rc="" elapsed=""
  count=$((count + 1))
  local d="$work/$count-$scenario-${timeout_secs:-none}"
  mkdir -p "$d"
  : > "$work/timeout.log"; : > "$work/runuser.log"; : > "$d/pid"
  export CASE_DIR="$d" BIN_SCENARIO="$scenario" BIN_PID_FILE="$d/pid"
  export BIN="$work/polaris-server" DATA="$work/data" ENV_FILE="$work/polaris.env"
  export PATH="$work/mockbin:$PATH"
  if [ -n "$timeout_secs" ]; then export POLARIS_CMD_TIMEOUT="$timeout_secs"; else unset POLARIS_CMD_TIMEOUT; fi
  if bash -c "$driver" > "$d/output" 2>&1; then driver_rc=0; else driver_rc=$?; fi
  [ "$driver_rc" = 0 ] || fail "$scenario/timeout=${timeout_secs:-none}: driver failed (output: $(< "$d/output"))"
  # shellcheck disable=SC1090
  source "$d/result"
  [ "$rc" = "$expected_rc" ] || fail "$scenario/timeout=${timeout_secs:-none}: status $rc, expected $expected_rc"
  [ "$elapsed" -lt 10 ] || fail "$scenario/timeout=${timeout_secs:-none}: took ${elapsed}s, expected under 10s"
  if [ "$expect_timeout_call" = yes ]; then
    grep -q "^timeout -k 30 $timeout_secs runuser -u polaris -- $BIN backup $d/backup.polaris$" "$work/timeout.log" \
      || fail "$scenario/timeout=${timeout_secs:-none}: timeout was not called with the expected arguments (log: $(< "$work/timeout.log"))"
  else
    [ ! -s "$work/timeout.log" ] || fail "$scenario/timeout=${timeout_secs:-none}: timeout must not be called (log: $(< "$work/timeout.log"))"
  fi
  grep -q "^runuser -u polaris -- $BIN backup $d/backup.polaris$" "$work/runuser.log" \
    || fail "$scenario/timeout=${timeout_secs:-none}: runuser was not called with the expected arguments (log: $(< "$work/runuser.log"))"
  if [ -s "$d/pid" ]; then
    kill -0 "$(cat "$d/pid")" 2>/dev/null && fail "$scenario/timeout=${timeout_secs:-none}: hung backup process $(cat "$d/pid") is still alive"
  fi
  printf 'PASS: %s/timeout=%s (rc %s, %ss)\n' "$scenario" "${timeout_secs:-none}" "$rc" "$elapsed"
}

# A hanging backup with a cap must be killed by timeout (124) within seconds.
run_case hang 2 124 yes
# A healthy backup with a cap still succeeds, and timeout is applied.
run_case ok 2 0 yes
# Without a cap, timeout is not involved at all.
run_case ok "" 0 no

# The upgrade backup step must set the cap, or the fix above never runs.
grep -qF 'POLARIS_CMD_TIMEOUT=120 polaris_cmd backup "$backup"' "$INSTALLER" \
  || fail "do_update backup step does not set POLARIS_CMD_TIMEOUT=120"
printf 'PASS: do_update backup step sets POLARIS_CMD_TIMEOUT=120\n'
printf 'All %s backup timeout cases passed.\n' "$count"
