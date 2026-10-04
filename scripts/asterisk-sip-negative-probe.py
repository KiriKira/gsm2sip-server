#!/usr/bin/env python3
"""Verify local TLS trust behavior and mandatory SDES-SRTP negotiation."""

import hashlib
import os
import re
import socket
import ssl
import sys
import uuid


HOST = "127.0.0.1"
SERVER_NAME = "sip.localhost"
PORT = int(os.environ.get("SIP_PORT", "5061"))
CERT_FILE = os.environ.get("ASTERISK_TLS_CERT_FILE", ".local/asterisk/sip.crt")
ENDPOINT = os.environ.get("SIP_PROBE_ENDPOINT", "dev_00000000000000000000000000000001")
REALM = "gsm2sip"
PASSWORD = os.environ.get("SIP_PROBE_PASSWORD")


def expect_untrusted_certificate() -> None:
    context = ssl.create_default_context()
    try:
        with socket.create_connection((HOST, PORT), timeout=5) as raw:
            with context.wrap_socket(raw, server_hostname=SERVER_NAME):
                raise RuntimeError("system trust unexpectedly accepted local self-signed certificate")
    except ssl.SSLCertVerificationError:
        pass


def open_trusted_connection():
    context = ssl.create_default_context(cafile=CERT_FILE)
    raw = socket.create_connection((HOST, PORT), timeout=5)
    raw.settimeout(5)
    tls = context.wrap_socket(raw, server_hostname=SERVER_NAME)
    return tls, tls.makefile("rb")


def receive_response(reader):
    status_line = reader.readline().decode("ascii", "replace").strip()
    if not status_line.startswith("SIP/2.0 "):
        raise RuntimeError("Asterisk returned an invalid SIP response")
    headers = {}
    while True:
        line = reader.readline()
        if line in (b"\r\n", b"\n", b""):
            break
        decoded = line.decode("ascii", "replace").strip()
        if ":" not in decoded:
            continue
        key, value = decoded.split(":", 1)
        headers.setdefault(key.lower(), []).append(value.strip())
    length = int(headers.get("content-length", ["0"])[0])
    if length:
        reader.read(length)
    return int(status_line.split()[1]), headers


def send_invite(tls, sequence, auth=None):
    branch = "z9hG4bK" + uuid.uuid4().hex
    tag = "probe" + uuid.uuid4().hex[:8]
    call_id = uuid.uuid4().hex + "@localhost"
    uri = "sips:call.local-probe@sip.localhost:5061;transport=tls"
    sdp = (
        "v=0\r\n"
        "o=probe 1 1 IN IP4 127.0.0.1\r\n"
        "s=local probe\r\n"
        "c=IN IP4 127.0.0.1\r\n"
        "t=0 0\r\n"
        "m=audio 40000 RTP/AVP 0 9\r\n"
        "a=rtpmap:0 PCMU/8000\r\n"
        "a=rtpmap:9 G722/8000\r\n"
        "a=sendrecv\r\n"
    ).encode("ascii")
    # Use a new transaction branch for the authenticated retry, with the same
    # Call-ID and From tag. This is enough for Asterisk's Digest challenge.
    headers = [
        f"INVITE {uri} SIP/2.0",
        f"Via: SIP/2.0/TLS probe.invalid;branch={branch};rport",
        "Max-Forwards: 70",
        f"From: <sips:{ENDPOINT}@sip.localhost>;tag={tag}",
        f"To: <{uri}>",
        f"Call-ID: {call_id}",
        f"CSeq: {sequence} INVITE",
        f"Contact: <sips:{ENDPOINT}@sip.localhost:5061;transport=tls>",
        "Content-Type: application/sdp",
        f"Content-Length: {len(sdp)}",
    ]
    if auth:
        headers.append("Authorization: " + auth)
    tls.sendall(("\r\n".join(headers) + "\r\n\r\n").encode("ascii") + sdp)
    return call_id, tag, uri, sdp


def parse_digest_challenge(value):
    if not value.lower().startswith("digest "):
        raise RuntimeError("Asterisk did not issue a Digest challenge")
    fields = dict(re.findall(r'([A-Za-z]+)="?([^", ]+)"?', value[7:]))
    if fields.get("realm") != REALM or not fields.get("nonce"):
        raise RuntimeError("Asterisk returned an unexpected Digest realm")
    if fields.get("algorithm", "MD5").upper() != "MD5":
        raise RuntimeError("Asterisk selected an unsupported local probe digest")
    return fields


