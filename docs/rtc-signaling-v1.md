# RTC v1 协商模块（尚未发布网络接口）

本契约对应 `internal/rtcsignal`、[command schema](../protocol/rtc-v1/command.schema.json)
和六份 [fixtures](../protocol/rtc-v1/fixtures)。它是 W2 的独立协商模块，不是已经可以使用的 WebRTC 通话 API。
`/v1/ws` 继续只做唤醒；现有 OpenAPI、能力声明、SIP intent 和呼叫控制入口均未改动。
在共享通话占用、网关 winner/fencing、引擎标记及 ARI 恢复隔离落地前，不注册新信令端点。

## 身份和权限

协调器提供可信 Binding：node/owner/call/leg/attempt/client/gateway/SIM ID 与 mapping revision。
一个 leg 只有一个主机和一个网关；它不代表所有来电候选，也不是跨节点 winner。
所有 ID 是 1–128 字节的 ASCII 字母、数字、下划线或连字符。

参与者命令只携带 version、call_id、leg_id、attempt_id、operation_id、generation、revision、kind
及该 kind 的 payload。sender 由已认证会话确定，recipient 由 Binding 推导。
命令不得声明 owner/sender/recipient，未知字段、重复 JSON key、显式 null、数组和无效 UTF-8 被拒绝。
同一设备内 operation_id 对解码后的命令字段进行 SHA-256 去重；等价 JSON 转义/空白不会制造新操作。
相同键换 payload 为冲突。去重键包含设备，不能借别人的操作 ID 领取 ACK。

PostgresRepository 创建时必须注入 Authorizer，没有默认放行实现。Authorizer 在事务内重新验证当前会话、
设备状态、owner/participant、通话引擎、共享占用、SIM revision 及授权；必要的资源锁必须与撤销/变更操作协调。
当前只有测试用 authorizer，**尚无生产 call coordinator 适配器**。不得直接使用主机提交的 Binding 创建记录。

## 状态、顺序与终止

```text
waiting_offer --主机 offer--> waiting_answer --网关 answer--> negotiated
      ^                                                       |
      +--------主机 restart，generation + 1--------------------+

任何非终态 --close / 授权撤销 / 协商或授权过期--> closed
```

无论蜂窝去电还是来电，主机是唯一 SDP offerer；网关只能 answer。negotiated 仅说明服务端接受了该组 SDP，
不证明 native 应用成功、ICE 连通或双向音频，更不触发拨号、接听、winner、ACTIVE、计时或占用释放。
媒体/蜂窝状态仍须由后续协调器按总计划合并；没有这些信息时不能提前执行蜂窝操作。

初始 generation/revision 均为 1。offer/answer/restart 使用精确的当前 revision；其他命令 revision 必须为 0。
每个新操作提交后 revision 加一，candidate 与描述的并发可能让 SDP revision 冲突，调用方须读快照后重试。
close 不要求 revision，避免挂断被并发 ICE 阻塞；它可以携带当前或更早的 generation，终结同一 call/leg/attempt，
保证 ICE restart 与用户取消并发时仍由取消获胜。它不能指向未来 generation 或另一次 attempt。
重复操作返回原 ACK，不增加 revision、不恢复状态、不重新发出 SDP/ICE。

restart 只允许 negotiated 状态，由主机发起；清空旧代次候选/描述，但保留历史操作摘要。
旧 generation 的新消息被拒绝；旧操作的历史 ACK 不是当前状态。closed 不可重开，迟到应答/候选不能使其复活。
协商超时需要终结该 leg；不得自动创建另一通蜂窝呼叫来“恢复”。

## 上限、有效期和重连

| 项目 | v1 限制 |
|---|---|
| JSON 原始帧 | 96 KiB，最多两层对象，无数组 |
| SDP | 非空、有效 UTF-8、无 NUL，最多 64 KiB（另受原始帧限制） |
| 单 candidate | 最多 2048 UTF-8 字节，无 CR/LF/NUL；SDP mid 使用 ID 规则，m-line index 为 0–15 |
| candidate 数 | 每端每代 128，end-of-candidates 后该端不能再提交 candidate |
| generation | 1–8，超限明确结束/失败，不重新拨号 |
| 去重记录 | 每 leg 最多 2304 项；不通过淘汰旧键让操作可重执行，预算耗尽仍可 close |
| SDP/ICE 有效期 | 15 秒，且不超过授权期限 |
| 一次协商 | 30 秒，且不超过授权期限；提交 offer 不能延长该期限 |
| 一次授权 | 最长 60 秒；只能由协调器在事务中重新校验后延长，不是参与者 wire 操作 |
| 持久 checkpoint | 最大 4 MiB；独立数据格式，不得作为 wire snapshot 或写日志 |

Schema 的 maxLength 以字符计，运行时另做 UTF-8 **字节数**校验，不能只凭 schema 验证大 SDP/candidate。
candidate 可早于描述到达，但服务端 Pending 按“描述、候选”交付；每个条目保留其原 sequence 和到期时间。
接收端要在对应 setRemoteDescription 成功后才能把有界缓冲交给 native。
普通候选/EOC 必须保持服务端发送顺序；恢复时只排序同一代的描述前缓冲，不把缺失序列当成成功同步。

