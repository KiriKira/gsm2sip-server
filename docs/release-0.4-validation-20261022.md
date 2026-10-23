# SoulGem、Homura、Madoka 0.4.0 联合验证（2026-10-22）

> **总体结果：FAIL / PARTIAL，不能判定六组全部通过。** 已执行的主要通话、多节点与升级用例通过；全程 SIP/ARI 零流量断言失败，Fold UI smoke 因资源预检未通过而未执行。混合版本回退按用户后续要求排除。原始计划见[联合测试计划](plan-0.4-joint-test.md)和[0.4 发布说明](release-0.4.md)。

**用户后续确认的范围调整：三端由用户保证同步升级、同步降级，因此原计划组 5 的单组件回退与混合版本兼容矩阵列为 `EXCLUDED — 用户管理同步升级/降级`，不再作为本轮待完成项。** 已执行的探索结果和失败原样保留；这不表示原计划中的混合版本兼容要求通过。Magisk 的安装、激活和回退也按用户授权排除。

## 六组完成情况

| 计划组 | 结论 | 范围与限制 |
| --- | --- | --- |
| 1. 升级 | **PASS（APK/服务端路径）** | 覆盖安装、配对与数据保留、遗留 SIP 清理通过；Magisk 刷入按用户授权未执行。 |
| 2. 通话 | **PASS（已执行用例）** | 95 秒来去电、控制、熄屏、TURN/TCP、异常恢复通过；各结果对应的实际 APK 见下文。最终 APK 的 active-card UI 拨号抽测另有 PASS，没有重复 95 秒通话。 |
| 3. 多节点 | **PASS** | 部分节点不可达、空闲切换与通话保持/空闲回切通过，使用调度修复 APK。 |
| 4. Homura UI | **PARTIAL** | 已配对截图、记录筛选、设置/日志、浅深色真实通话卡片通过；官方 Fold smoke 未执行，旋转/重建与宽屏完整验收未获得 PASS，短信行截图仍缺。 |
| 5. 混合版本回退 | **EXCLUDED** | 用户保证三端同步升级/降级；已执行探索结果保留，不继续单组件兼容矩阵。 |
| 6. 负向与清理 | **FAIL（全程零流量断言）** | 最终 B/C 的 UDP 5060 各 47 包；其余端口均为 0。无 Asterisk、镜像扫描、设备撤销、一体监督及故障注入清理通过。 |

## 被测源代码与构建

| 组件 | 升级前基线 | 0.4 候选 | 实际产物 |
| --- | --- | --- | --- |
| SoulGem Server | `bf41e9efd3bd4e3e6895e2c896f5ded1cf10675e` | `621c9e14ed8830dbec538b96fcedad7bdd2e48c4` | API 镜像远端 Docker image config ID `sha256:f97daf5bb3332497f33c8d1441615a622645d1296e17e4a4890c2dcbc113ff05`；一体镜像 image config ID `sha256:00b55d42b2bb432868f5824f8ce15efafc114db424fb17cac0f82e29c710e842` |
| Homura Gateway | `dee22f93f73a935b1ff970bb7c090fa6e4bd8da8` | `e6b3bd724c51ca3954b3ffc3489bcb19e5d6bd03` | 最新 0.4 APK SHA-256 `d0b09cf046bbf98a54ecb2a42cc3e678aaf4f676bc539f0db31fb239a98f13e7`；Magisk ZIP SHA-256 `557710466032df0f7d47a35d0e4e0c0964fce90e953549846dfbdc8677ddc581`，见 `artifacts/builds/candidate-touch-fix/manifest.json`。用户授权跳过 Magisk，因此 ZIP 未刷入 |
| Madoka Host | **`e5cf86f96e747a7cee5ffb24572af72379c3851a`** | `453fc009267aa4b288facab356827aebc0c2cb1e` | 0.4 APK SHA-256 `a2cdd246c4b83aa4d23aa5dab8cce48cf05e9454ce24b21bcf6e82375cab5ff1` |

两端 APK 使用共同签名证书 SHA-256 `2c613d0386c319b54abd14796fee004ea44681bfe3b69ddd33f7062cd3845f28`，版本号均为 `1000000000`；基线与候选均可覆盖安装。Gateway APK `d0b09cf…f13e7` 与 instrumentation APK `9fa8f06f…be57c8` 曾安装并完成 metadata 核验；ZIP `55771046…ddc581` 已构建并完成 metadata 核验，按用户授权未刷入 Magisk。Case 5 探索的 Gateway-baseline 来电失败已保留；当前 A/B/C、Gateway 和 Host 均已恢复为 0.4 候选。构建清单为 `artifacts/builds/candidate-touch-fix/manifest.json`。API 28 聚焦测量测试 **1 PASS、0 skipped**，XML 为 `artifacts/regression-status-pill-48dp.xml`。服务端 API 与一体镜像的本地构建也成功，构建清单记录工作树干净。表中的 Docker 值是 image config ID，不是 registry digest；本地与导入镜像的 config ID 不同，但构建清单记录 rootfs diff IDs 与运行配置一致。

基线和联测环境使用 KVM：实际 Gateway 为 API 36 default x86_64 AVD 及其虚拟 modem，Host 为 API 29；Server A/C 为一体镜像，B 为分拆部署，A/B/C 使用独立网络，无 Asterisk 进程。`google_apis` 克隆环境启动失败，未用于联测。用户授权跳过 Magisk 环境，因此模块引导、安装、激活和回退均为 `NOT RUN — 用户授权跳过`；这不代表 Magisk 路径通过。实际测试使用原 API 36 default AVD。

Gateway 的调度修复位于 commit `03e694d`，包含在候选 `1f1d70b` 的 APK `8935f213…5375` 中；该版本加入主控制失败时健康 secondary 的轮询恢复。随后 commit `e6b3bd7` 修复状态 pill 实际触控高度：原视图在 160 dpi 实测为 40 px，虽然可点击但没有额外触控 delegate；现在最小高度为 48 dp。APK `d0b09cf…f13e7` 已安装并完成 metadata 核验；混合版本探索结束后，当前 Gateway 已恢复此 0.4 候选。组 3 测试是在 `8935f213…5375` 上完成；95 秒媒体用例是在此前 `75ca1156…b779` 上完成。两批结果都没有在 `d0b09cf…f13e7` 上重跑。

## 已完成项目

