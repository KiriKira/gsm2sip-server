#!/usr/bin/env python3
"""Round-two SMS capture/revoke and read-only retention after session revocation.

Each label uses a fresh temporary client through the normal baseline capture
helper and revokes its session with POST /v1/auth/revoke before any call smoke.
Later retention reads server rows and Android SQLite without a client session.
Default prints a plan; --execute is required. Never updates SQL data.
"""
from __future__ import annotations
import argparse
import contextlib
import hashlib
import datetime as dt
import importlib.util
import io
import json
import os
from pathlib import Path
import uuid

LABELS = ('initial', 'r3-candidate', 'r4-baseline', 'r4-candidate')

# Explicit enum allowlist; never infer safety from exception text or a prefix.
SAFE_BASELINE_FAILURE_REASONS = frozenset({
    'adb_command_failed',
    'adb_snapshot_failed',
    'adb_unavailable',
    'api_capabilities_invalid',
    'api_request_rejected',
    'api_request_rejected_after_refresh',
    'api_unavailable_or_invalid',
    'baseline_capture_incomplete',
    'cleanup_requires_case6_complete',
    'cleanup_state_invalid',
    'database_identity_invalid',
    'database_mapping_invalid',
    'database_snapshot_empty',
    'database_snapshot_invalid',
    'docker_command_failed',
    'docker_unavailable',
    'enable_gateway_sms_archive_first',
    'expected_one_live_gateway',
    'gateway_archive_disabled_during_capture',
    'gateway_archive_missing_baseline',
    'gateway_archive_schema_invalid',
    'gateway_archive_schema_missing',
    'gateway_owner_ambiguous',
    'host_history_missing_baseline',
    'host_history_sync_timeout',
    'inbound_message_id_invalid',
    'local_history_schema_invalid',
    'message_list_invalid',
    'outbound_acceptance_invalid',
    'outbound_expiry_invalid',
    'outbound_sms_not_submitted',
    'outbound_sms_product_deadline',
    'pairing_claim_invalid',
    'pairing_code_creation_failed',
    'probe_auth_state_invalid',
    'probe_auth_state_permissions_unsafe',
    'probe_auth_state_symlink_refused',
    'probe_auth_state_unreadable',
    'refresh_response_invalid',
    'refusing_non_emulator_target',
    'required_emulator_offline',
    'server_inbound_sms_timeout',
    'server_message_invalid',
    'server_message_retention_mismatch',
    'server_outbound_sms_retention_mismatch',
    'sqlite_snapshot_unreadable',
    'state_already_cleaned',
    'state_already_exists',
    'state_invalid',
    'state_missing',
    'state_permissions_unsafe',
    'state_unreadable',
    'temporary_client_revoke_failed',
})
_BASELINE_ERROR_TYPES = set()



def load_base(root, label='initial'):
    spec = importlib.util.spec_from_file_location('round2_sms_baseline', root / 'scripts/sms-baseline.py')
    base = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(base)
    _BASELINE_ERROR_TYPES.add(base.BaselineError)
    # API and adb stay on the original lab; all writable helper state belongs
    # to this run and, except initial, to a separate label directory.
    private = root / 'private' if label == 'initial' else root / 'private/sms-cases' / label
    base.PRIVATE = private
    base.EVIDENCE = root / 'evidence'
    base.STATE_FILE = private / 'sms-baseline-state.json'
    base.PROBE_AUTH_FILE = private / 'sms-probe-client-auth.json'
    base.active_gateway_binding = lambda: pinned_gateway_binding(base, root)
    return base


def safe_failure_reason(failure):
    if isinstance(failure, tuple(_BASELINE_ERROR_TYPES)):
        code = failure.__dict__.get('code')
        if type(code) is str and code in SAFE_BASELINE_FAILURE_REASONS:
            return code
    return type(failure).__name__