Read 先提交权威快照，终态不再附带任何信令。候选计数保留在 TTL 清理之后，避免“早期候选过期、EOC 仍有效”
被误当完整集合：这种情况下返回 ReplayComplete=false 和空 Signals，不交付部分重放。
已有 native 连接若仍能证明其本地描述和媒体有效，可按恢复策略继续；需要重放但无法证明完整时，
在同一业务 call 下开启新 generation（或终结尚未完成的 leg），不能无条件重放历史或再次拨号。
信令读取或 ACK 不续期授权；ExtendAuthorization 不能复活过期/撤销的授权，也不移动既有协商期限。
Revoke 只关闭信令授权；后续运行时必须让两端暂停/终止媒体并让网关收敛物理通话，不能只关 WSS。

## 提交与存储边界

`0015_rtc_signal_legs.sql` 只新增独立表/索引，不改变旧 call_sessions、gateway_call_slots、短信或身份表。
当前运行时不创建该表记录。它不是第二套呼叫占用或历史，也不能替代共享业务呼叫事务。

PostgreSQL 使用行锁序列化同一 leg 的跨进程写入，强制事务 synchronous_commit=on；
状态、操作摘要与待发信令写在同一 checkpoint，成功提交后 Service 才能返回 ACK/快照。
原始 SDP/ICE 与去重摘要分开处理；Sweep 使用索引、批量上限 100 和 SKIP LOCKED 清理过期 payload，
并提交协商/授权终态。保留摘要可以在清理 SDP 后仍拒绝重复执行。
启用未来 RTC admission 前必须先启动清理 worker；当前未启用，也没有插入记录的生产路径。
TTL 指当前逻辑记录清理，不代表 PostgreSQL WAL/备份中的历史字节被即时擦除；禁止记录 checkpoint/SDP/ICE 明文日志。

已验证：使用升级后的库运行 0014 及以前的 migration 文件集不会删除 0015 记录或报未知版本。
这只证明当前 migration runner 的兼容性，完整旧二进制、活动呼叫与数据回退仍需 W4/W5 验收。

## Android 接收组件

两仓库中的 `com.callagent.rtc.RemoteIceBuffer` 暂时保持源码和测试字节一致，未接入服务/UI，也未引入 SDK 依赖。
它绑定 node/call/leg/attempt/generation 与预期远端设备；忽略其他 scope/旧代，拒绝冒名 sender。
它要求单个远端描述，使用实例/代次 token 隔离迟到 native 完成回调；成功应用描述前不释放候选。
重复 sequence 不重复执行，换 payload、过期缓冲、溢出、EOC 后候选及时钟倒退会清空并关闭缓冲。
closed 不可由新 generation 重开。调用方仍须把返回的候选放到同一个 PeerConnection 串行执行器并在执行前复核代次。
该类不负责 JSON、WSS、认证、重连策略或用户接听授权，不能仅实例化它就标记 WebRTC 已可用。

两端已增加相同的 `RtcCommandCodec`，严格解析/编码本命令契约，使用同一份六种 canonical fixtures；
不建立网络连接，不解析服务端 snapshot/ACK，也不启动音频。跨仓库脚本同时检查 codec、guard、测试和 fixtures 字节一致。

## 独立 WSS 传输模块

`NewWebSocketHandler` 已实现该 Service 的 WSS 传输，测试挂载在
`/rtc/v1/legs/{leg_id}/signals`；生产 Server.Handler/cmd/api 尚未注册此路径，不能将测试挂载视为已发布接口。
它要求直接 HTTPS，拒绝 Origin 和不可信证书；不会自动相信代理转发头，TLS 终止拓扑在接入时另行确认。
必须显式注入会话验证函数和已授权的 Repository；现有生产协调器/Authorizer 尚未完成。

输出为 type=snapshot（state.snapshot/signals/replay_complete）、type=ack（ack/duplicate）或 type=error（code）。
snapshot 按各参与者 scope 读取已提交状态，signals 为数组，终态或缺失重放时为空数组。
初次连接先读快照；每条命令和定时 pulse 重新校验会话，历史 ACK 也不能绕过撤销。
默认独立预算：全 handler 64 连接（含待鉴权/升级者）、每设备 2 连接、每连接 16 条待发结果、
16 条命令突发/每秒恢复 8 条；10 秒复验会话，3 秒存储/写截止时间，45 秒读/pong 窗口。
慢消费者/过载只关闭相应语音连接；Close 只关闭该 handler，不接触短信 wake 连接或 HTTP server。
这个传输不创建业务 call、不开放媒体，也没有完成 Android WSS 客户端/生产协调器的接入。

真实 WSS + Pion 媒体协议测试与明确未验收项见 [协议验收记录](webrtc-protocol-verification.md)。

## 验证

```sh
go test -race ./...  # 设置 TEST_DATABASE_URL 后包含真实 PostgreSQL 集成检查
go vet ./...
python3 scripts/check_contract.py
python3 scripts/check_rtc_contract.py
go test ./internal/rtcsignal -run '^$' -fuzz FuzzCommandsCannotCrossBindingsOrReviveClosedLeg -fuzztime=5s -parallel=2
```

两端继续执行原有 `:app:assembleDebug :app:testDebugUnitTest`。
交叉契约脚本在相邻 Android 仓库存在时比较共用源码/测试；只有服务端 checkout 时显式报告该项跳过。
这些验证不替代 WebRTC native、蜂窝 PCM、TURN、UI 截图、后台及多节点真实通话验收。
