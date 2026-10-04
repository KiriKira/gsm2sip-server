# 三端联合实施顺序

更新：2026-10-04。这里列的是开发和验收顺序，不代表任何阶段已完成。三份仓库计划和 [protocol-v1.md](protocol-v1.md) 配合使用。

| 阶段 | 网关 gsm2sip | server | 主机 client-android | 完成门槛 |
|---|---|---|---|---|
| M0 风险验证 | 指定旧机两卡音频捕获/注入、呼出选卡、来电识卡、DTMF；建立可复现基线 | Asterisk TLS/SRTP 探针和内网开发环境 | 成熟 SDK TLS/SRTP + Telecom + 锁屏呼入探针 | 双向数字音频及基础选卡在目标旧机通过；SDK可构建且证书拒绝通过；无法通过时先报告适配问题 |
| M1 契约和基础 | 修TLS字节分帧、生产安全开关、SimRegistry/ledger | OpenAPI/JSON Schema、配对、owner/device/SIM表、事件和命令事务 | 工程骨架、配对/凭据、远端两卡状态页、mock fixtures | 三端锁定同一协议版本；错误SIM不会变默认SIM |
| M2 双向短信 | HTTPS journal/upload/claim、指定卡SmsManager、分片回执 | 消息API、幂等队列/事件、wake-only WSS及权限和限流 | 收件/会话/回复选卡、OTP、草稿和状态 | 异网两卡中文/emoji/多分片收发；崩溃后unknown不补发 |
| M3 前台完整通话 | 可信SIM/call_id SIP元数据、通话设备锁、终止竞态 | intent授权、ARI/Stasis生命周期、SRTP锚定和history | App内拨号/接听/挂断/DTMF/蓝牙/Telecom | 不借助外部SIP app完成两卡接打电话，错误/重放intent不拨号 |
| M4 后台和恢复 | 心跳、网络重连、本地接听/挂机与server收敛 | push pending/ready/expiry/cancel、断线恢复、状态校正 | 锁屏/Doze推送唤醒、通知/full-screen资格降级、切网恢复 | 主机睡眠真实来电通过；迟到push不响；不能唤醒的状态有明确诊断 |
| M5 持续运行与发布 | 原生工具可复现、权限改造、持久签名、Magisk兼容性 | Compose部署/证书续期/备份恢复/监控/升级回退 | 凭据保护、稳定签名升级、折叠屏和通知体验 | 24h熄屏+72h压力/掉线运行；三端重启、证书轮换、DB恢复与upgrade验证 |

可以并行：M0 三端探针；M1 schema 与客户端 mock/网关纯逻辑；M2 server/API/UI/网关执行器；M3 SIP routing 与Telecom UI。必须顺序执行：先确认旧机音频可行性，再承诺完整通话；先冻结契约再分别接入；先前台通话状态机再处理后台唤醒；先构建并验收再发布。

当前 M1/M2 短信核心及系统库补扫、刷新恢复、主机 durable receipt 已落代码。M3 的内置 SDK/Telecom、SIP 配置和 ARI 编排，以及 M4 的 pending/ready/expiry 与后台 SIP 服务已实现；实现不表示已完成两卡音频、锁屏或弱网实机验收。FCM、ICE/TURN 仍待实现；现有跨网媒体恢复策略、M5 持续运行和生产发布仍待验收。折叠屏按实际窗口和铰链适配，Z Fold8 需最终实机验证。

本地软件测试、官方 Android Emulator 与 Asterisk 探针用于初步验证；没有执行真实短信、计费拨号或公网生产部署。结果以验证产物及三份 PR 的对应提交记录为准。
