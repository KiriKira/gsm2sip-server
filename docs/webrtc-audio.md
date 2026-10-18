# Experimental WebRTC voice with existing SIP call control

The host's **WebRTC 语音（实验）** switch freezes the audio transport for a
new outgoing call or an incoming invitation. Enabled calls use native
ICE / DTLS-SRTP / Opus between host and gateway. SIP/Asterisk still handles
registration, call authorization, incoming winner selection and call control.
The shared gateway occupancy and durable dispatch ledger remain authoritative.
An RTC failure ends the selected call; it does not silently dial again or fall
back to SIP audio. Turn the switch off before the next call for SIP audio.

Deploy the updated **API and Asterisk images**, plus gateway, before enabling
the host switch. Updating only the API leaves the old SIP RTP watchdog active
and can terminate a healthy WebRTC call after 30 seconds. Configure:

```dotenv
RTC_MEDIA_ENABLED=true
RTC_NODE_ID=your-stable-node-name
RTC_STUN_URLS=stun:turn.example.com:3478
RTC_TURN_URLS=turn:turn.example.com:3478?transport=udp,turn:turn.example.com:3478?transport=tcp
RTC_TURN_SHARED_SECRET=<openssl rand -hex 32>
```

Use a different stable node ID on each independent server. Keep the shared
secret private and configure the same value in that API and its coturn.
Both compose deployments and the all-in-one container forward these variables.
RTC is disabled by default. `/v1/capabilities` advertises `rtc_audio`; the
gateway registers its native SDK capability before a host can select RTC.

The supplied Asterisk build supports a bounded `GSM2SIP_RTC_UNTIL` channel lease.
After authorizing a negotiated RTC read, the API renews **only that participant's
SIP channel** for 12 seconds. Asterisk checks this lease every second; missing,
expired, malformed or more than 20 seconds ahead values retain the normal RTP
watchdog. No endpoint timeout is disabled or raised. SIP-only calls never obtain
this lease. Client loss, revoked authorization or API/ARI failure stops renewal;
the existing watchdog then tears down the control channel. This does not send
SIP speech, enable an audio fallback or authorize another GSM dispatch.

## Network ports

| Port | Purpose | Required when |
| --- | --- | --- |
| Existing HTTPS TCP 443 | Authenticated SDP/ICE relay and call APIs | Always; no additional signaling port |
| Existing SIP TLS TCP 5061 (or configured UDP 5060) | Existing call control | Always for this compatibility adapter |
| Existing UDP RTP range, default 10000–10199 | Existing SIP audio | SIP audio remains supported |
| TURN UDP **and** TCP 3478 | STUN and authenticated TURN allocation | Recommended for phones behind NAT |
| TURN UDP 49160–49259 | Bounded TURN media relay | Required with the supplied TURN deployment |
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
