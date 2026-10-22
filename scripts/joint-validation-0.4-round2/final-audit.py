#!/usr/bin/env python3
"""Flush and audit the final negative-test evidence for the joint lab.

Run on the lab host after functional testing is complete. This script only
signals the exact tcpdump children recorded by private/capture-pids.json; all
other checks are read-only. It never records raw process arguments, packet
lines, Docker process names, environment variables, or log contents.
"""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
import pathlib
import re
import shlex
import subprocess
import time


ROOT = pathlib.Path("/home/ubuntu/gsm-webrtc-validation/joint-0.4")
PRIVATE = ROOT / "private"
EVIDENCE = ROOT / "evidence"
CAPTURE_PIDS = PRIVATE / "capture-pids.json"
PORTS = (5060, 5061, 8088)
EXPECTED_NODES = {"a": "gmr-a", "b": "gmr-b", "c": "gmr-c"}
EXPECTED_FILTER = ["port", "5060", "or", "port", "5061", "or", "port", "8088"]
EXPECTED_PCAP = {
    node: PRIVATE / f"no-sip-{node}.pcap"
    for node in EXPECTED_NODES
}

# Container names are fixed from the provisioned A/B/C topology. Only
# process counts and the Asterisk match count are emitted from `docker top`.
DOCKER_COMPONENTS = (
    ("a", "aio", "gsm-rtc-joint"),
    ("a", "turn", "gsm-rtc-joint-turn"),
    ("b", "api", "gsm-rtc-mesh-b-api"),
    ("b", "worker", "gsm-rtc-mesh-b-worker"),
    ("b", "postgres", "gsm-rtc-mesh-b-db"),
    ("b", "turn", "gsm04turn-turn-1"),
    ("c", "aio", "gsm-rtc-mesh-c"),
)


def run(argv: list[str], timeout: float = 15.0) -> subprocess.CompletedProcess | None:
    try:
        return subprocess.run(
            argv,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            check=False,
            timeout=timeout,
        )
    except (OSError, subprocess.TimeoutExpired):
        return None


def decode(data: bytes | None) -> str:
    return (data or b"").decode("utf-8", errors="replace")


def load_capture_manifest() -> tuple[list[dict], bool]:
    try:
        value = json.loads(CAPTURE_PIDS.read_text(encoding="utf-8"))
    except (OSError, ValueError, TypeError):
        return [], False
    if not isinstance(value, list):
        return [], False

    entries: list[dict] = []
    seen_nodes: set[str] = set()
    valid = len(value) == 3
    for item in value:
        if not isinstance(item, dict):
            valid = False
            continue
        node = item.get("node")
        interface = item.get("interface")
        pid = item.get("pid")
        if (
            not isinstance(node, str)
            or node not in EXPECTED_NODES
            or interface != EXPECTED_NODES.get(node)
            or not isinstance(pid, int)
            or isinstance(pid, bool)
            or pid <= 1
            or node in seen_nodes
        ):
            valid = False
            continue
        seen_nodes.add(node)
        entries.append({"node": node, "interface": interface, "wrapper_pid": pid})
    if seen_nodes != set(EXPECTED_NODES):
        valid = False
    return entries, valid


def process_table() -> dict[int, dict[str, object]] | None:
    result = run(["ps", "-eo", "pid=,ppid=,state=,comm="], timeout=8.0)
    if result is None or result.returncode != 0:
        return None
    table: dict[int, dict[str, object]] = {}
    for line in decode(result.stdout).splitlines():
        fields = line.strip().split(None, 3)
        if len(fields) != 4:
            continue
        try:
            pid, ppid = int(fields[0]), int(fields[1])
        except ValueError:
            continue
        table[pid] = {"ppid": ppid, "state": fields[2], "comm": fields[3]}
    return table


def descendants(root_pid: int, table: dict[int, dict[str, object]]) -> set[int]:
    found: set[int] = set()
    pending = [root_pid]
    while pending:
        parent = pending.pop()
        children = [pid for pid, row in table.items() if row["ppid"] == parent and pid not in found]
        found.update(children)
        pending.extend(children)
    return found


def process_start_ticks(pid: int) -> str | None:
    try:
        stat = pathlib.Path(f"/proc/{pid}/stat").read_text(encoding="ascii")
        fields_after_comm = stat[stat.rfind(")") + 2:].split()
        # /proc/<pid>/stat field 22 is starttime; after comm, it is index 19.
        return fields_after_comm[19]
    except (OSError, IndexError):
        return None


