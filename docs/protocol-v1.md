# gsm2sip 三端协议 v1（实现目标，尚未实现）

更新：2026-10-03。权威源在 `KiriKira/gsm2sip-server/docs/protocol-v1.md`；另两仓库用链接和固定协议提交号引用，不独立修改副本。所有 v1 示例是待实现的接口，不表示当前服务已存在。

## 1. 固定架构与边界

| 路径 | v1 约定 |
|---|---|
| 旧 root Android ↔ VPS 的呼叫 | SIP/TLS；SDES-SRTP `AES_CM_128_HMAC_SHA1_80`；VPS Asterisk/PJSIP 锚定媒体 |
| 未 root Android ↔ VPS 的呼叫 | App 内成熟 SIP SDK，使用相同安全配置；不自行编写 SIP/音频协议栈 |
| 两端 ↔ VPS 的短信、状态、命令 | HTTPS JSON + WSS 通知；数据库提交后才产生应用层确认 |
| VPS 内部 | 控制服务 ↔ Asterisk 的 ARI/AMI 只在内部网络或 loopback；数据库、管理接口不对公网开放 |

生产不静默降级到 UDP SIP/明文 RTP。不建立旧手机到主机的直连媒体。TLS+SRTP 是分段加密，VPS 能处理媒体及短信，不能宣称端到端加密。旧网关已有 SIP MESSAGE 仅作为基线测试或显式迁移适配；v1 生产短信执行器只接受 HTTPS 命令，不得两条通道各发送一次。

## 2. 身份、配对与权限

- `owner_id` 是唯一拥有者；`gateway_id`、`client_id` 是服务端签发的 UUID。v1 支持一个拥有者、一个网关、两张 SIM、一个主客户端，表结构保留多设备能力。
- 初始管理员用 VPS 本地 CLI 建立，随后生成一次性、短时配对码/二维码。配对码绑定角色（gateway/client），消费一次即失效，限速；不用公开注册或仓库内默认密码。
- HTTPS access token 短期有效（目标 15 分钟）；refresh token 可轮换、可撤销，服务端仅存哈希，Android Keystore 保护本地凭据。WSS 用授权头建立会话，不把 token 放 URL/日志。所有资源查询都按 owner/device 范围校验。
- gateway 与 client 使用不同 API 凭据和 SIP endpoint/auth/AOR。gateway 只能同步自身状态、认领自身命令、上报自身事件；client 只能操作授权 SIM，不能调用 root/ADB/shell。
- API 配对与 SIP 凭据由同一权限控制面管理，解绑立即撤销 API 凭据并使 SIP endpoint 停用。SIP Call-ID、From、客户端自带 X-* 头均不是授权依据。

配对成功响应一次性下发 API 会话和 SIP bootstrap（`device_id,role,sip_endpoint_id,auth_username,auth_realm,password,aor,registrar_uri,outbound_proxy_uri`）。设备与 SIP auth/AOR 固定一对一，v1 每 AOR 一个有效 contact；password 随机生成，若需再次下发由控制面加密保存，不以明文写配置仓库/DB普通列。`GET /devices/self/sip-config` 返回非秘密连接配置；凭据丢失用已授权 API 的 `POST /devices/self/sip-credentials/rotate` 重发一次新密码并撤销旧凭据，不能重放配对码再造 endpoint。API refresh 不自动轮换 SIP 密码。

SIP realm 使用配置的固定域名（如 `sip.example.com`），TLS 端口默认 5061；两端 REGISTER/INVITE 的 401/407 Digest 流程必须实测支持相同算法与 `qop=auth`。server 以通过 Digest 验证的 endpoint/auth identity 反查 device_id；不以 From 声称的号码认身份。SHA-256 优先在网关和 SDK 已实现并互通后启用，不能将当前仅简化 MD5 的网关直接接到只接受 SHA-256 的配置。凭据轮换、端点禁用和已有注册 contact 清理均可验证。

## 3. 双 SIM 身份和版本

`sim_id` 是服务端分配的稳定 UUID，代表一个经确认的 SIM 绑定；`mapping_revision` 是网关级递增整数。Android `subscriptionId`、`slotIndex`、`PhoneAccountHandle` 是旧手机上的当前映射，不作为客户端可直接指定的执行地址。

