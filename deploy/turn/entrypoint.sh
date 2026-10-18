#!/bin/sh
set -eu
umask 077
: "${RTC_TURN_SHARED_SECRET:?set the same private secret as the API}"
: "${TURN_PUBLIC_IP:?set the publicly reachable IPv4 address}"
: "${TURN_PRIVATE_IP:?set this host network interface IPv4 address}"
: "${TURN_REALM:?set a stable DNS realm}"
# These values become individual config lines. Reject line injection and
# ambiguous hostnames for the explicit 1:1 public/private NAT mapping.
case "$TURN_PUBLIC_IP:$TURN_PRIVATE_IP" in *[!0-9.:]*) exit 2;; esac
case "$TURN_REALM:$RTC_TURN_SHARED_SECRET" in *[!a-zA-Z0-9.:_-]*) exit 2;; esac
[ "${#RTC_TURN_SHARED_SECRET}" -ge 32 ] || exit 2
cat > /tmp/gsm2sip-turn.conf <<EOF
pidfile=/tmp/turnserver.pid
relay-threads=1
listening-port=3478
listening-ip=$TURN_PRIVATE_IP
relay-ip=$TURN_PRIVATE_IP
external-ip=$TURN_PUBLIC_IP/$TURN_PRIVATE_IP
min-port=49160
max-port=49259
realm=$TURN_REALM
use-auth-secret
static-auth-secret=$RTC_TURN_SHARED_SECRET
fingerprint
no-cli
no-tls
no-dtls
no-tcp-relay
no-multicast-peers
user-quota=4
total-quota=64
denied-peer-ip=0.0.0.0-0.255.255.255
denied-peer-ip=10.0.0.0-10.255.255.255
denied-peer-ip=100.64.0.0-100.127.255.255
denied-peer-ip=127.0.0.0-127.255.255.255
denied-peer-ip=169.254.0.0-169.254.255.255
denied-peer-ip=172.16.0.0-172.31.255.255
denied-peer-ip=192.168.0.0-192.168.255.255
denied-peer-ip=::1
denied-peer-ip=fc00::-fdff:ffff:ffff:ffff:ffff:ffff:ffff:ffff
denied-peer-ip=fe80::-febf:ffff:ffff:ffff:ffff:ffff:ffff:ffff
log-file=stdout
simple-log
EOF
exec turnserver -c /tmp/gsm2sip-turn.conf
