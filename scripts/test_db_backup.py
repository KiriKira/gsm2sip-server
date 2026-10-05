"""Unit and PostgreSQL integration tests for db_backup.py.

The integration class requires TEST_DATABASE_URL plus PostgreSQL 17 client
tools. It creates isolated temporary databases and removes them afterward.
"""

from __future__ import annotations

import errno
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import sys
import tempfile
import time
import unittest
from urllib.parse import quote, urlsplit, urlunsplit
from unittest import mock


ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(Path(__file__).resolve().parent))
import db_backup  # noqa: E402


TEST_DATABASE_URL = os.environ.get("TEST_DATABASE_URL", "")
REQUIRED_TOOLS = ("pg_dump", "pg_restore", "psql")
POSTGRES_TEST_IMAGE = "postgres:17.6@sha256:00bc86618629af00d2937fdc5a5d63db3ff8450acf52f0636ec813c7f4902929"
SUBPROCESS_TIMEOUT_SECONDS = 120


class BackupPublicationTests(unittest.TestCase):
    def test_existing_backup_paths_are_never_replaced(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            output = directory / "old.dump"
            manifest = Path(str(output) + ".manifest.json")
            temp_dump = directory / "staged.dump"
            temp_manifest = directory / "staged.json"
            output.write_bytes(b"previous archive")
            manifest.write_bytes(b"previous manifest")
            temp_dump.write_bytes(b"new archive")
            temp_manifest.write_bytes(b"new manifest")

            with self.assertRaises(db_backup.BackupError):
                db_backup.publish_backup_pair(temp_dump, output, temp_manifest, manifest)

            self.assertEqual(output.read_bytes(), b"previous archive")
            self.assertEqual(manifest.read_bytes(), b"previous manifest")

    def test_manifest_publish_failure_removes_only_new_partial_pair(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            old_dump = directory / "known-good.dump"
            old_manifest = Path(str(old_dump) + ".manifest.json")
            old_dump.write_bytes(b"known good archive")
            old_manifest.write_bytes(b"known good manifest")
            staged_dump = directory / "new.staged.dump"
            staged_manifest = directory / "new.staged.json"
            new_dump = directory / "new.dump"
            new_manifest = Path(str(new_dump) + ".manifest.json")
            staged_dump.write_bytes(b"new archive")
            staged_manifest.write_bytes(b"new manifest")
            original_link = os.link

            def fail_manifest_link(source, destination, *args, **kwargs):
                if Path(destination) == new_manifest:
                    raise OSError(errno.ENOSPC, "simulated publication failure")
                return original_link(source, destination, *args, **kwargs)

            with mock.patch.object(db_backup.os, "link", side_effect=fail_manifest_link):
                with self.assertRaises(db_backup.BackupError):
                    db_backup.publish_backup_pair(staged_dump, new_dump, staged_manifest, new_manifest)

            self.assertFalse(new_dump.exists())
            self.assertFalse(new_manifest.exists())
            self.assertEqual(old_dump.read_bytes(), b"known good archive")
            self.assertEqual(old_manifest.read_bytes(), b"known good manifest")


class LocalConnectionTests(unittest.TestCase):
    def test_url_password_is_private_passfile_and_child_environment_is_sanitized(self) -> None:
        url = "postgresql://backup_user:p%3Ass%5Cword@db.example:5432/backup_db?sslmode=disable"
        environment = {
            "DATABASE_URL": url,
            "TEST_DATABASE_URL": "postgresql://test-only:never-inherit@db.example/test_db",
            "PGPASSWORD": "legacy-password",
            "POSTGRES_PASSWORD": "compose-password",
            "SECRETS_ENCRYPTION_KEY": "private-encryption-key",
        }
        with mock.patch.dict(os.environ, environment, clear=True):
            connection = db_backup.LocalConnection()
            passfile = Path(connection.temp_passfile or "")
            try:
                args = connection.database_args()
                self.assertNotIn("p%3Ass%5Cword", " ".join(args))
                self.assertNotIn("DATABASE_URL", " ".join(args))
                self.assertEqual(
                    passfile.read_text(encoding="utf-8"),
                    "db.example:5432:backup_db:backup_user:p\\:ss\\\\word\n",
                )
                self.assertEqual(stat.S_IMODE(passfile.stat().st_mode), 0o600)
                for key in (
                    "DATABASE_URL",
                    "TEST_DATABASE_URL",
                    "PGPASSWORD",
                    "POSTGRES_PASSWORD",
                    "SECRETS_ENCRYPTION_KEY",
                ):
                    self.assertNotIn(key, connection.env)
                self.assertEqual(connection.env["PGPASSFILE"], str(passfile))
            finally:
                connection.__exit__()
            self.assertFalse(passfile.exists())


class PostgreSQLBackupIntegrationTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        compose_ready = shutil.which("docker") is not None
        if TEST_DATABASE_URL:
            missing_tools = [tool for tool in REQUIRED_TOOLS if not shutil.which(tool)]
            if missing_tools:
                raise AssertionError(
                    "TEST_DATABASE_URL is set but PostgreSQL client tools are missing: "
                    + ", ".join(missing_tools)
                )
            cls.backend = "local"
        elif compose_ready:
            cls.backend = "compose"
        else:
            raise unittest.SkipTest("set TEST_DATABASE_URL with PostgreSQL 17 clients or install Docker Compose")
        cls.compose_project = None
        cls.compose_file = None
        cls.suffix = f"{os.getpid()}_{os.urandom(4).hex()}"
        cls.names = {
            key: f"gsm2sip_bak_{cls.suffix}_{key}"
            for key in (
                "source",
                "restored",
                "badsha",
                "nonempty",
                "atomic_source",
                "atomic_target",
                "legacy_source",
                "legacy_target",
            )
        }
        cls.tmp = tempfile.TemporaryDirectory(prefix="gsm2sip-backup-test-")
        cls.directory = Path(cls.tmp.name)
        cls.archive = cls.directory / "server.dump"
        cls.database_urls = {key: cls.database_url(name) for key, name in cls.names.items()}
        try:
            cls._initialize_test_databases()
        except BaseException:
            cls._cleanup_test_databases()
            raise

    @classmethod
    def _initialize_test_databases(cls) -> None:
        if cls.backend == "compose":
            cls.compose_project = f"gsm2sipbackup{cls.suffix.replace('_', '')}"
            cls.compose_file = cls.directory / "compose.yaml"
            cls.compose_file.write_text(
                "services:\n"
                "  postgres:\n"
                f"    image: {POSTGRES_TEST_IMAGE}\n"
                "    environment:\n"
                "      POSTGRES_DB: postgres\n"
                "      POSTGRES_USER: backup_test\n"
                "      POSTGRES_PASSWORD: backup-test-only\n"
                "    healthcheck:\n"
                "      test: [\"CMD-SHELL\", \"pg_isready -U backup_test -d postgres\"]\n"
                "      interval: 1s\n"
                "      timeout: 1s\n"
                "      retries: 30\n",
                encoding="utf-8",
            )
            started = cls.run_compose(["up", "-d", "postgres"])
            if started.returncode:
                raise unittest.SkipTest("could not start temporary PostgreSQL 17 Compose service")
            deadline = time.monotonic() + 60
            while time.monotonic() < deadline:
                ready = cls.run_compose(["exec", "-T", "postgres", "pg_isready", "-U", "backup_test", "-d", "postgres"])
                if ready.returncode == 0:
                    break
                time.sleep(1)
            else:
                raise unittest.SkipTest("temporary PostgreSQL 17 service did not become ready")
        for name in cls.names.values():
            cls.run_sql_url(cls.admin_url(), f"CREATE DATABASE {name}")
        cls.install_schema(cls.database_urls["source"])
        cls.seed_fixture(cls.database_urls["source"])
        cls.install_minimal_schema(cls.database_urls["atomic_source"])
        cls.install_schema(cls.database_urls["legacy_source"], through="0001_identity.sql")
        cls.seed_legacy_fixture(cls.database_urls["legacy_source"])

    @classmethod
    def tearDownClass(cls) -> None:
        cls._cleanup_test_databases()

    @classmethod
    def _cleanup_test_databases(cls) -> None:
        if hasattr(cls, "tmp"):
            if getattr(cls, "backend", None) == "compose" and cls.compose_file is not None:
                try:
                    cls.run_compose(["down", "-v"], timeout=20)
                except Exception:
                    pass
            elif getattr(cls, "backend", None) == "local" and hasattr(cls, "names"):
                for name in cls.names.values():
                    try:
                        cls.run_sql_url(cls.admin_url(), f"DROP DATABASE IF EXISTS {name} WITH (FORCE)")
                    except Exception:
                        # Preserve the original test result if DB access was lost.
                        pass
            cls.tmp.cleanup()

    @classmethod
    def admin_url(cls) -> str:
        if cls.backend == "compose":
            return "compose:///postgres"
        parts = urlsplit(TEST_DATABASE_URL)
        return urlunsplit((parts.scheme, parts.netloc, "/postgres", parts.query, ""))

    @classmethod
    def database_url(cls, name: str) -> str:
        if cls.backend == "compose":
            return "compose:///" + name
        parts = urlsplit(TEST_DATABASE_URL)
        return urlunsplit((parts.scheme, parts.netloc, "/" + quote(name, safe=""), parts.query, ""))

    @classmethod
    def compose_base(cls) -> list[str]:
        return [
            "docker",
            "compose",
            "--file",
            str(cls.compose_file),
            "--project-name",
            str(cls.compose_project),
        ]

    @classmethod
    def run_compose(
        cls,
        arguments: list[str],
        *,
        input_text: str | None = None,
        timeout: int = SUBPROCESS_TIMEOUT_SECONDS,
    ) -> subprocess.CompletedProcess[str]:
        try:
            return subprocess.run(
                cls.compose_base() + arguments,
                cwd=ROOT,
                input=input_text,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
                check=False,
                timeout=timeout,
            )
        except subprocess.TimeoutExpired as exc:
            raise AssertionError("Docker Compose test command timed out") from exc

    @classmethod
    def run_psql(cls, url: str, *, sql: str | None = None, input_text: str | None = None) -> str:
        if cls.backend == "compose":
            database = urlsplit(url).path.lstrip("/")
            command = cls.compose_base() + [
                "exec",
                "-T",
                "postgres",
                "psql",
                "--username=backup_test",
                "--dbname=" + database,
                "--no-psqlrc",
                "--no-align",
                "--tuples-only",
                "--set=ON_ERROR_STOP=1",
            ]
            if sql is not None:
                command += ["--command", sql]
            elif input_text is not None:
                command += ["--file=-"]
            completed = subprocess.run(
                command,
                cwd=ROOT,
                input=input_text,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
                check=False,
                timeout=SUBPROCESS_TIMEOUT_SECONDS,
            )
            if completed.returncode:
                raise AssertionError(f"psql failed with status {completed.returncode}; diagnostics withheld")
            return completed.stdout.strip()

        with db_backup.LocalConnection(url) as connection:
            command = ["psql", "--no-psqlrc", "--no-align", "--tuples-only", "--set=ON_ERROR_STOP=1"]
            command += connection.database_args()
            if sql is not None:
                command += ["--command", sql]
            elif input_text is not None:
                command += ["--file=-"]
            try:
                completed = subprocess.run(
                    command,
                    cwd=ROOT,
                    input=input_text,
                    stdout=subprocess.PIPE,
                    stderr=subprocess.PIPE,
                    text=True,
                    env=connection.env,
                    check=False,
                    timeout=SUBPROCESS_TIMEOUT_SECONDS,
                )
            except FileNotFoundError as exc:
                raise AssertionError("psql was not found") from exc
        if completed.returncode:
            raise AssertionError(f"psql failed with status {completed.returncode}; diagnostics withheld")
        return completed.stdout.strip()

    @classmethod
    def run_sql_url(cls, url: str, sql: str) -> str:
        return cls.run_psql(url, sql=sql)

    @classmethod
    def install_schema(cls, url: str, *, through: str | None = None) -> None:
        cls.run_sql_url(
            url,
            "CREATE TABLE public.schema_migrations (name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())",
        )
        migration_sql = []
        for migration in sorted((ROOT / "migrations").glob("*.sql")):
            migration_sql.append(migration.read_text(encoding="utf-8"))
            migration_sql.append(
                "\nINSERT INTO public.schema_migrations(name) VALUES ('"
                + migration.name
                + "');\n"
            )
            cls.run_psql(url, input_text="\n".join(migration_sql))
            migration_sql.clear()
            if migration.name == through:
                break

    @classmethod
    def install_minimal_schema(cls, url: str) -> None:
        cls.run_sql_url(
            url,
            "CREATE TABLE public.schema_migrations (name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now()); "
            "INSERT INTO public.schema_migrations(name) VALUES ('0010_client_metadata.sql'); "
            "CREATE TABLE public.commands (status TEXT NOT NULL CHECK (status <> 'unknown'), updated_at TIMESTAMPTZ NOT NULL); "
            "INSERT INTO public.commands(status,updated_at) VALUES ('queued',clock_timestamp())",
        )

    @classmethod
    def seed_legacy_fixture(cls, url: str) -> None:
        cls.run_psql(
            url,
            input_text="""
INSERT INTO public.owners(id,display_name) VALUES ('70000000-0000-4000-8000-000000000001','Legacy backup owner');
INSERT INTO public.devices(id,owner_id,role,name) VALUES
 ('70000000-0000-4000-8000-000000000002','70000000-0000-4000-8000-000000000001','gateway','Legacy gateway');
INSERT INTO public.sessions(id,device_id,access_hash,access_expires_at,refresh_hash,refresh_expires_at) VALUES
 ('70000000-0000-4000-8000-000000000003','70000000-0000-4000-8000-000000000002',decode(repeat('11',32),'hex'),clock_timestamp()+interval '1 day',decode(repeat('22',32),'hex'),clock_timestamp()+interval '30 days');
INSERT INTO public.pairing_codes(id,owner_id,role,code_hash,expires_at) VALUES
 ('70000000-0000-4000-8000-000000000004','70000000-0000-4000-8000-000000000001','client',decode(repeat('33',32),'hex'),clock_timestamp()+interval '1 day');
""",
        )

    @classmethod
    def seed_fixture(cls, url: str) -> None:
        fixture_sql = r"""
INSERT INTO public.owners(id,display_name) VALUES
 ('10000000-0000-4000-8000-000000000001','Backup test owner');
INSERT INTO public.devices(id,owner_id,role,name) VALUES
 ('10000000-0000-4000-8000-000000000002','10000000-0000-4000-8000-000000000001','gateway','Test gateway one'),
 ('10000000-0000-4000-8000-000000000003','10000000-0000-4000-8000-000000000001','client','Test client'),
 ('10000000-0000-4000-8000-000000000004','10000000-0000-4000-8000-000000000001','gateway','Test gateway two');
INSERT INTO public.gateways(device_id,mapping_revision) VALUES
 ('10000000-0000-4000-8000-000000000002',7),
 ('10000000-0000-4000-8000-000000000004',7);
INSERT INTO public.sim_bindings(sim_id,owner_id,gateway_id,slot_index,label,state,identity_verified,mapping_revision) VALUES
 ('20000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000002',0,'SIM 一号','active',true,7),
 ('20000000-0000-4000-8000-000000000002','10000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000004',0,'SIM two','active',true,7);
INSERT INTO public.sessions(id,device_id,access_hash,access_expires_at,refresh_hash,refresh_expires_at) VALUES
 ('30000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000003',decode(repeat('11',32),'hex'),clock_timestamp()+interval '1 day',decode(repeat('22',32),'hex'),clock_timestamp()+interval '30 days');
INSERT INTO public.pairing_codes(id,owner_id,role,code_hash,expires_at) VALUES
 ('30000000-0000-4000-8000-000000000002','10000000-0000-4000-8000-000000000001','client',decode(repeat('33',32),'hex'),clock_timestamp()+interval '1 day');
INSERT INTO public.sip_endpoint_bindings(device_id,endpoint_id,auth_username,aor) VALUES
 ('10000000-0000-4000-8000-000000000003','dev_0123456789abcdef0123456789abcdef','client-auth','client-aor');
INSERT INTO public.ps_endpoints(id) VALUES ('dev_0123456789abcdef0123456789abcdef');
INSERT INTO public.ps_auths(id,username,password_digest) VALUES
 ('auth-client','client-auth','MD5:00000000000000000000000000000000');
INSERT INTO public.ps_aors(id) VALUES ('client-aor');
INSERT INTO public.device_sip_credentials(device_id,idempotency_hash,response_ciphertext,replay_expires_at) VALUES
 ('10000000-0000-4000-8000-000000000003',decode(repeat('44',32),'hex'),decode('01020304','hex'),clock_timestamp()+interval '1 day');
INSERT INTO public.refresh_recoveries(session_id,old_refresh_hash,idempotency_key_hash,new_refresh_hash,response_ciphertext,expires_at) VALUES
 ('30000000-0000-4000-8000-000000000001',decode(repeat('55',32),'hex'),decode(repeat('66',32),'hex'),decode(repeat('77',32),'hex'),decode('05060708','hex'),clock_timestamp()+interval '1 day');
INSERT INTO public.messages(id,owner_id,client_device_id,gateway_id,sim_id,mapping_revision,direction,from_address,to_address,body,status,part_count) VALUES
 ('40000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000003','10000000-0000-4000-8000-000000000002','20000000-0000-4000-8000-000000000001',7,'inbound','+15550000001','+15550000002','Привет 世界 📩 — received on SIM 一号','received',1),
 ('40000000-0000-4000-8000-000000000002','10000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000003','10000000-0000-4000-8000-000000000004','20000000-0000-4000-8000-000000000002',7,'inbound','+15550000003','+15550000004','第二张 SIM 的短信；مرحبا 🌍','received',1),
 ('40000000-0000-4000-8000-000000000003','10000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000003','10000000-0000-4000-8000-000000000002','20000000-0000-4000-8000-000000000001',7,'outbound','+15550000002','+15550000005','Queued private test SMS 🧪','queued',1),
 ('40000000-0000-4000-8000-000000000004','10000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000003','10000000-0000-4000-8000-000000000002','20000000-0000-4000-8000-000000000001',7,'outbound','+15550000002','+15550000006','Accepted private test SMS','accepted_by_gateway',1),
 ('40000000-0000-4000-8000-000000000005','10000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000003','10000000-0000-4000-8000-000000000002','20000000-0000-4000-8000-000000000001',7,'outbound','+15550000002','+15550000007','Dispatching private test SMS','dispatching',1),
 ('40000000-0000-4000-8000-000000000006','10000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000003','10000000-0000-4000-8000-000000000002','20000000-0000-4000-8000-000000000001',7,'outbound','+15550000002','+15550000008','Submitted private test SMS','submitted',1);
INSERT INTO public.commands(id,message_id,owner_id,gateway_id,sim_id,mapping_revision,to_address,body,status,expires_at) VALUES
 ('50000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000003','10000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000002','20000000-0000-4000-8000-000000000001',7,'+15550000005','Queued private test SMS 🧪','queued',clock_timestamp()+interval '10 minutes'),
 ('50000000-0000-4000-8000-000000000004','40000000-0000-4000-8000-000000000004','10000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000002','20000000-0000-4000-8000-000000000001',7,'+15550000006','Accepted private test SMS','accepted_by_gateway',clock_timestamp()+interval '10 minutes'),
 ('50000000-0000-4000-8000-000000000005','40000000-0000-4000-8000-000000000005','10000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000002','20000000-0000-4000-8000-000000000001',7,'+15550000007','Dispatching private test SMS','dispatching',clock_timestamp()+interval '10 minutes'),
 ('50000000-0000-4000-8000-000000000006','40000000-0000-4000-8000-000000000006','10000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000002','20000000-0000-4000-8000-000000000001',7,'+15550000008','Submitted private test SMS','submitted',clock_timestamp()+interval '10 minutes');
INSERT INTO public.message_parts(message_id,part_index,state) VALUES
 ('40000000-0000-4000-8000-000000000005',0,'dispatching'),
 ('40000000-0000-4000-8000-000000000006',0,'submitted');
INSERT INTO public.sms_rate_ledger(sim_id,message_id) VALUES
 ('20000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000003'),
 ('20000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000004'),
 ('20000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000005'),
 ('20000000-0000-4000-8000-000000000001','40000000-0000-4000-8000-000000000006');
INSERT INTO public.gateway_events(owner_id,gateway_id,event_id,sequence,payload_hash,event_json) VALUES
 ('10000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000002','50000000-0000-4000-8000-000000000002',1,decode(repeat('88',32),'hex'),'{"event_type":"sms.received","sequence":1}');
INSERT INTO public.server_events(cursor,id,owner_id,event_type,message_id,event_json) VALUES
 (1,'50000000-0000-4000-8000-000000000003','10000000-0000-4000-8000-000000000001','message.received','40000000-0000-4000-8000-000000000001','{"message_id":"40000000-0000-4000-8000-000000000001"}');
INSERT INTO public.outbox(owner_id,event_id,kind,payload) VALUES
 ('10000000-0000-4000-8000-000000000001','50000000-0000-4000-8000-000000000003','client_event','{"message_id":"40000000-0000-4000-8000-000000000001"}');
INSERT INTO public.client_event_receipts(device_id,owner_id,durable_cursor) VALUES
 ('10000000-0000-4000-8000-000000000003','10000000-0000-4000-8000-000000000001',1);
INSERT INTO public.call_sessions(call_id,owner_id,client_device_id,gateway_id,sim_id,mapping_revision,direction,to_address,state,expires_at,gateway_endpoint_id,client_endpoint_id) VALUES
 ('60000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000003','10000000-0000-4000-8000-000000000002','20000000-0000-4000-8000-000000000001',7,'outgoing','+15550000009','reserved',clock_timestamp()+interval '5 minutes','dev_fedcba9876543210fedcba9876543210','dev_0123456789abcdef0123456789abcdef');
INSERT INTO public.call_intents(intent_id,call_id,owner_id,client_device_id,gateway_id,sim_id,mapping_revision,to_address,token_hash,payload_hash,state,expires_at) VALUES
 ('60000000-0000-4000-8000-000000000002','60000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000003','10000000-0000-4000-8000-000000000002','20000000-0000-4000-8000-000000000001',7,'+15550000009',decode(repeat('99',32),'hex'),decode(repeat('aa',32),'hex'),'reserved',clock_timestamp()+interval '5 minutes');
UPDATE public.call_sessions SET intent_id='60000000-0000-4000-8000-000000000002' WHERE call_id='60000000-0000-4000-8000-000000000001';
INSERT INTO public.gateway_call_slots(gateway_id,call_id) VALUES
 ('10000000-0000-4000-8000-000000000002','60000000-0000-4000-8000-000000000001');
INSERT INTO public.call_sessions(call_id,owner_id,client_device_id,gateway_id,sim_id,mapping_revision,direction,to_address,state,expires_at,wake_nonce,gateway_endpoint_id,client_endpoint_id) VALUES
 ('60000000-0000-4000-8000-000000000003','10000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000003','10000000-0000-4000-8000-000000000004','20000000-0000-4000-8000-000000000002',7,'incoming','+15550000010','pending_wakeup',clock_timestamp()+interval '5 minutes','old-call-wake-nonce','dev_fedcba9876543210fedcba9876543210','dev_0123456789abcdef0123456789abcdef');
INSERT INTO public.call_participants(call_id,owner_id,client_device_id,endpoint_id,wake_nonce,state) VALUES
 ('60000000-0000-4000-8000-000000000003','10000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000003','dev_0123456789abcdef0123456789abcdef','old-participant-wake-nonce','pending_wakeup');
INSERT INTO public.gateway_call_slots(gateway_id,call_id) VALUES
 ('10000000-0000-4000-8000-000000000004','60000000-0000-4000-8000-000000000003');
"""
        cls.run_psql(url, input_text=fixture_sql)

    @classmethod
    def invoke_backup_tool(cls, action: str, *arguments: str, database_url: str) -> subprocess.CompletedProcess[str]:
        environment = os.environ.copy()
        environment["SECRETS_ENCRYPTION_KEY"] = "backup-test-env-secret-never-export-6ac47d"
        command = [sys.executable, str(ROOT / "scripts" / "db_backup.py"), action, *arguments]
        if cls.backend == "compose":
            database = urlsplit(database_url).path.lstrip("/")
            command += ["--compose", "--compose-file", str(cls.compose_file), "--project-name", str(cls.compose_project)]
            if database:
                command += ["--database", database]
            environment.pop("DATABASE_URL", None)
        else:
            environment["DATABASE_URL"] = database_url
        return subprocess.run(
            command,
            cwd=ROOT,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            env=environment,
            check=False,
            timeout=SUBPROCESS_TIMEOUT_SECONDS,
        )

    def test_01_backup_is_private_and_existing_destination_is_preserved(self) -> None:
        completed = self.invoke_backup_tool("backup", "--output", str(self.archive), database_url=self.database_urls["source"])
        self.assertEqual(completed.returncode, 0, completed.stderr)
        manifest_path = Path(str(self.archive) + ".manifest.json")
        self.assertEqual(stat.S_IMODE(self.archive.stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE(manifest_path.stat().st_mode), 0o600)
        original_archive = self.archive.read_bytes()
        original_manifest = manifest_path.read_bytes()
        manifest = json.loads(original_manifest)
        self.assertEqual(manifest["schema_revision"], "0010_client_metadata.sql")
        password = urlsplit(TEST_DATABASE_URL).password or "" if self.backend == "local" else "backup-test-only"
        self.assertNotIn(password, original_manifest.decode("utf-8"))
        self.assertNotIn(b"backup-test-env-secret-never-export-6ac47d", original_archive)
        self.assertNotIn(b"backup-test-env-secret-never-export-6ac47d", original_manifest)
        self.assertNotIn("DATABASE_URL", manifest)

        repeated = self.invoke_backup_tool("backup", "--output", str(self.archive), database_url=self.database_urls["source"])
        self.assertNotEqual(repeated.returncode, 0)
        self.assertEqual(self.archive.read_bytes(), original_archive)
        self.assertEqual(manifest_path.read_bytes(), original_manifest)

    def test_02_bad_sha_is_rejected_before_target_changes(self) -> None:
        damaged = self.directory / "damaged.dump"
        damaged.write_bytes(self.archive.read_bytes()[:-1] + bytes([self.archive.read_bytes()[-1] ^ 1]))
        Path(str(damaged) + ".manifest.json").write_bytes(Path(str(self.archive) + ".manifest.json").read_bytes())
        result = self.invoke_backup_tool("restore", "--input", str(damaged), database_url=self.database_urls["badsha"])
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("SHA-256", result.stderr)
        self.assertEqual(self.run_sql_url(self.database_urls["badsha"], "SELECT to_regclass('public.owners') IS NULL"), "t")

    def test_03_nonempty_target_is_refused_without_removing_existing_table(self) -> None:
        self.run_sql_url(self.database_urls["nonempty"], "CREATE TABLE public.keep_me(id integer)")
        result = self.invoke_backup_tool("restore", "--input", str(self.archive), database_url=self.database_urls["nonempty"])
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("empty database", result.stderr)
        self.assertEqual(self.run_sql_url(self.database_urls["nonempty"], "SELECT to_regclass('public.keep_me') IS NOT NULL"), "t")
        self.assertEqual(self.run_sql_url(self.database_urls["nonempty"], "SELECT to_regclass('public.owners') IS NULL"), "t")

    def test_04_restore_preserves_history_and_quarantines_runtime_work(self) -> None:
        result = self.invoke_backup_tool("restore", "--input", str(self.archive), database_url=self.database_urls["restored"])
        self.assertEqual(result.returncode, 0, result.stderr)
        bodies = self.run_sql_url(self.database_urls["restored"], "SELECT body FROM public.messages WHERE direction='inbound' ORDER BY id")
        self.assertEqual(
            bodies.splitlines(),
            ["Привет 世界 📩 — received on SIM 一号", "第二张 SIM 的短信；مرحبا 🌍"],
        )
        checks = self.run_sql_url(
            self.database_urls["restored"],
            "SELECT (SELECT count(*) FROM public.sim_bindings)=2,"
            "(SELECT count(*) FROM public.client_event_receipts WHERE durable_cursor=1)=1,"
            "(SELECT count(*) FROM public.gateway_events)=1,"
            "(SELECT count(*) FROM public.messages)=6,"
            "(SELECT count(*) FROM public.commands WHERE status='unknown')=3,"
            "(SELECT count(*) FROM public.messages WHERE direction='outbound' AND status='unknown')=3,"
            "(SELECT count(*) FROM public.commands WHERE status='submitted')=1,"
            "(SELECT count(*) FROM public.messages WHERE direction='outbound' AND status='submitted')=1,"
            "(SELECT count(*) FROM public.message_parts WHERE state='unknown')=1,"
            "(SELECT count(*) FROM public.message_parts WHERE state='submitted')=1,"
            "(SELECT count(*) FROM public.sms_rate_ledger)=4,"
            "(SELECT count(*) FROM public.outbox WHERE processed_at IS NULL)=0,"
            "(SELECT count(*) FROM public.refresh_recoveries)=0,"
            "(SELECT count(*) FROM public.sessions WHERE access_expires_at>now() OR refresh_expires_at>now())=0,"
            "(SELECT count(*) FROM public.pairing_codes WHERE consumed_at IS NULL)=0,"
            "(SELECT count(*) FROM public.devices WHERE state='active')=0,"
            "(SELECT count(*) FROM public.sip_endpoint_bindings WHERE state='active')=0,"
            "(SELECT count(*) FROM public.ps_auths)=0,"
            "(SELECT count(*) FROM public.device_sip_credentials WHERE replay_expires_at>now())=0,"
            "(SELECT count(*) FROM public.commands WHERE status IN ('queued','accepted_by_gateway','dispatching'))=0,"
            "(SELECT count(*) FROM public.call_sessions WHERE state='unknown')=2,"
            "(SELECT count(*) FROM public.call_intents WHERE state='reserved')=0,"
            "(SELECT count(*) FROM public.gateway_call_slots)=2,"
            "(SELECT count(*) FROM public.call_participants WHERE wake_nonce LIKE 'backup-restore-revoked-%')=1"
        )
        self.assertEqual(checks.split("|"), ["t"] * 24)

    def test_05_sanitize_sql_failure_rolls_back_the_schema_restore(self) -> None:
        archive = self.directory / "minimal.dump"
        backup = self.invoke_backup_tool("backup", "--output", str(archive), database_url=self.database_urls["atomic_source"])
        self.assertEqual(backup.returncode, 0, backup.stderr)
        result = self.invoke_backup_tool("restore", "--input", str(archive), database_url=self.database_urls["atomic_target"])
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.run_sql_url(self.database_urls["atomic_target"], "SELECT to_regclass('public.schema_migrations') IS NULL"), "t")

    def test_06_migration_0001_backup_restores_with_identity_quarantine(self) -> None:
        archive = self.directory / "legacy.dump"
        backup = self.invoke_backup_tool("backup", "--output", str(archive), database_url=self.database_urls["legacy_source"])
        self.assertEqual(backup.returncode, 0, backup.stderr)
        result = self.invoke_backup_tool("restore", "--input", str(archive), database_url=self.database_urls["legacy_target"])
        self.assertEqual(result.returncode, 0, result.stderr)
        checks = self.run_sql_url(
            self.database_urls["legacy_target"],
            "SELECT (SELECT count(*) FROM public.schema_migrations)=1,"
            "(SELECT count(*) FROM public.devices WHERE state='revoked')=1,"
            "(SELECT count(*) FROM public.sessions WHERE access_expires_at='-infinity'::timestamptz AND refresh_expires_at='-infinity'::timestamptz)=1,"
            "(SELECT count(*) FROM public.pairing_codes WHERE consumed_at IS NOT NULL)=1,"
            "to_regclass('public.messages') IS NULL",
        )
        self.assertEqual(checks.split("|"), ["t"] * 5)


if __name__ == "__main__":
    unittest.main()