| 计划组 | 项目 | 结果与证据 |
| --- | --- | --- |
| 升级 | 三节点 Server 切换至 0.4、能力与旧路由检查 | **PASS**：A/B/C 均报告 RTC audio/control 能力；旧 SIP 配置及 call-intents 路径返回 404。切换记录中的 A/B/C Asterisk 进程数为 0。 |
| 升级 | 遗留 SIP 通话清理 | **PASS**：升级前写入的未结束 SIP 通话及 participant 均变为 `ended/sip_removed`，网关占用槽释放；重复清理不再改变结果。升级验证确认 call history 仍可读取。 |
| 升级 | Gateway/Host 覆盖安装 | **PASS**：两端从基线 APK 以 `install -r` 覆盖到 `0.4.0-joint`，版本码保持 `1000000000`，instrumentation fixture 与应用数据保留。 |
| 升级 | 候选三端安装前后的短信收发与保存 | **PASS**：合成收、发短信各一条；Server、Gateway 本地归档和 Host history 均记录一条，正文只以哈希留证。`verified` 记录为 13:37:30Z，晚于候选 Server 切换及首次 Gateway/Host APK 覆盖安装（13:36:52Z、13:37:05Z、13:37:16Z）；后续 bugfix APK 覆盖安装不改变这次首轮候选升级验证结果。 |
| 升级 | 配对、SIM 和 Host 三节点数据保留 | **PASS**：升级验证记录同一 Gateway/SIM、原映射 revision 1、A/B/C 三个 Host 节点键仍在，`data_retained=true`。 |
| 通话 | Host 发起去电，95 秒双向媒体、静音/恢复、DTMF、Host 挂断 | **PASS**：合成 PCM 双向持续增长 95.72 秒；两端静音/恢复和 DTMF 通过；通话结束、服务端释放槽、Gateway 账本只有一条完成记录。 |
| 通话 | 虚拟来电，接听后 95 秒双向媒体、静音/恢复、DTMF、Gateway 挂断 | **PASS**：振铃约 3.04 秒；接听前 PCM 为 0；接通后媒体持续 96.25 秒；控制项通过，Gateway 正常挂断后 Host 最终为 `ENDED`。 |
| 通话 | 来电方挂断、Host 拒接、来电方取消 | **PASS**：三项各自通过，均无残留占用；证据分别记录在对应 JSON 中。 |
| 通话 | WebSocket 唤醒阻断及一次控制轮询网络错误 | **PASS**：来电在 4.33 秒开始振铃；注入轮询错误后仍能继续接通，双向音频持续 12.71 秒。 |
| 回退 | 单组件回退及混合版本兼容 | **EXCLUDED — 用户管理三端同步升级/降级**。探索 run `20261021T191621Z-47bb1e29` 的 Server 单独回退/前滚、fresh SMS 与双向通话通过；Gateway-baseline fresh SMS 与去电通过，来电 `e7b297a5` **FAIL**。失败证据保留，未将其改判为通过；Host 单独回退不再执行。 |
| 通话 | 组 2：媒体断连和控制租约停止续期 | **PASS**：`media-disconnected` 与 `control-lease-stopped` 均通过，见 `evidence/functional-batch-20261021T183241Z.json`。这两项使用此前的 `75ca1156…b779` Gateway APK；不代表在最终 `d0b09cf…f13e7` APK 上重跑。 |
| 通话 | 无就绪 Host 时的来电拒接 | **PASS**：修正 helper 后的远端结果显示一条 state=2 Telecom 来电、三次 RTC offer 失败、同一 call ID 对应的新 `503 GATEWAY_UNAVAILABLE`，随后 Telecom 移除；Gateway 拒接，A/B/C 的 `call_sessions` 计数前后不变。该联测结果在远端 `evidence/no-ready-host.json` 和 `functional-batch-20261021T182536Z.json`。 |
| 通话 | 屏蔽 UDP 后使用 TURN/TCP | **PASS**：去电双向 relay/relay 媒体 36.27 秒，实际 Telecom 仅一通；服务端终态为 `ended/cellular_ended`，槽释放，Gateway 账本仅一条完成记录。证据在远端 `evidence/turn-tcp-only.json`。 |
| 通话 | 熄屏后来电、通知接听并持续通话 | **PASS**：熄屏待机 75.589 秒，接听后熄屏音频 76.377 秒；通知实际执行 `ANSWER` 和 `HANG UP`，接听前 PCM 为 0，前后台同一应用进程，实际 Telecom 仅一通且服务端槽已释放。证据在远端 `evidence/screenoff-75s-notification-audio.json`。 |
| 多节点 | 组 3：部分节点不可达与空闲 failover/hold/failback | **PASS**：调度修复 Gateway APK `8935f213…5375` 上的 `incoming-partial-node-unreachable` 与 `idle-failover-hold-failback` 均通过，见远端 `evidence/functional-batch-20261021T185406Z.json`。 |
| 界面 | Gateway 已配对状态、SIM 设置、日志、记录筛选及深色状态页截图 | **PASS（截图采集与恢复范围）**：8 组截图/XML 完成，恢复原 Home 页、记录筛选和夜间模式，没有更改配对或应用数据。最新状态 pill 48 dp 聚焦回归通过，见 `artifacts/regression-status-pill-48dp.xml`。官方 Fold/UI smoke 与旋转重建项仍未完成，不能据此认定整组通过。 |
| 界面与通话抽测 | 最终 0.4 APK 的真实 UI 拨号与通话中卡片（浅色/深色） | **PASS**：`gateway-active-card-light-dark-run2.json` 记录双向合成音频 12.753 秒，两种主题卡片均通过；Host 挂断后服务端 `ended/cellular_ended`、槽释放，Gateway 仅一条 `completed` 账本，实际 Telecom 仅一通。截图/XML 位于 `ui-active-card-run2/`。该抽测不替代或重复上述 95 秒媒体用例。 |
| 界面 | Host 设置页检查 SIP/WebRTC 通话模式 | **PASS**：只读检查了设置页与滚动内容，没有发现通话模式开关或 SIP/WebRTC 选择项；检查后恢复原电话页，未改设置。 |
| 负向清理 | 设备撤销后的旧 SIP 表数据 | **PASS**：`ps_auths`、`ps_endpoints`、`ps_aors` 和 `device_sip_credentials` 各从 1 行变为 0 行。 |
| 负向清理 | 一体镜像监督 | **PASS**：PostgreSQL 干净关闭；终止真实 turnserver 进程后容器按重启策略恢复，restart count 从 0 增至 1。 |
| 负向清理 | 最终运行进程与故障注入清理 | **PASS（补审后）**：`docker-no-asterisk-audit.json` 以 `docker top <container> -eo pid,comm` 核验 7 个组件均运行，Asterisk 为 0。`final-negative-audit.json` 同时确认 HTTP/network fault 配置为空、无 GSM04 iptables 规则、A 网桥无 netem。 |
| 负向清理 | 0.4 镜像内 Asterisk 可执行文件 | **PASS**：API 与一体镜像扫描均未发现 Asterisk 可执行文件。 |

## 有针对性回归的已修复问题

| 缺陷 | 修复与回归证据 |
| --- | --- |
| 0.4 启动时遗留 SIP 清理没有将旧终止原因为 `sip_removed`，participant 也需要一致结束 | Server `EndLegacySIPCalls` 统一设置通话和 participant 的终止原因并释放槽。测试以非空旧 reason（通话 `ari_reconciliation_uncertain`、participant `legacy_participant_reason`）验证覆盖、history 可读及重复调用幂等。实际 disposable PostgreSQL 18 集成测试 `TestLegacySIPCallsGetNoMediaAndAreEnded`：**PASS，未跳过**；命令和 stdout 在 `artifacts/backend-regression.log`。 |
| Incoming fork 中，winner 发生时其他节点的 create 仍可能在途；终态请求若先得到 404 并确认，会漏掉之后创建的来电 | Gateway fork coordinator 在 winner/ended 后关闭新 offer；在途 create 返回后再排终态，未知响应仍保留清理尝试；明确 `GATEWAY_UNAVAILABLE` 不计为已成功 offer。`IncomingForkCoordinatorTest` 3 项：winner/create barrier、ended 覆盖未知 create、明确拒绝后健康节点继续，**3 PASS、0 skipped**。 |
| 正常远端挂断时 Host media authorization 可能先于控制轮询关闭，导致 Host 报 `FAILED` | Host 重新读取控制快照：`end_requested` 或 `ended` 作为正常断开；仍 active 或 GET 失败则 fail closed 并尝试 hangup。`RtcCallEngineMediaClosureTest` **4 PASS、0 skipped**；上表 96.25 秒真实联测也验证 Gateway 正常挂断后 Host 最终 `ENDED`。 |
| Gateway 控制终态和媒体 session close 竞争时可能发送重复/冲突关闭 | 终态关闭路径避免与 signaling close 竞争，默认 close 保持单次且幂等。`RtcAudioSessionCloseOrderingTest` **2 PASS、0 skipped**。 |
| Gateway 状态 pill 的实际可触控高度只有 40 px（160 dpi），且没有触控 delegate 扩大命中区域 | commit `e6b3bd7` 将最小高度设为 48 dp；API 28 实际测量和布局回归 `GatewayMaterialLayoutTest.statusPillHasA48dpMeasuredTouchTarget` **1 PASS、0 skipped**，见 `artifacts/regression-status-pill-48dp.xml`。APK SHA-256 `d0b09cf…f13e7` 的安装核验与 ZIP SHA-256 `55771046…ddc581` 的构建核验通过；当前 Gateway 已恢复该 0.4 APK，组 3 和 95 秒媒体用例仍引用上述各自实际产物，未改写为最终 APK 的重跑结果。 |
| Gateway 处理服务端 media 授权关闭时，需要核验真实控制终态并保持非正常失败语义 | `RtcAuthorizationClosurePolicyTest` 覆盖 fresh `end_requested`、已 ended、仍 active、陈旧结果及 GET 错误：**4 PASS、0 skipped**。 |
| 主控制面失败的指数退避也暂停健康 secondary 的同步，导致 failback 超过 120 秒 | Gateway `03e694d` 让已成功同步的 secondary 将下一轮控制轮询恢复为 30 秒；没有 secondary 成功时仍保留指数退避。生产使用的 retry/sync 调度回归 XML `artifacts/regression-com.callagent.gateway.service.ControlRetryScheduleTest.xml`：**5 PASS、0 skipped**；调度修复 APK `8935f213…5375` 上的真实组 3 failback 重跑通过。 |

