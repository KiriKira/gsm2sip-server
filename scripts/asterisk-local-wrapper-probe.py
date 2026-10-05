#!/usr/bin/env python3
"""Prove Local/Stasis wrapper headers against a local TLS SIP sink.

This probe uses randomized realtime endpoint names/call IDs and never contacts
an external carrier. It pauses the local API (if it was running) so the probe
websocket is the only consumer of its synthetic Stasis events.
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import json
import os
import secrets
import socket
import ssl
import struct
import subprocess
import sys
import threading
import time
import uuid
from collections import Counter
from pathlib import Path
from urllib.error import HTTPError, URLError
from urllib.parse import urlencode
from urllib.request import ProxyHandler, Request, build_opener


ROOT = Path(__file__).resolve().parents[1]
APP = "gsm2sip"
REALM = "gsm2sip"


def read_env_file() -> dict[str, str]:
    values: dict[str, str] = {}
    env_file = ROOT / ".env"
    if not env_file.exists():
        return values
    for line in env_file.read_text().splitlines():
        key, separator, value = line.partition("=")
        if not separator or not key or key.lstrip().startswith("#"):
            continue
        value = value.strip()
        if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
            value = value[1:-1]
        values[key.strip()] = value
    return values


def run_command(args: list[str], *, input_text: str | None = None, check: bool = True) -> str:
    result = subprocess.run(
        args,
        cwd=ROOT,
        input=input_text,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    if check and result.returncode:
        raise RuntimeError(f"local probe command failed: {args[0]} ({result.returncode})")
    return result.stdout.strip()


def asterisk_network() -> tuple[str, str]:
    container_id = run_command(["docker", "compose", "ps", "-q", "asterisk"])
    if not container_id:
        raise RuntimeError("Asterisk Compose container is not running")
    inspect = json.loads(run_command(["docker", "inspect", container_id]))[0]
    networks = inspect["NetworkSettings"]["Networks"]
    network_name, details = next(iter(networks.items()))
    network = json.loads(run_command(["docker", "network", "inspect", network_name]))[0]
    gateway = network["IPAM"]["Config"][0]["Gateway"]
    return details["IPAddress"], gateway


def db_sql(statement: str, *, check: bool = True) -> None:
    result = subprocess.run(
        [
            "docker", "compose", "exec", "-T", "postgres", "psql", "-X",
            "-v", "ON_ERROR_STOP=1", "-U", "gsm2sip", "-d", "gsm2sip",
        ],
        cwd=ROOT,
        input=statement,
        text=True,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        check=False,
    )
    if check and result.returncode:
        raise RuntimeError("local probe could not prepare its isolated realtime endpoint")


def db_boolean(statement: str) -> bool:
    result = subprocess.run(
        [
            "docker", "compose", "exec", "-T", "postgres", "psql", "-X", "-A", "-t",
            "-U", "gsm2sip", "-d", "gsm2sip", "-c", statement,
        ],
        cwd=ROOT,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
        check=False,
    )
    if result.returncode:
        raise RuntimeError("local probe could not inspect its realtime fixture schema")
    return result.stdout.strip().lower() == "t"


class APIServicePause:
    def __init__(self) -> None:
        self.was_running = bool(run_command(["docker", "compose", "ps", "--status", "running", "-q", "api"]))

    def stop(self) -> None:
        if self.was_running:
            run_command(["docker", "compose", "stop", "-t", "10", "api"])

    def restore(self) -> None:
        if self.was_running:
            run_command(["docker", "compose", "start", "api"])


def recv_exact(sock: socket.socket, count: int) -> bytes:
    data = bytearray()
    while len(data) < count:
        part = sock.recv(count - len(data))
        if not part:
            raise EOFError("ARI websocket closed")
        data.extend(part)
    return bytes(data)


class ARIEvents:
    def __init__(self, host: str, username: str, password: str):
        self.sock = socket.create_connection((host, 8088), timeout=5)
        self.sock.settimeout(5)
        self.buffer = b""
        self.queued_events: list[dict] = []
        key = base64.b64encode(secrets.token_bytes(16)).decode("ascii")
        auth = base64.b64encode(f"{username}:{password}".encode()).decode("ascii")
        path = "/ari/events?" + urlencode({"app": APP, "subscribeAll": "true"})
        request = (
            f"GET {path} HTTP/1.1\r\nHost: {host}:8088\r\n"
            "Upgrade: websocket\r\nConnection: Upgrade\r\n"
            f"Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n"
            f"Authorization: Basic {auth}\r\n\r\n"
        )
        self.sock.sendall(request.encode("ascii"))
        response = bytearray()
        while b"\r\n\r\n" not in response:
            block = self.sock.recv(4096)
            if not block:
                raise RuntimeError("ARI rejected its events websocket")
            response.extend(block)
            if len(response) > 16384:
                raise RuntimeError("ARI websocket response exceeded limit")
        headers, _, remainder = response.partition(b"\r\n\r\n")
        status_line = headers.split(b"\r\n", 1)[0]
        expected = base64.b64encode(
            hashlib.sha1((key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()
        )
        if b" 101 " not in status_line or expected.lower() not in headers.lower():
            raise RuntimeError("ARI rejected or failed to validate its events websocket")
        self.buffer = remainder
        self.wait_ready()

    def read_exact(self, count: int) -> bytes:
        data = bytearray()
        if self.buffer:
            take = min(count, len(self.buffer))
            data.extend(self.buffer[:take])
            self.buffer = self.buffer[take:]
        if len(data) < count:
            data.extend(recv_exact(self.sock, count - len(data)))
        return bytes(data)

    def receive_frame(self) -> tuple[int, bytes]:
        first, second = self.read_exact(2)
        size = second & 0x7F
        if size == 126:
            size = struct.unpack("!H", self.read_exact(2))[0]
        elif size == 127:
            size = struct.unpack("!Q", self.read_exact(8))[0]
        mask = self.read_exact(4) if second & 0x80 else b""
        payload = bytearray(self.read_exact(size))
        if mask:
            for index in range(size):
                payload[index] ^= mask[index % 4]
        return first & 0x0F, bytes(payload)

    def send_control(self, opcode: int, payload: bytes) -> None:
        mask = secrets.token_bytes(4)
        body = bytes(value ^ mask[index % 4] for index, value in enumerate(payload))
        self.sock.sendall(bytes((0x80 | opcode, 0x80 | len(body))) + mask + body)

    def wait_ready(self) -> None:
        nonce = secrets.token_bytes(8)
        self.send_control(9, nonce)
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            self.sock.settimeout(max(0.1, deadline - time.monotonic()))
            try:
                opcode, payload = self.receive_frame()
            except TimeoutError:
                continue
            if opcode == 8:
                raise RuntimeError("ARI closed its events websocket during readiness check")
            if opcode == 9:
                self.send_control(10, payload)
            elif opcode == 10 and payload == nonce:
                return
            elif opcode == 1:
                self.queued_events.append(json.loads(payload))
        raise RuntimeError("ARI events websocket did not answer its readiness ping")

    def next_event(self, predicate, timeout: float = 15) -> dict:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            while self.queued_events:
                event = self.queued_events.pop(0)
                if predicate(event):
                    return event
            self.sock.settimeout(max(0.1, deadline - time.monotonic()))
            try:
                opcode, payload = self.receive_frame()
            except TimeoutError:
                continue
            if opcode == 8:
                raise RuntimeError("ARI websocket closed during local wrapper probe")
            if opcode == 9:
                self.send_control(10, payload)
            elif opcode == 1:
                event = json.loads(payload)
                if predicate(event):
                    return event
        raise TimeoutError("timed out waiting for expected Stasis event")

    def close(self) -> None:
        try:
            self.send_control(8, b"")
        except OSError:
            pass
        self.sock.close()


HTTP = build_opener(ProxyHandler({}))


def ari_request(host: str, username: str, password: str, method: str, path: str, body=None):
    data = None if body is None else json.dumps(body).encode()
    auth = base64.b64encode(f"{username}:{password}".encode()).decode("ascii")
    request = Request(
        f"http://{host}:8088/ari/{path}",
        data=data,
        method=method,
        headers={"Authorization": f"Basic {auth}", "Content-Type": "application/json"},
    )
    try:
        with HTTP.open(request, timeout=8) as response:
            raw = response.read()
            return json.loads(raw) if raw else None
    except HTTPError as error:
        raw = error.read(4096)
        message = ""
        try:
            parsed = json.loads(raw)
            message = str(parsed.get("message", ""))
        except (UnicodeDecodeError, json.JSONDecodeError, AttributeError):
            pass
        message = message.replace(password, "[redacted]").replace("\n", " ").replace("\r", " ")[:240]
        suffix = f": {message}" if message else ""
        raise RuntimeError(f"ARI {method} failed with HTTP {error.code}{suffix}") from None
    except URLError:
        raise RuntimeError(f"ARI {method} request failed to connect") from None


def sip_read_message(conn: ssl.SSLSocket, buffered: bytearray) -> tuple[str, list[tuple[str, str]], bytes]:
    while b"\r\n\r\n" not in buffered:
        block = conn.recv(4096)
        if not block:
            raise EOFError("SIP connection closed")
        buffered.extend(block)
        if len(buffered) > 65536:
            raise RuntimeError("SIP header block exceeded probe limit")
    header_bytes, _, remainder = buffered.partition(b"\r\n\r\n")
    lines = header_bytes.decode("latin1").split("\r\n")
    items: list[tuple[str, str]] = []
    for line in lines[1:]:
        name, separator, value = line.partition(":")
        if separator:
            items.append((name.strip(), value.strip()))
    length = 0
    for name, value in items:
        if name.lower() == "content-length":
            length = int(value)
            break
    buffered.clear()
    buffered.extend(remainder)
    while len(buffered) < length:
        block = conn.recv(4096)
        if not block:
            raise EOFError("SIP body ended unexpectedly")
        buffered.extend(block)
    body = bytes(buffered[:length])
    del buffered[:length]
    return lines[0], items, body


class LocalTLSSink:
    def __init__(self, bind_ip: str, cert_path: Path, key_path: Path):
        self.context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        self.context.load_cert_chain(str(cert_path), str(key_path))
        self.listener = socket.socket()
        self.listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self.listener.bind((bind_ip, 0))
        self.listener.listen(8)
        self.listener.settimeout(0.5)
        self.host, self.port = self.listener.getsockname()
        self.invites: list[dict] = []
        self.lock = threading.Lock()
        self.stopped = threading.Event()
        self.thread = threading.Thread(target=self.serve, daemon=True)
        self.thread.start()

    @staticmethod
    def response_sdp() -> bytes:
        crypto = base64.b64encode(secrets.token_bytes(30)).decode("ascii")
        return (
            "v=0\r\n"
            "o=- 1 1 IN IP4 127.0.0.1\r\n"
            "s=Local Asterisk wrapper probe\r\n"
            "c=IN IP4 127.0.0.1\r\n"
            "t=0 0\r\n"
            "m=audio 40000 RTP/SAVP 0 8 9\r\n"
            "a=rtpmap:0 PCMU/8000\r\n"
            "a=rtpmap:8 PCMA/8000\r\n"
            "a=rtpmap:9 G722/8000\r\n"
            f"a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:{crypto}\r\n"
            "a=sendrecv\r\n\r\n"
        ).encode("ascii")

    def invite_count(self) -> int:
        with self.lock:
            return len(self.invites)

    def invites_for(self, call_id: str) -> list[dict]:
        with self.lock:
            return [dict(item) for item in self.invites if item["call_id"] == call_id]

    def answer(self, conn: ssl.SSLSocket, request_line: str, headers: list[tuple[str, str]]) -> None:
        header_map: dict[str, list[str]] = {}
        for name, value in headers:
            header_map.setdefault(name.lower(), []).append(value)
        def first(name: str, fallback: str = "") -> str:
            values = header_map.get(name.lower(), [])
            return values[0] if values else fallback

        method = request_line.split(" ", 1)[0].upper()
        if method == "INVITE":
            x_headers = [(name, value) for name, value in headers if name.lower().startswith("x-gsm")]
            custom_map: dict[str, list[str]] = {}
            for name, value in x_headers:
                custom_map.setdefault(name.lower(), []).append(value)
            call_id = (custom_map.get("x-gsm-call-id") or custom_map.get("x-gsm2sip-call-id") or [""])[0]
            with self.lock:
                request_parts = request_line.split()
                target = request_parts[1] if len(request_parts) > 1 else ""
                self.invites.append({"call_id": call_id, "request_target": target, "x_headers": x_headers})
            response_sdp = self.response_sdp()
            to_value = first("to", "<sip:sink>")
            if ";tag=" not in to_value.lower():
                to_value += ";tag=" + secrets.token_hex(8)
            response_headers = [
                "SIP/2.0 200 OK",
                *[f"Via: {value}" for value in header_map.get("via", [])],
                f"From: {first('from', '<sip:probe@localhost>')}",
                f"To: {to_value}",
                f"Call-ID: {first('call-id', 'local-probe')}",
                f"CSeq: {first('cseq', '1 INVITE')}",
                f"Contact: <sip:sink@{self.host}:{self.port};transport=tls>",
                "Content-Type: application/sdp",
                f"Content-Length: {len(response_sdp)}",
                "",
                "",
            ]
            conn.sendall("\r\n".join(response_headers).encode("ascii") + response_sdp)
        elif method in ("BYE", "CANCEL", "OPTIONS"):
            response_headers = [
                "SIP/2.0 200 OK",
                *[f"Via: {value}" for value in header_map.get("via", [])],
                f"From: {first('from', '<sip:probe@localhost>')}",
                f"To: {first('to', '<sip:sink>')}",
                f"Call-ID: {first('call-id', 'local-probe')}",
                f"CSeq: {first('cseq', '1 OPTIONS')}",
                "Content-Length: 0",
                "",
                "",
            ]
            conn.sendall("\r\n".join(response_headers).encode("ascii"))

    def handle(self, raw: socket.socket) -> None:
        try:
            with self.context.wrap_socket(raw, server_side=True) as conn:
                conn.settimeout(12)
                buffered = bytearray()
                while not self.stopped.is_set():
                    request_line, headers, _body = sip_read_message(conn, buffered)
                    self.answer(conn, request_line, headers)
        except (OSError, ssl.SSLError, EOFError, ValueError):
            return

    def serve(self) -> None:
        while not self.stopped.is_set():
            try:
                client, _peer = self.listener.accept()
            except socket.timeout:
                continue
            except OSError:
                return
            threading.Thread(target=self.handle, args=(client,), daemon=True).start()

    def close(self) -> None:
        self.stopped.set()
        self.listener.close()


def expected_header_values(kind: str, call_id: str) -> dict[str, str]:
    if kind == "gateway":
        return {
            "x-gsm-protocol-version": "1",
            "x-gsm-call-id": call_id,
            "x-gsm-sim-id": "probe-sim-" + call_id.replace("-", "")[:12],
            "x-gsm-mapping-revision": "73",
        }
    return {"x-gsm2sip-call-id": call_id}


def wait_for_application(host: str, username: str, password: str) -> None:
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        try:
            application = ari_request(host, username, password, "GET", f"applications/{APP}")
            if application.get("name") == APP:
                return
        except RuntimeError:
            pass
        time.sleep(0.1)
    raise RuntimeError("ARI did not report the gsm2sip Stasis application ready")


def run_flow(
    events: ARIEvents,
    sink: LocalTLSSink,
    host: str,
    username: str,
    password: str,
    kind: str,
    endpoint: str,
) -> None:
    call_id = str(uuid.uuid4())
    sim_id = "probe-sim-" + call_id.replace("-", "")[:12]
    context = "gsm2sip-ari-gateway" if kind == "gateway" else "gsm2sip-ari-client"
    child_kind = "gateway-leg" if kind == "gateway" else "client-leg"
    variables = {"__GSM2SIP_ENDPOINT": endpoint, "__GSM2SIP_CALL_ID": call_id}
    if kind == "gateway":
        variables.update(
            {
                "__GSM2SIP_DESTINATION": "15551234567",
                "__GSM2SIP_SIM_ID": sim_id,
                "__GSM2SIP_MAPPING_REVISION": "73",
                "__GSM2SIP_PROTOCOL_VERSION": "1",
            }
        )
    channel_id = "probe-wrapper-" + uuid.uuid4().hex
    query = urlencode(
        {
            "endpoint": f"Local/s@{context}/n",
            "app": APP,
            "appArgs": f"originate-wrapper,{call_id}",
            "channelId": channel_id,
            # Local wrapper and test endpoint advertise only codecs the
            # isolated UAS can answer without accessing outside media.
            "formats": "ulaw,alaw,g722",
        }
    )
    before_count = sink.invite_count()
    channel = ari_request(host, username, password, "POST", "channels/create?" + query, {"variables": variables})
    wrapper_id = channel["id"]
    child_id = ""
    try:
        wrapper = events.next_event(
            lambda event: event.get("type") == "StasisStart"
            and event.get("channel", {}).get("id") == wrapper_id
        )
        if wrapper.get("args") != ["originate-wrapper", call_id]:
            raise RuntimeError(f"{kind} wrapper Stasis args did not match")
        time.sleep(0.6)
        if sink.invite_count() != before_count:
            raise RuntimeError(f"{kind} wrapper sent an INVITE before ARI /dial")

        # This is deliberately the sole /dial request for this wrapper.
        ari_request(host, username, password, "POST", f"channels/{wrapper_id}/dial?timeout=8")
        child = events.next_event(
            lambda event: event.get("type") == "StasisStart"
            and event.get("args", [None])[0] == child_kind
            and len(event.get("args", [])) >= 2
            and event["args"][1] == call_id
        )
        child_id = child.get("channel", {}).get("id", "")
        if not child_id:
            raise RuntimeError(f"{kind} child Stasis event omitted its channel ID")

        deadline = time.monotonic() + 12
        captured: list[dict] = []
        while time.monotonic() < deadline:
            captured = sink.invites_for(call_id)
            if captured:
                break
            time.sleep(0.05)
        if not captured:
            raise RuntimeError(f"{kind} wrapper produced no local TLS INVITE after /dial")
        time.sleep(0.8)
        captured = sink.invites_for(call_id)
        if len(captured) != 1:
            raise RuntimeError(f"{kind} wrapper produced {len(captured)} INVITEs after one /dial")

        observed_headers = captured[0]["x_headers"]
        observed_names = [name.lower() for name, _value in observed_headers]
        expected = expected_header_values(kind, call_id)
        if kind == "gateway":
            target = captured[0]["request_target"]
            authority = target[4:].split(";", 1)[0] if target.lower().startswith("sip:") else ""
            target_user, separator, target_host = authority.rpartition("@")
            if not separator or target_user != "15551234567" or target_host != f"{sink.host}:{sink.port}":
                raise RuntimeError("gateway INVITE did not target the exact dialed number at the local TLS contact")
        if Counter(observed_names) != Counter(expected.keys()):
            raise RuntimeError(f"{kind} INVITE X-GSM header names or counts did not match")
        for name, value in observed_headers:
            if expected.get(name.lower()) != value:
                raise RuntimeError(f"{kind} INVITE contained an unexpected X-GSM header value")
        names = ",".join(name.upper() for name in expected)
        print(f"PASS: {kind} wrapper emitted 0 INVITEs before /dial and exactly 1 after; {names} values/counts verified")
    finally:
        for item in (child_id, wrapper_id):
            if item:
                try:
                    ari_request(host, username, password, "DELETE", f"channels/{item}")
                except RuntimeError:
                    pass


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--keep-api-stopped", action="store_true", help="do not restart an API container that was running before the probe")
    args = parser.parse_args()

    env = read_env_file()
    username = env.get("ARI_USERNAME", "gsm2sip")
    password = env.get("ARI_PASSWORD", "local-dev-change-me")
    cert = Path(env.get("ASTERISK_TLS_CERT_FILE", "./.local/asterisk/sip.crt"))
    key = Path(env.get("ASTERISK_TLS_KEY_FILE", "./.local/asterisk/sip.key"))
    if not cert.is_absolute():
        cert = ROOT / cert
    if not key.is_absolute():
        key = ROOT / key
    if not cert.is_file() or not key.is_file():
        raise RuntimeError("local Asterisk TLS certificate/key are missing")

    pause = APIServicePause()
    api_stopped = False
    sink: LocalTLSSink | None = None
    events: ARIEvents | None = None
    endpoint_ids: list[str] = []
    contact_column_added = False
    try:
        api_stopped = pause.was_running
        pause.stop()
        host, bind_ip = asterisk_network()
        sink = LocalTLSSink(bind_ip, cert, key)

        suffix = uuid.uuid4().hex
        gateway_endpoint = "probe-gw-" + suffix[:20]
        client_endpoint = "probe-cl-" + suffix[12:32]
        endpoint_ids = [gateway_endpoint, client_endpoint]
        auth_password = secrets.token_urlsafe(32)
        # The only persisted auth value is the temporary Digest A1 hash. It is
        # neither emitted to logs nor used outside these local test endpoints.
        auth_digest = hashlib.md5(f"{gateway_endpoint}:{REALM}:{auth_password}".encode()).hexdigest()
        # Do not append ;transport=tls here: Asterisk Sorcery treats a semicolon
        # as a multi-value delimiter for realtime fields. The endpoint's
        # transport-tls setting selects TLS for this local sip: contact.
        contact = f"sip:sink@{sink.host}:{sink.port}"
        contact_column_added = not db_boolean(
            "SELECT EXISTS(SELECT 1 FROM information_schema.columns "
            "WHERE table_schema=current_schema() AND table_name='ps_aors' AND column_name='contact')"
        )
        if contact_column_added:
            db_sql("ALTER TABLE ps_aors ADD COLUMN contact TEXT")
        db_sql(
            f"INSERT INTO ps_aors(id,max_contacts,remove_existing,qualify_frequency,contact) VALUES "
            f"('{gateway_endpoint}',1,'yes',0,'{contact}'),('{client_endpoint}',1,'yes',0,'{contact}');\n"
            f"INSERT INTO ps_auths(id,auth_type,realm,username,password_digest) VALUES "
            f"('{gateway_endpoint}','userpass','{REALM}','{gateway_endpoint}','MD5:{auth_digest}');\n"
            "INSERT INTO ps_endpoints(id,transport,aors,auth,context,disallow,allow,direct_media,force_rport,rewrite_contact,rtp_symmetric,media_encryption,media_encryption_optimistic,dtmf_mode,identify_by) VALUES "
            f"('{gateway_endpoint}','transport-tls','{gateway_endpoint}','{gateway_endpoint}','gsm-gateway','all','alaw,ulaw,g722','no','yes','yes','yes','sdes','no','rfc4733','username'),"
            f"('{client_endpoint}','transport-tls','{client_endpoint}',NULL,'gsm-client','all','alaw,ulaw,g722','no','yes','yes','yes','sdes','no','rfc4733','username');\n"
        )

        events = ARIEvents(host, username, password)
        wait_for_application(host, username, password)
        for kind, endpoint in (("gateway", gateway_endpoint), ("client", client_endpoint)):
            run_flow(events, sink, host, username, password, kind, endpoint)
        print("PASS: local TLS UAS answered gateway/client calls from temporary static contact fixtures with SDES-SRTP; no public carrier or SIM was used")
    finally:
        if events is not None:
            events.close()
        if sink is not None:
            sink.close()
        if endpoint_ids:
            quoted_ids = ",".join("'" + endpoint_id + "'" for endpoint_id in endpoint_ids)
            db_sql(
                f"DELETE FROM ps_endpoints WHERE id IN ({quoted_ids});\n"
                f"DELETE FROM ps_auths WHERE id IN ({quoted_ids});\n"
                f"DELETE FROM ps_aors WHERE id IN ({quoted_ids});\n",
                check=False,
            )
        if contact_column_added:
            db_sql("ALTER TABLE ps_aors DROP COLUMN contact", check=False)
        if api_stopped and not args.keep_api_stopped:
            pause.restore()
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as error:
        print(f"FAIL: local Asterisk wrapper probe: {error}", file=sys.stderr)
        raise SystemExit(1)
