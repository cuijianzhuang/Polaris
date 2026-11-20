#!/bin/bash
# cf-failover.sh — Cloudflare DNS failover watchdog (HA standby only).
#
# Runs every minute from cron on the standby. It checks the primary over its
# public IP (bypassing current DNS); if the primary fails 3 checks in a row
# and the local panel is healthy, it moves the polaris A record to the
# standby. One-way only: it never switches back (failback is manual, to
# avoid flapping). Do NOT run this on the primary.
#
# Configuration comes from the environment or /etc/cf-failover.env
# (KEY=VALUE, 0600). Nothing is hardcoded: the installer writes the env
# file from the join config.
#   CF_TOKEN       Cloudflare API token (Zone:DNS:Edit for the zone)
#   CF_ZONE_ID     Cloudflare zone id for the domain's zone
#   CF_RECORD_ID   Cloudflare DNS record id of the polaris A record
#   CF_DOMAIN      e.g. polaris.cuijianzhuang.com
#   CF_ADMIN_PATH  admin path prefix, "/" when none (the API lives under
#                  <admin-path>/api/v1; default "/" keeps /api/v1/info)
#   CF_PRIMARY_IP  primary public IP (dmit)
#   CF_STANDBY_IP  standby public IP (this machine, HK)
#   WITNESS_URL    witness arbiter base URL, e.g. http://<WITNESS_IP>:8443
#                  (optional). When set, the DNS flip needs quorum: this
#                  machine must fail 3 checks in a row AND the witness must
#                  also report the primary unreachable AND the witness must
#                  answer at all. A witness that cannot be reached, or one
#                  that still sees the primary, blocks the flip — never flip
#                  on a single observer. When unset, the old local-only
#                  behaviour applies.
set -u
[ -f /etc/cf-failover.env ] && set -a && . /etc/cf-failover.env && set +a
CF_TOKEN="${CF_TOKEN:-}"
CF_ZONE_ID="${CF_ZONE_ID:-}"
CF_RECORD_ID="${CF_RECORD_ID:-}"
CF_DOMAIN="${CF_DOMAIN:-}"
CF_ADMIN_PATH="${CF_ADMIN_PATH:-/}"
CF_PRIMARY_IP="${CF_PRIMARY_IP:-}"
CF_STANDBY_IP="${CF_STANDBY_IP:-}"
WITNESS_URL="${WITNESS_URL:-}"
# Test hooks (the installer always uses the defaults above).
STATE="${CF_STATE_FILE:-/var/lib/cf-failover.state}"
LOG="${CF_LOG_FILE:-/var/log/cf-failover.log}"
SUPPRESS="${CF_SUPPRESS_FILE:-/var/lib/polaris/failover-suppress}"
FAIL_N=3

log() { echo "$(date '+%F %T') $*" >> "$LOG"; }

[ -n "$CF_TOKEN" ] && [ -n "$CF_ZONE_ID" ] && [ -n "$CF_RECORD_ID" ] && \
[ -n "$CF_DOMAIN" ] && [ -n "$CF_PRIMARY_IP" ] && [ -n "$CF_STANDBY_IP" ] || {
  echo "cf-failover: CF_TOKEN/CF_ZONE_ID/CF_RECORD_ID/CF_DOMAIN/CF_PRIMARY_IP/CF_STANDBY_IP 未配全（/etc/cf-failover.env 或环境变量）" >&2
  exit 1
}

# Sync-upgrade suppression: during a planned cluster upgrade, the local
# panel drops this file before the primary restarts, so the planned
# outage is not mistaken for a failure. The panel sets until = start +
# 30 minutes — a hard cap this script never extends. Past it the normal
# rules below apply again, so a primary that really died during its
# upgrade is still failed over. The failure counter is left untouched:
# counting resumes from its previous value once the window ends.
if [ -f "$SUPPRESS" ]; then
  s_until="$(sed -n 's/^until=//p' "$SUPPRESS" | head -n 1)"
  s_op="$(sed -n 's/^op=//p' "$SUPPRESS" | head -n 1)"
  case "$s_until" in
    ''|*[!0-9]*) log "sync upgrade suppress file unreadable, ignoring" ;;
    *) if [ "$(date +%s)" -lt "$s_until" ]; then
         log "suppressed by sync upgrade op ${s_op:-unknown} until $s_until"
         exit 0
       fi ;;
  esac
