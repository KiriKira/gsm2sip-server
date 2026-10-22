#!/usr/bin/env python3
"""Export a tightly filtered RTC media/control evidence snapshot.

This helper is intentionally read-only against the remote lab. It reads the
two private Android logcat captures and Docker logs for the three server nodes;
if a node has no parseable call-ID log rows, it may read the matching call
snapshots from that node's PostgreSQL database. Raw input lines are never
written, printed, or included in errors.
"""

from __future__ import annotations

import argparse
import datetime as dt
import gzip
import hashlib
import json
import os
import pathlib
import re
import shlex
import subprocess
import zlib
from typing import Callable, Iterable


DEFAULT_REMOTE = "ubuntu@18.119.132.34"
DEFAULT_KEY = "/data0/codes/kiri-m7i-flex-large.pem"
DEFAULT_REMOTE_ROOT = "/home/ubuntu/gsm-webrtc-validation/joint-0.4"

UUID_RE = re.compile(
    r"^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-"
    r"[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$"
)
KV_RE = re.compile(r"(?:^|\s)([a-z_]+)=([^\s]+)")
QUOTED_MSG_RE = re.compile(
    r'(?:^|\s)msg="(RTC call created|call media selected|'
    r'RTC cellular media active|RTC control committed|RTC signal committed)"'
)
ISO_TS_RE = re.compile(r"\b(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z)\b")
LOGCAT_TS_RE = re.compile(r"\b(\d{2}-\d{2}\s+\d{2}:\d{2}:\d{2}\.\d{3})\b")

DEVICE_EVENTS = {
    "selected", "native_ready", "offer_created", "offer_committed",
    "answer_created", "answer_committed", "answer_applied",
    "restart_committed", "end_of_candidates_committed", "close_committed",
    "cellular_active", "media_and_cellular_ready", "native_closed",
    "audio_enabled", "audio_suspended", "late_local_candidate_ignored",
    "dtmf_sent", "dtmf_received", "stats", "prepared",
    "cellular_pcm_started", "failed", "closed",
}
PEER_STATES = {"new", "connecting", "connected", "disconnected", "failed", "closed"}
ICE_STATES = {"new", "checking", "connected", "completed", "disconnected", "failed", "closed"}
GATHERING_STATES = {"new", "gathering", "complete"}
CONTROL_KINDS = {
    "start", "accept", "release", "winner", "dispatch", "ringing", "active",
    "reject", "cancel", "hangup", "answered_elsewhere", "ended",
}
SIGNAL_KINDS = {"offer", "answer", "candidate", "end_of_candidates", "close", "restart"}
SERVER_MESSAGES = {
    "RTC call created": "call_created",
    "call media selected": "media_selected",
    "RTC cellular media active": "cellular_media_active",
}
CALL_STATES = {
    "reserved", "pending_wakeup", "ringing", "dialing", "connecting",
    "active", "ended", "unknown",
}
CONTROL_TRANSPORTS = {"rtc", "sip"}
CANDIDATE_TYPES = {"host", "srflx", "prflx", "relay"}

# Emit only known, non-user-controlled reason tokens. Unknown reason values are
# omitted instead of being copied or generalized from a potentially sensitive
# free-form string.
SAFE_REASONS = {
    "audio_capture_init", "audio_capture_start", "audio_capture_runtime",
    "audio_playout_init", "audio_playout_start", "audio_playout_runtime",
    "cellular_authority_update_failed", "dtls_connection_failed", "ice_failed",
    "ice_recovery_timeout", "invalid_control_message", "local_candidate_budget",
    "media_negotiation_timeout", "media_signal_or_native_failure",
    "native_or_offer_setup_failed", "remote_closed", "server_authorization_closed",
    "signal_unavailable", "external_pcm_capture_format", "external_pcm_playout_format",
    "native_orchestrator_failed", "pcm_timeout", "ice_timeout", "opus_unavailable",
    "normal_clearing", "host_hangup", "gateway_hangup", "cellular_ended",
    "sip_removed", "answered_elsewhere", "rtc_media_failed", "rtc_media_timeout",
    "ari_reconciliation_uncertain", "timeout", "cancelled", "rejected", "completed",
}

NUMERIC_FIELDS = {
    "elapsed_ms", "rate", "tx_packets", "rx_packets", "pcm_capture",
    "pcm_decoded", "pcm_underruns", "pcm_overruns",
}
NUMERIC_RE = re.compile(r"^[0-9]{1,18}$")
TOKEN_FIELDS = {
    "media", "call_control", "call_id", "leg_id", "role", "event",
    "reason", "selected_pair", "revision",
} | NUMERIC_FIELDS


