#!/bin/sh
# Healthy when the API is ready and Asterisk answers ARI.
set -eu
curl --fail --silent --max-time 3 http://127.0.0.1:8080/readyz >/dev/null
ARI_USERNAME=gsm2sip ARI_PASSWORD="$(cat /run/gsm2sip/ari_password)" \
    exec /usr/local/bin/gsm2sip-asterisk-healthcheck
