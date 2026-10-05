# Local and production operations

## Local Asterisk probe

The local Compose stack builds Asterisk from pinned upstream sources instead of
pulling an unversioned image. The current build uses Asterisk 22.11.0 (release
archive SHA-256 `3bd5ee040509a3d3cd9b1ba9520c18e6ec0a7e7981ca68c457dcd36ba3c54d94`),
the Debian Bookworm base image pinned by digest, and PJProject commit
`a67b8e81b0024b993f47e463c01c67c25cda116f` with archive SHA-256
`86b7bb049ad33b27f487d057d92f217717c40c1bcb4233c0e8db9f964d34cbc5`. The
PJSIP snapshot includes upstream TLS/DNS validation fixes newer than the PJSIP
copy bundled with that Asterisk release. The Open Source Opus translator is
also pinned to commit `a959f072d3f364be983dd27e6e250b038aaef747` and checked
by the local codec translation probe before the deployment claims Opus support.
The client codec profile keeps Opus disabled by default (`SIP_ENABLE_OPUS=false`);
enable it only after both-direction sample transcoding passes on the target image.

Run `scripts/asterisk-local-probe.sh` on a machine with Docker Compose, OpenSSL,
and Python 3. It creates a short-lived self-signed certificate for
`sip.localhost` under the ignored `.local/` directory, copies `.env.example` to
`.env` if needed, generates a private stable 32-byte encryption key when the
local file has no key, builds the Asterisk and API images, and starts the local
PostgreSQL/Asterisk/API services. The sample `.env` values are for local
development only.

The probe makes a real HTTP request to ARI inside the Asterisk container and
checks that Compose does not publish port 8088. It waits for API migrations,
creates an isolated realtime PJSIP endpoint with only an A1 Digest hash in
`ps_auths`, then verifies that system TLS trust rejects the self-signed
certificate, explicit local trust accepts it, and an authenticated INVITE with
an RTP offer lacking SDES crypto receives SIP 488. It checks the configured
30-second RTP and 60-second hold timeouts and executes one-second sample
transcodes between Opus and A-law, G.711 mu-law, and G.722 in both directions.
The ARI wrapper probe creates Local channels, confirms no INVITE is sent before
the single `/dial`, then checks exactly one INVITE per leg, the numeric gateway
destination, and each expected trusted header at a local TLS UAS. The UAS
answers with SDES-SRTP. This validates signaling and negotiation; it does not
assert an RTP audio round trip. No carrier, SIM, or external number is contacted.
Stop local containers with `docker compose down`;
`docker compose down -v` also removes the local PostgreSQL volume.

## Deployment settings

Supply a stable base64-encoded 32-byte `SECRETS_ENCRYPTION_KEY` from a secret
manager. Keep it unchanged across API restarts and back it up with the database
because it encrypts one-time SIP credential replay responses. Configure
`ARI_USERNAME` and a random `ARI_PASSWORD` consistently for the API and
Asterisk. The API uses `ARI_URL=http://asterisk:8088/ari` on the private
Compose network; Compose intentionally publishes no ARI or AMI port.

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

Keep that `.env` file and reuse the same key. Generating a new value after SIP
credentials have been stored will make their encrypted replay responses
unreadable.

Set `SIP_SERVER_NAME` to the public DNS name in the Asterisk certificate SAN,
and `SIP_PORT=5061`. Keep `SIP_ENABLE_OPUS=false` until the pinned translator's
local sample-transcode probe passes on the deployed image; enable it explicitly
only when client Opus negotiation is supported. Mount a production CA-issued certificate and matching key
through `ASTERISK_TLS_CERT_FILE` and `ASTERISK_TLS_KEY_FILE`. The self-signed
local certificate must never be used in a public deployment. For a publicly
trusted certificate, leave `SIP_CA_PEM_FILE` empty; set it only when clients
must receive a private CA certificate. Set
`ASTERISK_EXTERNAL_SIGNALING_ADDRESS` and `ASTERISK_EXTERNAL_MEDIA_ADDRESS` to
the public signaling/media address, and set `SIP_BIND_ADDR=0.0.0.0` when the
SIP listener must accept traffic beyond the local host.

Permit only the configured SIP TLS listener on TCP 5061 and the configured
RTP/RTCP UDP range (default 10000–10199) through the host firewall. The service
requires TLS 1.2 or newer and PJSIP SDES-SRTP (`media_encryption=sdes`,
`media_encryption_optimistic=no`); media is anchored with `direct_media=no`.
Endpoint rows are provisioned by the API into PostgreSQL realtime tables. SIP
authentication stores `MD5:<A1>` in `ps_auths.password_digest`; no plaintext
SIP password column is used. PJSIP endpoints use TLS transport and the NAT
options `rtp_symmetric`, `force_rport`, and `rewrite_contact`.

The client and gateway dialplan contexts are separate. Client call intent comes
only from the `call.<one-shot-token>` request URI and is passed to the private
ARI Stasis app without dialplan logging. Client-supplied X-GSM headers are
ignored. Gateway call metadata is read only on the Digest-authenticated
`gsm-gateway` endpoint and validated again by the API coordinator. Outbound
gateway signaling adds the four trusted `X-GSM-*` metadata headers; the client
leg uses the distinct `X-GSM2SIP-Call-ID` correlation header.

ARI creates `Local/s@gsm2sip-ari-gateway/n` or
`Local/s@gsm2sip-ari-client/n` wrappers in the `gsm2sip` Stasis app and dials
those wrappers. The Local dialplan uses a pre-dial hook to add trusted headers
before the PJSIP INVITE and places the PJSIP child channel in Stasis after
answer. The coordinator correlates on the child PJSIP channel name, while the
wrapper remains a control channel. Keep ARI and its credentials on the private
network.

## PostgreSQL backup and isolated restore

The server backup contains the complete PostgreSQL database in custom
`pg_dump` format. It covers SMS bodies and parts, gateway event history,
commands, identities, SIM mappings, call state, receipts, audit rows, and the
encrypted SIP credential replay records. It is not an encrypted archive: the
dump contains private message and phone data, access-token hashes, SIP digests,
and encrypted credential ciphertext. Store it on an encrypted disk in a
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
PostgreSQL 17 database and PostgreSQL 17 client tools installed:

```sh
python3 -m unittest discover -s scripts -p 'test_db_backup.py'
```
