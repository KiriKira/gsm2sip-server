# gsm2sip 多设备协议 v1（实现与验收目标）

更新：2026-10-04。权威源在 `KiriKira/gsm2sip-server/docs/protocol-v1.md`；另两仓库用链接和固定协议提交号引用，不独立修改副本。`openapi/openapi.yaml` 与 `docs/server-wire-addendum.md` 给出可执行的 HTTP 契约；具体功能状态见仓库 README。平台标识可为未来系统扩展，但不代表对应客户端 UI、推送或通话能力已经实现。

## 1. 固定架构与边界

| 路径 | v1 约定 |
|---|---|
| 旧 root Android ↔ VPS 的呼叫 | SIP/TLS；SDES-SRTP `AES_CM_128_HMAC_SHA1_80`；VPS Asterisk/PJSIP 锚定媒体 |
| 未 root Android ↔ VPS 的呼叫 | App 内成熟 SIP SDK，使用相同安全配置；不自行编写 SIP/音频协议栈 |
| Windows 或其他宿主 ↔ VPS | 共用 owner、HTTP、事件恢复和 SIP 设备契约；每个已配对安装有独立凭据和 AOR。Windows 客户端/UI 目前是未来适配目标，不据此宣称已实现 |
| 两端 ↔ VPS 的短信、状态、命令 | HTTPS JSON + WSS 通知；数据库提交后才产生应用层确认 |
| VPS 内部 | 控制服务 ↔ Asterisk 的 ARI/AMI 只在内部网络或 loopback；数据库、管理接口不对公网开放 |

生产不静默降级到 UDP SIP/明文 RTP。不建立旧手机到主机的直连媒体。TLS+SRTP 是分段加密，VPS 能处理媒体及短信，不能宣称端到端加密。旧网关已有 SIP MESSAGE 仅作为基线测试或显式迁移适配；v1 生产短信执行器只接受 HTTPS 命令，不得两条通道各发送一次。

## 2. 身份、配对与权限

- `owner_id` 是唯一拥有者；`gateway_id`、`client_id` 是服务端签发的 UUID。v1 的家庭配置可有一个网关、最多两张 SIM 和多个配对 client；短信仍走该网关。不同 client 是独立授权设备，不是同一账号的共享 SIP contact。
- 初始管理员用 VPS 本地 CLI 建立，随后生成一次性、短时配对码/二维码。配对码绑定角色（gateway/client），消费一次即失效，限速；不用公开注册或仓库内默认密码。
- `POST /pairings/claim` 的 client 请求可带开放平台标识 `platform`，规范化后为 1..32 个小写 ASCII 字符，匹配 `[a-z][a-z0-9_-]*`；省略或仅空白默认为 `unknown`。`android`、`windows`、`linux` 可作约定标签，服务端不维护 OS 白名单；该字段只是展示元数据，不授予 root/短信/呼叫特权，也不宣告 UI/运行时支持。`GET /clients` 只允许 client 角色调用，并返回同 owner 下已配对 client（含 revoked 状态），含当前项的 `is_self`，不返回凭据。
- HTTPS access token 短期有效（目标 15 分钟）；refresh token 可轮换、可撤销，服务端仅存哈希，客户端须使用本机安全存储保护凭据（Android Keystore 或未来 OS 提供的等价存储）。WSS 用授权头建立会话，不把 token 放 URL/日志。所有资源查询都按 owner/device 范围校验。
- gateway 与每个 client 使用不同 API 会话及 SIP endpoint/auth/AOR。gateway 只能同步自身状态、认领自身命令、上报自身事件；client 只能操作授权 SIM，不能调用 root/ADB/shell。`GET /clients` 列表、会话 refresh/revoke、客户端事件 durable receipt 和短信写请求幂等键均按当前配对设备隔离；事件数据仍按 owner 授权读取，各 host 必须各自持久化游标并确认自己的本地提交。
- API 与 SIP 身份按设备绑定；被控制面标记为 `revoked` 的设备不得再通过 API/SIP 认证。当前 `/auth/revoke` 只撤销请求内 refresh token 所属的会话，不改变设备 `state` 或撤销同 owner 的其它设备；`GET /clients` 是只读清单，没有新增的 owner-facing 设备撤销/恢复 API。SIP Call-ID、From、客户端自带 X-* 头均不是授权依据。

