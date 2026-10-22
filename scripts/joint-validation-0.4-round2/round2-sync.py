#!/usr/bin/env python3
"""R4: server -> Gateway -> Host rollback, then the same forward order.

Default is plan-only. No isolated component rollback/smoke is run. Shared
read-only identity/version/retention checks come from the case-5 library, but
its session-expiry workaround and single-component workflow are never called.
No SQL data mutation, uninstall, data clear, or product-timeout adjustment.
"""
from __future__ import annotations
import argparse
import datetime as dt
import fcntl
import importlib.util
import json
import os
import re
import subprocess
import sys
from pathlib import Path
import uuid

ORDER = ('server', 'gateway', 'host')
R3_PHASE = 'r3-all-candidate-final'


def plan():
    return {'status': 'PLAN_ONLY', 'writes_remote_state': False,
            'rollback_order': list(ORDER), 'forward_order': list(ORDER),
            'required_r3_phase': R3_PHASE,
            'required_r3_audit': R3_PHASE + '-negative-audit.json',
            'after_each_step': ['versions', 'pairing', 'SIM binding', 'Host node list', 'SMS baseline and Gateway archive retention'],
            'all_baseline': ['fresh SIP/ARI capture', 'one fresh outbound and inbound SMS, normal API revoke and inactive-session proof', 'one outgoing and incoming 12s call', 'flush/count/hash capture'],
            'all_candidate': ['one fresh outbound and inbound SMS, normal API revoke and inactive-session proof', 'one outgoing and incoming 12s call'],
            'sql_mutation': False, 'mixed_version_call_tests': False,
            'server_flags': {'RTC_MEDIA_ENABLED': 'true', 'RTC_CONTROL_ENABLED': 'true', 'SIP_COMPATIBILITY_ENABLED': 'false'},
            'failure': 'stop testing; confirm idle before attempting forward recovery in server/Gateway/Host order'}


