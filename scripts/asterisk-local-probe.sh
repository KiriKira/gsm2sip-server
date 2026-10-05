#!/bin/sh
set -eu

endpoint=
cleanup_probe_endpoint() {
    [ -n "${endpoint:-}" ] || return 0
    docker compose exec -T postgres psql -X -v ON_ERROR_STOP=1 -U gsm2sip -d gsm2sip \
        >/dev/null 2>&1 <<SQL || true
DELETE FROM ps_endpoints WHERE id='$endpoint';
DELETE FROM ps_auths WHERE id='$endpoint';
DELETE FROM ps_aors WHERE id='$endpoint';
SQL
}
trap cleanup_probe_endpoint EXIT

if [ ! -f .env ]; then
    cp .env.example .env
fi

# Keep the local replay-encryption key stable across probe runs. The old sample
# value is deliberately recognized so upgrading an existing local checkout
# replaces that public key once; user-generated keys are preserved.
python3 - <<'PY'
import base64
import os
import secrets
import tempfile
from pathlib import Path

path = Path(".env")
lines = path.read_text().splitlines()
indexes = [i for i, line in enumerate(lines) if line.startswith("SECRETS_ENCRYPTION_KEY=")]
if len(indexes) > 1:
    raise SystemExit(".env contains more than one SECRETS_ENCRYPTION_KEY entry")
index = indexes[0] if indexes else len(lines)
original_value = lines[index].split("=", 1)[1].strip() if indexes else ""
sample_key = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
changed = not indexes or not original_value or original_value == sample_key
if changed:
    value = base64.b64encode(secrets.token_bytes(32)).decode("ascii")
    line = "SECRETS_ENCRYPTION_KEY=" + value
    if indexes:
        lines[index] = line
    else:
        lines.append(line)
else:
    try:
        decoded = base64.b64decode(original_value, validate=True)
    except Exception as exc:
        raise SystemExit("SECRETS_ENCRYPTION_KEY must be base64-encoded 32-byte data") from exc
    if len(decoded) != 32:
        raise SystemExit("SECRETS_ENCRYPTION_KEY must decode to exactly 32 bytes")

if changed:
    descriptor, temporary = tempfile.mkstemp(prefix=".env.", dir=path.parent)
    with os.fdopen(descriptor, "w") as output:
        output.write("\n".join(lines) + "\n")
        output.flush()
        os.fsync(output.fileno())
    os.replace(temporary, path)

os.chmod(path, 0o600)
PY
scripts/asterisk-local-cert.sh

command -v docker >/dev/null 2>&1 || { echo 'docker is required' >&2; exit 1; }
command -v python3 >/dev/null 2>&1 || { echo 'python3 is required' >&2; exit 1; }
command -v openssl >/dev/null 2>&1 || { echo 'openssl is required' >&2; exit 1; }

docker compose config --quiet
docker compose config --format json | python3 -c '
import json,sys
c=json.load(sys.stdin)
ports=c["services"]["asterisk"].get("ports",[])
assert all(int(p["target"]) != 8088 for p in ports), "ARI must not be published"
'

if [ "${SKIP_BUILD:-0}" = 1 ]; then
    docker compose up -d postgres asterisk api
else
    docker compose up --build -d postgres asterisk api
fi

service_id=$(docker compose ps -q asterisk)
if [ -z "$service_id" ]; then
    echo 'Asterisk container did not start' >&2
    exit 1
fi
healthy=false
i=0
while [ "$i" -lt 60 ]; do
    state=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}starting{{end}}' "$service_id")
    if [ "$state" = healthy ]; then healthy=true; break; fi
    if [ "$state" = unhealthy ]; then break; fi
    i=$((i + 1))
    sleep 2
done
if [ "$healthy" != true ]; then
    docker compose logs --tail=100 asterisk >&2
    echo 'Asterisk ARI health check did not become healthy' >&2
    exit 1
fi

