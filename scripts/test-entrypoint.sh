#!/usr/bin/env bash
# Entry-point tests for the installer: panel-binary maintenance commands
# (ha-token, ha-seal-secret, ha sync-wg, witness serve) must be forwarded
# to the panel binary (polaris-server), not rejected as unknown parameters.
# Run from any directory: bash scripts/test-entrypoint.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALLER="${INSTALLER_UNDER_TEST:-$SCRIPT_DIR/install-panel.sh}"
work="$(mktemp -d "${TMPDIR:-/tmp}/polaris-entrypoint-test.XXXXXXXX")"
work="$(cd "$work" && pwd -P)"
# Only remove the test directory created above, never an empty/root path.
trap '[[ "$work" = /*/polaris-entrypoint-test.* ]] && rm -rf "$work"' EXIT

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# Fake panel binary: records the arguments it was called with.
cat > "$work/fake-bin" <<'FAKE'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$FAKE_ARGS"
exit 0
FAKE
chmod +x "$work/fake-bin"
export FAKE_ARGS="$work/called"

# Copy the installer and point BIN/ENV_FILE at fakes.
cp "$INSTALLER" "$work/installer.sh"
sed -i "s|BIN=/usr/local/bin/polaris-server|BIN=$work/fake-bin|" "$work/installer.sh"
sed -i "s|ENV_FILE=\$ETC/polaris.env|ENV_FILE=$work/fake.env|" "$work/installer.sh"
grep -q "BIN=$work/fake-bin" "$work/installer.sh" || fail "BIN not redirected in test copy"
grep -q "ENV_FILE=$work/fake.env" "$work/installer.sh" || fail "ENV_FILE not redirected in test copy"
printf 'POLARIS_SECRET_KEY=test-key\n' > "$work/fake.env"

run() { # run DESC EXPECTED_ARGS... -- ARGS...
  local desc="$1" want="$2"; shift 2
  : > "$FAKE_ARGS"
  if bash "$work/installer.sh" "$@" > "$work/out" 2>&1; then rc=0; else rc=$?; fi
  [ "$rc" = 0 ] || fail "$desc exited $rc (see $work/out)"
  got="$(cat "$FAKE_ARGS" 2>/dev/null || true)"
  [ "$got" = "$want" ] || fail "$desc forwarded [$got], want [$want]"
  echo "ok: $desc"
}

run "ha sync-wg" "ha sync-wg" ha sync-wg
run "ha-token" "ha-token" ha-token
run "ha-seal-secret" "ha-seal-secret mysecret" ha-seal-secret mysecret
run "witness serve" "witness serve --listen 127.0.0.1:8443" witness serve --listen 127.0.0.1:8443

# An unknown subcommand must still be rejected and must not reach the binary.
: > "$FAKE_ARGS"
if bash "$work/installer.sh" bogus-command > "$work/out" 2>&1; then
  fail "bogus-command should have failed"
fi
grep -q "未知参数" "$work/out" || fail "bogus-command should print 未知参数 (see $work/out)"
[ ! -s "$FAKE_ARGS" ] || fail "bogus-command reached the binary: $(cat "$FAKE_ARGS")"
echo "ok: unknown subcommand rejected"

echo "all entrypoint tests passed"
