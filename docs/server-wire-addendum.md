# Server v1 wire addendum

Updated 2026-10-04. This addendum fills request/response shapes omitted from
`protocol-v1.md`; it does not change the protocol's authority or its safety
rules. `openapi/openapi.yaml` is the machine-readable counterpart. All paths
are under `/v1`, JSON is UTF-8, IDs are UUIDs, and timestamps are UTC RFC3339.

## Pairing and sessions

`POST /pairings/claim` authorizes the initial device using a one-time pairing
code rather than a bearer session. Refresh and revoke use the refresh token
in their request body. Pairing request:

```json
{"pairing_code":"one-time-secret","device_name":"Windows host","platform":"windows"}
```

`platform` is optional, descriptive metadata. The server trims it; missing or
whitespace-only input is stored as `unknown`. Other values must match
`^[a-z][a-z0-9_-]{0,31}$` after trimming. This is an open label, not an OS
allowlist: examples include `android`, `windows`, and `linux`, and future
labels are accepted without a server upgrade. Platform does not grant a role,
root access, telephony permissions, SIP media support, or a client UI. The role
and owner come from the server-side pairing row, never the request. The pairing
response does not echo platform.
Success is `200`:

```json
{
  "owner_id":"UUID", "device_id":"UUID", "role":"client",
  "access_token":"opaque-random-token", "access_expires_at":"RFC3339",
  "refresh_token":"opaque-random-token", "refresh_expires_at":"RFC3339",
  "sip":{"available":false,"reason":"sip_not_configured"}
}
```

SIP credentials are omitted while SIP is not configured. An unavailable SIP
capability must not be interpreted as a bootstrap account. Access lifetime is
15 minutes; refresh lifetime defaults to 30 days. `POST /auth/refresh` takes
`{"refresh_token":"..."}` and returns the same four token fields as pairing.
Refresh is single-use and atomically replaces both tokens. Reuse of the old
refresh token with a different idempotency key is `401 SESSION_REVOKED`. When
the server's stable recovery-encryption key is configured, a client can send a
durably saved `Idempotency-Key` and retry the exact refresh request after a
lost response. The server keeps only the latest encrypted response for the
session and replays it only for the same old token and key while the session's
current refresh token remains valid. The cache expires at that token's
`refresh_expires_at`; replay does not rotate tokens or extend their lifetime.
Without the recovery-encryption key, refresh still rotates normally but a lost
response cannot be recovered. `POST /auth/revoke` takes the same body
and returns `204`; it revokes the refresh family and its current access token.

All authenticated HTTP and WebSocket requests use
`Authorization: Bearer <access_token>`. Never put credentials in URLs. Writes use `Idempotency-Key`; its namespace is
`owner_id + device_id + operation`. Same key and body replays the original
resource; same key and a different body returns `409 IDEMPOTENCY_CONFLICT`.

`GET /clients` is client-role only and lists every paired client belonging to
the authenticated owner's account, including devices whose pair state is
`revoked`. It returns only `{"items":[{"id":"UUID","name":"Host",
"platform":"windows","state":"active","is_self":true}]}`. The
current authenticated device has `is_self:true`; exactly one returned item is
self. No token, endpoint, SIP username, or password is exposed. This is a
device list, not an account-sharing surface. Every host retains its own access
and refresh session, SIP endpoint/AOR, local cursor receipts, and operation
idempotency namespace.

## Wake-only WebSocket

`GET /v1/ws` upgrades an authenticated connection for either a `client` or
`gateway` device. Send the current access token in the `Authorization: Bearer`
header; query parameters (including token-like parameters) and request bodies
are rejected. No WebSocket subprotocol is required. Clients do not send
application messages; the server closes such a connection with code `1008`.
An application frame larger than 4 KiB closes with `1009`. Browser connections
must have the same origin as the request host. Native Android clients may omit
`Origin`.

Immediately after the upgrade the server sends exactly:

```json
{"protocol_version":1,"type":"sync_required"}
```

It repeats that same frame when the caller's marker changes. A client marker
tracks two independent sources: the maximum durable `server_events.cursor` for
that client's owner, and the maximum `call_events.cursor` for that exact
`owner_id + client_device_id`. Owner SMS events wake every paired client; call
participant changes wake only that participant's device. These markers are
not merged into one durable cursor and the frame does not reveal either value.
A gateway marker covers only commands matching its authenticated device and owner: queued
commands count only until expiry, while accepted or dispatching commands remain
visible until terminal. Marker contents are internal and never appear in the
frame. Hints may repeat or be coalesced; clients must use their HTTPS durable
cursor and gateways must use HTTPS command sync to recover after disconnects or
missed hints. A wake frame does not mean a call is ready and carries no event,
command, number, owner, device, or cursor data.