以上 Android 回归 XML 保存在 `artifacts/regression-*.xml` 和 `artifacts/incoming-fork-regression.xml`。针对性测试通过不替代未执行的 Fold UI 验收，也不改变最终抓包断言失败。

## 环境恢复与映射 revision

升级保持验证时原 Gateway ID 与 SIM ID 的绑定为 revision 1，见 `artifacts/evidence/upgrade-preservation.json`；该证据保持不变。之后 API 36 AVD 重启未提供原 SIM fingerprint，既有 `localConfirmationExpired` 安全策略使映射确认过期，heartbeat 将映射 revision 从 1 推进到 2，来电被 `No verified SIM account mapping` 拒绝。一次普通 UI 提案产生了未确认的临时 SIM ID；它没有被确认，随后在测试环境 FK 引用清零后移除。

随后 x86 测试 fixture 以“同一 AVD、同一 subscription、显式原 `sim_id`”走既有提案与确认接口，并通过原有 fingerprint/account 校验，没有放宽产品安全检查。`artifacts/evidence/emulator-retained-sim-recovery.json` 记录 **PASS**：同一 Gateway ID、原 SIM ID 恢复到 revision 4。这个 revision 4 是升级保持 PASS 之后为后续联测所做的明确环境恢复；revision 1 的 baseline/升级证据没有覆盖或改写。后续测试的预期 revision 只能引用此 PASS 证明，不从运行时当前值动态推导。

## 未通过、未执行及排除项

| 计划组 | 项目 | 当前状态 |
| --- | --- | --- |
| 界面 | 官方 Fold smoke、旋转/重建和屏幕尺寸完整验收 | **NOT RUN — 资源安全预检未通过**：可用内存 5059 MiB，小于 5120 MiB 门槛，Fold guest 与 smoke 均未启动，不计通过。launcher 自动恢复原 Gateway 5554；Host 5556 始终在线。证据为 `ui-smoke/attempt-fold-official-20261022/launcher.log`、`launcher-result.json` 与 `resource-gate.json`；结构化摘要记录 5059/5120 MiB、Fold 未启动、launcher 结果与日志哈希，并保留 `gateway_serial_restored` 字段矛盾。 |
| 回退 | 原计划组 5：每次只回退一个组件的兼容矩阵 | **EXCLUDED — 用户管理三端同步升级/降级**。探索结果见下文；不再继续 Host 单独回退或扩展混合版本矩阵。最终全 0.4 的 UI 拨号与通话卡片抽测已 PASS，见上表。 |
| 界面证据 | 已配对 dark mode 10 组截图及短信行证明 | **PARTIAL**：截图已采集；仍缺 SMS history 行证明，待补。 |
| 负向清理 | 全测试期间 5060/5061/8088 的零流量 | **FAIL（连续抓包的全局断言）**：最新计数 A 三个端口均为 0，B/C 各捕获 UDP 5060 流量 47 包，其余被测端口为 0。混合版本探索中旧 Gateway 启动了 SIP 注册；不能用此前零流量 midpoint 证明覆盖这段流量，也不能声称全程为 0。三个 tcpdump 已向实际子进程发送 SIGINT、退出并完成 pcap flush/read/hash；最终无 Asterisk 的只读补审 PASS。原全程抓包结果保持 FAIL。 |

首次长来电联测在修复前发现 Host 把正常远端挂断报告为失败，保留在 `incoming-95s-controls-pre-hangup-fix-failure.json`；修复后同一用例通过，原失败证据未覆盖。一次 outgoing mute 检查的首轮失败来自测试脚本对 Gateway 静音方向的判定错误，修正脚本后用例通过。无就绪 Host 用例最初也因 helper 在新 GSM 来电到达 Telecom 前提前退出而记录失败；修正 helper 后，远端关联实际 Telecom add/remove 与同一 call ID 的新 503，结果 PASS。旧失败仍作为历史证据保留，不覆盖新结果。

媒体中断首轮失败 `current_main_activity_focus_timeout` 发生在拨出前；通知 shade 前置修正后，`functional-batch-20261021T183241Z.json` 的 media-disconnected 与 control-lease-stopped 通过。该次多节点 failback 仍失败：A 控制请求退避也暂停了 mesh secondary 同步。Gateway commit `03e694d` 让至少一个 secondary 成功后把控制轮询恢复到 30 秒；聚焦测试 XML `artifacts/regression-com.callagent.gateway.service.ControlRetryScheduleTest.xml` 记录 **5 PASS、0 skipped**。调度修复 Gateway APK `8935f213…5375` 的新 batch `20261021T185406Z` 中 partial-fork 与 idle failover/hold/failback 均 PASS；此前 120 秒超时仅保留为历史失败证据。

## 混合版本探索及恢复（用户已排除此兼容矩阵）

Case 5 前次 run `20261021T185941Z` 因 harness 把 baseline revision 1 用于映射 revision 4 的环境而失败。只读核对确认该 run 的两个短信哈希在 A/B/C 的 messages 与 commands 中均无记录；旧错误 label ledger 已归档，未重放短信。`sms-probe.py` 随后固定引用 `emulator-retained-sim-recovery.json` 的 PASS 与明确 revision 4，不从运行时当前值动态取预期结果。

run `20261021T190324Z-a2129c52` 的 fresh SMS 与 Server-baseline 去电通过；来电因临时 SMS-only client 触发 baseline 兼容保护而失败。代理记录 A 的 incoming create 返回 `409 RTC_COMPATIBILITY_REQUIRES_SIP`；B/C 创建成功，但使用 A 的 Host 看不到该呼叫。baseline Server 要求具有有效 session 的所有 active client 都具备 fresh/enabled RTC capability，临时短信 client 不符合。该批次保留为历史 **FAIL**。后续 run `20261021T191621Z-47bb1e29` 仅在 Server-baseline 来电探针窗口临时隔离该 client 的一个 session，随后精确恢复（`restore_status=PASS`），受保护的真实 Host/Gateway sessions 未改变；Server 单独回退及前滚、fresh SMS、来去电均 **PASS**。

同一 run 的 Gateway-baseline fresh SMS 与去电 **PASS**，来电 `e7b297a5-fe81-4d5e-9b42-654cbc46c8ce` **FAIL**。旧 Gateway 的 `ControlApiClient` 将候选服务器未返回的 `sip_call_control` 默认为 true，于是启动 SIP 注册；注册失败引发重连，打断正在建立的来电。这一失败发生于三端不同步的探索窗口；现已按用户确认排除该兼容矩阵，不继续为旧版本增加兼容路径，也没有改写失败或宣布兼容通过。连续抓包已受到该窗口 SIP 注册流量影响：A 的被测端口计数均为 0，B/C UDP 5060 各 47 包，因而全程零 SIP/ARI 断言为 **FAIL**。

恢复时先确认虚拟 modem 无通话，再由带严格 preflight 的 x86 测试 fixture 通过 Gateway 已认证的正常终态队列结束遗留 `unknown` 呼叫，没有直接修改数据库。`evidence/baseline-orphan-cleanup-verified.json` 为 **PASS**：Gateway 原来电账本保留 `failed`，占用数为 0；A 为 `ended/cellular_ended`，A/B/C 均无 Gateway 占用槽；原 Case 5 失败证据独立保留。恢复所用 instrumentation APK SHA-256 为 `424631d8f24c78ffbf9422442fc1f07d192e5e7c36f67a9b97af9465b6fa0b73`，应用 APK 未改变。此前一次 fixture 安装/启动竞态使用了旧 androidTest APK，导致 harness 误报恢复失败；最终明确执行相应恢复动作并核验为 PASS，旧尝试仍保留。

