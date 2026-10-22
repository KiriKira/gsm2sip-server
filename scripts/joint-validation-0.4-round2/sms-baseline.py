#!/usr/bin/env python3
"""Capture and verify one synthetic SMS baseline on the joint-0.4 lab host.

`capture` intentionally creates a temporary same-owner client, sends one SMS
through the Gateway virtual modem, and injects one synthetic incoming SMS.
`verify` is read-only apart from rotating the temporary client's auth session
if its access token expired. `cleanup` revokes that session only when called
with --case6-complete.

This helper is deliberately pinned to the lab's two Android emulators and the
synthetic number. It has no serial/number override and never targets a handset.
It does not print request/response bodies, tokens, pairing codes, or SMS text.
"""

import argparse
import hashlib
import io
import json
import os
import re
import sqlite3
import subprocess
import tarfile
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from datetime import datetime, timezone
from pathlib import Path, PurePosixPath


REMOTE_ROOT = Path("/home/ubuntu/gsm-webrtc-validation/joint-0.4")
PRIVATE = REMOTE_ROOT / "private"
EVIDENCE = REMOTE_ROOT / "evidence"
STATE_FILE = PRIVATE / "sms-baseline-state.json"
PROBE_AUTH_FILE = PRIVATE / "sms-probe-client-auth.json"

API = "http://127.0.0.1:18096"
ADB = "/home/ubuntu/gsm-webrtc-validation/android-sdk/platform-tools/adb"
GATEWAY_SERIAL = "emulator-5554"
HOST_SERIAL = "emulator-5556"
GATEWAY_PACKAGE = "com.callagent.gateway"
HOST_PACKAGE = "com.callagent.host"
SYNTHETIC_NUMBER = "+15550100200"
MESSAGE_TTL_SECONDS = 300  # The API command's normal product TTL.
HOST_SYNC_DEADLINE_SECONDS = 60
POLL_INTERVAL_SECONDS = 1.0

UUID_RE = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$", re.I)
DB_BASENAME_RE = re.compile(r"^[A-Za-z0-9_.-]+$")


class BaselineError(Exception):
    """An error with a safe, non-sensitive code for terminal output."""

    def __init__(self, code):
        super().__init__(code)
        self.code = code


class APIStatusError(Exception):
    def __init__(self, status):
        super().__init__(str(status))
        self.status = status


def prepare_dirs():
    PRIVATE.mkdir(parents=True, exist_ok=True, mode=0o700)
    EVIDENCE.mkdir(parents=True, exist_ok=True, mode=0o700)
    PRIVATE.chmod(0o700)
    EVIDENCE.chmod(0o700)


def atomic_json(path, value):
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    fd, temp_name = tempfile.mkstemp(prefix=".sms-baseline-", dir=str(path.parent))
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as out:
            json.dump(value, out, sort_keys=True, indent=2)
            out.write("\n")
            out.flush()
            os.fsync(out.fileno())
        os.replace(temp_name, path)
        path.chmod(0o600)
    except Exception:
        try:
            os.unlink(temp_name)
        except OSError:
            pass
        raise


def read_state():
    if not STATE_FILE.exists():
        raise BaselineError("state_missing")
    try:
        mode = STATE_FILE.stat().st_mode & 0o777
        if mode & 0o077:
            raise BaselineError("state_permissions_unsafe")
        value = json.loads(STATE_FILE.read_text(encoding="utf-8"))
    except BaselineError:
        raise
    except Exception:
        raise BaselineError("state_unreadable") from None
    if not isinstance(value, dict) or value.get("schema") != "gsm04-sms-baseline-v1":
        raise BaselineError("state_invalid")
    if value.get("cleanup_complete"):
        raise BaselineError("state_already_cleaned")
    return value


def api_json(method, path, body=None, token=None, headers=None, timeout=12):
    request_headers = {"Accept": "application/json"}
    if body is not None:
        request_headers["Content-Type"] = "application/json"
    if token:
        request_headers["Authorization"] = "Bearer " + token
    if headers:
        request_headers.update(headers)
    encoded = None if body is None else json.dumps(body, separators=(",", ":")).encode()
    request = urllib.request.Request(API + path, data=encoded, method=method, headers=request_headers)
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            raw = response.read()
            if not raw:
                return None
            return json.loads(raw.decode("utf-8"))
    except urllib.error.HTTPError as error:
        status = error.code
        error.close()
        raise APIStatusError(status) from None
    except (urllib.error.URLError, TimeoutError, OSError, ValueError):
        raise BaselineError("api_unavailable_or_invalid") from None


