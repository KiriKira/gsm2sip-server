# PLAN — gsm2sip-server 三端转发服务 v1

更新：2026-10-03。审查时仓库为空（无实现代码）；以下均为待实现任务，未部署服务器。

本文是 server 仓库的可执行任务清单，协议字段、HTTP 路径、状态名和边界条件以 [三端协议 v1](docs/protocol-v1.md) 为准；跨仓库依赖和联合验收以 [联合 roadmap](docs/roadmap.md) 为准。不要在此文复制或另行定义 wire protocol。

## 旧计划审视结论

旧 gateway plan 中“Asterisk/PJSIP、TLS、SRTP、独立 API”的方向适合作为基线，但短信部分写“先保留 SIP MESSAGE，server 接收后存库/API”，没有说明 Asterisk 到 Go API 的持久化边界。Asterisk PJSIP 的确可以用 `message_context` 把入站 MESSAGE 路由到 dialplan，也有 `MESSAGE(body)` 和 ARI `TextMessageReceived`；这些能力本身不等于把短信写入 PostgreSQL、事务提交后才响应。RFC 3428 说明 202 Accepted 可表示 relay 已接受，但端到端投递不保证。故 v1 生产短信走 HTTPS 事件上传和 HTTPS 命令领取；SIP MESSAGE 仅放在显式启用的测试/迁移 adapter 中，且如果未来实现独立 SIP adapter，只有在事件事务落库后才可由该受控 UAS 回应 202。禁止把 Asterisk 一般 200/202 或非持久 ARI 事件误当作 durable ACK。

旧计划还把主力机集成 SIP 留到以后决定，这与当前“主力机集成接打电话必需”不符。Server 的首个完整呼叫验收要包含 app SIP/TLS+SDES-SRTP、Android Telecom、休眠唤醒、DSDS 单通话槽和按稳定 `sim_id` 路由。

## 建议仓库结构

```text
PLAN.md                    # 本任务清单的正式版
docs/protocol-v1.md             # 三端协议唯一权威源（本仓库维护）
openapi/openapi.yaml            # HTTP/JSON API，CI 校验
cmd/api/main.go                 # HTTPS API、WSS 会话
cmd/worker/main.go              # outbox、推送、ARI 呼叫编排
internal/{auth,devices,sims,messages,commands,calls}/
internal/{store,httpapi,websocket,asterisk,push}/
migrations/                     # PostgreSQL 有序迁移
deploy/compose.yaml             # caddy/api/worker/asterisk/postgres
deploy/Caddyfile
deploy/asterisk/{pjsip.conf,extensions.conf,ari.conf,rtp.conf}
deploy/.env.example             # 仅变量名/占位值，禁止真实密钥
scripts/{bootstrap,backup,restore,healthcheck}.*
```

API 与 worker 使用同一 Go 镜像，分别启动；数据库事务 outbox 驱动 WSS/FCM 通知，不额外引入 Redis/MQ。Asterisk ARI 仅供 worker 通过 Compose 私网访问。公网仅开 Caddy HTTPS/WSS、Asterisk SIP/TLS 和配置的 SRTP/RTP 范围。

## 实施任务

### S0 — 构建与部署骨架

**文件：** `go.mod`、`cmd/api`、`cmd/worker`、`deploy/compose.yaml`、`deploy/Caddyfile`、`deploy/.env.example`、`scripts/bootstrap.*`、`PLAN.md`。

**工作：** 建立 Go lint/test/build、配置加载与错误处理；Compose 启动 Caddy、API、worker、Asterisk、PostgreSQL；加入 DB migration 执行、存活/就绪探针、配置密钥生成和部署说明。镜像固定版本/摘要；PostgreSQL、ARI/AMI 不映射公网，秘密不进镜像和 git。

**验收：** 全新 VPS 依文档启动；TLS HTTPS 可用；外网无法访问 PG/ARI/AMI；端口扫描只看到文档列出的服务端口；缺关键 secret 时服务拒绝启动。

**依赖：** 无。后续任务依赖此层可重复启动。

### S1 — 核心 schema、迁移和一致性约束

**文件：** `migrations/0001_identity.sql`、`0002_messages.sql`、`0003_calls.sql`、`internal/store/*`。

**工作：** 实现 owner、gateway、client、SIM binding、message/event、gateway command、outbox、call intent/session、push token、audit 数据。按公共协议增加唯一约束与外键；出站 command 与 message/outbox 同事务；入站 event 与 message/outbox 同事务；claim lease 原子更新；gateway 活动呼叫槽使用事务锁/CAS。SIM binding 保存服务端 UUID、绑定状态、revision 和当前运行观察值（SIM 身份核验在网关完成）；映射变化递增 revision 并审计。

