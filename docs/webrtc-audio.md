# Experimental WebRTC voice with existing SIP call control

The host's **WebRTC 语音（实验）** switch freezes the audio transport for a
new outgoing call or an incoming invitation. Enabled calls use native
ICE / DTLS-SRTP / Opus between host and gateway. SIP/Asterisk still handles
registration, call authorization, incoming winner selection and call control.
The shared gateway occupancy and durable dispatch ledger remain authoritative.
An RTC failure ends the selected call; it does not silently dial again or fall
back to SIP audio. Turn the switch off before the next call for SIP audio.

Deploy updated API and gateway before enabling the host switch. Configure:

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