fi

# Check the primary over its public IP directly (bypasses current DNS).
code=$(curl -sk -o /dev/null -w "%{http_code}" --resolve "$CF_DOMAIN:443:$CF_PRIMARY_IP" --max-time 8 "https://$CF_DOMAIN${CF_ADMIN_PATH%/}/api/v1/info" || echo 000)
if [ "$code" = "200" ]; then echo 0 > "$STATE"; exit 0; fi

# The local panel must be healthy before switching, or the DNS would point
# at a broken standby.
local_code=$(curl -s -o /dev/null -w "%{http_code}" --max-time 5 "http://127.0.0.1:8080${CF_ADMIN_PATH%/}/api/v1/info" || echo 000)
if [ "$local_code" != "200" ]; then log "primary fail but local panel unhealthy, skip"; exit 0; fi

n=$(cat "$STATE" 2>/dev/null || echo 0); n=$((n+1)); echo "$n" > "$STATE"
log "primary check failed ($n/$FAIL_N)"
[ "$n" -lt "$FAIL_N" ] && exit 0

# Quorum (three-condition rule): the witness must agree the primary is
# down, and answer at all — asking it is itself the reachability proof.
# Without a witness the old local-only behaviour applies; a witness that
# cannot be reached, or one that still sees the primary, blocks the flip.
# The witness emits compact JSON ({"reachable":false,...}), so grep for the
# exact token; anything else is "unclear" and also blocks the flip.
if [ -n "$WITNESS_URL" ]; then
  verdict="$(curl -fsS --max-time 5 "$WITNESS_URL/verdict?node=dmit" 2>/dev/null)" || { log "witness unreachable, skip flip"; exit 0; }
  if printf '%s' "$verdict" | grep -q '"reachable":false'; then
    log "witness agrees primary is down, proceeding"
  elif printf '%s' "$verdict" | grep -q '"reachable":true'; then
    log "witness says primary reachable, skip flip"
    echo 0 > "$STATE"
    exit 0
  else
    log "witness verdict unclear, skip flip"
    exit 0
  fi
else
  log "no witness configured, flipping on local checks only"
fi

cur=$(curl -s -H "Authorization: Bearer $CF_TOKEN" \
  "https://api.cloudflare.com/client/v4/zones/$CF_ZONE_ID/dns_records/$CF_RECORD_ID" \
  | python3 -c "import sys,json;print(json.load(sys.stdin)['result']['content'])" 2>/dev/null || echo "")
[ "$cur" = "$CF_STANDBY_IP" ] && { log "already on standby"; exit 0; }

curl -s -X PUT -H "Authorization: Bearer $CF_TOKEN" -H "Content-Type: application/json" \
  "https://api.cloudflare.com/client/v4/zones/$CF_ZONE_ID/dns_records/$CF_RECORD_ID" \
  --data "{\"type\":\"A\",\"name\":\"${CF_DOMAIN%%.*}\",\"content\":\"$CF_STANDBY_IP\",\"ttl\":60,\"proxied\":false}" >> "$LOG"
log "DNS flipped to $CF_STANDBY_IP"

# Report the promotion to the witness so the fencing check on the primary
# (polaris-ha-fence.sh) blocks it from starting as primary again. Best
# effort: the flip already happened, a witness outage must not fail it.
if [ -n "$WITNESS_URL" ]; then
  if curl -fsS --max-time 5 -X POST "$WITNESS_URL/promoted" -H "Content-Type: application/json" -d '{"node":"hk"}' >/dev/null 2>&1; then
    log "witness recorded promotion (hk)"
  else
    log "witness promote report failed (ignored)"
  fi
fi
