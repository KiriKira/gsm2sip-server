#!/usr/bin/env python3
"""Run real local protocol peers and pinned coturn; never touch production services."""
import base64
from collections import Counter
import hashlib
import hmac
import ipaddress
import json
import os
from pathlib import Path
import secrets
import shutil
import socket
import subprocess
import sys
import tempfile
import time

ROOT = Path(__file__).resolve().parent
DOCKER = ["docker", "--host=unix:///var/run/docker.sock"]
ENV = {key: value for key, value in os.environ.items() if key not in
       {"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_TLS", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"}}
REQUIRED_TESTS = {
    "TestDirectWebRTCOpusOverDTLSSRTP", "TestTURNRelayTransports", "TestTURNRelayTransports/UDP",
    "TestTURNRelayTransports/TCP", "TestTURNRelayTransports/TLS",
    "TestTURNRejectsInvalidAndExpiredCredentials", "TestTURNRejectsInvalidAndExpiredCredentials/wrong_password",
    "TestTURNRejectsInvalidAndExpiredCredentials/expired_REST_credential",
    "TestTURNRejectsUntrustedTLSCertificate", "TestSTUNBindingReturnsCorrelatedMappedAddress",
    "TestTURNRejectsTLSHostnameMismatch", "TestDTLSRejectsMismatchedFingerprint",
    "TestWSSNegotiationAndICERestartKeepTheSameBusinessCall",
}


def evaluate_tests(output, returncode):
    records = [json.loads(line) for line in output.splitlines() if line.strip()]
    counts = Counter(item["Action"] for item in records if "Test" in item and item["Action"] in ("pass", "fail", "skip"))
    passed = {item["Test"] for item in records if item["Action"] == "pass" and "Test" in item}
    if returncode != 0 or any(item["Action"] in ("fail", "skip") for item in records) or not REQUIRED_TESTS.issubset(passed):
        raise RuntimeError("protocol_suite_failed_or_incomplete")
    return records, counts


def command(args, **kwargs):
    try:
        return subprocess.run(args, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                              text=True, env=ENV, timeout=120, **kwargs).stdout
    except subprocess.CalledProcessError as failure:
        # These setup commands contain no credential/key material in arguments.
        # Server logs and the private configuration are never printed here.
        print("Setup command failed: " + args[0] + " " + args[1], file=sys.stderr)
        print(failure.stderr[-2000:], file=sys.stderr)
        raise


def certificates(directory, address):
    directory.mkdir()
    # Ephemeral test-only keys, never copied into repository configuration.
    command(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
             "-subj", "/CN=Gsm2Sip local protocol CA", "-keyout", str(directory / "ca.key"),
             "-out", str(directory / "ca.pem")])
    command(["openssl", "req", "-newkey", "rsa:2048", "-nodes", "-subj", "/CN=local-turn-probe",
             "-keyout", str(directory / "server.key"), "-out", str(directory / "server.csr")])
    extension = directory / "extensions.cnf"
    extension.write_text(f"subjectAltName=IP:{address}\nextendedKeyUsage=serverAuth\n")
    command(["openssl", "x509", "-req", "-in", str(directory / "server.csr"), "-CA", str(directory / "ca.pem"),
             "-CAkey", str(directory / "ca.key"), "-CAcreateserial", "-days", "1", "-extfile", str(extension),
             "-out", str(directory / "server.pem")])


def credential(secret, expiry):
    username = f"{expiry}:local-probe"
    password = base64.b64encode(hmac.new(secret.encode(), username.encode(), hashlib.sha1).digest()).decode()
    return username, password


def wait_port(address):
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        try:
            with socket.create_connection((address, 5349), timeout=0.3):
                return
        except OSError:
            time.sleep(0.1)
    raise RuntimeError("isolated_turn_listener_did_not_start")


def main():
    output = ROOT / "artifacts"
    output.mkdir(exist_ok=True)
    result = {"status": "FAIL", "scope": "local_headless_protocols", "android_native": "NOT_RUN",
              "cellular_audio": "NOT_RUN", "public_network_matrix": "NOT_RUN"}
    containers = []
    network = "gsm2sip-rtc-protocol-" + secrets.token_hex(5)
    network_created = False
    exit_code = 1
    private = Path(tempfile.mkdtemp(prefix="gsm2sip-rtc-protocol-", dir="/tmp"))
    try:
        pin = json.loads((ROOT / "pin.json").read_text())
        result["pins"] = pin
        for name, expected in pin["fixtures"].items():
            if hashlib.sha256((ROOT / "testdata" / name).read_bytes()).hexdigest() != expected:
                raise RuntimeError("pinned_Opus_fixture_checksum_mismatch")
        command(DOCKER + ["info", "--format", "{{.ServerVersion}}"])
        command(DOCKER + ["pull", pin["coturn_image"]])
        # Build with the normal trusted proxy roots before changing roots for
        # the test binary. go.sum remains enforced; no insecure TLS switches.
        command(["go", "test", "-c", "-race", "-o", str(output / "protocols.test")], cwd=ROOT)
        existing_ids = command(DOCKER + ["network", "ls", "-q"]).split()
        existing = json.loads(command(DOCKER + ["network", "inspect"] + existing_ids))
        used = [ipaddress.ip_network(value["Subnet"]) for item in existing
                for value in (item.get("IPAM", {}).get("Config") or []) if value.get("Subnet")]
        subnet = next((ipaddress.ip_network(f"10.253.{index}.0/28") for index in range(1, 255)
                       if not any(ipaddress.ip_network(f"10.253.{index}.0/28").overlaps(other)
                                  for other in used if other.version == 4)), None)
        if subnet is None:
            raise RuntimeError("no_unused_local_test_subnet")
        command(DOCKER + ["network", "create", "--internal", "--subnet", str(subnet), network])
        network_created = True
        settings = json.loads(command(DOCKER + ["network", "inspect", network]))[0]
        subnet = ipaddress.ip_network(settings["IPAM"]["Config"][0]["Subnet"])
        addresses = [str(subnet.network_address + i) for i in (2, 3)]
        secret = secrets.token_urlsafe(32)
        for index, address in enumerate(addresses):
            directory = private / str(index)
            certificates(directory, address)
            configuration = directory / "turnserver.conf"
            configuration.write_text(
                "realm=local-protocol.test\nuse-auth-secret\nstatic-auth-secret=" + secret +
                f"\nlistening-ip={address}\nrelay-ip={address}\nlistening-port=3478\ntls-listening-port=5349\n"
                "cert=/probe/server.pem\npkey=/probe/server.key\nmin-port=49000\nmax-port=49031\n"
                "relay-threads=1\n"
                "no-cli\nno-tcp-relay\nno-multicast-peers\nno-tlsv1\nno-tlsv1_1\n"
                "user-quota=8\ntotal-quota=16\nlog-file=stdout\npidfile=/tmp/turnserver.pid\n")
            name = network + "-" + str(index)
            containers.append(name)
            # No published host ports, no host networking, no external routing.
            # The pinned image stamps CAP_NET_BIND_SERVICE on its binary;
            # retain only that bounding capability so exec is permitted. Run
            # as the workspace uid, with no-new-privileges and a read-only root.
            command(DOCKER + ["run", "-d", "--name", name, "--network", network, "--ip", address,
                              "--user", f"{os.getuid()}:{os.getgid()}", "--read-only", "--tmpfs", "/tmp", "--cap-drop=ALL", "--cap-add=NET_BIND_SERVICE",
                              "--security-opt", "no-new-privileges", "--entrypoint", "/usr/bin/turnserver",
                              "--mount", f"type=bind,src={directory},dst=/probe,readonly", pin["coturn_image"],
                              "-c", "/probe/turnserver.conf"])
            try:
                wait_port(address)
            except RuntimeError:
                print(command(DOCKER + ["inspect", "--format", "{{.State.Status}} {{.State.ExitCode}} {{.State.Error}}", name]), file=sys.stderr)
                logs = subprocess.run(DOCKER + ["logs", name], stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                      text=True, env=ENV, timeout=10)
                diagnostic = logs.stdout + logs.stderr
                print(diagnostic[-5000:].replace(secret, "[REDACTED_TEST_SECRET]"), file=sys.stderr)
                raise
        username, password = credential(secret, int(time.time()) + 240)
        expired_username, expired_password = credential(secret, int(time.time()) - 3600)
        config = {"address": addresses[0], "untrusted_address": addresses[1], "username": username,
                  "password": password, "expired_username": expired_username, "expired_password": expired_password,
                  "ca_file": str(private / "0/ca.pem")}
        config_path = private / "config.json"
        config_path.write_text(json.dumps(config))
        test_env = dict(ENV, RTC_PROTOCOL_CONFIG=str(config_path), SSL_CERT_FILE=config["ca_file"])
        process = subprocess.run(["go", "tool", "test2json", "-t", "-p", "local_headless_protocols",
                                  str(output / "protocols.test"), "-test.v", "-test.timeout=90s"],
                                 cwd=ROOT, env=test_env, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                 text=True, timeout=100)
        # Only test-controlled messages are retained. Pion log output is disabled;
        # SDP, candidates, certificate keys and REST credentials are not logged.
        (output / "tests.jsonl").write_text(process.stdout)
        records = [json.loads(line) for line in process.stdout.splitlines() if line.strip()]
        counts = Counter(item["Action"] for item in records if "Test" in item and item["Action"] in ("pass", "fail", "skip"))
        result["tests"] = dict(counts)
        result["test_binary_sha256"] = hashlib.sha256((output / "protocols.test").read_bytes()).hexdigest()
        result["fixture_sha256"] = {path.name: hashlib.sha256(path.read_bytes()).hexdigest()
                                    for path in sorted((ROOT / "testdata").glob("*.json"))}
        for item in records:
            if item["Action"] in ("pass", "fail", "skip") and "Test" in item:
                print(item["Action"].upper() + " " + item["Test"])
        evaluate_tests(process.stdout, process.returncode)
        result["status"] = "PASS"
        exit_code = 0
    except (OSError, ValueError, subprocess.SubprocessError, RuntimeError) as failure:
        result["reason"] = str(failure) if type(failure) is RuntimeError else type(failure).__name__
    finally:
        cleanup_errors = []
        for name in reversed(containers):
            try:
                command(DOCKER + ["rm", "-f", name])
            except subprocess.CalledProcessError as failure:
                if "No such container" not in failure.stderr:
                    cleanup_errors.append("container_cleanup_failed")
        if network_created:
            try:
                command(DOCKER + ["network", "rm", network])
            except subprocess.SubprocessError:
                cleanup_errors.append("network_cleanup_failed")
        shutil.rmtree(private)
        if cleanup_errors:
            result.update(status="FAIL", cleanup_errors=cleanup_errors)
            exit_code = 1
        (output / "result.json").write_text(json.dumps(result, indent=2) + "\n")
        print(json.dumps(result, indent=2))
    return exit_code


if __name__ == "__main__":
    sys.exit(main())
