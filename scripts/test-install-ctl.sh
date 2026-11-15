#!/usr/bin/env bash
# Focused, offline regression tests for the actual installer function.
# Run from any directory: bash scripts/test-install-ctl.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALLER="${INSTALLER_UNDER_TEST:-$SCRIPT_DIR/install-panel.sh}"
work="$(mktemp -d "${TMPDIR:-/tmp}/polaris-install-ctl-test.XXXXXXXX")"
work="$(cd "$work" && pwd -P)"
# Only remove the test directory created above, never an empty/root path.
trap '[[ "$work" = /*/polaris-install-ctl-test.* ]] && rm -rf "$work"' EXIT
export INSTALLER

cat > "$work/mocks.sh" <<'MOCKS'
warn() { printf '%s\n' "$*" >> "$CASE_DIR/warnings"; }
latest_version() { printf '%s\n' 'v-test'; }
# Keep hashing real, including on macOS where sha256sum may not be installed.
sha256sum() {
  [ "$SCENARIO" != hash-failure ] || return 44
  if command -v sha256sum >/dev/null; then command sha256sum "$@"; else shasum -a 256 "$@"; fi
}
fetch_origin() {
  printf 'origin %s\n' "$1" >> "$CASE_DIR/fetches"
  [ "$1" = "$RELEASE/checksums.txt" ] || return 90
  printf 'partial download\n' > "$2"
  [ "$SCENARIO" != offline ] || return 22
  cp "$CASE_DIR/release/checksums.txt" "$2"
}
fetch() {
  printf 'mirror %s\n' "$1" >> "$CASE_DIR/fetches"
  [ "$1" = "$RELEASE/install-panel.sh" ] || return 91
  printf 'partial download\n' > "$2"
  [ "$SCENARIO" != script-offline ] || return 22
  cp "$CASE_DIR/release/install-panel.sh" "$2"
  [ "$SCENARIO" != tamper ] || printf 'tampered\n' >> "$2"
}
mktemp() {
  [ "$SCENARIO" != temp-failure ] || return 43
  local made
  made="$(command mktemp "$@")" || return $?
  printf '%s\n' "$made" >> "$CASE_DIR/temps"
  printf '%s\n' "$made"
}
install() {
  printf 'install %s\n' "$*" >> "$CASE_DIR/commands"
  [ "$SCENARIO" != install-failure ] || return 41
  [ "$#" = 4 ] && [ "$1" = -m ] && [ "$2" = 0755 ] && [ "$4" = "$CTL" ] || return 92
  cp "$3" "$4" && chmod 0755 "$4"
}
ln() {
  printf 'ln %s\n' "$*" >> "$CASE_DIR/commands"
  [ "$SCENARIO" != link-failure ] || return 42
  [ "$#" = 3 ] && [ "$1" = -sf ] && [ "$2" = "$CTL" ] && [ "$3" = "$CTL_ALIAS" ] || return 93
  command ln "$@"
}
# No network or system installer can be reached, even if the implementation changes.
curl() { return 94; }
MOCKS

# Extract only the production function definitions into a plain file.
# bash 3.2 (macOS) reuses /dev/fd/63 across consecutive `source <(...)`
# calls, so sourcing two functions via process substitution silently drops
# the second one. A real file avoids that and never runs installer dispatch.
awk '/^install_ctl\(\) \{/ {copy=1} copy {print} copy && /^}/ {exit}' "$INSTALLER" > "$work/functions.sh"
awk '/^is_script\(\)/ {print; exit}' "$INSTALLER" >> "$work/functions.sh"
export MOCKS="$work/mocks.sh" FUNCTIONS="$work/functions.sh"

# The driver runs as a separate bash process so `set -e` semantics are real.
# Its $0 is supplied by the caller to model a file-backed install, the
# already-installed command, or a pipe (process substitution -> /dev/fd/*).
driver='
set -euo pipefail
source "$MOCKS"
source "$FUNCTIONS"
declare -F install_ctl is_script >/dev/null
if [ "$CALL_MODE" = conditional ]; then
  install_ctl || exit $?
else
  install_ctl