def authenticated_json(state, method, path, body=None, headers=None):
    auth_state, auth_path = active_auth_state(state)
    try:
        return api_json(method, path, body=body, token=auth_state["access_token"], headers=headers)
    except APIStatusError as error:
        if error.status != 401:
            raise BaselineError("api_request_rejected") from None
    refresh_session(auth_state, auth_path)
    try:
        return api_json(method, path, body=body, token=auth_state["access_token"], headers=headers)
    except APIStatusError:
        raise BaselineError("api_request_rejected_after_refresh") from None


def active_auth_state(state):
    """Use the probe's separately stored rotating session, if it exists.

    SMS probes never replace baseline message state. Keeping the refresh-token
    rotation in its own private sidecar also lets later baseline verification
    and the explicit post-case cleanup use the same saved temporary client.
    """
    if not PROBE_AUTH_FILE.exists() and not PROBE_AUTH_FILE.is_symlink():
        return state, STATE_FILE
    if PROBE_AUTH_FILE.is_symlink():
        raise BaselineError("probe_auth_state_symlink_refused")
    if PROBE_AUTH_FILE.stat().st_mode & 0o077:
        raise BaselineError("probe_auth_state_permissions_unsafe")
    try:
        auth_state = json.loads(PROBE_AUTH_FILE.read_text(encoding="utf-8"))
    except Exception:
        raise BaselineError("probe_auth_state_unreadable") from None
    if (not isinstance(auth_state, dict) or
            auth_state.get("schema") != "gsm04-sms-probe-auth-v1" or
            auth_state.get("client_device_id") != state.get("client_device_id") or
            not auth_state.get("access_token") or not auth_state.get("refresh_token")):
        raise BaselineError("probe_auth_state_invalid")
    return auth_state, PROBE_AUTH_FILE


def refresh_session(state, state_file=STATE_FILE):
    key = state.get("pending_refresh_key")
    if not key:
        key = "smsbaseline-refresh:" + uuid.uuid4().hex
        state["pending_refresh_key"] = key
        atomic_json(state_file, state)
    response = api_json(
        "POST",
        "/v1/auth/refresh",
        body={"refresh_token": state["refresh_token"]},
        headers={"Idempotency-Key": key},
    )
    if not isinstance(response, dict) or not response.get("access_token") or not response.get("refresh_token"):
        raise BaselineError("refresh_response_invalid")
    state["access_token"] = response["access_token"]
    state["refresh_token"] = response["refresh_token"]
    state["access_expires_at"] = response.get("access_expires_at")
    state["refresh_expires_at"] = response.get("refresh_expires_at")
    state.pop("pending_refresh_key", None)
    atomic_json(state_file, state)


def adb_text(serial, *args, timeout=25):
    try:
        result = subprocess.run(
            [ADB, "-s", serial, *args], capture_output=True, text=True,
            timeout=timeout, check=False,
        )
    except (OSError, subprocess.TimeoutExpired):
        raise BaselineError("adb_unavailable") from None
    if result.returncode != 0:
        raise BaselineError("adb_command_failed")
    return result.stdout.strip()


def adb_bytes(serial, *args, timeout=90):
    try:
        result = subprocess.run(
            [ADB, "-s", serial, *args], capture_output=True,
            timeout=timeout, check=False,
        )
    except (OSError, subprocess.TimeoutExpired):
        raise BaselineError("adb_snapshot_failed") from None
    if result.returncode != 0:
        raise BaselineError("adb_snapshot_failed")
    return result.stdout


def check_emulator(serial):
    if adb_text(serial, "get-state") != "device":
        raise BaselineError("required_emulator_offline")
    if adb_text(serial, "shell", "getprop", "ro.kernel.qemu") != "1":
        raise BaselineError("refusing_non_emulator_target")


def start_app(serial, package):
    adb_text(serial, "shell", "am", "start", "-W", "-n", package + "/.MainActivity", timeout=35)


def check_api():
    response = api_json("GET", "/v1/capabilities")
    if not isinstance(response, dict) or not isinstance(response.get("protocol_version"), int):
        raise BaselineError("api_capabilities_invalid")