After any client wake hint, the client syncs its owner-scoped SMS event cursor
and local SQLite receipt separately from its own paginated `GET /calls` view.
Call state uses each participant's `state_revision`; `/events/ack` acknowledges
only the reported SMS durable cursor and is not a call acknowledgement. The
server does not expose a separate call-events read/ack API. Reconciliation
reads current per-client call snapshots, while `sync_required` remains only a
wake signal.

The server sends a WebSocket ping every 20 seconds and closes a connection if
no pong arrives within 45 seconds. It checks the same access token and device
state about once per second; expiry, revocation, token rotation, or an owner,
device, or role change closes the connection with `1008`. A per-device limit
of two connections applies. A shutdown closes active connections with `1001`.
Upgrade errors use the usual HTTP errors before the `101` response (`401` for
invalid credentials, `403` for a disallowed origin, and `429` for the device
connection budget).

Production clients connect with `wss://` and keep platform CA trust and
hostname verification enabled. The API may run over private HTTP behind a
trusted Caddy TLS terminator. Plain `ws://` is for local development and tests.
This endpoint is only a synchronization hint; it does not implement FCM,
incoming-call push, call-ready state, SIP, or ARI.

## Heartbeat and two-phase SIM binding

`POST /gateways/{gateway_id}/heartbeat` request:

```json
{
  "sequence":42,"protocol_version":1,"app_version":"1.0.0","root":true,
  "sip_registered":false,"battery_percent":87,"charging":true,
  "sim_states":[{"sim_id":"UUID","mapping_revision":3,
    "service_state":"in_service","identity_verified":true}]
}
```

The response is `200 {"server_time":"RFC3339","online":true,
"stale_after_seconds":90,"mapping_revision":4,"invalidated_sim_ids":[]}`.
Send about every 30 seconds. `sim_id` values must already have been issued by
this server. Server online state is based on `last_seen_at`; SIP registration
is an independent field. The request is a full snapshot of every known SIM;
report absent or uncertain cards with `identity_verified:false`. If an active
SIM is reported unverified or omitted, the server changes it to `unverified`,
increments the gateway-wide mapping revision, updates all current bindings to
that revision, and returns the revoked IDs. Persist this response before
processing commands. Replaying the same sequence and body returns the stored
response if the HTTP response was lost. A later heartbeat with an old revision
returns `409 SIM_MAPPING_CHANGED`; fetch `/gateways/{id}/sims` and resync.
Heartbeat cannot reactivate a SIM; only local confirmation can.

The same `POST /gateways/{gateway_id}/sim-bindings` path has two phases. A
proposal request is:

```json
{"operation_id":"UUID","phase":"propose","mappings":[
  {"slot_index":0,"label":"Work","carrier_name":"Carrier",
   "phone_number":"+8613800000000"},
  {"slot_index":1,"label":"Moved card","existing_sim_id":"UUID",
   "same_sim_verified":true}
]}
```

The server allocates stable `sim_id` values and a new gateway-wide
`mapping_revision`, returning `200 {"operation_id":"UUID","phase":"proposed",
"mapping_revision":4,"mappings":[{"sim_id":"UUID","slot_index":0,
"state":"pending_local_confirmation","mapping_revision":4}]}`. The gateway
must persist the IDs and ask for explicit local confirmation before reporting
the mapping active. Confirmation request:

```json
{"operation_id":"UUID","phase":"confirm","confirmations":[
  {"sim_id":"UUID","slot_index":0,"confirmed":true}
]}
```

The response has `phase:"confirmed"` and mappings with state `active` or
`unverified`. A false confirmation is retained as unverified and cannot be
used for SMS or calls. The server trusts the authenticated gateway's
confirmation statement; it cannot independently inspect the modem or SIM.
When moving a known card, include `existing_sim_id` only after the phone's
local identity check verifies it is the same SIM and set
`same_sim_verified:true`; otherwise omit both and the server issues a new
UUID. SIM fingerprints, ICCID and IMSI stay on the phone. Replaying a phase
with the same operation and content returns the original response; a changed
payload returns `409 IDEMPOTENCY_CONFLICT`.

## Commands and event journal

