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
    'pairing-claim-response.json': 'PairingResult',
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
print(f'OpenAPI and {len(fixtures) + 1} fixture files passed; {len(mutations)} invalid events rejected')
