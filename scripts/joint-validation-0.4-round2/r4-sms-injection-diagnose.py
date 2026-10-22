#!/usr/bin/env python3
"""R4 pre-check: why did the baseline (0.3) inbound SMS injection fail before?

Installs nothing and changes no product state. It injects exactly one synthetic
inbound SMS through the emulator console (the same command the baseline helper
uses), then reads back what each layer saw:

  1. emulator console reply  -> was the injection accepted at all?
  2. Gateway 0.3 local SMS archive (SQLite, read-only snapshot)
  3. server messages table on A/B/C (read-only SELECT)
  4. system logcat lines mentioning SMS during the window

The JSON verdict distinguishes an emulator/injection-harness failure from an
old-product failure. Only a harness verdict may be followed by the R4 rollback
run; a product verdict must stop the plan.

Usage: r4-sms-injection-diagnose.py [--wait 60] [--out evidence/...json]
"""
from __future__ import annotations
import argparse
import datetime as dt
import hashlib
import io
import json
import re
import sqlite3
import subprocess
import tarfile
import tempfile
import time
from pathlib import Path

ROOT = Path('/home/ubuntu/gsm-webrtc-validation/joint-0.4')
ADB = '/home/ubuntu/gsm-webrtc-validation/android-sdk/platform-tools/adb'
GATEWAY_SERIAL = 'emulator-5554'
HOST_SERIAL = 'emulator-5556'
GATEWAY_PACKAGE = 'com.callagent.gateway'
SYNTHETIC_NUMBER = '+155****0200'
GATEWAY_ID = '77e3592d-d32d-466b-a5fd-89e39bb9061a'


def adb(*args, timeout=30):
    result = subprocess.run([ADB, '-s', GATEWAY_SERIAL, *args], capture_output=True, text=True,
                            timeout=timeout)
    return result.returncode, result.stdout.strip(), result.stderr.strip()


def docker_psql(node, query):
    if node == 'b':
        template = ['exec', 'gsm-rtc-mesh-b-db', 'psql', '-X', '-U', 'gsm2sip', '-d', 'gsm2sip', '-At', '-c']
    else:
        container = 'gsm-rtc-joint' if node == 'a' else 'gsm-rtc-mesh-c'
        port = '25432' if node == 'a' else '55432'
        template = ['exec', '--user', 'gsm2sip', container, '/usr/lib/postgresql/17/bin/psql',
                    '-X', '-q', '-h', '/run/postgresql', '-p', port, '-U', 'gsm2sip',
                    '-d', 'gsm2sip', '-At', '-c']
    if not query.lstrip().upper().startswith('SELECT ') or ';' in query:
        raise RuntimeError('read_only_select_required')
    result = subprocess.run(['sudo', '-n', 'docker', *template,
                             'BEGIN READ ONLY; ' + query + '; COMMIT;'],
                            capture_output=True, text=True, timeout=60)
    if result.returncode != 0:
        raise RuntimeError('psql_failed_' + node)
    return [line for line in result.stdout.splitlines() if line.strip()]


def gateway_archive():
    """Read-only snapshot of the gateway databases via run-as tar."""
    result = subprocess.run([ADB, '-s', GATEWAY_SERIAL, 'exec-out', 'run-as', GATEWAY_PACKAGE,
                             'tar', '-cf', '-', 'databases'], capture_output=True, timeout=120)
    if result.returncode != 0:
        raise RuntimeError('gateway_snapshot_failed')
    temp = Path(tempfile.mkdtemp(prefix='r4diag-', dir=str(ROOT / 'private')))
    with tarfile.open(fileobj=io.BytesIO(result.stdout)) as archive:
        archive.extractall(temp, filter='data')
    db = temp / 'databases/gateway-data.db'
    connection = sqlite3.connect('file:' + str(db) + '?mode=ro', uri=True)
    rows = connection.execute(
        'SELECT message_id,direction,status,body FROM sms_archive_history ORDER BY rowid DESC LIMIT 40'
    ).fetchall()
    total = connection.execute('SELECT count(*) FROM sms_archive_history').fetchone()[0]
    options = connection.execute('SELECT count(*) FROM sms_archive_options').fetchone()[0]
    connection.close()
    return {'path': str(temp), 'total_rows': total, 'option_rows': options,
            'recent': [{'message_id': r[0], 'direction': r[1], 'status': r[2],
                        'body_sha256': hashlib.sha256(str(r[3]).encode()).hexdigest(),
                        'body_length': len(str(r[3]) or '')} for r in rows]}


