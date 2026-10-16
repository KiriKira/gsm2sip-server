# WebRTC 迁移实施记录

更新：2026-10-16。三端专用分支均为 `feat/webrtc-migration`。总计划见 [plan-webrtc.md](plan-webrtc.md)。

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

## 下一阶段门槛

1. W1：锁定可重复构建的 Android WebRTC SDK/上游提交、ABI、许可证和自定义双向 PCM 接口；
   单独探针先证明蜂窝下行可注入编码器、解码音频可进入蜂窝上行，且不替换为默认麦克风/扬声器。
2. 在相同探针中验证 Opus、8/16 kHz 音源重采样、DTMF、前台采音授权和强制 TURN relay；
   真机两张 SIM 的双向数字音频为必要证据。未通过前保持原引擎默认值。
3. W2 后续：确定新信令 schema/版本、权限、generation、幂等/终态、网关全局 winner 裁决、
   独立的语音停止路径和混合版本协商。先给出可失败的模型/集成测试，再实现新传输。
4. W0 后续：补齐屏幕、通知、权限和所有协议专用设置的等价映射，继续保持 UI/外围行为的发布阻断条件。