`GET /gateways/{id}/commands?cursor=...&limit=100` returns
`{"commands":[{command_id,message_id,sim_id,mapping_revision,to,text,
expires_at,payload_sha256,state}],"next_cursor":null}` for queued and
nonterminal claimed commands, ordered oldest first. The default page is 100
and the maximum is 100. Begin each full queue sync without a cursor and follow
`next_cursor` until null; the cursor only advances through that sync. The
server computes `payload_sha256` over UTF-8 fields joined by a
single NUL byte, in this order: `command_id`, `message_id`, `gateway_id`,
`sim_id`, decimal `mapping_revision`, `to`, `text`, and `expires_at` formatted
as UTC RFC3339Nano. NUL is rejected in SMS text. The digest excludes status,
creation time and modem-derived part count, so retries remain stable. A claim
is `POST /gateways/{id}/commands/{command_id}/claim` with
`{"payload_sha256":"the exact hash returned by GET"}`. The response includes
the same hash, immutable command fields and `state:"accepted_by_gateway"`. A
claim is sticky to that gateway and idempotent: response loss may repeat the
same command; expiry or mapping mismatch prevents a first claim and returns
409 (`COMMAND_EXPIRED` or `SIM_MAPPING_CHANGED`). Claimed work is never
transferred to another gateway. The gateway's durable execution ledger is
still required; `unknown` work must never be sent again.

`POST /gateways/{id}/events:batch` accepts `{ "events": [...] }`, at most 50
events and 256 KiB. The envelope is the one in `protocol-v1.md`. Supported
M2 event types:

* `sms.received`: payload `{message_id,from,text,parts}`. `sim_id` and
  `mapping_revision` may be null only when SIM resolution is unknown.
* `sms.dispatching`: the first execution event for an outbound command;
  payload `{message_id,command_id,part_count}`. `part_count` comes from the
  target handset's `SmsManager.divideMessage()` result, computed before any
  modem side effect and persisted by the gateway. The server never guesses
  it from text length or Unicode characters.
* `sms.command_state`: payload `{message_id,command_id,state,error?}` for a
  terminal outcome before modem division (`expired` or `failed`) or an
  ambiguous execution (`unknown`). This creates no part rows when the device
  has not called `divideMessage()`; `part_count` stays null. A late command
  outcome cannot override an already dispatching/submitted/delivered message.
* `sms.part_state`: payload `{message_id,command_id,part_index,state,
  result_code?,error?}`. State is `submitted`, `delivered`, `failed`,
  `expired`, or `unknown`; the separate `sms.dispatching` event initializes
  every part as dispatching. `part_index` is zero-based. `error` is
  a bounded, user-displayable diagnostic; do not put secrets in it. Part
  states never regress: delivered cannot return to dispatching, and unknown
  can advance only when a real late modem callback provides evidence.

All items are stored in one PostgreSQL transaction. The response is sent only
after commit: `200 {"acks":[{"event_id":"UUID","event_cursor":"opaque",
"duplicate":false}]}`. Duplicate event ID plus identical canonical payload
returns its original cursor; a different payload is `409 IDEMPOTENCY_CONFLICT`.
Sequence collisions with another event are also conflicts. A failed batch has
no ACKs and no partial writes. The gateway deletes journal rows only after
receiving and persisting each ACK.

## Message create and pagination

`POST /messages` uses the protocol example fields and returns `202` with
`{message_id,command_id,status:"queued",created_at,expires_at}`. The server
does not know `part_count` until the gateway reports `sms.dispatching`.
`GET /messages/{id}` returns message fields plus ordered `parts:[{part_index,
state,result_code?,error?}]`. Its complete detail shape is
`{message_id,command_id,gateway_id,sim_id,mapping_revision,direction,from,to,
text,status,part_count,created_at,expires_at,parts}`. Nullable fields are
explicitly null. `part_count` is null until modem division is reported; inbound
messages have direction `inbound` and state `received`. Outbound status values
are `queued`, `accepted_by_gateway`, `dispatching`, `submitted`, `delivered`,
`failed`, `expired`, and `unknown`. List endpoints return `{items,next_cursor}`;
`next_cursor` is null at the end. `GET /gateways` items are
`{gateway_id,device_name,online,last_seen_at,mapping_revision,sequence,
protocol_version,app_version,root,sip_registered,battery_percent,charging}`.
`GET /gateways/{id}/sims` is `{mapping_revision,sims:[{sim_id,slot_index,label,
carrier_name,phone_number,state,mapping_revision,identity_verified,
service_state}]}`; carrier, number, and service state may be null. SIM states
are `pending_local_confirmation`, `active`, `unverified`, and `removed`.
`GET /events` items are `{cursor,event_id,type,occurred_at,message}`. Event type
is `message.created`, `message.received`, or `message.state_changed`; `message`
has the current MessageDetail projection, read in one consistent database
snapshot for the page. Event type and occurrence time remain historical; the
message body is refreshed so replaying an old `message.created` event cannot
roll a host back from a later delivered state. `resync_required` is false while server
events are not pruned. Cursors are opaque to clients and are always
combined with the caller's owner/device scope. Page size defaults to 50 and is
bounded at 100. `GET /events` returns `{items,next_cursor,resync_required}`;
its cursor represents committed server events, not a gateway's sequence.

