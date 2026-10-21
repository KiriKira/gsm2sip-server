# Independent Go call control, version 1

With `RTC_CONTROL_ENABLED=true` and `RTC_MEDIA_ENABLED=true`, the upgraded Host
WebRTC switch selects Go business call control and native ICE/DTLS-SRTP/Opus.
The switch is in Settings; the dialer keeps its contact suggestions. Each call
freezes its actual control and media adapters. Logs on API, Host and Gateway
include `call_id`, `call_control=rtc|sip`, and media transport. Failed RTC calls
end without SIP fallback, a second modem dispatch or a new call ID.

`SIP_COMPATIBILITY_ENABLED=true` is the default for existing deployments. Keep
the existing API/Asterisk configuration and credentials during migration.
Legacy clients and the Host switch's off setting keep the original SIP engine.
An upgraded Host uses an optional SIP incoming listener only when the server
advertises that compatibility adapter. Its new outgoing RTC calls use Go.
The entire incoming participant set chooses one adapter before any invitation:
if any authorized client has RTC off or has not advertised RTC, the whole fork
uses SIP. A committed RTC invitation never turns into a SIP invitation.

Set `SIP_COMPATIBILITY_ENABLED=false` after upgrading or revoking obsolete
clients. This skips ARI/SIP initialization, SIP credentials and Asterisk in the
all-in-one entrypoint. `deploy/compose.rtc.yaml` runs API, PostgreSQL and the
outbox worker without an Asterisk image. Pure RTC clients do not initialize
PJSUA2 or read SIP credentials. Existing pairing, SIM mappings, encryption keys,
SMS feeds and node identities stay in their existing stores. This is not a new
pairing protocol; preserve the database and stable encryption key on upgrade.

## HTTPS operations

All requests use the existing paired bearer session under `/v1`, with version
`1`. The [OpenAPI contract](../openapi/openapi.yaml) describes request fields.

| Method and path | Meaning |
| --- | --- |
| `POST /rtc/control/capability` | Explicit `{version:1, enabled:true|false}`; renew every 25 seconds, fresh for 90 seconds |
| `GET /rtc/control/incoming-policy` | Gateway reads whole-fork adapter choice; incoming creation rechecks it |
| `POST /rtc/control/calls` | Host reserves an outgoing call, without dialing |
| `POST /rtc/control/incoming` | Gateway reports a genuine ringing cellular call to each eligible node |
| `GET /rtc/control/calls` | At most 32 device-scoped snapshots; suggested poll interval 750 ms |
| `GET /rtc/control/calls/{id}` | Current authenticated snapshot and participant lease renewal |
| `POST /rtc/control/calls/{id}/operations` | Durable operation UUID, expected revision and operation kind |

Create includes `operation_id`, installation-wide `call_id`, node `gateway_id`,
stable `sim_id`, `mapping_revision`, and `to`. Only incoming creation may report
`from`; it is never a client-supplied outgoing caller identity. UUIDs are
canonicalized before hashing. Retrying the same UUID and payload returns its
committed ACK, including after API restart. Reusing it with another payload
returns a conflict. A revision conflict requires rereading authority before
submitting a new operation UUID. A timeout retries the exact operation, not a
new cellular effect. Current credentials are checked even for an old ACK.

Outgoing sequence: reserve → Host `start` → both peers negotiate media →
Gateway `dispatch` → local durable dispatch ledger → real Telecom dial → real
cellular ACTIVE and started PCM → media active report → Gateway `active`.
Neither reservation, ICE connectivity nor a client request alone means ACTIVE.

Incoming sequence: genuine cellular ring → one call ID offered through eligible
nodes → client `accept` reserves a candidate for at most 20 seconds → native
media preparation → Gateway installation-wide SQLite winner CAS → server
`winner` → exactly one real Telecom answer. Losing nodes/participants receive
`answered_elsewhere`; one client's rejection leaves other candidates ringing.
PCM stays closed before answer and real cellular ACTIVE.

The remaining operations are `ringing`, `release`, `reject`, `cancel`, `hangup`,
`ended` and `answered_elsewhere`, with actor and state checks in the API.
Invitation deadlines are 25 seconds incoming and 30 seconds outgoing. Polling
cannot extend them. Both device identities, the exact current bearer session,
owner, participant, SIM revision and shared gateway slot are rechecked.

## Failure, restart and shared occupancy

Go checks control leases every five seconds, with a 20-second lease. Expired
dispatched calls become `unknown` with end requested; their server slot remains
occupied until the Gateway reports genuine cellular END. An undispatched
outgoing reservation can safely end and release its slot. Terminal/unknown
calls cannot resume or dispatch again. Only the Gateway can resolve unknown by
confirmed END. SIP reconciliation, fail-closed cleanup and preemption never
retire RTC slots.

Gateway SQLite retains an installation-wide cellular slot, per-call dispatch
ledger, immutable winner and durable per-node terminal queue. Legacy SIP
dispatch also checks this slot. Restart with an uncertain modem effect does
not redial; confirmed cellular END atomically clears local occupancy and queues
node cleanup. An active call stays on its original node when a higher-priority
node returns. Idle selection can fail back after health recovery.

## Configurable network ports

Call control, SDP and ICE signaling reuse the existing HTTPS API port. No new
TCP signaling listener is required. With SIP disabled, no SIP or SIP RTP ports
are required. During compatibility, retain the configured SIP and RTP ports.

TURN is recommended for mobile NATs. All additional supplied TURN listeners
are configurable:

```dotenv
TURN_LISTEN_PORT=13478
TURN_RELAY_MIN_PORT=52000
TURN_RELAY_MAX_PORT=52099
RTC_STUN_URLS=stun:turn.example.com:13478
RTC_TURN_URLS=turn:turn.example.com:13478?transport=udp,turn:turn.example.com:13478?transport=tcp
```

Open TCP **and** UDP 13478 and UDP 52000–52099 on TURN's firewall, cloud security
group and NAT. Defaults are 3478 and 49160–49259. Keep relay port numbers
unchanged through NAT and advertise the correct public/private address pair.
The API and coturn must use the same private TURN shared secret. Phones receive
listener URLs through the API; relay ports are learned through ICE. Invalid,
reversed and overlapping ranges are rejected before coturn starts. The supplied
template does not enable TURN TLS. Details: [WebRTC media](webrtc-audio.md).

## Validation boundary

Server tests use real PostgreSQL with no SIP bindings or ARI controller for
independent control, durable retry/restart, concurrent accept and dispatch,
revocation, unknown-slot retention and isolation from SIP cleanup. Joint KVM
tests use real Android UI, virtual modem/Telecom and native WebRTC. The debug
instrumentation's synthetic PCM replaces only physical call-audio hardware.
Passing those tests does not establish physical handset HAL audio; that last
step requires a real Gateway handset. Lab test keys and injected test backends
are excluded from the permanent-signed published application builds.
