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
set -u
[ -f /etc/cf-failover.env ] && set -a && . /etc/cf-failover.env && set +a
CF_TOKEN="${CF_TOKEN:-}"
CF_ZONE_ID="${CF_ZONE_ID:-}"
CF_RECORD_ID="${CF_RECORD_ID:-}"
CF_DOMAIN="${CF_DOMAIN:-}"
CF_ADMIN_PATH="${CF_ADMIN_PATH:-/}"
CF_PRIMARY_IP="${CF_PRIMARY_IP:-}"
CF_STANDBY_IP="${CF_STANDBY_IP:-}"
# Test hooks (the installer always uses the defaults above).
STATE="${CF_STATE_FILE:-/var/lib/cf-failover.state}"
LOG="${CF_LOG_FILE:-/var/log/cf-failover.log}"
FAIL_N=3

log() { echo "$(date '+%F %T') $*" >> "$LOG"; }

[ -n "$CF_TOKEN" ] && [ -n "$CF_ZONE_ID" ] && [ -n "$CF_RECORD_ID" ] && \
[ -n "$CF_DOMAIN" ] && [ -n "$CF_PRIMARY_IP" ] && [ -n "$CF_STANDBY_IP" ] || {
  echo "cf-failover: CF_TOKEN/CF_ZONE_ID/CF_RECORD_ID/CF_DOMAIN/CF_PRIMARY_IP/CF_STANDBY_IP 未配全（/etc/cf-failover.env 或环境变量）" >&2
  exit 1
}

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

cur=$(curl -s -H "Authorization: Bearer $CF_TOKEN" \
  "https://api.cloudflare.com/client/v4/zones/$CF_ZONE_ID/dns_records/$CF_RECORD_ID" \
  | python3 -c "import sys,json;print(json.load(sys.stdin)['result']['content'])" 2>/dev/null || echo "")
[ "$cur" = "$CF_STANDBY_IP" ] && { log "already on standby"; exit 0; }

curl -s -X PUT -H "Authorization: Bearer $CF_TOKEN" -H "Content-Type: application/json" \
  "https://api.cloudflare.com/client/v4/zones/$CF_ZONE_ID/dns_records/$CF_RECORD_ID" \
  --data "{\"type\":\"A\",\"name\":\"${CF_DOMAIN%%.*}\",\"content\":\"$CF_STANDBY_IP\",\"ttl\":60,\"proxied\":false}" >> "$LOG"
log "DNS flipped to $CF_STANDBY_IP"
