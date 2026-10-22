# WebRTC 迁移实施记录

> **0.4.0 起**：服务器改名 SoulGem，SIP、Asterisk 与 ARI 已移除，通话只走 RTC 呼叫控制与 WebRTC。本文有关 SIP 的内容为历史记录；当前行为见 [rtc-call-control-v1.md](rtc-call-control-v1.md) 与 [release-0.4.md](release-0.4.md)。

> 2026-10-19：独立 Go/HTTPS 呼叫控制与原生 WebRTC 已完成两台持续运行的 KVM 模拟器、三个真实 Docker 节点联合验收，并发布沿用原签名的 Debug Release。纯模式三节点不运行 Asterisk，SIP/ARI 抓包为 0；呼入/呼出双向测试 PCM、呼叫事件、TURN UDP/TCP、控制/媒体故障与 A→B→C→A 切换通过。WebRTC 开关关闭时的 SIP 来电、开启时的混合来电事件另行通过。物理蜂窝 HAL 和真实手机音频仍待真机验收。 [本批独立控制报告](webrtc-independent-validation-20261019.md)。下文旧阶段按记录日期阅读。

更新：2026-10-16。三端专用分支均为 `feat/webrtc-migration`。总计划见 [plan-webrtc.md](plan-webrtc.md)。

最新结果：第三批已完成真实 headless WebRTC/STUN/TURN/WSS 协议检查、WSS 传输模块和两端严格命令 codec。
13 项实际协议检查、服务端 114 项、主机 186 项、网关 175 项回归通过，详情见
[协议验收记录](webrtc-protocol-verification.md)；可准备云手机开始 Android 原生 SDK/合成 PCM 验收。
第二批已完成隔离 SDK/PCM 探针、RTC 协商状态机、PostgreSQL 事务存储及两端 ICE 缓冲。
当前仍默认 SIP，没有生产 WebRTC 信令入口或媒体路径；真机验收由用户后续提供设备/环境后执行。
第二批详情与余项在本文后半部分；第一批的“未执行”描述保留为当时的历史记录。

## 第一批：保留 SIP 的通话边界改造

本批落地的是可替换的调用边界，运行时仍使用原 SIP/PJSUA2/Asterisk。没有启用 WebRTC 能力声明、
新信令端点、TURN 或新媒体，也没有改变既有 OpenAPI、短信协议、数据库和设备配对。
W0 和 W2 仍为部分完成，不能据此跳过 W1 或标记完整 WebRTC 已实现。

| 仓库 | 选用的上游基线 | 本批内容 |
|---|---|---|
| gsm2sip-server | b15b8df | HTTP handler 依赖 CallController，ARI Manager 作为现有实现注入；启动/事件循环仍由 cmd/api 管理，v1 intent DTO 保持不变 |
| gsm2sip-client-android | 1a71889 | CallEngine、协议无关的状态/拒接原因与 SIP 适配器；HostCallService 使用新接口，页面的引擎可用性检测经过同一工厂；实现提交 22ce826 |
| gsm2sip | 773eaea | 保留现有语音实现，合入上游熄屏/拔充电器保活更新，作为后台行为基线；专用分支合并提交 f83a423 |

主机兼容适配明确区分引擎操作 ID、传输 dialog ID 和经服务器核对的业务 call_id。
连接 READY 不产生通话 ACTIVE，不自动开启音频；接听/音频授权和 Telecom 路由沿用原有路径。
拒接和超时仍分别使用原 SIP 响应；native 初始化失败时补充清理部分创建的资源，并保留最初的异常。
服务层保留运行代次隔离、现有凭据启动流程及 SIP 重试策略；这些尚不是完整的协议无关 bootstrap。

服务端接口保持已认证的 owner/device scope、已有错误码、历史读取和 nil Manager 行为。
它只是 HTTP 消费侧边界，不是已抽离的 ARI 状态机，更不允许把新 WebRTC 事件直接伪装成 ARI 事件。

## 已执行的本批验证

| 检查 | 结果 |
|---|---|
| 服务端，提供 TEST_DATABASE_URL 后 go test -race ./... | 75 项通过，0 失败/跳过；含现有 HTTP、短信、鉴权、抢接、游标及 ARI 模拟集成测试 |
| 服务端 go vet ./... | 通过 |
| scripts/check_contract.py | OpenAPI 和 21 份 fixtures 通过；非法事件、平台、client 与 SIM 状态被拒绝 |
| 主机 :app:assembleDebug + :app:testDebugUnitTest | Debug 构建通过，171 项测试通过，0 失败/错误/跳过 |
| 网关 :app:assembleDebug + :app:testDebugUnitTest | 合入保活基线后的 Debug 构建通过，160 项测试通过，0 失败/错误/跳过 |
| 网关 tools/test-magisk-runtime.sh | PASS |

