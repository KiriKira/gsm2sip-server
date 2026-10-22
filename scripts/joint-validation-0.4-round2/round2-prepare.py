#!/usr/bin/env python3
"""Prepare an isolated round-two lab tree; default prints a plan only.

Run this on the lab host only after final candidate APKs have been built.
Existing evidence/private files are read as inputs and never modified. Script
copies replace the fixed joint lab root, preserving original helper timeouts.
No Docker, ADB, HTTP, SSH, or SQL operation is performed by this preparer.
"""
from __future__ import annotations
import argparse
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import sys

LAB_ROOT = Path('/home/ubuntu/gsm-webrtc-validation/joint-0.4')
PRIVATE_INPUTS = ('a.env', 'b.env', 'c.env')
REFERENCE_INPUTS = ('baseline-identities.json', 'emulator-retained-sim-recovery.json')
FLAGS = {'RTC_MEDIA_ENABLED': 'true', 'RTC_CONTROL_ENABLED': 'true', 'SIP_COMPATIBILITY_ENABLED': 'false'}


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + '\n')
    path.chmod(0o600)


def prepare(args):
    source = args.lab_root.resolve()
    target = source / 'round2' / args.run_id
    if target.exists() or target.is_symlink():
        raise ValueError('run_directory_already_exists')
    baseline = args.baseline_builds.resolve()
    candidate = args.candidate_builds.resolve()
    required = [source / 'private' / name for name in PRIVATE_INPUTS[:4]]
    required += [source / 'evidence/baseline-identities.json', source / 'docker-compose',
                 source / 'b-override.yaml', source / 'deploy/compose.yaml']
    for builds in (baseline, candidate):
        required += [builds / role / (role + suffix) for role in ('gateway', 'host')
                     for suffix in ('-debug.apk', '-debug-androidTest.apk')]
    if any(not path.is_file() or path.is_symlink() for path in required):
        raise ValueError('required_input_missing_or_symlink')
    for name in ('http-fault.json', 'fault-state.json'):
        path = source / 'private' / name
        if path.is_symlink() or (path.exists() and json.loads(path.read_text()) != {}):
            raise ValueError('source_fault_state_not_clean')
    for name in PRIVATE_INPUTS:
        path = source / 'private' / name
        if path.exists() and (path.is_symlink() or path.stat().st_mode & 0o077):
            raise ValueError('private_input_permissions_unsafe')
    script_source = Path(__file__).resolve().parent
    script_paths = sorted(script_source.glob('*.py'))
    if not all((script_source / name).is_file() for name in ('round2-sync.py', 'round2-capture.py', 'round2-sms.py', 'final-audit.py')):
        raise ValueError('round2_helpers_incomplete')
    os.umask(0o077)
    target.mkdir(parents=True, mode=0o700)
    for name in ('scripts', 'private', 'evidence', 'builds', 'deploy'):
        (target / name).mkdir(mode=0o700)
    manifest = {'schema': 'gsm04-round2-preparation-v1', 'run_id': args.run_id,
                'created_at': dt.datetime.now(dt.timezone.utc).isoformat(),
                'source_root': str(source), 'run_root': str(target), 'status': 'PREPARED',
                'references': [], 'scripts': [], 'build_roots': {'baseline': str(baseline), 'candidate': str(candidate)},
                'server_flags': FLAGS, 'baseline_evidence_modified': False,
                'sms_baseline_policy': 'fresh_capture_in_this_run_then_normal_api_revoke_before_calls'}
    old_sms = source / 'private/sms-baseline-state.json'
    if old_sms.exists():
        if old_sms.is_symlink() or not old_sms.is_file():
            raise ValueError('old_sms_state_unsafe')
        old_state = json.loads(old_sms.read_text())
        manifest['first_round_sms_reference'] = {'path': str(old_sms), 'sha256': digest(old_sms),
            'cleanup_complete': old_state.get('cleanup_complete') is True,
            'phase': old_state.get('phase'), 'copied_as_live_state': False}
    for path in script_paths:
        text = path.read_text().replace(str(LAB_ROOT), str(target))
        # A local dry fixture can select a different source root; the deployed
        # source copies still contain the fixed production-lab literal above.
        destination = target / 'scripts' / path.name
        destination.write_text(text)
        destination.chmod(0o700)
        manifest['scripts'].append({'name': path.name, 'source_sha256': digest(path), 'sha256': digest(destination)})
    # Isolated case-5 SMS labels prevent an old label from being reused as a
    # supposed fresh second-round SMS probe. No original ledger is copied.
    path = target / 'scripts/sms-probe.py'
    text = path.read_text().replace('LABELS = ("server-baseline", "gateway-baseline", "host-baseline")',
                                   'LABELS = ("r3-candidate", "r4-baseline", "r4-candidate")')
    path.write_text(text)
    # R4 uses retained volumes and the established network layout, with all
    # three rollback flags explicit even if a source env file is incomplete.
    path = target / 'scripts/server-version.py'
    text = path.read_text().replace("docker(*args,'soulgem-aio:'+tag)",
        "docker(*args,'-e','RTC_MEDIA_ENABLED=true','-e','RTC_CONTROL_ENABLED=true','-e','SIP_COMPATIBILITY_ENABLED=false','soulgem-aio:'+tag)")
    if text == path.read_text():
        raise ValueError('server_switch_template_changed')
    text = text.replace("'-f',str(root/'b-override.yaml')]", "'-f',str(root/'b-override.yaml'),'-f',str(root/'b-round2-rtc.yaml')]")
    if "b-round2-rtc.yaml" not in text:
        raise ValueError('compose_switch_template_changed')
    path.write_text(text)
    (target / 'b-round2-rtc.yaml').write_text('services:\n  api:\n    environment:\n' + ''.join('      ' + key + ': "' + value + '"\n' for key, value in FLAGS.items()))
    for name in PRIVATE_INPUTS:
        path = source / 'private' / name
        if path.is_file():
            destination = target / 'private' / name
            shutil.copyfile(path, destination)
            destination.chmod(0o600)
    for name in ('a.env', 'b.env', 'c.env'):
        path = target / 'private' / name
        lines = [line for line in path.read_text().splitlines() if line.split('=', 1)[0] not in FLAGS]
        path.write_text('\n'.join(lines + [key + '=' + value for key, value in FLAGS.items()]) + '\n')
    for name in REFERENCE_INPUTS:
        path = source / 'evidence' / name
        if path.exists():
            if not path.is_file() or path.is_symlink():
                raise ValueError('identity_reference_unsafe')
            destination = target / 'evidence' / name
            shutil.copyfile(path, destination)
            destination.chmod(0o600)
            manifest['references'].append({'path': 'evidence/' + name, 'sha256': digest(path),
                                            'origin': 'first_round_reference_only'})
    for name in ('http-fault.json', 'fault-state.json'):
        write_json(target / 'private' / name, {})
    for stage, builds in (('baseline', baseline), ('candidate', candidate)):
        (target / 'builds' / stage).symlink_to(builds, target_is_directory=True)
    for name in ('docker-compose', 'b-override.yaml'):
        shutil.copyfile(source / name, target / name)
    (target / 'docker-compose').chmod(0o700)
    shutil.copyfile(source / 'deploy/compose.yaml', target / 'deploy/compose.yaml')
    # Preserve relative compose references without copying env/secrets into
    # evidence. The lab compose helper uses these only from its private tree.
    manifest['scripts'] = [{'name': row['name'], 'source_sha256': row['source_sha256'],
                            'sha256': digest(target / 'scripts' / row['name'])} for row in manifest['scripts']]
    write_json(target / 'evidence/round2-preparation.json', manifest)
    write_json(target / 'private/round2-marker.json', {'run_id': args.run_id, 'run_root': str(target)})
    return {'status': 'PREPARED', 'run_root': str(target), 'manifest': str(target / 'evidence/round2-preparation.json')}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--lab-root', type=Path, default=LAB_ROOT)
    parser.add_argument('--run-id', required=True)
    parser.add_argument('--baseline-builds', type=Path, required=True)
    parser.add_argument('--candidate-builds', type=Path, required=True)
    parser.add_argument('--execute', action='store_true')
    args = parser.parse_args()
    if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,79}', args.run_id) or args.run_id in ('.', '..'):
        parser.error('invalid run ID')
    if not args.execute:
        print(json.dumps({'status': 'PLAN_ONLY', 'run_root': str(args.lab_root / 'round2' / args.run_id),
                          'writes_remote_state': False, 'copies_private_credentials': True,
                          'requires_live_sms_baseline': False,
                          'creates_fresh_sms_baseline_later': True, 'server_flags': FLAGS,
                          'starts_tests': False, 'preserves_first_round': True}, sort_keys=True))
        return 0
    try:
        print(json.dumps(prepare(args), sort_keys=True))
    except (OSError, ValueError):
        # Inputs may contain credentials; never interpolate their contents.
        print(json.dumps({'status': 'FAIL', 'reason': 'preparation_failed_check_inputs_or_partial_run_tree'}))
        return 1
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