def docker_output(*args, timeout=25):
    try:
        result = subprocess.run(
            ["sudo", "-n", "docker", *args], capture_output=True,
            text=True, timeout=timeout, check=False,
        )
    except (OSError, subprocess.TimeoutExpired):
        raise BaselineError("docker_unavailable") from None
    if result.returncode != 0:
        raise BaselineError("docker_command_failed")
    return result.stdout


def active_gateway_binding():
    query = """
SELECT d.owner_id::text,g.device_id::text,g.mapping_revision::text,
       b.sim_id::text,b.slot_index::text
FROM devices d
JOIN gateways g ON g.device_id=d.id
JOIN sim_bindings b ON b.gateway_id=g.device_id AND b.owner_id=d.owner_id
WHERE d.role='gateway' AND d.state='active'
  AND b.state='active' AND b.identity_verified
  AND b.mapping_revision=g.mapping_revision
  AND g.last_seen_at > clock_timestamp()-interval '90 seconds'
ORDER BY g.last_seen_at DESC,b.slot_index ASC
""".strip()
    output = docker_output(
        "exec", "--user", "gsm2sip", "gsm-rtc-joint", "/usr/lib/postgresql/17/bin/psql",
        "-X", "-h", "/run/postgresql", "-p", "25432", "-U", "gsm2sip", "-d", "gsm2sip",
        "-At", "-F", "|", "-v", "ON_ERROR_STOP=1", "-c", query,
    )
    rows = []
    for line in output.splitlines():
        fields = line.strip().split("|")
        if len(fields) != 5:
            continue
        owner_id, gateway_id, revision, sim_id, slot = fields
        if not all(UUID_RE.fullmatch(v) for v in (owner_id, gateway_id, sim_id)):
            raise BaselineError("database_identity_invalid")
        try:
            rows.append({"owner_id": owner_id, "gateway_id": gateway_id,
                         "mapping_revision": int(revision), "sim_id": sim_id,
                         "slot_index": int(slot)})
        except ValueError:
            raise BaselineError("database_mapping_invalid") from None
    gateway_ids = {row["gateway_id"] for row in rows}
    if len(gateway_ids) != 1 or not rows:
        raise BaselineError("expected_one_live_gateway")
    owner_ids = {row["owner_id"] for row in rows}
    if len(owner_ids) != 1:
        raise BaselineError("gateway_owner_ambiguous")
    return sorted(rows, key=lambda item: item["slot_index"])[0]


def create_pairing_code(owner_id):
    output = docker_output(
        # The admin CLI reads the database password from /data/secrets, which
        # is intentionally root-readable inside the container. Keep its
        # secret-bearing output captured and parse only the one-time code.
        "exec", "gsm-rtc-joint", "gsm2sip-admin",
        "pairing-code", "create", "--owner-id", owner_id, "--role", "client", "--ttl", "10m",
    )
    for line in output.splitlines():
        key, sep, value = line.partition("=")
        if sep and key == "pairing_code" and 20 <= len(value) <= 128:
            return value
    raise BaselineError("pairing_code_creation_failed")


def snapshot_databases(serial, package):
    archive = adb_bytes(serial, "exec-out", "run-as", package, "tar", "-cf", "-", "databases")
    extracted = {}
    try:
        with tarfile.open(fileobj=io.BytesIO(archive), mode="r:") as bundle:
            for member in bundle.getmembers():
                path = PurePosixPath(member.name)
                if not member.isfile() or len(path.parts) != 2 or path.parts[0] != "databases":
                    continue
                basename = path.parts[1]
                if not DB_BASENAME_RE.fullmatch(basename):
                    continue
                source = bundle.extractfile(member)
                if source is None:
                    continue
                extracted[basename] = source.read()
    except (tarfile.TarError, OSError):
        raise BaselineError("database_snapshot_invalid") from None
    if not any(name.endswith(".db") for name in extracted):
        raise BaselineError("database_snapshot_empty")
    return extracted


def database_files_in_temp(snapshot, directory):
    for basename, content in snapshot.items():
        path = Path(directory) / basename
        path.write_bytes(content)
        path.chmod(0o600)
    return sorted(Path(directory).glob("*.db"))


def readonly_db(path):
    try:
        connection = sqlite3.connect("file:" + path.as_posix() + "?mode=ro", uri=True, timeout=5)
        connection.execute("PRAGMA query_only=ON")
        return connection
    except sqlite3.Error:
        raise BaselineError("sqlite_snapshot_unreadable") from None


