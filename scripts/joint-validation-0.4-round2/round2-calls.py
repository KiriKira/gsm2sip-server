#!/usr/bin/env python3
"""Round-two calls using the existing real Telecom/RTC/PCM assertions.

Default prints the fixed case plan. --execute requires an isolated run tree.
R1 contains no mute or DTMF extension. R4 uses the same quick-call assertions.
"""
from __future__ import annotations
import argparse
import importlib.util
import json
from pathlib import Path

MODES = {
    'r1-outgoing-95s': ('outgoing', 95, 'host'),
    'r1-incoming-95s': ('incoming', 95, 'gateway'),
    'r1-peer-hangup-10s': ('incoming', 10, 'modem'),
    'r4-baseline-outgoing': ('outgoing', 12, 'host'),
    'r4-baseline-incoming': ('incoming', 12, 'gateway'),
    'r4-candidate-outgoing': ('outgoing', 12, 'host'),
    'r4-candidate-incoming': ('incoming', 12, 'gateway'),
}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('mode', choices=MODES)
    parser.add_argument('--execute', action='store_true')
    args = parser.parse_args()
    direction, seconds, hangup = MODES[args.mode]
    if not args.execute:
        print(json.dumps({'status': 'PLAN_ONLY', 'mode': args.mode, 'direction': direction,
                          'audio_seconds': seconds, 'hangup_origin': hangup,
                          'controls': [], 'writes_remote_state': False}, sort_keys=True))
        return 0
    root = Path(__file__).resolve().parents[1]
    if not (root / 'private/round2-marker.json').is_file():
        parser.error('requires an isolated root created by round2-prepare.py')
    if (root / 'evidence' / (args.mode + '.json')).exists():
        parser.error('evidence exists; preserve it and use a fresh isolated run tree')
    spec = importlib.util.spec_from_file_location('round2_core', root / 'scripts/core-calls.py')
    core = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(core)
    sms_spec = importlib.util.spec_from_file_location('round2_call_sms_guard', root / 'scripts/round2-sms.py')
    sms_guard = importlib.util.module_from_spec(sms_spec)
    sms_spec.loader.exec_module(sms_guard)
    try:
        inactive_clients = sms_guard.assert_no_live_clients(root)
        owner_rtc = sms_guard.assert_owner_rtc_ready(root)
    except Exception as failure:
        print(json.dumps({'status': 'FAIL', 'mode': args.mode, 'reason': 'sms_session_or_owner_rtc_precondition_failed'}, sort_keys=True))
        return 1
    case = core.Case(args.mode)
    case.result['temporary_sms_clients_inactive_before_call'] = inactive_clients
    case.result['owner_rtc_capability_before_call'] = owner_rtc
    try:
        if direction == 'outgoing':
            case.outgoing()
        else:
            case.incoming()
            core.cmd('host', 'answer')
            case.connected()
        case.tones(seconds)
        case.end(hangup)
        case.result['host_terminal'] = case.result['samples'][-1]['host']['phase']
        case.result['gateway_terminal'] = 'completed' if case.result['gateway_ledger']['rows'][0][1] == 'completed' else 'invalid'
        case.result['gateway_bridge'] = case.result['samples'][-1]['gateway']['bridge']
        case.result['gateway_telecom_state'] = case.result['samples'][-1]['gateway']['telecom_state']
        case.pass_()
    except Exception as failure:
        case.result.update(status='FAIL', error=type(failure).__name__)
        case.save()
        print(json.dumps({'status': 'FAIL', 'mode': args.mode, 'reason': type(failure).__name__}, sort_keys=True))
        return 1
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
