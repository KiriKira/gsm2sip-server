#!/usr/bin/env python3
"""Validate protocol fixtures against OpenAPI and immutable command hashes."""
from pathlib import Path
import hashlib
import copy
import json
import yaml
from jsonschema import Draft202012Validator, FormatChecker
from referencing import Registry, Resource
from openapi_spec_validator import validate

ROOT = Path(__file__).resolve().parents[1]
spec = yaml.safe_load((ROOT / 'openapi/openapi.yaml').read_text())
validate(spec)
registry = Registry().with_resource('urn:gsm2sip:openapi', Resource.from_contents(
    dict(spec, **{'$schema': 'https://json-schema.org/draft/2020-12/schema'})))
fixtures = {
    'rtc-control-outgoing.json': 'RTCOutgoingCall',
    'rtc-control-incoming.json': 'RTCIncomingCall',
    'rtc-control-incoming-remaining.json': 'RTCIncomingCall',
    'reception-config.json': 'ReceptionConfig',
    'reception-update.json': 'ReceptionUpdate',
    'wake-sync-required-domains.json': 'WakeSyncRequired',
    'rtc-control-operation.json': 'RTCControlOperation',
    'rtc-control-snapshot.json': 'RTCControlSnapshot',
    'wake-sync-required.json': 'WakeSyncRequired',
    'capabilities-response.json': 'Capabilities',
    'client-event-receipt-response.json': 'ClientEventReceipt',
    'call-ringing-response.json': 'RemoteCall',
    'pairing-claim-response.json': 'PairingResult',
    'pairing-claim-request-windows.json': 'PairingClaim',
    'pairing-claim-request-blank-platform.json': 'PairingClaim',
    'clients-list-response.json': 'ClientList',
    'call-answered-elsewhere.json': 'RemoteCall',
    'heartbeat-request.json': 'HeartbeatRequest',
    'heartbeat-response-invalidated.json': 'HeartbeatResult',
    'sim-bindings-response.json': 'SimBindingResult',
    'gateway-list-response.json': 'GatewayList',
    'sim-list-response.json': 'SimList',
    'message-detail-queued.json': 'MessageDetail',
    'events-page-response.json': 'EventPage',
    'gateway-event-dispatching.json': 'GatewayEvent',
    'gateway-event-expired.json': 'GatewayEvent',
}
for filename, schema_name in fixtures.items():
    schema = {'$ref': f'urn:gsm2sip:openapi#/components/schemas/{schema_name}'}
    Draft202012Validator(schema, registry=registry, format_checker=FormatChecker()).validate(
        json.loads((ROOT / 'fixtures/api' / filename).read_text()))
wake_validator = Draft202012Validator(
    {'$ref': 'urn:gsm2sip:openapi#/components/schemas/WakeSyncRequired'},
    registry=registry, format_checker=FormatChecker())
wake_frame = json.loads((ROOT / 'fixtures/api/wake-sync-required.json').read_text())
for invalid_wake in (
    {**wake_frame, 'protocol_version': 2},
    {**wake_frame, 'type': 'call_ready'},
    {**wake_frame, 'owner_id': 'must-not-be-present'},
    {**wake_frame, 'domains': []},
    {**wake_frame, 'domains': ['call', 'call']},
    {**wake_frame, 'domains': ['history']},
):
    assert not wake_validator.is_valid(invalid_wake), f'Unsafe wake frame accepted: {invalid_wake}'
outgoing_validator = Draft202012Validator(
    {'$ref': 'urn:gsm2sip:openapi#/components/schemas/RTCOutgoingCall'},
    registry=registry, format_checker=FormatChecker())
outgoing = json.loads((ROOT / 'fixtures/api/rtc-control-outgoing.json').read_text())
assert outgoing_validator.is_valid(outgoing), 'Outgoing fixture rejected'
assert not outgoing_validator.is_valid({**outgoing, 'remaining_ring_ms': 1000}), 'Outgoing call accepted a ring window'
incoming_validator = Draft202012Validator(
    {'$ref': 'urn:gsm2sip:openapi#/components/schemas/RTCIncomingCall'},
    registry=registry, format_checker=FormatChecker())
incoming = json.loads((ROOT / 'fixtures/api/rtc-control-incoming-remaining.json').read_text())
for remaining in (0, 60001, '21400'):
    assert not incoming_validator.is_valid({**incoming, 'remaining_ring_ms': remaining}), f'Bad ring window accepted: {remaining}'
command_page = json.loads((ROOT / 'fixtures/api/command-list-response.json').read_text())
Draft202012Validator(
    {'$ref': 'urn:gsm2sip:openapi#/paths/~1gateways~1{gateway_id}~1commands/get/responses/200/content/application~1json/schema'},
    registry=registry, format_checker=FormatChecker()).validate(command_page)
