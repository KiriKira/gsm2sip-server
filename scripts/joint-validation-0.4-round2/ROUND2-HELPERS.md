# 第二轮联测辅助脚本

这些是测试准备和执行工具，不是测试结果。默认 plan-only 的工具只有加
`--execute` 才开始写文件或操作实验环境。第一轮报告及证据保留原样。

## 独立目录

把本目录最新脚本放到实验机原 `joint-0.4/scripts/` 后，在实验机运行准备器。
不要用第一轮中途 APK；`--candidate-builds` 指向三个最终 head 重构建后的 APK 目录，
下面目录中的每个角色都应包含主 APK 与匹配的 androidTest APK。

```sh
python3 scripts/round2-prepare.py --run-id RUN_ID \
  --baseline-builds /path/to/builds/baseline \
  --candidate-builds /path/to/builds/final-candidate
# 同一命令追加 --execute 才创建目录；不启动测试。
```

准备器生成 `joint-0.4/round2/RUN_ID/`，复制脚本并把脚本里的固定实验根目录
改为这个新目录。仅必要的服务器 env 和身份引用会从原目录复制；
后续状态、新日志、抓包和证据只写新目录。第一轮短信 session/state、probe auth 与 label 均不会复制。
APK 目录通过只读用途的目录链接引用，不会复制或改动 APK。
准备器拒绝复用 run ID，也拒绝启用中的故障配置。

短信 capture 固定使用身份引用中的 gateway_id/sim_id；若存在合法的
`emulator-retained-sim-recovery.json`，严格使用它声明的同一身份，以其 revision 作为最低值。
正常冷启动可推进 revision；只读查询要求该精确绑定 active、verified、g/b 映射相等、
revision 不低于引用值且 Gateway 在线，并与原模拟器 joint-ready.json 的 READY revision 一致。
证据保存最低值、实际 observed revision 和 joint-ready/只读数据库来源；R4 同时拒绝
相对本轮已观测 revision 倒退。其他旧
Gateway 不影响选择；不会按最新时间选择其他网关。

第一轮短信状态无论是否 cleanup 都只保存路径、哈希和 cleanup 标志作为来源引用，不复制成 live 状态。
本轮必须独立 capture；准备器保留本轮 `private/sms-baseline-state.json` 不存在，允许
直接运行本轮 `sms-baseline.py capture`，随后执行 `round2-sms.py revoke --label initial --execute`。
推荐统一通过 `round2-sms.py capture --label r3-candidate --execute` 完成 capture、正常 API revoke 和只读留存核验。
`round2-preparation.json` 标记来自第一轮的身份引用，它们只用于身份对照。
原实验机的容器/网络/数据库由脚本继续使用，因此第二轮不能与其他实验并发。

进入新运行目录后，启动该目录中的 `logcat-supervisor.py host` 和 `gateway`，
让 `core-calls.py` 读取本轮自己的日志。两端 instrumentation 也必须处于 READY。
logcat supervisor 是持续运行的采集工具，不是 plan-only。

## R1 / R2 / R3 / R5

在三端均为最终 0.4、版本及 SHA 已记录后开始 R3；phase 名不能复用。

```sh
python3 scripts/round2-capture.py --phase r3-all-candidate
sudo -n python3 scripts/round2-capture.py --phase r3-all-candidate --execute
python3 scripts/round2-sms.py capture --label r3-candidate --execute
python3 scripts/round2-sms.py check-idle --execute
python3 scripts/round2-calls.py r1-outgoing-95s --execute
python3 scripts/round2-calls.py r1-incoming-95s --execute
python3 scripts/round2-calls.py r1-peer-hangup-10s --execute
python3 scripts/multi-node.py incoming-partial-node-unreachable
python3 scripts/multi-node.py idle-failover-hold-failback
```

R1 去电由 Host 挂断；来电由 Gateway 挂断；对方挂断用固定模拟号码的 modem
cancel。仅播放指定秒数 PCM，不附加 mute/DTMF。沿用真实 Telecom 次数、
双向 PCM 增长、Host `ENDED`、Gateway `completed` 账本、空槽断言。
Gateway 测试 status 不提供 `phase=ENDED` 字段，故证据以 bridge/Telecom/账本
原始字段记录，不虚构 App 状态字段。

R3 窗口还必须实际完成两端各一次重启、R5 短信行浅色/深色截图及短信详情，
以及至少 15 分钟空闲。这些动作没有被上述命令自动代替。
`multi-node.py` 是会直接执行的已有工具，不是 plan-only。恢复 A 后通话保持 B、
挂断后空闲回切的原有 90 秒截止不被放宽。

R3 全部覆盖完成后，先单独结束全 0.4 抓包，再开始 R4：

```sh
sudo -n python3 scripts/final-audit.py --root "$PWD" \
  --capture-phase r3-all-candidate --output-name r3-all-candidate-negative-audit.json
```

抓包 identity gate 与审计必须通过 `sudo -n python3` 以 root 执行，才能只读检查
root tcpdump 的 `/proc/<pid>/fd`。普通 ubuntu 身份虽然可能启动 tcpdump，
却无法完成身份确认，不能因此重复启动另一组抓包。R4 整体也用 sudo 执行，
让其内部抓包与审计继承 root 身份。

