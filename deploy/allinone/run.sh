#!/bin/bash
# Supervisor for the all-in-one image: PostgreSQL, Asterisk, API and worker.
# Everything persistent lives under /data. If any process exits, the others
# are stopped and the container exits so Docker's restart policy restarts it.
set -euo pipefail

log() { printf '[gsm2sip] %s\n' "$*" >&2; }
die() { log "$*"; exit 1; }

# Empty SIP_SERVER_NAME: each phone gets the host it used to reach the API.
SIP_SERVER_NAME="${SIP_SERVER_NAME:-}"
# Loopback-only internal ports, chosen to stay clear of host services when the
# container runs with --network host.
PG_PORT="${PG_PORT:-55432}"
ARI_PORT="${ARI_PORT:-58088}"
HTTP_ADDR="${HTTP_ADDR:-:${HTTP_PORT:-8080}}"
PG_BIN=/usr/lib/postgresql/17/bin
DATA=/data
PGDATA="$DATA/postgres"
RUN_DIR=/run/gsm2sip
install -d -m 0755 "$RUN_DIR"
install -d -m 0700 "$DATA/secrets"

# Generated once and kept in the data volume unless supplied by the operator.
secret_file() {
    local name="$1" generator="$2" file="$DATA/secrets/$1"
    if [ ! -s "$file" ]; then
        (umask 077; eval "$generator" > "$file.tmp" && mv "$file.tmp" "$file")
        log "generated $name in $file"
    fi
    cat "$file"
}

DB_PASSWORD="$(secret_file db_password 'openssl rand -hex 24')"
if [ -z "${SECRETS_ENCRYPTION_KEY:-}" ]; then
    SECRETS_ENCRYPTION_KEY="$(secret_file secrets_encryption_key 'openssl rand -base64 32')"
fi
SIP_COMPATIBILITY_ENABLED="${SIP_COMPATIBILITY_ENABLED:-true}"
case "$SIP_COMPATIBILITY_ENABLED" in true|false) ;; *) die "SIP_COMPATIBILITY_ENABLED must be true or false";; esac
ARI_PASSWORD=""
if [ "$SIP_COMPATIBILITY_ENABLED" = true ]; then
# ARI never leaves the container; a fresh password per start is enough.
ARI_PASSWORD="$(openssl rand -hex 24)"
(umask 077; printf '%s' "$ARI_PASSWORD" > "$RUN_DIR/ari_password")
fi
printf 'PG_PORT=%s\nARI_PORT=%s\nHTTP_ADDR=%s\nSIP_COMPATIBILITY_ENABLED=%s\n' "$PG_PORT" "$ARI_PORT" "$HTTP_ADDR" "$SIP_COMPATIBILITY_ENABLED" > "$RUN_DIR/ports"
DATABASE_URL="postgresql://gsm2sip:${DB_PASSWORD}@127.0.0.1:${PG_PORT}/gsm2sip?sslmode=disable"

if [ "$SIP_COMPATIBILITY_ENABLED" = true ]; then
# SIP_TRANSPORT=udp: plain SIP and RTP for a trusted LAN or VPN, no certificate.
SIP_TRANSPORT="$(printf '%s' "${SIP_TRANSPORT:-tls}" | tr '[:upper:]' '[:lower:]')"
case "$SIP_TRANSPORT" in
    tls) SIP_PORT="${SIP_PORT:-5061}" ;;
    udp) SIP_PORT="${SIP_PORT:-5060}" ;;
    *) die "SIP_TRANSPORT must be tls or udp" ;;
esac

# SIP TLS: mounted certificate, or a self-signed one kept in the volume.
install -d -m 0755 /run/secrets 2>/dev/null || true
if [ "$SIP_TRANSPORT" = udp ]; then
    log "SIP over UDP without TLS; call media is not encrypted"
    SIP_CA_PEM_FILE=""
elif [ -s /run/secrets/sip_tls_cert ] && [ -s /run/secrets/sip_tls_key ]; then
    log "using the mounted SIP certificate"
    SIP_CA_PEM_FILE="${SIP_CA_PEM_FILE:-}"
else
    [ -n "$SIP_SERVER_NAME" ] || die "SIP over TLS needs a mounted certificate or SIP_SERVER_NAME for a self-signed one (or use SIP_TRANSPORT=udp)"
    tls="$DATA/tls"
    install -d -m 0755 "$tls"
    if [ ! -s "$tls/sip.crt" ] || [ "$(cat "$tls/name" 2>/dev/null)" != "$SIP_SERVER_NAME" ]; then
        (umask 077; openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
            -keyout "$tls/sip.key" -out "$tls/sip.crt" -subj "/CN=$SIP_SERVER_NAME" \
            -addext "subjectAltName=DNS:$SIP_SERVER_NAME" 2>/dev/null)
        chmod 0644 "$tls/sip.crt"
        printf '%s' "$SIP_SERVER_NAME" > "$tls/name"
        log "generated a self-signed SIP certificate for $SIP_SERVER_NAME"
    fi
    cp "$tls/sip.crt" /run/secrets/sip_tls_cert
    (umask 077; cp "$tls/sip.key" /run/secrets/sip_tls_key)
    # Phones receive this certificate as their trust anchor.
    SIP_CA_PEM_FILE="$tls/sip.crt"