def failure_result(root, action, label, failure):
    result = {'status': 'FAIL', 'action': action, 'label': label,
              'reason': safe_failure_reason(failure), 'error_type': type(failure).__name__}
    stamp = dt.datetime.now(dt.timezone.utc).strftime('%Y%m%dT%H%M%S%fZ')
    path = root / 'evidence' / ('round2-sms-failure-' + stamp + '-' + uuid.uuid4().hex[:8] + '.json')
    record = {'schema': 'gsm04-round2-sms-failure-v1', **result,
              'recorded_at': dt.datetime.now(dt.timezone.utc).isoformat(),
              'exception_text_written': False, 'raw_message_text_written': False}
    try:
        load_base(root).atomic_json(path, record)
        result['evidence'] = str(path)
    except Exception:
        # A filesystem/module failure must not expose either exception's text.
        result['evidence_write_failed'] = True
    return result


def pinned_identity(root):
    original_path = root / 'evidence/baseline-identities.json'
    recovery_path = root / 'evidence/emulator-retained-sim-recovery.json'
    def read_reference(path):
        if path.is_symlink() or not path.is_file():
            raise RuntimeError('pinned_identity_reference_missing_or_unsafe')
        return json.loads(path.read_text())
    original = read_reference(original_path).get('gateway_identity')
    if not isinstance(original, dict) or original.get('status') != 'READY':
        raise RuntimeError('pinned_gateway_identity_invalid')
    identity = {key: original.get(key) for key in ('gateway_id', 'sim_id', 'mapping_revision')}
    for key in ('gateway_id', 'sim_id'):
        if str(uuid.UUID(str(identity[key]))) != identity[key]:
            raise RuntimeError('pinned_gateway_uuid_invalid')
    if type(identity['mapping_revision']) is not int or identity['mapping_revision'] < 1:
        raise RuntimeError('pinned_mapping_revision_invalid')
    reference = original_path
    if recovery_path.exists() or recovery_path.is_symlink():
        recovery = read_reference(recovery_path)
        recovered = recovery.get('gateway_identity')
        if (recovery.get('status') != 'PASS'
                or recovery.get('reason') != 'explicit_same_virtual_modem_confirmation_after_fingerprintless_AVD_reboot'
                or not isinstance(recovered, dict) or recovered.get('status') != 'READY'
                or any(recovered.get(key) != identity[key] for key in ('gateway_id', 'sim_id'))
                or type(recovered.get('mapping_revision')) is not int
                or recovered['mapping_revision'] <= identity['mapping_revision']):
            raise RuntimeError('pinned_retained_sim_recovery_mismatch')
        identity['mapping_revision'] = recovered['mapping_revision']
        reference = recovery_path
    return {**identity, 'reference': reference.name,
            'reference_sha256': hashlib.sha256(reference.read_bytes()).hexdigest()}


