#!/usr/bin/env python3
"""Configure the colocated relay from existing install credentials and routes."""
import base64
import hashlib
import hmac
import ipaddress
import os
from pathlib import Path
import re
import shlex
import socket
import subprocess
import sys

os.umask(0o077)
run = Path('/run/gsm2sip')
env = os.environ

def ipv4(value):
    address = ipaddress.IPv4Address(value)
    if address.is_unspecified or address.is_loopback or address.is_multicast:
        raise ValueError('TURN needs a usable interface/address')
    return str(address)

def local_address():
    if env.get('TURN_PRIVATE_IP'):
        return ipv4(env['TURN_PRIVATE_IP'])
    try:
        with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as route:
            route.connect(('1.1.1.1', 53))  # Select a route; sends no packet.
            return ipv4(route.getsockname()[0])
    except OSError:
        for candidate in socket.gethostbyname_ex(socket.gethostname())[2]:
            try:
                return ipv4(candidate)
            except ValueError:
                pass
        raise ValueError('No usable IPv4 interface for the TURN relay')

def public_address(private):
    explicit = env.get('TURN_PUBLIC_IP') or env.get('PUBLIC_IP')
    if explicit:
        return ipv4(socket.gethostbyname(explicit))
    if ipaddress.ip_address(private).is_global:
        return private
    for host, port in (('stun.cloudflare.com', 3478), ('stun.l.google.com', 19302)):
        try:
            result = subprocess.run(
                ['turnutils_stunclient', '-p', str(port), '-L', private, host],
                stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, timeout=4)
            match = re.search(r'UDP reflexive addr[^\n]*?(\d+\.\d+\.\d+\.\d+)', result.stdout)
            if match:
                return ipv4(match[1])
        except (OSError, subprocess.TimeoutExpired, ValueError):
            pass
    print('[gsm2sip] TURN public address discovery unavailable; using interface address. '
          'For NAT without STUN access, reuse PUBLIC_IP or set TURN_PUBLIC_IP.', file=sys.stderr)
    return private

def port(name, default):
    value = env.get(name, str(default))
    if not re.fullmatch(r'[1-9][0-9]{0,4}', value) or int(value) > 65535:
        raise ValueError(name + ' must be a port in 1..65535')
    return int(value)

try:
    private = local_address()
    public = public_address(private)
    listener = port('TURN_LISTEN_PORT', 3478)
    minimum = port('TURN_RELAY_MIN_PORT', 49160)
    maximum = port('TURN_RELAY_MAX_PORT', 49259)
    if minimum > maximum or minimum <= listener <= maximum:
        raise ValueError('TURN relay range must be ordered and exclude its listening port')
    secret = env.get('RTC_TURN_SHARED_SECRET')
    if not secret:
        key = base64.b64decode(env['SECRETS_ENCRYPTION_KEY'], validate=True)
        if len(key) != 32:
            raise ValueError('Invalid install encryption key')
        secret = hmac.new(key, b'gsm2sip/turn-auth/v1', hashlib.sha256).hexdigest()
    realm = env.get('TURN_REALM') or 'gsm2sip'
    if len(secret) < 32 or not re.fullmatch(r'[a-zA-Z0-9.:_-]+', secret + realm):
        raise ValueError('Invalid TURN credential or realm')
    lines = [
        'pidfile=/run/gsm2sip/turn.pid', 'relay-threads=1',
        'listening-ip=0.0.0.0', 'listening-port=' + str(listener),
        'relay-ip=' + private, 'min-port=' + str(minimum), 'max-port=' + str(maximum),
        'realm=' + realm, 'use-auth-secret', 'static-auth-secret=' + secret,
        'fingerprint', 'no-cli', 'no-tls', 'no-dtls', 'no-tcp-relay',
        'no-multicast-peers', 'user-quota=4', 'total-quota=64',
        'log-file=stdout', 'simple-log',
    ]
    if public != private:
        lines.append('external-ip=' + public + '/' + private)
    for denied in ('0.0.0.0-0.255.255.255', '10.0.0.0-10.255.255.255',
                   '100.64.0.0-100.127.255.255', '127.0.0.0-127.255.255.255',
                   '169.254.0.0-169.254.255.255', '172.16.0.0-172.31.255.255',
                   '192.168.0.0-192.168.255.255', '::1',
                   'fc00::-fdff:ffff:ffff:ffff:ffff:ffff:ffff:ffff',
                   'fe80::-febf:ffff:ffff:ffff:ffff:ffff:ffff:ffff'):
        lines.append('denied-peer-ip=' + denied)
    (run / 'turn.conf').write_text('\n'.join(lines) + '\n')
    values = {
        'RTC_TURN_SHARED_SECRET': secret,
        'RTC_STUN_URLS': env.get('RTC_STUN_URLS') or f'stun:{public}:{listener}',
        'RTC_TURN_URLS': f'turn:{public}:{listener}?transport=udp,turn:{public}:{listener}?transport=tcp',
    }
    (run / 'turn.env').write_text(''.join(k + '=' + shlex.quote(v) + '\n' for k, v in values.items()))
    print(f'[gsm2sip] managed TURN listens on 0.0.0.0:{listener}; relay {minimum}-{maximum}; advertised {public}', file=sys.stderr)
except (ValueError, KeyError, OSError) as error:
    print('[gsm2sip] TURN configuration failed: ' + str(error), file=sys.stderr)
    sys.exit(2)
