# 功能与弱网恢复状态

更新：2026-10-04。代码实现与真机验收分别记录；模拟器无法验收运营商或 Magisk 音频。

| 功能 | 实现与验收边界 |
|---|---|
| 双向短信、指定 SIM、任务幂等、消息与事件持久化 | 已实现；真实双卡和运营商回执待验收 |
| 短信系统库补扫 | 网关可选 READ_SMS、启用时间起点或显式全历史导入、分页 checkpoint 与恢复去重；无需 root |
| 主机持久化确认 | 消息、游标、通知待发在 SQLite 同一事务；之后提交服务器 receipt，服务器继续保留事件 |
| refresh 响应丢失 | 同原 token/请求键恢复当前代际的加密响应，窗口为该 refresh 自然到期时间；撤销和再次轮换使旧结果失效 |
| SIP/ARI/通话意图 | 主机内置 PJSUA2；服务端一次性授权、设备占位、Stasis 编排、媒体锚定、pending/ready/expiry、历史；完整三端音频待真机 |
| Telecom 和后台来电 | self-managed ConnectionService、CallStyle、实际通话 FGS、用户开启后台 SIP 注册与 HTTPS 查询；Doze/锁屏接听待真机 |
| 折叠屏 | 按窗口与 FoldingFeature 适配、避让铰链/打孔/系统栏/IME、保存 Activity 状态；Z Fold8 实机待验收 |
| 弱网音质 | 主机 Opus RTCP 反馈、码率/FEC/PLC/DTX 策略已落代码；服务器通过 translator probe 后可启用 SIP_ENABLE_OPUS，仅主机腿使用 Opus，网络效果待测 |
| FCM、ICE/TURN | 仍待实现；现有 IP-change/会话更新策略已实现，跨网络续话的互通与恢复效果仍待实测 |
| 正式签名、公网部署、双卡 DSDS、24h/72h、备份恢复 | 待部署/真机验收 |

| 场景 | 当前行为与限制 |
|---|---|
| 主机长期断网 | 服务端保留已提交事件，恢复后按 durable cursor 分页补齐；离线期间无法即时提醒 |
| 旧机断网仍收短信 | 广播收件与事件先写 SQLite WAL/FULL，重连上传同 event_id；Android 未交付的广播可在用户授权后从系统短信库补扫 |
| 服务端 ACK 丢失 | 重投同 event_id/hash，事务去重；不创建第二条消息 |
| WSS 提示丢失 | WSS 仅唤醒；HTTPS 轮询补齐，网络回归触发重试；Doze 和系统限制会延迟 |
| refresh 响应丢失或连续离线 | 两端先持久化原 token 与请求键，恢复后重试同请求；服务器不延长令牌自然有效期 |
| 消息页损坏或游标跳跃 | 主机整页校验后才提交；拒绝坏行、乱序或跳跃，不静默跳过正文 |
| 通知时进程退出 | pending 与消息同事务；Android 接受 notify 后确认，重放可能更新同 ID 通知；通知关闭时不能保证提醒 |
| 发短信中进程退出 | modem 前写 dispatching，无法证明未发送则保留 unknown；不自动补发造成重复计费 |
| 网络恢复时任务已过期 | 拒绝过期发送；“不漏收”不代表所有过期发送都应执行 |
| ARI 断线/服务重启 | 枚举并收敛通道；关联通道仍存在或无法确认消失时保持 unknown/busy；ARI 确认全部关联通道消失后终止记录并释放 gateway lease |
| 通话完全断网/换 IP | 主机网络回归触发 PJSIP IP-change/会话更新并查询权威状态；丢网宽限有上限，Asterisk RTP 超时 30 秒、hold 60 秒；ICE/TURN 和复杂 NAT 下不能保证续话 |
| 假连网/DNS/TLS 失败 | 保持证书/主机名验证并退避；不降为明文 SIP/RTP |

已进入网关 SQLite 或服务端 PostgreSQL 的短信，能在存储保持完好、权限恢复与网络最终恢复的条件下补齐。服务器无法找回运营商未交付、系统未保存且应用未收到的短信；磁盘损坏、删除应用数据或数据库也超出保证。通知投递和短信正文持久化是不同保证。

短信模式使用 Android 标准订阅/SmsManager/SMS_RECEIVED、可选 READ_SMS、HTTPS 与 SQLite，无需 su、Magisk、录音权限或默认电话角色。语音的账户 broker、数字音频与特权迁移仅在用户显式启用时运行。普通安装的短信受限权限仍取决于安装器和系统策略；授权失败必须报告不可用。
