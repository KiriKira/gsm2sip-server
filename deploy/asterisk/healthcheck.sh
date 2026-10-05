#!/bin/sh
set -eu

: "${ARI_USERNAME:?ARI_USERNAME is required}"
: "${ARI_PASSWORD:?ARI_PASSWORD is required}"

curl --fail --silent --show-error --max-time 2 --config - \
    >/dev/null <<EOF
url = "http://127.0.0.1:8088/ari/asterisk/ping"
user = "${ARI_USERNAME}:${ARI_PASSWORD}"
EOF
