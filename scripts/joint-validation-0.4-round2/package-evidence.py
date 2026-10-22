#!/usr/bin/env python3
"""Build a checked ZIP of reviewed GSM joint-test evidence.

This helper is intentionally local-only. It reads the project artifact folders,
validates structured evidence, and creates an archive only when explicitly run.
Raw logs, private material, backups, and unapproved media JSONL are excluded.
"""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
import re
import struct
import sys
import tempfile
import xml.etree.ElementTree as ET
import zipfile
from pathlib import Path
from typing import Any


PROJECT = Path(__file__).resolve().parents[1]
ARTIFACTS = PROJECT / "artifacts"
EVIDENCE = ARTIFACTS / "evidence"
ISOLATED_SCOPE = False
MEDIA_DIRS = (
    PROJECT / "evidence" / "media-logs",
    EVIDENCE / "media-logs",
)
MEDIA_FILES = {
    "host.jsonl", "gateway.jsonl", "server-a.jsonl", "server-b.jsonl",
    "server-c.jsonl",
}
ALLOWED_SUFFIXES = {".json", ".jsonl", ".png", ".xml"}
MEDIA_MARKERS = {
    "selected_pair", "tx_packets", "rx_packets", "pcm_capture",
    "pcm_decoded", "pcm_underruns", "pcm_overruns", "control_transport",
}
MEDIA_SOURCES = {"host_log", "gateway_log", "server_log", "server_db_state"}
UUID_RE = re.compile(
    r"^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-"
    r"[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$"
)
BEARER_RE = re.compile(
    r"\bBearer[ \t]+[A-Za-z0-9._~+/=-]{24,}(?![A-Za-z0-9._~+/=-])",
    re.IGNORECASE,
)
PLACEHOLDERS = {
    "", "none", "null", "unset", "unavailable", "not set", "not present",
    "not configured", "redacted", "<redacted>", "[redacted]", "omitted",
    "<omitted>", "n/a", "na", "false", "***",
}
BACKUP_NAME_RE = re.compile(r"(?:^|[._-])(?:backup|bak|old)(?:[._-]|$)", re.IGNORECASE)


class PackageError(Exception):
    pass


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def relpath(path: Path) -> str:
    return path.relative_to(PROJECT).as_posix()


def read_evidence(path: Path) -> bytes:
    try:
        return path.read_bytes()
    except OSError:
        raise PackageError(f"cannot read evidence file: {relpath(path)}") from None


def excluded(path: Path) -> bool:
    parts = [part.lower() for part in path.relative_to(PROJECT).parts]
    if any(part in {"private", ".git", "keystore", "keys", "secrets"} for part in parts):
        return True
    name = path.name.lower()
    if name == ".env" or name.startswith(".env."):
        return True
    if BACKUP_NAME_RE.search(path.stem):
        return True
    return False


def safe_member(path: Path) -> bool:
    return path.is_file() and not path.is_symlink() and not excluded(path)


def json_path(parent: str, key: str | int) -> str:
    if isinstance(key, int):
        return f"{parent}[{key}]"
    if re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", key):
        return f"{parent}.{key}"
    return f"{parent}[{json.dumps(key, ensure_ascii=True)}]"


def normalized_key(key: str) -> str:
    key = re.sub(r"(?<!^)(?=[A-Z])", "_", key)
    return re.sub(r"[^a-z0-9]+", "_", key.lower()).strip("_")


def looks_present(value: Any) -> bool:
    if value is None:
        return False
    if isinstance(value, bool):
        return value
    if isinstance(value, (int, float)):
        return value != 0
    if isinstance(value, str):
        text = value.strip()
        return text.lower() not in PLACEHOLDERS and not re.fullmatch(r"\*+", text)
    if isinstance(value, (dict, list)):
        return bool(value)
    return True


def contains_present_leaf(value: Any) -> bool:
    if isinstance(value, dict):
        return any(contains_present_leaf(child) for child in value.values())
    if isinstance(value, list):
        return any(contains_present_leaf(child) for child in value)
    return looks_present(value)


