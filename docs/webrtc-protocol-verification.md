# WebRTC 协议验收记录

> 2026-10-18：本分支已新增主机真实语音开关、两端 native WebRTC 媒体和经身份/通话占用校验的 HTTPS 信令适配器；SIP 保留呼叫控制。部署和端口以 [WebRTC 音频说明](webrtc-audio.md) 为准。完整双模拟器联合验收仍在进行，尚未发布可供安装的新 Release。下文是 10 月 16 日各批次的历史验收记录，不能作为本次三端通话的通过证据。

更新：2026-10-16，分支 `feat/webrtc-migration`。

**当前可在本环境执行的协议检查与软件回归已通过，可开始准备云手机进行 Android 原生验收。**
该结论不表示生产 WebRTC 接入或蜂窝通话迁移完成；三端仍默认 SIP，UI 和外围业务路径没有切换。
本批主机提交 `0bd5686`，网关提交 `c4b0e7f`；服务端实现与本记录同批提交。
可审阅的受控结果快照见 [webrtc-headless-result.json](webrtc-headless-result.json)。

| 检查层 | 实际结果 | 适用范围 |
|---|---|---|
| 真实 WebRTC 协议集成 | 13 项通过，0 失败/跳过，race 开启 | 直连 ICE/DTLS-SRTP、Opus RTP 双向完整性、STUN Binding、TURN UDP/TCP/TLS 强制中继、凭据/CA/主机名/指纹拒绝、WSS 交换 SDP 与原 call ICE restart |
| 协议运行器证据校验 | 3 项通过 | 缺少任一必测协议、失败、跳过、零测试或进程失败不能冒充 PASS |
| 服务端 go test -race ./... | 114 项通过，0 测试失败/跳过 | PostgreSQL、短信/身份/历史、ARI 模拟、wake-only WS、RTC 状态机与新 WSS handler；另有无测试文件的包 |
| 服务端 go vet ./... | 通过 | 主模块及独立协议 module |
| 原 OpenAPI/fixtures | 原 21 份通过 | 未修改现有接口、短信协议或能力声明 |
| RTC 跨语言契约 | 6 份命令 fixtures、15 类非法变体通过 | 两端 guard/codec/测试、三端 canonical fixtures 字节一致 |
| 严格 decoder fuzz | 5 秒预算，实际约 6 秒，512 次执行，通过 | 无配对 UTF-16 代理字符、非法 UTF-8 和规范化重编码不变性；不是完整协议一致性认证 |
| 主机 Debug + 单元测试 | 构建通过，186 项通过，0 失败/错误/跳过 | 含严格 codec、既有 SIP/界面模型和外围回归 |
| 网关 Debug + 单元测试 | 构建通过，175 项通过，0 失败/错误/跳过 | 含严格 codec、既有 SIP/RTP/SRTP/DTMF 和外围回归 |
| 网关 Magisk runtime harness | PASS | 软件协议回归，不表示真实 Magisk 设备已验收 |
| 独立 Android 媒体探针 | 本批工具 5 项复验通过；前批 APK/4 项 JVM 验证有效 | 本批未改探针源码，未运行 native 仪器测试 |

协议 harness 的 [固定版本、执行步骤与覆盖范围](../probes/protocols/README.md) 可重现检查。
使用 coturn `4.6.3@sha256:71c3c990283385567f11794ee692e3a47b66fd9b0bb39e42afbe776e331dd888`、
Pion WebRTC `v4.1.6`（提交 `f35dc4efd0ac74d45119b0d91c73a35c91bef0c4`），生产 API 不依赖它们。
结果在 `probes/protocols/artifacts/result.json`，测试事件在 `tests.jsonl`；受控结果可记录测试二进制与 fixture 哈希，
临时凭据/私钥/配置不保存到报告，结束后删除测试容器/网络，不改变原 PostgreSQL 容器。

## 本批实现边界

`internal/rtcsignal/WebSocketHandler` 已提供真实 WSS 传输实现，但**尚未注册到生产路由**：
必须显式注入 Service、会话验证函数以及事务内 Authorizer；目前仍缺生产呼叫协调器适配，不能绕过 shared occupancy/engine/winner 门槛。
它要求 HTTPS、无 Origin 的原生客户端，在认证/数据库操作前预留独立连接预算，每设备限制连接数；
每条命令和定时 pulse 复验会话，读取有界、队列有界、发送截止时间有界，过载关闭单条语音连接。
ACK 在事务成功后发送，广播只针对 leg 成员的已提交权威快照；不会打开音频、拨号或产生 winner。
生产 HTTPS 若在反向代理终止，需在接入时解决可信 TLS 边界；当前不会自动信任 X-Forwarded-Proto。

新增 WSS 检查实际覆盖：401/403/跨 Origin/不可信证书、连接配额、会话撤销、信令篡改、重复操作、取消、
写入失败不 ACK/不消耗操作、二进制/超大帧、慢消费者队列关闭，以及关闭语音 handler 后其他 HTTP 仍可用。
这证明模块隔离与协议行为；真实应用的短信/语音故障注入仍待后续运行时接入。

两端 `RtcCommandCodec` 不建立连接、不启动引擎、没有 UI/服务调用点，使用 Android 严格 JsonReader 读取同一份命令契约。
拒绝重复/未知字段、null、数组、尾随数据、宽松 JSON、错误数值类型、非法身份和 UTF-8 字节超限。
同时拒绝无配对 UTF-16 代理字符，避免 Go 自动替换而 Android 保留原值导致跨端 payload/hash 不一致；有效代理对保持原样。
编码器也校验内部调用者，不能把 SDP/ICE 附到错误的 kind。生产会话 bootstrap、服务端 snapshot/ACK 适配和 Android WSS 客户端仍需后续接入。

## 云手机的下一步

云手机用于先解除 W1 的 Android 原生阻塞：64 位（arm64-v8a/x86_64）、可安装 APK、可使用 ADB；优先 Android 16。
通过 ADB 读取实际版本/ABI/权限后，在网关根目录运行：

```sh
python3 probes/webrtc/tools/run_probe.py --serial DEVICE_SERIAL
```

仅安装独立探针，不替换两端应用，不清除配对、短信或设置，不拨打电话，也不要求 root。
成功要求真实 native PeerConnection/Opus 双向解码、合成音调和帧数达到门槛、默认录音/播放路径未使用、资源释放完成。
云手机可用于后续 native 网络/后台检查，但不能据此宣布真实蜂窝 PCM、双卡、Magisk 路由或 IVR DTMF 已通过。

仍未执行：Android native、真实蜂窝/双 SIM、实际公网/运营商网络、完整 UI 截图/通知/后台/权限矩阵、长通话凭据轮换和持续运行。
生产 Authorizer/共享占用/ARI 隔离、网关音频桥/全局裁决、Android 引擎及生产能力声明仍在迁移计划中。

本次日志：`/workspace/.onboarding/webrtc-protocols.log`、`webrtc-server-check3.json`、
`webrtc-host-check3.log`、`webrtc-gateway-check3.log`。报告区分本地协议 PASS 与设备 NOT_RUN，不以 Go 测试替代原生音频验收。
