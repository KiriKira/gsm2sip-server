#!/usr/bin/env python3
"""Send one synthetic SMS pair per case-5 label, then verify it idempotently.

The first invocation for a label submits one outbound message through API A and
injects one synthetic inbound SMS on the pinned Gateway emulator. The label's
private ledger is written before either side effect. Any later invocation for
that label is read-only: it reconciles saved IDs and verifies server, Gateway
archive, and Host history, but never submits or injects another message.

This script reuses the saved temporary SMS-baseline client. It never creates a
pairing, uses an RTC endpoint, targets another API node, or accepts a serial or
phone-number override. Output contains IDs and hashes only, never SMS text or
credentials.
"""

import argparse
import datetime
import fcntl
import hashlib
import importlib.util
import json
import os
import re
import sys
import time
import urllib.parse
import uuid
from pathlib import Path


SCRIPT_DIR = Path(__file__).resolve().parent
BASELINE_SCRIPT = SCRIPT_DIR / "sms-baseline.py"
_spec = importlib.util.spec_from_file_location("sms_probe_baseline", BASELINE_SCRIPT)
if _spec is None or _spec.loader is None:
    raise RuntimeError("sms_baseline_helper_unavailable")
BASE = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(BASE)

PRIVATE = BASE.PRIVATE
EVIDENCE = BASE.EVIDENCE
PROBE_DIR = PRIVATE / "sms-probes"
AUTH_FILE = PRIVATE / "sms-probe-client-auth.json"
LOCK_FILE = PRIVATE / "sms-probe.lock"
BASELINE_IDENTITIES = EVIDENCE / "baseline-identities.json"
RECOVERY_IDENTITY_REFERENCE = EVIDENCE / "emulator-retained-sim-recovery.json"
RECOVERY_IDENTITY_REASON = "explicit_same_virtual_modem_confirmation_after_fingerprintless_AVD_reboot"
LABELS = ("server-baseline", "gateway-baseline", "host-baseline")
HEX64_RE = re.compile(r"^[0-9a-f]{64}$")


class ProbeError(Exception):
    """A safe, non-sensitive failure code."""


def utc_now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds")


def label_state_path(label):
    return PROBE_DIR / (label + ".json")


def read_identity_reference(path, code):
    if path.is_symlink() or not path.is_file():
        raise ProbeError(code)
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except Exception:
        raise ProbeError(code) from None
    if not isinstance(value, dict):
        raise ProbeError(code)
    return value


def probe_binding_state(baseline_state):
    """Use only the pinned baseline or its explicit, same-SIM recovery proof."""
    baseline = read_identity_reference(BASELINE_IDENTITIES, "baseline_identity_reference_invalid")
    original = baseline.get("gateway_identity")
    if (not isinstance(original, dict) or original.get("status") != "READY" or
            not BASE.UUID_RE.fullmatch(str(original.get("gateway_id", ""))) or
            not BASE.UUID_RE.fullmatch(str(original.get("sim_id", ""))) or
            type(original.get("mapping_revision")) is not int or
            original["mapping_revision"] < 1):
        raise ProbeError("baseline_identity_reference_invalid")
    if (baseline_state.get("gateway_id") != original["gateway_id"] or
            baseline_state.get("sim_id") != original["sim_id"] or
            type(baseline_state.get("mapping_revision")) is not int or
            baseline_state.get("mapping_revision") != original["mapping_revision"]):
        raise ProbeError("probe_baseline_identity_mismatch")

    result = dict(baseline_state)
    if RECOVERY_IDENTITY_REFERENCE.exists() or RECOVERY_IDENTITY_REFERENCE.is_symlink():
        recovery = read_identity_reference(
            RECOVERY_IDENTITY_REFERENCE, "recovery_identity_reference_invalid"
        )
        recovered = recovery.get("gateway_identity")
        if (recovery.get("status") != "PASS" or
                recovery.get("reason") != RECOVERY_IDENTITY_REASON or
                not isinstance(recovered, dict) or recovered.get("status") != "READY" or
                recovered.get("gateway_id") != original["gateway_id"] or
                recovered.get("sim_id") != original["sim_id"] or
                type(recovered.get("mapping_revision")) is not int or
                recovered["mapping_revision"] <= original["mapping_revision"]):
            raise ProbeError("recovery_identity_reference_mismatch")
        result["mapping_revision"] = recovered["mapping_revision"]
    return result