fi
printf "continued\n" > "$CASE_DIR/continued"
'

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
count=0
run_case() {
  local scenario="$1" source_kind="$2" expected="$3" call_mode="${4:-direct}" rc d
  count=$((count + 1))
  d="$work/$count-$scenario-$source_kind-$call_mode"
  mkdir -p "$d/release" "$d/bin" "$d/tmp" "$d/fallback-tmp"
  printf '#!/usr/bin/env bash\nprintf "release fixture\\n"\n' > "$d/release/install-panel.sh"
  printf '#!/usr/bin/env bash\nprintf "running fixture\\n"\n' > "$d/running.sh"
  printf 'keep parent\n' > "$d/tmp/keep"
  local hash
  if command -v sha256sum >/dev/null; then
    hash="$(command sha256sum "$d/release/install-panel.sh" | awk '{print $1}')"
  else
    hash="$(shasum -a 256 "$d/release/install-panel.sh" | awk '{print $1}')"
  fi
  printf '%s  install-panel.sh\n' "$hash" > "$d/release/checksums.txt"
  case "$scenario" in
    old-release) printf '%s  other-asset\n' "$hash" > "$d/release/checksums.txt" ;;
    star-checksum) printf '%s *install-panel.sh\n' "$hash" > "$d/release/checksums.txt" ;;
  esac
  export CASE_DIR="$d" SCENARIO="$scenario" SOURCE_KIND="$source_kind" CALL_MODE="$call_mode"
  export CTL="$d/bin/polaris" CTL_ALIAS="$d/bin/polaris-ctl"
  export TMP="$d/tmp" TMPDIR="$d/fallback-tmp" VERSION=v-test
  export REPO=example/Polaris RELEASE=https://github.com/example/Polaris/releases/download/v-test
  case "$source_kind" in
    release) cp "$d/release/install-panel.sh" "$TMP/install-panel.sh" ;;
    existing) cp "$d/release/install-panel.sh" "$CTL" ;;
    unset-tmp) unset TMP ;;
    empty-tmp) TMP="" ;;
    latest) VERSION="" ;;
  esac
  # Pass a real $0 for file-backed/already-installed sources; use a process
  # substitution ([ -f "$0" ] is false) for pipe and every other scenario.
  case "$source_kind" in
    running)
      if bash -c "$driver" "$d/running.sh" > "$d/output" 2>&1; then rc=0; else rc=$?; fi ;;
    existing)
      if bash -c "$driver" "$CTL" > "$d/output" 2>&1; then rc=0; else rc=$?; fi ;;
    *)
      if bash -c "$driver" <(printf '#!/usr/bin/env bash\n') > "$d/output" 2>&1; then rc=0; else rc=$?; fi ;;
  esac
  [ "$rc" = "$expected" ] || fail "$scenario/$source_kind/$call_mode: status $rc, expected $expected (output: $(< "$d/output"))"
  if [ "$expected" = 0 ]; then
    [ -f "$d/continued" ] || fail "$scenario: did not continue"
  else
    [ ! -e "$d/continued" ] || fail "$scenario: swallowed failure"
  fi
  # Every temporary directory made by the production function must be gone.
  if [ -f "$d/temps" ]; then
    while IFS= read -r made; do
      [ ! -e "$made" ] || fail "$scenario: leaked $made"
    done < "$d/temps"
  fi
  local leftovers
  shopt -s nullglob dotglob
  leftovers=("$d/tmp"/ctl.* "$d/fallback-tmp"/*)
  shopt -u nullglob dotglob
  [ "${#leftovers[@]}" = 0 ] || fail "$scenario: temporary paths remain"
  [ -f "$d/tmp/keep" ] || fail "$scenario: removed parent temporary directory"
  case "$scenario" in
    offline|script-offline|tamper|old-release|hash-failure)
      [ -s "$d/warnings" ] || fail "$scenario: missing warning"
      [ ! -e "$d/commands" ] || fail "$scenario: attempted install or alias registration"
      [ ! -e "$CTL" ] && [ ! -L "$CTL_ALIAS" ] || fail "$scenario: installed unverified script/alias"
      ;;
    install-failure|temp-failure)
      [ ! -e "$CTL" ] && [ ! -L "$CTL_ALIAS" ] || fail "$scenario: installed script/alias after failure"
      if [ -e "$d/commands" ]; then
        ! grep -q '^ln ' "$d/commands" || fail "$scenario: attempted alias after install failure"
      fi
      ;;
    link-failure)
      [ -f "$CTL" ] && [ ! -L "$CTL_ALIAS" ] || fail "$scenario: unexpected link-failure state"
      ;;
    *)
      [ -x "$CTL" ] || [ "$source_kind" = existing ] || fail "$scenario: missing executable command"
      [ -L "$CTL_ALIAS" ] && [ "$(readlink "$CTL_ALIAS")" = "$CTL" ] || fail "$scenario: missing/wrong alias"
      if [ "$source_kind" = running ]; then
        cmp -s "$CTL" "$d/running.sh" || fail "$scenario: running source precedence changed"
      else
        cmp -s "$CTL" "$d/release/install-panel.sh" || fail "$scenario: wrong installed script"
      fi
      ;;
  esac
  if [ -e "$d/fetches" ]; then
    grep -q "^origin $RELEASE/checksums.txt$" "$d/fetches" || fail "$scenario: checksums did not use origin"
    ! grep -q '^mirror .*checksums.txt$' "$d/fetches" || fail "$scenario: checksums used mirror"
  fi
  printf 'PASS: %s/%s/%s (status %s)\n' "$scenario" "$source_kind" "$call_mode" "$rc"
}

for mode in direct conditional; do
  run_case success pipe 0 "$mode"
  run_case star-checksum pipe 0 "$mode"
  run_case tamper pipe 0 "$mode"
  run_case offline pipe 0 "$mode"
  run_case script-offline pipe 0 "$mode"
  run_case old-release pipe 0 "$mode"
  run_case hash-failure pipe 0 "$mode"
  run_case install-failure pipe 41 "$mode"
  run_case link-failure pipe 42 "$mode"
  run_case temp-failure pipe 43 "$mode"
  for source_kind in release running; do
    run_case success "$source_kind" 0 "$mode"
    run_case install-failure "$source_kind" 41 "$mode"
    run_case link-failure "$source_kind" 42 "$mode"
  done
done
run_case success unset-tmp 0
run_case success empty-tmp 0
run_case success latest 0
run_case success existing 0
printf 'All %s install_ctl regression cases passed.\n' "$count"