新增主机测试覆盖：连接就绪不触发通话/采音、三类呼叫标识不可互换、活动/终态顺序、迟到确认不能复活通话、
拒接与过期映射、启动失败释放资源、原错误详情保留。使用 fake SIP engine 验证兼容契约，不视为真实 native 音频验收。
新增服务端测试覆盖：替换控制器仍保留鉴权/角色校验和 v1 响应；语音不可用时网关状态同步和历史读取可用；
typed-nil 旧 Manager 不会导致历史端点解引用崩溃。

当前环境日志位于 `/workspace/.onboarding/webrtc-host-check.log`、`webrtc-server-check.json`、
`webrtc-gateway-baseline.log`，不是发布产物。真机通话、UI 截图比较、Android 仪器测试、
WebRTC SDK、PCM 与 TURN 探针均未在本批执行，不能由以上单元/集成测试替代。

## W0 初步冻结清单与未解决的设置映射

| 入口/存储 | 当前语义和迁移约束 |
|---|---|
| 主机 MainActivity / CallReadiness | 保留电话/短信/设置、拨号与来电/通话控件和状态；本批仅改变可用性检测的工厂入口，没有调整布局或文案 |
| 主机 CallPreferences | 保留 host-call-settings、foreground_sip_opt_in 与 session_instance 的已有存储键和 profile 归属；节点切换不能清除用户选择 |
| 主机 SipCredentialStore / SipConfiguration | 本批仍是真实 SIP 凭据与可用性，不伪造 REGISTER 或把连接 READY 当新协议 capability |
| 网关 SIP 服务器/端口/用户名/密码 | 保留现有配置、VoiceCredentialStore 和重新加载流程；纯 WebRTC 的等价操作尚未定义，不允许默认切换后静默无效 |
| 网关 codec：g722 / g711 / both | 现有可见编码选择不能在换成 Opus 后被忽略；需在 W1 确定兼容支持或可保持原语义的映射，否则阻断默认切换/移除旧引擎 |
| 网关 use_stun、sip_tls、srtp_enabled 及提示 | 保留原开关、服务器控制的只读状态与真实含义；不能把它们直接改名或伪装成 ICE/DTLS 设置 |
| 网关 agent_vol_step 与蜂窝音频 profile | 保留音量调节、采集/注入路由及物理 SIM 行为；在 PCM 探针中验证实际音频效果 |
| 网关 autoconnect / 后台运行 / 诊断确认 | 保留后台选择和显式开启语音的时序；短信模式不启动 root/录音；保留新增上游 StayAwake 生命周期 |
| 两端 mesh / 备份 / 短信库 | 不改节点优先级、会话、SIM revision、短信账本、游标、备份格式或签名；本批无数据库 migration |

该表是已检查入口的初步清单，不代表 W0 已完成。下一步须为所有可见设置建立逐项映射和可复现的
UI fixtures，并在固定设备/字体/分辨率上记录截图和操作基线；现有 SIP 测试记录不得替代本批未执行的实机检查。

## 第一批结束时的门槛

1. W1：锁定可重复构建的 Android WebRTC SDK/上游提交、ABI、许可证和自定义双向 PCM 接口；
   单独探针先证明蜂窝下行可注入编码器、解码音频可进入蜂窝上行，且不替换为默认麦克风/扬声器。
2. 在相同探针中验证 Opus、8/16 kHz 音源重采样、DTMF、前台采音授权和强制 TURN relay；
   真机两张 SIM 的双向数字音频为必要证据。未通过前保持原引擎默认值。
3. W2 后续：确定新信令 schema/版本、权限、generation、幂等/终态、网关全局 winner 裁决、
   独立的语音停止路径和混合版本协商。先给出可失败的模型/集成测试，再实现新传输。
4. W0 后续：补齐屏幕、通知、权限和所有协议专用设置的等价映射，继续保持 UI/外围行为的发布阻断条件。

## 第二批：隔离媒体探针、持久化协商与两端缓冲

用户明确测试网关为 **Android 16 + Magisk root**，并要求先尽可能实现代码，随后由用户提供测试/环境。
不再等待设备连接来推进独立软件模块；这不解除 W1 实机音频与默认切换的验收门槛。