for command in command_page['commands']:
    Draft202012Validator({'$ref': 'urn:gsm2sip:openapi#/components/schemas/Command'},
                         registry=registry, format_checker=FormatChecker()).validate(command)
    names = ['command_id', 'message_id', 'gateway_id', 'sim_id', 'mapping_revision',
             'to', 'text', 'expires_at']
    digest = hashlib.sha256('\0'.join(str(command[n]) for n in names).encode()).hexdigest()
    assert digest == command['payload_sha256'], 'Command fixture payload hash mismatch'
event_validator = Draft202012Validator(
    {'$ref': 'urn:gsm2sip:openapi#/components/schemas/GatewayEvent'},
    registry=registry, format_checker=FormatChecker())
base_event = json.loads((ROOT / 'fixtures/api/gateway-event-dispatching.json').read_text())
mutations = [
    lambda e: e.update(protocol_version=2),
    lambda e: e.update(sequence=0),
    lambda e: e['payload'].pop('part_count'),
    lambda e: e['payload'].update(part_count=0),
    lambda e: e['payload'].update(part_count='2'),
    lambda e: e['payload'].update(state='delivered'),
    lambda e: e.update(sim_id=None),
    lambda e: e.update(mapping_revision=None),
]
for mutate in mutations:
    event = copy.deepcopy(base_event)
    mutate(event)
    assert not event_validator.is_valid(event), f'Unsafe event accepted: {event}'
claim_validator = Draft202012Validator(
    {'$ref': 'urn:gsm2sip:openapi#/components/schemas/PairingClaim'},
    registry=registry, format_checker=FormatChecker())
claim = json.loads((ROOT / 'fixtures/api/pairing-claim-request-windows.json').read_text())
for value in ('Windows', 'windows!', 'x' * 33):
    invalid_claim = {**claim, 'platform': value}
    assert not claim_validator.is_valid(invalid_claim), f'Invalid platform accepted: {value!r}'
client_validator = Draft202012Validator(
    {'$ref': 'urn:gsm2sip:openapi#/components/schemas/ClientSummary'},
    registry=registry, format_checker=FormatChecker())
clients = json.loads((ROOT / 'fixtures/api/clients-list-response.json').read_text())['items']
base_client = clients[0]
for invalid_client in (
    {**base_client, 'platform': 'Windows'},
    {**base_client, 'state': 'online'},
    {**base_client, 'access_token': 'must-not-be-listed'},
):
    assert not client_validator.is_valid(invalid_client), f'Unsafe client row accepted: {invalid_client}'
heartbeat_validator = Draft202012Validator(
    {'$ref': 'urn:gsm2sip:openapi#/components/schemas/HeartbeatRequest'},
    registry=registry, format_checker=FormatChecker())
heartbeat = json.loads((ROOT / 'fixtures/api/heartbeat-request.json').read_text())
radio_mutations = [
    lambda r: r.update(signal_level=5),
    lambda r: r.update(network_type='LTE<script>'),
    lambda r: r.update(network_operator=''),
    lambda r: r.update(iccid='must-not-be-sent'),
    lambda r: r.update(country_iso='cn'),
]
for mutate in radio_mutations:
    invalid_heartbeat = copy.deepcopy(heartbeat)
    mutate(invalid_heartbeat['sim_states'][0]['radio'])
    assert not heartbeat_validator.is_valid(invalid_heartbeat), f'Unsafe radio accepted: {invalid_heartbeat}'
print(
    f'OpenAPI and {len(fixtures) + 1} fixture files passed; '
    f'{len(mutations)} invalid events, 3 invalid platform labels, 3 unsafe client rows and '
    f'{len(radio_mutations)} unsafe SIM radio reports rejected'
)

# Call control must not accept a trusted identity override or a SIP URI.
for fixture, schema_name, mutations in [
    ('rtc-control-outgoing.json','RTCOutgoingCall', [lambda x:x.update(version=2),lambda x:x.update(mapping_revision=0),lambda x:x.update(owner_id='other'),lambda x:x.update(sip_uri='sip:forged'),lambda x:x.update(**{'from':'forged'}),lambda x:x.update(to='caller\nforged')]),
    ('rtc-control-operation.json','RTCControlOperation',[lambda x:x.update(revision=0),lambda x:x.update(kind='dial_modem'),lambda x:x.update(winner_client_id='override'),lambda x:x.pop('operation_id')]),
]:
    validator=Draft202012Validator({'$ref':'urn:gsm2sip:openapi#/components/schemas/'+schema_name},registry=registry,format_checker=FormatChecker())
    for mutation in mutations:
        value=json.loads((ROOT/'fixtures/api'/fixture).read_text());mutation(value)
        assert not validator.is_valid(value), 'Unsafe independent control mutation accepted: '+str(value)
print('Independent RTC control fixtures and forbidden mutations passed')