def read_private_json(path, schema):
    if path.is_symlink():
        raise ProbeError("private_state_symlink_refused")
    if not path.is_file():
        raise ProbeError("private_state_missing")
    if path.stat().st_mode & 0o077:
        raise ProbeError("private_state_permissions_unsafe")
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except Exception:
        raise ProbeError("private_state_unreadable") from None
    if not isinstance(value, dict) or value.get("schema") != schema:
        raise ProbeError("private_state_invalid")
    return value


def read_auth(baseline_state):
    if AUTH_FILE.exists() or AUTH_FILE.is_symlink():
        auth = read_private_json(AUTH_FILE, "gsm04-sms-probe-auth-v1")
        if auth.get("client_device_id") != baseline_state.get("client_device_id"):
            raise ProbeError("probe_client_identity_changed")
        if not auth.get("access_token") or not auth.get("refresh_token"):
            raise ProbeError("probe_client_auth_invalid")
        return auth

    if not baseline_state.get("access_token") or not baseline_state.get("refresh_token"):
        raise ProbeError("baseline_client_auth_missing")
    auth = {
        "schema": "gsm04-sms-probe-auth-v1",
        "client_device_id": baseline_state["client_device_id"],
        "access_token": baseline_state["access_token"],
        "access_expires_at": baseline_state.get("access_expires_at"),
        "refresh_token": baseline_state["refresh_token"],
        "refresh_expires_at": baseline_state.get("refresh_expires_at"),
    }
    BASE.atomic_json(AUTH_FILE, auth)
    return auth


def refresh_auth(auth):
    key = auth.get("pending_refresh_key")
    if not key:
        key = "sms-probe-refresh:" + uuid.uuid4().hex
        auth["pending_refresh_key"] = key
        BASE.atomic_json(AUTH_FILE, auth)
    response = BASE.api_json(
        "POST", "/v1/auth/refresh",
        body={"refresh_token": auth["refresh_token"]},
        headers={"Idempotency-Key": key},
    )
    if not isinstance(response, dict) or not response.get("access_token") or not response.get("refresh_token"):
        raise ProbeError("probe_refresh_response_invalid")
    auth["access_token"] = response["access_token"]
    auth["refresh_token"] = response["refresh_token"]
    auth["access_expires_at"] = response.get("access_expires_at")
    auth["refresh_expires_at"] = response.get("refresh_expires_at")
    auth.pop("pending_refresh_key", None)
    BASE.atomic_json(AUTH_FILE, auth)


def auth_json(auth, method, path, body=None, headers=None):
    try:
        return BASE.api_json(method, path, body=body, token=auth["access_token"], headers=headers)
    except BASE.APIStatusError as error:
        if error.status != 401:
            raise ProbeError("api_request_rejected_http_" + str(error.status)) from None
    refresh_auth(auth)
    try:
        return BASE.api_json(method, path, body=body, token=auth["access_token"], headers=headers)
    except BASE.APIStatusError as error:
        raise ProbeError("api_request_rejected_after_refresh_http_" + str(error.status)) from None


def message_detail(auth, message_id):
    return auth_json(auth, "GET", "/v1/messages/" + urllib.parse.quote(message_id, safe=""))


def list_messages(auth, sim_id):
    query = urllib.parse.urlencode({"limit": "100", "sim_id": sim_id})
    value = auth_json(auth, "GET", "/v1/messages?" + query)
    if not isinstance(value, dict) or not isinstance(value.get("items"), list):
        raise ProbeError("message_list_invalid")
    return value["items"]