def build_authorization(challenge, uri, call_id, tag):
    qop = challenge.get("qop", "auth")
    if "auth" not in qop.split(","):
        raise RuntimeError("Asterisk Digest challenge omitted qop=auth")
    nonce = challenge["nonce"]
    cnonce = uuid.uuid4().hex
    nonce_count = "00000001"
    ha1 = hashlib.md5(f"{ENDPOINT}:{REALM}:{PASSWORD}".encode()).hexdigest()
    ha2 = hashlib.md5(f"INVITE:{uri}".encode()).hexdigest()
    response = hashlib.md5(
        f"{ha1}:{nonce}:{nonce_count}:{cnonce}:auth:{ha2}".encode()
    ).hexdigest()
    fields = [
        f'username="{ENDPOINT}"',
        f'realm="{REALM}"',
        f'nonce="{nonce}"',
        f'uri="{uri}"',
        f'response="{response}"',
        'algorithm=MD5',
        'qop=auth',
        f'nc={nonce_count}',
        f'cnonce="{cnonce}"',
    ]
    if challenge.get("opaque"):
        fields.append(f'opaque="{challenge["opaque"]}"')
    return "Digest " + ", ".join(fields)


def main() -> int:
    if not PASSWORD:
        raise RuntimeError("SIP_PROBE_PASSWORD is required")
    expect_untrusted_certificate()
    tls, reader = open_trusted_connection()
    try:
        call_id, tag, uri, sdp = send_invite(tls, 1)
        status, headers = receive_response(reader)
        if status != 401:
            raise RuntimeError(f"expected initial Digest 401, received SIP {status}")
        challenges = headers.get("www-authenticate", [])
        if not challenges:
            raise RuntimeError("Asterisk omitted WWW-Authenticate")
        challenge = parse_digest_challenge(challenges[0])

        # Reuse the same dialog identifiers for the authenticated request.
        branch = "z9hG4bK" + uuid.uuid4().hex
        nonce = challenge["nonce"]
        cnonce = uuid.uuid4().hex
        nc = "00000001"
        ha1 = hashlib.md5(f"{ENDPOINT}:{REALM}:{PASSWORD}".encode()).hexdigest()
        ha2 = hashlib.md5(f"INVITE:{uri}".encode()).hexdigest()
        response = hashlib.md5(f"{ha1}:{nonce}:{nc}:{cnonce}:auth:{ha2}".encode()).hexdigest()
        auth_fields = [
            f'username="{ENDPOINT}"', f'realm="{REALM}"', f'nonce="{nonce}"',
            f'uri="{uri}"', f'response="{response}"', "algorithm=MD5",
            "qop=auth", f"nc={nc}", f'cnonce="{cnonce}"',
        ]
        if challenge.get("opaque"):
            auth_fields.append(f'opaque="{challenge["opaque"]}"')
        auth = "Digest " + ", ".join(auth_fields)
        headers_out = [
            f"INVITE {uri} SIP/2.0",
            f"Via: SIP/2.0/TLS probe.invalid;branch={branch};rport",
            "Max-Forwards: 70",
            f"From: <sips:{ENDPOINT}@sip.localhost>;tag={tag}",
            f"To: <{uri}>",
            f"Call-ID: {call_id}",
            "CSeq: 2 INVITE",
            f"Contact: <sips:{ENDPOINT}@sip.localhost:5061;transport=tls>",
            "Content-Type: application/sdp",
            f"Content-Length: {len(sdp)}",
            f"Authorization: {auth}",
        ]
        tls.sendall(("\r\n".join(headers_out) + "\r\n\r\n").encode("ascii") + sdp)
        for _ in range(4):
            status, _ = receive_response(reader)
            if status == 488:
                print("PASS: untrusted TLS rejected; trusted TLS accepted; no-SRTP INVITE rejected with 488")
                return 0
            if status in (401, 403, 404, 415, 420, 486, 503):
                raise RuntimeError(f"authenticated no-SRTP INVITE received SIP {status}, expected 488")
        raise RuntimeError("Asterisk did not reject the no-SRTP offer with SIP 488")
    finally:
        reader.close()
        tls.close()


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as exc:
        print(f"FAIL: {exc}", file=sys.stderr)
        raise SystemExit(1)
