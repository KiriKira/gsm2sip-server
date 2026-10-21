# WebRTC voice and optional SIP compatibility

The upgraded Host **WebRTC 语音** switch in Settings selects native
ICE / DTLS-SRTP / Opus. With `RTC_CONTROL_ENABLED=true`, it also selects
independent Go call control, without a SIP or Asterisk prerequisite. See
[control protocol, migration and pure RTC deployment](rtc-call-control-v1.md).

Keep `SIP_COMPATIBILITY_ENABLED=true` while supporting existing clients.
The switch off setting retains original SIP calls. On older servers without
independent control, WebRTC audio can still use the existing SIP-controlled
transition adapter. The actual selected control and media are shown per call
and logged by all three endpoints; a failed RTC call never silently falls back.

For the all-in-one image, enable RTC_CONTROL_ENABLED and RTC_MEDIA_ENABLED.
The image automatically supervises coturn, derives its stable authentication
secret from the existing install encryption key, listens on 0.0.0.0 and discovers
the relay address. No extra node ID, TURN secret, IP, realm or URL is required.
TURN_LISTEN_PORT / TURN_RELAY_MIN_PORT / TURN_RELAY_MAX_PORT remain optional
port overrides; forward/open the chosen ports in your firewall and NAT.
When public STUN discovery is unavailable behind NAT, the image also reuses
an existing PUBLIC_IP; TURN_PUBLIC_IP remains an optional advanced override.

For a separately managed external TURN service, configure:

```dotenv
RTC_CONTROL_ENABLED=true
RTC_MEDIA_ENABLED=true
RTC_STUN_URLS=stun:turn.example.com:3478
RTC_TURN_URLS=turn:turn.example.com:3478?transport=udp,turn:turn.example.com:3478?transport=tcp
RTC_TURN_SHARED_SECRET=<private-shared-secret>
```

RTC reuses the existing owner, paired device, session, call, gateway and SIM
identities. Servers need no extra RTC node ID, including for mesh failover.
Keep the TURN shared secret private and equal in its API and coturn. Compose and the all-in-one
container forward these variables. RTC media is disabled by default.

For the older SIP-controlled WebRTC adapter, deploy both updated API and
Asterisk images. Its negotiated peer renews only its authorized SIP channel's
bounded 12-second `GSM2SIP_RTC_UNTIL` lease, without raising the normal RTP
watchdog timeout. **Independent Go/RTC calls never renew this ARI lease and do
not need Asterisk.**

## Network ports

| Port | Purpose | Required when |
| --- | --- | --- |
| Existing HTTPS TCP 443 | Authenticated SDP/ICE relay and call APIs | Always; no additional signaling port |
| Existing SIP TLS TCP 5061 (or configured UDP 5060) | Optional legacy call control | Only with SIP compatibility |
| Existing UDP RTP range, default 10000–10199 | Existing SIP audio | SIP audio remains supported |
| TURN UDP **and** TCP `TURN_LISTEN_PORT` (default 3478) | STUN and authenticated TURN allocation | Recommended for phones behind NAT |
| TURN UDP `TURN_RELAY_MIN_PORT`–`TURN_RELAY_MAX_PORT` (default 49160–49259) | Bounded TURN media relay | Required with the supplied TURN deployment |
| Optional TCP 5349 | TURN over TLS | Only if separately configured with a valid certificate |

Direct ICE audio travels between the phones and does not pass through an API
media listener. Direct connectivity is not assured across mobile NATs. TURN
adds the ports shown above; API HTTPS alone does not relay audio. A TCP TURN
client connection still needs the server's UDP relay range. TURN/TLS is not
enabled by the supplied template and is not implied by opening TCP 5349.

`deploy/turn/compose.yaml` is an optional, digest-pinned coturn deployment using
Linux host networking and a 100-port relay range. Add `TURN_PUBLIC_IP`,
`TURN_PRIVATE_IP` (the host interface address) and `TURN_REALM` to the private
env file. For EC2 public IPv4 NAT use the actual public/private address pair;
security group, host firewall and NAT must preserve the relay port numbers.
The template denies relay access to private/internal peer addresses. Private
VPN deployments need an explicitly reviewed peer-address policy of their own.

All additional TURN ports are configurable in the same private env file. For example:

```dotenv
TURN_LISTEN_PORT=13478
TURN_RELAY_MIN_PORT=52000
TURN_RELAY_MAX_PORT=52099
RTC_STUN_URLS=stun:turn.example.com:13478
RTC_TURN_URLS=turn:turn.example.com:13478?transport=udp,turn:turn.example.com:13478?transport=tcp
```

This opens TCP/UDP 13478 and UDP 52000–52099 on the TURN host. Apply the same
values to the firewall, cloud security group and NAT; update the API's advertised
URLs and restart API and TURN. Phones obtain the URL from the API and require no
TURN port setting. The relay range is learned through ICE, so it does not go in
`RTC_TURN_URLS`. The entrypoint rejects invalid ports, reversed ranges and overlap
with the listener before starting coturn. Ports must be free on the host. The
optional TLS listener remains disabled; this setting does not enable TLS.

Run independently from the normal stack:

```sh
cd deploy/turn
docker compose --env-file ../.env up -d
```

## Authenticated adapter endpoints

All paths use the existing paired bearer session under `/v1`:

| Method and path | Scope |
| --- | --- |
| `POST /devices/self/rtc-capability` | Gateway advertises native SDK version 1; renewed by normal heartbeats |
| `POST /calls/{call_id}/media` | Host selects `{"media_transport":"webrtc"}` before SIP dial/answer; retry returns the same leg |
| `GET /calls/{call_id}/media` | Bound host/gateway reads the immutable choice; gateway waits for an incoming winner |
| `GET /rtc/legs/{leg_id}` | Bound peer reads committed signals and renews authorization against the current session and business lease |
| `POST /rtc/legs/{leg_id}` | Strict RTC v1 offer/answer/candidate/EOC/close command; durable ACK before publication |
| `POST /calls/{call_id}/media/active` | Gateway reports its negotiated leg only after real Telecom ACTIVE and audio backend start |

The caller never supplies a trusted owner/client/gateway/SIM binding. The adapter
rechecks device revocation, session, participant/winner, shared gateway slot and
SIM identity/revision. RTC negotiation cannot allocate another cellular executor.
Signal replay expires after 15 seconds; an already connected peer keeps its
applied native state and continues authorization renewal. A new peer cannot
recover from missing replay by pretending the call is connected. Current clients
end the selected call after unrecoverable ICE/signaling failure; they do not
start another GSM attempt or switch audio transport inside that call.

## Logs and acceptance scope

Host and gateway log `GsmRtc media=webrtc call_control=sip call_id=...` with
selection, committed SDP operation types, ICE/DTLS state, audio gating,
packet/PCM counters and selected candidate types. The API records the same
call/leg IDs and authorized operations. SDP, credentials and DTMF digits are
not included in these diagnostics. UI displays the frozen media transport;
SIP registration status describes call control and does not prove audio mode.

An emulator can prove real native encrypted media with synthetic PCM replacing
physical audio hardware, along with Telecom call events and application call
control. It cannot establish that a phone's modem/HAL supports capture and
injection of real cellular call audio. That final validation requires a rooted
supported device and a real call. Do not treat a standalone peer probe as
three-component call acceptance.