Common errors are `{error:{code,message,retryable},request_id}`. `401` means
missing/expired/revoked credentials; `403` means role or owner scope failure;
`404` hides out-of-scope IDs; `409` means idempotency, revision, or state
conflict; `413` means the event batch limit was exceeded; `429` is a budget
limit; and `503` means the capability is not configured. `retryable:true`
only allows retrying the same resource/key.

## SIP credentials and call control

Every paired device has its own endpoint/auth identity and AOR, including all
host clients under one owner. Each AOR allows at most one registered contact;
separate AORs allow multiple hosts to be reachable without multiple contacts
competing under one device identity. `GET /devices/self/sip-config` returns non-secret TLS configuration. When configured,
`POST /devices/self/sip-credentials/rotate` with a persisted `Idempotency-Key`
returns a new password once, with encrypted replay for five minutes. The same key
never rotates twice; an expired/obsolete key fails explicitly. Asterisk stores a
Digest A1 hash rather than the plaintext password. The TLS certificate server
name is distinct from the fixed SIP Digest realm `gsm2sip`.

`POST /call-intents` returns HTTP 202, reserves the owner's sole gateway call
slot for 30 seconds, and does not dial a carrier call. The slot is unique to the
gateway across all clients and SIM bindings. Only an authenticated client SIP
INVITE may consume the one-use token. Gateway routing uses the four trusted
`X-GSM-*` headers from protocol v1; the client leg uses `X-GSM2SIP-Call-ID`.
Outgoing calls remain a single client-created intent tied to that client and
chosen SIM. They never fall back to a host's local SIM or another paired SIM.

For an incoming cellular call, the server snapshots owner client devices that
are active, have an active SIP endpoint binding, and have at least one access
or refresh session that has not expired. A temporarily disconnected host with
a live refresh session is still eligible; endpoint-online status is checked
when it completes `/ready`. New clients paired after the snapshot join the next
incoming call, not the call already ringing. Each eligible client gets an
independent participant row sharing one `call_id`, its own `wake_nonce`, state,
and monotonic `state_revision`. `GET /calls` returns the caller's latest
participant snapshots, paginated by descending `created_at,call_id`; it is a
current-state snapshot, not a revision event timeline. `GET /calls/{id}` returns
only that call's caller-scoped projection. For incoming rows, the existing JSON field
`client_id` is the authenticated requesting device. A pending row exposes only
that device's `wake_nonce`; after its `/ready` succeeds the nonce is no longer
returned. `/ready` checks the authenticated device, call, nonce, pending
participant, deadline, current active device/binding and that device's actual
registered contact before dialing its own AOR. A nonce from another host cannot
ready this participant. A revoked device or device with no live access/refresh
session is excluded from new incoming snapshots; if a participant becomes
invalid during an active incoming call, that participant is ended without
joining another client identity to it.

`ready`, push delivery, REGISTER, and local UI state do not select the call
winner. The first authenticated client PJSIP channel that actually reaches
`Up` wins through an atomic selection. All other participants end with
`reason:"answered_elsewhere"`; a single host declining ends only its own
participant and leaves the others eligible. When the last participant declines,
or the 25-second incoming deadline expires, all participants end as
`no_answer`, the call ends, and the single gateway slot is released. A cellular
hangup also ends all related participants. One participant's revision and call
event are scoped to that device; no losing host should continue ringing.

Expiry/cancellation cannot revive a call. ARI disconnection/restart must
reconcile uncertain legs. An unknown call keeps its gateway lease while any
related channel remains or channel absence cannot be confirmed. Once a fresh
ARI inventory confirms that every related channel is gone, the call becomes
terminal and releases the lease. A time-based reservation expiry never releases
an active or uncertain call.

Calling requires configured SIP credentials, a connected ARI coordinator and
eligible device/SIM state. Missing configuration returns 503. Database `/readyz`
and SIP registration alone do not certify media capability. FCM and ICE/TURN
are not implemented. The host implements bounded IP-change/session recovery;
media resumption across real networks still requires interoperability testing.