def process_args(pid: int) -> list[str] | None:
    result = run(["ps", "-p", str(pid), "-o", "args="], timeout=5.0)
    if result is None or result.returncode != 0:
        return None
    try:
        return shlex.split(decode(result.stdout).strip())
    except ValueError:
        return None


def flag_value(args: list[str] | None, flag: str) -> str | None:
    if not args:
        return None
    for index, item in enumerate(args[:-1]):
        if item == flag:
            return args[index + 1]
        if item.startswith(flag) and len(item) > len(flag):
            return item[len(flag):]
    return None


def has_pcap_open(pid: int, expected: pathlib.Path) -> bool:
    try:
        expected_real = os.path.realpath(expected)
        for entry in os.scandir(f"/proc/{pid}/fd"):
            try:
                if os.path.realpath(f"/proc/{pid}/fd/{entry.name}") == expected_real:
                    return True
            except OSError:
                continue
    except OSError:
        return False
    return False


def inspect_capture(entry: dict, table: dict[int, dict[str, object]] | None) -> dict:
    node = entry["node"]
    interface = EXPECTED_NODES[node]
    wrapper_pid = entry["wrapper_pid"]
    wrapper = table.get(wrapper_pid) if table is not None else None
    child_ids = descendants(wrapper_pid, table or {}) if wrapper else set()
    tcpdump_ids = sorted(
        pid for pid in child_ids
        if table is not None and str(table[pid]["comm"]).lower() == "tcpdump"
    )
    actual_pid = tcpdump_ids[0] if len(tcpdump_ids) == 1 else None
    args = process_args(actual_pid) if actual_pid is not None else None
    expected_pcap = EXPECTED_PCAP[node]

    exe_match = bool(args and pathlib.Path(args[0]).name == "tcpdump")
    actual_iface = flag_value(args, "-i")
    actual_pcap = flag_value(args, "-w")
    try:
        filter_index = args.index("-w") + 2 if args and "-w" in args else -1
        filter_args = args[filter_index:] if filter_index >= 0 else []
    except (ValueError, TypeError):
        filter_args = []

    pcap_matches = actual_pcap == str(expected_pcap)
    open_matches = bool(actual_pid is not None and has_pcap_open(actual_pid, expected_pcap))
    result = {
        "node": node,
        "interface": interface,
        "wrapper_pid": wrapper_pid,
        "wrapper_is_sudo": bool(wrapper and str(wrapper["comm"]).lower() == "sudo"),
        "tcpdump_pid": actual_pid,
        "tcpdump_child_count": len(tcpdump_ids),
        "tcpdump_executable": "tcpdump" if exe_match else "missing_or_unrecognized",
        "interface_matches": actual_iface == interface,
        "unbuffered": bool(args and "-U" in args),
        "filter_matches_required_ports": filter_args == EXPECTED_FILTER,
        "pcap_target_matches": pcap_matches,
        "pcap_open_by_tcpdump": open_matches,
        "signal_sent": False,
        "process_exited": False,
        "wrapper_exited": False,
        "packet_counts": {str(port): None for port in PORTS},
        "pcap_read_ok": False,
        "pcap_bytes": None,
        "pcap_sha256": None,
    }
    result["safe_to_signal"] = bool(
        wrapper
        and result["wrapper_is_sudo"]
        and actual_pid is not None
        and exe_match
        and pcap_matches
        and open_matches
    )
    result["_tcpdump_start_ticks"] = process_start_ticks(actual_pid) if actual_pid else None
    return result


def still_same_capture(capture: dict) -> bool:
    pid = capture["tcpdump_pid"]
    if (
        not capture["safe_to_signal"]
        or pid is None
        or capture["_tcpdump_start_ticks"] is None
    ):
        return False
    table = process_table()
    if table is None:
        return False
    wrapper_pid = capture["wrapper_pid"]
    wrapper = table.get(wrapper_pid)
    child = table.get(pid)
    if (
        not wrapper
        or str(wrapper["comm"]).lower() != "sudo"
        or not child
        or str(child["comm"]).lower() != "tcpdump"
        or pid not in descendants(wrapper_pid, table)
        or process_start_ticks(pid) != capture["_tcpdump_start_ticks"]
    ):
        return False
    args = process_args(pid)
    return bool(
        args
        and pathlib.Path(args[0]).name == "tcpdump"
        and flag_value(args, "-i") == capture["interface"]
        and flag_value(args, "-w") == str(EXPECTED_PCAP[capture["node"]])
        and "-U" in args
    )


