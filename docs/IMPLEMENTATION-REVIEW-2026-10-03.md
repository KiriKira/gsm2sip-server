# 三仓实施审查与首批边界

审查基线：gateway `d5fb847`，server `f39b1b5`，client `23fb222`。
用户目标为旧 root Android 留两张实体 SIM，未 root Android 主机经 VPS
收发短信、接打蜂窝电话。本次按用户要求使用 GPT-6 Luna（max）分工调研、
编码和复审，主代理统一接口、检查代码并运行构建/集成测试。

## 计划审视

三份计划的职责和安全边界一致，server `docs/protocol-v1.md` 为权威。
HTTPS journal/command 取代生产 SIP MESSAGE 发送通道是必要条件：SIP 202
不能作为数据库持久化确认。`sim_id` 是服务器身份，subId/slot 是旧机观测；
线路失效必须停止，不能采用系统默认卡。设备只允许一通桥接电话。

实施前需要补足计划没有给出的响应结构、SIM UUID 分配/本地确认次序、
不可变 command 摘要、实际短信分片数的上报和发出前失败的事件。
这些具体结构统一写在本仓库 `docs/server-wire-addendum.md` 和 OpenAPI；
另两端复用相同样例，不能自行新增不同字段。

## 已核实的 M0 门槛

- 首批代码将语音选卡限定 API 31+；用户后续明确要求先按 Magisk 通用
  接口实现，此限制由下一批网关适配移除。API 30 已有公开
  `getSubscriptionId(PhoneAccountHandle)`，API 31 提供公开正向关联；API
  26–29 通过 Magisk 启动受限 system-UID `app_process`，尝试系统的精确
  account→subId 接口，只有当前有效、唯一、双向一致的账户才能使用。
  不按机型筛选、不解析 opaque handle.id、不回落默认卡；不支持的实际
  接口明确报告 unavailable，而不是按版本预先拒绝。资料见下方补充。
- `SmsManager.createForSubscriptionId` 是 API 31+ 实例方法；旧系统采用
  `getSmsManagerForSubscriptionId`。任何异常不得回落默认实例。
- `VOICE_CALL` 需要特权 `CAPTURE_AUDIO_OUTPUT`；root/特权安装不能证明
  OEM HAL 有蜂窝音频 capture 和 uplink injection。目标手机未连接，两张
  卡的双向音频、DTMF、DSDS、24h/72h 验收均未完成。
- PJSIP 正式 release 2.17 尚未包含已公告的 DNS、SAN NUL、CN/SAN 修复。
  官方固定提交 `a67b8e81b0024b993f47e463c01c67c25cda116f` 已包含相关
  上游修复，适合作为待审查/构建的候选；并不等于目标手机探针通过。
  主机首批不包含未验收的 SIP 引擎，呼叫入口必须清楚报告不可用。

## 首批实现目标与尚欠验收

本批集中在 M1 和 M2 短信基础：PostgreSQL 持久化配对/权限/消息/命令/事件，
旧机 SQLite 执行账本和指定卡短信，主机配对/两卡状态/收件/草稿/重试。
WSS/FCM/ARI、完整 SIP/Telecom 通话和生产部署仍须按联合 roadmap 实施。
未配置通话时 API 返回 `CALLING_NOT_READY`，不能产生计费呼叫。

执行器提交 modem 前持久化 dispatching；重启后不确定结果为 unknown，
保留原 ID 并接受真实迟到回执。客户端超时重试使用同 key、同 payload，
不通过新建任务“修复”未知结果。错误 revision、未确认卡和不可用订阅
不应产生 modem 调用；两端都需要相应测试及真实 SIM 验收。

当前没有真实号码、VPS 域名或目标设备，开发测试不拨号、不发送真实 SMS。
测试结果和未实现范围以各仓库 README/实施状态及 PR 验证记录为准；构建
成功和数据库模拟链路不代表双卡实机功能已经完成。

## 核查来源

- [TelephonyManager](https://developer.android.com/reference/android/telephony/TelephonyManager)
- [SmsManager](https://developer.android.com/reference/android/telephony/SmsManager)
- [VOICE_CALL](https://developer.android.com/reference/android/media/MediaRecorder.AudioSource)
- [DNS advisory](https://github.com/pjsip/pjproject/security/advisories/GHSA-pvmg-ph43-54r2)
- [SAN NUL advisory](https://github.com/pjsip/pjproject/security/advisories/GHSA-382p-87mh-r3q8)
- [CN/SAN advisory](https://github.com/pjsip/pjproject/security/advisories/GHSA-wm98-82v7-vjw2)
- [PJSIP 官方源](https://github.com/pjsip/pjproject/tree/a67b8e81b0024b993f47e463c01c67c25cda116f)

## 本批服务端验证

Go 1.27.1 下 `go test -race -count=1 ./...` 与 `go vet ./...` 通过，
8 个顶层测试无失败、无跳过，包含 5 个实际 PostgreSQL 流程/并发回归。
覆盖首次配对到双 SIM 确认和命令 claim、令牌轮换/撤销、owner 隔离、
事件事务回滚、失效 SIM 拒绝新任务、分片迟到证据以及提交顺序游标。
测试使用独立随机 schema，未发送实际短信。

OpenAPI 与 11 个 fixture 文件通过校验，8 个不安全事件负例均拒绝。
容器构建、Compose 配置和非 root 容器 smoke 通过：migrations、health/ready、
owner/配对 CLI、认证 HTTP 与 `CALLING_NOT_READY` 行为均已核验。
容器测试结束后删除其测试 schema 和容器；没有部署公网服务。

Android 构建、SQLite/SIP/RTP 测试和 lint 的最终结果记录在相应 PR。
原生 SQLite 的事务主写连接验证为 WAL + synchronous=FULL；此配置测试
不代替目标手机突然断电、射频回执和实际双卡验收。

## Magisk 通用适配补充

网关提供固定模块入口 `gsm2sipctl` 的只读能力与账户查询，以及持久的
root 本地音频配置。默认数字 Telephony Rx/Tx 按系统暴露的设备与实际
路由探测，不根据 `Build.MODEL/HARDWARE/BOARD` 自动选择 mixer preset。
Magisk priv-app 权限与 system-UID broker 分开处理；root UID 0 不能冒充
`android` 包的 UID 1000，避免 Binder 包名归因不一致。

服务端与主机继续保持 `CALLING_NOT_READY`，直到 SIP/ARI 通话链路完成。
通用适配允许继续开发；实际接口探测失败不会变成默认卡拨号或扬声器/
麦克风兜底成功。Magisk 本身没有统一蜂窝 PCM API。

- [Magisk su CLI](https://topjohnwu.github.io/Magisk/tools.html#su)
- [Magisk module lifecycle](https://topjohnwu.github.io/Magisk/guides.html#boot-scripts)
- [Android O 精确 account/subId 接口](https://android.googlesource.com/platform/frameworks/base/+/refs/tags/android-8.0.0_r1/telephony/java/android/telephony/TelephonyManager.java)
- [Android R 公开 reverse 接口](https://android.googlesource.com/platform/frameworks/base/+/refs/tags/android-11.0.0_r1/telephony/java/android/telephony/TelephonyManager.java)