def pinned_gateway_binding(base, root):
    identity = pinned_identity(root)
    rows = server_rows(base,
        "SELECT json_build_object('owner_id',d.owner_id,'gateway_id',g.device_id," +
        "'mapping_revision',g.mapping_revision,'sim_id',b.sim_id,'slot_index',b.slot_index)::text " +
        "FROM devices d JOIN gateways g ON g.device_id=d.id " +
        "JOIN sim_bindings b ON b.gateway_id=g.device_id AND b.owner_id=d.owner_id " +
        "WHERE d.role='gateway' AND d.state='active' AND b.state='active' AND b.identity_verified " +
        "AND g.device_id='" + identity['gateway_id'] + "'::uuid " +
        "AND b.sim_id='" + identity['sim_id'] + "'::uuid " +
        "AND g.mapping_revision=b.mapping_revision AND g.mapping_revision>=" + str(identity['mapping_revision']) +
        " AND g.last_seen_at>clock_timestamp()-interval '90 seconds'")
    if len(rows) != 1:
        raise RuntimeError('pinned_live_gateway_binding_missing_or_ambiguous')
    value = json.loads(rows[0])
    if (not isinstance(value, dict)
            or any(value.get(key) != identity[key] for key in ('gateway_id', 'sim_id'))
            or type(value.get('mapping_revision')) is not int
            or value['mapping_revision'] < identity['mapping_revision']
            or type(value.get('slot_index')) is not int or value['slot_index'] < 0
            or str(uuid.UUID(str(value.get('owner_id')))) != value.get('owner_id')):
        raise RuntimeError('pinned_gateway_binding_mismatch')
    previous_revision = identity['mapping_revision']
    for _label, state_path in known_states(root):
        previous = read_state(state_path)
        if any(previous.get(key) != identity[key] for key in ('gateway_id', 'sim_id')):
            raise RuntimeError('round2_sms_history_identity_changed')
        if type(previous.get('mapping_revision')) is not int:
            raise RuntimeError('round2_sms_history_revision_invalid')
        previous_revision = max(previous_revision, previous['mapping_revision'])
    if value['mapping_revision'] < previous_revision:
        raise RuntimeError('observed_mapping_revision_went_backwards')
    ready = json.loads(base.adb_text(base.GATEWAY_SERIAL, 'exec-out', 'run-as',
                                    base.GATEWAY_PACKAGE, 'cat', 'files/joint-ready.json'))
    if (not isinstance(ready, dict) or ready.get('status') != 'READY'
            or any(ready.get(key) != value[key] for key in ('gateway_id', 'sim_id'))
            or type(ready.get('mapping_revision')) is not int
            or ready['mapping_revision'] != value['mapping_revision']):
        raise RuntimeError('joint_ready_and_pinned_binding_disagree')
    observation = {'gateway_id': value['gateway_id'], 'sim_id': value['sim_id'],
        'minimum_mapping_revision': identity['mapping_revision'],
        'observed_mapping_revision': value['mapping_revision'],
        'maximum_previous_mapping_revision': previous_revision,
        'reference': identity['reference'], 'reference_sha256': identity['reference_sha256'],
        'revision_sources': ['gateway joint-ready.json', 'read-only active sim_bindings/gateways SELECT'],
        'joint_ready': {key: ready[key] for key in ('status', 'gateway_id', 'sim_id', 'mapping_revision')},
        'gateway_serial': base.GATEWAY_SERIAL, 'active_verified_binding': True}
    base.ROUND2_BINDING_OBSERVATION = observation
    value['revision_observation'] = observation
    return value


def read_state(path):
    if not path.is_file() or path.is_symlink() or path.stat().st_mode & 0o077:
        raise RuntimeError('unsafe_sms_state')
    state = json.loads(path.read_text())
    if state.get('schema') != 'gsm04-sms-baseline-v1':
        raise RuntimeError('invalid_sms_state')
    for key in ('client_device_id', 'gateway_id', 'sim_id', 'owner_id'):
        if str(uuid.UUID(str(state.get(key)))) != state.get(key):
            raise RuntimeError('invalid_sms_identity')
    return state


def server_rows(base, query):
    # Only static SELECT statements with validated UUID literals reach here.
    if not query.lstrip().upper().startswith('SELECT ') or ';' in query:
        raise RuntimeError('readonly_query_required')
    output = base.docker_output('exec', '--user', 'gsm2sip', 'gsm-rtc-joint',
        '/usr/lib/postgresql/17/bin/psql', '-X', '-q', '-h', '/run/postgresql',
        '-p', '25432', '-U', 'gsm2sip', '-d', 'gsm2sip', '-At', '-v', 'ON_ERROR_STOP=1',
        '-c', 'BEGIN READ ONLY; ' + query + '; COMMIT;')
    return [line for line in output.splitlines() if line.strip()]


def live_session_count(base, client_id):
    client_id = str(uuid.UUID(client_id))
    rows = server_rows(base, "SELECT count(*) FROM sessions WHERE device_id='" + client_id +
                       "'::uuid AND (access_expires_at>clock_timestamp() OR refresh_expires_at>clock_timestamp())")
    if len(rows) != 1:
        raise RuntimeError('session_count_unreadable')
    return int(rows[0])


