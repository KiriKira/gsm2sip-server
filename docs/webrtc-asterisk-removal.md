# 移除 Asterisk：独立控制实现与联合验收

> **0.4.0 起**：服务器改名 SoulGem，SIP、Asterisk 与 ARI 已移除，通话只走 RTC 呼叫控制与 WebRTC。本文有关 SIP 的内容为历史记录；当前行为见 [rtc-call-control-v1.md](rtc-call-control-v1.md) 与 [release-0.4.md](release-0.4.md)。

2026-10-19。目标仍是：新 WebRTC 路径不依赖 SIP、PJSUA2、Asterisk 或 ARI。原 SIP 仅可作为明确选择的兼容路径，不能成为 WebRTC 模式的运行前提。

已发布的 `debug-webrtc-joint-20261019` 只替换了音频：ICE/DTLS-SRTP/Opus 与测试 PCM 已经联合验证，但邀请、响铃、接听、拒接、挂断及业务通话状态仍通过 SIP/Asterisk。关掉 Asterisk 后该版本不能完成呼叫；本次媒体联测通过不代表原迁移目标完成。当前日志中的 `call_control=sip` 正是实际情况，不能只改显示文字冒充去除了依赖。

## 独立控制实现状态（2026-10-19）

2026-10-19：独立 Go/HTTPS 呼叫控制与原生 WebRTC 已完成两台持续运行的 KVM 模拟器、三个真实 Docker 节点联合验收，并发布沿用原签名的 Debug Release。纯模式三节点不运行 Asterisk，SIP/ARI 抓包为 0；呼入/呼出双向测试 PCM、呼叫事件、TURN UDP/TCP、控制/媒体故障与 A→B→C→A 切换通过。WebRTC 开关关闭时的 SIP 来电、开启时的混合来电事件另行通过。物理蜂窝 HAL 和真实手机音频仍待真机验收。 [报告、证据及原签名下载](webrtc-independent-validation-20261019.md)。独立控制、授权边界和部署契约见 [RTC 控制 v1](rtc-call-control-v1.md)。

## 旧过渡版本的依赖审计（历史）

| 位置 | 当前依赖 | 独立 WebRTC 路径必须实现 |
| --- | --- | --- |
| Host `CallEngineFactory` / `SipCallEngine` | WebRTC 音频仍由 `sip.start/register/makeCall/answer/hangup` 驱动，等 SIP confirmed 才连接 | 独立 RTC 呼叫引擎；邀请、接听、拒接、取消、挂断直接使用已认证的 Go 呼叫控制协议 |
| Host `HostCallService` / `CallRuntime` / `NodeMonitor` | SIP 配置、凭据、注册状态及 `sipAvailable/sipRegistered` 决定通话可用性 | 按实际选用引擎获取能力和就绪状态；RTC 模式不读取 SIP 凭据、不启动 PJSUA2、不以 SIP 注册为门槛 |
| Server `internal/calls` | CreateIntent 检查 ARI EndpointOnline；通话进展、桥接、接听者选定依赖 ARI 事件 | Go/PostgreSQL 直接维护呼叫状态、参与者、winner、revision、占用与授权；设备事件不能伪装成 ARI 事件 |
| Server `internal/httpapi/rtc_media.go` | 媒体授权依附于 SIP 协调器创建的 call_sessions、参与者及 gateway_call_slots | 使用同一业务授权边界接入新的独立呼叫协调器，复用持久化和隔离约束；不通过虚假 SIP 状态绕过校验 |
| Gateway `GatewayService` / `CallOrchestrator` / mesh voice clients | 蜂窝来电与出站执行由 SIP 消息及 SIP 音频适配器触发 | 已认证的控制流直接对接蜂窝执行；保留全局占用、SIM/revision 校验、持久化 dispatch ledger 和迟到事件隔离 |
| Server RTC SIP channel lease | 为避免旧 RTP 看门狗误拆正常 RTC 通话而续租 Asterisk channel | 新 RTC 路径使用自己的呼叫/参与者租约和故障回收；不创建或续租 Asterisk channel |
| Docker / 配置 | AIO 内含 Asterisk，API 仍配置 ARI/SIP | 提供 API + PostgreSQL + 按需 TURN 的部署，不要求 Asterisk 容器、ARI、SIP 端口或 SIP 密钥；旧 SIP 部署保持明确可选 |

## 已落地的软件控制链路要求

1. 冻结协议无关的呼叫能力、命令和事件契约：出站授权与邀请、网关准备/拒绝、响铃、接受/拒接/取消/挂断、真实蜂窝 ACTIVE/结束、revision、operation_id、到期及重连快照。复用现有身份和逐节点会话；不把 SDP 协商 reducer 当成完整呼叫状态机。
2. 在 Go/PostgreSQL 中实现该契约的持久协调器。共享 SIP/RTC 的网关占用与去重边界；原 SIP 只作为一个独立适配器。来电多客户端 winner 必须由服务端原子选定，其他候选不能获音频或继续接听。
3. Host 和 Gateway 接入该控制流，保留 Telecom、前台服务、通知操作、权限、物理蜂窝控制与 PCM 桥。媒体就绪不等于蜂窝已接通；收到重放命令不能重新拨号；ICE 失败不能启动 SIP 回落或影响短信服务。
4. 去除 RTC 模式下所有 SIP bootstrap、注册、URI、ARI endpoint/channel 的前置条件，并逐项替换 UI 就绪判断与节点探测。状态日志必须报告实际控制和音频路径。
5. 加入不运行 Asterisk 的部署与 CI/联合验收；完成后才将新路径标为 Asterisk 替代。未实现的能力不能标为 ready 或用假凭据维持旧判断。

## 软件联合验收条件与真机边界

使用现有持续运行的两台 KVM 模拟器和三个独立服务器节点，保留原配对/SIM/短信数据：

- 停止三个节点全部 Asterisk 进程；不配置 ARI 或 SIP 凭据，封闭 SIP 端口。从冷的应用进程开始，RTC 路径仍能初始化、呼入/呼出、接听/拒接/取消/挂断。
- 复跑双向测试 PCM 长通话、静音/DTMF、通知和 Telecom 状态、无接听前音频、TURN UDP/TCP、媒体与控制中断清理、节点备用切换和通话中锁定节点。
- 证明 RTC 模式没有 SIP 注册/INVITE/BYE 或 ARI 请求，Host 不实例化 PJSUA2；每个 call_id 最多一次蜂窝拨号，winner、SIM/身份隔离与撤销校验保持有效。
- 配对、短信、历史、备份、本地数据和原 UI 行为不能退化。可选 SIP 兼容路径另行验收，其存在不能成为 RTC 路径的依赖。
- 模拟器测试 PCM 仍只证明软件媒体闭环；物理蜂窝音频采集/注入和真实手机兼容性另行验证。

界面边界：WebRTC 语音开关只在设置页，不占拨号页联系人建议区域；通话页显示实际媒体/控制。新独立 Go/RTC Release 已满足上述软件闭环；原混合 Release 的历史限制仍成立，物理手机音频仍需真机验证。