def make_workflow(root, r3_phase=R3_PHASE, diagnose_baseline=False):
    spec = importlib.util.spec_from_file_location('round2_retention_library', root / 'scripts/rollback-cases.py')
    base = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(base)

    class Synchronized(base.Case5):
        def __init__(self):
            self.components = ORDER
            self.r3_phase = r3_phase
            self.diagnose_baseline = diagnose_baseline
            self.driver = base.load_module('round2_sync_driver', root / 'scripts/driver.py')
            self.core = base.load_module('round2_sync_core', root / 'scripts/core-calls.py')
            if self.driver.SERIALS != base.EXPECTED_SERIALS or self.driver.PACKAGES != base.EXPECTED_PACKAGES:
                raise base.Case5Error('unexpected_emulators_or_packages')
            self.run_id = dt.datetime.now(dt.timezone.utc).strftime('%Y%m%dT%H%M%SZ') + '-' + uuid.uuid4().hex[:8]
            self.lock_stream = (root / 'private/round2-sync.lock').open('a+')
            fcntl.flock(self.lock_stream.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
            self.logs = root / 'private' / ('r4-' + self.run_id)
            self.logs.mkdir(mode=0o700)
            self.report_path = root / 'evidence' / ('r4-synchronized-' + self.run_id + '.json')
            self.report = {'schema': 'gsm04-round2-sync-v1', 'case': 'R4', 'run_id': self.run_id,
                           'status': 'RUNNING', 'started_at': base.utc_now(), 'steps': [], 'snapshots': [],
                           'rollback_order': list(ORDER), 'forward_order': list(ORDER), 'smoke': [],
                           'sql_mutation_performed': False,
                           'sms_lifecycle': 'fresh_capture_normal_api_revoke_then_readonly_retention_before_calls'}
            self.step_number = 0
            self.baseline_identities, self.identity_reference = self.load_baseline_identities()
            self.round2_sms = base.load_module('round2_sync_binding', root / 'scripts/round2-sms.py')
            self.report['identity_reference'] = self.identity_reference
            self.save()

        def runtime_identity(self, require_idle=True):
            sms_base = self.round2_sms.load_base(root)
            binding = self.round2_sms.pinned_gateway_binding(sms_base, root)
            # Preserve the fixed Gateway/SIM identities. A normal cold start
            # may advance mapping_revision; read-only DB and READY must agree
            # and the revision must never go below the retained-SIM floor.
            expected = self.baseline_identities['gateway_identity']
            if any(binding[key] != expected[key] for key in ('gateway_id', 'sim_id')):
                raise base.Case5Error('round2_fixed_identity_changed')
            if binding['mapping_revision'] < expected['mapping_revision']:
                raise base.Case5Error('round2_mapping_revision_went_backwards')
            expected['mapping_revision'] = binding['mapping_revision']
            value = super().runtime_identity(require_idle=require_idle)
            value['mapping_revision_observation'] = binding['revision_observation']
            return value

        def privileged_child(self, name, script, args, timeout):
            command = ['sudo', '-n', sys.executable, str(root / 'scripts' / script), *args]
            entry = self.begin_step(name, command)
            log = self.logs / (str(self.step_number).zfill(2) + '-' + name + '.log')
            try:
                result = subprocess.run(command, cwd=root, stdin=subprocess.DEVNULL,
                                        stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                        text=True, timeout=timeout, check=False)
                log.write_text(result.stdout)
                log.chmod(0o600)
                payload = self.last_json(result.stdout)
                if result.returncode != 0:
                    raise base.Case5Error('privileged_' + script + '_failed')
                self.finish_step(entry, 'PASS', log=str(log), summary=payload)
                return payload
            except Exception:
                self.finish_step(entry, 'FAIL', log=str(log), reason='privileged_helper_failed')
                raise

        def retain(self, label, expected):
            self.snapshot(label, expected)
            payload = self.child('sms-retained-' + label, 'round2-sms.py',
                                 ['verify-all', '--execute'], timeout=180)
            if not isinstance(payload, dict) or payload.get('status') != 'PASS' or not payload.get('records'):
                raise base.Case5Error('round2_sms_retention_failed')

        def sms(self, label):
            payload = self.child('sms-fresh-' + label, 'round2-sms.py',
                                 ['capture', '--label', label, '--execute'], timeout=420)
            if not isinstance(payload, dict) or payload.get('status') != 'PASS' or len(payload.get('records', [])) != 1:
                raise base.Case5Error('round2_sms_capture_failed')
            record = payload['records'][0]
            if record.get('label') != label or record.get('fresh_pair_sent') is not True or record.get('live_session_count') != 0 or set(record.get('messages', {})) != {'inbound', 'outbound'}:
                raise base.Case5Error('round2_sms_capture_or_revoke_failed')
            return record

        def smoke(self, stage):
            label = 'r4-' + stage
            sms = self.sms(label)
            self.child('sms-sessions-inactive-before-' + stage + '-calls', 'round2-sms.py',
                       ['check-idle', '--execute'], timeout=60)
            self.save()
            calls = []
            for direction in ('outgoing', 'incoming'):
                payload = self.child('call-' + stage + '-' + direction, 'round2-calls.py',
                                     [label + '-' + direction, '--execute'], timeout=360)
                if (not isinstance(payload, dict) or payload.get('status') != 'PASS'
                        or float(payload.get('audio_seconds', 0)) < 12
                        or payload.get('host_terminal') != 'ENDED'
                        or payload.get('gateway_terminal') != 'completed'
                        or payload.get('actual_telecom_calls') != 1):
                    raise base.Case5Error('round2_call_assertions_failed')
                calls.append(base.safe_call_summary(payload))
            self.require_idle()
            self.report['smoke'].append({'stage': stage, 'sms': sms, 'calls': calls})
            self.save()
            self.retain('after-' + stage + '-smoke', {component: stage for component in ORDER})

        def assert_server_flags(self):
            expected = {'RTC_MEDIA_ENABLED': 'true', 'RTC_CONTROL_ENABLED': 'true', 'SIP_COMPATIBILITY_ENABLED': 'false'}
            for name in ('a.env', 'b.env', 'c.env'):
                lines = (root / 'private' / name).read_text().splitlines()
                values = dict(line.split('=', 1) for line in lines if '=' in line and not line.startswith('#'))
                if any(values.get(key) != value for key, value in expected.items()):
                    raise base.Case5Error('server_rollback_flags_missing')
            # The AIO switch must override these flags explicitly after env-file.
            text = (root / 'scripts/server-version.py').read_text()
            if any("'-e','" + key + '=' + value + "'" not in text for key, value in expected.items()):
                raise base.Case5Error('aio_rollback_flags_missing')
            if "b-round2-rtc.yaml" not in text or not (root / 'b-round2-rtc.yaml').is_file():
                raise base.Case5Error('compose_rollback_flags_missing')
            self.report['server_flags'] = expected
            self.save()

        def run(self):
            self.assert_server_flags()
            r3_capture = root / 'private/captures' / self.r3_phase
            r3_audit_path = root / 'evidence' / (self.r3_phase + '-negative-audit.json')
            try:
                r3_audit = json.loads(r3_audit_path.read_text())
                captures = r3_audit['captures']
                flushed = (
                    r3_capture.is_dir() and not r3_capture.is_symlink()
                    and r3_audit_path.is_file() and not r3_audit_path.is_symlink()
                    and r3_audit['capture_phase'] == self.r3_phase
                    and all(r3_audit['assertions'][key] is True for key in (
                        'capture_manifest_matches_three_bridges',
                        'actual_tcpdump_children_sigint_flushed',
                        'final_pcaps_read_and_hashed',
                    ))
                    and len(captures) == 3
                    and {capture['node'] for capture in captures} == {'a', 'b', 'c'}
                    and all(capture['signal_sent'] is True and capture['process_exited'] is True
                            and capture['wrapper_exited'] is True and capture['pcap_read_ok'] is True
                            and bool(capture['pcap_sha256']) for capture in captures)
                )
            except (OSError, ValueError, KeyError, TypeError):
                flushed = False
            if not flushed:
                raise base.Case5Error('finish_formal_r3_capture_before_r4_version_switch')
            self.report['r3_precondition'] = {'capture_phase': R3_PHASE, 'audit': str(r3_audit_path),
                                              'identity_flush_and_hash_assertions_verified': True}
            self.save()
            if (root / 'private/captures/r4-all-baseline').exists() or (root / 'evidence/r4-all-baseline-negative-audit.json').exists():
                raise base.Case5Error('r4_evidence_exists_use_fresh_run_tree')
            expected = {component: 'candidate' for component in ORDER}
            self.retain('initial-all-candidate', expected)
            started = False
            try:
                for stage in ('baseline', 'candidate'):
                    for component in ORDER:
                        self.require_idle()
                        started = True
                        self.change_component(component, stage)
                        expected[component] = stage
                        self.retain(stage + '-' + component, expected)
                    if stage == 'baseline':
                        self.privileged_child('capture-all-baseline-start', 'round2-capture.py',
                                   ['--phase', 'r4-all-baseline', '--execute'], timeout=30)
                        if self.diagnose_baseline:
                            # Characterise the previous round's "inbound SMS injection
                            # fails after rollback" while the lab is still all-baseline.
                            # A product verdict stops the rollback run; only a harness
                            # verdict may be followed by the smoke and the forward pass.
                            payload = self.privileged_child(
                                'r4-sms-injection-diagnose', 'r4-sms-injection-diagnose.py',
                                ['--out', str(root / 'evidence' / 'r4-sms-injection-diagnose.json')],
                                timeout=480)
                            verdict = str((payload or {}).get('verdict', ''))
                            self.report['baseline_sms_injection_diagnose'] = payload
                            self.save()
                            if verdict.startswith('product_'):
                                raise base.Case5Error('old_product_sms_injection_failure:' + verdict)
                        try:
                            self.smoke(stage)
                        finally:
                            self.privileged_child('capture-all-baseline-finalize', 'final-audit.py',
                                       ['--root', str(root), '--capture-phase', 'r4-all-baseline',
                                        '--output-name', 'r4-all-baseline-negative-audit.json'], timeout=240)
                    else:
                        self.smoke(stage)
                self.report.update(status='PASS', finished_at=base.utc_now(), final_stage='candidate')
                self.save()
                return self.report
            except Exception:
                if started:
                    self.recover_idle()
                    # Recovery also follows the published order. It performs
                    # no calls or SMS probes while the versions are mixed.
                    for component in ORDER:
                        self.change_component(component, 'candidate')
                    self.report['forward_recovery'] = self.snapshot('recovery-all-candidate', {c: 'candidate' for c in ORDER})
                    self.save()
                raise

    return Synchronized()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--execute', action='store_true')
    parser.add_argument('--r3-phase', default=R3_PHASE,
                        help='completed R3 capture phase this run must verify')
    parser.add_argument('--diagnose-baseline', action='store_true',
                        help='run r4-sms-injection-diagnose.py while all three components are rolled back')
    args = parser.parse_args()
    if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,79}', args.r3_phase):
        parser.error('invalid R3 capture phase')
    if not args.execute:
        print(json.dumps({**plan(), 'r3_phase': args.r3_phase}, sort_keys=True))
        return 0
    root = Path(__file__).resolve().parents[1]
    if not (root / 'private/round2-marker.json').is_file():
        parser.error('requires an isolated root created by round2-prepare.py')
    os.umask(0o077)
    workflow = None
    try:
        workflow = make_workflow(root, args.r3_phase, args.diagnose_baseline)
        result = workflow.run()
        print(json.dumps({'status': result['status'], 'report': str(workflow.report_path), 'final_stage': 'candidate'}, sort_keys=True))
    except Exception as failure:
        if workflow:
            workflow.report.update(status='FAIL', reason=type(failure).__name__, finished_at=dt.datetime.now(dt.timezone.utc).isoformat())
            workflow.save()
        print(json.dumps({'status': 'FAIL', 'reason': type(failure).__name__,
                          'report': str(workflow.report_path) if workflow else None}, sort_keys=True))
        return 1
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
