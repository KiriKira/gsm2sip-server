#!/usr/bin/env python3
"""Create and safely restore full gsm2sip PostgreSQL custom-format backups."""

from __future__ import annotations

import argparse
import datetime as dt
import errno
import hashlib
import hmac
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
from urllib.parse import parse_qsl, quote, unquote, urlencode, urlsplit, urlunsplit


ROOT = Path(__file__).resolve().parent.parent
QUARANTINE_SQL = Path(__file__).resolve().with_name("restore_quarantine.sql")
MANIFEST_VERSION = 1
COMPOSE_RESTORE_BLOCKERS = {"api", "worker", "asterisk"}
DB_NAME_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_-]{0,62}$")


class BackupError(Exception):
    pass


def cli() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Full PostgreSQL backup/restore for gsm2sip-server."
    )
    commands = parser.add_subparsers(dest="action", required=True)

    backup = commands.add_parser("backup", help="write a consistent custom-format dump and manifest")
    backup.add_argument("--output", "-o", required=True, type=Path, help="custom-format dump path")
    backup.add_argument("--database", help="source database name in Compose mode (defaults to POSTGRES_DB)")
    add_connection_options(backup)

    restore = commands.add_parser(
        "restore", help="restore into an empty isolated database and quarantine runtime state"
    )
    restore.add_argument("--input", "-i", required=True, type=Path, help="dump path (sidecar manifest required)")
    restore.add_argument(
        "--database",
        help="target database name in Compose mode (must already exist and be empty)",
    )
    add_connection_options(restore)
    return parser


def add_connection_options(parser: argparse.ArgumentParser) -> None:
    parser.add_argument(
        "--compose",
        action="store_true",
        help="run PostgreSQL tools in the Compose postgres service",
    )
    parser.add_argument(
        "--compose-file",
        type=Path,
        help="Compose file (defaults to this repository's compose.yaml)",
    )
    parser.add_argument(
        "--project-name",
        help="Compose project name, useful for a separate isolated recovery stack",
    )


def main() -> int:
    args = cli().parse_args()
    try:
        if args.compose and args.database and not DB_NAME_RE.fullmatch(args.database):
            raise BackupError("Compose database name must use letters, digits, underscore, or hyphen")
        if args.database and not args.compose and args.action == "backup":
            raise BackupError("--database is available only with --compose; DATABASE_URL selects the local source")
        if args.action == "backup":
            do_backup(args)
        else:
            do_restore(args)
    except BackupError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        print("error: interrupted", file=sys.stderr)
        return 130
    return 0


def do_backup(args: argparse.Namespace) -> None:
    output = args.output.expanduser().absolute()
    if not output.parent.is_dir():
        raise BackupError(f"output directory does not exist: {output.parent}")
    if output.name.endswith(".manifest.json"):
        raise BackupError("dump path must not end in .manifest.json")

    manifest_path = Path(str(output) + ".manifest.json")
    if output == manifest_path:
        raise BackupError("invalid output path")
    if path_exists(output) or path_exists(manifest_path):
        raise BackupError(
            "backup output or manifest already exists; choose a new timestamped filename (existing files were not changed)"
        )

    temp_fd, temp_name = secure_temp_file(output.parent, f".{output.name}.", ".tmp")
    temp_dump = Path(temp_name)
    temp_manifest: Path | None = None
    try:
        with connection(args) as db_connection:
            revision_before = schema_revision(db_connection)
            command = dump_command(args, db_connection)
            with os.fdopen(temp_fd, "wb") as stream:
                if isinstance(db_connection, ComposeConnection):
                    completed = compose_run(args, command, label="pg_dump", stdout=stream)
                else:
                    completed = run_process(command, label="pg_dump", stdout=stream, env=db_connection.env)
                if completed.returncode:
                    raise BackupError("pg_dump failed; check PostgreSQL access and client version")
                stream.flush()
                os.fsync(stream.fileno())
            if temp_dump.stat().st_size == 0:
                raise BackupError("pg_dump returned an empty archive")

            revision_after = schema_revision(db_connection)
            if revision_before != revision_after:
                raise BackupError("schema revision changed while the snapshot was being made; retry the backup")

        digest, byte_count = sha256_file(temp_dump)
        manifest = {
            "format_version": MANIFEST_VERSION,
            "archive_format": "pg_dump-custom",
            "archive_file": output.name,
            "archive_bytes": byte_count,
            "archive_sha256": digest,
            "schema_revision": revision_before,
            "backup_created_at_utc": utc_now(),
        }
        os.chmod(temp_dump, 0o600)
        temp_manifest = stage_manifest(manifest_path.parent, manifest_path.name, manifest)
        publish_backup_pair(temp_dump, output, temp_manifest, manifest_path)
        print(f"Backup written: {output} ({byte_count} bytes, schema {revision_before})")
        print(f"Manifest written: {manifest_path}")
    finally:
        try:
            os.close(temp_fd)
        except OSError:
            pass
        try:
            temp_dump.unlink()
        except FileNotFoundError:
            pass
        if temp_manifest is not None:
            try:
                temp_manifest.unlink()
            except FileNotFoundError:
                pass