def table_names(connection):
    return {row[0] for row in connection.execute("SELECT name FROM sqlite_master WHERE type='table'")}


def check_gateway_archive_enabled(snapshot):
    with tempfile.TemporaryDirectory(prefix="sms-baseline-gateway-") as directory:
        files = database_files_in_temp(snapshot, directory)
        for path in files:
            connection = readonly_db(path)
            try:
                if "sms_archive_options" in table_names(connection):
                    row = connection.execute(
                        "SELECT enabled FROM sms_archive_options WHERE singleton_id=1"
                    ).fetchone()
                    return bool(row and row[0] == 1)
            except sqlite3.Error:
                raise BaselineError("gateway_archive_schema_invalid") from None
            finally:
                connection.close()
    raise BaselineError("gateway_archive_schema_missing")


def history_rows(snapshot, expected, gateway):
    """Return only matched row counts/statuses; message text remains in memory."""
    found = {key: [] for key in expected}
    with tempfile.TemporaryDirectory(prefix="sms-baseline-db-") as directory:
        files = database_files_in_temp(snapshot, directory)
        for path in files:
            connection = readonly_db(path)
            try:
                tables = table_names(connection)
                table = "sms_archive_history" if gateway else "messages"
                if table not in tables:
                    continue
                columns = {row[1] for row in connection.execute("PRAGMA table_info(" + table + ")")}
                if gateway:
                    needed = {"message_id", "direction", "body", "status"}
                    if not needed.issubset(columns):
                        continue
                    query = "SELECT message_id,direction,body,status FROM sms_archive_history WHERE message_id IN (?,?)"
                else:
                    needed = {"server_id", "direction", "body", "status"}
                    if not needed.issubset(columns):
                        continue
                    query = "SELECT server_id,direction,body,status FROM messages WHERE server_id IN (?,?)"
                ids = [expected[k]["message_id"] for k in ("outbound", "inbound")]
                for message_id, direction, body, status in connection.execute(query, ids):
                    key = direction if direction in found else None
                    if key is None or message_id != expected[key]["message_id"]:
                        continue
                    body_hash = hashlib.sha256(str(body).encode("utf-8")).hexdigest()
                    found[key].append({"body_sha256": body_hash, "status": str(status)})
            except sqlite3.Error:
                raise BaselineError("local_history_schema_invalid") from None
            finally:
                connection.close()
    return found


def assert_history(snapshot, expected, gateway):
    rows = history_rows(snapshot, expected, gateway)
    for direction in ("outbound", "inbound"):
        matches = [row for row in rows[direction] if row["body_sha256"] == expected[direction]["sha256"]]
        if not matches:
            raise BaselineError("gateway_archive_missing_baseline" if gateway else "host_history_missing_baseline")
        expected[direction]["gateway_archive_count" if gateway else "host_history_count"] = len(matches)
        if gateway:
            expected[direction]["gateway_archive_status"] = matches[-1]["status"]
        else:
            expected[direction]["host_history_status"] = matches[-1]["status"]


def sms_text(run_id, direction):
    return "gsm04joint-" + direction + "-" + run_id


def sha256_text(value):
    return hashlib.sha256(value.encode("utf-8")).hexdigest()


def message_detail(state, message_id):
    return authenticated_json(state, "GET", "/v1/messages/" + urllib.parse.quote(message_id, safe=""))


def wait_outbound_submission(state, message_id, expires_at, expected_hash):
    try:
        deadline = datetime.fromisoformat(expires_at.replace("Z", "+00:00")).timestamp()
    except Exception:
        raise BaselineError("outbound_expiry_invalid") from None
    while time.time() < deadline:
        detail = message_detail(state, message_id)
        status = detail.get("status") if isinstance(detail, dict) else None
        if status in ("submitted", "delivered"):
            if (detail.get("direction") != "outbound" or
                    sha256_text(str(detail.get("text", ""))) != expected_hash):
                raise BaselineError("server_outbound_sms_retention_mismatch")
            return detail
        if status in ("failed", "expired", "unknown"):
            raise BaselineError("outbound_sms_not_submitted")
        time.sleep(POLL_INTERVAL_SECONDS)
    raise BaselineError("outbound_sms_product_deadline")