网关维护 `sim_id -> {active_sub_id, slot_index, phone_account_handle, identity_verified}`；按订阅变化监听重新验证。相同设备相同 SIM 的 subscriptionId 通常稳定，但跨设备/恢复出厂等不能依赖旧值。可获得特权 SIM 标识时仅本地存储加盐指纹；不上传完整 ICCID/IMSI。无法可靠验证换卡时，置为 `unverified` 并要求旧手机本地确认，不能按卡槽或电话号码猜测。

- 初次绑定在旧手机本地确认卡槽、运营商、标签及可选号码；号码可能为空，允许人工填写，但号码不是卡身份。
- 移槽经验证可保留 sim_id 并更新 revision；换成另一张卡创建新绑定；重装/恢复出厂需重新配对和绑定。
- 每个发短信/拨号命令携带 `gateway_id, sim_id, mapping_revision`。执行前检查 SIM 当前 active、identity_verified、revision 一致；不一致返回 `SIM_MAPPING_CHANGED`，失效返回 `SIM_UNAVAILABLE`，绝不回落系统默认 SIM。
- 入站 SIM 无法确认时保留事件为 `sim_id:null, sim_resolution:unknown`，客户端不提供一键原卡回复/回拨，不能把它冒充卡 1。
- v1 设备级最多一个桥接通话，包括 dialing/ringing/active；两张卡都可收短信。DSDS 通话时另一卡可达性取决于硬件/运营商，必须实测，不能承诺同时接两通电话。busy 不自动切卡，不排队过期拨号。

## 4. HTTP 接口（v1）

基础路径 `/v1`，UTF-8 JSON、UTC RFC3339 时间、UUID。写请求使用 `Idempotency-Key`；同 key+同 payload 返回原资源，同 key+不同 payload 返回 409。owner+client+operation 构成 key 的命名空间；业务记录保留期间保留幂等映射，不使用会导致延迟重复发送的短期缓存。

| 方法与路径 | 用途 |
|---|---|
| `POST /pairings/claim` | 一次性配对（配对码是此接口唯一初始授权） |
| `POST /auth/refresh`, `POST /auth/revoke` | 轮换和撤销会话 |
| `GET /devices/self/sip-config`, `POST /devices/self/sip-credentials/rotate` | SIP 非秘密连接配置、受控凭据恢复/轮换 |
| `GET /gateways`, `GET /gateways/{id}/sims` | 状态、SIM 标签、当前 revision 和能力 |
| `POST /gateways/{id}/heartbeat` | 网关心跳，30 秒一次；服务端 90 秒无心跳标离线 |
| `POST /gateways/{id}/sim-bindings` | 网关上报本地确认的映射变化；客户端不能直接修改 subId |
| `GET /messages?cursor=...&sim_id=...` | 不透明游标分页，按授权范围过滤 |
| `POST /messages` | 创建一条远端发短信任务，202 表示服务端落库 |
| `GET /messages/{id}` | 查询任务及每分片状态 |
| `GET /calls?cursor=...`, `GET /calls/{call_id}` | 通话记录及当前状态 |
| `POST /call-intents` | 预检、选卡、保留设备通话容量并生成一次性 SIP 拨号意图 |
| `DELETE /call-intents/{id}` | 取消尚未消费的意图；已通话用 SIP CANCEL/BYE |
| `POST /clients/{id}/push-token` | 当前设备登记/撤销推送 token |
| `POST /clients/{id}/ready` | `{call_id,wake_nonce}`；客户端查验该呼入并完成 SIP REGISTER，告知可以拨该客户端 |
| `GET /events?cursor=...` | 客户端漏事件补拉；超出保留期返回 resync_required |
| `WS /events` | `events_available/call_pending/gateway_changed` 等提示，必须可用 HTTP 补拉 |
| `GET /gateways/{id}/commands` | 网关查询可认领任务（正常 WSS 唤醒，断线后退避轮询） |
| `POST /gateways/{id}/commands/{command_id}/claim` | 单次原子认领，检查 SIM/version/TTL；返回完整任务 |
| `POST /gateways/{id}/events:batch` | 网关批量上传事件；成功响应逐项 durable event_id |

错误体：`{error:{code,message,retryable},request_id}`。401 无效身份；403 越权；409 payload/映射/状态冲突；429 限流；503 不可达。`retryable:true` 只允许重试同一资源/认领/同步，不能生成新的短信或新的计费拨号。

短信创建示例：

```json
{
  "gateway_id":"UUID", "sim_id":"UUID", "mapping_revision":3,
  "to":"+8613800000000", "text":"中文与 emoji 原文", "ttl_seconds":300
}
```

网关事件外壳：