配对成功响应下发 API 会话与 SIP 是否配置的状态。每个设备随后通过已授权 API 获取自己的非秘密 SIP 配置，并用持久化 Idempotency-Key 显式 bootstrap/rotate 取得密码（`endpoint_id,auth_username,auth_realm,password,aor,registrar_uri,outbound_proxy_uri,server_name`）；五分钟内同请求键可恢复相同加密响应。设备与 SIP auth/AOR 固定一对一，每 AOR 一个有效 contact，多个 host 通过不同 AOR 并行注册；限制单 AOR 一 contact 可避免一个设备的多 contact 产生非确定性振铃、推送关联和注册清理。password 随机生成，若需再次下发由控制面加密保存，不以明文写配置仓库/DB普通列。`GET /devices/self/sip-config` 返回非秘密连接配置；凭据丢失用已授权 API 的 `POST /devices/self/sip-credentials/rotate` 重发一次新密码并撤销旧凭据，不能重放配对码再造 endpoint。API refresh 不自动轮换 SIP 密码。

### 跨平台 client 接入约定

每台 host 安装独立 claim 一个 client 配对码，使用自己的 access/refresh 凭据、SIP endpoint、AOR、SMS durable receipt 和本地恢复游标。`platform` 只是开放的低信任标签：Windows、Android、MagiskVM 等都走相同 owner/client 授权；未知平台应可显示和排查，不应因未认识标签而拒绝合法配对。服务端绝不据此推断 root、短信特权、SIM 能力或“在线可响”。每个 OS 客户端按本机安全存储保护 API/SIP 密钥，也不向 VPS 发送共享 refresh token。

host 使用 HTTPS 同步消息与 calls、用 WSS 获取无数据的 `sync_required`、按 owner/device scope 拉取 durable 状态。发短信时由该 client 选择网关和经确认的远端 SIM，host 自己的电话卡/蜂窝栈不是执行器。需要呼叫的 host 按相同 SIP/TLS、Digest、SRTP 配置使用其单独 AOR；一个 AOR 最多一个 contact。来电 UI 根据当前设备自己的 participant projection 展示，`ended/answered_elsewhere` 必须停止振铃并显示另一台 host 已接听；reject 只结束该设备一条 participant，除非它是最后一个候选。OS 通知只是界面层，数据库状态和 SIP 信令才是决定依据。

Windows client UI、系统通知、后台运行和媒体验证当前未交付；平台标签 `windows` 仅允许识别未来设备，不能对用户展示为已支持。MagiskVM 也要针对实际宿主、Telecom/SIM 账户绑定、麦克风/扬声器和 SRTP 双向媒体分别验收；它不是可用语音能力的代称。该接口不承诺可读取 raw PCM，也不承诺运营商短信必达。

SIP Digest realm 固定为 `gsm2sip`，与 TLS 的公开服务器域名分开；TLS 端口默认 5061；两端 REGISTER/INVITE 的 401/407 Digest 流程必须实测支持相同算法与 `qop=auth`。server 以通过 Digest 验证的 endpoint/auth identity 反查 device_id；不以 From 声称的号码认身份。SHA-256 优先在网关和 SDK 已实现并互通后启用，不能将当前仅简化 MD5 的网关直接接到只接受 SHA-256 的配置。凭据轮换、端点禁用和已有注册 contact 清理均可验证。

## 3. 双 SIM 身份和版本

`sim_id` 是服务端分配的稳定 UUID，代表一个经确认的 SIM 绑定；`mapping_revision` 是网关级递增整数。Android `subscriptionId`、`slotIndex`、`PhoneAccountHandle` 是旧手机上的当前映射，不作为客户端可直接指定的执行地址。