def find_inbound(state, sim_id, expected_hash, gateway_id):
    query = urllib.parse.urlencode({"limit": "100", "sim_id": sim_id})
    response = authenticated_json(state, "GET", "/v1/messages?" + query)
    if not isinstance(response, dict) or not isinstance(response.get("items"), list):
        raise BaselineError("message_list_invalid")
    for detail in response["items"]:
        if not isinstance(detail, dict):
            continue
        if detail.get("direction") != "inbound" or detail.get("gateway_id") != gateway_id:
            continue
        if sha256_text(str(detail.get("text", ""))) == expected_hash:
            return detail
    return None


def wait_inbound(state, sim_id, expected_hash, gateway_id, timeout=HOST_SYNC_DEADLINE_SECONDS):
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        detail = find_inbound(state, sim_id, expected_hash, gateway_id)
        if detail is not None:
            return detail
        time.sleep(POLL_INTERVAL_SECONDS)
    raise BaselineError("server_inbound_sms_timeout")


def wait_host_history(expected):
    end = time.monotonic() + HOST_SYNC_DEADLINE_SECONDS
    while time.monotonic() < end:
        snapshot = snapshot_databases(HOST_SERIAL, HOST_PACKAGE)
        try:
            rows = history_rows(snapshot, expected, gateway=False)
        except BaselineError:
            raise
        if all(any(row["body_sha256"] == expected[key]["sha256"] for row in rows[key])
               for key in ("outbound", "inbound")):
            return rows
        time.sleep(POLL_INTERVAL_SECONDS)
    raise BaselineError("host_history_sync_timeout")


def build_safe_evidence(state, phase):
    evidence = {
        "schema": state["schema"],
        "phase": phase,
        "run_id": state["run_id"],
        "owner_id": state["owner_id"],
        "gateway_id": state["gateway_id"],
        "sim_id": state["sim_id"],
        "client_device_id": state["client_device_id"],
        "messages": state.get("messages", {}),
        "recorded_at": datetime.now(timezone.utc).isoformat(timespec="seconds"),
    }
    if state.get("pending_inbound_sha256"):
        evidence["pending_inbound_sha256"] = state["pending_inbound_sha256"]
    if state.get("pending_outbound_sha256"):
        evidence["pending_outbound_sha256"] = state["pending_outbound_sha256"]
    return evidence


def save_evidence(state, phase):
    report = build_safe_evidence(state, phase)
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    evidence_file = EVIDENCE / ("sms-baseline-" + state["run_id"] + "-" + phase + "-" + stamp + ".json")
    atomic_json(evidence_file, report)
    return report, evidence_file


def save_capture_failure_evidence(reason):
    """Persist safe phase/ID/hash evidence without copying session credentials."""
    if not STATE_FILE.exists():
        return
    try:
        state = json.loads(STATE_FILE.read_text(encoding="utf-8"))
        if not isinstance(state, dict) or state.get("schema") != "gsm04-sms-baseline-v1":
            return
        report = build_safe_evidence(state, "failed")
        report["failed_phase"] = state.get("phase")
        report["failure_reason"] = reason
        stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        evidence_file = EVIDENCE / ("sms-baseline-" + state["run_id"] + "-failed-" + stamp + ".json")
        atomic_json(evidence_file, report)
    except Exception:
        # The original failure remains the useful signal; never print file or
        # subprocess details that could contain credentials or message text.
        return


