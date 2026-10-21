#!/usr/bin/env python3
"""Check the staged RTC contract; does not enable production capabilities."""
import copy
import hashlib
import json
from pathlib import Path
from jsonschema import Draft202012Validator

ROOT = Path(__file__).resolve().parents[1]
DIRECTORY = ROOT / "protocol/rtc-v1"
schema = json.loads((DIRECTORY / "command.schema.json").read_text())
Draft202012Validator.check_schema(schema)
validator = Draft202012Validator(schema)
fixtures = sorted((DIRECTORY / "fixtures").glob("*.json"))
assert len(fixtures) == 6, "Expected one fixture per command kind"
for path in fixtures:
    validator.validate(json.loads(path.read_text()))

candidate = json.loads((DIRECTORY / "fixtures/candidate.json").read_text())
mutations = [
    lambda c: c.update(version=2),
    lambda c: c.update(sender_id="another-device"),
    lambda c: c.update(VERSION=1),
    lambda c: c.update(owner_id="another-owner"),
    lambda c: c.update(generation=9),
    lambda c: c.update(revision=1),
    lambda c: c.pop("revision"),
    lambda c: c.update(ice=None),
    lambda c: c.update(sdp=""),
    lambda c: c["ice"].update(sdp_mline_index=None),
    lambda c: c["ice"].update(sdp_mline_index=16),
    lambda c: c["ice"].update(candidate="candidate:\nforged"),
    lambda c: c["ice"].update(candidate="candidate:1\n"),
    lambda c: c.update(operation_id="operation\n"),
    lambda c: c["ice"].update(candidate="x" * 2049),
]
for mutation in mutations:
    value = copy.deepcopy(candidate)
    mutation(value)
    assert not validator.is_valid(value), "Unsafe RTC fixture mutation was accepted"

# The two staged SDK-independent Android guards intentionally have identical
# source/tests. Check sibling repositories when they are available, and make a
# single-repository invocation's skipped cross-repository check explicit.
siblings = [ROOT.parent / name for name in ("gsm2sip-client-android", "gsm2sip")]
if all(path.is_dir() for path in siblings):
    for area, filename in (("main", "RemoteIceBuffer.kt"), ("test", "RemoteIceBufferTest.kt"),
                           ("main", "RtcCommandCodec.kt"), ("test", "RtcCommandCodecTest.kt")):
        files = [path / f"app/src/{area}/java/com/callagent/rtc/{filename}" for path in siblings]
        hashes = [hashlib.sha256(path.read_bytes()).digest() for path in files]
        assert hashes[0] == hashes[1], "Android RTC guards drifted between repositories"
    for fixture in fixtures:
        for sibling in siblings:
            copied = sibling / "app/src/test/resources/rtc-v1" / fixture.name
            assert copied.read_bytes() == fixture.read_bytes(), "Android RTC fixture drifted from server canonical input"
    print("Android RTC guard, codec, tests and canonical fixtures match across both repositories")
else:
    print("SKIP: sibling Android repositories unavailable; cross-repository source check not run")
print(f"RTC schema: {len(fixtures)} fixtures and {len(mutations)} invalid mutations passed")
