#!/bin/sh
# Healthy when the API is ready and the managed TURN relay runs.
set -eu
. /run/gsm2sip/ports
curl --fail --silent --max-time 3 "http://127.0.0.1:${HTTP_ADDR##*:}/readyz" >/dev/null
if [ -s /run/gsm2sip/turn-supervisor.pid ]; then
    kill -0 "$(cat /run/gsm2sip/turn-supervisor.pid)"
fi