def capture():
    prepare_dirs()
    if STATE_FILE.exists():
        raise BaselineError("state_already_exists")
    check_emulator(GATEWAY_SERIAL)
    check_emulator(HOST_SERIAL)
    check_api()
    start_app(GATEWAY_SERIAL, GATEWAY_PACKAGE)
    start_app(HOST_SERIAL, HOST_PACKAGE)

    # Fail before creating a pairing if the opt-in local archive is disabled.
    initial_gateway_snapshot = snapshot_databases(GATEWAY_SERIAL, GATEWAY_PACKAGE)
    if not check_gateway_archive_enabled(initial_gateway_snapshot):
        raise BaselineError("enable_gateway_sms_archive_first")

    binding = active_gateway_binding()
    run_id = uuid.uuid4().hex[:16]
    pairing_code = create_pairing_code(binding["owner_id"])
    claim = api_json("POST", "/v1/pairings/claim", body={
        "pairing_code": pairing_code,
        "device_name": "joint-sms-baseline-" + run_id[:8],
        "platform": "android",
    })
    if (not isinstance(claim, dict) or claim.get("role") != "client" or
            claim.get("owner_id") != binding["owner_id"] or
            not UUID_RE.fullmatch(str(claim.get("device_id", ""))) or
            not claim.get("access_token") or not claim.get("refresh_token")):
        raise BaselineError("pairing_claim_invalid")

    state = {
        "schema": "gsm04-sms-baseline-v1",
        "phase": "paired",
        "run_id": run_id,
        "owner_id": binding["owner_id"],
        "gateway_id": binding["gateway_id"],
        "mapping_revision": binding["mapping_revision"],
        "sim_id": binding["sim_id"],
        "client_device_id": claim["device_id"],
        "access_token": claim["access_token"],
        "access_expires_at": claim.get("access_expires_at"),
        "refresh_token": claim["refresh_token"],
        "refresh_expires_at": claim.get("refresh_expires_at"),
        "messages": {},
    }
    atomic_json(STATE_FILE, state)

    outbound_text = sms_text(run_id, "outbound")
    outbound_hash = sha256_text(outbound_text)
    outbound_key = "smsbaseline:" + run_id + ":outbound"
    state["pending_outbound_sha256"] = outbound_hash
    state["pending_outbound_idempotency_key"] = outbound_key
    state["phase"] = "outbound_submitting"
    atomic_json(STATE_FILE, state)
    accepted = authenticated_json(state, "POST", "/v1/messages", body={
        "gateway_id": binding["gateway_id"],
        "sim_id": binding["sim_id"],
        "mapping_revision": binding["mapping_revision"],
        "to": SYNTHETIC_NUMBER,
        "text": outbound_text,
        "ttl_seconds": MESSAGE_TTL_SECONDS,
    }, headers={"Idempotency-Key": outbound_key})
    if not isinstance(accepted, dict) or not UUID_RE.fullmatch(str(accepted.get("message_id", ""))):
        raise BaselineError("outbound_acceptance_invalid")
    state["messages"]["outbound"] = {
        "message_id": accepted["message_id"],
        "sha256": outbound_hash,
        "server_status": accepted.get("status"),
    }
    state.pop("pending_outbound_sha256", None)
    state.pop("pending_outbound_idempotency_key", None)
    state["phase"] = "outbound_queued"
    atomic_json(STATE_FILE, state)
    outbound = wait_outbound_submission(state, accepted["message_id"], accepted.get("expires_at", ""), outbound_hash)
    state["messages"]["outbound"]["server_status"] = outbound.get("status")
    state["phase"] = "outbound_submitted"
    atomic_json(STATE_FILE, state)

    inbound_text = sms_text(run_id, "inbound")
    inbound_hash = sha256_text(inbound_text)
    state["pending_inbound_sha256"] = inbound_hash
    state["phase"] = "inbound_injected"
    atomic_json(STATE_FILE, state)
    # Exact serial + emulator-console command: fixed synthetic sender, no real device path.
    adb_text(GATEWAY_SERIAL, "emu", "sms", "send", SYNTHETIC_NUMBER, inbound_text, timeout=25)
    inbound = wait_inbound(state, binding["sim_id"], inbound_hash, binding["gateway_id"])
    if not UUID_RE.fullmatch(str(inbound.get("message_id", ""))):
        raise BaselineError("inbound_message_id_invalid")
    state["messages"]["inbound"] = {
        "message_id": inbound["message_id"],
        "sha256": inbound_hash,
        "server_status": inbound.get("status"),
    }
    state.pop("pending_inbound_sha256", None)
    state["phase"] = "inbound_received"
    atomic_json(STATE_FILE, state)

    expected = state["messages"]
    wait_host_history(expected)
    final_gateway_snapshot = snapshot_databases(GATEWAY_SERIAL, GATEWAY_PACKAGE)
    if not check_gateway_archive_enabled(final_gateway_snapshot):
        raise BaselineError("gateway_archive_disabled_during_capture")
    assert_history(final_gateway_snapshot, expected, gateway=True)
    host_snapshot = snapshot_databases(HOST_SERIAL, HOST_PACKAGE)
    assert_history(host_snapshot, expected, gateway=False)
    state["phase"] = "captured"
    state["captured_at"] = datetime.now(timezone.utc).isoformat(timespec="seconds")
    atomic_json(STATE_FILE, state)
    report, evidence_file = save_evidence(state, "captured")
    print(json.dumps({"status": "PASS", "phase": "captured", "evidence": str(evidence_file),
                      "messages": report["messages"], "client_device_id": state["client_device_id"]}, sort_keys=True))


