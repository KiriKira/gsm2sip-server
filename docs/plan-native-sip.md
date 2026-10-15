# 计划：用 Go 自研 SIP/媒体核心替换 Asterisk

状态：计划，未开始。前置条件：当前 Asterisk 版本在真机上完成双向通话验证（去电、来电各一次，
两端都能听到），以便先把网络和媒体路径的问题与“换引擎”分开。

## 为什么

两端（旧手机网关、主机 App）都是我们自己的实现，只用到 SIP 的一个小子集。Asterisk 带来的
成本已经多次出现在真机上：

- C 工具链问题：pjproject 调试断言导致 `pj_thread_this` abort；`BUILD_NATIVE`
  导致在非构建机 CPU 上 SIGILL（#14、#15）。
- 一体镜像约 260 MB，其中 Asterisk + pjproject 约占 190 MB。
- 呼叫状态分散在 dialplan、ARI 事件和 Go 状态机三处，排查要跨三层。

目标是一个 Go 进程同时承担 HTTP API、SIP 信令和 RTP 中继，呼叫状态只在一处。

## 非目标

- 不做通用 PBX：不支持第三方话机、SIP trunk、语音信箱、会议、转接。
- 不做转码：两端编码由我们控制，协商不出共同编码就拒绝（488）。
- 不改变现有 HTTP/WSS 协议、SIM 路由元数据（`X-GSM-*`）和 mesh 设计。

## Asterisk 目前承担的职责

| 职责 | 现在的实现 | 替换方式 |
|---|---|---|
| UDP/TLS 传输 | `pjsip-transport-{udp,tls}.conf` | Go 监听 `SIP_PORT`（`sipgo` 或自写最小栈） |
| 注册与摘要认证 | `ps_auths`/`ps_aors`/`ps_endpoints` 实时表，realm `gsm2sip` | 直接读 `device_sip_credentials`，复用已存的 MD5 摘要 |
| 端点识别 | `auth_username,username`（来电 From 是主叫号码） | 只按认证用户名识别，不再有 realm 不一致问题 |
| 呼叫入口 | dialplan `gsm-client`（`call.<token>`）、`gsm-gateway`（`X-GSM-*`） | B2BUA 收到 INVITE 后直接调用现有 calls 状态机 |
| 呼出腿 | ARI 建 Local 通道 + `Dial(PJSIP/...)` + 头部子程序 | B2BUA 直接向目标设备发 INVITE，带可信头 |
| 桥接 | ARI mixing bridge | 两条腿的 RTP 端点直接配对转发 |
| RTP 中继 | `res_rtp_asterisk`，`strictrtp`、`rtp_symmetric` | `pion/rtp` 端口池 + 对称锁定 + 源地址校验 |
| SRTP（TLS 模式） | `res_srtp`，SDES | `pion/srtp`，每条腿独立密钥，中继处解密再加密 |
| 媒体超时 | `rtp_timeout`/`rtp_timeout_hold` | 中继统计最后收包时间，超时挂断 |
| DTMF | RFC 4733 | 按两侧协商的 payload type 映射后透传 |
| 多主机振铃、先接者胜 | ARI + `incoming_participants.go` | 状态机不变，B2BUA 对其他候选腿发 CANCEL |

Go 侧目前只用到 ARI 的一个小子集：`Events`（`StasisStart`，参数为
`client-intent`/`gateway-inbound`/`client-leg`/`gateway-leg`；以及
`ChannelStateChange`、`ChannelDestroyed`）、`CreateChannel`、`Dial`、
`Create/Get/DestroyBridge`、`AddChannel`、`Answer`、`Progress`、`Hangup`、`ListChannels`、
`EndpointOnline`、`GetChannelVariable`。这就是替换的接缝。

## 设计

### 1. 先抽象：`calls.Switch`

在 `internal/calls` 定义接口，覆盖上面的调用面和事件（腿开始、状态变化、销毁），
事件语义保持与现在的 ARI 事件一致。第一个实现是 Asterisk 适配器，包装
`internal/asterisk.Client`，行为不变。状态机、存储和测试只依赖接口。

### 2. 新包 `internal/sipcore`

- **传输**：UDP 和 TLS（证书与现在的 `/data/tls` 相同），`rport` 和接收地址用于 NAT；
  每个已注册设备记录最后一次的源地址和端口，向它发请求时直接回到这个地址。
- **事务与对话**：INVITE/ACK/BYE/CANCEL/OPTIONS/re-INVITE、RFC 3261 计时器 A–K、
  401/407 质询、stale nonce、491。优先评估 `github.com/emiago/sipgo`；如果它的对话层
  不够用，只用它的解析器，事务层自己写。