网关维护 `sim_id -> {active_sub_id, slot_index, phone_account_handle, identity_verified}`；按订阅变化监听重新验证。相同设备相同 SIM 的 subscriptionId 通常稳定，但跨设备/恢复出厂等不能依赖旧值。可获得特权 SIM 标识时仅本地存储加盐指纹；不上传完整 ICCID/IMSI。无法可靠验证换卡时，置为 `unverified` 并要求旧手机本地确认，不能按卡槽或电话号码猜测。

- 初次绑定在旧手机本地确认卡槽、运营商、标签及可选号码；号码可能为空，允许人工填写，但号码不是卡身份。
- 移槽经验证可保留 sim_id 并更新 revision；换成另一张卡创建新绑定；重装/恢复出厂需重新配对和绑定。
- 每个发短信/拨号命令携带 `gateway_id, sim_id, mapping_revision`。执行前检查 SIM 当前 active、identity_verified、revision 一致；不一致返回 `SIM_MAPPING_CHANGED`，失效返回 `SIM_UNAVAILABLE`，绝不回落系统默认 SIM。
- 入站 SIM 无法确认时保留事件为 `sim_id:null, sim_resolution:unknown`，客户端不提供一键原卡回复/回拨，不能把它冒充卡 1。
- v1 设备级最多一个桥接通话，包括 dialing/ringing/active；两张卡都可收短信。DSDS 通话时另一卡可达性取决于硬件/运营商，必须实测，不能承诺同时接两通电话。busy 不自动切卡，不排队过期拨号。

## 4. HTTP 接口（v1）

基础路径 `/v1`，UTF-8 JSON、UTC RFC3339 时间、UUID。写请求使用 `Idempotency-Key`；同 key+同 payload 返回原资源，同 key+不同 payload 返回 409。owner+device+operation 构成 key 的命名空间；业务记录保留期间保留幂等映射，不使用会导致延迟重复发送的短期缓存。

| 方法与路径 | 用途 |
|---|---|
| `POST /pairings/claim` | 一次性配对（配对码是此接口唯一初始授权） |
| `POST /auth/refresh`, `POST /auth/revoke` | 轮换和撤销会话 |
| `GET /devices/self/sip-config`, `POST /devices/self/sip-credentials/rotate` | SIP 非秘密连接配置、受控凭据恢复/轮换 |
| `GET /clients` | 当前 owner 的配对 client 列表；只允许 client 角色，包含 `id,name,platform,state,is_self` |
| `GET /gateways`, `GET /gateways/{id}/sims` | 状态、SIM 标签、当前 revision 和能力 |
| `POST /gateways/{id}/heartbeat` | 网关心跳，30 秒一次；服务端 90 秒无心跳标离线 |
| `POST /gateways/{id}/sim-bindings` | 网关上报本地确认的映射变化；客户端不能直接修改 subId |
| `GET /messages?cursor=...&sim_id=...` | 不透明游标分页，按授权范围过滤 |
| `POST /messages` | 创建一条远端发短信任务，202 表示服务端落库 |
| `GET /messages/{id}` | 查询任务及每分片状态 |
| `GET /calls?cursor=...`, `GET /calls/{call_id}` | 当前已认证 client 自己参与的通话视图；incoming 同一 `call_id` 对每台设备有独立状态 |
| `POST /call-intents` | 预检、选卡、保留设备通话容量并生成一次性 SIP 拨号意图 |
| `DELETE /call-intents/{id}` | 取消尚未消费的意图；已通话用 SIP CANCEL/BYE |
| `POST /clients/{id}/ready` | `{call_id,wake_nonce}`；path id 必须是当前认证 client，完成 SIP REGISTER 后申领自己的来电参与腿 |
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