def scan_secrets(value: Any, source_path: str, path: str = "$") -> list[str]:
    """Return JSON paths only; never retain or format suspicious values."""
    findings: list[str] = []

    def walk(item: Any, current: str, turn_context: bool = False) -> None:
        if isinstance(item, dict):
            for raw_key, child in item.items():
                key = str(raw_key)
                normalized = normalized_key(key)
                child_path = json_path(current, key)
                in_turn = turn_context or "turn" in normalized or normalized in {
                    "ice_server", "ice_servers",
                }
                access_refresh = normalized in {"access_token", "refresh_token"}
                turn_secret = in_turn and any(
                    part in normalized.split("_")
                    for part in ("secret", "password", "credential", "credentials")
                )
                if (access_refresh or turn_secret) and contains_present_leaf(child):
                    findings.append(f"{source_path}:{child_path}")
                walk(child, child_path, in_turn)
        elif isinstance(item, list):
            for index, child in enumerate(item):
                walk(child, json_path(current, index), turn_context)
        elif isinstance(item, str) and BEARER_RE.search(item):
            findings.append(f"{source_path}:{current}")

    walk(value, path)
    return findings


def scan_xml_secrets(root: ET.Element, source_path: str) -> list[str]:
    findings: list[str] = []

    def walk(element: ET.Element, path: str, turn_context: bool = False) -> None:
        tag = element.tag.rsplit("}", 1)[-1]
        normalized = normalized_key(tag)
        in_turn = turn_context or "turn" in normalized or normalized in {
            "ice_server", "ice_servers",
        }
        tag_secret = normalized in {"access_token", "refresh_token"} or (
            in_turn and any(part in normalized.split("_") for part in (
                "secret", "password", "credential", "credentials",
            ))
        )
        if tag_secret and looks_present(element.text or ""):
            findings.append(f"{source_path}:{path}")
        elif element.text and BEARER_RE.search(element.text):
            findings.append(f"{source_path}:{path}")
        for attr, value in element.attrib.items():
            attr_key = normalized_key(attr)
            attr_secret = attr_key in {"access_token", "refresh_token"} or (
                in_turn and any(part in attr_key.split("_") for part in (
                    "secret", "password", "credential", "credentials",
                ))
            )
            if attr_secret and looks_present(value):
                findings.append(f"{source_path}:{path}/@{attr}")
            elif BEARER_RE.search(value):
                findings.append(f"{source_path}:{path}/@{attr}")
        for index, child in enumerate(list(element)):
            child_tag = child.tag.rsplit("}", 1)[-1]
            walk(child, f"{path}/{child_tag}[{index}]", in_turn)

    walk(root, "$")
    return findings