恢复后全 0.4 的真实 UI 拨号与浅深色通话卡片抽测 run2 已 PASS。Fold 尝试在启动前被内存预检拒绝，原 Gateway AVD 随后恢复。`launcher-result.json` 的 `smoke_exit_code=null`、`launcher_exit_code=2` 表明 smoke 未运行；该文件的 `gateway_serial_restored=false` 与同次 `launcher.log` 的最终恢复成功记录不一致。报告依据日志中的 `sys.boot_completed=1`、API 36 和 ADB 同时列出 5554/5556 记录恢复成功，并保留这个结果字段不一致，不将其当作 Fold 通过。

最终 `final-negative-audit.json` 为 **FAIL**：SIP/ARI 零流量断言不成立，且初次进程检查使用 `docker top -eo comm`，本机 Docker 返回 `Couldn't find PID field`，所以 7 个组件当时均为 unchecked。随后只读补审使用带 PID 列的 `-eo pid,comm`，`docker-no-asterisk-audit.json` 记录 7 个组件均运行且 Asterisk 为 0。补审只补足进程检查，没有覆盖初次审计失败或非零抓包。其余 capture 身份/过滤器、SIGINT flush、pcap 读取/哈希、fault 配置、iptables 与 netem 清理断言均通过。

## 运行限制与证据索引

联测在 Android Telecom、真实 RTC/ICE/DTLS-SRTP/Opus 服务和虚拟 modem 上运行；蜂窝侧音频由 16 kHz 合成 PCM 替代。Host/Gateway 音调用于验证双向帧增长、静音与 DTMF 路径，不代表物理蜂窝 HAL 音频或真实设备省电策略已通过。物理 SIM/modem 音频、真实手机后台策略仍需真机验收。

证据包以 `artifacts/evidence/` 的结构收录下列主要结果；原始敏感日志、密钥和数据库不提交到仓库。验证证据包作为 Release 附件提供，证据文件不提交到仓库。

| 类别 | 主要证据 |
| --- | --- |
| 构建与升级 | source/build manifests、`topology.json`、`upgrade-preservation.json`、`sms-baseline-…-{captured,verified}.json`、`legacy-call-seed.json` |
| 通话与多节点 | 各用例 JSON、`functional-batch-20261021T144503Z.json`、`functional-batch-20261021T182536Z.json`、`functional-batch-20261021T183241Z.json`、`functional-batch-20261021T185406Z.json`、`no-ready-host.json`、`turn-tcp-only.json`、`screenoff-75s-notification-audio.json` |
| 实际 UI 与 Fold 未执行证明 | Gateway/Host 已配对截图/XML、`ui-paired-final-pre-rollback/`、`gateway-active-card-light-dark-run2.json`、`ui-active-card-run2/`、`ui-smoke/attempt-fold-official-20261022/launcher.log`、`launcher-result.json` 与 `resource-gate.json` |
| 清理与最终负向审计 | `device-revoke-cleanup.json`、`aio-supervision.json`、`final-negative-audit.json`、`docker-no-asterisk-audit.json`；`no-sip-midpoint-20261021.json` 仅为中途零计数证明，不能替代最终非零结果 |
| 历史失败与恢复 | 早期用例 FAIL、混合回退探索 `case5-single-component-rollback-*.json`、`baseline-orphan-cleanup-verified.json`、`emulator-retained-sim-recovery.json` |
| 聚焦回归 | `artifacts/regression-*.xml`、`artifacts/incoming-fork-regression.xml`、`artifacts/backend-regression.log` |

## 第二轮补验（2026-10-22）

本节追加第二轮已完成结果，第一轮原文、FAIL 与证据保持不变。第二轮整体仍为 **FAIL / PARTIAL**：新抓包时段与短信 UI 验证通过，但来电、多节点通话唯一性及同步回退后的功能验证没有全部通过。第一轮尚未完成的 Fold 等项目不因本节而改判为 PASS。

第二轮候选使用以下重建提交；上一版仍固定为 Server `bf41e9e`、Gateway `dee22f9`、Host `e5cf86f`。

| 组件 | 第二轮候选完整提交 SHA |
| --- | --- |
| SoulGem Server | `c6e6013c44cb8d01df586bd53e98e1da55e8f157` |
| Homura Gateway | `e6b3bd724c51ca3954b3ffc3489bcb19e5d6bd03` |
| Madoka Host | `453fc009267aa4b288facab356827aebc0c2cb1e` |

| 补验项 | 结果 | 已完成事实与限制 |
| --- | --- | --- |
| R1：95 秒去电 | **PASS** | 双向媒体持续 95.776 秒。 |
| R1：95 秒来电及严格重试 | **FAIL** | Host 已发起接听流程，WebRTC peer 与 ICE 接通，但 Gateway 蜂窝通话仍为 `RINGING`，约 7.36 秒后变为 `DISCONNECTED/INCOMING_MISSED`。严格重试未收到振铃，不能认定来电通过。 |
| R1：10 秒来电方挂断 | **FAIL** | 未取得 `call_id`，未出现对应 Telecom/RIL call 事件。结合来电严格重试的结果，目前不能定性 R1 的原因。 |
| R2：部分节点不可达时来电 | **FAIL / unresolved** | `emu gsm call` 返回 OK，但等待 Host `INCOMING_RINGING` 的 20 秒窗口超时，`incoming-partial-node-unreachable` 返回 `call_state_timeout`；样本中 Gateway 始终为 `IDLE/telecom_state=7`，Host `phase=null`。尚未进入 C/winner 分支，现有证据不能将根因定性为产品或模拟器。 |
| R2：空闲 failover、通话保持与 failback | **FAIL / unresolved（完整 helper）** | A 不可达后切至 B 并通话、A 恢复后保持 B、挂断后独立 idle 观察回到 A，这些步骤通过；但 GsmCallService 日志有三条 `onCallAdded` 回调，helper 的 Telecom count 为 3，因此完整用例失败。回调日志没有稳定的 Telecom call ID，只能确认回调条数，不能推出三次唯一拨号；也无法离线去重或证明通话唯一性，所以仍保留 FAIL，不认定为纯统计误报。 |
| R3：全候选独立时段抓包与最终审计 | **PASS（该时段）** | capture phase `r3-all-candidate-final` 从 03:06:35Z 开始，覆盖 R1、R2、短信、R5，以及 Gateway/Host 的 force-stop 后冷启动；还覆盖 03:29:58–03:45:04Z 的 idle 观察。最终审计确认三桥 manifest、capture flush/read/hash、无 netem/故障 iptables 规则、无 Asterisk；A/B/C 的 5060、5061、8088 计数全部为 0。这只证明第二轮该独立时段，不覆盖第一轮全程非零结果。 |
| R4：三端同步回退与恢复 | **FAIL（功能验证未完成）** | 按 Server → Gateway → Host 完成回退；各步骤的版本、配对、SIM、节点与历史留存快照通过，全旧版本时段的 capture final audit 也通过。首次 baseline fresh SMS 的 outbound 已提交，但 inbound injection 失败；按计划停止并恢复，未执行 baseline 通话或前滚后功能 smoke。 |
| R4：停止后恢复状态 | **PASS（状态核验范围）** | 按 Server → Gateway → Host 恢复全部 0.4 版本；A/B/C active slots 均为 0，Gateway 为 `IDLE`，Host 保持 listening，身份保持。此状态核验不替代尚未执行的 baseline 通话或前滚 smoke。 |
| R5：短信记录与详情浅深色 UI | **PASS** | `evidence/r5-sms-ui/` 中的 `records-light`、`records-dark`、`detail-light`、`detail-dark` PNG/XML 已采集；详情与脱敏文本匹配。 |