呼入：网关从实际 cellular Call 的 PhoneAccountHandle 判定 SIM，持久化 call_id，经 SIP INVITE 携带相同头与 caller 信息；VPS 映射认证 gateway，验证 SIM。只有服务端接管本次路由后才可保留 pending；向网关返回 180，不提前 200/不接听蜂窝腿。接管时服务端将同 owner 下同时满足 `device.state=active`、SIP endpoint binding active、至少一个 access 或 refresh session 尚未过期的 client 快照为 participant；临时离线但仍可用 refresh 恢复的 host 也有自己的 pending。候选资格不要求此刻 `EndpointOnline`；真正 `/ready` 时才验证已注册 contact。每个 participant 对同一 `call_id` 有自己的 `client_id`、`wake_nonce`、state 与单调 `state_revision`。新配对设备不追加入正在响铃的呼入；无有效会话、设备撤销或 endpoint binding 失效的旧设备不纳入新呼入。客户端可用 WSS 同步提示后 HTTPS 查当前状态、SIP REGISTER、`/ready`，server 再拨该设备自己的 AOR。协议不承诺或依赖 FCM 等推送已可用。该 cellular call 及其所有 client legs 共用一个 gateway 通话 slot，不能用多个主机并发转成多通话。

`GET /calls/{call_id}` 与 `GET /calls` 只返回当前已授权 host 自己的 participant projection；响应保留字段名 `client_id`，incoming 时它等于当前请求设备 ID。列表游标按 `created_at DESC,call_id DESC` 排序分页；每页是各 participant 的最新快照，不是 revision 事件时间线。收到 WSS hint 后从头读取所有分页并按各行 `state_revision` 合并。pending participant 可见自己专属的 `wake_nonce` 和 expiry；完成 ready 后不再回显 nonce。`/ready` 原子核验 authenticated client_id、call_id、nonce、participant pending 状态、deadline，以及该设备真实 REGISTER contact 可用才可创建其 client leg。迟到/别的设备的 nonce 不能唤醒本设备或其他设备；重复 ready 不会创建第二条 leg。App 只在收到匹配 call_id 的实际 INVITE 后建立 Telecom 呼入 session；接听/拒接分别由 SIP 200/相应失败响应传递，未定义独立 HTTP answer。推送阶段可以显示准备/验证中的通知，不能假称已接通。

多个配对 host 可同时收到同一呼入的候选铃声，但只允许一个 client leg 获胜：只有通过 Digest 验证的 client endpoint 的真实 PJSIP channel `Up` 才参与原子 winner 选择；`/ready`、REGISTER、推送送达、或本地 wrapper/UI 报告 Up 均不算接听。首个获胜 client 进入 connecting/active 路径，其余 participant 变为 `ended`，`reason:"answered_elsewhere"`，且 loser 的 revision 只更新自己的视图。单台设备拒接（`busy`/`rejected`）或自身等待超时（`no_answer`）只结束该 participant，仍给其余 participant 振铃；最后一个候选拒绝，或 25 秒呼入 deadline 到达时，服务端将整个 call 置 `ended/no_answer` 并释放唯一 gateway slot。主叫取消/蜂窝腿结束时也结束全部 participant。所有清理由同一 call 编排状态串行化，不能因旧 SIP 状态复活终态。

整个 owner 仍只有一个网关通话 slot。一个候选接听后，设备本地来电/拒接、运营商取消、第二卡来电、客户端本地 SIM 通话都须收敛：busy 明确拒绝，主机远端 SIP CANCEL/BYE 和网关本地结束互相传播。仅获胜 client SIP leg 真正应答且 bridge 建立后才 answer cellular；ACK/BYE/CANCEL 竞态清理双方，不出现已挂机又接通。出站仍由一个 host 创建单次 intent 并锁住同一 slot；不因 host 无 SIM、忙或超时回退为本机蜂窝拨号，也不在其他配对 SIM 上自动 fallback。实际硬件不支持媒体恢复时断网结束通话并给出原因，不承诺无缝切网。

