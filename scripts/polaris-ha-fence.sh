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
