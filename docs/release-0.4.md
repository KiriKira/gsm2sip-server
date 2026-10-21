# 0.4.0 发布说明：升级与回退

0.4.0 是三端第一个正式版本号，同时彻底移除 SIP：

| 组件 | 新名称 | 包名 / 镜像（不变） | 0.4.0 变化 |
| --- | --- | --- | --- |
| 服务器 | SoulGem | `ghcr.io/kirikira/gsm2sip`、`gsm2sip-server`，二进制与数据库名不变 | 删除 Asterisk、ARI、SIP 凭据与 sip-config/call-intents 接口；通话只走 RTC 呼叫控制 + WebRTC |
| 主手机 | Madoka | `com.callagent.host` | 删除 PJSUA2 与 SIP 设置、WebRTC 开关；只用 WebRTC |
| 旧手机网关 | Homura | `com.callagent.gateway`、Magisk 模块 id `sip-gsm-gateway` | 删除 SIP/RTP/SRTP/STUN；界面改为与 Madoka 相同的样式 |

短信、配对、SIM 绑定、多节点、备份的接口和数据格式不变。

## 前提

- 两部手机都必须能到达 TURN（一体镜像自带 coturn，默认 `3478/udp+tcp` 与中继 `49160-49259/udp`）。分镜像部署按 [deploy-images.md](deploy-images.md) 配置 `RTC_TURN_URLS` 与 `RTC_TURN_SHARED_SECRET`，并在防火墙/安全组放行同样的端口。
- 原来为 SIP 开放的端口（5060/5061、RTP 10000-10199、ARI 8088）在 0.4 不再使用，可在确认不回退后关闭。

## 升级顺序

1. **备份**：`scripts/db_backup.py`（或 [operations.md](operations.md) 的备份步骤）并保存 `SECRETS_ENCRYPTION_KEY`。
2. **所有服务器节点**：把镜像标签换成 `v0.4.0`（或对应的 `sha-…`），重新启动。
   - 不需要数据库迁移，0.4 没有新增 schema。
   - 启动时把尚未结束的旧 SIP 通话（`control_transport='sip'`）一次性标为 `ended/sip_removed` 并释放网关占用。
   - `ARI_URL`、`SIP_SERVER_NAME`、`SIP_COMPATIBILITY_ENABLED` 等旧变量会被忽略并在日志里提示；`RTC_MEDIA_ENABLED`、`RTC_CONTROL_ENABLED` 也不再需要（0.4 始终开启）。**建议保留**这两项为 `true`、`SIP_COMPATIBILITY_ENABLED=false`（`deploy/env.example` 与 `deploy/compose.yaml` 已如此设置），以便回退时上一版仍走 RTC；0.4 只在 `SIP_COMPATIBILITY_ENABLED=true` 时提示忽略。
   - 分镜像部署删去 Asterisk 服务即可；`deploy/compose.yaml` 已不再包含它。
3. **Homura**：覆盖安装 0.4.0 APK 或 Magisk 模块（模块 id 不变，直接刷入覆盖）。打开“状态”页，确认“远程通话”显示通话控制已就绪；未启动语音的旧手机需在“诊断并启动”后再启用。
4. **Madoka**：覆盖安装 0.4.0 APK。设置里不再有 SIP 或 WebRTC 开关。

升级过程中三端版本不一致时，短信不受影响；通话以三端都到 0.4 为准。旧版 Madoka 若曾关闭 WebRTC 开关，会因服务器不再提供 SIP 配置而无法通话，升级 Madoka 即恢复。

## 回退

所有正式版共用固定 versionCode（Madoka 与 Homura 都是 `1000000000`），Android 允许直接覆盖安装相同 versionCode 的旧版本，**不需要卸载、不丢数据**。数据库与本地存储都保持兼容：0.4 没有删除任何表或列（旧 SIP 凭据行在设备撤销时照常清理）。

| 回退对象 | 方法 | 注意 |
| --- | --- | --- |
| 服务器 | 镜像标签换回上一版（WebRTC 合并后的 main 构建 `sha-…`），重启 | 上一版需 `RTC_MEDIA_ENABLED=true`、`RTC_CONTROL_ENABLED=true` 才能与 0.4 手机通话；保持 `SIP_COMPATIBILITY_ENABLED=false` 时不需要恢复 Asterisk。0.4 的 `deploy/compose.yaml` 已按这组默认值传入，只改 `GSM2SIP_VERSION` 即可；一体镜像回退时在 `docker run` 加上 `-e RTC_MEDIA_ENABLED=true -e RTC_CONTROL_ENABLED=true -e SIP_COMPATIBILITY_ENABLED=false`。0.4 期间结束的旧 SIP 通话保持 ended |
| Homura | 覆盖安装上一版 APK / Magisk ZIP | 0.4 服务器保留旧网关来电前查询的 `incoming-policy`（固定返回 RTC），单独回退网关也能接来电。只能回退到同样使用 versionCode `1000000000` 的构建（WebRTC 合并之后）；更早的按 `10000 + run` 编号的构建需卸载重装。上一版保留的 SIP 设置项为空，会按 RTC 运行 |
| Madoka | 覆盖安装上一版 APK | 上一版的 WebRTC 开关保留在升级前的状态；若曾关闭需重新打开，否则会尝试已不存在的 SIP |

回退单个组件即可，不需要三端同时回退：上一版服务器、Homura、Madoka 在 RTC 模式下与 0.4 互通（这是联合测试计划中的回退用例）。

## 已知限制

- 物理蜂窝 HAL 音频、厂商省电策略仍需真机验收；模拟器联测使用合成 PCM。
- 0.4 不提供 SIP 兼容，任何仍依赖 SIP 的第三方客户端都无法再通话。

图标将在正式发布前替换。