def signal_capture_children(captures: list[dict]) -> None:
    # Revalidate each specific child immediately before signaling to avoid a
    # stale PID being reused for an unrelated process.
    for capture in captures:
        if not still_same_capture(capture):
            continue
        result = run(["sudo", "-n", "kill", "-INT", str(capture["tcpdump_pid"])], timeout=5.0)
        capture["signal_sent"] = bool(result is not None and result.returncode == 0)


def process_is_gone_or_zombie(pid: int, expected_comm: str,
                              original_start_ticks: str | None,
                              table: dict[int, dict[str, object]]) -> bool:
    row = table.get(pid)
    if row is None:
        return True
    if str(row["comm"]).lower() != expected_comm:
        return True
    if str(row["state"]).startswith("Z"):
        return True
    current_start = process_start_ticks(pid)
    return original_start_ticks is not None and current_start != original_start_ticks


def wait_for_captures(captures: list[dict], timeout: float = 12.0) -> None:
    deadline = time.monotonic() + timeout
    while True:
        table = process_table()
        if table is None:
            if time.monotonic() >= deadline:
                return
            time.sleep(0.2)
            continue
        all_done = True
        for capture in captures:
            child_pid = capture["tcpdump_pid"]
            if child_pid is not None:
                capture["process_exited"] = process_is_gone_or_zombie(
                    child_pid, "tcpdump", capture.get("_tcpdump_start_ticks"), table
                )
            wrapper_pid = capture["wrapper_pid"]
            wrapper_ticks = capture.get("_wrapper_start_ticks")
            capture["wrapper_exited"] = process_is_gone_or_zombie(
                wrapper_pid, "sudo", wrapper_ticks, table
            )
            if capture["signal_sent"] and not (
                capture["process_exited"] and capture["wrapper_exited"]
            ):
                all_done = False
        if all_done or time.monotonic() >= deadline:
            return
        time.sleep(0.2)