第二轮证据根目录为远程 `/home/ubuntu/gsm-webrtc-validation/joint-0.4/round2/r2-20261022/evidence`。R4 根日志只保留摘要。上述 R1/R2/R4 失败与未执行步骤保持原状态；本轮没有据此扩大 SIP 兼容范围或恢复单组件混合版本回退矩阵。物理蜂窝 HAL 音频与真机省电策略仍不在模拟器通过范围内。

## 最终 head 补验与交接（2026-10-22）

用户要求停止测试并把可续跑材料同步到 GitHub。以下是前述第二轮报告之后，修复最新 Host 缺陷、从三个分支最新 head 重新构建并覆盖安装后取得的结果。先前轮次的 FAIL、R3/R4/R5 结果和证据均原样保留；本节不把旧 APK 的结果移植到最终 APK，也不表示 R1–R5 已全部完成。完整测试方法与本轮 helper 已加入 `scripts/joint-validation-0.4-round2/`。

最终 Android 构建均使用同一本地 debug keystore、`GSM_RELEASE_VERSION_CODE=1000000000`，通过 `adb install -r` 覆盖安装，没有卸载或清除数据；两端安装证据中的 `fixture_retained` 为 true。证书和 APK SHA-256 见 Release 附件中的 `final-build-manifest.json`。

| 组件 | 分支 head / 构建来源 |
| --- | --- |
| SoulGem Server | 分支 head `b8575f65438f7f4bdc5e44d12b39dac75c943692`；当前 0.4 镜像的代码来源 `c6e6013c44cb8d01df586bd53e98e1da55e8f157`（后续服务器提交只改验证文档） |
| Homura Gateway | `15e9ac7e3ad8a7168ba51ccaeda6b00325a32b0d` |
| Madoka Host | `f2680de748381fd09cc23d84c99d723b6ffb903a`，包含此前的已移除 SIM 选择修复 `264a0b2a18f10a6ee0b4197ab4bf117811ac6343` |

Host 最终修复将认证刷新锁从全局范围缩小到 pairing session，并让节点探测并行执行、短信历史刷新独立 single-flight，避免一个慢节点的刷新/探测阻塞其他节点。节点健康阈值和超时没有放宽。该提交的聚焦回归为 16 项通过（包含新增隔离回归），主 APK 与 androidTest APK 构建通过。

| 最终 head 用例 | 结果 | 记录 |
| --- | --- | --- |
| R1：95 秒去电 | **PASS** | 双向合成 PCM 持续 `95.730 s`；Host 挂断；实际 Telecom 1 通；Gateway 账本一条 `completed`，占用槽为 0；Host `ENDED`。 |
| R1：95 秒来电与正常挂断 | **PASS** | 双向合成 PCM 持续 `96.361 s`；Gateway 挂断；振铃延迟 `4.423 s`；接听前 PCM 为 0；实际 Telecom 1 通；账本一条 `completed`、占用槽为 0；Host `ENDED`。 |
| R1：来电方 10 秒后挂断 | **PASS** | 双向合成 PCM 持续 `10.732 s`；modem 发起挂断；实际 Telecom 1 通；账本完成且槽释放；Host `ENDED`。 |
| R2：部分节点不可达时来电 | **PASS** | B 的 create 失败有日志，C 记录 `ended/answered_elsewhere`，经 A 接通；双向合成 PCM `12.736 s`；实际 Telecom 1 通；账本完成且槽释放。 |
| R2：空闲 failover、通话保持及 failback | **FAIL（helper 原样保留）** | 结果文件记录通话在 B、A 恢复后仍保持 B、挂断后 idle failback 到 A 三项行为断言均 PASS；双向合成 PCM `12.801 s`、实际 Telecom 1 通、账本完成且槽释放。helper 最终状态仍为 FAIL，错误为 `[Errno 32] Broken pipe`。因此不将完整 R2 用例改判 PASS，续测代理应先复核输出管道/完成记录边界，再依据计划决定是否重跑。 |
| R3：最终 head 零 SIP/ARI 抓包 | **INCOMPLETE** | phase `r3-all-candidate-final` 起于 `2026-10-22T05:35:33Z`，最终审计于 `05:45:27Z` 完成。审计本身 PASS：A/B/C 各自的 5060、5061、8088 计数均为 0，三份 pcap 均 flush、可读并记录 SHA-256；Asterisk 为 0，netem、HTTP/network 故障配置及 GSM04 iptables 规则均为空。该约 9 分 55 秒窗口覆盖了本节 R1/R2 活动，但没有覆盖一次新短信收发、Homura 与 Madoka 各重启一次、R5 截图或至少 15 分钟空闲，不满足完整 R3。 |
| R4：同步回退再前滚 | **未在最终 head 上重跑** | 前一候选版本的 R4 结果仍按上节原样保留；不能代表 `15e9ac7`/`f2680de` 最终构建。 |
| R5：短信记录行与详情截图 | **未在最终 head 上重跑** | 前一候选版本的截图结果仍按上节保留；最终 head 尚无新的 R5 证据。 |

最终 head 证据来自远程运行目录 `/home/ubuntu/gsm-webrtc-validation/joint-0.4/round2/r2-retest-f2680de-20261022T0535Z/evidence`。打包器检查通过的 13 个文件及校验索引作为 Release 附件 `final-head-partial-evidence-20261022T0535Z.zip` 和 `final-head-partial-evidence-index-20261022T0535Z.json`；ZIP SHA-256 为 `66107f50ee40068f89e4945c74958b5e95b0c7a150fe359e531c189983ba9b74`。附件不包含 APK、私有目录、原始日志、数据库或 pcap。原始 pcap 留在服务器私有运行目录，仅经审计的计数与哈希进入证据包。

停止时没有测试或抓包 worker 在运行；Gateway `emulator-5554`（API 36）与 Host `emulator-5556`（API 29）均保持 booted，未冷启动。R3、R4、R5 的最终 head 未完成项交由后续代理按 `plan-0.4-joint-test.md` 与 `scripts/joint-validation-0.4-round2/README.md` 续跑。任何模拟器结果仍只代表 Android Telecom、真实 RTC 媒体栈和虚拟 modem；不声称真机蜂窝 HAL 音频通过。

## 续测（2026-10-22 12:00–13:05Z）：R2 修复、R3 未完成与实验室阻塞

本节由续测代理在同一最终 head 上追加。三端版本与上一节相同（Server 代码 `c6e6013`、Gateway `15e9ac7`、Host `f2680de`），直接复用实验机已有构建；未重新构建、未卸载、未清应用数据。上一节及更早轮次的 FAIL 原样保留，本节不把任何未完成项改判为 PASS。

### 续测起点

- 交接时 `emulator-5554`（API 36）与 `emulator-5556`（API 29）均 booted，无测试或抓包 worker。
- 安装包复核：Gateway base.apk `9aeef51e…f57dfb4`、Host base.apk `6d6b82a5…df3467aa`，与最终构建 manifest 一致；两端 instrumentation 重新启动并到 `READY`（`mapping_revision=9`，与只读 `sim_bindings/gateways` 一致）。

### R2 helper Broken pipe：根因、修复与最终 head 重跑

`idle-failover-hold-failback` 记录的最终 FAIL 不是行为失败，而是完成记录边界错误：

| 项 | 事实 |
| --- | --- |
| 触发 | `core-calls.py` 的 `pass_()` 先落盘 `PASS` 再向 stdout 打印摘要；读取端已关闭管道时 `print` 抛 `BrokenPipeError` |
| 放大 | `multi-node.run()` 的通用 `except` 捕获该异常，把已落盘的 PASS 改写成 `status=FAIL, error="[Errno 32] Broken pipe"` |
| 修复 | `pass_()` 先落盘再输出，捕获 `BrokenPipeError/OSError` 并把 stdout 指向 `/dev/null`（避免解释器退出刷新再次失败）；`multi-node.run()` 在通用失败分支前单独处理 `BrokenPipeError` |
| 回归 | `helper-epipe-regression.json`：闭合读端的子进程下，修复前 `exit=1`、记录 `FAIL [Errno 32] Broken pipe`；修复后 `exit=0`、记录 `PASS`（core-calls 修复前后 SHA-256 见该文件） |

最终 head 上两次独立完整重跑（未改产品、未放宽超时）：

