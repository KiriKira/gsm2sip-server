#!/bin/sh
# gsm2sip-admin inside the all-in-one container:
#   docker exec gsm2sip gsm2sip-admin owner create --name Home
set -eu
. /run/gsm2sip/ports
password="$(cat /data/secrets/db_password)"
exec env DATABASE_URL="postgresql://gsm2sip:${password}@127.0.0.1:${PG_PORT}/gsm2sip?sslmode=disable" \
    MIGRATIONS_DIR="${MIGRATIONS_DIR:-/app/migrations}" \
    setpriv --reuid=gsm2sip --regid=gsm2sip --init-groups /usr/local/libexec/gsm2sip-admin "$@"