def timestamp_from(line: str) -> str | None:
    match = ISO_TS_RE.search(line)
    if match:
        return match.group(1)
    match = LOGCAT_TS_RE.search(line)
    return match.group(1) if match else None


def tokens_from(line: str) -> dict[str, str]:
    # Discard all unrecognized keys at parse time, so their values never enter
    # even the transient parsed-field map.
    return {key: value for key, value in KV_RE.findall(line) if key in TOKEN_FIELDS}


def add_uuid(record: dict, field: str, value: str | None) -> bool:
    if value is None or not UUID_RE.fullmatch(value):
        return False
    record[field] = value.lower()
    return True


def add_numeric_fields(record: dict, tokens: dict[str, str]) -> None:
    for key in NUMERIC_FIELDS:
        value = tokens.get(key)
        if value is not None and NUMERIC_RE.fullmatch(value):
            record[key] = int(value)


def safe_device_event(value: str) -> str | None:
    if value in DEVICE_EVENTS:
        return value
    for prefix, states in (
        ("peer_", PEER_STATES),
        ("ice_", ICE_STATES),
        ("gathering_", GATHERING_STATES),
    ):
        if value.startswith(prefix) and value[len(prefix):] in states:
            return value
    return None


def parse_device_line(line: str, source: str) -> dict | None:
    tokens = tokens_from(line)
    if tokens.get("media") != "webrtc" or tokens.get("call_control") != "rtc":
        return None
    role = tokens.get("role")
    if role not in {"host", "gateway"}:
        return None
    event = safe_device_event(tokens.get("event", ""))
    if event is None:
        return None

    record: dict[str, object] = {"source": source, "event": event, "role": role}
    if not add_uuid(record, "call_id", tokens.get("call_id")):
        return None
    add_uuid(record, "leg_id", tokens.get("leg_id"))
    timestamp = timestamp_from(line)
    if timestamp:
        record["timestamp"] = timestamp

    reason = tokens.get("reason")
    if reason in SAFE_REASONS:
        record["reason"] = reason

    if event == "stats":
        pair = tokens.get("selected_pair", "")
        if "/" in pair:
            local, remote = pair.split("/", 1)
            if local in CANDIDATE_TYPES and remote in CANDIDATE_TYPES:
                record["selected_pair"] = f"{local}/{remote}"
        add_numeric_fields(record, tokens)
    elif event == "cellular_pcm_started":
        add_numeric_fields(record, tokens)
    elif event in {"native_ready", "offer_created"}:
        add_numeric_fields(record, tokens)
    return record


def parse_server_line(line: str, node: str) -> dict | None:
    # slog's message must match one of these fixed strings. Arbitrary msg text
    # and all unrecognized key/value fields are discarded.
    msg_match = QUOTED_MSG_RE.search(line)
    if not msg_match:
        return None
    message = msg_match.group(1)
    tokens = tokens_from(line)
    if tokens.get("call_control") not in {None, "rtc"}:
        return None

    event = SERVER_MESSAGES.get(message)
    if message == "RTC control committed":
        kind = tokens.get("event", "")
        if kind not in CONTROL_KINDS:
            return None
        event = f"control_{kind}_committed"
    elif message == "RTC signal committed":
        kind = tokens.get("event", "")
        if kind not in SIGNAL_KINDS:
            return None
        event = f"signal_{kind}_committed"
    if event is None:
        return None

    record: dict[str, object] = {"source": "server_log", "node": node, "event": event}
    if not add_uuid(record, "call_id", tokens.get("call_id")):
        return None
    add_uuid(record, "leg_id", tokens.get("leg_id"))
    timestamp = timestamp_from(line)
    if timestamp:
        record["timestamp"] = timestamp
    if message == "RTC control committed":
        revision = tokens.get("revision", "")
        if NUMERIC_RE.fullmatch(revision):
            record["state_revision"] = int(revision)
    return record


def ssh_base(args: argparse.Namespace) -> list[str]:
    return [
        "ssh", "-F", "/dev/null", "-o", "BatchMode=yes", "-o",
        "ConnectTimeout=12", "-o", "IdentitiesOnly=yes", "-i", args.ssh_key,
        args.remote,
    ]


def stream_remote(args: argparse.Namespace, remote_command: str,
                  consume: Callable[[str], None]) -> int:
    process = subprocess.Popen(
        ssh_base(args) + [remote_command], stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
    )
    assert process.stdout is not None
    try:
        for raw in iter(process.stdout.readline, b""):
            consume(raw.decode("utf-8", errors="replace").rstrip("\r\n"))
    finally:
        process.stdout.close()
    return process.wait()


