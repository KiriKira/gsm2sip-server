#!/bin/sh
set -eu

# This certificate is deliberately self-signed and only for the loopback probe.
# Production deployments must mount a certificate and private key issued for
# SIP_SERVER_NAME by the production CA.
cert=.local/asterisk/sip.crt
key=.local/asterisk/sip.key
mkdir -p .local/asterisk
chmod 0700 .local .local/asterisk

if [ -s "$cert" ] && [ -s "$key" ]; then
    exit 0
fi

umask 077
openssl req -x509 -newkey rsa:3072 -sha256 -nodes -days 30 \
    -keyout "$key" -out "$cert" \
    -subj '/CN=sip.localhost' \
    -addext 'subjectAltName=DNS:sip.localhost,IP:127.0.0.1' \
    -addext 'keyUsage=digitalSignature,keyEncipherment' \
    -addext 'extendedKeyUsage=serverAuth'
chmod 0600 "$key"
chmod 0644 "$cert"
