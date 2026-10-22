# Local and production operations

## Deployment settings

Supply a stable base64-encoded 32-byte `SECRETS_ENCRYPTION_KEY` from a secret
manager. Keep it unchanged across API restarts and back it up with the database:
it encrypts the refresh-recovery records, and the all-in-one image derives its
TURN credential from it. SIP calling, Asterisk and ARI were removed in 0.4; the
API logs and ignores leftover `SIP_*` and `ARI_*` settings.

For a manual local setup, copy `.env.example` to `.env` and generate the key
once without printing it to the terminal:

```sh
umask 077
tmp=$(mktemp .env.XXXXXX)
awk '!/^SECRETS_ENCRYPTION_KEY=/' .env > "$tmp"
printf 'SECRETS_ENCRYPTION_KEY=' >> "$tmp"
openssl rand -base64 32 | tr -d '\n' >> "$tmp"
printf '\n' >> "$tmp"
chmod 600 "$tmp"
mv "$tmp" .env
```

Calls need ICE servers both phones can reach: set `RTC_STUN_URLS`,
`RTC_TURN_URLS` and `RTC_TURN_SHARED_SECRET` (see [webrtc-audio.md](webrtc-audio.md)),
or use the all-in-one image, which runs its own relay.

On its first start 0.4 ends every call that a SIP release left unfinished, with
reason `sip_removed`, so its cellular slot cannot block new calls. Tables that
only SIP used (`sip_endpoint_bindings`, `device_sip_credentials`, `ps_*`) stay
untouched, so an earlier release can still start on the same database;
revoking a device still deletes its rows there.

## PostgreSQL backup and isolated restore

The server backup contains the complete PostgreSQL database in custom
`pg_dump` format. It covers SMS bodies and parts, gateway event history,
commands, identities, SIM mappings, call state, receipts, audit rows, and,
from releases before 0.4, encrypted SIP credential records. It is not an
encrypted archive: the dump contains private message and phone data,
access-token hashes, and any legacy SIP digests and credential ciphertext. Store it on an encrypted disk in a
private directory. The tool creates dump and manifest files with mode `0600`,
refuses to overwrite either file, and does not print database URLs or
passwords.

Use a directory on the encrypted backup volume and give each snapshot a unique
UTC filename. Compose mode runs PostgreSQL client tools inside the existing
`postgres` container and does not require host-side PostgreSQL clients:

```sh
install -d -m 700 /mnt/encrypted/gsm2sip-backups
python3 scripts/db_backup.py backup --compose \
  --output /mnt/encrypted/gsm2sip-backups/gsm2sip-20261005T120000Z.dump
```

The tool writes the dump and a sibling `.manifest.json` file. The manifest
records the archive SHA-256, byte count, backup time in UTC, and the latest
`schema_migrations` revision. The database dump uses PostgreSQL's consistent
snapshot behavior. The manifest detects damaged or mismatched files; it does
not encrypt or authenticate a backup against an attacker who can replace both
files.

For a Compose restore, use a new Compose project so its PostgreSQL data volume
is isolated from the source. Start only PostgreSQL. The default `gsm2sip`
database in a newly initialized volume is empty:

```sh
docker compose --project-name gsm2sip-recovery up -d postgres
python3 scripts/db_backup.py restore --compose \
  --project-name gsm2sip-recovery \
  --input /mnt/encrypted/gsm2sip-backups/gsm2sip-20261005T120000Z.dump
```

To restore into a separately created database in that project, create it first
and pass its name with `--database`. The restore tool never drops or creates a
database. It verifies the manifest hash and `pg_restore --list` output, refuses
any target with user tables or schemas, then runs the generated restore SQL and
the quarantine SQL in one `psql --single-transaction` transaction. A failed
restore rolls back the target to empty. With `--compose`, it refuses to run if
`api`, `worker`, or `asterisk` is running in the selected Compose project; it
never stops services automatically. For local PostgreSQL clients, set
`DATABASE_URL` to the already-created empty target database and stop every API,
worker, and Asterisk process that can use it before running `restore`.

Restore creates an isolated recovery database; the tool does not switch live
traffic or start application services. Quarantine expires all restored
API sessions, deletes pending refresh-recovery responses, consumes saved
pairing codes, revokes restored devices and SIP endpoint bindings, removes the
restored Asterisk realtime PJSIP auth/endpoint/AOR rows, and expires SIP
credential replay responses. Encrypted SIP credential ciphertext remains in
the database dump, but no environment secret is copied into the dump or
manifest. Keep the original `SECRETS_ENCRYPTION_KEY` in the secret manager; a
different key cannot decrypt ciphertext produced by the original deployment.
To use the recovered account, pair devices again, configure fresh SIP
endpoints, and verify each SIM-to-gateway mapping before bringing the isolated
server into service. Keep the old API, worker, and Asterisk stopped; use a new
Asterisk instance with no channels from the old deployment. This tool does not
perform that cutover automatically.

Every outbound `queued`, `accepted_by_gateway`, or `dispatching` SMS command is
changed to `unknown`, its message is updated to match, and dispatching parts
become `unknown`. The restore preserves message and command IDs, SMS rate
ledger, both SIM mappings, event history, and client receipts. It marks old
pending outbox notifications as processed so a restored notification cannot
report an outdated state. Unknown commands are not returned by the gateway
command list and cannot be claimed again; compare them with the carrier before
creating any replacement send.

Nonterminal calls become `unknown`, reserved call intents are cancelled, and
old call and participant wake nonces are retired. `gateway_call_slots` rows are
kept until ARI reconciliation proves that linked channels are gone. Never
restore a snapshot while the source Asterisk is serving calls or connect the
restored database to that old Asterisk: reconciliation deliberately hangs up
channels associated with unknown calls before releasing their slots. Use a
fresh isolated Asterisk with no old channels for any sandbox reconciliation.

A restored snapshot stops at its backup point. Later SMS, receipts, or account
changes are absent and must be synchronized again. Restoring an earlier
database cannot reverse SMS already sent, carrier charges, or a call already
placed. Compare uncertain outbound rows with carrier records before deciding
whether to submit a replacement. Verify the SIM mappings, then re-pair and
resynchronize clients as part of recovery.

The local PostgreSQL integration test exercises a real PostgreSQL 17 server,
including Unicode history on two SIMs, receipts, pending SMS quarantine,
corrupt-hash rejection, nonempty-target refusal, and transaction rollback when
quarantine SQL fails. Run it with `TEST_DATABASE_URL` set to an expendable
PostgreSQL 17 database and PostgreSQL 17 client tools installed. For direct
client mode, use `pg_dump`, `pg_restore`, and `psql` from the same major version
as the server. On Debian or Ubuntu, `pg_wrapper` may select an older installed
client even after installing `postgresql-client-17`; put
`/usr/lib/postgresql/17/bin` before `/usr/bin` in `PATH` when the server is
PostgreSQL 17, or use Compose mode:

```sh
python3 -m unittest discover -s scripts -p 'test_db_backup.py'
```