fi

# Without PUBLIC_IP Asterisk advertises, per phone, the local address that
# reaches that phone; this needs --network host. Behind NAT or Docker port
# mapping set PUBLIC_IP to the address phones use.
PUBLIC_IP="${PUBLIC_IP:-}"
ASTERISK_EXTERNAL_SIGNALING_ADDRESS="${ASTERISK_EXTERNAL_SIGNALING_ADDRESS:-$PUBLIC_IP}"
ASTERISK_EXTERNAL_MEDIA_ADDRESS="${ASTERISK_EXTERNAL_MEDIA_ADDRESS:-$PUBLIC_IP}"

fi

as_user() { local user="$1"; shift; setpriv --reuid="$user" --regid="$user" --init-groups "$@"; }

# PostgreSQL
if [ ! -s "$PGDATA/PG_VERSION" ]; then
    log "initializing PostgreSQL in $PGDATA"
    install -d -o postgres -g postgres -m 0700 "$PGDATA"
    pwfile="$(mktemp)"
    printf '%s' "$DB_PASSWORD" > "$pwfile"
    chown postgres "$pwfile"
    as_user postgres "$PG_BIN/initdb" -D "$PGDATA" -U gsm2sip --pwfile="$pwfile" \
        -E UTF8 --no-locale --auth-host=scram-sha-256 --auth-local=peer >/dev/null
    rm -f "$pwfile"
fi
chown -R postgres:postgres "$PGDATA"
# Not through as_user: a backgrounded shell function runs in a subshell, so
# $! would be that subshell, which ignores SIGINT, and PostgreSQL would never
# see the fast-shutdown signal.
setpriv --reuid=postgres --regid=postgres --init-groups "$PG_BIN/postgres" -D "$PGDATA" \
    -c listen_addresses=127.0.0.1 -c port="$PG_PORT" -c unix_socket_directories=/run/postgresql &
PG_PID=$!

pids=("$PG_PID")
# Wait up to $1 seconds for the given processes, then kill what is left, so a
# stuck service can never keep the container alive after another one died
# (Docker restarts the container only once this script exits).
# An exited child stays a zombie until `wait` reaps it, and `kill -0` still
# succeeds on a zombie, so check the process state instead.
running() {
    local state
    state=$(sed 's/^.*) //' "/proc/$1/stat" 2>/dev/null) || return 1
    [ "${state%% *}" != Z ]
}
wait_or_kill() {
    local deadline=$((SECONDS + $1)) pid
    shift
    for pid in "$@"; do
        while running "$pid" && [ "$SECONDS" -lt "$deadline" ]; do sleep 0.2; done
        if running "$pid"; then
            log "process $pid did not stop in time; killing it"
            kill -KILL "$pid" 2>/dev/null || true
        fi
        wait "$pid" 2>/dev/null || true
    done
}
stop_all() {
    trap - TERM INT
    log "stopping"
    for pid in "${pids[@]:1}"; do kill -TERM "$pid" 2>/dev/null || true; done
    # Both phases fit inside Docker's default 10-second stop timeout, so
    # PostgreSQL always finishes its shutdown checkpoint before a SIGKILL.
    wait_or_kill 5 "${pids[@]:1}"
    kill -INT "$PG_PID" 2>/dev/null || true
    wait_or_kill 4 "$PG_PID"
}
trap 'stop_all; exit 0' TERM INT

for _ in $(seq 1 60); do
    PGPASSWORD="$DB_PASSWORD" "$PG_BIN/pg_isready" -q -h 127.0.0.1 -p "$PG_PORT" -U gsm2sip -d postgres && break
    kill -0 "$PG_PID" 2>/dev/null || die "PostgreSQL exited during startup"
    sleep 1
done
if [ "$(PGPASSWORD="$DB_PASSWORD" "$PG_BIN/psql" -h 127.0.0.1 -p "$PG_PORT" -U gsm2sip -d postgres -tAc \
        "SELECT 1 FROM pg_database WHERE datname = 'gsm2sip'")" != 1 ]; then
    PGPASSWORD="$DB_PASSWORD" "$PG_BIN/psql" -h 127.0.0.1 -p "$PG_PORT" -U gsm2sip -d postgres -qc 'CREATE DATABASE gsm2sip'
fi