def matching_message(auth, sim_id, gateway_id, direction, expected_hash):
    matches = []
    for item in list_messages(auth, sim_id):
        if not isinstance(item, dict):
            continue
        if item.get("direction") != direction or item.get("gateway_id") != gateway_id:
            continue
        if BASE.sha256_text(str(item.get("text", ""))) == expected_hash:
            matches.append(item)
    if len(matches) > 1:
        raise ProbeError("duplicate_probe_server_message")
    return matches[0] if matches else None


def wait_matching_message(auth, state, direction, timeout=60):
    end = time.monotonic() + timeout
    expected = state["messages"][direction]
    while time.monotonic() < end:
        item = matching_message(
            auth, state["sim_id"], state["gateway_id"], direction, expected["sha256"]
        )
        if item is not None:
            return item
        time.sleep(BASE.POLL_INTERVAL_SECONDS)
    return None


def validate_message(detail, state, direction):
    expected = state["messages"][direction]
    if not isinstance(detail, dict):
        raise ProbeError("server_message_invalid")
    if (detail.get("direction") != direction or
            detail.get("gateway_id") != state["gateway_id"] or
            detail.get("sim_id") != state["sim_id"] or
            detail.get("mapping_revision") != state["mapping_revision"] or
            BASE.sha256_text(str(detail.get("text", ""))) != expected["sha256"]):
        raise ProbeError("server_message_retention_mismatch")
    expected["server_status"] = detail.get("status")
    if direction == "inbound" and detail.get("status") != "received":
        raise ProbeError("inbound_sms_not_received")
    return detail


def wait_outbound_submitted(auth, state):
    expected = state["messages"]["outbound"]
    detail = message_detail(auth, expected["message_id"])
    if not isinstance(detail, dict):
        raise ProbeError("outbound_message_invalid")
    expires_at = detail.get("expires_at") or expected.get("expires_at")
    try:
        deadline = datetime.datetime.fromisoformat(str(expires_at).replace("Z", "+00:00")).timestamp()
    except Exception:
        raise ProbeError("outbound_expiry_invalid") from None
    while True:
        detail = validate_message(message_detail(auth, expected["message_id"]), state, "outbound")
        status = detail.get("status")
        if status in ("submitted", "delivered"):
            return detail
        if status in ("failed", "expired", "unknown"):
            raise ProbeError("outbound_sms_not_submitted")
        if time.time() >= deadline:
            raise ProbeError("outbound_sms_product_deadline")
        time.sleep(BASE.POLL_INTERVAL_SECONDS)


def expected_history(state):
    return {
        direction: {
            "message_id": state["messages"][direction].get("message_id"),
            "sha256": state["messages"][direction]["sha256"],
        }
        for direction in ("outbound", "inbound")
    }


def local_history_ready(snapshot, expected, gateway):
    rows = BASE.history_rows(snapshot, expected, gateway=gateway)
    return all(
        any(row["body_sha256"] == expected[direction]["sha256"] for row in rows[direction])
        for direction in ("outbound", "inbound")
    )


def wait_local_history(expected, gateway):
    serial = BASE.GATEWAY_SERIAL if gateway else BASE.HOST_SERIAL
    package = BASE.GATEWAY_PACKAGE if gateway else BASE.HOST_PACKAGE
    end = time.monotonic() + BASE.HOST_SYNC_DEADLINE_SECONDS
    while time.monotonic() < end:
        snapshot = BASE.snapshot_databases(serial, package)
        if local_history_ready(snapshot, expected, gateway):
            return snapshot
        time.sleep(BASE.POLL_INTERVAL_SECONDS)
    raise ProbeError("gateway_archive_sync_timeout" if gateway else "host_history_sync_timeout")