def do_restore(args: argparse.Namespace) -> None:
    archive = args.input.expanduser().absolute()
    if not archive.is_file():
        raise BackupError(f"backup file not found: {archive}")
    if args.compose and args.database and not DB_NAME_RE.fullmatch(args.database):
        raise BackupError("Compose database name must use letters, digits, underscore, or hyphen")
    if args.database and not args.compose:
        raise BackupError("--database is available only with --compose; set DATABASE_URL for local clients")

    manifest = verify_manifest(archive)
    with connection(args) as db_connection:
        validate_archive(args, db_connection, archive)
        if args.compose:
            check_compose_restore_services(args)
            target = args.database
        else:
            target = None
        ensure_empty_target(args, db_connection, target)

        sql_fd, sql_name = secure_temp_file(None, "gsm2sip-restore-", ".sql")
        sql_path = Path(sql_name)
        try:
            restore_sql_command(args, db_connection, archive, sql_path, sql_fd)
            quarantine = QUARANTINE_SQL.read_bytes()
            with sql_path.open("ab") as stream:
                stream.write(b"\n\n-- gsm2sip post-restore quarantine.\n")
                stream.write(quarantine)
                stream.write(b"\n")
                stream.flush()
                os.fsync(stream.fileno())
            apply_sql(args, db_connection, sql_path, target)
        finally:
            try:
                os.close(sql_fd)
            except OSError:
                pass
            try:
                sql_path.unlink()
            except FileNotFoundError:
                pass

    if target:
        target_label = f"Compose database {target}"
    elif args.compose:
        target_label = "Compose postgres database"
    else:
        target_label = "DATABASE_URL target"
    print(
        f"Restored schema {manifest['schema_revision']} into {target_label}; "
        "old sessions and call authorization were quarantined."
    )
    print("Keep API, worker, and Asterisk stopped until recovery checks are complete.")


def connection(args: argparse.Namespace):
    if args.compose:
        return ComposeConnection(args)
    return LocalConnection()


class LocalConnection:
    def __init__(self, database_url: str | None = None) -> None:
        self.temp_passfile: str | None = None
        self.env = os.environ.copy()
        environment_url = self.env.pop("DATABASE_URL", "")
        self.env.pop("TEST_DATABASE_URL", None)
        self.env.pop("POSTGRES_PASSWORD", None)
        self.env.pop("SECRETS_ENCRYPTION_KEY", None)
        database_url = database_url if database_url is not None else environment_url
        password_from_env = self.env.pop("PGPASSWORD", "")
        if database_url:
            self.database_url, password, pass_fields = sanitized_url(database_url)
            if not password:
                password = password_from_env
            if password:
                self.temp_passfile = create_pgpass(pass_fields, password)
        else:
            self.database_url = ""
            if password_from_env:
                self.temp_passfile = create_pgpass(
                    (
                        self.env.get("PGHOST", "*"),
                        self.env.get("PGPORT", "*"),
                        self.env.get("PGDATABASE", "*"),
                        self.env.get("PGUSER", "*"),
                    ),
                    password_from_env,
                )
        if self.temp_passfile:
            self.env["PGPASSFILE"] = self.temp_passfile

    def __enter__(self):
        return self

    def __exit__(self, *_exc) -> None:
        if self.temp_passfile:
            try:
                os.unlink(self.temp_passfile)
            except FileNotFoundError:
                pass

    def database_args(self, database: str | None = None) -> list[str]:
        if self.database_url:
            url = self.database_url
            if database:
                parts = urlsplit(url)
                query = dict(parse_qsl(parts.query, keep_blank_values=True))
                query["dbname"] = database
                path = "/" + quote(database, safe="")
                url = urlunsplit((parts.scheme, parts.netloc, path, urlencode(query), ""))
            return ["--dbname", url]
        if database:
            return ["--dbname", database]
        return []


