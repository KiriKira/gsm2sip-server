#!/usr/bin/env python3
"""Case 5: serial one-component rollback/forward preservation smoke.

Default invocation prints a plan and performs no ADB, Docker, API or database
operations. Run on the joint-0.4 lab host with --execute only after all three
components are at 0.4 and the existing SMS baseline is captured.
"""

import argparse
import contextlib
import datetime
import fcntl
import importlib.util
import json
import os
import pathlib
import re
import subprocess
import sys
import time
import urllib.request
import uuid


ROOT = pathlib.Path("/home/ubuntu/gsm-webrtc-validation/joint-0.4")
PRIVATE = ROOT / "private"
EVIDENCE = ROOT / "evidence"
BASELINE_IDENTITIES = EVIDENCE / "baseline-identities.json"
RECOVERY_IDENTITY_REFERENCE = EVIDENCE / "emulator-retained-sim-recovery.json"
RECOVERY_IDENTITY_REASON = "explicit_same_virtual_modem_confirmation_after_fingerprintless_AVD_reboot"
API_NODES = {
    "A": "http://127.0.0.1:18098",
    "B": "http://10.203.78.2:8080",
    "C": "http://10.203.78.10:8080",
}
EXPECTED_SERIALS = {"gateway": "emulator-5554", "host": "emulator-5556"}
EXPECTED_PACKAGES = {
    "gateway": "com.callagent.gateway",
    "host": "com.callagent.host",
}
VERSION_NAMES = {
    "baseline": "0.3.0-joint-baseline",
    "candidate": ("0.4.0-joint", "0.4.0-joint-round2"),
}
COMPONENT_ORDER = ("server", "gateway", "host")
IDLE_PHASES = (None, "ENDED", "FAILED")
SMS_BASELINE_STATE = PRIVATE / "sms-baseline-state.json"
SMS_PROBE_AUTH = PRIVATE / "sms-probe-client-auth.json"
SMS_PROBE_LOCK = PRIVATE / "sms-probe.lock"
SMS_PROBE_SERVER_STATE = PRIVATE / "sms-probes" / "server-baseline.json"
TEMP_EXPIRED_REFRESH = "2000-01-01T00:00:00.000000Z"


class Case5Error(RuntimeError):
    pass


def load_module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    if spec is None or spec.loader is None:
        raise Case5Error("support_script_unavailable:" + path.name)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def utc_now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds")


