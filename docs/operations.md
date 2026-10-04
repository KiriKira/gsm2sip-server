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