**验收：** 迁移可从空库和前一版库重跑/升级；并发插入相同幂等键只有一个资源；同 ID 不同 payload 拒绝；两个并发呼叫事务只能取得一个 gateway lease；回滚事务没有残留 outbox。

**依赖：** S0；schema 与公共协议 v1 同步冻结。

### S2 — 配对、认证、SIM 权限和撤销

**文件：** `internal/auth/`、`internal/devices/`、`internal/sims/`、`internal/httpapi/pairings.go`、`internal/httpapi/auth.go`、对应 migrations 与 tests。

**工作：** 实现本地管理员初始化、限时一次性配对、gateway/client 不同身份、短期 access token、轮换和撤销。按 owner/device scope 校验所有查询和写请求；配对后创建独立 SIP endpoint/auth 凭证。SIM 绑定只能由 gateway 上报本地确认；换卡/指纹不匹配置为需确认，所有旧 revision 的发短信/拨号请求 fail closed。`subId`/slot 不作为服务器选卡地址。

**验收：** 配对码重放、过期或角色不符均失败；撤销立即阻止 API 与 SIP 注册/拨号；SIM 换槽不混卡；SIM 更换或 revision 过期不发送、不拨号且不给默认卡兜底；client 无法读写其他 owner/gateway。

**依赖：** S1；gateway 本地 SIM 身份行为按联合 roadmap M1 对接。

### S3 — HTTP/WSS、心跳、游标与 outbox

**文件：** `openapi/openapi.yaml`、`internal/httpapi/`、`internal/websocket/`、`internal/outbox/`、`internal/devices/heartbeat.go`。

**工作：** 按协议实现 `/v1` API、WSS events、gateway heartbeat 与 client ready/在线会话状态、游标补拉、权限过滤和限速。WSS 事件只作 `events_available` / `commands_available` / `call_pending` 提示，不承载唯一状态或短信正文。每 30 秒心跳、90 秒无心跳标 stale；恢复连接后按游标补拉。worker 重连/领取使用退避和 jitter。以服务端收到时间判在线，SIP REGISTER 状态独立记录。

**验收：** WSS 断线期间服务端重启/数据库恢复后仍能补取所有已提交事件；跨 owner 游标无数据泄漏；过期 cursor 按协议返回 resync；重复通知无副作用；限流返回契约错误码。

**依赖：** S2；协议 v1 API schema。

### S4 — 短信持久化与未知结果

**文件：** `internal/messages/`、`internal/commands/`、`internal/httpapi/messages.go`、`internal/outbox/`、`migrations/0002_messages.sql`、`tests/integration/sms_*`。

**工作：** 入站短信事件通过 HTTPS 幂等上传；数据库 commit 前不得返回 durable 成功。出站 `POST /messages` 创建一条持久任务；WSS 只唤醒，gateway 后续 HTTPS claim 获取命令。Claim 校验 SIM ID、revision、TTL、gateway 身份和设备能力，并使用原子单次 claim；同网关的响应丢失可重复读取同一任务，claim 超时不得转交或产生第二次发送。gateway 状态事件重复提交需幂等；`dispatching` 后无确证回执转 `unknown`，lease 超时不得自动再触发 modem 副作用。SMS 记录、command、audit 和 outbox 采用同一事务边界。

**验收：** 两卡中文/emoji/分片短信端到端；同 Idempotency-Key/事件 ID 重试不创建副作用；同 ID 不同 payload 返回冲突；关停 API/worker/DB 后恢复不丢已提交消息；模拟发送后丢回执为 `unknown` 且不会再次 claim 成新发送；送达状态只有收到相应送达报告才标记。

**依赖：** S1–S3 和 gateway M2 HTTPS journal/ledger。

### S5 — Asterisk TLS、强制 SRTP 与网络边界

**文件：** `deploy/asterisk/pjsip.conf`、`extensions.conf`、`ari.conf`、`rtp.conf`、`deploy/compose.yaml`、`scripts/healthcheck.*`。

**工作：** 固定 CI 验证过的 Asterisk/PJProject 版本；只启用 PJSIP，不使用 `chan_sip`。gateway/client 分开 endpoint/auth/AOR；生产只监听 TLS 5061，SDES-SRTP 强制 (`media_encryption=sdes`、`media_encryption_optimistic=no`)，`direct_media=no`。为 NAT 配置及实测 `rtp_symmetric`、`force_rport`、`rewrite_contact`；公布窄 RTP UDP 段并配置公网地址。ARI 绑定内网，认证值为密钥，不可被 HTTP proxy 外露。拒绝匿名来电和未授权出站。

