#!/usr/bin/env bash
# Requires the freshly built API image; never starts Asterisk or uses SIP keys.
set -euo pipefail
rtc_root=$(cd "$(dirname "$0")/.." && pwd)
rtc_temp=$(mktemp -d)
rtc_project="gsm-rtc-probe-$$"
rtc_port=${RTC_PROBE_HTTP_PORT:-18097}
rtc_compose=(docker compose --project-name "$rtc_project" --env-file "$rtc_temp/.env" -f "$rtc_root/deploy/compose.rtc.yaml")
cleanup() {
  "${rtc_compose[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  rm -rf "$rtc_temp"
}
trap cleanup EXIT
umask 077
{
  printf 'GSM2SIP_VERSION=%s\n' "${GSM2SIP_VERSION:-ci-smoke}"
  printf 'POSTGRES_PASSWORD=%s\n' "$(openssl rand -hex 24)"
  printf 'SECRETS_ENCRYPTION_KEY=%s\n' "$(openssl rand -base64 32)"
  printf 'HTTP_PORT=%s\n' "$rtc_port"
} > "$rtc_temp/.env"
"${rtc_compose[@]}" up -d --pull missing
for rtc_attempt in $(seq 1 60); do
  if curl --fail --silent --max-time 2 "http://127.0.0.1:$rtc_port/readyz" >/dev/null; then break; fi
  sleep 1
done
curl --fail --silent --show-error "http://127.0.0.1:$rtc_port/v1/capabilities" > "$rtc_temp/capabilities.json"
python3 - "$rtc_temp/capabilities.json" <<'PY'
import json,sys
c=json.load(open(sys.argv[1]))
assert c['rtc_call_control'] is True and c['rtc_audio'] is True
assert c['sip_call_control'] is False
PY
test "$("${rtc_compose[@]}" ps --services --status running | wc -l)" -eq 3
test "$("${rtc_compose[@]}" exec -T postgres psql -U gsm2sip -d gsm2sip -At -c 'SELECT count(*) FROM sip_endpoint_bindings')" = 0
"${rtc_compose[@]}" run --rm --no-deps --entrypoint /usr/local/bin/gsm2sip-admin api owner create --name rtc-probe >/dev/null
printf 'Pure RTC API/PostgreSQL/worker probe passed without Asterisk or SIP bindings\n'
