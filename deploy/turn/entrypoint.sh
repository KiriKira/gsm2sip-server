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
TURN_LISTEN_PORT=${TURN_LISTEN_PORT:-3478}
TURN_RELAY_MIN_PORT=${TURN_RELAY_MIN_PORT:-49160}
TURN_RELAY_MAX_PORT=${TURN_RELAY_MAX_PORT:-49259}
# Decimal only, without signs/leading zeroes (shell arithmetic can treat them as
# octal). Validate before writing configuration or starting a listener.
port() {
    case "$2" in ''|0*|*[!0-9]*) echo "Invalid $1: expected port 1..65535" >&2; exit 2;; esac
    [ "${#2}" -le 5 ] && [ "$2" -le 65535 ] || {
        echo "Invalid $1: expected port 1..65535" >&2; exit 2;
    }
}
port TURN_LISTEN_PORT "$TURN_LISTEN_PORT"
port TURN_RELAY_MIN_PORT "$TURN_RELAY_MIN_PORT"
port TURN_RELAY_MAX_PORT "$TURN_RELAY_MAX_PORT"
[ "$TURN_RELAY_MIN_PORT" -le "$TURN_RELAY_MAX_PORT" ] || {
    echo 'TURN relay range is reversed' >&2; exit 2;
}
if [ "$TURN_LISTEN_PORT" -ge "$TURN_RELAY_MIN_PORT" ] && [ "$TURN_LISTEN_PORT" -le "$TURN_RELAY_MAX_PORT" ]; then
    echo 'TURN listener must be outside the relay range' >&2; exit 2
fi
cat > /tmp/gsm2sip-turn.conf <<EOF
pidfile=/tmp/turnserver.pid
relay-threads=1
listening-port=$TURN_LISTEN_PORT
listening-ip=$TURN_PRIVATE_IP
relay-ip=$TURN_PRIVATE_IP
external-ip=$TURN_PUBLIC_IP/$TURN_PRIVATE_IP
min-port=$TURN_RELAY_MIN_PORT
max-port=$TURN_RELAY_MAX_PORT
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
