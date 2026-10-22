# 0.4.1 联测（简版）

三端用 `claude/charming-franklin-8piri8` 最新 head 构建（同一 debug keystore，`GSM_RELEASE_VERSION_CODE=1000000000`）。
Madoka 构建前把 Firebase 配置放到 `app/google-services.json`（用户提供，不提交）；FCM 用例的主手机用带 Google Play 的模拟器镜像，
服务器挂载用户提供的服务账号 JSON。只记 PASS/FAIL 与关键证据，不做抓包审计。

| # | 用例 | 判定 |
| --- | --- | --- |
| T1 | 从 0.4.0 覆盖升级三端（服务器先）；之后整机重启一次 Homura 所在模拟器 | 升级后与重启后 Homura 都无需点“诊断并启动”，状态页显示通话控制就绪；去电、来电、收发短信各一次正常 |
| T2 | 新开一个空服务器节点：`mahogsm-admin join create --url ...`，Homura 扫码后 **10 秒内** Madoka 扫同一个码 | 两端都加入；Madoka 节点列表出现该节点；同一角色再扫一次被拒 |
| T3 | 节点配置正确的 FCM；Madoka 熄屏并 `dumpsys deviceidle force-idle` | 服务器 `push_tokens` 有该设备；Doze 中来电 10 秒内响铃，新短信有通知 |
| T4 | 同一节点改为不配 FCM、再改为错误的服务账号 | `push` 为 false，`push_tokens` 无新行；通话与短信照常，Madoka 日志无反复登记 |
| T5 | FCM 正常，但在服务器主机防火墙临时拦截 API 端口后打来电 | 12 秒左右 Madoka 出现“有来电，但连不上服务器”；解除拦截后正常 |

正常使用中可能出现的失败照旧中断并记录。结果写入 `docs/release-0.4.1-validation-<日期>.md`。