class ComposeConnection:
    def __init__(self, args: argparse.Namespace) -> None:
        self.args = args
        self.env = os.environ.copy()
        self.database_url = ""

    def __enter__(self):
        return self

    def __exit__(self, *_exc) -> None:
        return None

    def base(self) -> list[str]:
        command = ["docker", "compose"]
        compose_file = self.args.compose_file
        if compose_file:
            command.extend(["-f", str(compose_file.expanduser().absolute())])
        if self.args.project_name:
            command.extend(["--project-name", self.args.project_name])
        return command

    def shell_command(self, script: str, *parameters: str) -> list[str]:
        return self.base() + [
            "exec",
            "-T",
            "postgres",
            "sh",
            "-eu",
            "-c",
            script,
            "gsm2sip-db-backup",
            *parameters,
        ]

    def database_args(self, database: str | None = None) -> list[str]:
        # Compose helpers pass the database name as a positional shell argument,
        # never as shell source. See psql_command below.
        del database
        return []


def sanitized_url(value: str) -> tuple[str, str, tuple[str, str, str, str]]:
    try:
        parts = urlsplit(value)
        port = parts.port
    except ValueError as exc:
        raise BackupError("DATABASE_URL is invalid") from exc
    if parts.scheme not in {"postgres", "postgresql"}:
        raise BackupError("DATABASE_URL must be a PostgreSQL URI")

    query_items = parse_qsl(parts.query, keep_blank_values=True)
    query = [(key, item) for key, item in query_items if key.lower() not in {"password", "sslpassword"}]
    user = unquote(parts.username) if parts.username else ""
    user_query = next((item for key, item in query_items if key.lower() == "user"), "")
    password = unquote(parts.password) if parts.password is not None else ""
    if not password:
        password = next((item for key, item in query_items if key.lower() == "password"), "")

    dbname = unquote(parts.path[1:]) if parts.path.startswith("/") else ""
    if not dbname:
        dbname = next((item for key, item in query_items if key.lower() == "dbname"), "*")
    host = parts.hostname or next((item for key, item in query_items if key.lower() == "host"), "*")
    port_value = str(port) if port else next((item for key, item in query_items if key.lower() == "port"), "*")
    pass_user = user or user_query or "*"
    pass_db = dbname or "*"
    host_for_url = parts.hostname or ""
    if ":" in host_for_url and not host_for_url.startswith("["):
        host_for_url = f"[{host_for_url}]"
    authority = f"{quote(user, safe='')}@" if user else ""
    authority += host_for_url
    if port:
        authority += f":{port}"
    clean_url = urlunsplit((parts.scheme, authority, parts.path, urlencode(query), ""))
    return clean_url, password, (host, port_value, pass_db, pass_user)


def create_pgpass(fields: tuple[str, str, str, str], password: str) -> str:
    def escape(value: str) -> str:
        if "\n" in value or "\r" in value:
            raise BackupError("PostgreSQL connection settings contain an unsupported newline")
        return value.replace("\\", "\\\\").replace(":", "\\:")

    fd, filename = tempfile.mkstemp(prefix="gsm2sip-pgpass-")
    os.fchmod(fd, 0o600)
    try:
        content = ":".join(escape(field) for field in (*fields, password)) + "\n"
        with os.fdopen(fd, "w", encoding="utf-8") as stream:
            stream.write(content)
            stream.flush()
            os.fsync(stream.fileno())
    except BaseException:
        try:
            os.unlink(filename)
        except FileNotFoundError:
            pass
        raise
    return filename