**验收：** 错证书/错误主机名/无 TLS/无 SDES offer 均失败；抓包证明无明文 SIP/SDP key 泄漏和 RTP fallback；公网只能访问 TLS SIP 与 RTP 配置范围；两端经不同 NAT 注册并可双向锚定音频；Asterisk 重启/证书续期后可恢复注册。

**依赖：** S0；client M0 SDK 探针和 gateway M0 音频条件。该项只能先用软电话测试网络/SRTP，最终仍须真网关验收。

### S6 — 呼出 intent 与 Asterisk ARI 编排

**文件：** `internal/calls/intents.go`、`internal/asterisk/ari/`、`internal/calls/outgoing.go`、`deploy/asterisk/extensions.conf`、`tests/integration/call_intent_*`。

**工作：** API 原子检查身份、号码/SIM/revision、gateway 在线和全局 call lease，创建短时单次 intent；只保存 token hash。主力机的 INVITE 先由 Asterisk 验证 SIP endpoint，再交 Go ARI app。worker 原子消费 intent，核对 call/client/gateway/SIM/revision/状态，之后才要求 Asterisk 创建 gateway INVITE；使用受控的 Request-URI 与服务器添加的 SIM 元数据头，剥离/忽略客户端自带路由头。所有通话状态、channel mapping 和超时写入 `call_session`；服务端失联时 fail closed 并清理双方通道。

**验收：** 正确意图走对 SIM；无效/过期/重放 token、错误主叫 SIP 账号、号码策略拒绝、旧映射在 gateway INVITE 前被阻断；SIP retransmission 不重复拨号；两并发 intent 只有一通 GSM leg；CANCEL/BYE/超时/服务重启竞态最终释放 lease。

**依赖：** S1、S2、S5、client M3 SIP UA、gateway M3 call ledger/header handling。

### S7 — 呼入 FCM 唤醒和 25 秒生命周期

**文件：** `internal/calls/incoming.go`、`internal/push/fcm/`、`internal/asterisk/ari/`、`internal/httpapi/clients_ready.go`、`tests/integration/incoming_wakeup_*`。

**工作：** 认证 gateway 入站 INVITE，校验 `sim_id`/revision，落库 pending 会话并持有 gateway call lease。worker 发高优先级 FCM 唤醒提醒（仅 call ID/expiry，不放短信/凭据）；Asterisk 向 gateway 回 180 但先不 answer。主力 app 恢复 SIP REGISTER 后上报 ready；worker 再让 Asterisk 呼叫正确 client contact。仅收到用户接听且 SIP client leg 确实接通后才向 gateway 发 200。拒接、主叫取消、FCM 失败、SIP 未注册或 25 秒到时要取消 pending channel、释放 lease 并记录具体原因。一个 primary client 的 endpoint 即为 v1 目标。

**验收：** app 前台、锁屏/Doze、进程被杀、FCM 延迟/丢弃/迟到分别实测；250 ms 级内状态检查不作为 SLA，按 25 秒 deadline 管理；gateway 用户没答前始终不收到 200；迟到 push 不再弹呼叫；busy/未注册返回明确结果；Core-Telecom 负责系统来电 UI，FCM 只负责唤醒。

**依赖：** S5、S6、client M4 push/ready/Core-Telecom，gateway M3/M4 保持来电并按 SIP 200 接蜂窝。

### S8 — DSDS 仲裁、运维和可恢复发布

**文件：** `internal/calls/lease.go`、`internal/commands/claim.go`、`deploy/`、`scripts/backup.*`、`scripts/restore.*`、`docs/operations.md`。

**工作：** gateway 级一个 cellular voice lease；通话中是否允许出站短信按目标机验证的能力决定；未验证时暂缓发送并保留原 SIM 与原 TTL，过期明确 expired，不转卡；第二路电话忙线拒绝。加入心跳、每 SIM service state、ARI contact、outbox lag、推送结果、DB/证书过期指标。配置 DB 最小权限、加密备份、保留期和删除流程；升级/迁移/回退手册。日志屏蔽正文、完整号码、token、Authorization、SDES 密钥。

**验收：** SIM A 通话时 SIM B 拨号被拒绝，未验证并发发送能力时短信只留在原 SIM 队列，已验证设备按能力发送；来电同时抢占符合协议忙线规则；24 小时屏灭和三端重启/网络断连/DB 恢复后状态可解释；恢复备份不会把 `unknown` 短信变回 queued；恢复演练证明备份可读。

**依赖：** S1–S7 与真实目标硬件集成。

### 可选兼容项 — SIP MESSAGE migration adapter

**位置：** `internal/legacy/sipmessage/` 或单独 `cmd/sipmessage-adapter/`；Compose 默认不启动，`profiles: [legacy-sip-message]` 显式启用。

