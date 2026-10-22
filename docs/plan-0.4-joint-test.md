# 0.4.0 三端联合测试计划（交 Codex 在 KVM 虚拟机上执行）

目标：在去掉 SIP 后，证明 SoulGem 0.4 + Homura 0.4 + Madoka 0.4 的短信、通话、多节点、界面和**升级/回退**都正常。沿用 2026-10-19 三次联合验收的环境与方法（[独立 Go/RTC](webrtc-independent-validation-20261019.md)、[熄屏与备用切换](webrtc-screenoff-validation-20261019.md)）；凡是那里已有的步骤，按原方式重做即可，本文只列 0.4 需要覆盖的内容和判定标准。

## 被测版本

| 组件 | 0.4（被测） | 上一版（升级起点与回退目标） |
| --- | --- | --- |
| SoulGem | `KiriKira/gsm2sip-server` 分支 `claude/charming-franklin-8piri8`（`3453784` 或更新） | `main` 的 `bf41e9e`（WebRTC 合并） |
| Homura | `KiriKira/gsm2sip` 分支 `claude/charming-franklin-8piri8`（`74a5d5f` 或更新） | `main` 的 `dee22f9` |
| Madoka | `KiriKira/gsm2sip-client-android` 分支 `claude/charming-franklin-8piri8`（`a1c03ab` 或更新） | 分支 `ci/webrtc-sip-debug-20261017` 的 `e5cf86f`（即 `0.3.0-debug-build-40`：`main` 的 `fee2e2d` 加唤醒修复） |

开测时取分支最新 head，并记录每个组件的完整提交 SHA、镜像 digest、APK SHA-256 与签名证书指纹。

**签名与 versionCode**：覆盖安装和回退用例要求两个版本签名相同、versionCode 相同。联测 instrumentation 也要求测试 APK 与被测 App 同一签名。因此两个版本都在同一台机器上用同一个 debug keystore 本地构建，并设置 `GSM_RELEASE_VERSION_CODE=1000000000`（与正式版一致），不要混用 CI 下载的正式签名 APK。

## 环境

- 一台带 `/dev/kvm` 的 Linux 主机（上次为 EC2），长时间运行，不要每个用例冷启动。
- Gateway 模拟器：API 36，`google_apis` x86_64，虚拟 modem 提供 SIM；Host 模拟器：API 29。两台都以普通进程运行被测 App，不挂调试器。
- 三个服务器节点 A/B/C，各用独立 bridge/veth 网络；节点 A 用一体镜像（含 PostgreSQL 与 coturn），B/C 至少一个用 `deploy/compose.yaml` 分镜像 + `deploy/turn/compose.yaml`。所有节点 **不运行 Asterisk**。
- 抓包：每个节点对 SIP 端口（5060/5061）与 ARI（8088）持续抓包，0.4 全程计数必须为 0。

### 构建

每个仓库分别检出“上一版”和“0.4”两个提交各构建一次（建议用 `git worktree`），产物按版本分目录保存：

```bash
# 服务器（每个提交各一次，标签区分版本）
docker build -t soulgem-aio:<ver> -f deploy/allinone/Dockerfile .
docker build -t soulgem-api:<ver> .
# 网关与主机（各自仓库根目录；两个版本用同一个 ~/.android/debug.keystore）
export GSM_RELEASE_VERSION_CODE=1000000000
GSM_RELEASE_VERSION_NAME=<ver>-joint ./gradlew :app:assembleDebug :app:assembleDebugAndroidTest -PrtcLabRunner=com.callagent.lab.GatewayJointInstrumentation   # Homura
GSM_RELEASE_VERSION_NAME=<ver>-joint ./gradlew :app:assembleDebug :app:assembleDebugAndroidTest -PrtcLabRunner=com.callagent.lab.HostJointInstrumentation      # Madoka
```

Homura 的 Magisk 模块用 `SKIP_INSTALL=1 bash build.sh debug` 生成，同样两个版本各一份。

### 联测 instrumentation（与 10-19 相同的文件协议）

被测 App 内运行真实服务、真实 Telecom/虚拟 modem、真实 ICE/DTLS-SRTP/Opus；只有蜂窝/麦克风 PCM 由合成音替代（Host 500 Hz，Gateway 900 Hz，16 kHz）。