def atomic_json(path, value):
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    temp_path = path.with_name("." + path.name + ".tmp")
    fd = os.open(temp_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as stream:
        json.dump(value, stream, sort_keys=True, indent=2)
        stream.write("\n")
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(temp_path, path)
    path.chmod(0o600)


def classify_server_capabilities(capabilities):
    if not isinstance(capabilities, dict):
        return "unknown"
    if capabilities.get("rtc_audio") is not True or capabilities.get("rtc_call_control") is not True:
        return "unknown"
    if all("sip" not in key.lower() for key in capabilities):
        return "candidate"
    if capabilities.get("sip_call_control") is False:
        return "baseline"
    return "unknown"


def safe_call_summary(payload):
    if not isinstance(payload, dict):
        return None
    keys = (
        "case", "status", "call_id", "active_node", "audio_seconds",
        "server_terminal", "gateway_ledger", "actual_telecom_calls",
        "outbound_modem_count", "hangup_origin",
    )
    return {key: payload[key] for key in keys if key in payload}


def safe_sms_summary(payload):
    if not isinstance(payload, dict):
        return None
    keys = ("status", "phase", "label", "evidence", "messages", "client_device_id",
            "gateway_id", "sim_id", "send_performed")
    return {key: payload[key] for key in keys if key in payload}


class Case5:
    def __init__(self, components=COMPONENT_ORDER):
        self.components = tuple(components)
        self.driver = load_module("case5_driver", ROOT / "scripts/driver.py")
        self.core = load_module("case5_core", ROOT / "scripts/core-calls.py")
        if self.driver.SERIALS != EXPECTED_SERIALS:
            raise Case5Error("unexpected_emulator_serials")
        if self.driver.PACKAGES != EXPECTED_PACKAGES:
            raise Case5Error("unexpected_android_packages")
        self.run_id = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
        lock_path = PRIVATE / "case5-single-component-rollback.lock"
        self.lock_stream = lock_path.open("a+")
        os.fchmod(self.lock_stream.fileno(), 0o600)
        try:
            fcntl.flock(self.lock_stream.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as failure:
            raise Case5Error("case5_already_running") from failure
        self.logs = PRIVATE / ("case5-" + self.run_id)
        self.logs.mkdir(parents=True, exist_ok=False, mode=0o700)
        self.logs.chmod(0o700)
        self.report_path = EVIDENCE / ("case5-single-component-rollback-" + self.run_id + ".json")
        self.report = {
            "schema": "gsm04-case5-rollback-v1",
            "case": "single_component_rollback_forward",
            "run_id": self.run_id,
            "status": "RUNNING",
            "started_at": utc_now(),
            "serials": dict(self.driver.SERIALS),
            "component_order": list(COMPONENT_ORDER),
            "selected_components": list(self.components),
            "scope": "full_matrix" if self.components == COMPONENT_ORDER else "selected_components_and_final_forward_smoke",
            "sms_mode": "one_outbound_and_one_inbound_probe_per_rollback_label_then_verify_only",
            "call_duration_seconds": 12,
            "snapshots": [],
            "steps": [],
            "components": [],
        }
        self.step_number = 0
        self.driver_module = self.driver
        self.core_module = self.core
        self.baseline_identities, self.identity_reference = self.load_baseline_identities()
        self.report["identity_reference"] = self.identity_reference
        self.save()

    def save(self):
        self.report["updated_at"] = utc_now()
        atomic_json(self.report_path, self.report)

    def load_baseline_identities(self):
        try:
            value = json.loads(BASELINE_IDENTITIES.read_text(encoding="utf-8"))
            gateway = value["gateway_identity"]
            nodes = value["host_node_keys"]
        except Exception as failure:
            raise Case5Error("baseline_identity_reference_missing_or_invalid") from failure
        if not isinstance(gateway, dict) or not isinstance(nodes, list) or not nodes:
            raise Case5Error("baseline_identity_reference_invalid")
        gateway_identity = {
            key: gateway.get(key)
            for key in ("gateway_id", "sim_id", "mapping_revision")
        }
        if (not isinstance(gateway_identity["gateway_id"], str) or
                not isinstance(gateway_identity["sim_id"], str) or
                type(gateway_identity["mapping_revision"]) is not int or
                gateway_identity["mapping_revision"] < 1):
            raise Case5Error("baseline_gateway_identity_invalid")

        identity_reference = {
            "baseline_file": str(BASELINE_IDENTITIES),
            "baseline_gateway_id": gateway_identity["gateway_id"],
            "baseline_sim_id": gateway_identity["sim_id"],
            "baseline_mapping_revision": gateway_identity["mapping_revision"],
            "mapping_revision_source": "baseline-identities.json",
            "setup_reference": None,
        }
        if RECOVERY_IDENTITY_REFERENCE.exists():
            try:
                recovery = json.loads(RECOVERY_IDENTITY_REFERENCE.read_text(encoding="utf-8"))
                recovered_gateway = recovery["gateway_identity"]
                recovered_revision = recovered_gateway["mapping_revision"]
            except Exception as failure:
                raise Case5Error("recovery_identity_reference_missing_or_invalid") from failure
            if (not isinstance(recovery, dict) or
                    recovery.get("status") != "PASS" or
                    recovery.get("reason") != RECOVERY_IDENTITY_REASON or
                    not isinstance(recovered_gateway, dict) or
                    recovered_gateway.get("status") != "READY" or
                    recovered_gateway.get("gateway_id") != gateway_identity["gateway_id"] or
                    recovered_gateway.get("sim_id") != gateway_identity["sim_id"] or
                    type(recovered_revision) is not int or
                    recovered_revision <= gateway_identity["mapping_revision"]):
                raise Case5Error("recovery_identity_reference_mismatch")
            gateway_identity["mapping_revision"] = recovered_revision
            identity_reference.update({
                "mapping_revision_source": "emulator-retained-sim-recovery.json",
                "setup_reference": {
                    "file": str(RECOVERY_IDENTITY_REFERENCE),
                    "status": recovery["status"],
                    "reason": recovery["reason"],
                    "gateway_id": recovered_gateway["gateway_id"],
                    "sim_id": recovered_gateway["sim_id"],
                    "mapping_revision": recovered_revision,
                },
            })

        return {
            "gateway_identity": gateway_identity,
            "host_node_keys": [
                {key: item.get(key) for key in ("key", "api_url")}
                for item in nodes if isinstance(item, dict)
            ],
        }, identity_reference

    def begin_step(self, name, command):
        self.step_number += 1
        entry = {
            "name": name,
            "command": command,
            "status": "RUNNING",
            "started_at": utc_now(),
        }
        self.report["steps"].append(entry)
        self.save()
        return entry

    def finish_step(self, entry, status, **fields):
        entry.update(fields)
        entry["status"] = status
        entry["finished_at"] = utc_now()
        self.save()

    def child(self, name, script, args, timeout=240):
        command = [script, *args]
        entry = self.begin_step(name, command)
        log_path = self.logs / (str(self.step_number).zfill(2) + "-" + re.sub(r"[^a-zA-Z0-9_.-]", "_", name) + ".log")
        try:
            result = subprocess.run(
                [sys.executable, str(ROOT / "scripts" / script), *args],
                cwd=ROOT,
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                text=True,
                timeout=timeout,
                check=False,
            )
        except subprocess.TimeoutExpired as failure:
            output = failure.stdout or ""
            if isinstance(output, bytes):
                output = output.decode("utf-8", errors="replace")
            log_path.write_text(output, encoding="utf-8")
            log_path.chmod(0o600)
            self.finish_step(entry, "FAIL", log=str(log_path), reason="timeout")
            raise Case5Error(name + "_timeout") from failure
        except Exception as failure:
            self.finish_step(entry, "FAIL", reason=type(failure).__name__)
            raise Case5Error(name + "_failed") from failure

        log_path.write_text(result.stdout, encoding="utf-8")
        log_path.chmod(0o600)
        payload = self.last_json(result.stdout)
        if result.returncode != 0:
            self.finish_step(entry, "FAIL", exit_code=result.returncode, log=str(log_path))
            raise Case5Error(name + "_failed")

        summary = None
        if script in ("sms-baseline.py", "sms-probe.py"):
            summary = safe_sms_summary(payload)
        elif script == "core-calls.py":
            summary = safe_call_summary(payload)
        elif script == "apps.py" and isinstance(payload, dict):
            summary = {
                key: payload[key] for key in
                ("status", "stage", "role", "inplace_install", "fixture_retained", "versions")
                if key in payload
            }
        elif script == "server-version.py" and isinstance(payload, dict):
            summary = {
                "status": payload.get("status"),
                "stage": payload.get("stage"),
                "nodes": [
                    {key: node.get(key) for key in ("node", "asterisk_processes", "compose")}
                    for node in payload.get("nodes", []) if isinstance(node, dict)
                ],
            }
        self.finish_step(entry, "PASS", log=str(log_path), summary=summary)
        return payload

    @staticmethod
    def last_json(output):
        for line in reversed(output.splitlines()):
            try:
                value = json.loads(line)
            except (ValueError, TypeError):
                continue
            if isinstance(value, dict):
                return value
        return None

    def server_versions(self):
        versions = {}
        for node, base_url in API_NODES.items():
            try:
                with urllib.request.urlopen(base_url + "/v1/capabilities", timeout=4) as response:
                    capabilities = json.load(response)
            except Exception as failure:
                raise Case5Error("server_node_unreachable:" + node) from failure
            versions[node] = {
                "stage": classify_server_capabilities(capabilities),
                "protocol_version": capabilities.get("protocol_version"),
                "rtc_audio": capabilities.get("rtc_audio"),
                "rtc_call_control": capabilities.get("rtc_call_control"),
                "sip_call_control": capabilities.get("sip_call_control"),
            }
        stages = {node["stage"] for node in versions.values()}
        if len(stages) != 1 or "unknown" in stages:
            raise Case5Error("server_versions_mixed_or_unrecognized")
        return next(iter(stages)), versions

    def app_version(self, role):
        serial = self.driver.SERIALS[role]
        package = self.driver.PACKAGES[role]
        self.driver.ADB[-1] = serial
        output = self.driver.adb("shell", "dumpsys", "package", package)
        codes = re.findall(r"^\s*versionCode=(\d+)", output, re.MULTILINE)
        names = re.findall(r"^\s*versionName=([^\s]+)", output, re.MULTILINE)
        if "1000000000" not in codes:
            stage = "unknown"
        else:
            stage = next(
                (candidate for candidate, expected in VERSION_NAMES.items()
                 if any(name in names for name in (expected if isinstance(expected, tuple) else (expected,)))),
                "unknown",
            )
        return {"stage": stage, "version_codes": sorted(set(codes)), "version_names": sorted(set(names))}

    def version_matrix(self):
        server_stage, server_nodes = self.server_versions()
        apps = {role: self.app_version(role) for role in ("gateway", "host")}
        return {
            "server": server_stage,
            "server_nodes": server_nodes,
            "gateway": apps["gateway"]["stage"],
            "gateway_version": apps["gateway"],
            "host": apps["host"]["stage"],
            "host_version": apps["host"],
        }

    def assert_matrix(self, expected, label):
        matrix = self.version_matrix()
        actual = {key: matrix[key] for key in ("server", "gateway", "host")}
        if actual != expected:
            raise Case5Error(label + "_version_matrix_mismatch")
        return matrix

    def runtime_identity(self, require_idle=True):
        gateway_ready = self.driver.read("gateway", "joint-ready.json")
        host_ready = self.driver.read("host", "joint-ready.json")
        if not gateway_ready or gateway_ready.get("status") != "READY":
            raise Case5Error("gateway_instrumentation_not_ready")
        if not host_ready or host_ready.get("status") != "READY":
            raise Case5Error("host_instrumentation_not_ready")

        gateway_identity = {
            key: gateway_ready.get(key)
            for key in ("gateway_id", "sim_id", "mapping_revision")
        }
        if gateway_identity != self.baseline_identities["gateway_identity"]:
            raise Case5Error("gateway_pairing_or_sim_identity_changed")

        host = self.core.cmd("host", "status")
        gateway = self.core.cmd("gateway", "status")
        node_health = host.get("node_health")
        if not isinstance(node_health, list):
            raise Case5Error("host_node_health_missing")
        host_node_keys = [
            {key: node.get(key) for key in ("key", "api_url")}
            for node in node_health if isinstance(node, dict)
        ]
        if host_node_keys != self.baseline_identities["host_node_keys"]:
            raise Case5Error("host_pairing_or_server_nodes_changed")
        active_node = host.get("active_node")
        if active_node not in {item["api_url"] for item in host_node_keys}:
            raise Case5Error("host_active_node_not_in_baseline_mesh")
        if host.get("listening") is not True:
            raise Case5Error("host_call_control_not_ready")
        if require_idle and (host.get("phase") not in IDLE_PHASES or gateway.get("bridge") != "IDLE"):
            raise Case5Error("active_call_prevents_component_switch")

        return {
            "gateway_identity": gateway_identity,
            "host_node_keys": host_node_keys,
            "active_node": active_node,
            "host_listening": host.get("listening"),
            "host_phase": host.get("phase"),
            "gateway_bridge": gateway.get("bridge"),
        }

    def active_slot_counts(self):
        counts = {}
        for node in ("a", "b", "c"):
            try:
                value = self.core.sql("SELECT count(*) FROM gateway_call_slots", node=node)
                counts[node.upper()] = int(value)
            except Exception as failure:
                raise Case5Error("cannot_confirm_idle_slots:" + node.upper()) from failure
            if counts[node.upper()] != 0:
                raise Case5Error("active_server_call_slot:" + node.upper())
        return counts

    def require_idle(self):
        runtime = self.runtime_identity(require_idle=True)
        slots = self.active_slot_counts()
        return {"runtime": runtime, "active_call_slots": slots}

    def snapshot(self, label, expected):
        matrix = self.assert_matrix(expected, label)
        idle = self.require_idle()
        value = {
            "label": label,
            "recorded_at": utc_now(),
            "component_stages": {key: matrix[key] for key in ("server", "gateway", "host")},
            "versions": {
                "server_nodes": matrix["server_nodes"],
                "gateway": matrix["gateway_version"],
                "host": matrix["host_version"],
            },
            "identity": idle["runtime"],
            "active_call_slots": idle["active_call_slots"],
        }
        self.report["snapshots"].append(value)
        self.save()
        return value

    def verify_sms(self, label):
        payload = self.child(label, "sms-baseline.py", ["verify"], timeout=180)
        if not isinstance(payload, dict) or payload.get("status") != "PASS":
            raise Case5Error(label + "_failed")
        summary = safe_sms_summary(payload)
        if not summary or summary.get("phase") != "verified":
            raise Case5Error(label + "_result_invalid")
        return summary

    def probe_sms(self, label):
        if label not in ("server-baseline", "gateway-baseline", "host-baseline"):
            raise Case5Error("sms_probe_label_invalid")
        payload = self.child("sms-probe-" + label, "sms-probe.py", ["--label", label], timeout=420)
        if not isinstance(payload, dict) or payload.get("status") != "PASS":
            raise Case5Error("sms_probe_" + label + "_failed")
        summary = safe_sms_summary(payload)
        if (not summary or summary.get("phase") not in ("captured", "verified") or
                summary.get("label") != label or not isinstance(summary.get("messages"), dict)):
            raise Case5Error("sms_probe_" + label + "_result_invalid")
        messages = summary["messages"]
        if set(messages) != {"outbound", "inbound"}:
            raise Case5Error("sms_probe_" + label + "_message_set_invalid")
        for direction in ("outbound", "inbound"):
            message = messages[direction]
            if (not isinstance(message, dict) or
                    not re.fullmatch(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}",
                                     str(message.get("message_id", "")), re.IGNORECASE) or
                    not re.fullmatch(r"[0-9a-f]{64}", str(message.get("sha256", ""))) or
                    not isinstance(message.get("gateway_archive_count"), int) or
                    message["gateway_archive_count"] < 1 or
                    not isinstance(message.get("host_history_count"), int) or
                    message["host_history_count"] < 1):
                raise Case5Error("sms_probe_" + label + "_retention_invalid")
        if (messages["outbound"].get("server_status") not in ("submitted", "delivered") or
                messages["inbound"].get("server_status") != "received"):
            raise Case5Error("sms_probe_" + label + "_server_status_invalid")
        return summary

    def _private_sms_client_id(self):
        values = []
        for path, schema in (
            (SMS_BASELINE_STATE, "gsm04-sms-baseline-v1"),
            (SMS_PROBE_AUTH, "gsm04-sms-probe-auth-v1"),
            (SMS_PROBE_SERVER_STATE, "gsm04-sms-probe-v1"),
        ):
            if path.is_symlink() or not path.is_file() or path.stat().st_mode & 0o077:
                raise Case5Error("sms_probe_private_identity_unavailable")
            try:
                state = json.loads(path.read_text(encoding="utf-8"))
            except Exception as failure:
                raise Case5Error("sms_probe_private_identity_invalid") from failure
            if not isinstance(state, dict) or state.get("schema") != schema:
                raise Case5Error("sms_probe_private_identity_invalid")
            if path == SMS_PROBE_SERVER_STATE and (
                    state.get("label") != "server-baseline" or
                    state.get("phase") not in ("captured", "verified")):
                raise Case5Error("sms_probe_server_label_invalid")
            values.append(state.get("client_device_id"))
        if (not all(isinstance(value, str) for value in values) or len(set(values)) != 1 or
                not re.fullmatch(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}", values[0], re.I)):
            raise Case5Error("sms_probe_client_identity_mismatch")
        return values[0].lower()

    @staticmethod
    def _a_sql(query):
        command = [
            "sudo", "-n", "docker", "exec", "--user", "gsm2sip", "gsm-rtc-joint",
            "/usr/lib/postgresql/17/bin/psql", "-X", "-h", "/run/postgresql",
            "-p", "25432", "-U", "gsm2sip", "-d", "gsm2sip", "-q", "-v",
            "ON_ERROR_STOP=1", "-A", "-t", "-F", "\t", "-c", query,
        ]
        try:
            result = subprocess.run(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                    text=True, timeout=15, check=False)
        except Exception as failure:
            raise Case5Error("server_baseline_session_query_failed") from failure
        if result.returncode != 0:
            raise Case5Error("server_baseline_session_query_failed")
        return [line for line in result.stdout.splitlines() if line]

    @contextlib.contextmanager
    def _exclude_sms_only_client_for_server_incoming(self):
        if SMS_PROBE_LOCK.is_symlink() or not SMS_PROBE_LOCK.is_file():
            raise Case5Error("sms_probe_lock_unavailable")
        lock_stream = SMS_PROBE_LOCK.open("r+")
        try:
            fcntl.flock(lock_stream.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as failure:
            lock_stream.close()
            raise Case5Error("sms_probe_in_progress") from failure
        session_id = owner_id = original_expiry = None
        mutation_attempted = False
        ledger = report_record = None
        private_ledger = None
        try:
            client_id = self._private_sms_client_id()
            gateway_id = self.baseline_identities["gateway_identity"]["gateway_id"].lower()
            session_rows = self._a_sql(
                f"""SELECT s.id::text,s.device_id::text,
                to_char(s.refresh_expires_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
                d.owner_id::text,d.role,d.state,COALESCE(c.enabled,false)::text,
                (s.refresh_expires_at>clock_timestamp())::text
                FROM sessions s JOIN devices d ON d.id=s.device_id
                LEFT JOIN rtc_control_capabilities c ON c.device_id=d.id
                WHERE s.device_id='{client_id}' ORDER BY s.id"""
            )
            if len(session_rows) != 1 or len(session_rows[0].split("\t")) != 8:
                raise Case5Error("sms_probe_session_not_unique")
            fields = session_rows[0].split("\t")
            if fields[1] != client_id or fields[4:8] != ["client", "active", "false", "true"]:
                raise Case5Error("sms_probe_session_precondition_failed")
            session_id, owner_id, original_expiry = fields[0].lower(), fields[3].lower(), fields[2]
            protected = self._a_sql(
                f"""SELECT s.id::text||'|'||s.device_id::text||'|'||
                to_char(s.access_expires_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"')||'|'||
                to_char(s.refresh_expires_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"')
                FROM sessions s JOIN devices d ON d.id=s.device_id
                WHERE d.owner_id='{owner_id}' AND d.id<>'{client_id}' ORDER BY s.id"""
            )
            counts = self._a_sql(
                f"""SELECT count(DISTINCT d.id) FILTER (WHERE d.id='{gateway_id}' AND d.role='gateway'
                AND d.state='active' AND s.refresh_expires_at>clock_timestamp()),
                count(DISTINCT d.id) FILTER (WHERE d.role='client' AND d.state='active'
                AND d.id<>'{client_id}' AND s.refresh_expires_at>clock_timestamp())
                FROM devices d JOIN sessions s ON s.device_id=d.id WHERE d.owner_id='{owner_id}'"""
            )
            if len(counts) != 1 or counts[0].split("\t") != ["1", "1"]:
                raise Case5Error("host_or_gateway_session_precondition_failed")

            private_ledger = PRIVATE / ("server-baseline-incoming-session-isolation-" + self.run_id + ".json")
            if private_ledger.exists() or private_ledger.is_symlink():
                raise Case5Error("session_isolation_ledger_already_exists")
            ledger = {
                "schema": "gsm04-server-baseline-incoming-session-isolation-v1",
                "run_id": self.run_id,
                "client_device_id": client_id,
                "session_id": session_id,
                "original_refresh_expires_at": original_expiry,
                "temporary_refresh_expires_at": TEMP_EXPIRED_REFRESH,
                "rtc_capability_enabled": False,
                "protected_owner_session_count": len(protected),
                "status": "PREPARED",
            }
            atomic_json(private_ledger, ledger)
            report_record = {
                "affected_client_count": 0,
                "affected_session_count": 0,
                "restore_status": "PENDING",
            }
            component = next((item for item in reversed(self.report["components"])
                              if item.get("component") == "server"), None)
            if component is None:
                raise Case5Error("server_rollback_record_missing")
            component["incoming_session_isolation"] = report_record
            self.save()
            mutation_attempted = True
            try:
                update = self._a_sql(
                    f"""UPDATE sessions s SET refresh_expires_at='{TEMP_EXPIRED_REFRESH}'::timestamptz
                    FROM devices d WHERE d.id=s.device_id AND s.id='{session_id}'::uuid
                    AND s.device_id='{client_id}'::uuid AND d.role='client' AND d.state='active'
                    AND s.refresh_expires_at='{original_expiry}'::timestamptz
                    AND NOT EXISTS(SELECT 1 FROM rtc_control_capabilities c WHERE c.device_id=d.id AND c.enabled)
                    RETURNING s.id::text||'|'||s.device_id::text||'|'||
                    to_char(s.refresh_expires_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"')"""
                )
                if len(update) != 1 or update[0].split("|") != [session_id, client_id, TEMP_EXPIRED_REFRESH]:
                    raise Case5Error("sms_probe_session_exclusion_failed")
                report_record.update(affected_client_count=1, affected_session_count=1)
                ledger["status"] = "EXCLUDED"
                atomic_json(private_ledger, ledger)
                self.save()
                yield
            finally:
                if mutation_attempted and ledger is not None:
                    try:
                        ledger["status"] = "RESTORING"
                        atomic_json(private_ledger, ledger)
                        current = self._a_sql(f"""SELECT to_char(refresh_expires_at AT TIME ZONE 'UTC',
                            'YYYY-MM-DD"T"HH24:MI:SS.US"Z"') FROM sessions
                            WHERE id='{session_id}' AND device_id='{client_id}'""")
                        if current == [TEMP_EXPIRED_REFRESH]:
                            restored = self._a_sql(f"""UPDATE sessions SET refresh_expires_at='{original_expiry}'::timestamptz
                                WHERE id='{session_id}'::uuid AND device_id='{client_id}'::uuid
                                AND refresh_expires_at='{TEMP_EXPIRED_REFRESH}'::timestamptz
                                RETURNING to_char(refresh_expires_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"')""")
                            if restored != [original_expiry]:
                                raise Case5Error("sms_probe_session_restore_failed")
                        elif current != [original_expiry]:
                            raise Case5Error("sms_probe_session_restore_conflict")
                        verified = self._a_sql(
                            f"""SELECT s.id::text,s.device_id::text,
                            to_char(s.refresh_expires_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
                            d.role,d.state,COALESCE(c.enabled,false)::text FROM sessions s
                            JOIN devices d ON d.id=s.device_id LEFT JOIN rtc_control_capabilities c ON c.device_id=d.id
                            WHERE s.device_id='{client_id}' ORDER BY s.id"""
                        )
                        after = self._a_sql(f"""SELECT s.id::text||'|'||s.device_id::text||'|'||
                            to_char(s.access_expires_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"')||'|'||
                            to_char(s.refresh_expires_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"')
                            FROM sessions s JOIN devices d ON d.id=s.device_id
                            WHERE d.owner_id='{owner_id}' AND d.id<>'{client_id}' ORDER BY s.id""")
                        if (len(verified) != 1 or verified[0].split("\t") !=
                                [session_id, client_id, original_expiry, "client", "active", "false"] or
                                after != protected):
                            raise Case5Error("host_or_gateway_session_restore_verification_failed")
                        ledger["status"] = "RESTORED"
                        ledger["restored_at"] = utc_now()
                        ledger["protected_owner_sessions_unchanged"] = True
                        atomic_json(private_ledger, ledger)
                        report_record["restore_status"] = "PASS"
                        report_record["protected_host_gateway_sessions_unchanged"] = True
                        self.save()
                    except Exception as failure:
                        ledger["status"] = "RESTORE_FAILED"
                        atomic_json(private_ledger, ledger)
                        if report_record is not None:
                            report_record["restore_status"] = "FAIL"
                            self.save()
                        raise Case5Error("server_baseline_aux_session_restore_failed") from failure
        finally:
            try:
                fcntl.flock(lock_stream.fileno(), fcntl.LOCK_UN)
            finally:
                lock_stream.close()

    def quick_call(self, component, direction, stage):
        mode = "quick-" + direction + "-case5-" + component + "-" + stage + "-" + self.run_id
        if component == "server" and direction == "incoming" and stage == "rollback":
            with self._exclude_sms_only_client_for_server_incoming():
                payload = self.child(
                    "call-" + component + "-" + stage + "-" + direction,
                    "core-calls.py",
                    [mode],
                    timeout=360,
                )
        else:
            payload = self.child(
                "call-" + component + "-" + stage + "-" + direction,
                "core-calls.py",
                [mode],
                timeout=360,
            )
        summary = safe_call_summary(payload)
        if not summary or summary.get("status") != "PASS":
            raise Case5Error("call_" + direction + "_result_invalid")
        if summary.get("active_node") not in {item["api_url"] for item in self.baseline_identities["host_node_keys"]}:
            raise Case5Error("call_used_unknown_server_node")
        audio_seconds = float(summary.get("audio_seconds", 0))
        if audio_seconds < 12.0 or audio_seconds > 20.0:
            raise Case5Error("call_audio_window_not_12_seconds")
        if summary.get("actual_telecom_calls") != 1:
            raise Case5Error("actual_telecom_call_count_invalid")
        terminal = str(summary.get("server_terminal", ""))
        if not terminal.startswith("ended|") or not terminal.endswith("|rtc|f"):
            raise Case5Error("server_call_terminal_or_slot_invalid")
        ledger = summary.get("gateway_ledger")
        if not isinstance(ledger, dict) or ledger.get("occupied") != 0 or len(ledger.get("rows", [])) != 1:
            raise Case5Error("gateway_call_ledger_invalid")
        return summary

    def component_tool(self, component, stage):
        if component == "server":
            return self.child("server-" + stage, "server-version.py", [stage], timeout=600)
        return self.child("app-" + component + "-" + stage, "apps.py", ["install", stage, component], timeout=420)

    def start_app(self, component):
        self.child("start-" + component, "apps.py", ["start", component], timeout=240)

    def change_component(self, component, stage):
        self.component_tool(component, stage)
        if component in ("gateway", "host"):
            self.start_app(component)

    def expected_matrix(self, target=None, target_stage=None):
        matrix = {"server": "candidate", "gateway": "candidate", "host": "candidate"}
        if target is not None:
            matrix[target] = target_stage
        return matrix

    def recover_idle(self):
        attempts = []
        for role in ("host", "gateway"):
            try:
                self.core.cmd(role, "hangup")
                attempts.append(role + "_hangup_sent")
            except Exception:
                attempts.append(role + "_hangup_unavailable")
        try:
            self.driver.ADB[-1] = self.driver.SERIALS["gateway"]
            self.driver.adb("emu", "gsm", "cancel", "+15550100200")
            attempts.append("fixed_emulator_call_cancelled")
        except Exception:
            attempts.append("fixed_emulator_cancel_unavailable")
        self.report.setdefault("recovery_attempts", []).append({"at": utc_now(), "actions": attempts})
        self.save()
        deadline = time.monotonic() + 45
        last_failure = None
        while time.monotonic() < deadline:
            try:
                idle = self.require_idle()
                self.report.setdefault("recovery_attempts", [])[-1]["idle_confirmed"] = True
                self.save()
                return idle
            except Exception as failure:
                last_failure = failure
                time.sleep(2)
        self.report.setdefault("recovery_attempts", [])[-1]["idle_confirmed"] = False
        self.report.setdefault("recovery_attempts", [])[-1]["reason"] = type(last_failure).__name__ if last_failure else "unknown"
        self.save()
        raise Case5Error("call_not_idle; refusing_component_forward") from last_failure

    def test_rollback(self, component):
        entry = {
            "component": component,
            "rollback_status": "RUNNING",
            "started_at": utc_now(),
            "calls": [],
        }
        self.report["components"].append(entry)
        self.save()
        mutation_started = False
        calls_started = False
        idle_proven_after_calls = False
        failure = None
        try:
            self.snapshot("before-" + component + "-rollback", self.expected_matrix())
            self.require_idle()
            mutation_started = True
            self.change_component(component, "baseline")
            self.snapshot(
                "rollback-" + component,
                self.expected_matrix(component, "baseline"),
            )
            entry["sms_before_calls"] = self.verify_sms("sms-" + component + "-rollback-before-calls")
            entry["sms_probe_label"] = component + "-baseline"
            entry["sms_probe_before_calls"] = self.probe_sms(entry["sms_probe_label"])
            calls_started = True
            entry["calls"].append(self.quick_call(component, "outgoing", "rollback"))
            entry["calls"].append(self.quick_call(component, "incoming", "rollback"))
            self.require_idle()
            idle_proven_after_calls = True
            entry["sms_after_calls"] = self.verify_sms("sms-" + component + "-rollback-after-calls")
            entry["sms_probe_after_calls"] = self.probe_sms(entry["sms_probe_label"])
            entry["rollback_status"] = "PASS"
            entry["rollback_finished_at"] = utc_now()
            self.save()
        except Exception as error:
            failure = error
            entry["rollback_status"] = "FAIL"
            entry["rollback_error"] = type(error).__name__ + ":" + str(error)
            entry["rollback_finished_at"] = utc_now()
            self.save()
        finally:
            if mutation_started:
                try:
                    if calls_started and not idle_proven_after_calls:
                        self.recover_idle()
                    self.change_component(component, "candidate")
                    self.snapshot("forward-" + component, self.expected_matrix())
                    entry["forward_status"] = "PASS"
                    entry["sms_after_forward"] = self.verify_sms("sms-" + component + "-forward")
                    if entry.get("sms_probe_label"):
                        entry["sms_probe_after_forward"] = self.probe_sms(entry["sms_probe_label"])
                    entry["forward_finished_at"] = utc_now()
                    self.save()
                except Exception as recovery_error:
                    entry["forward_status"] = "FAIL"
                    entry["forward_error"] = type(recovery_error).__name__ + ":" + str(recovery_error)
                    entry["forward_finished_at"] = utc_now()
                    self.save()
                    raise Case5Error("component_forward_failed:" + component) from recovery_error
        if failure is not None:
            raise Case5Error("rollback_checks_failed:" + component) from failure

    def run(self):
        if not ROOT.is_dir() or not PRIVATE.is_dir() or not EVIDENCE.is_dir():
            raise Case5Error("joint_lab_paths_unavailable")
        required_files = [
            ROOT / "scripts/apps.py",
            ROOT / "scripts/server-version.py",
            ROOT / "scripts/core-calls.py",
            ROOT / "scripts/sms-baseline.py",
            ROOT / "scripts/sms-probe.py",
        ]
        for role in ("gateway", "host"):
            for stage in ("baseline", "candidate"):
                for name in (role + "-debug.apk", role + "-debug-androidTest.apk"):
                    required_files.append(ROOT / "builds" / stage / role / name)
        if any(not path.is_file() for path in required_files):
            raise Case5Error("required_rollback_artifacts_missing")
        initial = self.snapshot("initial-all-candidate", self.expected_matrix())
        self.report["initial_snapshot"] = initial["label"]
        self.report["initial_sms"] = self.verify_sms("sms-initial-all-candidate")
        self.save()

        for component in self.components:
            self.test_rollback(component)

        final = self.snapshot("final-all-candidate", self.expected_matrix())
        self.report["final_snapshot"] = final["label"]
        self.report["final_sms_before_calls"] = self.verify_sms("sms-final-all-candidate-before-calls")
        self.report["final_sms_probes_before_calls"] = {
            label: self.probe_sms(label) for label in ("server-baseline", "gateway-baseline", "host-baseline")
        }
        final_calls_started = False
        final_idle_proven = False
        try:
            self.report["final_calls"] = []
            final_calls_started = True
            self.report["final_calls"].append(self.quick_call("all-0.4", "outgoing", "final"))
            self.report["final_calls"].append(self.quick_call("all-0.4", "incoming", "final"))
            self.require_idle()
            final_idle_proven = True
        finally:
            if final_calls_started and not final_idle_proven:
                self.recover_idle()
        self.report["final_sms_after_calls"] = self.verify_sms("sms-final-all-candidate-after-calls")
        self.report["final_sms_probes_after_calls"] = {
            label: self.probe_sms(label) for label in ("server-baseline", "gateway-baseline", "host-baseline")
        }
        self.report["status"] = "PASS"
        self.report["finished_at"] = utc_now()
        self.save()
        return self.report


def plan():
    return {
        "status": "PLAN_ONLY",
        "writes_remote_state": False,
        "order": [
            "require all-candidate versions, captured SMS baseline, baseline pairing/SIM/node identity, and idle calls/slots",
            "server A/B/C rollback -> verify saved SMS + first server-baseline outbound/inbound probe -> outgoing and incoming 12s calls -> verify both SMS ledgers -> forward all server nodes",
            "Gateway-only in-place rollback -> verify saved SMS + first gateway-baseline outbound/inbound probe -> same calls and retention checks -> forward Gateway",
            "Host-only in-place rollback -> verify saved SMS + first host-baseline outbound/inbound probe -> same calls and retention checks -> forward Host",
            "all 0.4 final saved-SMS and all three probe-label verification -> outgoing and incoming 12s calls -> verify all SMS again",
        ],
        "identity_reference": "gateway_id, sim_id and Host node keys remain pinned to baseline-identities.json; mapping_revision uses the exact passing retained-SIM recovery proof when present and stays fixed across every rollback and forward step",
        "sms": "each rollback label sends exactly one outbound SMS to and one synthetic inbound SMS from the fixed virtual number on first use; sms-probe.py persists intent before sending and every existing label is GET/SQLite verification only on rerun, forward, and final checks",
        "calls": "reuses core-calls.py quick-outgoing/quick-incoming modes; each asserts a 12s WebRTC/Telecom call, server terminal state, gateway ledger and idle slot",
        "rollback": "apps.py install uses adb install -r -g and fixture reuse; server-version.py retains volumes/DB/keys/routing",
        "recovery": "after a component rollback begins, failures attempt to restore only that component to candidate; a call that cannot be proven idle blocks forwarding and the next component",
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--execute", action="store_true", help="run the fixed case 5 sequence; default is plan-only")
    parser.add_argument("--components", nargs="+", choices=COMPONENT_ORDER, default=list(COMPONENT_ORDER),
                        help="explicit affected/remaining component rerun; report records subset scope")
    args = parser.parse_args()
    if len(set(args.components)) != len(args.components):
        parser.error("duplicate component")
    components = tuple(c for c in COMPONENT_ORDER if c in args.components)
    if not args.execute:
        print(json.dumps(plan(), sort_keys=True))
        return
    os.umask(0o077)
    workflow = None
    try:
        workflow = Case5(components)
        report = workflow.run()
        print(json.dumps({
            "status": report["status"],
            "report": str(workflow.report_path),
            "run_id": workflow.run_id,
            "components": [item["component"] for item in report["components"]],
            "final_stage": "candidate",
        }, sort_keys=True), flush=True)
    except Exception as failure:
        if workflow is not None:
            workflow.report["status"] = "FAIL"
            workflow.report["finished_at"] = utc_now()
            workflow.report["failure"] = type(failure).__name__ + ":" + str(failure)
            workflow.save()
            print(json.dumps({
                "status": "FAIL",
                "report": str(workflow.report_path),
                "run_id": workflow.run_id,
                "reason": workflow.report["failure"],
            }, sort_keys=True), flush=True)
        else:
            print(json.dumps({"status": "FAIL", "reason": type(failure).__name__}, sort_keys=True), flush=True)
        raise SystemExit(1)


if __name__ == "__main__":
    main()