- **注册**：从数据库读凭据，在内存维护绑定（过期时间、源地址），`EndpointOnline` 直接查这里。
- **B2BUA**：每通电话两条腿，每条腿有独立的 Call-ID、tag、CSeq 和 SDP。来自网关的
  INVITE 只有在认证用户名属于网关设备时才信任 `X-GSM-*`，与现在 dialplan 的规则一致。

### 3. 新包 `internal/media`

- 端口池 `RTP_START`–`RTP_END`，每条腿一对 RTP/RTCP 端口。
- 对称锁定：只接受来自对端第一个有效包的源地址，之后严格校验，与 `strictrtp` 一致。
- SDP 中的地址：用设备连到服务器时用的本机地址（与现在 API 下发 SIP 主机的方式一致）；
  设置了 `PUBLIC_IP` 时用它。
- 编码：优先使用双方共同支持的编码（Opus、PCMA、PCMU、G722）；两条腿必须协商到同一种编码，
  否则 488。payload type 号不同时只改 RTP 头的 PT 字段。
- SRTP：TLS 模式下每条腿用 SDES 协商自己的密钥；中继解密后用另一条腿的密钥重新加密。
- 统计：收发包数、丢包、最后收包时间；提供 `/v1/...` 诊断接口，替代
  `pjsip show channelstats`。

### 4. 切换方式

- 环境变量 `SIP_ENGINE=asterisk|native`，默认 `asterisk`，直到 native 在真机上验证通过。
- 一体镜像在 `native` 下不启动 Asterisk；健康检查只看 API。
- 两种引擎写同一套数据库表，可以随时切回。

## 分阶段 PR

1. **接口重构**：引入 `calls.Switch` 和 Asterisk 适配器，没有行为变化，现有测试全部通过。
2. **注册**：`sipcore` 的传输、REGISTER、摘要认证、OPTIONS、在线状态；与 Asterisk 并行
   监听另一端口供测试。
3. **信令**：B2BUA 实现去电（`call.<token>`）、来电（`X-GSM-*`）、多主机抢接、CANCEL、
   超时、挂断原因；`SIP_ENGINE=native` 第一次可以接通（先不带媒体）。
4. **媒体**：RTP 中继、对称锁定、超时、DTMF、编码协商；然后 SRTP。
5. **切换与验证**：CI 中两种引擎都跑端到端测试；真机验收（见下）通过后默认切到 `native`。
6. **移除 Asterisk**：保留一个版本的回退能力后删除 Asterisk 镜像、模板和 ARI 代码；
   一体镜像目标小于 80 MB。

## 测试

- **协议单元测试**：重传与去重、CANCEL 和 200 OK 交叉、401 重试、stale nonce、对已认证
  INVITE 再次质询即拒绝、re-INVITE、BYE 与迟到的 ACK。
- **Go 集成测试**：在测试中用 SIP 客户端模拟网关和主机两端，覆盖注册、去电、来电、多主机
  抢接，并在 RTP 两个方向发包，断言转发、锁定和超时。
- **互操作测试**：CI 构建 pjsua（与主机 App 用的 pjsua2 同源）对接 native 引擎，覆盖 UDP 和
  TLS+SRTP。
- **现有测试**：模拟器 UI 冒烟和 PostgreSQL 集成测试对两种引擎都运行。

## 真机验收

- 去电、来电各 10 次，双向有声，接通延迟不劣于 Asterisk。
- 两台主机同时在线时来电，只有先接的一台接通，另一台停止振铃。
- 主机在局域网和 Tailscale 之间切换时通话不断，或在超时内干净结束。
- 连续运行 24 小时无崩溃、无内存增长；容器重启后设备在一个注册周期内恢复。

## 风险

| 风险 | 对策 |
|---|---|
| SIP 边界情况遗漏 | 先只支持我们自己两端的行为；未知请求一律 501/405；协议测试覆盖每个已知时序 |
| 与 pjsua2 的互操作问题 | CI 里跑真实 pjsua 对接 |
| 中继性能 | 单通话只有两路 20 ms 包，Go 足够；用基准测试确认 |
| 安全 | 保持现有规则：设备只能用自己的凭据；可信头只来自已认证的网关；限制未认证请求速率 |

## 以后：WebRTC

两端都是自己的 App，可以进一步用 WebRTC（服务端 `pion` 做信令和 TURN/中继）替代 SIP，
获得回声消除、抖动缓冲、ICE/TURN（包括走 TCP/TLS，可以经过 cloudflared）和 DTLS-SRTP。
代价是两个 App 的通话层要重写。等 native SIP 稳定后再评估。