def revoke(base, state):
    if not state.get('cleanup_complete'):
        auth, auth_path = base.active_auth_state(state)
        if auth.get('pending_refresh_key'):
            base.refresh_session(auth, auth_path)
        token = auth.get('refresh_token')
        if not isinstance(token, str) or not token:
            raise RuntimeError('temporary_refresh_token_missing')
        base.api_json('POST', '/v1/auth/revoke', body={'refresh_token': token})
    count = live_session_count(base, state['client_device_id'])
    if count != 0:
        raise RuntimeError('temporary_client_still_has_live_session')
    state.pop('access_token', None)
    state.pop('refresh_token', None)
    state.pop('pending_refresh_key', None)
    state.update(cleanup_complete=True, phase='cleanup_complete', cleanup_at=dt.datetime.now(dt.timezone.utc).isoformat(),
                 cleanup_method='POST /v1/auth/revoke', live_session_count_after_revoke=0)
    base.atomic_json(base.STATE_FILE, state)
    if base.PROBE_AUTH_FILE.exists():
        base.PROBE_AUTH_FILE.unlink()
    return state


def known_states(root):
    paths = []
    if (root / 'private/sms-baseline-state.json').exists():
        paths.append(('initial', root / 'private/sms-baseline-state.json'))
    for label in LABELS[1:]:
        path = root / 'private/sms-cases' / label / 'sms-baseline-state.json'
        if path.exists():
            paths.append((label, path))
    return paths


def assert_no_live_clients(root):
    checked = []
    for label, path in known_states(root):
        base = load_base(root, label)
        state = read_state(path)
        if not state.get('cleanup_complete') or live_session_count(base, state['client_device_id']) != 0:
            raise RuntimeError('revoke_temporary_sms_client_before_call_smoke')
        checked.append({'label': label, 'client_device_id': state['client_device_id'], 'live_session_count': 0})
    return checked


def assert_owner_rtc_ready(root):
    owners = {read_state(path)['owner_id'] for _label, path in known_states(root)}
    checked = []
    base = load_base(root)
    if not owners:
        owners.add(base.active_gateway_binding()['owner_id'])
    for owner in sorted(owners):
        rows = server_rows(base,
            "SELECT json_build_object('live_client_count',count(*),'incompatible_client_count'," +
            "count(*) FILTER (WHERE NOT COALESCE(c.enabled AND c.last_seen_at>clock_timestamp()-interval '90 seconds',false)))::text " +
            "FROM devices d LEFT JOIN rtc_control_capabilities c ON c.device_id=d.id " +
            "WHERE d.owner_id='" + owner + "'::uuid AND d.role='client' AND d.state='active' " +
            "AND EXISTS(SELECT 1 FROM sessions ss WHERE ss.device_id=d.id AND ss.refresh_expires_at>clock_timestamp())")
        if len(rows) != 1:
            raise RuntimeError('owner_rtc_policy_unreadable')
        value = json.loads(rows[0])
        if value.get('live_client_count', 0) < 1 or value.get('incompatible_client_count') != 0:
            raise RuntimeError('owner_rtc_policy_has_incompatible_live_client')
        checked.append({'owner_id': owner, **value})
    return checked


def retain_one(root, label, path):
    base = load_base(root, label)
    state = read_state(path)
    expected = state.get('messages', {})
    if not state.get('cleanup_complete') or set(expected) != {'outbound', 'inbound'}:
        raise RuntimeError('completed_revoked_sms_pair_required')
    if live_session_count(base, state['client_device_id']) != 0:
        raise RuntimeError('temporary_sms_client_live')
    # Body text is captured privately in memory and hashed; no raw SQL output
    # or SMS text is emitted to evidence, terminal, or subprocess log files.
    for direction in ('outbound', 'inbound'):
        message_id = str(uuid.UUID(expected[direction]['message_id']))
        rows = server_rows(base,
            "SELECT json_build_object('owner_id',owner_id,'gateway_id',gateway_id,'sim_id',sim_id," +
            "'direction',direction,'body',body,'status',status)::text FROM messages WHERE id='" + message_id + "'::uuid")
        if len(rows) != 1:
            raise RuntimeError('server_sms_missing_or_duplicate')
        detail = json.loads(rows[0])
        if (any(str(detail.get(key)) != state[key] for key in ('owner_id', 'gateway_id', 'sim_id'))
                or detail.get('direction') != direction
                or base.sha256_text(str(detail.get('body', ''))) != expected[direction]['sha256']):
            raise RuntimeError('server_sms_retention_mismatch')
        if detail.get('status') not in (('submitted', 'delivered') if direction == 'outbound' else ('received',)):
            raise RuntimeError('server_sms_status_invalid')
        expected[direction]['server_status'] = detail['status']
    base.check_emulator(base.GATEWAY_SERIAL)
    base.check_emulator(base.HOST_SERIAL)
    base.assert_history(base.snapshot_databases(base.GATEWAY_SERIAL, base.GATEWAY_PACKAGE), expected, gateway=True)
    base.assert_history(base.snapshot_databases(base.HOST_SERIAL, base.HOST_PACKAGE), expected, gateway=False)
    return {'label': label, 'client_device_id': state['client_device_id'], 'gateway_id': state['gateway_id'],
            'sim_id': state['sim_id'], 'messages': expected, 'live_session_count': 0,
            'verification': 'read_only_server_select_and_android_sqlite'}


