# Reception v2：独立电话订阅、短信同步与按需唤醒

本文件是 Reception v2 的协议权威，与 `openapi/openapi.yaml` 和 `fixtures/api/` 中的样例一起维护。设计讨论、实施顺序与 mesh 取舍保留在 Madoka 仓库：[实施 PLAN](https://github.com/KiriKira/gsm2sip-client-android/blob/main/docs/reception-v2-PLAN.md)、[mesh 整合决定](https://github.com/KiriKira/gsm2sip-client-android/blob/main/docs/reception-mesh-decision.md)。第 1 节起为迁入的设计正文（来源：gsm2sip-client-android PR #37 `2522593`）；与第 0 节冲突时，以第 0 节记录的已实现契约为准。

## 0. 实现状态与精确契约

R1 已在四个仓库的 `claude/vibrant-babbage-ye0ey3` 分支实现：

- **SoulGem（本仓库）：** 本节所列全部契约。
- **Kyubey：** wire2 字段白名单；按 `TTL`/`Urgency` 头写入 FCM 的 TTL 与优先级，扣除中继自身耗时；限流按 kind 分开。
- **Homura：**
  - 振铃窗口从第一次蜂窝 RINGING 起按单调时钟计时，向 v2 节点发送 `remaining_ring_ms`。
  - `NO_INCOMING_SUBSCRIBERS` 视为明确拒绝，继续尝试其他节点。
  - winner 账本增加 `decision` 列，接听与整通拒接在同一个 CAS 里互斥。
- **Madoka：**
  - 对 v2 节点写一次接收选择（来电取“后台来电接收”开关，短信取“后台同步”开关），按节点持久保存服务端的回答。
  - 对这些节点不再续约 capability、申请 sleep、持有 `COMPAT_IDLE` 或设精确闹钟。
  - 推送可用时，呼叫控制在空闲轮询后连同前台通知一起停止，真实来电的推送再启动它；后台同步服务对该节点不再保持 WebSocket，改为 15 分钟 HTTPS 补查。
  - 推送不可用时保留 WebSocket：只由服务端 ping，间隔由客户端经 `Wake-Ping-Seconds` 申请，从 120 秒开始，连接稳定时逐步升到 300 秒，疑似空闲断开时减半，最低 60 秒。
  - wire2 推送和 WebSocket `domains` 按领域分发；用户拒接在网关支持时发 `decline_all`；用户关闭“后台来电接收”后，打开页面不再自动重新开启；通话页显示各节点的同步状态。
  - 通话记录本来就只在前台同步时经 HTTPS 获取，不会启动呼叫控制，无需改动。

- **Homura（补充）：** 拒接已写入账本、蜂窝拒绝前进程中断时，重启后 Telecom 重新送来的同一通来电会被直接拒掉，不再响铃。
- **Madoka（补充）：** 拒接后先显示等待网关确认，再回读结果；网络切换时替换旧网络上的 WebSocket，并等切换稳定约 2 秒后重连；诊断导出包含各节点的接收模式。

验证顺序：先按 [模拟器验收计划](https://github.com/KiriKira/gsm2sip-client-android/blob/main/docs/reception-v2-emulator-test-plan.md)，再做计划第 6 节的真机验收与 8 小时待机观察。

### 能力与配置

- `GET /v1/capabilities` 增加 `reception_v2`（节点启用呼叫控制时为 true）。
- `GET/PUT /v1/devices/self/reception`，仅 client 角色。请求 `{"version":1|2,"expected_revision","incoming_enabled","sms_enabled"}`，四项必填；`expected_revision` 与当前不等返回 409 `RECEPTION_REVISION_CONFLICT`。未写过的设备读作 `version=1`、`revision=0`、`sms_enabled=true`，`incoming_enabled` 取其呼叫控制 capability 的 `enabled`。
- 设备在 `version=2` 时：
  - 只要 `incoming_enabled=true`、设备有效且 refresh 会话未过期，就进入来电分叉，不看 capability 的 90 秒刷新、`sleep_until` 或推送端点失败次数；离线也建立参与者。
  - 主动呼出不再要求 capability 或 incoming 开启。旧的 capability 写入不改变 v2 偏好。
- PUT 把 `incoming_enabled` 改为 false 时，在同一事务内只撤出本设备仍处于 `ringing` 的参与者（reason `reception_disabled`），不挂断通话、不释放网关占用；本设备已经 accept 成为候选的通话不受影响。
- `version=1` 可写回，设备随即回到旧候选规则与 wire1 推送。

### 来电窗口

- `POST /v1/rtc/control/incoming` 可选 `remaining_ring_ms`（1–60000），仅发给宣告 `reception_v2` 的节点（旧节点拒绝未知字段）。截止时间为 `min(25000, remaining_ring_ms)` 毫秒；该字段不计入操作幂等身份，重试可带更小值并重放首次结果，截止时间不变。
- 带该字段的请求在本节点没有任何可接来电的 client 时返回 503 `NO_INCOMING_SUBSCRIBERS`；不带时保持 `GATEWAY_UNAVAILABLE`。两者都不创建通话。
- 呼叫控制快照在 ringing/connecting 等未终止状态带 `remaining_ms`，为服务端时钟下的剩余毫秒数。

### 推送（wire2）

- 按设备的 reception 版本选择格式：`version=2` 发 wire2，其余设备保持 wire1 的混合提示（`TTL: 60`、`Urgency: high`）。
- wire2 正文为 `{"v":2,"kind","device_id","wake_id","revision","endpoint_generation"}`，call 另带 `call_id`；TTL 与优先级只通过 HTTP `TTL`（整秒，向下取整）和 `Urgency` 头传递。
- **call 推送：**
  - 只为呼入通话发送；每通一条，`wake_id` 为 `call-event-<该通话最新事件 cursor>`，`revision` 为通话 `state_revision`，TTL 为剩余振铃时间，`Urgency: high`。
  - 剩余不足 3 秒不再发送。每轮最多尝试 3 次，每次超时 `min(3s, 剩余-3s)`，间隔约 1/3 秒；仍失败则该通话 1 秒后再试，并累加 `call_failures`。
  - 已被中继受理的 `(设备, generation, call_id, revision)` 记在进程内存，不重复发送。call 水位只推进到第一个未完成通话之前。
- **SMS 推送：**
  - 仅在 `sms_enabled=true` 时发送。`wake_id` 为 `sms-event-<cursor>`，`TTL: 86400`；新事件中有 `message.received` 时 `Urgency: high`，否则 `normal`。
  - 沿用 `failures/next_attempt_at` 指数退避（上限 5 分钟）。关闭期间不推进 SMS 水位，重新开启后积压只发一条提示。
- 端点重新登记为不同 URL 时 `endpoint_generation` 加一（同 URL 不变），PUT 响应和 GET 状态都返回该值；所有结果只更新或删除读到的那一代端点。
- call 与 SMS 分别有 4 个和 2 个并发 worker，每个设备的每个领域同时只有一个任务。来电创建和呼入通话状态变化提交后立即触发一轮分发；原每秒扫描保留为兜底。

### 整通拒接（global_decline_v1）

- 节点在 `/v1/capabilities` 宣告 `global_decline_v1`。网关只在节点宣告后才在 `POST /v1/rtc/control/capability` 带 `"global_decline": true`（client 带此字段返回 400；不带则视为不支持）。
- 该网关的呼入快照带 `global_decline: true`。只有此时 client 才能发 `decline_all`，否则 409，只能用原来的 `reject` 退出本机参与。
- `decline_all` 要求：client 角色、呼入、尚无 winner，且本设备不是当前接听候选。它只记录请求，快照随即带 `decline_requested: true`；不改变通话状态，也不请求结束，不会抢在网关之前宣称已拒接。
- 网关在安装范围的 winner 账本里裁决：拒接与接听互斥，先提交者胜。拒接胜出后拒绝蜂窝来电，等真实 END 之后再用网关操作 `declined` 结束各节点的通话。各参与设备的终态原因均为 `declined`，并释放占用。

### WebSocket

- client 可在升级请求中带 `Wake-Ping-Seconds`（20–300，否则 400 `INVALID_WAKE_PING`），服务端即按此间隔 ping，pong 等待为该间隔加 25 秒，客户端不再自行 ping。Madoka 只对 v2 节点发送该头，旧节点仍是服务端 20 秒 ping。

- 对 v2 client，`sync_required` 帧增加 `domains`（`call`、`sms`），说明是哪个领域变化。`sms_enabled=false` 时不为短信变化发提示，重新开启后补发一次。
- 通话变化提交后立即唤醒所有连接，不等 1 秒轮询。旧客户端只检查 `protocol_version` 与 `type`，可忽略新增字段。

## 1. 已完成的参考与设计选择

以下来源已阅读。借鉴的是移动端接收模型，保留现有 HTTPS 呼叫控制和原生 WebRTC，不重新引入 SIP 引擎。

| 官方来源 | 已核对的实践 | 本项目采用的设计 |
| --- | --- | --- |
| [RFC 8599](https://datatracker.ietf.org/doc/html/rfc8599)，2019，尤其 §4、§5 | 推送唤醒休眠 UA，恢复注册后继续投递请求 | 接收身份与当前连接分离；邀请在服务器等待；不用短在线心跳证明接收资格。RFC 本身仍有注册刷新要求，本提案的持久订阅是我们的选择 |
| [Flexisip PushNotification 当前文档](https://wiki.linphone.org/xwiki/wiki/public/view/Flexisip/Core%20servers/Configuration%20reference%20guide/master/module/PushNotification)，文档注明 `2.7.0-alpha-97-g34521b37`；[2.4.2 文档](https://wiki.linphone.org/xwiki/wiki/public/view/Flexisip/Core%20servers/Configuration%20reference%20guide/2.4.2/module/PushNotification) | 来电与消息使用不同 TTL；可委托 HTTP 中继；移动端不必保持永久连接 | 短期来电邀请、持久短信事实；Kyubey 只负责投递；不让短信积压阻塞电话 |
| [Flexisip 对迟到推送的处理](https://www.linphone.org/en/news/the-end-of-ghost-calls-on-iphone-thanks-to-flexisip-2-2/) | 保留已取消呼叫上下文，处理手机醒来时呼叫已结束的情况 | 客户端先取权威状态再响铃；保留终态，迟到推送不能复活通话。文中的 iOS CallKit 行为不直接移植到 Android |
| [Kamailio TSILO](https://www.kamailio.org/docs/modules/stable/modules/tsilo.html) | 保存事务，在新 contact 出现后追加分支 | 来电事务允许已订阅但暂时断连的设备加入；仍受原截止时间约束。TSILO 内存事务本身不等于我们的持久 outbox |
| [Twilio Voice Android FAQ](https://www.twilio.com/docs/voice/sdks/android/faq)，Incoming Calls / Push notifications | 后台来电使用推送；设备 token 更新时重新登记；推送登记与短期访问令牌不同 | token 换代触发登记，不按分钟刷新接听资格；保持鉴权与推送端点分离 |
| [Android Doze](https://developer.android.com/training/monitoring-device-state/doze-standby)、[FCM 优先级与处理生命周期](https://firebase.google.com/docs/cloud-messaging/android-message-priority) | Doze 限制网络/闹钟；紧急用户可见通知与后台同步不同；异步处理须有受支持的生命周期 | 不用一分钟精确闹钟维持协议；真实来电/新短信通知按平台规则投递；无声探测不用紧急推送 |

这些来源支持架构选择，不证明 Madoka 已达到这些项目的来电成功率或耗电水平。

## 2. 首版范围与已有存储

本协议保留“持久接收意图、电话/短信解耦、按需来电”的目标，采用现有 `/v1` API 的可协商扩展。名称 Reception v2 指接收语义版本，不要求新建整套 `/v2` API。完整 mesh 的取舍见 [整合决策](https://github.com/KiriKira/gsm2sip-client-android/blob/main/docs/reception-mesh-decision.md)。

| 数据 | 首版做法 |
| --- | --- |
| 来电与参与者、取消/终态 | 复用 `call_sessions`、`rtc_control_calls`、`call_participants`、`call_events`；不另建邀请表 |
| 短信数据与增量 | 复用 `server_events` 与已有短信 API/cursor；当前没有清理机制，不增加30天到期、410、快照、bootstrap |
| 接收偏好 | 新增一张 `device_reception`，按设备保存 `protocol_version/incoming_enabled/sms_enabled/revision`；既有认证限制owner和角色 |
| 推送端点 | 扩展现有 `push_endpoints` 的端点generation与call/SMS独立退避字段；保留已有两个推送水位 |
| 待投递事件 | 继续扫描已有持久事件与未推进的推送水位；不另建outbox、attempt和receipt表 |
| 通话记录 | 沿用现有历史API和缓存；获取记录不初始化RTC，不要求独立第三套订阅协议 |

既有事件是持久事实，进程重启后从已提交水位恢复。首版部署约束为每个独立节点/数据库一个推送调度实例；三节点不是同库三副本。明确部署同库多API推送副本时，才增加数据库领取互斥；不能声称当前方案已经提供多worker exactly-once。

## 3. 必须保持的语义

- 接收偏好与当前连接、最近一次成功推送分离。`incoming_enabled=true`、有效设备授权与RTC能力决定订阅；临时失败或休眠不自动取消。端点/在线连接决定可用投递路径，`failures>0` 不等于永久失效。
- 关闭来电只关闭本设备新呼入。短信收发、历史、已接通/已成功进入接听流程的电话及用户主动呼出继续工作；开页面和收到SMS不重新开启监听。
- 首版可为所有已订阅设备建立参与者，即使当时没有可用路由；路由缺失显示受限，真实来电按固定窗口到期，不假称已送达。
- **呼出必须拆开校验。** 当前 `rtcControlCreate` 的outgoing分支也调用 `rtcCandidateSQL`；新版本显式呼出改查会话、owner、SIM、网关占用与RTC能力，不要求incoming enabled。临时活跃capability不允许覆盖持久incoming偏好。
- 推送只是提示；手机认证查询真实call状态后才响铃。保持call_id去重、活动控制租约、WebRTC和Homura dispatch账本；winner账本仅扩展为接听/整通拒接互斥裁决。
- v2空闲无 `COMPAT_IDLE`、无周期call列表查询、无capability/sleep续约、无一分钟精确闹钟。FCM不可用也不恢复常驻锁。

## 4. 最小接口扩展

`GET /v1/capabilities` 增加 `reception_v2: true`；整通拒接另协商 `global_decline_v1`（按选中Homura支持能力确认）。客户端以配置PUT的version=2表明支持新接收语义和wire2，服务端据此选择消息格式；不额外增加手机wire版本登记或中继握手API。旧客户端仍发wire1，新客户端接受wire1/2；实际收到的格式用于诊断。

只增加一项配置资源（GET/PUT），其余复用旧接口：

| 接口 | 扩展 |
| --- | --- |
| `GET /v1/devices/self/reception` | 返回version、两项偏好、revision；未迁移返回version=1和旧偏好来源 |
| `PUT /v1/devices/self/reception` | `version:2, expected_revision, incoming_enabled, sms_enabled`；首次revision=0；事务提交新偏好并切该设备接收语义 |
| 现有push-endpoint PUT/GET | 返回服务端endpoint_generation；相同端点幂等返回同generation，实际替换才递增 |
| 现有 `/rtc/control/incoming` | 新Homura在能力协商后附可选 `remaining_ring_ms`；响应仍使用现有call快照；仅无订阅时返回明确的节点范围错误 |
| 现有 `/rtc/control/calls/{id}` 和operations | 复用权威快照、accept与本机退出语义，新增协商后的decline_all kind；响应补可选 `remaining_ms` 供单调计时 |
| 现有events API/WSS | HTTP短信cursor不改；wire2 WSS分类call/SMS hint，断线恢复按对应领域补一次数据 |

配置示例：

```json
{
  "version": 2,
  "expected_revision": 7,
  "incoming_enabled": false,
  "sms_enabled": true
}
```

PUT成功返回revision=8与完整当前配置。相同expected_revision重复写、响应丢失时，客户端GET：若内容一致即视为确认，否则按**最新本地意图**重算。409只表示修订冲突；401沿现有refresh；撤销按现有鉴权拒绝。不为配置另建操作历史表或迁移接口。

同一设备修改由本地持久队列串行发送；本地intent sequence屏蔽旧响应。新服务端保存version=2后，旧capability请求不能覆盖持久偏好。升级保留用户明确意图，未确认的默认值不能将电话自动开启。

`sms_enabled` 控制手机后台同步/通知，不控制Homura采集、不删除服务端数据、不禁止用户主动查询/发送。首版不增加短信开关UI，只给领域独立运行留下稳定语义。

## 5. 关闭监听与接听竞态

本地先持久写incoming=false，再停止接纳新邀请；远端离线时保存待同步状态。短信push、启动、切网、打开页面均先检查本地状态；远端尚未确认期间可能仍收到不透明call hint，但不启动RTC。

服务器关闭事务与现有accept按统一锁序串行：关闭先提交，未接听参与者退出且后续accept拒绝；accept先提交，已进入接听流程的这一通继续。仅关闭监听不强制终止已有通话，通话结束后电话运行时退出。取消待接邀请不得全局挂断其他设备，也不得释放实际仍占用的Homura蜂窝槽。

本地快速开/关只提交最新未发送意图；在途旧请求结果确认后再发送新revision。重新开启只参与新创建的来电，不复活关闭时退出的旧参与者。全局“接收来电”控制当前安装所有配对，远端分别确认；离线节点显示待同步，本机关闭立即生效。

端点共享给短信与电话；关闭电话不DELETE端点、不清短信cursor。备份恢复不复活旧token/会话；重新配对需要明确接收意图。正式开关UI可后续发布，底层关闭语义在首版实现。

### 用户确认：拒接结束整通来电，关闭监听仅影响本机

用户已明确选择“拒接结束整通来电（所有设备停止）”。这替代旧多主机约定中的本机reject行为；实现合并时同步更新 `multi-host.md` 和README。关闭“接收来电”仍只是本设备退出，不能借此全局挂断；需要仅本机忽略时使用原participant退出语义，不混用用户的拒接按钮。

- 新能力 `global_decline_v1` 由SoulGem按Homura实际支持情况返回。Madoka在既有operations接口提交新kind `decline_all` 和已有operation_id/revision；服务端复查owner、当前设备参与者身份、尚未终止的呼入，保存待裁决请求（在现有rtc_control_calls扩展少量请求/结果字段），供Homura从现有控制通道读取，不新增peer协议。
- SoulGem接受请求只返回 `pending`，不能立即把全局call标成ended。Madoka立即停止本机铃声；在Homura确认前不声称已拒接所有设备，失败显示尚未确认。对方已挂断则收敛到已有终态。
- Homura把拒接与 `claimCallWinner` 纳入同一个安装范围持久裁决：扩展现有call_winners的decision字段（原行迁移为accepted），declined行记录原请求来源/ID；所有winner读写都必须检查decision，不能把declined行当同一赢家重入成功。拒接与接听竞争同call唯一键/SQLite事务，不能放在两个独立互不相斥的账本。
- Homura真实蜂窝仍在振铃、尚无已提交winner时，先持久提交declined，再执行蜂窝reject；若winner先提交，decline_all返回 `already_answered`，不得因迟到拒接挂掉已接通/已胜出的通话。服务器临时候选accept尚不等于Homura最终winner，跨节点竞态以Homura提交顺序为准。
- declined持久裁决成功后关闭IncomingForkCoordinator的后续offer并执行蜂窝reject；真实蜂窝END确认后，才通过既有每节点terminal队列向全部已创建/UNKNOWN分叉发布ended及declined原因，设备收到确认后停止响铃。不能仅因CAS成功就在旧terminal队列伪造ended；本机可先停止铃声，整通拒接成功以真实结束为准。没有收到结果的离线设备在恢复时取权威终态，不能复活邀请；网络隔离时无法保证所有屏幕瞬时变化。
- 蜂窝reject失败或进程在持久裁决后中断，重启根据裁决与真实modem状态继续清理/上报；不伪造蜂窝END、不提前清占用。终态队列沿现有持久重试，重复decline_all不二次接听/改变winner。
- mixed version不静默改变旧reject含义。仅在服务器和Homura支持global_decline_v1时显示/执行整通“拒接”；旧组合明确为“仅本机忽略”或提示需要升级。新Madoka不得向旧节点发不认识的kind后假称整通已拒接。全局确认可终止旧设备参与的分叉，关闭监听永远不发decline_all。

## 6. 来电窗口与多节点分叉

复用现有邀请快照，无需新增invitation资源：

1. Homura真实蜂窝振铃生成已有全局call_id，并记录本地单调25秒预算。向每个节点尝试前重新计算remaining_ring_ms；过期或蜂窝结束后停止新offer。
2. SoulGem首次成功创建时使用 `expires_at = server_now + min(25000, remaining_ring_ms)`；已有call/同一操作重试返回原截止时间，不延长。旧网关缺字段时保留原服务端25秒窗口。取消仍以真实蜂窝END为准。
3. 各节点的deadline可以略有不同。相对剩余预算可能包含请求在途延迟，不声称跨节点精确同步；Homura发送终态与执行前核验保证真实结束后不接听。不加入5秒时钟偏差拒绝机制。
4. **`NO_INCOMING_SUBSCRIBERS` 只代表当前节点。** Homura现有代码已经将 `GATEWAY_UNAVAILABLE` 归为REJECTED，并继续遍历其他ready节点；不是本轮新修复的分叉缺陷。新错误码仅改善诊断，加入相同REJECTED分类；旧网关继续收到原错误。UNKNOWN创建仍按现有不确定结果/terminal队列处理。
5. 所有节点都明确无订阅时才结束本轮远端offer，保持原蜂窝振铃，不自动接听；继续现有最终通话记录。单个无订阅节点不创建伪造参与者或占用新的控制槽；不为此重写未接历史存储。
6. 同一次来电有可用WSS和push时立即同时发，不等500ms回执。客户端按call_id/revision去重，先查当前状态再创建一次Telecom。
7. 推送在截止前投递，手机预算取服务端remaining_ms并使用单调时钟；终态、已退出参与者、不存在或本地关闭均不响。取消hint可重复，已经响铃时也有本地截止计时。终态保存沿用现有策略，不新设保留/清理系统。
8. 接听继续现有accept→媒体准备→Homura winner CAS；真实ACTIVE只由蜂窝/媒体事实确认。活动控制轮询与20秒租约本次不改，空闲监听不承担该租约。

## 7. Wake wire2 与推送调度

Kyubey `28213b59` 的 `src/relay.js` 丢弃扩展字段，`src/fcm.js` 固定HIGH/60秒，因此中继改造是本轮上线依赖。[源码](https://github.com/KiriKira/Kyubey/blob/28213b59122936e3b44c7f0a2d41dc8cd43e19f0/src/fcm.js)。

最小wire2 envelope示例（送入FCM data时数值按要求转字符串）：

```json
{
  "v": 2,
  "kind": "call",
  "device_id": "origin-pairing-device-uuid",
  "wake_id": "call-event-187",
  "call_id": "call-uuid",
  "revision": 4,
  "endpoint_generation": 2
}
```

`kind=call/sms/check`；call查询快照区分邀请/取消，sms只运行短信同步，check保持provider validate-only而不唤醒手机。wake_id由来源现有事件标识构造，在 `(来源配对,device_id,kind)` 内唯一，不是全网序号。消息不含号码、短信正文、token、任意取数据URL。手机仅匹配已配对来源；来源代发也保留原device_id，不能换成出口节点的身份。

端点换代由服务端generation和手机登记结果识别；Kyubey无状态透传，不假装能判定全局最新代际。若新端点消息先于登记响应到达，手机可对已配对来源做一次有界端点GET确认后再取业务，不因登记响应竞态漏掉真实来电；短信无需重置已保存cursor。

传输使用现有标准HTTP头作为唯一TTL/优先级来源，删除正文ttl_ms：`TTL: 18`（非负整秒）与 `Urgency: high|normal|low|very-low`。SoulGem发送前按call剩余时间向下取整，预算不足3秒不再发新来电提示；Kyubey解析/限制头值，扣除自身处理耗时后写FCM android.ttl，high映射HIGH，其余映射NORMAL。无头的旧请求保留60秒/high兼容默认；非法头返回400，处理期间预算耗尽不发送；TTL=0按即时投递不存储语义处理，R1的call发送方不会产生不足预算的0秒请求。每请求最多3秒，总重试受原call预算约束；call不使用跨call共用collapse key。该头部契约也适用于采用相同约定的其他HTTP推送端点；仅接受任意HTTPS URL并不证明完整兼容所有UnifiedPush/WebPush实现，其他端点仍须匹配载荷和认证约定。

旧Kyubey忽略新增字段并输出v=1，因此新Madoka无call_id时做一次有界call列表查询，仍检查本地开关/当前终态，不恢复周期监听。新SoulGem对已迁移设备可发送wire2到旧中继；旧中继不保证真实TTL或字段透传，诊断标为旧投递格式。**不可对旧客户端一律发送v=2：当前 `MadokaMessagingService.kt:12` 会丢弃v不是null/1的消息。** 服务端须按已有接收配置版本选wire，新中继保留v1编码。新中继可在现有check响应增加能力头辅助确认TTL支持，无新增握手流程、无定时探测；HTTP201本身不能证明字段已透传。

Kyubey在有界解析、kind校验后按 `(sealed endpoint, call|sms|check)` 限流，call有独立额度，不能被SMS/check耗尽当前共享的60次/分钟。可复用现有Rate Limiter binding的独立key，不增加全局配额数据库；仍有FCM项目级限流，分kind不承诺消除此限制。多节点共享同endpoint的事实保留，不能按device_id分桶让发送者无限绕过额度。

推送worker保留现有事件扫描，改动必须包括：

- 创建/修改来电事务成功提交后，向进程内有界合并通知通道提交 `(device_id,call_id)`；dispatcher立即扫描该设备待发事件，WSS也及时通知。不在事务内发网络请求。原每秒扫描仅补漏，提交后进程崩溃/通知丢失仍由持久事件恢复，不新增消息总线。
- call与SMS独立水位、退避、并发配额；call可用4个worker、SMS用2个，都是有界默认值。每个设备/领域进程内单任务，不用每个tick创建无界goroutine。
- call读取水位之后的事件并按call_id聚合到当前状态，处理该批每个call；全部已受理或明确过期/终态无需发送后才推进**已处理范围**。不能仅取MAX事件只发最后一通，就跳过另一通来电。
- 用有界进程内缓存记录 `(device_id, endpoint_generation, call_id)` 最后已受理的事件/revision，以及该call失败的next_attempt_at。批中一通失败不使其他已成功call每秒重发；更高revision（特别是取消）不被旧缓存屏蔽，端点换代重新投递仍有效业务。缓存只优化重复投递，不替代持久水位，重启丢失可安全重发。水位仍按连续已处理范围推进；条目仅在对应水位越过、generation失效或该业务无需再投递时清理，数量有界，极端淘汰允许重复但不跳事件。
- SMS提示成功只推进SMS水位；call成功只推进call水位。事件在发送期间追加，下一轮继续；新事件不受上一通已结束call的长退避阻塞。
- 网络错误不推进；重启可重发但客户端幂等。call尝试受原deadline约束，初次后约1/3秒退避，不超时重发过期邀请；SMS沿独立指数退避到5分钟。provider受理仅表示handoff，不能标记设备已响铃。
- 替换endpoint时旧worker成功/失败只能更新自己读到的generation；旧410不能删除新端点。暂时失败不得以failures>0移除持久订阅。
- `received/fetched/presented/suppressed` 作为手机本地诊断阶段；首版不增加回执HTTP、持久receipt表，也不为拿回执额外唤醒。

无完整outbox不等于只存内存：已有call_events/server_events与水位承担恢复。未来需要跨节点**持久接管**任务或同库多调度实例时，才加入任务claim/lease；届时复用同一wake_id与deadline。

## 8. 短信/电话运行时解耦

`sms_enabled=false` 是服务端与本机的共同约束：服务器停止该设备SMS push及SMS WSS hint，手机停止自动SMS后台同步/通知，仍允许显式刷新/发短信，不影响其他设备、电话或Homura采集。worker发送前重查状态；已在provider中的提示由本机拦截。停止时不删除短信、不推进手机已持久cursor；重新开启时服务端对积压生成一个合并hint，手机从原cursor补齐并按现有去重/通知策略处理，不逐条补发旧push。电话关闭与SMS关闭互不隐式设置。

HTTP短信feed/cursor与通知去重账本保持原格式；当前server_events已是短信事件，不凭空建立新feed/快照。旧缓存迁移只增加领域配置和wire版本，不迁移短信cursor。

`PushWake`/WSS按版本分发：wire2 call仅调CallReceiver查单call；wire2 sms仅调SmsSync；旧wire1混合提示允许继续双路，但本地incoming关闭时绝不启动电话。通话记录按原历史API在前台/已有数据同步任务更新，不能启动RTC。

提取一个进程级接收协调器，管理偏好、短任务与共享传输即可；不用为每个topic新建服务。`HostBackgroundService`不再拥有电话引擎生命周期；通话runtime只在有效新邀请或用户主动拨号时创建。`NodeMonitor.maintain`在v2停capability续约，SMS工作不能附带全节点电话维护。

有系统推送时空闲可以断开仅为来电维护的WSS；SMS按已有推送/系统任务/前台补拉，不让“短信默认开”迫使电话service常驻。WorkManager可用于延迟补齐，但不是来电定时器。新SMS用户可见通知可使用即时类别，静默状态同步用普通类别，避免所有hint都用HIGH。

## 9. 无FCM和混合节点

无系统推送时，保持一条每authority共享的分类WSS（多个独立authority可能各一条），无常驻CPU锁，非唤醒心跳默认120秒；依据真实连接断开可在60–300秒范围调整，单侧ping。只在路径变化/真实失败时合并重连，1秒到5分钟退避；信号强度抖动不重建连接。

重连后一次读取当前有效call和SMS增量，平时不空轮询calls、不精确闹钟续租。断流期间存在来电延迟风险；单纯WSS/FGS不能保证Doze下秒级到达，这个限制必须显示，不能退回持续持锁让测试通过。手机工作锁初值10秒，工作完成立即释放，活跃通话独立所有者。

v2接收选择只有自动路径：有合格push用push，否则尽力stream；用户明确关闭incoming高于一切。首版不增加三种产品模式配置。正式无GMS系统provider适配另开工作，接口抽象本身不算已解决手机唤醒。

按authority协商：旧server保留原adapter并标明旧协议限制；新server的旧设备继续原候选逻辑。用户关闭来电的本地拦截对任何版本都有效。一个旧节点不将其他v2节点降级；不承诺混合旧节点已达到全部v2省电指标。

## 10. 迁移与回退

优先部署兼容server与Kyubey，再升级Homura和Madoka；旧中继仍可兼容接收，但不计为已落实真实TTL。配置PUT成功的同一事务使设备进入version=2；在已有call结束后切换，客户端停止旧空闲续约。短信cursor/执行账本不动；不增加独立migration API。

回退功能由配置资源/服务端设备级开关完成，保留关闭意图与短信偏好；只有incoming仍开启才恢复对应legacy接收。旧客户端不能通过自动capability请求抹掉v2关闭，必须由用户明确重新授权。v2新列/表为扩展迁移，不在回退时删除。

仅复用现有事件的首版不等于完成真正去中心化复制。未来mesh代发复用kind、来源device、wake_id、call_id、generation与deadline；手机合并到单入口还需要独立的授权路由，不能拿代发功能冒充。