def verify():
    prepare_dirs()
    state = read_state()
    if state.get("phase") not in ("captured", "verified") or set(state.get("messages", {})) != {"outbound", "inbound"}:
        raise BaselineError("baseline_capture_incomplete")
    check_emulator(GATEWAY_SERIAL)
    check_emulator(HOST_SERIAL)
    check_api()

    for direction in ("outbound", "inbound"):
        expected = state["messages"][direction]
        detail = message_detail(state, expected["message_id"])
        if not isinstance(detail, dict):
            raise BaselineError("server_message_invalid")
        if detail.get("direction") != direction or sha256_text(str(detail.get("text", ""))) != expected["sha256"]:
            raise BaselineError("server_message_retention_mismatch")
        expected["server_status"] = detail.get("status")

    gateway_snapshot = snapshot_databases(GATEWAY_SERIAL, GATEWAY_PACKAGE)
    assert_history(gateway_snapshot, state["messages"], gateway=True)
    host_snapshot = snapshot_databases(HOST_SERIAL, HOST_PACKAGE)
    assert_history(host_snapshot, state["messages"], gateway=False)
    state["phase"] = "verified"
    state["verified_at"] = datetime.now(timezone.utc).isoformat(timespec="seconds")
    atomic_json(STATE_FILE, state)
    report, evidence_file = save_evidence(state, "verified")
    print(json.dumps({"status": "PASS", "phase": "verified", "evidence": str(evidence_file),
                      "messages": report["messages"], "client_device_id": state["client_device_id"]}, sort_keys=True))


def cleanup(case6_complete):
    prepare_dirs()
    if not case6_complete:
        raise BaselineError("cleanup_requires_case6_complete")
    state = read_state()
    if state.get("phase") not in ("paired", "outbound_queued", "outbound_submitted", "inbound_injected",
                                    "inbound_received", "captured", "verified"):
        raise BaselineError("cleanup_state_invalid")
    auth_state, auth_path = active_auth_state(state)
    if auth_state.get("pending_refresh_key"):
        refresh_session(auth_state, auth_path)
    try:
        api_json("POST", "/v1/auth/revoke", body={"refresh_token": auth_state["refresh_token"]})
    except APIStatusError as error:
        if error.status != 204:
            raise BaselineError("temporary_client_revoke_failed") from None
    except BaselineError:
        raise
    state.pop("access_token", None)
    state.pop("refresh_token", None)
    state.pop("pending_refresh_key", None)
    state["cleanup_complete"] = True
    state["phase"] = "cleanup_complete"
    state["cleanup_at"] = datetime.now(timezone.utc).isoformat(timespec="seconds")
    atomic_json(STATE_FILE, state)
    if auth_path == PROBE_AUTH_FILE:
        try:
            PROBE_AUTH_FILE.unlink()
        except FileNotFoundError:
            pass
    report, evidence_file = save_evidence(state, "cleanup_complete")
    print(json.dumps({"status": "PASS", "phase": "cleanup_complete",
                      "client_device_id": state["client_device_id"],
                      "evidence": str(evidence_file), "message_count": len(report["messages"])}, sort_keys=True))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)
    subparsers.add_parser("capture", help="create temporary client and capture baseline SMS")
    subparsers.add_parser("verify", help="read-only post-upgrade server and SQLite retention check")
    cleanup_parser = subparsers.add_parser("cleanup", help="revoke temporary client after case 6")
    cleanup_parser.add_argument("--case6-complete", action="store_true",
                                help="required guard confirming the planned device-revocation case is finished")
    args = parser.parse_args()
    try:
        if args.command == "capture":
            capture()
        elif args.command == "verify":
            verify()
        else:
            cleanup(args.case6_complete)
    except BaselineError as error:
        if args.command == "capture":
            save_capture_failure_evidence(error.code)
        print(json.dumps({"status": "FAIL", "reason": error.code}, sort_keys=True))
        raise SystemExit(1)
    except Exception:
        # Subprocess/HTTP exceptions can carry response fragments; never display them.
        if args.command == "capture":
            save_capture_failure_evidence("unexpected_error_details_suppressed")
        print(json.dumps({"status": "FAIL", "reason": "unexpected_error_details_suppressed"}, sort_keys=True))
        raise SystemExit(1)


if __name__ == "__main__":
    main()