def evidence(root, action, records):
    stamp = dt.datetime.now(dt.timezone.utc).strftime('%Y%m%dT%H%M%S%fZ')
    path = root / 'evidence' / ('round2-sms-' + action + '-' + stamp + '.json')
    result = {'schema': 'gsm04-round2-sms-v1', 'status': 'PASS', 'action': action,
              'recorded_at': dt.datetime.now(dt.timezone.utc).isoformat(), 'records': records,
              'sql_data_mutation': False, 'raw_message_text_written': False}
    load_base(root).atomic_json(path, result)
    return {'status': 'PASS', 'phase': action, 'evidence': str(path), 'records': records}


def capture_pair(root, label):
    base = load_base(root, label)
    if base.STATE_FILE.exists():
        raise RuntimeError('sms_label_exists_no_resend')
    # Also guard callers which captured an initial baseline directly with
    # sms-baseline.py capture but have not yet revoked its temporary session.
    assert_no_live_clients(root)
    try:
        with contextlib.redirect_stdout(io.StringIO()):
            base.capture()
    except Exception as failure:
        base.save_capture_failure_evidence(safe_failure_reason(failure))
        raise
    finally:
        if base.STATE_FILE.exists():
            state = read_state(base.STATE_FILE)
            revoke(base, state)
    record = retain_one(root, label, base.STATE_FILE)
    record['owner_rtc_capability_after_revoke'] = assert_owner_rtc_ready(root)
    record.update(fresh_pair_sent=True, session_revoke_method='POST /v1/auth/revoke',
                  binding_reference=pinned_identity(root),
                  binding_observation=base.ROUND2_BINDING_OBSERVATION)
    return record


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('capture', 'revoke', 'verify-all', 'check-idle'))
    parser.add_argument('--label', choices=LABELS, default='initial')
    parser.add_argument('--execute', action='store_true')
    args = parser.parse_args()
    if not args.execute:
        print(json.dumps({'status': 'PLAN_ONLY', 'action': args.action, 'label': args.label,
                          'capture': 'sms-baseline.py capture in isolated state directory',
                          'revoke': 'POST /v1/auth/revoke then SELECT live_session_count=0',
                          'retention': 'read-only server and Android SQLite', 'sql_data_mutation': False}, sort_keys=True))
        return 0
    root = Path(__file__).resolve().parents[1]
    if not (root / 'private/round2-marker.json').is_file():
        parser.error('requires isolated round2 root')
    os.umask(0o077)
    try:
        if args.action == 'capture':
            records = [capture_pair(root, args.label)]
        elif args.action == 'revoke':
            base = load_base(root, args.label)
            revoke(base, read_state(base.STATE_FILE))
            records = assert_no_live_clients(root)
        elif args.action == 'check-idle':
            records = assert_no_live_clients(root)
            records.append({'owner_rtc_capability': assert_owner_rtc_ready(root)})
        else:
            paths = known_states(root)
            if not paths:
                raise RuntimeError('round2_sms_baseline_missing')
            records = [retain_one(root, label, path) for label, path in paths]
        print(json.dumps(evidence(root, args.action, records), sort_keys=True))
    except Exception as failure:
        print(json.dumps(failure_result(root, args.action, args.label, failure), sort_keys=True))
        return 1
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