30 秒 TTL 只约束未消费 intent；不能以它到期释放正在 dialing/active 的通话锁。真正蜂窝呼入可抢占尚未消费的出站 intent（使旧 token 失效）；已消费并开始蜂窝呼出的通话不能被另一路覆盖。SIP 重传属于原事务，复用已消费 intent 的原路由；携带相同 token 的另一新事务拒绝，不能再次拨号。服务端重启先与 Asterisk 活动通道及网关状态对账；不确定通话保持 unknown/busy，不能靠 lease 过期盲目放行。

状态：`pending_wakeup -> ringing -> connecting -> active -> ended`；呼出可 `reserved -> dialing -> connecting -> active -> ended`。附方向、SIM、timestamps、reason；busy/canceled/no_answer/failed 不当作正常接通。事件包含 revision，拒绝旧状态回写覆盖 ended。DTMF 统一 RFC4733 telephone-event，并验证 gateway 真正转到蜂窝 DTMF；勿仅确认 SIP 包。v1 暂不实现双路通话、conference/transfer/voicemail/远端 emergency 通话；拨号明确是远端 SIM。

这里每个 incoming participant 的通话 `state_revision` 由服务端单调生成，区别于 gateway 聚合 call 状态及 SIM 的 `mapping_revision`；incoming participant 的 `client_id` 是该客户端本身。`call_events` 的 uniqueness scope 是 `(call_id, client_device_id, state_revision)`，每设备只消费自己的参与者状态；WSS 仍发送无数据的 `sync_required`，marker 独立计算每设备 call cursor，并与 owner 范围 SMS cursor/receipt 分开。收到 wake hint 后，客户端分别通过 SMS `GET /events` + `/events/ack` 与本地化 `GET /calls` 同步；ACK SMS cursor 不会确认其它设备的本地写入，也不确认 call state。呼叫不新增事件 ACK API，设备状态按当前视图和 participant revision 恢复。网关上报 cellular 状态用自身持久 sequence；服务端将 SIP/Asterisk 与 cellular 证据合并，ended 为终态，乱序 ringing 不能让它复活。仅获胜客户端和蜂窝两腿均建立才记 active/answered_at；answered_at 与 ended_at 计算时长，不把唤醒或拨号等待算通话。

### Asterisk 的控制与媒体边界

v1 以控制服务中的 ARI/Stasis coordinator 为唯一呼叫编排者；AMI 仅用于已验证的状态查询或配置维护。dialplan 完成 SIP endpoint 认证后将来话交到 Stasis；coordinator 验证/消费 intent、保存业务 call_id 与各 channel ID 的对应，再创建 PJSIP 对端 channel、监听应答/失败并建立 anchored bridge。呼入待唤醒 channel 不调用 Answer；只有 client leg 应答后才对 gateway leg 应答。CANCEL/BYE/timeout/本地蜂窝结束均幂等终止两腿和占用；ARI 事件断线后重新枚举通道对账，不能依非持久事件推断未拨过。

Asterisk RTP engine 独立分配两腿端口和 SRTP key，HTTP 不分配媒体端口。默认示例 `rtpstart=10000,rtpend=10199`，Compose 显式发布 `10000-10199/udp` 并同步 VPS 防火墙；也可改范围但三处配置保持一致。SIP/TLS 默认5061/tcp，API/WSS443/tcp；证书签发若用HTTP challenge才另开80。桥接网络部署设置正确 `local_net/external_media_address/external_signaling_address`，不能向公网客户端宣告容器IP。两腿 `direct_media=no,media_encryption=sdes,media_encryption_optimistic=no`，无 AVP fallback；两端最小 codec 集取网关已验证的 PCMA/G.722，不先依赖 Opus 转码。RFC4733 payload 动态协商，不硬编码101；RTCP若启用需协商并使用SRTCP，当前网关未验证的SRTCP能力纳入M0记录/补齐，不能漏发端口或默默发送明文控制包。验收检查SDP公网地址、两腿各自的端口/加密和实际双向音频。