| run | 结果 | 事实 |
| --- | --- | --- |
| `r2-resume-20261022T1211Z` | **PASS** | active_node=B；双向合成 PCM `12.81 s`；实际 Telecom **1 通**；账本 1 行 `completed`、占用槽 0；服务端 `ended|cellular_ended|rtc|f`；A 恢复后仍保持 B（`consecutiveSuccesses>=3`）；挂断后空闲回切 A |
| `r2-gate-20261022T1229Z` | **PASS** | `13.28 s`；Telecom 1 通；拨号就绪 `8.69 s`（期间两次 `dial_disabled:正在检查服务器通话能力…`）；其余断言同上 |

### R3 窗口：两次尝试均未走完（不构成 R3 PASS）

两次都使用新的 run 目录与新的 capture phase，未覆盖既有证据；两次的抓包收尾审计本身通过。

| 尝试 | run / phase | 已通过 | 中断点 |
| --- | --- | --- | --- |
| 第一次 | `r3-r4-resume-20261022T1217Z` / `r3-all-candidate-resume-20261022T1220Z`（12:18:47–12:24:29Z） | 抓包启动、短信收发（inbound `received`、outbound `delivered`，两端口径与归档均匹配）、R1 三通（95.76 / 96.40 / 11.32 s，Telecom 各 1 通）、R2 partial-node **PASS** | R2 idle-failover `failed_before_active` |
| 第二次 | `r3-r4-resume2-20261022T1234Z` / `r3-all-candidate-resume2-20261022T1240Z`（12:35:09–12:40:07Z） | preflight（三端版本/APK SHA/服务端能力）、抓包启动、短信收发、R1 三通（96.34 / 96.31 / 10.73 s，Telecom 各 1 通） | R2 partial-node `B_exclusion_not_evidenced`（后续 R2 idle-failover、重启、R5、空闲均未执行） |

两次窗口的最终审计（`*-negative-audit.json`）均为 PASS：三桥 manifest 匹配、只对实际 tcpdump 子进程 SIGINT、pcap flush/可读/哈希、无 netem、无故障 iptables、无 Asterisk，A/B/C 的 5060、5061、8088 计数**全部为 0**。这只证明该时段，不证明完整 R3。

两次中断的定性（证据见对应 evidence）：

1. 第一次的 R2 idle-failover：故障后约 9 s 拨号，Host 在本地拒绝（`dial_disabled:远程网关当前离线`），A/B/C 都没有新建 `call_sessions` 行；网关直到 12:24:37 才记录 `CONTROL: NETWORK_ERROR` 与 `MESH: healthy secondary keeps control polling at 30s`，即故障窗口短于产品 30 s 控制轮询节拍。属接线时序问题，不是通话行为失败。
2. 第二次的 R2 partial-node：网关日志为 `event=incoming_offered nodes=2`（B 在 offer 前被排除），没有 `incoming_offer_failed`；C 仍 `answered_elsewhere`、A 接通 `14.09 s`。产品对已知不可达节点选择"不 offer"而不是"尝试后失败"，原断言只接受后者，属断言过窄。

### 实验室阻塞：已接通通话在控制过期后不可恢复（本次中断的原因）

第二次窗口在 `B_exclusion` 断言处中断时通话已接通但未执行挂断，留下通话 `8aab9419-fe70-4350-95e3-6abc295bb38b`：

- A（`12:39:41` 创建，incoming，媒体 active）在控制过期后为 `unknown|control_expired`、`end_requested=t`、`media cellular_active=t`、winner 已设置、slot 占用=t；
- 网关 SQLite 中 `call_ledger` 为 `unknown/incoming`、`gateway_call_slots` 占用=1。

该状态无法由产品自身或既有 fixture 解除：

| 尝试 | 结果 |
| --- | --- |
| final APK 的 `recover_idle_call` | `IllegalStateException:unknown_op`（最终 fixture 无此 op） |
| 旧 recovery fixture（androidTest SHA-256 `ad9c6814e5de…e2201f24`） | `recovery_precondition_or_operation_failed` |
| host / gateway 的 `hangup`（带 `call_id`）、`reject`、`clear` | `hangup` 返回 OK 但不终结；`reject` 返回命令失败；`clear` 不改变服务端与槽状态 |
| force-stop 后重启 instrumentation | 不变 |
| 整机 `adb reboot`（`sys.boot_completed=1`） | 不变 |

实测后果：新的去电在授权阶段被拒（Host `FAILED`、`runtime_status=通话服务暂不可用，请刷新服务器状态后重试`、`call_id=pending`），`12:39:45` 之后 A 没有新建任何 `call_sessions` 行；来电不再振铃（`emu gsm call` 返回 OK、`emu gsm list` 仅 `OK`、Host 20 s 内无 `INCOMING_RINGING`）。

判定：本例由测试在断言处中断触发，但"已接通的通话收不到任何终态信号"在正常使用中同样可达（网关 App 被系统杀死 / OOM / 电池优化暂停同步——产品设置页自身提示电池优化会暂停同步；断电等），最终 APK 没有恢复路径且跨 App 重启与整机重启都不消失，等于该网关此后既不能去电也不能来电，除非清应用数据。按用户指示，"正常使用可能出现"的失败即中断测试并记录，因此本轮**不删除该通话、不改数据库、不对实验机做清理动作**；实验室在该状态下无法继续 R3/R4。

### R4 / R5 状态

- R4（三端同步回退再前滚）**未执行**：受上节阻塞。上一轮"回退后入站短信注入失败"的复现与定性需要在旧版本状态下重跑短信路径与通话 smoke，本轮无法安全进行；也没有为旧版本添加任何兼容代码。
- R5 在最终 head 的正式截图**未采集**。`r5-sms-ui.py` 在实验机上做过一次 dry-run（浅/深色记录页含短信行、详情弹窗字段完整、`uimode` 恢复为 `no`），dry-run 写私有目录，**不算本轮 R5 证据**。

### 本轮 helper 变更（`scripts/joint-validation-0.4-round2/`）

| 文件 | 变更 | 原因 |
| --- | --- | --- |
| `core-calls.py` | `pass_()` 先落盘并容忍 EPIPE；`outgoing()` 增加有界拨号就绪闸门（`dial_disabled` 的能力探测与"网关未就绪"两类拒绝都重试，记录 `dial_disabled_reasons`、`dial_ready_seconds`，上限 20 s / 120 s） | 完成记录不被输出管道改写；故障切换后不等网关在目标节点上线就拨号会误报 |
| `multi-node.py` | `BrokenPipeError` 完成边界；B 排除证据接受"尝试失败"或"offer 前排除（nodes=2，C answered_elsewhere）"；失败路径补挂断 | 同上，并避免再次产生遗留通话 |
| `round2-sync.py` | 新增 `--r3-phase`，R3 phase 不再硬编码 `r3-all-candidate-final` | 新 run 必须使用新 phase |
| `r3-window.py`（新增） | 单窗口编排：preflight 记录三端版本/APK SHA/服务端能力 → 抓包 → 短信 → R1 → R2（含 B 扰动后的 settle）→ 两端重启 → R5 → ≥15 min 空闲 → 收尾审计；失败也会收尾抓包 | 让一个窗口可完整覆盖 R3，且中断时留下有界证据 |
| `r5-sms-ui.py`（新增） | 真实 `input tap/swipe` 的 R5 采集：记录页浅/深色 + 短信详情弹窗，缺短信行即 FAIL | R5 需要可重复、可验收的采集 |
| `r4-sms-injection-diagnose.py`（新增，未执行） | 旧版本入站短信注入的分层判定：模拟器控制台应答 → 网关本地归档 → 三节点 messages → logcat → SMS provider | R4 需先判断"旧版产品问题"还是"模拟器/注入脚本问题" |
| `SHA256SUMS` | 同步更新 | — |

### 证据位置

本轮证据根目录（远程，未打包、未提交仓库）：