if [ "$SIP_COMPATIBILITY_ENABLED" = true ]; then
# Asterisk (its entrypoint renders config, then drops to the asterisk user).
env ARI_USERNAME=gsm2sip ARI_PASSWORD="$ARI_PASSWORD" ASTERISK_ARI_BIND_ADDR=127.0.0.1 ASTERISK_ARI_PORT="$ARI_PORT" \
    ASTERISK_EXTERNAL_SIGNALING_ADDRESS="$ASTERISK_EXTERNAL_SIGNALING_ADDRESS" \
    ASTERISK_EXTERNAL_MEDIA_ADDRESS="$ASTERISK_EXTERNAL_MEDIA_ADDRESS" \
    POSTGRES_HOST=127.0.0.1 POSTGRES_PORT="$PG_PORT" POSTGRES_DB=gsm2sip POSTGRES_USER=gsm2sip \
    POSTGRES_PASSWORD="$DB_PASSWORD" RTP_START="$RTP_START" RTP_END="$RTP_END" \
    SIP_TRANSPORT="$SIP_TRANSPORT" SIP_PORT="$SIP_PORT" \
    /usr/local/bin/gsm2sip-asterisk-entrypoint &
pids+=("$!")
for _ in $(seq 1 60); do
    ARI_USERNAME=gsm2sip ARI_PASSWORD="$ARI_PASSWORD" ASTERISK_ARI_PORT="$ARI_PORT" /usr/local/bin/gsm2sip-asterisk-healthcheck 2>/dev/null && break
    kill -0 "${pids[1]}" 2>/dev/null || { log "Asterisk exited during startup"; stop_all; exit 1; }
    sleep 1
done

fi

# Default RTC deployment manages its own relay and credentials. Explicit TURN
# URLs still select an external relay, preserving existing advanced deployments.
if [ "${RTC_MEDIA_ENABLED:-false}" = true ] && [ -z "${RTC_TURN_URLS:-}" ]; then
    if ! SECRETS_ENCRYPTION_KEY="$SECRETS_ENCRYPTION_KEY" PUBLIC_IP="${PUBLIC_IP:-}" \
        python3 /usr/local/libexec/gsm2sip-turn-config.py; then
        stop_all
        exit 1
    fi
    . "$RUN_DIR/turn.env"
    turnserver -c "$RUN_DIR/turn.conf" &
    TURN_PID=$!
    pids+=("$TURN_PID")
    printf '%s\n' "$TURN_PID" > "$RUN_DIR/turn-supervisor.pid"
fi

# API and worker
app_env=(
    DATABASE_URL="$DATABASE_URL" MIGRATIONS_DIR="$MIGRATIONS_DIR"
    SIP_COMPATIBILITY_ENABLED="$SIP_COMPATIBILITY_ENABLED"
    SECRETS_ENCRYPTION_KEY="$SECRETS_ENCRYPTION_KEY" HTTP_ADDR="$HTTP_ADDR"
    RTC_CONTROL_ENABLED="${RTC_CONTROL_ENABLED:-false}"
    RTC_MEDIA_ENABLED="${RTC_MEDIA_ENABLED:-false}"
    RTC_STUN_URLS="${RTC_STUN_URLS:-}" RTC_TURN_URLS="${RTC_TURN_URLS:-}"
    RTC_TURN_SHARED_SECRET="${RTC_TURN_SHARED_SECRET:-}"
)
if [ "$SIP_COMPATIBILITY_ENABLED" = true ]; then
    app_env+=(
    ARI_URL="http://127.0.0.1:${ARI_PORT}/ari" ARI_USERNAME=gsm2sip ARI_PASSWORD="$ARI_PASSWORD"
    ARI_APPLICATION=gsm2sip SIP_SERVER_NAME="$SIP_SERVER_NAME" SIP_PORT="$SIP_PORT" SIP_TRANSPORT="$SIP_TRANSPORT"
    SIP_CA_PEM_FILE="$SIP_CA_PEM_FILE" SIP_ENABLE_OPUS="${SIP_ENABLE_OPUS:-false}"
    )
fi
env -i PATH=/usr/bin:/bin "${app_env[@]}" setpriv --reuid=gsm2sip --regid=gsm2sip --init-groups \
    /usr/local/libexec/gsm2sip-api &
pids+=("$!")
env -i PATH=/usr/bin:/bin DATABASE_URL="$DATABASE_URL" MIGRATIONS_DIR="$MIGRATIONS_DIR" \
    setpriv --reuid=gsm2sip --regid=gsm2sip --init-groups /usr/local/libexec/gsm2sip-worker &
pids+=("$!")
log "started PostgreSQL, API and worker (SIP compatibility: $SIP_COMPATIBILITY_ENABLED)"

set +e
wait -n "${pids[@]}"
status=$?
log "a service exited with status $status"
stop_all
exit 1
