#!/bin/bash
# Supervisor for the all-in-one SoulGem image: PostgreSQL, TURN, API and worker.
# Everything persistent lives under /data. If any process exits, the others
# are stopped and the container exits so Docker's restart policy restarts it.
set -euo pipefail

log() { printf '[gsm2sip] %s\n' "$*" >&2; }
die() { log "$*"; exit 1; }

# Loopback-only internal ports, chosen to stay clear of host services when the
# container runs with --network host.
PG_PORT="${PG_PORT:-55432}"
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
for name in SIP_COMPATIBILITY_ENABLED SIP_SERVER_NAME SIP_TRANSPORT ARI_PORT; do
    [ -z "${!name:-}" ] || log "SIP calling was removed in 0.4; ignoring $name"
done
printf 'PG_PORT=%s\nHTTP_ADDR=%s\n' "$PG_PORT" "$HTTP_ADDR" > "$RUN_DIR/ports"
DATABASE_URL="postgresql://gsm2sip:${DB_PASSWORD}@127.0.0.1:${PG_PORT}/gsm2sip?sslmode=disable"

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

# The image manages its own relay and credentials. Explicit TURN URLs select
# an external relay instead.
if [ -z "${RTC_TURN_URLS:-}" ]; then
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
    SECRETS_ENCRYPTION_KEY="$SECRETS_ENCRYPTION_KEY" HTTP_ADDR="$HTTP_ADDR"
    RTC_STUN_URLS="${RTC_STUN_URLS:-}" RTC_TURN_URLS="${RTC_TURN_URLS:-}"
    RTC_TURN_SHARED_SECRET="${RTC_TURN_SHARED_SECRET:-}"
)
env -i PATH=/usr/bin:/bin "${app_env[@]}" setpriv --reuid=gsm2sip --regid=gsm2sip --init-groups \
    /usr/local/libexec/gsm2sip-api &
pids+=("$!")
env -i PATH=/usr/bin:/bin DATABASE_URL="$DATABASE_URL" MIGRATIONS_DIR="$MIGRATIONS_DIR" \
    setpriv --reuid=gsm2sip --regid=gsm2sip --init-groups /usr/local/libexec/gsm2sip-worker &
pids+=("$!")
log "started PostgreSQL, API and worker"

set +e
wait -n "${pids[@]}"
status=$?
log "a service exited with status $status"
stop_all
exit 1