```json
{
  "protocol_version":1, "event_id":"UUID", "gateway_id":"UUID",
  "sequence":42, "occurred_at":"2026-10-03T06:00:00Z",
  "type":"sms.received", "sim_id":"UUID", "mapping_revision":3,
  "payload":{"message_id":"UUID","from":"10690000","text":"原文","parts":1}
}
```

`sequence` 是每个配对网关持久化的单调序号；服务端客户端事件游标另行分配，不把设备时间当排序/幂等依据。批次目标最多 50 项、256 KiB；单消息原文上限 16 KiB，拒绝而非截断。手机号输入保留原文，按选定 SIM 的国家/地区校验；入站短号/字母 sender 原样显示，不能一律转换 E.164。

## 5. 短信持久化和副作用

入站：广播 -> 网关事务写入 inbox+event journal -> HTTPS 上报 -> 服务端 inbox+event outbox 同一事务提交 -> 回复 durable event_id -> 网关才标记可清理。上传重试沿用 event_id；服务端 `(gateway_id,event_id)` 唯一。不能按“相同发送者+正文+时间窗口”简单去重，以免吃掉两条合法相同验证码。

出站：客户端本地任务+key -> 服务端 message+command 同事务 -> WSS 通知 -> 网关 HTTPS claim -> 网关事务写入执行账本 -> **先持久化 dispatching 再调用指定 subId 的 SmsManager** -> SENT/DELIVERED callback 按 message_id+part_index 记录 -> 重试上报事件。服务端认领状态不代替网关副作用账本。

状态含 `queued, accepted_by_gateway, dispatching, submitted, delivered, failed, expired, unknown`。部分分片失败/未知必须逐片展示；全部 SENT 成功才 submitted，全部 delivery 成功才 delivered；运营商没回执时不伪造 delivered。拒绝/回执要带 Android resultCode 和可用错误信息，但日志遮蔽号码/正文。

claim 对同一个已配对网关是幂等的：响应丢失后可再次读取相同任务，GET commands 也包含本设备尚未完成的 claimed 任务。网关先写本地 ledger 才答应用层 accepted；服务端不因 claim 超时自动创建新 command 或转交另一个设备。TLS 重试复用原 command_id/message_id；payload hash 不同则拒绝。待提交任务在恢复后重新检查 TTL 和映射。

如果进程在调用 modem 后、回执前死亡，不能知道是否已经发送：保持 `unknown` 并尝试查询/晚到回执；不能重领、重启或过 lease 自动重发。尚未进入 dispatching 的确定未发送任务才可重试。已提交分片不能整条补发。用户主动新发另一个任务时提醒可能重复。移动网络上不承诺严格 exactly-once。

服务端到网关 command TTL 默认 300 秒（1..3600 可配）；过期在 dispatch 前检查并标 expired。离线期间输入保留为未提交草稿，或服务端明确展示待网关，不能显示已发送。UI 的离线缓存不丢正文，恢复网络用原 key 查询原任务。

限流服务端和网关独立执行，默认每 SIM 5 条/分钟、30 条/小时，附设备总预算；分片计费另有预算，不因分页/重试重算次数。恢复配置和队列不重置已计费窗口。

## 6. 呼叫协议与状态机

应用级 `call_id` 是 UUID，跨 Asterisk B2BUA 的不同 SIP Call-ID 保持不变，不能用 SIP Call-ID 当全局业务 ID。每个 intent 绑定 owner/client/gateway/SIM/revision/number/expire，默认 30 秒，opaque token 至少 128 位随机数；不写完整 token 日志。相同 Idempotency-Key 不能重复创建 intent/占位。

呼出：`POST /call-intents` 只预检并短时保留通话容量，**不拨蜂窝电话**。返回 `intent_id,call_id,sip_uri:"sips:call.<token>@sip.example.com",expires_at`。client 通过 SIP INVITE 发起；服务端按真实认证 endpoint 原子验证/消费 token、剥离 client 的路由头，把电话号码 Request-URI 和下列可信头加入网关 leg：

```text
X-GSM-Protocol-Version: 1
X-GSM-Call-Id: <UUID>
X-GSM-Sim-Id: <UUID>
X-GSM-Mapping-Revision: <integer>
```

gateway 在该已验证服务器 TLS 连接上校验可信头、可用 SIM/PhoneAccountHandle、并发和 call ledger，随后拨号；未知映射拒绝。SIP retransmission 复用同一事务，应用幂等同 call_id；同 call_id 不同号码/SIM 拒绝。不回落 `tel:` 本机拨号，也不执行来自 WSS/API 的第二条独立拨号命令。