def verify_local_history(state):
    expected = expected_history(state)
    gateway_snapshot = wait_local_history(expected, gateway=True)
    if not BASE.check_gateway_archive_enabled(gateway_snapshot):
        raise ProbeError("gateway_archive_disabled")
    BASE.assert_history(gateway_snapshot, expected, gateway=True)
    host_snapshot = wait_local_history(expected, gateway=False)
    BASE.assert_history(host_snapshot, expected, gateway=False)
    for direction in ("outbound", "inbound"):
        state["messages"][direction].update(expected[direction])


def safe_messages(state):
    result = {}
    for direction in ("outbound", "inbound"):
        item = state["messages"][direction]
        result[direction] = {
            key: item[key] for key in (
                "message_id", "sha256", "server_status", "gateway_archive_count",
                "gateway_archive_status", "host_history_count", "host_history_status",
            ) if key in item
        }
    return result


def save_evidence(state, status):
    report = {
        "schema": "gsm04-sms-probe-evidence-v1",
        "label": state["label"],
        "status": status,
        "run_id": state["run_id"],
        "client_device_id": state["client_device_id"],
        "gateway_id": state["gateway_id"],
        "sim_id": state["sim_id"],
        "mapping_revision": state["mapping_revision"],
        "messages": safe_messages(state),
        "recorded_at": utc_now(),
    }
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    path = EVIDENCE / ("sms-probe-" + state["label"] + "-" + state["run_id"] + "-" + stamp + ".json")
    BASE.atomic_json(path, report)
    return path


def save_state(state):
    BASE.atomic_json(label_state_path(state["label"]), state)


def validate_state(state, label, baseline_state):
    if (state.get("schema") != "gsm04-sms-probe-v1" or state.get("label") != label or
            state.get("client_device_id") != baseline_state.get("client_device_id") or
            state.get("gateway_id") != baseline_state.get("gateway_id") or
            state.get("sim_id") != baseline_state.get("sim_id") or
            state.get("mapping_revision") != baseline_state.get("mapping_revision") or
            not isinstance(state.get("messages"), dict) or
            set(state["messages"]) != {"outbound", "inbound"}):
        raise ProbeError("probe_state_invalid")
    if not re.fullmatch(r"[0-9a-f]{16}", str(state.get("run_id", ""))):
        raise ProbeError("probe_state_invalid")
    for direction in ("outbound", "inbound"):
        item = state["messages"][direction]
        if not isinstance(item, dict) or not HEX64_RE.fullmatch(str(item.get("sha256", ""))):
            raise ProbeError("probe_state_invalid")
        message_id = item.get("message_id")
        if message_id is not None and not BASE.UUID_RE.fullmatch(str(message_id)):
            raise ProbeError("probe_state_invalid")


def verify_existing(auth, state, baseline_state):
    validate_state(state, state.get("label"), baseline_state)
    BASE.check_emulator(BASE.GATEWAY_SERIAL)
    BASE.check_emulator(BASE.HOST_SERIAL)
    BASE.check_api()

    # A persisted label always takes the verification-only path. A lost POST
    # response may be reconciled from the server list, but no side effect is
    # retried and no emulator SMS is reinjected.
    for direction in ("outbound", "inbound"):
        item = state["messages"][direction]
        if not item.get("message_id"):
            found = wait_matching_message(auth, state, direction)
            if found is None:
                raise ProbeError("existing_probe_message_missing_no_resend")
            item["message_id"] = found.get("message_id")
            if not BASE.UUID_RE.fullmatch(str(item["message_id"])):
                raise ProbeError("server_message_id_invalid")
            if direction == "outbound":
                item["expires_at"] = found.get("expires_at")
            save_state(state)

        detail = validate_message(message_detail(auth, item["message_id"]), state, direction)
        if direction == "inbound":
            if detail.get("status") != "received":
                raise ProbeError("inbound_sms_not_received")
        else:
            detail = wait_outbound_submitted(auth, state)
            validate_message(detail, state, direction)

    if state.get("messages", {}).get("inbound", {}).get("injected_at") is None:
        # Do not infer that an emulator-injection attempt can safely be
        # repeated. Reaching this branch means the server happened to contain
        # a matching event, which is sufficient to finish verification.
        state["messages"]["inbound"]["observed_server_at"] = utc_now()
    verify_local_history(state)
    state["phase"] = "verified"
    state["verified_at"] = utc_now()
    save_state(state)
    evidence = save_evidence(state, "verified")
    return evidence