def compose_run(args: argparse.Namespace, command: list[str], *, label: str, **kwargs):
    kwargs.setdefault("stderr", subprocess.PIPE)
    kwargs.setdefault("stdin", subprocess.DEVNULL)
    try:
        completed = subprocess.run(command, cwd=ROOT, check=False, **kwargs)
    except FileNotFoundError as exc:
        raise BackupError("Docker Compose was not found") from exc
    if completed.returncode:
        raise BackupError(f"{label} failed (exit {completed.returncode}); check Docker Compose and database access")
    return completed


def run_process(command: list[str], *, label: str, **kwargs):
    kwargs.setdefault("stderr", subprocess.PIPE)
    try:
        completed = subprocess.run(command, cwd=ROOT, check=False, **kwargs)
    except FileNotFoundError as exc:
        executable = command[0]
        raise BackupError(f"{executable} was not found; install PostgreSQL client tools or use --compose") from exc
    if completed.returncode:
        # PostgreSQL diagnostic output can include connection information, so
        # never echo child stderr or the command line here.
        return completed
    return completed


def schema_revision(db_connection) -> str:
    query = "SELECT max(name) FROM schema_migrations"
    if isinstance(db_connection, ComposeConnection):
        command = db_connection.shell_command(
            'db="$POSTGRES_DB"; if [ -n "$1" ]; then db="$1"; fi; exec psql --username="$POSTGRES_USER" --dbname="$db" --no-psqlrc --no-align --tuples-only --set=ON_ERROR_STOP=1 --command "$2"',
            db_connection.args.database or "",
            query,
        )
        # Empty $1 means use Compose's POSTGRES_DB. Query is passed as $2.
        completed = compose_run(db_connection.args, command, label="schema revision query", stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    else:
        command = ["psql", "--no-psqlrc", "--no-align", "--tuples-only", "--set=ON_ERROR_STOP=1"]
        command += db_connection.database_args()
        command += ["--command", query]
        completed = run_process(command, label="schema revision query", stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, env=db_connection.env)
    revision = completed.stdout.strip()
    if not revision or "\n" in revision:
        raise BackupError("schema_migrations is missing or has no applied migration")
    return revision


def dump_command(args: argparse.Namespace, db_connection) -> list[str]:
    if isinstance(db_connection, ComposeConnection):
        script = 'db="$POSTGRES_DB"; if [ -n "$1" ]; then db="$1"; fi; exec pg_dump --format=custom --no-owner --no-acl --username="$POSTGRES_USER" --dbname="$db"'
        return db_connection.shell_command(script, args.database or "")
    command = ["pg_dump", "--format=custom", "--no-owner", "--no-acl"]
    command += db_connection.database_args()
    return command


def validate_archive(args: argparse.Namespace, db_connection, archive: Path) -> None:
    if isinstance(db_connection, ComposeConnection):
        with archive.open("rb") as stream:
            command = db_connection.shell_command("exec pg_restore --list")
            completed = compose_run(args, command, label="pg_restore --list", stdin=stream, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    else:
        command = ["pg_restore", "--list", str(archive)]
        completed = run_process(command, label="pg_restore --list", stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=db_connection.env)
    listing = completed.stdout
    if not listing or b" TOC " not in listing[:4096] and b"; Archive created" not in listing[:4096]:
        raise BackupError("pg_restore --list returned no valid archive table of contents")


def check_compose_restore_services(args: argparse.Namespace) -> None:
    base = ComposeConnection(args).base()
    command = base + ["ps", "--services", "--status", "running"]
    completed = compose_run(args, command, label="Compose service check", stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    running = {line.strip() for line in completed.stdout.splitlines() if line.strip()}
    blockers = sorted(running & COMPOSE_RESTORE_BLOCKERS)
    if blockers:
        names = ", ".join(blockers)
        raise BackupError(f"Compose restore refused while these services are running: {names}; stop them manually first")


EMPTY_TARGET_QUERY = """
SELECT CASE WHEN
    EXISTS (
        SELECT 1 FROM pg_catalog.pg_class c
        JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
          AND n.nspname NOT LIKE 'pg\\_%' ESCAPE '\\'
          AND c.relkind IN ('r', 'p', 'v', 'm', 'S', 'f')
    ) OR EXISTS (
        SELECT 1 FROM pg_catalog.pg_namespace
        WHERE nspname NOT IN ('pg_catalog', 'information_schema', 'public')
          AND nspname NOT LIKE 'pg\\_%' ESCAPE '\\'
    )
THEN 'NONEMPTY' ELSE 'EMPTY' END
""".strip()


def ensure_empty_target(args: argparse.Namespace, db_connection, database: str | None) -> None:
    if isinstance(db_connection, ComposeConnection):
        script = 'db="$POSTGRES_DB"; if [ -n "$1" ]; then db="$1"; fi; exec psql --username="$POSTGRES_USER" --dbname="$db" --no-psqlrc --no-align --tuples-only --set=ON_ERROR_STOP=1 --command "$2"'
        command = db_connection.shell_command(script, database or "", EMPTY_TARGET_QUERY)
        completed = compose_run(args, command, label="target database check", stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    else:
        command = ["psql", "--no-psqlrc", "--no-align", "--tuples-only", "--set=ON_ERROR_STOP=1"]
        command += db_connection.database_args(database)
        command += ["--command", EMPTY_TARGET_QUERY]
        completed = run_process(command, label="target database check", stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, env=db_connection.env)
    state = completed.stdout.strip()
    if state != "EMPTY":
        if state == "NONEMPTY":
            raise BackupError("restore target contains user tables or schemas; use a new empty database (nothing was changed)")
        raise BackupError("could not prove the restore target is empty (nothing was changed)")


def restore_sql_command(args: argparse.Namespace, db_connection, archive: Path, sql_path: Path, sql_fd: int) -> None:
    os.fchmod(sql_fd, 0o600)
    if isinstance(db_connection, ComposeConnection):
        with archive.open("rb") as source, os.fdopen(sql_fd, "wb") as destination:
            command = db_connection.shell_command(
                "exec pg_restore --exit-on-error --no-owner --no-acl --file=/dev/stdout"
            )
            completed = compose_run(
                args,
                command,
                label="pg_restore SQL generation",
                stdin=source,
                stdout=destination,
                stderr=subprocess.PIPE,
            )
            destination.flush()
            os.fsync(destination.fileno())
    else:
        command = ["pg_restore", "--exit-on-error", "--no-owner", "--no-acl", "--file", str(sql_path), str(archive)]
        os.close(sql_fd)
        completed = run_process(command, label="pg_restore SQL generation", stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, env=db_connection.env)
        if completed.returncode:
            raise BackupError("pg_restore could not generate the restore script; target was not changed")
        return
    if completed.returncode:
        raise BackupError("pg_restore could not generate the restore script; target was not changed")


def apply_sql(args: argparse.Namespace, db_connection, sql_path: Path, database: str | None) -> None:
    if isinstance(db_connection, ComposeConnection):
        script = 'db="$POSTGRES_DB"; if [ -n "$1" ]; then db="$1"; fi; exec psql --username="$POSTGRES_USER" --dbname="$db" --no-psqlrc --single-transaction --set=ON_ERROR_STOP=1 --file=-'
        with sql_path.open("rb") as stream:
            command = db_connection.shell_command(script, database or "")
            try:
                completed = compose_run(args, command, label="transactional restore", stdin=stream, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
            except BackupError as exc:
                raise BackupError("transactional restore failed; PostgreSQL rolled back the target database") from exc
    else:
        command = ["psql", "--no-psqlrc", "--single-transaction", "--set=ON_ERROR_STOP=1"]
        command += db_connection.database_args(database)
        command += ["--file", str(sql_path)]
        completed = run_process(command, label="transactional restore", stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, env=db_connection.env)
    if completed.returncode:
        raise BackupError("transactional restore failed; PostgreSQL rolled back the target database")


def verify_manifest(archive: Path) -> dict[str, object]:
    manifest_path = Path(str(archive) + ".manifest.json")
    try:
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise BackupError(f"backup manifest is missing or invalid: {manifest_path}") from exc
    if not isinstance(manifest, dict) or manifest.get("format_version") != MANIFEST_VERSION:
        raise BackupError("backup manifest version is unsupported")
    if manifest.get("archive_format") != "pg_dump-custom":
        raise BackupError("backup manifest does not describe a PostgreSQL custom-format archive")
    expected_hash = manifest.get("archive_sha256")
    if not isinstance(expected_hash, str) or not re.fullmatch(r"[0-9a-f]{64}", expected_hash):
        raise BackupError("backup manifest SHA-256 is invalid")
    expected_bytes = manifest.get("archive_bytes")
    if not isinstance(expected_bytes, int) or expected_bytes < 1:
        raise BackupError("backup manifest byte count is invalid")
    revision = manifest.get("schema_revision")
    if not isinstance(revision, str) or not revision:
        raise BackupError("backup manifest schema revision is missing")
    digest, byte_count = sha256_file(archive)
    if byte_count != expected_bytes or not hmac.compare_digest(digest, expected_hash):
        raise BackupError("backup archive SHA-256 or byte count does not match its manifest")
    return manifest


def stage_manifest(directory: Path, final_name: str, payload: dict[str, object]) -> Path:
    fd, temp_name = secure_temp_file(directory, f".{final_name}.", ".tmp")
    temp_path = Path(temp_name)
    complete = False
    try:
        with os.fdopen(fd, "w", encoding="utf-8", newline="\n") as stream:
            json.dump(payload, stream, ensure_ascii=False, sort_keys=True, indent=2)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.chmod(temp_path, 0o600)
        complete = True
        return temp_path
    finally:
        try:
            os.close(fd)
        except OSError:
            pass
        if not complete:
            try:
                temp_path.unlink()
            except FileNotFoundError:
                pass


def publish_backup_pair(temp_dump: Path, output: Path, temp_manifest: Path, manifest_path: Path) -> None:
    """Publish both complete files without replacing any existing path."""
    if path_exists(output) or path_exists(manifest_path):
        raise BackupError("backup output appeared during publication; no existing file was replaced")
    dump_published = False
    try:
        os.link(temp_dump, output)
        dump_published = True
        os.link(temp_manifest, manifest_path)
    except OSError as exc:
        if dump_published:
            unlink_if_same_inode(output, temp_dump)
        if exc.errno == errno.EEXIST:
            raise BackupError("backup output appeared during publication; no existing file was replaced") from None
        raise BackupError("could not publish the backup and manifest; any previous backup files were left unchanged") from None
    fsync_directory(output.parent)


def path_exists(path: Path) -> bool:
    return os.path.lexists(path)


def unlink_if_same_inode(path: Path, staging_path: Path) -> None:
    try:
        published = path.stat()
        staged = staging_path.stat()
        if published.st_dev == staged.st_dev and published.st_ino == staged.st_ino:
            path.unlink()
    except FileNotFoundError:
        pass


def secure_temp_file(directory: Path | None, prefix: str, suffix: str) -> tuple[int, str]:
    fd, filename = tempfile.mkstemp(prefix=prefix, suffix=suffix, dir=directory)
    os.fchmod(fd, 0o600)
    return fd, filename


def sha256_file(path: Path) -> tuple[str, int]:
    digest = hashlib.sha256()
    size = 0
    with path.open("rb") as stream:
        while True:
            chunk = stream.read(1024 * 1024)
            if not chunk:
                break
            size += len(chunk)
            digest.update(chunk)
    return digest.hexdigest(), size


def utc_now() -> str:
    return dt.datetime.now(dt.timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")


def fsync_directory(path: Path) -> None:
    flags = getattr(os, "O_DIRECTORY", 0) | os.O_RDONLY
    try:
        descriptor = os.open(path, flags)
    except OSError:
        return
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


if __name__ == "__main__":
    raise SystemExit(main())