该审计直接执行，只向 manifest 对应且重新确认身份的实际 tcpdump 子进程发 SIGINT，
随后等待退出、读包计数并保存哈希；不按进程名杀进程。审计拒绝覆盖输出。
抓包开始证据与审计完成时间共同界定窗口；pcap 和 stderr 留在 private。
Docker 审计使用 `docker top -eo pid,comm`，只输出计数，不保存进程名。
审计/全部测试完成后，把本轮输出交回实验用户再下载打包；只对本轮目录执行：

```sh
sudo chown -R "$(id -u):$(id -g)" "$PWD/evidence" "$PWD/private/captures"
```

## R4

```sh
python3 scripts/round2-sync.py
sudo -n python3 scripts/round2-sync.py --execute
```

顺序固定为服务器 A/B/C → Gateway → Host 全部回旧，再按同一顺序全部回 0.4。
每一步检查版本、配对、SIM、节点列表，以及本轮已 capture 的全部短信对/归档。
后续保留校验通过只读服务器 SELECT 和两端 SQLite 完成，不依赖已撤销 token。
仅全旧和全新两个一致状态各创建独立临时 client，发送一次新收发短信，立即
通过正常 `POST /v1/auth/revoke` 撤销辅助 session，只读证明 live session=0，
并核对同 owner 的 live RTC client 聚合没有不兼容客户端，然后各测一次12秒去电和来电。
没有逐组件回退通话，也不执行旧 helper 的 sessions SQL 修改。每次调用
`round2-calls.py` 前也重复临时 session 与 owner RTC capability 只读门禁。
全旧阶段另起 `r4-all-baseline` 抓包并自动做 flush/count/hash 审计。
AIO 运行参数显式传入 `RTC_MEDIA_ENABLED=true`、`RTC_CONTROL_ENABLED=true`、
`SIP_COMPATIBILITY_ENABLED=false`；B compose 另加同值覆盖文件，数据卷不变。

任何失败都会停测。版本切换开始后，先确认空闲，才按服务器 → Gateway → Host
尝试恢复 0.4；无法确认空闲时拒绝改版本，输出实际 FAIL 等待处理。
正常 API revoke 或只读 inactive/owner RTC 校验失败就停止，不会带着活跃短信临时 session
进入来电 smoke，也不能用 SQL 过期 session 绕过。R4 不能在同一个运行目录重跑覆盖已有 phase/通话证据。

## 本地打包

把本轮 evidence 拉回本地项目内的独立目录；保留 `round2-preparation.json` 的
来源说明。可用已有媒体导出器的 `--remote-root` 指向本轮目录、输出到本轮
`evidence/media-logs`，不要沿用第一轮日志或输出目录。

```sh
python3 scripts/package-evidence.py \
  --evidence-root round2/RUN_ID/evidence \
  --output round2/RUN_ID/evidence-package.zip \
  --index round2/RUN_ID/evidence-package-index.json
```

这个模式只发现给定证据目录，跳过第一轮 artifacts 根摘要及 builds manifest。
保留原 JSON/XML/JSONL/PNG 结构与凭据检查、媒体白名单、ZIP CRC/内容哈希检查，
输出必须显式使用与第一轮不同的路径。默认拒绝覆盖 ZIP/index。
报告只能根据实际执行证据追加；这些 helper 的本地参数/模拟验证不构成 R1–R5 PASS。

## 续测新增工具与两个就绪闸门（2026-10-22）

- `r3-window.py --phase <phase> [--idle-seconds 900]`：一个窗口内按序执行 preflight（记录三端版本/APK SHA/服务端能力）→ 抓包 → 短信 → R1 → R2（B 扰动后 settle 20 s）→ Gateway/Host 各重启一次 → R5 → 空闲 → 收尾审计；任一步失败即停止，但仍会收尾抓包并把状态写入 `private/<phase>-status.json`。
- `r5-sms-ui.py --out DIR`：真实 `input tap/swipe` 采集记录页浅/深色与短信详情弹窗；找不到短信行或详情弹窗即 FAIL。
- `r4-sms-injection-diagnose.py`：旧版本入站短信注入的分层判定（模拟器控制台应答 → 网关本地归档 → 三节点 messages → logcat → SMS provider），只读且有明确 verdict；未在本轮执行。
- `round2-sync.py --r3-phase <phase>`：R3 phase 不再硬编码，R4 校验对应窗口的审计。
- `core-calls.py` 的拨号就绪闸门：`dial_disabled` 的两类拒绝（能力探测、网关尚未在目标节点上线）都会在有限时间内重试并记录 `dial_disabled_reasons`/`dial_ready_seconds`；不改变产品超时，也不放宽通话断言。
- `multi-node.py`：完成记录以证据文件为准（`BrokenPipeError` 不改写 PASS）；B 不可达的证据接受"尝试后失败"或"offer 前排除（`nodes=2` 且 C `answered_elsewhere`）"；失败路径补发挂断，避免留下占用槽的遗留通话。