def count_pcap_port(path: pathlib.Path, port: int) -> tuple[int | None, bool]:
    process: subprocess.Popen | None = None
    try:
        process = subprocess.Popen(
            ["sudo", "-n", "tcpdump", "-nn", "-q", "-r", str(path), f"port {port}"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
        )
        assert process.stdout is not None
        count = 0
        # Stream and discard decoded packet lines as they arrive, keeping no
        # addresses or packet text in memory beyond the current line.
        for line in iter(process.stdout.readline, b""):
            if line.strip():
                count += 1
        returncode = process.wait(timeout=45.0)
        return (count, True) if returncode == 0 else (None, False)
    except (OSError, subprocess.TimeoutExpired):
        if process is not None and process.poll() is None:
            try:
                process.kill()
                process.wait(timeout=5.0)
            except (OSError, subprocess.TimeoutExpired):
                pass
        return None, False
    finally:
        if process is not None and process.stdout is not None:
            process.stdout.close()


def hash_file(path: pathlib.Path) -> tuple[int | None, str | None]:
    digest = hashlib.sha256()
    size = 0
    try:
        with path.open("rb") as source:
            while True:
                block = source.read(1024 * 1024)
                if not block:
                    break
                size += len(block)
                digest.update(block)
    except OSError:
        return None, None
    return size, digest.hexdigest()


def finalize_pcap_counts(captures: list[dict]) -> None:
    for capture in captures:
        path = EXPECTED_PCAP[capture["node"]]
        path_ok = path.is_file() and os.path.realpath(path) == str(path)
        if not path_ok:
            continue
        counts: dict[str, int | None] = {}
        read_ok = True
        for port in PORTS:
            count, ok = count_pcap_port(path, port)
            counts[str(port)] = count
            read_ok = read_ok and ok
        capture["packet_counts"] = counts
        capture["pcap_read_ok"] = read_ok
        capture["pcap_bytes"], capture["pcap_sha256"] = hash_file(path)


def audit_fault_config(path: pathlib.Path) -> dict:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
        readable = True
    except (OSError, ValueError, TypeError):
        value = None
        readable = False
    return {"readable": readable, "empty_object": readable and value == {}}


def audit_iptables() -> dict:
    result = run(["sudo", "-n", "iptables-save"], timeout=12.0)
    if result is None or result.returncode != 0:
        return {"checked": False, "gsm04_reference_count": None, "clean": False}
    text = decode(result.stdout)
    references = set(re.findall(r"\bGSM_04_[A-Z0-9_]+\b", text))
    return {
        "checked": True,
        "gsm04_reference_count": len(references),
        "clean": len(references) == 0,
    }


def audit_qdisc() -> dict:
    interface_result = run(["ip", "-j", "link", "show", "dev", "gmr-a"], timeout=8.0)
    try:
        interfaces = json.loads(decode(interface_result.stdout)) if interface_result and interface_result.returncode == 0 else None
    except (ValueError, TypeError):
        interfaces = None
    interface_present = isinstance(interfaces, list) and len(interfaces) == 1

    result = run(["sudo", "-n", "tc", "-j", "qdisc", "show", "dev", "gmr-a"], timeout=12.0)
    try:
        rows = json.loads(decode(result.stdout)) if result and result.returncode == 0 else None
    except (ValueError, TypeError):
        rows = None
    if not isinstance(rows, list):
        return {
            "checked": False, "interface_present": interface_present,
            "qdisc_count": None, "netem_count": None, "no_netem": False,
        }
    kinds = [row.get("kind") for row in rows if isinstance(row, dict)]
    netem_count = sum(kind == "netem" for kind in kinds)
    return {
        "checked": True,
        "interface_present": interface_present,
        "qdisc_count": len(rows),
        "netem_count": netem_count,
        "no_netem": netem_count == 0,
    }


def parse_docker_top(data: bytes) -> tuple[int, int] | None:
    """Parse an explicit PID+comm table; malformed output cannot pass audit."""
    lines = [line.strip() for line in decode(data).splitlines() if line.strip()]
    if not lines or lines[0].upper().split() not in (["PID", "COMMAND"], ["PID", "COMM"]):
        return None
    names = []
    for line in lines[1:]:
        fields = line.split(None, 1)
        if len(fields) != 2 or not fields[0].isdigit() or int(fields[0]) <= 0:
            return None
        names.append(pathlib.Path(fields[1].strip("'\"")).name.lower())
    return len(names), sum(name == "asterisk" or name.startswith("asterisk:") for name in names)


def audit_container(node: str, component: str, container: str) -> dict:
    # Docker requires the host PID column to identify each container process.
    result = run(["sudo", "-n", "docker", "top", container, "-eo", "pid,comm"], timeout=15.0)
    counts = parse_docker_top(result.stdout) if result is not None and result.returncode == 0 else None
    return {
        "node": node,
        "component": component,
        "checked": counts is not None,
        "process_count": counts[0] if counts else None,
        "asterisk_process_count": counts[1] if counts else None,
        "no_asterisk": counts is not None and counts[0] > 0 and counts[1] == 0,
    }


def scrub_capture(capture: dict) -> dict:
    return {key: value for key, value in capture.items() if not key.startswith("_") and key != "safe_to_signal"}


def main() -> int:
    global ROOT, PRIVATE, EVIDENCE, CAPTURE_PIDS, EXPECTED_PCAP
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=pathlib.Path, default=ROOT,
                        help="isolated lab run root containing private/ and evidence/")
    parser.add_argument("--output-name", default="final-negative-audit.json")
    parser.add_argument("--capture-phase", help="isolated capture subdirectory under private/captures/")
    args = parser.parse_args()
    if pathlib.Path(args.output_name).name != args.output_name or not args.output_name.endswith(".json"):
        parser.error("--output-name must be a JSON basename")
    if args.capture_phase and not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,79}", args.capture_phase):
        parser.error("invalid capture phase")
    ROOT = args.root.resolve()
    PRIVATE, EVIDENCE = ROOT / "private", ROOT / "evidence"
    capture_root = PRIVATE / "captures" / args.capture_phase if args.capture_phase else PRIVATE
    CAPTURE_PIDS = capture_root / "capture-pids.json"
    EXPECTED_PCAP = {node: capture_root / f"no-sip-{node}.pcap" for node in EXPECTED_NODES}
    if (EVIDENCE / args.output_name).exists():
        parser.error("output exists; use a fresh --output-name or run root")
    EVIDENCE.mkdir(parents=True, exist_ok=True)

    entries, manifest_valid = load_capture_manifest()
    before = process_table()
    captures = [inspect_capture(entry, before) for entry in entries]
    for capture in captures:
        capture["_wrapper_start_ticks"] = process_start_ticks(capture["wrapper_pid"])

    # Send SIGINT only to each independently identified tcpdump child, never
    # the sudo wrapper. All children are signaled before any final pcap reads.
    signal_capture_children(captures)
    wait_for_captures(captures)
    finalize_pcap_counts(captures)

    docker_results = [audit_container(*item) for item in DOCKER_COMPONENTS]
    http_fault = audit_fault_config(PRIVATE / "http-fault.json")
    network_fault = audit_fault_config(PRIVATE / "fault-state.json")
    iptables = audit_iptables()
    qdisc = audit_qdisc()

    expected_capture_nodes = set(EXPECTED_NODES)
    actual_capture_nodes = {capture["node"] for capture in captures}
    capture_configuration_ok = bool(
        manifest_valid
        and actual_capture_nodes == expected_capture_nodes
        and len(captures) == 3
        and all(
            capture["wrapper_is_sudo"]
            and capture["tcpdump_child_count"] == 1
            and capture["tcpdump_executable"] == "tcpdump"
            and capture["interface_matches"]
            and capture["unbuffered"]
            and capture["filter_matches_required_ports"]
            and capture["pcap_target_matches"]
            and capture["pcap_open_by_tcpdump"]
            for capture in captures
        )
    )
    captures_flushed = bool(
        len(captures) == 3
        and all(
            capture["signal_sent"] and capture["process_exited"] and capture["wrapper_exited"]
            for capture in captures
        )
    )
    pcaps_read = bool(
        len(captures) == 3
        and all(capture["pcap_read_ok"] and capture["pcap_sha256"] for capture in captures)
    )
    ports_zero = bool(
        pcaps_read
        and all(
            capture["packet_counts"] == {str(port): 0 for port in PORTS}
            for capture in captures
        )
    )
    docker_top_clean = bool(
        len(docker_results) == len(DOCKER_COMPONENTS)
        and all(
            item["checked"] and item["no_asterisk"]
            and isinstance(item["process_count"], int) and item["process_count"] > 0
            for item in docker_results
        )
    )
    assertions = {
        "capture_manifest_matches_three_bridges": capture_configuration_ok,
        "actual_tcpdump_children_sigint_flushed": captures_flushed,
        "final_pcaps_read_and_hashed": pcaps_read,
        "sip_and_ari_port_counts_zero": ports_zero,
        "all_server_containers_have_no_asterisk": docker_top_clean,
        "http_fault_config_empty": http_fault["empty_object"],
        "network_fault_config_empty": network_fault["empty_object"],
        "no_gsm04_iptables_rules_or_references": iptables["checked"] and iptables["clean"],
        "gmr_a_exists_and_has_no_netem": qdisc["checked"] and qdisc["interface_present"] and qdisc["no_netem"],
    }
    result = {
        "schema_version": 1,
        "created_at": dt.datetime.now(dt.timezone.utc).isoformat().replace("+00:00", "Z"),
        "status": "PASS" if all(assertions.values()) else "FAIL",
        "capture_phase": args.capture_phase,
        "captures": [scrub_capture(capture) for capture in captures],
        "docker_top": docker_results,
        "fault_configs": {
            "http_fault": http_fault,
            "network_fault": network_fault,
        },
        "iptables": iptables,
        "qdisc": qdisc,
        "assertions": assertions,
        "safety": {
            "raw_packet_lines_written": False,
            "raw_process_arguments_written": False,
            "environment_variables_written": False,
            "stderr_or_raw_logs_written": False,
        },
    }
    destination = EVIDENCE / args.output_name
    temporary = destination.with_suffix(".json.tmp")
    temporary.write_text(json.dumps(result, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    os.chmod(temporary, 0o600)
    temporary.replace(destination)
    print(json.dumps({"status": result["status"], "output": str(destination), "assertions": assertions}, sort_keys=True))
    return 0 if result["status"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