| 仓库 | 本批内容与提交 |
|---|---|
| gsm2sip | `0ece58e`：独立 `probes/webrtc/` 构建、SDK pin/Java 外部播放适配、双 native PeerConnection 合成 PCM 探针、证据运行器；`2d9c38a`：RemoteIceBuffer 与 10 项测试 |
| gsm2sip-client-android | `65126fa`：同一 RemoteIceBuffer、10 项测试和当前集成边界记录；未添加 WebRTC SDK |
| gsm2sip-server | `internal/rtcsignal` reducer/service/PostgreSQL repository、增量 migration 0015、RTC schema/六种命令 fixtures、跨仓库一致性检查和实验契约说明（与本记录同批提交） |

### 媒体探针

探针为独立 APK `com.callagent.gateway.webrtcprobe`，不接入两端 UI/服务、无 Activity，无麦克风/电话/root 权限。
固定第三方 `io.github.webrtc-sdk:android:150.7871.01`、Maven AAR SHA-256、发布者提交、
发布者声明的 WebRTC 源码提交、Java 源码 SHA、LICENSE/PATENTS/完整第三方声明。
准备脚本通过 TLS 和校验和获取输入，坏缓存报错；sources.jar 为占位内容，使用固定上游源码替代输入。

SDK 原有外部采集能力用于注入 PCM；原播放回调仍会写默认 AudioTrack，所以用固定 Java 源码构建
探针专用外部播放分支，不创建/写默认 AudioTrack、不修改系统通话音量，保留原 JNI 方法和构造器。
保留的 arm64-v8a/x86_64 native 库与上游 AAR 字节一致，ELF PT_LOAD 满足 16 KiB 对齐。
这是固定预编译发布物与本地 Java 派生构建的来源证据，**不是从源码重建 native 库的证明**。

仪器测试会建立两组真实 native PeerConnection、仅 Opus 协商与 ICE/DTLS-SRTP，分别注入不同合成音，
要求双向解码得到对端音调、帧数达标、默认录音/播放路径未使用，释放完成后才 PASS。
运行器从当前源码重建，检查 instrumentation code 和结构化证据，不能仅凭 adb 退出 0 算通过。
本地实际运行结果为 **BLOCKED / no_selected_android_device**，不是通过或失败的 native 音频验收。

本次探针 APK SHA-256：`983479ca5df351bfaa034af5661aaa2b8fb8b398aa746f5309bb4145e94c3c69`。
APK 位于网关 `probes/webrtc/build/outputs/apk/debug/Gsm2SipWebRtcProbe-debug.apk`，
构建 provenance 与运行结果在同目录构建树内，均为被忽略的本地产物。
设备到位后从网关根目录执行：`python3 probes/webrtc/tools/run_probe.py --serial DEVICE_SERIAL`。
该探针只验合成 PCM，不拨号；PASS 也不能替代蜂窝 PCM 或 TURN 验收。

### 信令与持久化

[RTC v1 实验契约](rtc-signaling-v1.md) 固定该子模块的身份、状态、generation、revision、幂等和资源上限。
主机固定 offer、网关 answer；negotiated 不等于蜂窝 ACTIVE，不执行拨号/接听或产生 winner。
跨 owner/device/call/leg/attempt 消息被拒绝；相同操作键换 payload 冲突，历史 ACK 不复活终态。
取消在与应答、ICE restart 的任意处理顺序下都终结该 leg，不被过期 revision 或旧 generation 吞掉。
候选早到有界保存，restart 清掉旧协商；关闭、过期和撤销后不能恢复。
重连通过保留的候选计数识别 TTL 清理造成的重放缺口，不把仍有效的 EOC 当完整集合。

`0015_rtc_signal_legs.sql` 只新增独立表/索引。行锁序列化跨进程更新，强制同步提交，
Service 只在状态/摘要/信令事务提交成功后返回 ACK；有界 Sweep 清理过期 SDP/ICE 并写入终态。
TTL 清理不丢失去重摘要。协调器可以受限续期，不能复活过期授权；参与者没有自行续期命令。
每次访问（含重复请求）必须通过事务内 Authorizer；没有默认放行实现。
当前只有测试用 authorizer，**生产呼叫授权适配器尚未实现**。