## 7. 连接恢复和生命周期

- WSS 仅加速通知，不保存唯一消息；owner SMS durable marker 可唤醒该 owner 的所有 client，call participant marker 只唤醒对应 client。hint frame 仍不包含数据。断线用指数退避+jitter，上限 60 秒；客户端收 hint 后分别同步 SMS durable cursor 与当前 client 的 calls view。设备和服务端重启从 DB 恢复，重复事件不产生重复副作用。
- 服务端 heartbeat 判在线与 SIP REGISTER 可达状态分开；`online` 不代表 SIM ready/audio usable/client able_to_ring。状态须含 root、SIP、每卡 service、busy、音频 profile、temperature、battery/charging、app/protocol versions。
- DB 备份恢复可能回滚命令状态：网关的执行账本是避免再发的第二道约束；设备账本丢失后重配对，旧 dispatching/unknown 禁止重新执行。
- 未来 host UI（含 Windows）需展示当前设备平台/配对状态和 participant 自己的通话状态；对 `answered_elsewhere` 展示“其他设备已接听”，不能继续显示来电可接听。未完成 Windows UI/音频集成时须明确显示未支持/不可用状态。WSS 是同步提示而非 OS 后台保证；FCM/APNs/其他 push 都未在此契约中承诺。Android force-stop 后后台能力不可承诺；无 GMS 的备用唤醒模式需针对目标 ROM 实测并清楚标注限制。

## 8. 三端共同验收与契约测试

- 卡1/卡2分别呼入、呼出、中文/emoji/多分片短信；回复与回拨严格原卡；换卡/移槽/缺卡/空号码/旧revision不得错发。
- 一卡通话时另一卡短信及来电实测，记录 DSDS 限制；两端并发命令、SIP重传、双击、服务端重启不得重复拨号/短信。
- 进程在 modem 提交前后各崩溃、SENT/DELIVERED 丢失/迟到、DB恢复，确认 unknown 不补发。
- 同 owner 两台以上 host 的同时呼入、迟到 ready、其中一台拒接、另一台接听、winner/loser 乱序事件；确认每设备 nonce、participant revision 和 call event marker 相互独立，loser 显示 `answered_elsewhere`。
- 主机 Wi-Fi 与移动数据异网、公网 NAT、锁屏/Doze 24小时、蓝牙、网络切换、推送迟到及 force-stop；未 root 主机不索取短信/蜂窝控制特权。Windows host 使用同一 HTTPS/SIP 认证契约但需独立 UI 与媒体验证；平台字符串开放不等于 Windows client 已交付。
- TLS 错证书、缺 SRTP、越权 SIM、任意自定义头、过期/重放 intent、匿名 SIP、超长/CRLF 注入输入均拒绝。
- server 仓库保存 OpenAPI/JSON Schema、短信重试与通话竞态 fixtures，Android 仓库锁定协议提交并复用 fixtures；协议变更必须先更新 server 权威源并同步三端，不能单端自创字段。

## 9. 核对资料

- [Android Subscription ID 与唯一标识](https://developer.android.com/identity/user-data-ids)
- [SubscriptionManager](https://developer.android.com/reference/android/telephony/SubscriptionManager)
- [SmsManager](https://developer.android.com/reference/android/telephony/SmsManager)
- [Asterisk PJSIP 配置及 SDES](https://docs.asterisk.org/Asterisk_22_Documentation/API_Documentation/Module_Configuration/res_pjsip/)
- [Asterisk NAT/media anchoring](https://docs.asterisk.org/Configuration/Channel-Drivers/SIP/Configuring-res_pjsip/Configuring-res_pjsip-to-work-through-NAT/)
- [Asterisk MessageSend 的成功不保证送达](https://docs.asterisk.org/Asterisk_20_Documentation/API_Documentation/Dialplan_Applications/MessageSend/)