**实现：** 只收已认证 gateway 的历史 inbound MESSAGE；校验长度/内容类型/ID；按协议映射为同一 HTTPS 规范事件；必须是能控制 SIP 最终响应的独立受控 UAS（或经证明可控的实现），唯一键落库和事务提交成功后才回 202；不能先由 Asterisk 自动 202 再补写 API。失败回可重试响应。禁止在 Asterisk dialplan 里直接 `MessageSend()` 给 client 并假设它已进 inbox；出站短信不从 adapter 执行，防止 SMS 双发。

**验收：** DB 断开时不回成功；重复同 payload 只写一次；同 ID 不同 payload 返回冲突；关 adapter 不影响 HTTPS v1；生产默认 Compose 不开端口。

**依赖：** S4；仅在旧 gateway 仍需兼容时实施，不阻塞 v1。

## 官方一手资料

- [Asterisk PJSIP 配置参考](https://docs.asterisk.org/Latest_API/API_Documentation/Module_Configuration/res_pjsip/)：SRTP 的 `sdes` 与 `media_encryption_optimistic`、MESSAGE `message_context`。
- [Asterisk NAT/PJSIP](https://docs.asterisk.org/Configuration/Channel-Drivers/SIP/Configuring-res_pjsip/Configuring-res_pjsip-to-work-through-NAT/) 与 [Transport/TLS](https://docs.asterisk.org/Configuration/Channel-Drivers/SIP/Configuring-res_pjsip/PJSIP-Configuration-Sections-and-Relationships/)。
- [Asterisk MESSAGE()](https://docs.asterisk.org/Asterisk_23_Documentation/API_Documentation/Dialplan_Functions/MESSAGE/)；[ARI data models](https://docs.asterisk.org/Asterisk_22_Documentation/API_Documentation/Asterisk_REST_Interface/Asterisk_REST_Data_Models/)（含 `TextMessageReceived`）。
- [IETF RFC 3428](https://datatracker.ietf.org/doc/html/rfc3428)：SIP MESSAGE 202 只表明 relay 接受，端到端投递不保证。
- [IETF RFC 8599](https://datatracker.ietf.org/doc/html/rfc8599)：SIP push 后 UA 刷新 REGISTER 的行为基础。
- [Firebase Android FCM priority](https://firebase.google.com/docs/cloud-messaging/android-message-priority)：高优先级用于时效性用户可见消息；FCM 可能依使用行为降级。
- [Android Core-Telecom](https://developer.android.com/develop/connectivity/telecom/voip-app/telecom?hl=zh-cn)：`CallsManager.addCall` 系统呼叫集成。
- [PostgreSQL COMMIT](https://www.postgresql.org/docs/current/sql-commit.htm)、[`INSERT ... ON CONFLICT`](https://www.postgresql.org/docs/18/sql-insert.html) 与 [unique constraints](https://www.postgresql.org/docs/current/ddl-constraints.html)：事务确认、幂等与并发约束。


## 必须补齐的呼叫和运维细节

- 配对返回 SIP bootstrap、受控轮换和 client_id/gateway_id 到 auth/AOR 的一对一关系；对应协议 devices/self 配置与轮换接口。撤销后清除旧 contact；不以 From 作为客户端身份。
- `/ready` 必须核对具体 call_id、wake_nonce、deadline 与真实 REGISTER；不接受不带来电上下文的 ready。重复 ready 不重复建 leg，迟到 A 来电不能触发 B。
- ARI/Stasis coordinator 是唯一编排者，持久保存 call_id 与每个 PJSIP channel 的映射；未消费 intent 30s 到期可释放，dialing/active 不按意图 TTL 释放。实际呼入可抢占未消费 intent。
- gateway placeCall 崩溃后 unknown 不重拨；worker/ARI 断线后对账活跃通道再放行新电话，不能 lease 到期就认为蜂窝通话结束。
- RTP/RTCP/SRTCP 端口由 Asterisk 分配，默认示例 UDP10000–10199 与 Compose、VPS 防火墙一致，SDP 使用可达公网地址，两腿分别验证 SDES。HTTP API 只维护 call/channel ID。
- 证书由受控 ACME 流程提供给 Caddy 和 Asterisk（共享或复制 PEM，私钥最小权限），验证续期后 Asterisk transport 实际使用新证书；需要重启/重载的版本预留维护流程，不声称 Caddy 续期自动更新 SIP。
- DB durable ACK 要求 PostgreSQL `synchronous_commit=on`，不得在缓存入队后提前成功。正文保留清理不删除幂等 tombstone；容量满拒绝新任务或告警，不能丢已接受任务。