# A real HTTP request is performed from inside the private Asterisk network
# namespace. The Compose assertion above guarantees the ARI listener has no
# host-published port.
docker compose exec -T asterisk /usr/bin/curl --fail --silent --show-error \
    --max-time 3 --config - >/dev/null <<EOF
url = "http://127.0.0.1:8088/ari/asterisk/ping"
user = "$(sed -n 's/^ARI_USERNAME=//p' .env | tail -n 1):$(sed -n 's/^ARI_PASSWORD=//p' .env | tail -n 1)"
EOF

# Let the API apply the repository migrations, then seed one isolated local
# Digest endpoint with only the RFC-compatible MD5 A1 digest in ps_auths.
ready=false
i=0
while [ "$i" -lt 60 ]; do
    migrated=$(docker compose exec -T postgres psql -X -At -U gsm2sip -d gsm2sip \
        -c "SELECT to_regclass('public.ps_auths') IS NOT NULL AND to_regclass('public.ps_endpoints') IS NOT NULL")
    if [ "$migrated" = t ]; then ready=true; break; fi
    i=$((i + 1))
    sleep 2
done
if [ "$ready" != true ]; then
    docker compose logs --tail=100 api >&2
    echo 'API migrations did not create Asterisk realtime tables' >&2
    exit 1
fi

endpoint=dev_00000000000000000000000000000001
password=$(openssl rand -hex 32)
digest=$(printf '%s' "$endpoint:gsm2sip:$password" | md5sum | cut -d ' ' -f 1)
docker compose exec -T postgres psql -X -v ON_ERROR_STOP=1 -U gsm2sip -d gsm2sip \
    >/dev/null <<SQL
INSERT INTO ps_auths(id,auth_type,realm,username,password_digest,supported_algorithms_uas,supported_algorithms_uac)
VALUES ('$endpoint','userpass','gsm2sip','$endpoint','MD5:$digest','MD5','MD5')
ON CONFLICT (id) DO UPDATE SET password_digest=EXCLUDED.password_digest;
INSERT INTO ps_aors(id,max_contacts,remove_existing,qualify_frequency)
VALUES ('$endpoint',1,'yes',30) ON CONFLICT (id) DO NOTHING;
INSERT INTO ps_endpoints(id,transport,aors,auth,context,disallow,allow,direct_media,force_rport,rewrite_contact,rtp_symmetric,media_encryption,media_encryption_optimistic,dtmf_mode,identify_by,rtp_timeout,rtp_timeout_hold)
VALUES ('$endpoint','transport-tls','$endpoint','$endpoint','gsm-client','all','alaw,ulaw,g722','no','yes','yes','yes','sdes','no','rfc4733','auth_username,username',30,60)
ON CONFLICT (id) DO UPDATE SET media_encryption='sdes',media_encryption_optimistic='no',rtp_timeout=30,rtp_timeout_hold=60;
SQL

# Read the realtime object back through PJSIP itself so this verifies the
# running Asterisk channel policy, not just the SQL schema defaults.
endpoint_show=$(docker compose exec -T asterisk /usr/sbin/asterisk -rx "pjsip show endpoint $endpoint")
printf '%s\n' "$endpoint_show" | grep -Eq '^[[:space:]]*rtp_timeout[[:space:]]*:[[:space:]]*30[[:space:]]*$' || {
    echo 'Asterisk endpoint does not report the 30-second RTP timeout' >&2
    exit 1
}
printf '%s\n' "$endpoint_show" | grep -Eq '^[[:space:]]*rtp_timeout_hold[[:space:]]*:[[:space:]]*60[[:space:]]*$' || {
    echo 'Asterisk endpoint does not report the 60-second RTP hold timeout' >&2
    exit 1
}
echo 'PASS: realtime PJSIP endpoint reports 30s media and 60s hold timeouts'

plain_password_columns=$(docker compose exec -T postgres psql -X -At -U gsm2sip -d gsm2sip \
    -c "SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='ps_auths' AND column_name='password'")