def create_probe_state(label, baseline_state):
    run_id = uuid.uuid4().hex[:16]
    outbound_text = "gsm04probe-" + label + "-" + run_id + "-outbound"
    inbound_text = "gsm04probe-" + label + "-" + run_id + "-inbound"
    state = {
        "schema": "gsm04-sms-probe-v1",
        "label": label,
        "phase": "prepared",
        "run_id": run_id,
        "created_at": utc_now(),
        "client_device_id": baseline_state["client_device_id"],
        "gateway_id": baseline_state["gateway_id"],
        "sim_id": baseline_state["sim_id"],
        "mapping_revision": baseline_state["mapping_revision"],
        "messages": {
            "outbound": {
                "sha256": BASE.sha256_text(outbound_text),
                "idempotency_key": "sms-probe:" + label + ":" + run_id + ":outbound",
            },
            "inbound": {"sha256": BASE.sha256_text(inbound_text)},
        },
    }
    save_state(state)
    return state, outbound_text, inbound_text


def first_run(auth, baseline_state, label):
    BASE.check_emulator(BASE.GATEWAY_SERIAL)
    BASE.check_emulator(BASE.HOST_SERIAL)
    BASE.check_api()
    binding = {
        "gateway_id": baseline_state.get("gateway_id"),
        "sim_id": baseline_state.get("sim_id"),
        "mapping_revision": baseline_state.get("mapping_revision"),
    }
    if (not BASE.UUID_RE.fullmatch(str(binding["gateway_id"])) or
            not BASE.UUID_RE.fullmatch(str(binding["sim_id"])) or
            not isinstance(binding["mapping_revision"], int) or binding["mapping_revision"] < 1):
        raise ProbeError("baseline_sms_binding_invalid")
    gateway_snapshot = BASE.snapshot_databases(BASE.GATEWAY_SERIAL, BASE.GATEWAY_PACKAGE)
    if not BASE.check_gateway_archive_enabled(gateway_snapshot):
        raise ProbeError("enable_gateway_sms_archive_first")

    state, outbound_text, inbound_text = create_probe_state(label, baseline_state)
    outbound = state["messages"]["outbound"]
    outbound["submit_started_at"] = utc_now()
    state["phase"] = "outbound_submit_started"
    save_state(state)
    try:
        accepted = auth_json(auth, "POST", "/v1/messages", body={
            "gateway_id": binding["gateway_id"],
            "sim_id": binding["sim_id"],
            "mapping_revision": binding["mapping_revision"],
            "to": BASE.SYNTHETIC_NUMBER,
            "text": outbound_text,
            "ttl_seconds": BASE.MESSAGE_TTL_SECONDS,
        }, headers={"Idempotency-Key": outbound["idempotency_key"]})
    except Exception:
        # Keep the intent ledger: a later run can only reconcile by GET and
        # must never replay the side effect for this label.
        save_state(state)
        raise
    if not isinstance(accepted, dict) or not BASE.UUID_RE.fullmatch(str(accepted.get("message_id", ""))):
        raise ProbeError("outbound_acceptance_invalid")
    outbound["message_id"] = accepted["message_id"]
    outbound["expires_at"] = accepted.get("expires_at")
    outbound["server_status"] = accepted.get("status")
    state["phase"] = "outbound_accepted"
    save_state(state)

    detail = wait_outbound_submitted(auth, state)
    validate_message(detail, state, "outbound")
    state["phase"] = "outbound_submitted"
    save_state(state)

    inbound = state["messages"]["inbound"]
    inbound["injection_started_at"] = utc_now()
    state["phase"] = "inbound_injection_started"
    save_state(state)
    try:
        BASE.adb_text(
            BASE.GATEWAY_SERIAL, "emu", "sms", "send",
            BASE.SYNTHETIC_NUMBER, inbound_text, timeout=25,
        )
    except Exception:
        # Emulator command outcome may be uncertain. The label is already
        # durable, so all later invocations remain verification-only.
        save_state(state)
        raise
    inbound["injected_at"] = utc_now()
    state["phase"] = "inbound_injected"
    save_state(state)

    found = wait_matching_message(auth, state, "inbound")
    if found is None:
        raise ProbeError("server_inbound_sms_timeout")
    inbound["message_id"] = found.get("message_id")
    if not BASE.UUID_RE.fullmatch(str(inbound["message_id"])):
        raise ProbeError("inbound_message_id_invalid")
    inbound["server_status"] = found.get("status")
    state["phase"] = "inbound_received"
    save_state(state)

    evidence = verify_existing(auth, state, baseline_state)
    return evidence


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--label", required=True, choices=LABELS)
    args = parser.parse_args()
    os.umask(0o077)
    try:
        BASE.prepare_dirs()
        PROBE_DIR.mkdir(parents=True, exist_ok=True, mode=0o700)
        PROBE_DIR.chmod(0o700)
        lock_fd = os.open(LOCK_FILE, os.O_WRONLY | os.O_CREAT, 0o600)
        os.fchmod(lock_fd, 0o600)
        with os.fdopen(lock_fd, "w") as lock_stream:
            fcntl.flock(lock_stream.fileno(), fcntl.LOCK_EX)
            baseline_state = BASE.read_state()
            if (baseline_state.get("phase") not in ("captured", "verified") or
                    set(baseline_state.get("messages", {})) != {"outbound", "inbound"} or
                    not BASE.UUID_RE.fullmatch(str(baseline_state.get("client_device_id", "")))):
                raise ProbeError("baseline_capture_incomplete")
            probe_baseline = probe_binding_state(baseline_state)
            auth = read_auth(baseline_state)
            state_path = label_state_path(args.label)
            if state_path.exists() or state_path.is_symlink():
                state = read_private_json(state_path, "gsm04-sms-probe-v1")
                evidence = verify_existing(auth, state, probe_baseline)
                result = {"status": "PASS", "phase": "verified", "label": args.label,
                          "evidence": str(evidence), "client_device_id": state["client_device_id"],
                          "gateway_id": state["gateway_id"], "sim_id": state["sim_id"],
                          "messages": safe_messages(state), "send_performed": False}
            else:
                evidence = first_run(auth, probe_baseline, args.label)
                state = read_private_json(state_path, "gsm04-sms-probe-v1")
                result = {"status": "PASS", "phase": "captured", "label": args.label,
                          "evidence": str(evidence), "client_device_id": state["client_device_id"],
                          "gateway_id": state["gateway_id"], "sim_id": state["sim_id"],
                          "messages": safe_messages(state), "send_performed": True}
            print(json.dumps(result, sort_keys=True))
    except (ProbeError, BASE.BaselineError) as error:
        print(json.dumps({"status": "FAIL", "label": args.label, "reason": str(error)}, sort_keys=True))
        raise SystemExit(1)
    except BASE.APIStatusError as error:
        print(json.dumps({"status": "FAIL", "label": args.label,
                          "reason": "api_status_" + str(error.status)}, sort_keys=True))
        raise SystemExit(1)
    except Exception:
        # Do not leak exception strings: requests, subprocesses, or state may
        # contain a token or SMS text.
        print(json.dumps({"status": "FAIL", "label": args.label, "reason": "internal_error"}, sort_keys=True))
        raise SystemExit(1)


if __name__ == "__main__":
    main()
