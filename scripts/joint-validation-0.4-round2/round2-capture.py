#!/usr/bin/env python3
"""Start fresh A/B/C port captures in one named phase; default is plan-only.

Use final-audit.py --root RUN_ROOT --capture-phase PHASE --output-name NAME.json
for safe SIGINT/flush/count/hash finalization. A phase is never reused. Capture
process arguments and packet text stay in private files, outside evidence ZIPs.
"""
from __future__ import annotations
import argparse
import datetime as dt
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import time


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument('--phase', required=True)
    parser.add_argument('--execute', action='store_true')
    args = parser.parse_args()
    if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,79}', args.phase):
        parser.error('invalid phase')
    root = args.root.resolve()
    capture_root = root / 'private/captures' / args.phase
    plan = {'status': 'PLAN_ONLY', 'phase': args.phase, 'root': str(root),
            'capture_root': str(capture_root), 'ports': [5060, 5061, 8088],
            'protocols': ['UDP', 'TCP'], 'bridges': ['gmr-a', 'gmr-b', 'gmr-c']}
    if not args.execute:
        print(json.dumps(plan, sort_keys=True))
        return 0
    if not (root / 'private/round2-marker.json').is_file():
        parser.error('requires an isolated root created by round2-prepare.py')
    if capture_root.exists() or capture_root.is_symlink():
        parser.error('capture phase exists; choose a fresh phase')
    os.umask(0o077)
    evidence_path = root / 'evidence' / ('capture-start-' + args.phase + '.json')
    if evidence_path.exists():
        parser.error('capture start evidence exists')
    capture_root.mkdir(parents=True, mode=0o700)
    spec = importlib.util.spec_from_file_location('round2_capture_audit', root / 'scripts/final-audit.py')
    audit = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(audit)
    audit.CAPTURE_PIDS = capture_root / 'capture-pids.json'
    audit.EXPECTED_PCAP = {node: capture_root / f'no-sip-{node}.pcap' for node in audit.EXPECTED_NODES}
    entries = []
    started_at = dt.datetime.now(dt.timezone.utc).isoformat()
    try:
        for node, interface in audit.EXPECTED_NODES.items():
            with (capture_root / (node + '-tcpdump.log')).open('wb') as stderr:
                child = subprocess.Popen(['sudo', '-n', 'tcpdump', '-i', interface, '-U', '-nn',
                                          '-w', str(audit.EXPECTED_PCAP[node]), *audit.EXPECTED_FILTER],
                                         stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                                         stderr=stderr, start_new_session=True)
            entries.append({'node': node, 'interface': interface, 'pid': child.pid})
            audit.CAPTURE_PIDS.write_text(json.dumps(entries, sort_keys=True) + '\n')
        deadline = time.monotonic() + 8
        while True:
            table = audit.process_table()
            inspections = [audit.inspect_capture({'node': item['node'], 'wrapper_pid': item['pid']}, table)
                           for item in entries]
            if all(item['safe_to_signal'] and item['interface_matches'] and item['unbuffered']
                   and item['filter_matches_required_ports'] for item in inspections):
                break
            if time.monotonic() >= deadline:
                raise RuntimeError('capture_start_identity_check_failed')
            time.sleep(0.2)
        result = {**plan, 'status': 'CAPTURING', 'started_at': started_at,
                  'manifest': str(audit.CAPTURE_PIDS), 'all_three_capture_identities_verified': True}
    except Exception:
        # The recorded exact PIDs remain available to final-audit; never kill
        # by name or silently pass an incompletely started capture window.
        result = {**plan, 'status': 'FAIL', 'started_at': started_at,
                  'reason': 'capture_start_failed', 'started_process_count': len(entries)}
    evidence_path.write_text(json.dumps(result, indent=2, sort_keys=True) + '\n')
    print(json.dumps(result, sort_keys=True))
    return 0 if result['status'] == 'CAPTURING' else 1


if __name__ == '__main__':
    raise SystemExit(main())