- 启动：`adb shell am instrument -w <pkg>.test/com.callagent.lab.<Gateway|Host>JointInstrumentation`，前先写 `files/joint-fixture.json`（`api_url`、`pairing_code`、Host 另需 `gateway_id`，复用配对时 `reuse_pairing: true`）。
- 就绪：`files/joint-ready.json` 为 `{"status":"READY"}`；之后每条命令写 `files/joint-command.json`（`id` 唯一），结果在 `files/joint-result.json`。
- Gateway 命令：`reconnect`、`mesh_add`（`api_url`、`pairing_code`）、`hangup`、`mute`（`enabled`）、`status`、`stop`。结果含 `bridge`、`telecom_state`、`pcm`、`dtmf_events`；`media`/`call_control` 固定为 `webrtc`/`rtc`。
- Host 命令：`listen`、`reconnect`、`mesh_add`、`dial`（`number` 只允许 `+15550100200`）、`answer`、`reject`、`hangup`、`mute`、`dtmf`、`clear`、`status`、`stop`。**变化**：`rtc` 命令只接受 `enabled: true`，`false` 返回 `sip_removed`（0.4 没有开关）。
- 来电：`adb -s <gateway> emu gsm call <号码>`；对方挂断：`emu gsm cancel <号码>`。

## 用例与判定

每个用例记录：三端 `call_id` 日志关联、PCM 计数（双向 `remote_tone_frames` 持续增长、接听前为 0）、结束原因、网关通话账本终态。

### 1. 升级（先装上一版，再覆盖到 0.4）

1. 三端都装上一版，服务器 `RTC_MEDIA_ENABLED=true RTC_CONTROL_ENABLED=true SIP_COMPATIBILITY_ENABLED=false`；完成配对、SIM 确认，收发短信，留一条网关短信归档。
2. 人为在数据库插入一条未结束的 `control_transport='sip'` 通话（复制上一版建表后的行即可），占住网关槽。
3. 按 [release-0.4.md](release-0.4.md) 的顺序升级：服务器 → Homura（`adb install -r`，再刷一次 Magisk ZIP）→ Madoka（`adb install -r`）。
4. 判定：
   - 服务器启动日志出现结束旧 SIP 通话；该行 `ended/sip_removed`，网关槽已释放，随后新通话可以拨出。
   - `GET /v1/capabilities` 中 `rtc_audio`、`rtc_call_control` 为 true，没有任何含 `sip` 的键；`/v1/devices/self/sip-config`、`/v1/call-intents` 已不存在（404 或 405）。
   - 两个 App 不需卸载，配对、SIM 绑定、短信历史、网关短信归档、主机节点列表全部保留；`dumpsys package` 显示 versionCode `1000000000`、versionName `0.4.0-…`。
   - Homura 首页“远程通话”在诊断并启动后显示通话控制已就绪；Madoka 设置中没有 SIP/WebRTC 开关。

### 2. 通话（全部 0.4）

| 用例 | 判定 |
| --- | --- |
| Madoka 实际 UI 拨出，通话 ≥ 95 秒 | 双向 PCM；Homura 只拨一次（账本一行） |
| 虚拟来电，Madoka 接听，≥ 95 秒 | 同上；接听前两端 PCM 为 0 |
| 静音/恢复（两端）、DTMF、任一端挂断、对方挂断 | 状态与结束原因正确，账本终态 `completed` |
| Madoka 拒接、来电方取消 | 蜂窝挂断，无残留占用 |
| 唤醒丢失：阻断 Madoka 到 `/v1/ws` 的连接后来电 | 8 秒兜底轮询内开始振铃，仍可接听；轮询中注入一次网络错误后轮询继续 |
| 没有就绪的 Madoka 时来电 | 服务器 503 `GATEWAY_UNAVAILABLE`，网关拒接 GSM，无通话记录残留 |
| 屏蔽 UDP 后（只剩 TURN/TCP） | 仍能接通并双向音频 |
| 熄屏 ≥ 75 秒后来电，通知接听、熄屏通话、通知挂断 | 同 10-19 熄屏用例 |
| 媒体中途断开 / 控制租约停止续期 | 通话结束或标 `unknown` 保留占用，**不得**重复拨号 |

### 3. 多节点

