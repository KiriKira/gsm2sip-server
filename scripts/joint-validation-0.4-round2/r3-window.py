#!/usr/bin/env python3
"""R3 window driver: one capture phase covering R1, R2, SMS, restarts, R5, idle.

Every step is logged with UTC timestamps and its raw output kept in private/.
The first failing step stops the window: the capture is still finalized (flush,
count, hash) so the phase stays bounded, and the script exits non-zero.

Usage: r3-window.py --phase r3-all-candidate-<UTC> [--idle-seconds 900]
"""
from __future__ import annotations
import argparse
import datetime as dt
import json
import os
import re
import subprocess
import sys
import time
from pathlib import Path

ROOT = Path('/home/ubuntu/gsm-webrtc-validation/joint-0.4')
ADB = '/home/ubuntu/gsm-webrtc-validation/android-sdk/platform-tools/adb'
SERIALS = {'gateway': 'emulator-5554', 'host': 'emulator-5556'}
SCRIPTS = ROOT / 'scripts'


def utc():
    return dt.datetime.now(dt.timezone.utc).isoformat(timespec='seconds')


class Window:
    def __init__(self, phase, idle_seconds):
        self.phase = phase
        self.idle_seconds = idle_seconds
        self.private = ROOT / 'private'
        self.evidence = ROOT / 'evidence'
        self.log_path = self.private / ('r3-window-' + phase + '.log')
        self.status_path = self.private / ('r3-window-' + phase + '-status.json')
        self.status = {'schema': 'gsm04-r3-window-v1', 'phase': phase, 'status': 'RUNNING',
                       'started_at': utc(), 'steps': []}

    def log(self, message):
        line = utc() + ' ' + message
        with self.log_path.open('a') as out:
            out.write(line + '\n')
        print(line, flush=True)

    def save(self):
        self.status_path.write_text(json.dumps(self.status, indent=2, ensure_ascii=False) + '\n')

    def run(self, name, command, timeout, expect=None, sudo=False, evidence=None):
        entry = {'step': name, 'command': ' '.join(command), 'started_at': utc(),
                 'sudo': sudo, 'timeout': timeout}
        self.log('START ' + name)
        full = (['sudo', '-n'] if sudo else []) + command
        try:
            result = subprocess.run(full, cwd=str(ROOT), stdin=subprocess.DEVNULL,
                                    stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                    text=True, timeout=timeout)
        except subprocess.TimeoutExpired:
            entry.update(status='FAIL', reason='timeout', finished_at=utc())
            self.status['steps'].append(entry)
            self.save()
            self.log('TIMEOUT ' + name)
            raise
        entry.update(exit_code=result.returncode, finished_at=utc())
        if evidence:
            (self.private / (name + '.log')).write_text(result.stdout)
        if result.returncode != 0:
            entry.update(status='FAIL', reason='nonzero_exit')
            self.status['steps'].append(entry)
            self.save()
            self.log('FAIL ' + name + ' exit=' + str(result.returncode))
            raise RuntimeError(name + '_failed')
        payload = None
        for line in reversed(result.stdout.strip().splitlines()):
            line = line.strip()
            if line.startswith('{') and line.endswith('}'):
                try:
                    payload = json.loads(line)
                    break
                except ValueError:
                    continue
        if payload is not None:
            entry['summary'] = {key: payload.get(key) for key in
                                ('status', 'phase', 'evidence', 'records', 'reason', 'error',
                                 'audio_seconds', 'actual_telecom_calls', 'active_node')
                                if key in payload}
            if expect and payload.get('status') != expect:
                entry.update(status='FAIL', reason='unexpected_status:' + str(payload.get('status')))
                self.status['steps'].append(entry)
                self.save()
                self.log('FAIL ' + name + ' status=' + str(payload.get('status')))
                raise RuntimeError(name + '_status_' + str(payload.get('status')))
        entry['status'] = 'PASS'
        self.status['steps'].append(entry)
        self.save()
        self.log('PASS ' + name)
        return payload

    def step(self, name, *args, expect='PASS', sudo=False, timeout=900):
        return self.run(name, [sys.executable] + [str(SCRIPTS / args[0])] + list(args[1:]),
                        timeout=timeout, expect=expect, sudo=sudo, evidence=True)

    def restart_app(self, role):
        serial = SERIALS[role]
        package = 'com.callagent.gateway' if role == 'gateway' else 'com.callagent.host'
        started = time.monotonic()
        subprocess.run([ADB, '-s', serial, 'shell', 'am', 'force-stop', package], check=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        entry = {'step': 'restart-' + role, 'action': 'force-stop then instrumented cold start',
                 'started_at': utc()}
        self.log('START restart-' + role)
        payload = self.run('restart-' + role, [sys.executable, str(SCRIPTS / 'apps.py'), 'start', role],
                           timeout=300, expect=None, evidence=True)
        entry['status'] = 'PASS'
        entry['seconds'] = time.monotonic() - started
        entry['ready'] = payload
        entry['finished_at'] = utc()
        self.status.setdefault('restarts', []).append(entry)
        self.save()

    def pause(self, name, seconds):
        self.log('START ' + name + ' ' + str(seconds) + 's')
        entry = {'step': name, 'started_at': utc(), 'seconds': seconds}
        time.sleep(seconds)
        entry.update(status='PASS', finished_at=utc())
        self.status['steps'].append(entry)
        self.save()

    def preflight(self):
        """Record the three sides' real identities before the capture starts."""
        import hashlib
        adb = ADB
        info = {'schema': 'gsm04-r3-window-preflight-v1', 'phase': self.phase, 'recorded_at': utc(),
                'apps': {}, 'servers': {}, 'captured_phase': None}
        packages = {'gateway': ('emulator-5554', 'com.callagent.gateway'),
                    'host': ('emulator-5556', 'com.callagent.host')}
        for role, (serial, package) in packages.items():
            path = subprocess.run([adb, '-s', serial, 'shell', 'pm', 'path', package],
                                  capture_output=True, text=True, timeout=60).stdout.strip()
            apk = path.split(':', 1)[1].strip() if ':' in path else ''
            digest = ''
            if apk:
                digest = subprocess.run([adb, '-s', serial, 'exec-out', 'sh', '-c', 'sha256sum ' + apk],
                                        capture_output=True, text=True, timeout=120).stdout.split()[0]
            dump = subprocess.run([adb, '-s', serial, 'shell', 'dumpsys', 'package', package],
                                  capture_output=True, text=True, timeout=60).stdout
            ready = subprocess.run([adb, '-s', serial, 'exec-out', 'run-as', package, 'cat',
                                    'files/joint-ready.json'], capture_output=True, text=True, timeout=60).stdout
            try:
                ready_doc = json.loads(ready)
            except ValueError:
                ready_doc = None
            info['apps'][role] = {
                'serial': serial, 'package': package, 'apk_path': apk, 'apk_sha256': digest,
                'versions': [line.strip() for line in dump.splitlines()
                             if 'versionCode=' in line or 'versionName=' in line],
                'joint_ready': ready_doc}
        images = subprocess.run(['sudo', '-n', 'docker', 'ps', '--format',
                                 '{{.Names}} {{.Image}} {{.Status}}'],
                                capture_output=True, text=True, timeout=60).stdout
        info['servers']['containers'] = [line for line in images.splitlines()
                                         if any(name in line for name in
                                                ('gsm-rtc-joint', 'gsm-rtc-mesh-b', 'gsm-rtc-mesh-c'))]
        for node, url in (('a', 'http://127.0.0.1:18096'), ('b', 'http://10.203.78.2:8080'),
                          ('c', 'http://10.203.78.10:8080')):
            result = subprocess.run(['curl', '-sS', '--max-time', '8', url + '/v1/capabilities'],
                                    capture_output=True, text=True)
            info['servers'][node] = {'url': url, 'capabilities': result.stdout.strip()}
        missing = [role for role, app in info['apps'].items()
                   if not app['apk_sha256'] or not app['joint_ready']
                   or app['joint_ready'].get('status') != 'READY']
        info['status'] = 'PASS' if not missing else 'FAIL'
        info['missing_ready_or_hash'] = missing
        path = self.evidence / ('window-preflight-' + self.phase + '.json')
        path.write_text(json.dumps(info, indent=2, ensure_ascii=False) + '\n')
        path.chmod(0o600)
        self.status['steps'].append({'step': 'preflight', 'status': info['status'],
                                     'evidence': str(path), 'started_at': utc(), 'finished_at': utc()})
        self.save()
        self.log(('PASS' if info['status'] == 'PASS' else 'FAIL') + ' preflight')
        if info['status'] != 'PASS':
            raise RuntimeError('preflight_not_ready')

    def idle(self):
        self.log('START idle-wait ' + str(self.idle_seconds) + 's')
        entry = {'step': 'idle-wait', 'started_at': utc(), 'requested_seconds': self.idle_seconds}
        end = time.monotonic() + self.idle_seconds
        while time.monotonic() < end:
            remaining = int(end - time.monotonic())
            time.sleep(min(60, max(1, remaining)))
            self.log('idle remaining ' + str(int(end - time.monotonic())) + 's')
        entry.update(status='PASS', finished_at=utc(), seconds=self.idle_seconds)
        self.status['steps'].append(entry)
        self.save()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--phase', required=True)
    parser.add_argument('--idle-seconds', type=int, default=900)
    args = parser.parse_args()
    if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,79}', args.phase):
        parser.error('invalid phase')
    if (ROOT / 'private/captures' / args.phase).exists():
        parser.error('capture phase exists; choose a fresh phase')
    window = Window(args.phase, args.idle_seconds)
    window.save()
    failed = None
    try:
        window.preflight()
        window.step('capture-start', 'round2-capture.py', '--phase', args.phase, '--execute',
                    sudo=True, timeout=120, expect='CAPTURING')
        window.step('sms-capture-r3-candidate', 'round2-sms.py', 'capture', '--label',
                    'r3-candidate', '--execute', timeout=600)
        window.step('sms-check-idle', 'round2-sms.py', 'check-idle', '--execute', timeout=120)
        window.step('r1-outgoing-95s', 'round2-calls.py', 'r1-outgoing-95s', '--execute', timeout=600)
        window.step('r1-incoming-95s', 'round2-calls.py', 'r1-incoming-95s', '--execute', timeout=600)
        window.step('r1-peer-hangup-10s', 'round2-calls.py', 'r1-peer-hangup-10s', '--execute', timeout=600)
        window.step('r2-incoming-partial-node-unreachable', 'multi-node.py',
                    'incoming-partial-node-unreachable', timeout=900)
        # The partial-node case deliberately drops B for the phone; let the
        # Gateway's mesh view of B settle before the failover case needs it.
        window.pause('settle-after-partial-node', 20)
        window.step('r2-idle-failover-hold-failback', 'multi-node.py',
                    'idle-failover-hold-failback', timeout=900)
        window.restart_app('gateway')
        window.restart_app('host')
        window.step('r5-sms-ui', 'r5-sms-ui.py', '--out', str(window.evidence / ('r5-sms-ui-' + args.phase)),
                    timeout=600)
        window.idle()
    except Exception as error:
        failed = type(error).__name__ + ':' + str(error)[:160]
        window.log('STOPPED ' + failed)
    finally:
        # Always bound the capture window, even after a failed step.
        try:
            window.run('capture-finalize', [sys.executable, str(SCRIPTS / 'final-audit.py'),
                                            '--root', str(ROOT), '--capture-phase', args.phase,
                                            '--output-name', args.phase + '-negative-audit.json'],
                       timeout=300, expect=None, sudo=True, evidence=True)
        except Exception as error:
            failed = failed or ('finalize:' + type(error).__name__)
        window.status.update(finished_at=utc())
        window.status['status'] = 'FAIL' if failed else 'PASS'
        if failed:
            window.status['failure'] = failed
        window.save()
    print(json.dumps({'status': window.status['status'], 'phase': args.phase,
                      'failure': failed, 'status_file': str(window.status_path)}, ensure_ascii=False))
    return 0 if window.status['status'] == 'PASS' else 1


if __name__ == '__main__':
    raise SystemExit(main())