def remote_cat(args: argparse.Namespace, relative: str,
               consume: Callable[[str], None]) -> int:
    path = f"{args.remote_root}/private/{relative}"
    process = subprocess.Popen(
        ssh_base(args) + [f"gzip -c -- {shlex.quote(path)}"],
        stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
    )
    assert process.stdout is not None
    failed = False
    try:
        # Decompress directly from the SSH pipe; neither compressed input nor
        # raw log lines are written to disk or exposed in error messages.
        with gzip.GzipFile(fileobj=process.stdout, mode="rb") as stream:
            for raw in iter(stream.readline, b""):
                consume(raw.decode("utf-8", errors="replace").rstrip("\r\n"))
    except (OSError, EOFError, zlib.error):
        failed = True
        if process.poll() is None:
            process.terminate()
    finally:
        process.stdout.close()
        exit_code = process.wait()
    return 255 if failed else exit_code


def remote_docker_logs(args: argparse.Namespace, container: str,
                       consume: Callable[[str], None]) -> int:
    command = "sudo -n docker logs --timestamps " + shlex.quote(container)
    return stream_remote(args, command, consume)


def collect_source(args: argparse.Namespace, name: str, reader: Callable,
                   parser: Callable[[str], dict | None], source: str,
                   records: list[dict]) -> dict:
    seen = 0
    emitted = 0

    def consume(line: str) -> None:
        nonlocal seen, emitted
        seen += 1
        record = parser(line)
        if record is not None:
            records.append(record)
            emitted += 1

    try:
        exit_code = reader(args, name, consume)
    except OSError:
        exit_code = 255
    return {
        "status": "read" if exit_code == 0 else "unavailable",
        "lines_seen": seen,
        "records": emitted,
        "remote_exit_code": exit_code,
        "source": source,
    }


def psql_snapshot_sql(call_ids: list[str]) -> str:
    # call_ids have already passed canonical UUID validation.
    uuid_array = ",".join(f"'{call_id}'::uuid" for call_id in call_ids)
    return (
        "SELECT call_id::text,state,"
        "CASE WHEN reason IN (" + ",".join(f"'{r}'" for r in sorted(SAFE_REASONS)) + ") "
        "THEN reason ELSE '' END,control_transport,state_revision,"
        "to_char(updated_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"') "
        "FROM call_sessions WHERE call_id=ANY(ARRAY[" + uuid_array + "]::uuid[]) "
        "ORDER BY call_id"
    )


def db_command(node: str, sql: str) -> str:
    # All container names and command structure are fixed here. SQL contains
    # only validated UUIDs and fixed enum literals.
    qsql = shlex.quote(sql)
    if node == "a":
        inner = (
            "docker exec --user gsm2sip gsm-rtc-joint "
            "/usr/lib/postgresql/17/bin/psql -X -h /run/postgresql -p 25432 "
            f"-U gsm2sip -d gsm2sip -At -F '|' -c {qsql}"
        )
    elif node == "b":
        inner = (
            "docker exec gsm-rtc-mesh-b-db psql -X -U gsm2sip -d gsm2sip "
            f"-At -F '|' -c {qsql}"
        )
    else:
        inner = (
            "docker exec --user gsm2sip gsm-rtc-mesh-c "
            "/usr/lib/postgresql/17/bin/psql -X -h /run/postgresql -p 55432 "
            f"-U gsm2sip -d gsm2sip -At -F '|' -c {qsql}"
        )
    return "sudo -n " + inner


def parse_db_row(line: str, node: str) -> dict | None:
    fields = line.split("|")
    if len(fields) != 6:
        return None
    call_id, state, reason, control_transport, revision, timestamp = fields
    if not UUID_RE.fullmatch(call_id) or state not in CALL_STATES:
        return None
    if control_transport not in CONTROL_TRANSPORTS or not NUMERIC_RE.fullmatch(revision):
        return None
    if not ISO_TS_RE.fullmatch(timestamp):
        return None
    record: dict[str, object] = {
        "source": "server_db_state",
        "node": node,
        "timestamp": timestamp,
        "call_id": call_id.lower(),
        "event": "db_state_snapshot",
        "state": state,
        "control_transport": control_transport,
        "state_revision": int(revision),
    }
    if reason in SAFE_REASONS:
        record["reason"] = reason
    return record


def read_db_node(args: argparse.Namespace, node: str, call_ids: list[str]) -> tuple[list[dict], int]:
    if not call_ids:
        return [], 0
    sql = psql_snapshot_sql(call_ids)
    records: list[dict] = []
    command = db_command(node, sql)
    # Build an SSH command that cannot interpolate shell metacharacters from
    # outside the validated UUID list. Output is parsed line by line only.
    exit_code = stream_remote(args, command, lambda line: _append_db(line, node, records))
    return records, exit_code


