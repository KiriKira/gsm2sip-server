# 0.4.1 联测（简版）

三端用 `claude/charming-franklin-8piri8` 最新 head 构建（同一 debug keystore，`GSM_RELEASE_VERSION_CODE=1000000000`）。
Madoka 构建前把 Firebase 配置放到 `app/google-services.json`（用户提供，不提交），并设 `MAHOGSM_PUSH_RELAY_URL` 为用户部署的
Kyubey 地址；推送用例的主手机用带 Google Play 的模拟器镜像。服务器不需要推送配置。只记 PASS/FAIL 与关键证据，不做抓包审计。

| # | 用例 | 判定 |
| --- | --- | --- |
| T1 | 前提：Homura 模拟器装有 Magisk 模块（`dumpsys package com.callagent.gateway` 显示 `CAPTURE_AUDIO_OUTPUT: granted=true`），Homura 是默认拨号应用，且升级前后台运行已启用、通话控制已在运行（`shared_prefs/gateway.xml` 里 `autoconnect=true`；用户点过停止就不会自动恢复，这是预期行为）。不满足则 T1 记 N/A 并跳过，不记 FAIL。从 0.4.0 覆盖升级三端（服务器先）；之后整机重启一次 Homura 所在模拟器 | 升级后与重启后 Homura 都无需点“诊断并启动”，状态页显示通话控制就绪；去电、来电、收发短信各一次正常 |
| T2 | 新开一个空服务器节点：`mahogsm-admin join create --url ...`，Homura 扫码后 **10 秒内** Madoka 扫同一个码 | 两端都加入；Madoka 节点列表出现该节点；同一角色再扫一次被拒 |
| T3 | Madoka 熄屏并 `dumpsys deviceidle force-idle` | 服务器 `push_endpoints` 有该设备；Doze 中来电 10 秒内响铃，新短信有通知 |
| T4 | 在 T3 的 Madoka 上 `adb install -r` 覆盖一个空中继地址的构建（保留数据），打开 Madoka | 1 分钟内 `push_endpoints` 无该设备（日志 `push=withdrawn`）；通话与短信照常，Madoka 日志无反复登记 |
| T5 | 推送正常，但在服务器主机防火墙临时拦截 API 端口后打来电 | 12 秒左右 Madoka 出现“有来电，但连不上服务器”；解除拦截后正常 |

模拟器没有摄像头时，用 `VIEW` intent 代替扫码，整条命令交给设备 shell，链接用单引号包住（否则 `&` 会截断链接）：

```
adb -s <serial> shell "am start -a android.intent.action.VIEW -d 'mahogsm://join?v=2&n=...'"
```

加入失败时额外记录：`adb logcat -d | grep 'dat=mahogsm'` 中链接的长度（码只用一次，可记原文），以及 Homura 设置页里的设备名。

正常使用中可能出现的失败照旧中断并记录。结果写入 `docs/release-0.4.1-validation-<日期>.md`。