- `/home/ubuntu/gsm-webrtc-validation/joint-0.4/round2/r2-resume-20261022T1211Z/`
- `/home/ubuntu/gsm-webrtc-validation/joint-0.4/round2/r2-gate-20261022T1229Z/`
- `/home/ubuntu/gsm-webrtc-validation/joint-0.4/round2/r3-r4-resume-20261022T1217Z/`
- `/home/ubuntu/gsm-webrtc-validation/joint-0.4/round2/r3-r4-resume2-20261022T1234Z/`
- `/home/ubuntu/gsm-webrtc-validation/joint-0.4/round2/r2-verify-20261022T1247Z/`（helper 验证与遗留通话只读诊断，含 `orphan-recovery-*` 失败记录）

### 本节结论

| 计划组 | 状态 |
| --- | --- |
| R1 | 最终 head 在两个 R3 窗口内各通过三通（Telecom 各 1 通、双向 PCM 增长、账本 `completed`、槽释放）；窗口未完成，不作为 R3 通过 |
| R2 | Broken pipe 修复后在最终 head **完整重跑 PASS ×2**；窗口内两次失败已定性为接线/断言问题，原证据保留 |
| R3 | **INCOMPLETE**：两次窗口都在 R2 阶段中断；两次抓包审计通过（A/B/C 5060、5061、8088 全 0），但没有覆盖"短信 + R1 + R2 + 两端各重启 + R5 + ≥15 min 空闲"的完整窗口 |
| R4 | **NOT RUN**（实验室被遗留通话阻塞） |
| R5 | **NOT RUN**（最终 head 无正式证据） |

## 第三轮续测（2026-10-22 13:40–15:20Z）：b6dfcea 遗留通话修复与 R3 完整窗口

### 被测版本与构建

| 端 | 版本 | 说明 |
| --- | --- | --- |
| Gateway (Homura) | `b6dfcea`（代码） | APK `versionName=0.4.0-dev`、`versionCode=1000000000`、sha256 `b478dd62…`；构建只传了 `GSM_RELEASE_VERSION_CODE`，未传 `GSM_RELEASE_VERSION_NAME`（影响见 R4 一节） |
| Host (Madoka) | `f2680de` | `0.4.0-joint-round2`，沿用上一轮候选构建 |
| Server | `c6e6013` | 未改动 |

- 构建必须在 `gsm2sip-gateway-lab` 仓库进行，并加 `-PrtcLabRunner=com.callagent.lab.GatewayJointInstrumentation`；产品仓库 `gsm2sip` 的 androidTest 没有联测 fixture。
- 环境事故：构建期间 `/`（30 GB）被写满，5554 模拟器进程被杀，恢复约 45 分钟；冷启动后虚拟 SIM 指纹丢失，按既有 `explicit_same_virtual_modem_confirmation_after_fingerprintless_AVD_reboot` 流程重确认（`mapping_revision` 10→11）。这是实验室容量问题（SDK 6.5 G、AVD 3.2 G、Docker 镜像 3.2 G、历史 run 1.7 G），与产品无关。

### R0：遗留通话释放 — **FAIL（时限未满足），产品行为有效**

- 14:05:45.9 执行 `apps.py start`，14:05:58 READY；脚本在 **58.7 s** 读到 `call_ledger=unknown / rtc_cellular_slot=1 / A=unknown|control_expired`，判 FAIL（`evidence/r0-legacy-unknown-call-release.json`，`samples=[]`，首版采样未生效）。
- 日志 `10-22 14:06:57.299 … call_id=8aab9419-fe70-4350-95e3-6abc295bb38b event=unknown_call_released nodes=3`：释放发生在 READY 后 **59 s**、启动后 **72 s**。
- 释放后状态：账本 `failed`、`rtc_cellular_slot=0`、`rtc_control_terminals=0`、A `ended|cellular_ended`。
- 随后 `quick-outgoing-r0` 13.43 s（PCM）、`quick-incoming-r0` 12.81 s 均 PASS，各 1 通 Telecom，服务端 `ended|cellular_ended|rtc|f`。

结论：修复路径与终态都正确，但"60 秒内"按 `app_start` 计时未满足（差约 1 s），本节按 FAIL 记录。若要 PASS 口径，需要先明确计时起点（launch / app_start / 首次 reconcile tick）。

### R0b：force-stop 后保持蜂窝、再挂断 — 首轮 **FAIL（harness 缺陷）**，修正后 **PASS**

首轮（`r0-recovery.py` 初版）：force-stop 后执行 `emu gsm cancel`，60.8 s 未释放，报 `not_released_after_cancel`。定性证据：

- `dumpsys telecom` 仍显示 `Call id=TC@15 … state=ACTIVE`、`tpac=ComponentInfo{com.android.phone/com.android.services.telephony.TelephonyConnectionService}`；
- 模拟器控制台 `gsm cancel` 只撤掉模拟器侧通话，已接通的 Telecom 通话记录保持 ACTIVE，产品"蜂窝仍在则不释放"的判断正确，不是产品缺陷。

修正：`end_cellular()` = `emu gsm cancel` + `input keyevent KEYCODE_ENDCALL`，0.1–0.4 s 内 Telecom 转 `DISCONNECTING/DISCONNECTED`。

复跑结果（`evidence/r0b-force-stop-cellular-held.json`）：

- 保持蜂窝 61.2 s 期间 `released_lines=[]`，账本、服务端、槽三项检查均未变化（**不释放 ✓**）；
- ENDCALL 后 **9.4 s** 释放：账本 `failed`、槽 0、terminals 0、A `ended|cellular_ended`；
- 随访去电、来电均 PASS。

过程事故：第一版复跑证据被后续 `cp joint-0.4/evidence/r0*.json` 覆盖（旧文件盖掉 run 内新文件），已重跑补录；规则：证据只落一处，禁止跨目录通配符覆盖。

### R0c：先挂断再启动 — **PASS**

force-stop → ENDCALL（0.1 s，`TC@7 DISCONNECTING`）→ `apps.py start`；启动后 **8.9 s** 释放，账本 `failed`、槽 0、A `ended|cellular_ended`；随访去电 13.43 s、来电 12.81 s PASS。

### R3 完整窗口 — **PASS**

run `r3-round3-20261022T1340Z`，phase `r3-all-candidate-round3-20261022T1433Z`。

| 步骤 | 结果 | 关键数据 |
| --- | --- | --- |
| preflight | PASS | APK sha256、包名、服务端身份 |
| 短信采集 | PASS | 入站、出站各一条 |
| R1 去电 95 s | PASS | PCM 96.27 s，Telecom 1 通，A 承载 |
| R1 来电 95 s | PASS | PCM 96.32 s，Telecom 1 通 |
| R1 对端挂断 | PASS | PCM 11.40 s，Telecom 1 通 |
| R2 partial node | PASS | 13.05 s，B 排除路径有证据 |
| R2 idle failover / hold / failback | PASS | 12.82 s，切到 B（10.203.78.2） |
| 两端各重启一次 | PASS | Gateway、Host |
| R5 短信行与详情截图 | PASS | 见下 |
| 空闲 900 s | PASS | — |
| 收尾审计 | PASS | A/B/C 5060、5061、8088 全 0；pcap flush + sha256；无 Asterisk、无 netem、无 iptables |

R5：`evidence/r5-sms-ui-r3-all-candidate-round3-20261022T1433Z/` 浅色、深色记录页均含「短信 · 正文已清除」行，详情弹窗为 `Incoming / 22/10/2026 14:32:24 / 0 chars, 1 part`。首轮截图没把短信行框进画面，已改为滚动居中后重拍并替换正式证据（旧图保留在 `private/r5-first-capture/`）。

版本身份注记：本窗口的 Gateway APK `versionName` 是 `0.4.0-dev`（同 commit `b6dfcea`），与 `0.4.0-joint-round2` 只差版本字符串；若要求按发布身份出证据，需用 `GSM_RELEASE_VERSION_NAME=0.4.0-joint-round2` 重建后重跑窗口。

### R4 — 未完成（前置条件不满足，非产品失败）

`round2-sync.py --execute --r3-phase … --diagnose-baseline` 连续 4 次在 `initial-all-candidate_version_matrix_mismatch` 退出，证据保留为 `evidence/r4-synchronized-20261022T150412Z…`、`…150753Z…`、`…150811Z…`、`…151228Z…`：