def sha(text):
    return hashlib.sha256(text.encode()).hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--wait', type=int, default=60)
    parser.add_argument('--out', default='')
    args = parser.parse_args()
    stamp = dt.datetime.now(dt.timezone.utc)
    run_id = stamp.strftime('%Y%m%dT%H%M%SZ')
    text = 'gsm04r4diag-inbound-' + run_id
    report = {'schema': 'gsm04-r4-inbound-injection-diagnosis-v1', 'run_id': run_id,
              'started_at': stamp.isoformat(), 'synthetic_number': SYNTHETIC_NUMBER,
              'text_sha256': sha(text), 'steps': []}
    out = Path(args.out) if args.out else ROOT / 'evidence' / ('r4-inbound-injection-diagnosis-' + run_id + '.json')
    log_path = ROOT / 'private/gateway-logcat.txt'
    try:
        offset = log_path.stat().st_size if log_path.exists() else 0
        report['gateway_log_offset'] = offset
        _, ready, _ = adb('shell', 'run-as', GATEWAY_PACKAGE, 'cat', 'files/joint-ready.json')
        report['joint_ready'] = ready
        try:
            gateway_id = json.loads(ready)['gateway_id']
        except Exception:
            raise RuntimeError('joint_ready_unreadable') from None
        report['gateway_id'] = gateway_id
        versions = adb('shell', 'dumpsys', 'package', GATEWAY_PACKAGE)[1]
        report['gateway_version'] = [line.strip() for line in versions.splitlines()
                                     if 'versionName=' in line]
        before = gateway_archive()
        report['archive_before'] = {'total_rows': before['total_rows'], 'option_rows': before['option_rows']}
        report['server_before'] = {}
        for node in ('a', 'b', 'c'):
            report['server_before'][node] = docker_psql(
                node, "SELECT count(*) FROM messages WHERE gateway_id='" + gateway_id + "'::uuid")[0]
        report['steps'].append({'step': 'preconditions', 'status': 'PASS'})

        command = ['emu', 'sms', 'send', SYNTHETIC_NUMBER, text]
        started = time.monotonic()
        code, stdout, stderr = adb(*command, timeout=30)
        injection = {'command': 'adb -s ' + GATEWAY_SERIAL + ' ' + ' '.join(command),
                     'exit_code': code, 'stdout': stdout[:200], 'stderr': stderr[:200],
                     'accepted': code == 0 and 'OK' in stdout.upper(), 'seconds': time.monotonic() - started}
        report['injection'] = injection
        report['steps'].append({'step': 'inject', 'status': 'PASS' if injection['accepted'] else 'FAIL'})

        target = sha(text)
        archive_hit = None
        server_hit = None
        deadline = time.monotonic() + args.wait
        timeline = []
        while time.monotonic() < deadline:
            elapsed = round(time.monotonic() - started, 1)
            if archive_hit is None:
                snap = gateway_archive()
                for row in snap['recent']:
                    if row['body_sha256'] == target:
                        archive_hit = {**row, 'snapshot_rows': snap['total_rows'], 'at_seconds': elapsed}
                        break
            if server_hit is None:
                for node in ('a', 'b', 'c'):
                    rows = docker_psql(node, "SELECT id::text,status,direction FROM messages WHERE gateway_id='"
                                       + gateway_id + "'::uuid AND body='" + text.replace("'", "''") + "'")
                    if rows:
                        server_hit = {'node': node, 'rows': rows, 'at_seconds': elapsed}
                        break
            timeline.append({'at_seconds': elapsed, 'archive': archive_hit is not None,
                             'server': server_hit is not None})
            if archive_hit and server_hit:
                break
            time.sleep(3)
        report['archive_hit'] = archive_hit
        report['server_hit'] = server_hit
        report['poll_timeline'] = timeline[-6:]
        after = gateway_archive()
        report['archive_after'] = {'total_rows': after['total_rows']}

        matches = []
        if log_path.exists():
            with log_path.open('rb') as handle:
                handle.seek(offset)
                fresh = handle.read().decode(errors='replace')
            for line in fresh.splitlines():
                if re.search(r'sms|Sms|SMS', line):
                    matches.append(line[:200])
        report['logcat_sms_line_count'] = len(matches)
        report['logcat_sms_sample'] = matches[:12]
        report['steps'].append({'step': 'observe', 'status': 'PASS'})

        inbox = adb('shell', 'content', 'query', '--uri', 'content://sms/inbox',
                    '--projection', '_id,address,body,date')
        report['sms_provider_query'] = {'exit_code': inbox[0], 'stdout': inbox[1][:300],
                                        'stderr': inbox[2][:200]}

        if not injection['accepted']:
            verdict = 'harness_emulator_console_rejected'
        elif archive_hit and not server_hit:
            verdict = 'product_old_gateway_did_not_upload_or_server_rejected'
        elif server_hit and not archive_hit:
            verdict = 'product_old_gateway_server_only'
        elif not archive_hit and not server_hit:
            verdict = 'injection_not_delivered_to_app_or_not_archived'
        else:
            verdict = 'injection_ok_both_layers'
        report.update(verdict=verdict, finished_at=dt.datetime.now(dt.timezone.utc).isoformat())
        report['status'] = 'PASS'
    except Exception as error:
        report.update(status='FAIL', error=type(error).__name__, error_detail=str(error)[:200],
                      finished_at=dt.datetime.now(dt.timezone.utc).isoformat())
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(report, indent=2, sort_keys=True) + '\n')
    out.chmod(0o600)
    print(json.dumps({'status': report['status'], 'verdict': report.get('verdict'),
                      'injection': report.get('injection', {}).get('accepted'),
                      'archive_hit': bool(report.get('archive_hit')),
                      'server_hit': bool(report.get('server_hit')),
                      'evidence': str(out)}, sort_keys=True))
    return 0 if report.get('status') == 'PASS' else 1


if __name__ == '__main__':
    raise SystemExit(main())