- 网关和主机都加入 A/B/C。来电时 A 正常、**B 不可达**：网关跳过 B（日志 `incoming_offer_failed`），A/C 照常振铃并可接听（0.4 新行为，旧版会整通失败）。
- 主机在 A 接听：C 上的参与者收到 `answered_elsewhere`。
- 空闲时 A 不可达 → 主机自动切到 B 并可通话；A 恢复后，通话中保持 B，挂断空闲后回切 A。

### 4. Homura 新界面

- 用 Homura 仓库的 `scripts/android-ui-smoke.py --scenario gateway`（已适配底部导航 `navSettings`/`navHome`）跑一次 Android KVM UI smoke 工作流，结果须 PASS。
- 截图：状态页（未配对、已配对短信在线、通话就绪、通话中卡片）、记录页（含呼入/呼出/未接/短信，切换筛选）、设置页全页、运行日志页；深色模式各一张。检查文字不被截断、点按区域 ≥ 48dp、折叠屏宽度下内容居中且不超过 760dp。
- 旋转/重建后：当前页、设置页滚动位置、正在编辑的服务器地址/设备名（含光标与键盘）恢复；一次性配对码不恢复。

### 5. 回退（每次只回退一个组件，测完再前滚）

| 场景 | 操作 | 判定 |
| --- | --- | --- |
| 仅服务器回退 | 只把镜像换回上一版（compose 中改镜像标签）重启（用 0.4 的 `deploy/compose.yaml`）；一体镜像按 release-0.4.md 加 3 个 `-e` | 0.4 Homura/Madoka 短信与双向通话正常；不需要 Asterisk |
| 仅 Homura 回退 | `adb install -r` 上一版 APK（不卸载）；Magisk 刷上一版 ZIP | 配对与 SIM 保留；**来电**可接通（依赖 0.4 服务器的 `incoming-policy` 兼容接口），去电正常 |
| 仅 Madoka 回退 | `adb install -r` 上一版 APK | 配对、短信历史保留；WebRTC 开关为开时通话正常 |
| 全部前滚回 0.4 | 再次覆盖安装/换镜像 | 数据仍完整，用例 2 抽测拨出与来电各一次通过 |

### 6. 负向与清理

- 全程 SIP/ARI 抓包为 0；`docker top` 无 asterisk 进程；0.4 镜像内无 asterisk 可执行文件。
- 一体镜像：重启后 PostgreSQL 干净关闭；杀掉 turnserver 后容器退出并由重启策略拉起。
- 设备撤销后该设备的旧 `ps_*`/`device_sip_credentials` 行被删除。

## 交付物

- 在服务器仓库新增 `docs/release-0.4-validation-<日期>.md`，格式参照 10-19 报告：提交与 digest、逐项 PASS/FAIL、发现并修复的问题。
- 证据包（JSON 结果、三端仅含媒体状态的日志、截图、抓包计数）上传为 Release 附件，不提交到仓库。
- 物理蜂窝 HAL 音频、真机省电策略**不**在模拟器上宣称通过，仍列为待真机测试。
- 发现缺陷：在对应仓库的 `claude/charming-franklin-8piri8` 分支修复并补回归测试后重跑受影响的用例；不要放宽产品超时或跳过测试。

## 第二轮补测（第一轮结果见 [release-0.4-validation-20261022.md](release-0.4-validation-20261022.md)）

范围按用户确认：三端只做**同步**升级/降级，单组件回退与混合版本不再测。Magisk 刷入继续按用户授权跳过（模块挂载与启动另由 GitHub 上的 Magisk AVD smoke 覆盖）。Fold UI smoke 已由 GitHub Actions 的 Android KVM UI smoke 在最终网关提交上通过，不需在本机重跑。

全部用例使用三个分支的最终 head（开测时记录 SHA）重新构建的产物，不沿用第一轮中途的 APK。