- 期望矩阵要求 Gateway `0.4.0-joint-round2`，实际 `0.4.0-dev`；
- 三端回退/前滚与 `r4-sms-injection-diagnose.py` 均未开始执行。

处置：用 `GSM_RELEASE_VERSION_CODE=1000000000 GSM_RELEASE_VERSION_NAME=0.4.0-joint-round2` 重建同 commit 的 Homura 并 `install -r`（保留数据）后重跑 R4；诊断仍在回退阶段先跑，若判定为旧版产品问题立即停下报告，不为旧版加兼容代码。

### helper 变更（随本节提交）

- 新增 `r0-recovery.py`：覆盖 R0/R0b/R0c，`end_cellular()` 用 `KEYCODE_ENDCALL` 真正结束 Telecom 通话，证据写 run 内 `evidence/`。
- `r5-sms-ui.py`：`reveal_sms_row()` 先把短信行滚动到屏幕中部再截图。
- `round2-sync.py`：新增 `--diagnose-baseline`（回退状态先跑短信注入诊断），`--r3-phase` 校验 R3 审计后才会换版本。
- `SHA256SUMS` 同步更新。

### 时间消耗（供排期参考）

- 构建与环境恢复约 75 分钟：磁盘写满 45、重新构建约 20、AVD/虚拟 SIM 重确认约 10。
- R0b 假失败排查约 40 分钟（`emu gsm cancel` 语义）。
- R0/R0b/R0c 三次用例 + R3 完整窗口 + R4 前置失败约 50 分钟。

## 第三轮续测（续）：R4 三端同步回退 — **FAIL（回退阶段被 SIM 绑定闸门阻塞）**，旧版短信注入问题定性为 **harness 缺陷**

run `r3-round3-20261022T1340Z`，正式记录 `evidence/r4-synchronized-20261022T154618Z-fd878fbf.json`。

### 先修掉的构建问题

上一版 APK 的 `versionName=0.4.0-dev` 被 `rollback-cases.VERSION_NAMES` 拒绝（只接受 `0.4.0-joint` / `0.4.0-joint-round2`），此前 4 次 R4 因此停在 `initial-all-candidate_version_matrix_mismatch`。本轮在 `build-homura-lab-b6dfcea` @ `b6dfcea` 重编：

- `GSM_RELEASE_VERSION_CODE=1000000000 GSM_RELEASE_VERSION_NAME=0.4.0-joint-round2`，`-PrtcLabRunner=com.callagent.lab.GatewayJointInstrumentation`
- `app-debug.apk` sha256 `de455bc1b2c168bc72831b22c2232371668b92d44febaec444b8c295de3e03fa`，versionCode `1000000000`
- `install -r -g` 保留数据；旧包留存为 `gateway-debug.apk.0.4.0-dev.bak`，`manifest.json` 已更新

### R4 实际执行到哪一步

| 步骤 | 结果 |
| --- | --- |
| `sms-retained-initial-all-candidate` | PASS |
| `server-baseline`（A/B/C 降到 baseline） | PASS |
| `sms-retained-baseline-server` | PASS |
| `app-gateway-baseline`（Gateway 换 0.3.0-joint） | PASS |
| `start-gateway`（baseline） | PASS |
| `sms-retained-baseline-gateway` | PASS |
| Host 回退前的 `require_idle()` | **FAIL** `pinned_gateway_binding`：`sim_bindings.state='unverified'`、`identity_verified=false` |

快照 `initial-all-candidate` / `baseline-server` / `baseline-gateway` 均为 13；`idle_confirmed=false`；`recovery_attempts[0].reason=RuntimeError`。未做任何 SQL 篡改、未卸载、未清数据、未重启模拟器。

### 阻塞根因：回退装包会作废保留的 SIM 绑定

- 这台 AVD 无指纹，每次对 `com.callagent.gateway` 做 `install -r`，应用都会重评 SIM 身份；服务端把固定 SIM 行置为 `sim_bindings.state='unverified'`、`identity_verified=false`，`gateways.mapping_revision` 递增（本轮 13→14→15→16）。
- `round2-sync` 的 `pinned_gateway_binding()` 要求 `state='active' AND identity_verified`，所以 Host 回退前的 `require_idle()` 直接失败。
- 恢复是产品原生的、需要本地确认：
  - 候选版本 test runner 支持 `reconfirm_retained_emulator_sim` + `reconfirm_operation_id`，本轮用它恢复了 3 次（rev 13/15/16，SIM `9d581e56-…`）；
  - **0.3.0-joint-baseline 的 test runner 没有这个分支**，其 UI 只能确认服务端新提议的 SIM（`0fbb04f1-… pending_local_confirmation`），确认即会替换固定 SIM id、破坏本联测的固定身份。
- 结论：在这套 lab 上，R4 的"三端全回退"阶段可达性取决于 0.3.0 是否有操作员 SIM 确认路径，或 harness 是否提供跳过换包的步骤。这需要产品/测试设计侧决定，不是本轮 harness 能自行绕过的。

### 上一轮"回退后入站短信注入失败"的定性：**harness 缺陷，非 0.3.0 产品问题**

`r4-sms-injection-diagnose.py` 发现并就地修复两个自身缺陷（原文件保留 `*.orig-20261022T1554Z`）：

1. `adb()` 在 `sudo` 下以 root 运行；root 的 adb server 对 `adb emu …` 一律回 `KO: unknown command`，导致注入永远被记成 "rejected"。修法：`euid==0` 时用 `sudo -n -u ubuntu -H` 调 adb。
2. `docker_psql()` 把 psql 的命令标签也当行计数；节点 B 不带 `-q` 会输出 `BEGIN/COMMIT`，于是产生假的 `server_hit` 与错误的 `server_before` 计数。修法：过滤 `BEGIN/COMMIT/SET`。

修复前后对比（baseline gateway 状态）：

| 时间 | verdict | accepted | archive_hit | server_hit |
| --- | --- | --- | --- | --- |
| 15:52（修复前） | `harness_emulator_console_rejected` | false | false | true（假阳） |
| 15:56（修复后） | `injection_ok_both_layers` | true | true | true |

证据：`evidence/r4-sms-injection-diagnose-baseline-20261022T1556Z.json`。verdict 不是 `product_*`，按既定判定继续；即上一轮"回退后入站短信注入失败"是注入脚本/环境问题，旧版产品无此项缺陷。

### 前滚（候选态）实测

恢复到全候选后手工测量（server A/B/C `c6e6013`、gateway `0.4.0-joint-round2`、host `0.4.0-joint-round2`）：

| 项 | 结果 |
| --- | --- |
| 短信收发 `r4-candidate` | PASS：出站送达、入站收到、Gateway 归档 1/1、Host 历史 1/1、`live_session_count=0` |
| 去电 12 s | PASS：PCM 12.82 s，Telecom 1 通，Host `ENDED`，账本 `completed`，服务端 `ended|cellular_ended|rtc|f` |
| 来电 12 s | PASS：PCM 12.87 s，ring 延迟 4.22 s，Telecom 1 通，终态同上 |

证据：`evidence/round2-sms-capture-20261022T155507857498Z.json`、`evidence/r4-candidate-outgoing.json`、`evidence/r4-candidate-incoming.json`。

### R4 结论

- 产品层面：前滚态短信、去电、来电全 PASS；回退态在 baseline gateway 上短信注入 `accepted/archive/server` 三层均命中，**未复现旧版产品缺陷**。
- 流程层面：R4 的"三端全回退"未走完，卡在 0.3.0 换包后 SIM 绑定失效且无操作员确认入口。因此 R4 记为 **FAIL（流程阻塞）**，不记为产品失败。
- 收尾状态：全候选、Gateway `0.4.0-joint-round2` READY（SIM `9d581e56-…`，rev 16）、Host `0.4.0-joint-round2` READY、bridge IDLE、host phase ENDED/listening。
- 待产品/测试设计决定：0.3.0 是否提供 SIM 本地确认路径，或 harness 是否改为跳过换包；确定后 R4 可重跑（当前 lab 状态即为可重跑起点）。