def _append_db(line: str, node: str, records: list[dict]) -> None:
    record = parse_db_row(line, node)
    if record is not None:
        records.append(record)


def write_jsonl(path: pathlib.Path, records: Iterable[dict]) -> tuple[int, str]:
    digest = hashlib.sha256()
    count = 0
    temporary = path.with_suffix(path.suffix + ".tmp")
    with temporary.open("w", encoding="utf-8", newline="\n") as output:
        for record in records:
            data = (json.dumps(record, sort_keys=True, separators=(",", ":")) + "\n").encode()
            output.write(data.decode("utf-8"))
            digest.update(data)
            count += 1
    os.chmod(temporary, 0o600)
    temporary.replace(path)
    return count, digest.hexdigest()


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--remote", default=DEFAULT_REMOTE)
    parser.add_argument("--ssh-key", default=DEFAULT_KEY)
    parser.add_argument("--remote-root", default=DEFAULT_REMOTE_ROOT)
    parser.add_argument(
        "--output", type=pathlib.Path,
        default=pathlib.Path(__file__).resolve().parents[1] / "evidence" / "media-logs",
    )
    args = parser.parse_args()

    args.remote_root = args.remote_root.rstrip("/")
    args.output.mkdir(parents=True, exist_ok=True, mode=0o700)
    try:
        os.chmod(args.output, 0o700)
    except OSError:
        pass

    collected: dict[str, list[dict]] = {"host": [], "gateway": [], "server-a": [], "server-b": [], "server-c": []}
    source_summary: dict[str, dict] = {}

    for role in ("host", "gateway"):
        name = f"{role}-logcat.txt"
        source_summary[role] = collect_source(
            args, name,
            lambda a, n, c: remote_cat(a, n, c),
            lambda line, role=role: parse_device_line(line, role),
            role, collected[role],
        )

    server_containers = {
        "a": ("gsm-rtc-joint",),
        "b": ("gsm-rtc-mesh-b-api", "gsm-rtc-mesh-b-worker"),
        "c": ("gsm-rtc-mesh-c",),
    }
    server_log_records: dict[str, list[dict]] = {node: [] for node in server_containers}
    for node, containers in server_containers.items():
        for container in containers:
            name = f"{node}:{container}"
            status = collect_source(
                args, container,
                lambda a, n, c: remote_docker_logs(a, n.split(":", 1)[-1], c),
                lambda line, node=node: parse_server_line(line, node),
                "server_log", server_log_records[node],
            )
            source_summary[f"server-{node}-{container.rsplit('-', 1)[-1]}"] = status
        collected[f"server-{node}"] = server_log_records[node]

    all_call_ids = sorted({
        record["call_id"]
        for rows in collected.values()
        for record in rows
        if isinstance(record.get("call_id"), str)
    })
    db_fallback: dict[str, dict] = {}
    for node in ("a", "b", "c"):
        if server_log_records[node]:
            db_fallback[node] = {"mode": "server_log", "records": len(server_log_records[node])}
            continue
        if not all_call_ids:
            db_fallback[node] = {
                "mode": "no_call_ids",
                "records": 0,
                "evidence_note": "no call IDs available for a database state snapshot",
            }
            continue
        try:
            rows, exit_code = read_db_node(args, node, all_call_ids)
        except OSError:
            rows, exit_code = [], 255
        collected[f"server-{node}"].extend(rows)
        db_fallback[node] = {
            "mode": "server_db_state" if exit_code == 0 else "unavailable",
            "records": len(rows),
            "remote_exit_code": exit_code,
            "evidence_note": "database state snapshot, not server log",
        }

    sha_summary: dict[str, dict] = {}
    for key, rows in collected.items():
        filename = f"{key}.jsonl"
        count, digest = write_jsonl(args.output / filename, rows)
        sha_summary[filename] = {"records": count, "sha256": digest}

    summary = {
        "schema_version": 1,
        "created_at": dt.datetime.now(dt.timezone.utc).isoformat().replace("+00:00", "Z"),
        "sources": source_summary,
        "server_node_evidence": db_fallback,
        "call_ids_seen": len(all_call_ids),
        "files": sha_summary,
        "safety": {
            "raw_lines_written": False,
            "unknown_fields_emitted": False,
            "database_rows_are_logs": False,
        },
    }
    summary_path = args.output / "summary.json"
    temporary = summary_path.with_suffix(".json.tmp")
    temporary.write_text(json.dumps(summary, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    os.chmod(temporary, 0o600)
    temporary.replace(summary_path)
    print(json.dumps({"output": str(args.output), "files": sha_summary}, sort_keys=True))
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except KeyboardInterrupt:
        raise SystemExit(130)