| # | 用例 | 判定 |
| --- | --- | --- |
| R1 | **最终产物回归**：95 秒去电（Madoka 拨出、Madoka 挂断）与 95 秒来电（Homura 侧挂断）各一次；对方挂断一次 | 双向 PCM 持续增长；正常挂断后两端都是 `ENDED`（不得为 `FAILED`）；网关账本每通一行 `completed`，槽释放 |
| R2 | **最终产物多节点**：B 不可达时来电；空闲时 A 不可达切到 B、通话中恢复 A 保持 B、挂断空闲后回切 A | 同第一轮组 3；回切在 120 秒内 |
| R3 | **全 0.4 零 SIP/ARI 抓包**：A/B/C 重新开始抓 5060/5061（UDP+TCP）与 8088，三端全为 0.4，覆盖 R1、R2、一次收发短信、Homura 与 Madoka 各重启一次，以及至少 15 分钟空闲 | 三个节点所有被测端口计数全为 0；记录 pcap 哈希与起止时间 |
| R4 | **三端同步回退再前滚**：按 [release-0.4.md](release-0.4.md) 的顺序把服务器（带三个 `RTC_*`/`SIP_COMPATIBILITY_ENABLED` 变量）、Homura、Madoka 依次覆盖回上一版；随后再依次升回 0.4 | 每一步之后：配对、SIM 绑定、短信历史、网关短信归档、主机节点列表保留，无需卸载；回退完成后与前滚完成后各做一次收发短信、一次去电、一次来电并正常挂断；全旧版本期间抓包 SIP 也应为 0（上一版服务器 `SIP_COMPATIBILITY_ENABLED=false`） |
| R5 | **界面补证**：记录页含短信行（浅色与深色），点开短信详情 | 截图/XML 显示短信行与详情；文字无截断 |

R3 可与 R1、R2、R5 同一时间窗口执行；R4 放在最后，结束后三端回到 0.4。结果追加到 `release-0.4-validation-20261022.md` 的新小节（或新建同格式报告），第一轮的 FAIL 原样保留。

## 第三轮（续测阻塞后）

续测记录的阻塞（[续测小节](release-0.4-validation-20261022.md)：已接通来电在控制过期后变成 `unknown`，网关账本与两端槽永久占用）是产品缺陷，已在 Homura `b6dfcea` 修复：网关空闲且 Telecom 没有任何通话时，把仍占着 `rtc_cellular_slot` 的 `unknown` 账本行记为 `failed`，并向主节点与所有 mesh 节点排队 `ended`；节点轮询送达后服务器把该通话结束为 `ended|cellular_ended` 并释放槽。Telecom 一直没有报告的去电派发在 30 秒后按同样方式放弃；拆除开始后才出现的去电会被挂断。服务器与 Madoka 不变（服务器代码 `c6e6013`、Madoka `f2680de`）。

只需用原 debug keystore 重建 Homura（`GSM_RELEASE_VERSION_CODE=1000000000`），**在当前阻塞状态下**直接 `adb install -r`，不清数据、不改数据库。

| # | 用例 | 判定 |
| --- | --- | --- |
| R0 | **遗留通话恢复**：安装后启动 Homura，等待至多 60 秒 | 网关日志 `call_id=8aab9419-… event=unknown_call_released`；账本该行 `failed`、`rtc_cellular_slot` 0 行、`rtc_control_terminals` 清空；A 上该通话 `ended|cellular_ended` 且 `gateway_call_slots` 无该行；随后一次去电、一次来电正常接通并挂断（`ENDED`，账本 `completed`） |
| R0b | **复现并验证恢复**：来电接通后 `am force-stop` Homura，保持蜂窝通话 ≥ 30 秒（服务器转为 `unknown|control_expired`），再启动 Homura | 蜂窝通话仍在（`emu gsm list` 可见）期间**不得**释放；`emu gsm cancel` 挂断后 60 秒内释放，下一通去电与来电正常 |
| R0c | 同 R0b，但在 force-stop 期间先挂断蜂窝通话，再启动 Homura | 启动后 60 秒内释放；下一通去电与来电正常 |

R0 通过后按第二轮表格继续，范围与判定不变：

1. **R3 完整窗口**（`r3-window.py`）：短信、R1、R2、两端各重启一次、R5、≥15 分钟空闲在同一窗口内走完。R2 partial-node 接受"offer 前排除 B（`nodes=2`、C `answered_elsewhere`）"作为 B 排除证据；idle-failover 拨号前等网关在 B 上就绪（helper 已有闸门）。
2. **R5** 在 R3 窗口内正式采集（`r5-sms-ui.py`）。
3. **R4** 最后执行：先在回退状态下运行 `r4-sms-injection-diagnose.py` 定性上一轮"回退后入站短信注入失败"，再按原判定做回退与前滚。

正常使用可能出现的失败仍然中断测试并记录；结果追加到验证报告的新小节，既有 FAIL 原样保留。
