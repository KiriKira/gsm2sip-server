# 0.4.1 发布说明

项目统一名为 **mahogsm**（SoulGem 服务器 / Homura 旧手机 / Madoka 主手机）。包名、镜像名、数据库与 Magisk 模块 id 不变，
三端可直接覆盖升级与回退。服务器需与两部手机一起升级到 0.4.1：0.4.1 的手机配对时会声明角色，0.4.0 服务器会拒绝。

## 变化

- **一个码加入两部手机**：`mahogsm-admin join create --url https://手机访问的地址` 打印 `mahogsm://join` 链接和终端二维码，
  旧手机和主手机各扫一次，每种手机只能用一次，默认 15 分钟过期。服务器没有账户时自动创建；多节点用 `--also URL=CODE`
  并进同一个码。管理命令由 `gsm2sip-admin` 改名为 `mahogsm-admin`，`scripts/mesh-join-link.py` 已删除。
  按角色的配对码仍可用，且拒绝另一种手机使用（码不作废）。
- **新节点加入不再撞时序**：Homura 加入服务器时立即记下其扩展，第一次心跳就上报安装 ID；Madoka 添加节点时若旧手机尚未
  上报，最多等待 45 秒，并区分“旧手机还没加入”和“是另一台旧手机”。
- **Homura 语音自动恢复**：Magisk 模块已挂载（应用持有 `CAPTURE_AUDIO_OUTPUT`）、仍是默认电话应用且语音权限齐全时，
  重启、升级、进程重启后自动启动语音，不再需要点“诊断并启动”；用户停止网关后不再自动恢复。
- **FCM 唤醒主手机（可选）**：服务器设 `FCM_SERVICE_ACCOUNT_FILE` 后，Madoka（有 Google Play 服务、构建带 Firebase 配置）
  在该节点登记令牌；新短信或来电时服务器发高优先级 data 消息，只含 `sms`/`call` 与设备 ID，不含号码或正文。
  - 节点没有 FCM 或密钥错误时，`/v1/capabilities` 的 `push` 为 false，手机不登记、每节点每天最多问一次，轮询与 0.4.0 相同。
  - 只有当前节点确认登记后，空闲轮询才由 8 s / 3 s 放宽为 30 s / 10 s。
  - 推送到达但连不上任何节点时，12 秒后提示“有来电，但连不上服务器”，不额外重试。
  - 手机离开节点（撤销会话）时服务器删除其令牌。

## 升级（三端一起）

1. **服务器**：备份后把 `GSM2SIP_VERSION` 改为 `v0.4.1`，`docker compose pull && docker compose up -d`。新增迁移
   `0015_join_codes_push.sql` 只建新表。要用 FCM：按 [deploy-images.md](deploy-images.md#推送唤醒fcm可选) 挂载服务账号 JSON
   并设 `FCM_SERVICE_ACCOUNT_FILE`，启动日志出现 `push wake-ups enabled`。
2. **Homura**：覆盖安装 `gateway.apk`（Magisk 用户无需重刷模块，重刷只更新模块显示的版本）。
3. **Madoka**：覆盖安装 `gsm2sip-host.apk`。

## 回退

三端一起回退到 0.4.0，服务器先回。0.4.0 不读 `join_codes`、`push_tokens` 两张新表，保留无害；0.4.1 生成的加入码在回退后失效，
需改用 `gsm2sip-admin pairing-code create`。