if [ "$plain_password_columns" != 0 ]; then
    echo 'ps_auths must not have a plaintext password column' >&2
    exit 1
fi

SIP_PROBE_PASSWORD="$password" python3 scripts/asterisk-sip-negative-probe.py

if [ "${SKIP_CODEC_PROBE:-0}" = 1 ]; then
    echo 'SKIP: Opus sample transcoding is deferred for this pre-build runtime check'
else
translation=$(docker compose exec -T asterisk /usr/sbin/asterisk -rx 'core show translation comp 1')
opus_paths=$(docker compose exec -T asterisk /usr/sbin/asterisk -rx 'core show translation paths opus 48000')
alaw_paths=$(docker compose exec -T asterisk /usr/sbin/asterisk -rx 'core show translation paths alaw 8000')
ulaw_paths=$(docker compose exec -T asterisk /usr/sbin/asterisk -rx 'core show translation paths ulaw 8000')
g722_paths=$(docker compose exec -T asterisk /usr/sbin/asterisk -rx 'core show translation paths g722 16000')
TRANSLATION="$translation" OPUS_PATHS="$opus_paths" ALAW_PATHS="$alaw_paths" \
ULAW_PATHS="$ulaw_paths" G722_PATHS="$g722_paths" python3 - <<'PY'
import re
import os

lines = [line.split() for line in os.environ["TRANSLATION"].splitlines() if line.strip()]
required_codecs = {"opus", "alaw", "ulaw", "g722", "slin8", "slin16"}
header = next((line for line in lines if required_codecs.issubset(line)), None)
if header is None:
    raise SystemExit("codec translation table is missing Opus/G.711/G.722 formats")

rows = {}
for line in lines:
    if len(line) == len(header) + 1 and line[0] in required_codecs:
        rows[line[0]] = line[1:]

path_tables = {
    "opus": os.environ["OPUS_PATHS"],
    "alaw": os.environ["ALAW_PATHS"],
    "ulaw": os.environ["ULAW_PATHS"],
    "g722": os.environ["G722_PATHS"],
}

def table_codec(name, rate):
    if name != "slin":
        return name
    return f"slin{int(rate) // 1000}"

def path_for(src, dst):
    pattern = re.compile(rf"^\s*{re.escape(src)}:\d+\s+To\s+{re.escape(dst)}:\d+\s*:\s*(.*?)\s*$")
    for line in path_tables[src].splitlines():
        match = pattern.match(line)
        if match:
            route = match.group(1)
            if "No Translation Path" in route:
                break
            segments = re.findall(r"\(([^@()]+)@(\d+)\)", route)
            if len(segments) >= 2:
                return [table_codec(name, rate) for name, rate in segments]
            break
    raise SystemExit(f"codec transcode path {src} -> {dst} is unavailable")

for src, dst in (
    ("opus", "alaw"), ("opus", "ulaw"), ("opus", "g722"),
    ("alaw", "opus"), ("ulaw", "opus"), ("g722", "opus"),
):
    route = path_for(src, dst)
    for edge_src, edge_dst in zip(route, route[1:]):
        if edge_src not in rows or header.count(edge_dst) != 1:
            raise SystemExit(f"codec translation table is missing {edge_src} -> {edge_dst}")
        cost = rows[edge_src][header.index(edge_dst)]
        if not cost.isdecimal() or int(cost) <= 0:
            raise SystemExit(f"codec transcode edge {edge_src} -> {edge_dst} failed its one-second sample probe")

print("PASS: one-second Opus/alaw/ulaw/g722 sample transcoding in both directions")
PY
fi
echo 'PASS: private ARI HTTP, realtime Digest endpoint, and mandatory SRTP probe'

if [ "${SKIP_WRAPPER_PROBE:-0}" = 1 ]; then
    echo 'SKIP: Local/Stasis header wrapper probe was not requested'
else
    python3 scripts/asterisk-local-wrapper-probe.py
fi