网关必须把指定 SIM 对应的当前 `PhoneAccountHandle` 作为 `TelecomManager.EXTRA_PHONE_ACCOUNT_HANDLE` 放入 `placeCall` 的 Bundle；禁止空 Bundle 或不带 account 的 ACTION_CALL fallback。先将 call ledger 标记 `dispatching` 并提交，再调用 Telecom；重启后以现存 Telecom Call/account/号码/时序与业务账本对账。无法证明未拨出的 call_id 置 unknown、不重拨，并通知服务端保守保持 busy，直到可确认通话已终止；不要按同号码猜测成另一通电话。

呼入：网关从实际 cellular Call 的 PhoneAccountHandle 判定 SIM，持久化 call_id，经 SIP INVITE 携带相同头与 caller 信息；VPS 映射认证 gateway，验证 SIM。只有服务端接管本次路由后才可保留 pending；向网关返回 180，不提前 200/不接听蜂窝腿。在线客户端直接 Dial，睡眠客户端用 data push（只含 call_id、expiry，不含号码/SMS）唤醒 -> HTTPS 查当前有效状态 -> SIP REGISTER -> `/ready` -> server Dial 正确 AOR。等待窗口目标 ≤25 秒且受蜂窝来电实际终止约束，超时/主叫挂机立即取消；迟到推送不响铃。

`GET /calls/{call_id}` 对指定已授权客户端返回当前 pending 呼入的 `wake_nonce` 和 expiry；`/ready` 原子核验 client_id、call_id、nonce、pending状态、deadline，以及该设备真实 REGISTER contact 可用才可创建客户端 leg。迟到 A 呼入的 ready 不能唤醒 B；重复 ready 返回原结果，不创建第二条 SIP leg。App 只在收到匹配 call_id 的实际 INVITE 后建立 Telecom 呼入 session；接听/拒接分别由 SIP 200/相应失败响应传递，未定义独立 HTTP answer。推送阶段可以显示准备/验证中的通知，不能假称已接通。

初期一位主客户端、一条桥接通话。设备本地接听/拒接、运营商取消、第二卡来电、客户端本地 SIM 通话都须收敛：busy 明确拒绝，主机远端 SIP CANCEL/BYE 和网关本地结束互相传播。仅用户接听且客户端 leg 建立后才 answer cellular；ACK/BYE/CANCEL 竞态清理双方，不出现已挂机又接通。实际硬件不支持媒体恢复时断网结束通话并给出原因，不承诺无缝切网。

30 秒 TTL 只约束未消费 intent；不能以它到期释放正在 dialing/active 的通话锁。真正蜂窝呼入可抢占尚未消费的出站 intent（使旧 token 失效）；已消费并开始蜂窝呼出的通话不能被另一路覆盖。SIP 重传属于原事务，复用已消费 intent 的原路由；携带相同 token 的另一新事务拒绝，不能再次拨号。服务端重启先与 Asterisk 活动通道及网关状态对账；不确定通话保持 unknown/busy，不能靠 lease 过期盲目放行。

状态：`pending_wakeup -> ringing -> connecting -> active -> ended`；呼出可 `reserved -> dialing -> connecting -> active -> ended`。附方向、SIM、timestamps、reason；busy/canceled/no_answer/failed 不当作正常接通。事件包含 revision，拒绝旧状态回写覆盖 ended。DTMF 统一 RFC4733 telephone-event，并验证 gateway 真正转到蜂窝 DTMF；勿仅确认 SIP 包。v1 暂不实现双路通话、conference/transfer/voicemail/远端 emergency 通话；拨号明确是远端 SIM。

这里的通话 `state_revision` 由服务端聚合器单调生成，区别于 SIM 的 mapping_revision。网关上报 cellular 状态用自身持久 sequence；服务端将 SIP/Asterisk 与 cellular 证据合并，ended 为终态，乱序 ringing 不能让它复活。仅客户端和蜂窝两腿均建立才记 active/answered_at；answered_at 与 ended_at 计算时长，不把唤醒或拨号等待算通话。

### Asterisk 的控制与媒体边界