模块没有注册 HTTP/WSS 路由，没有 capability 发布或 cmd/api 启动调用，也不创建生产 RTC 记录。
它不创建独立业务通话/占用，不修改旧 call_sessions、gateway_call_slots 或 ARI 状态。
启用前必须完成共享引擎/占用事务、ARI 隔离、网关全局 winner/fencing 和 TTL worker 启动，禁止绕过这些边界。

### 两端组件及 UI/外围影响

同一 `RemoteIceBuffer` 在两仓库保持源码和测试字节一致，绑定节点、业务身份、代次和远端设备，
等对应 setRemoteDescription 成功再释放候选；实例/代次 token 隔离迟到回调。
重放不重复执行；冲突、溢出、过期缓冲、EOC 后候选及时钟回退会关闭缓冲；关闭后不能重开。
组件尚未接入服务/页面，无 Android/native 依赖，两端生产 Debug APK 中均无 WebRTC native 库。
未来适配器仍须完成单一 native 执行器、到期时间与时钟偏差处理、认证重连和音频授权。

本批未改变布局、文案、通知、权限、Manifest、用户设置、短信、配对、SIM、mesh、备份或后台服务生命周期。
网关探针有自己的独立 Manifest。服务端新增存储 migration，但没有修改旧表行为。
这说明本批改动边界与软件回归结果，不能据此宣称真实 UI/后台验收已完成。

### 第二批验证记录

| 检查 | 实际结果 |
|---|---|
| 服务端 TEST_DATABASE_URL + go test -race ./... | 99 项测试通过，0 测试失败/跳过；含 PostgreSQL 并发去重、重启加载、真实写入失败回滚、撤销/owner 隔离、TTL 清理、取消竞态与重放缺口；另有 6 个无测试文件的包 |
| go vet ./... | 通过 |
| 原 scripts/check_contract.py | OpenAPI 与原 21 份 fixtures 通过，原非法输入检查通过 |
| 新 scripts/check_rtc_contract.py | 6 份 RTC fixtures、15 类非法输入及两端源码/测试一致性通过 |
| RTC fuzz | 最终代码 5 秒通过，65,769 次执行；覆盖身份隔离和终态不可复活 |
| 主机 :app:assembleDebug + :app:testDebugUnitTest | 构建通过；181 项通过，0 失败/错误/跳过 |
| 网关 :app:assembleDebug + :app:testDebugUnitTest | 构建通过；170 项通过，0 失败/错误/跳过 |
| tools/test-magisk-runtime.sh | PASS；不代表 Magisk 真机验收 |
| 独立探针 assembleDebug + testDebugUnitTest | 构建通过；4 项 JVM 测试通过 |
| 探针 Python 工具测试 | 5 项通过；错误 instrumentation 结果、单向/无音频证据和坏缓存无法冒充成功 |
| 探针产物 | zipalign -P 16 / 签名验证通过；两 ABI native 字节一致，许可证资产完整 |
| migration runner 兼容性 | 升级库可运行原 0014 及以前文件集；不是完整二进制/活动呼叫回退验收 |
| 原生仪器测试 | BLOCKED：无 ADB 设备，未执行 |
| 蜂窝 PCM / 双 SIM / 8–16 kHz 重采样 / DTMF / TURN / UI 截图 / 持续运行 | 未执行 |

构建/测试日志在 `/workspace/.onboarding/webrtc-probe-build.log`、`webrtc-server-check2.json`、
`webrtc-host-check2.log`、`webrtc-gateway-check2.log`。构建产物和日志未提交 Git。

## 当前后续实施顺序

1. 保持 SIP 默认，完成服务端共享 engine/occupancy、能力协商、ARI 恢复隔离及生产 Authorizer；
   将已验证的 WSS handler 接入独立生产路由并启动 TTL worker；保留其独立连接/队列/速率预算，不改 wake-only `/v1/ws`。
2. 网关实现协议无关的 CellularAudioBridge、语音独立停止、共享执行账本/全局 winner 与 fencing；
   不让媒体故障触发短信/控制同步服务停止，不另建绕过 SIP 占用的执行入口。
3. 两端增加仅开发配置可启用的 WebRTC 引擎和完整认证/重连适配；保持现有 UI 状态与音频焦点。
   加入真实采样率转换、DTMF 和受授权的媒体开放，不将本合成探针直接接进生产蜂窝路径。
4. 用户准备设备后执行当前 native 探针，再扩展蜂窝 PCM、两 SIM、TURN、后台和多节点端到端验收。
   W0 设置映射、UI 基线、SDK 可复现原生构建和灰度/回退证据仍是默认切换门槛。