def parse_json_bytes(data: bytes, label: str) -> Any:
    try:
        return json.loads(data.decode("utf-8-sig"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        where = f" line {exc.lineno}" if isinstance(exc, json.JSONDecodeError) else ""
        raise PackageError(f"invalid JSON at {label}{where}") from None


def parse_jsonl_bytes(data: bytes, label: str) -> list[Any]:
    try:
        text = data.decode("utf-8-sig")
    except UnicodeDecodeError:
        raise PackageError(f"invalid JSONL encoding at {label}") from None
    rows: list[Any] = []
    for number, line in enumerate(text.splitlines(), 1):
        if not line.strip():
            continue
        try:
            rows.append(json.loads(line))
        except json.JSONDecodeError:
            raise PackageError(f"invalid JSONL at {label} line {number}") from None
    return rows


def walk_json_rows(value: Any):
    if isinstance(value, dict):
        yield value
        for child in value.values():
            yield from walk_json_rows(child)
    elif isinstance(value, list):
        for child in value:
            yield from walk_json_rows(child)


def is_exporter_media_row(row: Any) -> bool:
    if not isinstance(row, dict):
        return False
    if row.get("source") in MEDIA_SOURCES:
        return True
    if row.get("role") in {"host", "gateway"}:
        return True
    return bool(MEDIA_MARKERS.intersection(row))


def validate_media_dir(directory: Path) -> list[Path]:
    if not directory.exists():
        return []
    if directory.is_symlink() or not directory.is_dir() or excluded(directory):
        raise PackageError(f"unsafe media evidence directory: {relpath(directory)}")
    summary_path = directory / "summary.json"
    members = [directory / name for name in sorted(MEDIA_FILES)]
    if not summary_path.exists() and not any(path.exists() for path in members):
        return []
    if not safe_member(summary_path) or any(not safe_member(path) for path in members):
        raise PackageError(f"incomplete exporter output: {relpath(directory)}")

    summary = parse_json_bytes(read_evidence(summary_path), relpath(summary_path))
    if not isinstance(summary, dict) or summary.get("schema_version") != 1:
        raise PackageError(f"unrecognized media exporter summary: {relpath(summary_path)}")
    safety = summary.get("safety")
    if not isinstance(safety, dict) or any(
        safety.get(key) is not False
        for key in ("raw_lines_written", "unknown_fields_emitted", "database_rows_are_logs")
    ):
        raise PackageError(f"media exporter safety marker missing: {relpath(summary_path)}")
    files = summary.get("files")
    if not isinstance(files, dict) or set(files) != MEDIA_FILES:
        raise PackageError(f"media exporter file manifest mismatch: {relpath(summary_path)}")

    for path in members:
        label = relpath(path)
        raw = read_evidence(path)
        row_meta = files.get(path.name)
        if not isinstance(row_meta, dict) or row_meta.get("sha256") != sha256(raw):
            raise PackageError(f"media exporter hash mismatch: {label}")
        rows = parse_jsonl_bytes(raw, label)
        if row_meta.get("records") != len(rows):
            raise PackageError(f"media exporter record count mismatch: {label}")
        for row in rows:
            if not isinstance(row, dict) or not is_exporter_media_row(row):
                raise PackageError(f"unexpected media exporter row: {label}")
            if set(row) - {
                "source", "node", "timestamp", "call_id", "leg_id", "event", "role",
                "reason", "selected_pair", "state_revision", "state", "control_transport",
                "elapsed_ms", "rate", "tx_packets", "rx_packets", "pcm_capture",
                "pcm_decoded", "pcm_underruns", "pcm_overruns",
            }:
                raise PackageError(f"unexpected media exporter fields: {label}")
            if not isinstance(row.get("call_id"), str) or not UUID_RE.fullmatch(row["call_id"]):
                raise PackageError(f"invalid media exporter identifier: {label}")
    # Validate the summary as JSON and scan its actual structured values too.
    findings = scan_secrets(summary, relpath(summary_path))
    if findings:
        raise PackageError("credential-like value found at " + ", ".join(findings))
    return [summary_path, *members]


def discover_candidates(
    include_empty_pcap: bool, output: Path, index: Path
) -> tuple[list[Path], set[Path]]:
    candidates: set[Path] = set()
    approved_media: set[Path] = set()
    excluded_paths = {output.resolve(), index.resolve()}

    def add(path: Path) -> None:
        if path.resolve() in excluded_paths:
            return
        if safe_member(path):
            candidates.add(path)

    if EVIDENCE.exists():
        for path in EVIDENCE.rglob("*"):
            if not path.is_file() or path.is_symlink() or excluded(path):
                continue
            if any(parent.name == "media-logs" for parent in path.parents):
                continue
            suffix = path.suffix.lower()
            if suffix in ALLOWED_SUFFIXES:
                add(path)
            elif suffix == ".pcap" and include_empty_pcap:
                add(path)

    # Only the named result summaries and regression reports are admitted at
    # the artifacts root; arbitrary logs, archives, APKs, and backups stay out.
    for path in (() if ISOLATED_SCOPE else (
        ARTIFACTS / "image-no-asterisk.json",
        ARTIFACTS / "source-manifest.json",
        ARTIFACTS / "incoming-fork-regression.xml",
    )):
        add(path)
    if not ISOLATED_SCOPE and ARTIFACTS.exists():
        for path in ARTIFACTS.glob("regression*.xml"):
            add(path)
    builds = ARTIFACTS / "builds"
    if not ISOLATED_SCOPE and builds.exists():
        for path in builds.rglob("manifest.json"):
            add(path)

    for directory in MEDIA_DIRS:
        for path in validate_media_dir(directory):
            add(path)
            if path.suffix.lower() == ".jsonl":
                approved_media.add(path)

    return sorted(candidates, key=relpath), approved_media


def validate_pcap_header(path: Path, label: str) -> None:
    raw = read_evidence(path)
    if len(raw) != 24:
        raise PackageError(f"pcap is not an empty capture (global header only): {label}")
    byte_order = {
        b"\xd4\xc3\xb2\xa1": "<",
        b"\xa1\xb2\xc3\xd4": ">",
        b"\x4d\x3c\xb2\xa1": "<",
        b"\xa1\xb2\x3c\x4d": ">",
    }.get(raw[:4])
    if byte_order is None:
        raise PackageError(f"unsupported pcap header: {label}")
    _, major, minor, _zone, _sigfigs, snaplen, _network = struct.unpack(
        byte_order + "IHHIIII", raw
    )
    if (major, minor) != (2, 4) or snaplen == 0:
        raise PackageError(f"invalid pcap global header: {label}")


def validate_candidate(
    path: Path, include_empty_pcap: bool, approved_media: set[Path]
) -> tuple[bytes, dict[str, Any]]:
    label = relpath(path)
    try:
        data = read_evidence(path)
    except PackageError:
        raise
    suffix = path.suffix.lower()
    if suffix == ".json":
        parsed = parse_json_bytes(data, label)
        findings = scan_secrets(parsed, label)
        if findings:
            raise PackageError("credential-like value found at " + ", ".join(findings))
    elif suffix == ".jsonl":
        rows = parse_jsonl_bytes(data, label)
        findings: list[str] = []
        for row_number, row in enumerate(rows, 1):
            findings.extend(scan_secrets(row, label, f"$[line {row_number}]"))
            if is_exporter_media_row(row) and path not in approved_media:
                raise PackageError(f"media JSONL outside validated exporter output: {label}")
        if findings:
            raise PackageError("credential-like value found at " + ", ".join(findings))
    elif suffix == ".xml":
        try:
            xml_root = ET.fromstring(data)
        except ET.ParseError:
            raise PackageError(f"invalid XML evidence: {label}") from None
        findings = scan_xml_secrets(xml_root, label)
        if findings:
            raise PackageError("credential-like value found at " + ", ".join(findings))
    elif suffix == ".png":
        if not data.startswith(b"\x89PNG\r\n\x1a\n"):
            raise PackageError(f"invalid PNG evidence: {label}")
    elif suffix == ".pcap" and include_empty_pcap:
        validate_pcap_header(path, label)
    else:
        raise PackageError(f"unsupported evidence type: {label}")
    return data, {"path": label, "size": len(data), "sha256": sha256(data)}


def write_json_atomic(path: Path, value: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temp_path: Path | None = None
    try:
        with tempfile.NamedTemporaryFile(
            mode="w", encoding="utf-8", newline="\n", dir=path.parent,
            prefix=f".{path.name}.", suffix=".tmp", delete=False,
        ) as handle:
            temp_path = Path(handle.name)
            json.dump(value, handle, indent=2, sort_keys=True)
            handle.write("\n")
        os.replace(temp_path, path)
    finally:
        if temp_path is not None and temp_path.exists():
            temp_path.unlink()


def build_package(args: argparse.Namespace) -> dict[str, Any]:
    output = args.output.resolve()
    index_path = args.index.resolve()
    if output == index_path:
        raise PackageError("archive and index paths must differ")
    if (output.exists() or index_path.exists()) and not args.overwrite:
        raise PackageError("output already exists; pass --overwrite to replace it")

    paths, approved_media = discover_candidates(args.include_empty_pcap, output, index_path)
    if not paths:
        raise PackageError("no eligible evidence files found")

    payloads: list[tuple[str, bytes]] = []
    entries: list[dict[str, Any]] = []
    for path in paths:
        data, record = validate_candidate(path, args.include_empty_pcap, approved_media)
        payloads.append((record["path"], data))
        entries.append(record)

    package_index = {
        "schema_version": 1,
        "created_at": dt.datetime.now(dt.timezone.utc).isoformat().replace("+00:00", "Z"),
        "files": entries,
    }
    index_bytes = (json.dumps(package_index, indent=2, sort_keys=True) + "\n").encode("utf-8")
    output.parent.mkdir(parents=True, exist_ok=True)
    temp_archive: Path | None = None
    try:
        with tempfile.NamedTemporaryFile(
            dir=output.parent, prefix=f".{output.name}.", suffix=".tmp", delete=False
        ) as handle:
            temp_archive = Path(handle.name)
        with zipfile.ZipFile(temp_archive, "w", compression=zipfile.ZIP_DEFLATED) as archive:
            for name, data in payloads:
                archive.writestr(name, data)
            archive.writestr("PACKAGE-INDEX.json", index_bytes)

        with zipfile.ZipFile(temp_archive, "r") as archive:
            if archive.testzip() is not None:
                raise PackageError("archive CRC verification failed")
            expected_names = {name for name, _data in payloads} | {"PACKAGE-INDEX.json"}
            if set(archive.namelist()) != expected_names:
                raise PackageError("archive member list verification failed")
            for name, data in payloads:
                packed = archive.read(name)
                if len(packed) != len(data) or sha256(packed) != sha256(data):
                    raise PackageError(f"archive content verification failed: {name}")
            if archive.read("PACKAGE-INDEX.json") != index_bytes:
                raise PackageError("archive index verification failed")

        archive_bytes = temp_archive.read_bytes()
        result = {
            **package_index,
            "archive": relpath(output) if output.is_relative_to(PROJECT) else str(output),
            "archive_size": len(archive_bytes),
            "archive_sha256": sha256(archive_bytes),
            "verified": True,
        }
        os.replace(temp_archive, output)
        temp_archive = None
        write_json_atomic(index_path, result)
        return result
    finally:
        if temp_archive is not None and temp_archive.exists():
            temp_archive.unlink()


def main() -> int:
    global EVIDENCE, MEDIA_DIRS, ISOLATED_SCOPE
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--output", type=Path, default=ARTIFACTS / "evidence-package.zip",
        help="ZIP destination (default: artifacts/evidence-package.zip)",
    )
    parser.add_argument(
        "--index", type=Path, default=ARTIFACTS / "evidence-package-index.json",
        help="sidecar SHA-256 index (default: artifacts/evidence-package-index.json)",
    )
    parser.add_argument("--evidence-root", type=Path,
                        help="package only this directory within the project; excludes first-round root summaries")
    parser.add_argument("--include-empty-pcap", action="store_true",
                        help="include only classic PCAP files containing a valid 24-byte header and no packets")
    parser.add_argument("--overwrite", action="store_true",
                        help="replace an existing ZIP and index")
    args = parser.parse_args()
    if args.evidence_root is not None:
        EVIDENCE = args.evidence_root.resolve()
        if not EVIDENCE.is_relative_to(PROJECT) or EVIDENCE == PROJECT or args.evidence_root.is_symlink():
            parser.error("--evidence-root must be a real directory strictly within the project")
        if EVIDENCE == ARTIFACTS / "evidence":
            parser.error("use default mode for first-round artifacts/evidence")
        if args.output.resolve() == ARTIFACTS / "evidence-package.zip" or args.index.resolve() == ARTIFACTS / "evidence-package-index.json":
            parser.error("isolated evidence requires explicit distinct --output and --index")
        ISOLATED_SCOPE = True
        MEDIA_DIRS = (EVIDENCE / "media-logs",)
    try:
        result = build_package(args)
    except PackageError as exc:
        # Error messages contain paths and structural reasons only, never JSON values.
        print(f"package-evidence: {exc}", file=sys.stderr)
        return 2
    except OSError:
        print("package-evidence: filesystem operation failed", file=sys.stderr)
        return 2
    print(json.dumps({
        "archive": result["archive"],
        "archive_sha256": result["archive_sha256"],
        "files": len(result["files"]),
        "index": str(args.index),
        "verified": result["verified"],
    }, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