v1 以控制服务中的 ARI/Stasis coordinator 为唯一呼叫编排者；AMI 仅用于已验证的状态查询或配置维护。dialplan 完成 SIP endpoint 认证后将来话交到 Stasis；coordinator 验证/消费 intent、保存业务 call_id 与各 channel ID 的对应，再创建 PJSIP 对端 channel、监听应答/失败并建立 anchored bridge。呼入待唤醒 channel 不调用 Answer；只有 client leg 应答后才对 gateway leg 应答。CANCEL/BYE/timeout/本地蜂窝结束均幂等终止两腿和占用；ARI 事件断线后重新枚举通道对账，不能依非持久事件推断未拨过。

Asterisk RTP engine 独立分配两腿端口和 SRTP key，HTTP 不分配媒体端口。默认示例 `rtpstart=10000,rtpend=10199`，Compose 显式发布 `10000-10199/udp` 并同步 VPS 防火墙；也可改范围但三处配置保持一致。SIP/TLS 默认5061/tcp，API/WSS443/tcp；证书签发若用HTTP challenge才另开80。桥接网络部署设置正确 `local_net/external_media_address/external_signaling_address`，不能向公网客户端宣告容器IP。两腿 `direct_media=no,media_encryption=sdes,media_encryption_optimistic=no`，无 AVP fallback；两端最小 codec 集取网关已验证的 PCMA/G.722，不先依赖 Opus 转码。RFC4733 payload 动态协商，不硬编码101；RTCP若启用需协商并使用SRTCP，当前网关未验证的SRTCP能力纳入M0记录/补齐，不能漏发端口或默默发送明文控制包。验收检查SDP公网地址、两腿各自的端口/加密和实际双向音频。

## 7. 连接恢复和生命周期

- WSS 仅加速通知，不保存唯一消息；断线用指数退避+jitter，上限 60 秒，并使用 HTTP 游标/任务列表补齐。设备和服务端重启从 DB 恢复，重复事件不产生重复副作用。
- 服务端 heartbeat 判在线与 SIP REGISTER 可达状态分开；`online` 不代表 SIM ready/audio usable/client able_to_ring。状态须含 root、SIP、每卡 service、busy、音频 profile、temperature、battery/charging、app/protocol versions。
- DB 备份恢复可能回滚命令状态：网关的执行账本是避免再发的第二道约束；设备账本丢失后重配对，旧 dispatching/unknown 禁止重新执行。
- 推送高优先级用于真实及时用户事件；Android force-stop 后后台能力不可承诺，客户端显示必须手动启动。无 GMS 只能采用验证过的替代 push provider，或用户开启可见常驻连接模式（耗电、Doze/OEM限制需标识），不能把 WSS 当系统保证唤醒。

## 8. 三端共同验收与契约测试

- 卡1/卡2分别呼入、呼出、中文/emoji/多分片短信；回复与回拨严格原卡；换卡/移槽/缺卡/空号码/旧revision不得错发。
- 一卡通话时另一卡短信及来电实测，记录 DSDS 限制；两端并发命令、SIP重传、双击、服务端重启不得重复拨号/短信。
- 进程在 modem 提交前后各崩溃、SENT/DELIVERED 丢失/迟到、DB恢复，确认 unknown 不补发。
- 主机 Wi-Fi 与移动数据异网、公网 NAT、锁屏/Doze 24小时、蓝牙、网络切换、推送迟到及 force-stop；未 root 主机不索取短信/蜂窝控制特权。
- TLS 错证书、缺 SRTP、越权 SIM、任意自定义头、过期/重放 intent、匿名 SIP、超长/CRLF 注入输入均拒绝。
- server 仓库保存 OpenAPI/JSON Schema、短信重试与通话竞态 fixtures，Android 仓库锁定协议提交并复用 fixtures；协议变更必须先更新 server 权威源并同步三端，不能单端自创字段。

## 9. 核对资料

- [Android Subscription ID 与唯一标识](https://developer.android.com/identity/user-data-ids)
- [SubscriptionManager](https://developer.android.com/reference/android/telephony/SubscriptionManager)
- [SmsManager](https://developer.android.com/reference/android/telephony/SmsManager)
- [Asterisk PJSIP 配置及 SDES](https://docs.asterisk.org/Asterisk_22_Documentation/API_Documentation/Module_Configuration/res_pjsip/)
- [Asterisk NAT/media anchoring](https://docs.asterisk.org/Configuration/Channel-Drivers/SIP/Configuring-res_pjsip/Configuring-res_pjsip-to-work-through-NAT/)
- [Asterisk MessageSend 的成功不保证送达](https://docs.asterisk.org/Asterisk_20_Documentation/API_Documentation/Dialplan_Applications/MessageSend/)
