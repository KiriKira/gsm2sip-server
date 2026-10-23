# 0.4.1 联测验证（2026-10-23）

| 测项 | 结果 | 关键证据 |
| --- | --- | --- |
| T1 | FAIL（基线条件不完整，升级后断言未通过） | 服务端先升级到 `sha-4ee9118` 并返回 `ready`。Homura 以 `adb install -r` 覆盖升级至 `0.4.1-dev`，未卸载；升级后状态页为“未运行”，仍显示“诊断并启动”。没有点击该按钮。截图 SHA-256：`5da6c2864fceb9d68995fb4ed63a16ba4d4a6c21ef0b44a9ff5e719cf26320ae`。检查到升级前保存的 `autoconnect=false`，且模拟器 `/data/adb/modules` 不存在，`CAPTURE_AUDIO_OUTPUT` 未授予；因此这个模拟器没有处于可验证自动恢复的已启用语音基线，不能据此认定为 0.4.1 回归。未执行重启或通话断言；之后转测独立的 T2。 |
| T2 | FAIL | 在空节点 `http://10.0.2.2:18099` 用 `mahogsm-admin join create` 创建一个码。Homura 0.4.1 打开该加入链接并确认后，界面返回 `未添加 (Pairing code, device name, or platform is invalid.)`；服务器 `join_codes` 中该码的 `gateway_redeemed_at` 与 `client_redeemed_at` 均为空。截图 SHA-256：`e1c2e98924d77fd96e0f48637fddee7d869433835e6182883ef7c29f87eeaa98`。模拟器没有摄像头扫码输入，本次以 Android `VIEW` intent 注入同一 `mahogsm://join` 链接；未到 Madoka 加入步骤。 |
| T3 | NOT RUN | 在 T2 的 Homura 加入请求失败后按计划停止。 |
| T4 | NOT RUN | 在 T2 的 Homura 加入请求失败后按计划停止。 |
| T5 | NOT RUN | 在 T2 的 Homura 加入请求失败后按计划停止。 |

## 被测构建

- SoulGem：服务器分支 `claude/charming-franklin-8piri8`，`4ee9118a0a0fa06d17db94538f74e352cf2f4f50`；镜像 `ghcr.io/kirikira/gsm2sip:sha-4ee9118`，本机镜像 ID `sha256:7b17f85e632bb2bf38134af57d442ec06239d99daac4c6f59656563ad939d540`。升级后 `/readyz` 为 `{"status":"ready"}`。旧数据卷升级前已备份，实验机路径 `/home/ubuntu/gsm-webrtc-validation/round3-041/pre-041-data-volume.tar.gz`，SHA-256 `94542d62b45daf85e97dd6ec0a9094b5b49b57b7d365bd0ad17dd0f59e6aa6cf`。
- Homura：网关分支 head `a353f4d9e6cd84cd626088f478307aa208ec8282`，APK SHA-256 `eddf1605af569227a11f6de9c436a3d4bcf54e3f788704fd85e8c433b3e7d116`，versionCode `1000000000`。
- Madoka：主机分支 head `a20d0b4923bd2f4dcdb0ec170df3f7b2c29bdbfc`。临时 CI run [39902835740](https://github.com/KiriKira/gsm2sip-client-android/actions/runs/39902835740) 成功，使用 Secret 写入 Firebase 配置并设中继地址；APK versionCode `1000000000`。为 API 29 x86_64 模拟器另构建并安装了不带 Firebase 配置及中继地址的 debug APK（SHA-256 `360177c766d5a4676f8777eb18156df1669b547b866b12919f71e85ed1f6a4ef`，versionCode `1000000000`）；本轮在 T2 失败后未将其用于加入尝试。两个客户端构建的证书 SHA-256 均为 `2c613d0386c319b54abd14796fee004ea44681bfe3b69ddd33f7062cd3845f28`。

临时 CI 分支与 GitHub artifact 已删除；Firebase JSON 未提交。T2 按正常使用流程出现失败后停止本轮测试；其余未运行用例没有宣称通过。

## 第二轮重跑

本轮使用独立空节点 D 和以下最新 head 重跑；上面的第一轮结果及产物记录保留不变。

| 测项 | 结果 | 关键证据 |
| --- | --- | --- |
| T1 | N/A | 升级前 `gateway.xml` 为 `autoconnect=false`、`background_opted_in=false`、`background_user_stopped=true`、`service_running=false`；`/data/adb/modules` 不存在，`CAPTURE_AUDIO_OUTPUT` 未授予。不满足计划中的已启用语音及 Magisk 前提，按计划跳过。 |
| T2 | PASS | 用同一枚实际加入码，通过完整 Android `VIEW` intent 加入 D。Homura 先打开链接，Madoka 在 112 ms 后打开；两端均确认，确认操作相差 131 ms。D 的 `join_codes` 建码时间为 `2026-10-23T07:02:05.385587Z`，`gateway_redeemed_at=2026-10-23T07:02:31.567973Z`、`client_redeemed_at=2026-10-23T07:02:31.412121Z`，兑换相差约 156 ms。D 当前 gateway/client 各一台。 |
| T3 | FAIL（推送登记阻断） | FCM Madoka 的保存 token 存在，但 endpoint 不存在；D 的 `push_endpoints` 数量为 0。`HostPush` 曾记录 `push=refused`，本地偏好保存拒绝状态及 24 小时退避。使用保存 token 对配置的 `https://kyubey.openkiri.zip/v1/register` 做诊断请求，返回 HTTP 403 Cloudflare JavaScript challenge，未得到 endpoint。未执行 `force-idle`，未发送测试来电，也未验证 Doze 短信通知。 |
| T4 | NOT RUN | T3 的正常推送登记流程失败后，按计划停止本轮测试。 |
| T5 | NOT RUN | 同上，未执行 API 阻断或模拟推送。 |

T2 使用的命令形式如下，两台设备分别注入同一条完整链接；链接整体由设备 shell 的单引号保护，避免 `&` 被截断：

```sh
adb -s <serial> shell "am start -a android.intent.action.VIEW -d '<同一完整加入链接>'"
```

另有一枚 `2026-10-23T06:59:58.629058Z` 创建的预检码，两角色均未兑换且已失效，不计入上述计时尝试。报告不保存 token、endpoint、二维码或一次性加入码。上述 403 是诊断请求的实测结果；本轮未捕获此前 `push=refused` 请求的原始响应，因此不把两者写成已证实的同一次失败。

本轮被测构建：

- SoulGem：`d7d8a2d53c589d81d4ec3a93eca43dcf8cc2628b`；D 使用镜像 `ghcr.io/kirikira/gsm2sip:sha-d7d8a2d`，已核对 digest `sha256:324611776659c8c4364a33891b6aa7497c8176abebc58cc1b55bc6ea34bae58c` 及 revision。
- Homura：`29109c2c68389c096a750256e2266aaaf6c277b0`；APK SHA-256：`16b80519be271a6a4a607c7f29396880d3ac2161c5c2df7a55a62f5e580d589d`。
- Madoka：`75af4b7c35bb9885308a1052b81de3d7c50b81a1`；带 Firebase 配置和 Kyubey 地址的 FCM APK，临时 CI run [39913412209](https://github.com/KiriKira/gsm2sip-client-android/actions/runs/39913412209)，APK SHA-256：`8008b7b11b0e0570b6a5e5e9889c51503f98da2619e816fdaec0939d166bbd9c`。本轮用于带 Google Play 服务的 API 29 模拟器。

两个 Android APK 使用同一 debug keystore，versionCode 均为 `1000000000`。本轮在 T3 阻断后停止，没有把未执行的 Doze、通话、短信或故障提示断言计为通过。

## 第三轮重跑（2026-10-23）

按本分支最新版 `docs/plan-0.4.1-joint-test.md` 继续验证 T3–T5；T1/T2 未重跑，沿用第二轮结论。D 升级为 `sha-f67071f` 后为 healthy，保留 D 的持久数据；本轮使用仅连接 D 的额外 synthetic gateway 身份，通过生产 API 生成事件，不直接写数据库，也不操作 A/B/C。

| 测项 | 结果 | 关键证据 |
| --- | --- | --- |
| T1 | N/A（沿用，未重跑） | 第二轮已确认升级前后台运行、语音及 Magisk 前提不满足，按计划跳过；本轮不重新作升级或重启断言。 |
| T2 | PASS（沿用，未重跑） | 沿用第二轮同一码、两角色成功加入的计时证据；不宣称在 `f67071f` 上重新执行过 T2。 |
| T3 | PASS（synthetic gateway 推送通知范围） | D 已登记 Madoka 的 `push_endpoints` 1 行。Madoka 屏幕为 OFF，`deviceidle mState=IDLE`；生产 incoming API 返回 HTTP 201。`call_id=c57c5ca7-c96d-4e61-9f73-d28b7b420e2e`，AVD 记录 `GsmRtc event=control_incoming` 与 `HostCall Incoming call ringing`，请求至 ringing log 实测约 0.3 秒（宿主与模拟器时钟差约 1.05 秒）。通知 ID `7401`、channel `incoming-calls-ringing`，带接听/拒接 actions；之后 call cleanup 为 `ended`。唯一标记短信 `T3-SMS-20261023-0809` 经生产 `events:batch` 返回 HTTP 200，约 1.9 秒内出现 `host-inbound-sms-content` 通知；D 推送 cursor 成功推进，`failures=0`。 |
| T4 | FAIL（覆盖空中继构建后旧登记仍在） | 同一 Madoka、同一 D client `de9cbed4-5ac6-420e-b4d5-3240cb5cd881`，以 `adb install -r` 从 FCM APK 覆盖为空 relay APK，保留数据，未人工删除 endpoint 或换新身份。正常打开并等待 12 秒后，该设备的 `push_endpoints` 仍为 1；该观察窗口 `HostPush` 日志为 0 行，服务仍 active。不满足“`push_endpoints` 无该设备”的判定，按计划停止，未执行 T4 后续通话/短信断言。 |
| T5 | NOT RUN | T4 正常覆盖切换流程出现失败后，按计划停止；未执行 API 阻断及其恢复断言。 |

T3 的 PASS 范围为 **synthetic gateway → D → Kyubey → FCM → Madoka Doze 来电/短信通知**：生产 API 建立真实 incoming call 和 SMS event，再由服务器正常推送。它不代表 Homura 蜂窝接入、蜂窝/HAL 音频或扬声器实际播放铃声已验证。

本轮预检中，实验机指定 curl 的旧版本不支持 `%{header{cf-mitigated}}`；改用 `-D` 读取响应头后，实测 HTTP 404，`cf-mitigated` 为空，本轮预检未出现 403。此结果不改写第二轮已经记录的 403 失败。

本轮构建与覆盖安装证据：

- SoulGem：`f67071f4646fdf78233bf80b8af5d721db8c1061`，镜像 `ghcr.io/kirikira/gsm2sip:sha-f67071f`；多架构 digest `sha256:5af3461af364f3171d30d59cd51a040997ce59b56c042281aeb70265d03105fa`。镜像发布 run [39920753113](https://github.com/KiriKira/gsm2sip-server/actions/runs/39920753113) 的 amd64/arm64 smoke 及发布均成功，两个架构的 revision 均与该完整提交一致。
- Madoka：`75af4b7c35bb9885308a1052b81de3d7c50b81a1`。T3 的 FCM APK SHA-256 为 `8008b7b11b0e0570b6a5e5e9889c51503f98da2619e816fdaec0939d166bbd9c`；T4 覆盖安装的空 relay APK SHA-256 为 `c9d15d3cfa62c24d4778d71dc9402c51b80524610acd3eb7734cff4ad05e387f`。两者 versionCode 均为 `1000000000`，签名证书 SHA-256 均为 `2c613d0386c319b54abd14796fee004ea44681bfe3b69ddd33f7062cd3845f28`。

T4 私有证据保存在实验机 `private-r3/t4-transition.txt`。测试后已恢复上述 FCM APK（SHA-256 `8008b7b11b0e0570b6a5e5e9889c51503f98da2619e816fdaec0939d166bbd9c`），D 中该设备的 endpoint 登记仍保留 1 行。报告不包含私人 token、endpoint URL 或一次性配对码。

## 第四轮（2026-10-23）

本轮按计划提交 `d9cc4d75e2609518eed88f15acf0b46c3b9ec6cb` 的 `docs/plan-0.4.1-joint-test.md` 验证 T4–T6。T1 N/A、T2/T3 PASS 沿用前轮证据，本轮未重跑；其中 T3 仍限于第三轮注明的 synthetic gateway 推送通知范围，不代表新 APK 已重跑 T3 或验证蜂窝/HAL 音频。

| 测项 | 结果 | 关键证据 |
| --- | --- | --- |
| T1 | N/A（沿用，未重跑） | 既有模拟器不满足已启用语音及 Magisk 前提，沿用第二轮判定。 |
| T2 | PASS（沿用，未重跑） | 沿用第二轮同一码、两角色成功加入的计时证据。 |
| T3 | PASS（沿用，未重跑） | 沿用第三轮 D-only synthetic gateway → D → Kyubey → FCM → Madoka Doze 来电/短信通知证据及其范围限制。 |
| T4 | FAIL（推送撤销子项 PASS，后续正常使用失败） | 同一 Madoka 保留数据，以 `adb install -r` 安装 FCM APK 后，D 中该设备的 `push_endpoints` 数量为 1；再以 `adb install -r` 覆盖空 relay APK后数量为 0。设备日志为 `10-23 09:03:07.556 I/HostPush(21209): node 993544863 push=withdrawn`，满足 1 分钟内撤销登记。后续普通通话/短信验证前，Homura UI 显示“未运行”、后台同步已停止、SIM 确认待处理；诊断提示系统通话音频采集需要 Magisk，属于既有 T1 环境限制。普通 SMS 注入已被 gateway 收到并 journaled，但同步停止且 SIM mapping 待确认，不能计为端到端短信通过。随后按 UI 选择 SIM 并点击确认，正常使用流程返回 `SIM 确认失败：SIM_CONFIRM_FAILED`，触发中断规则；T4 的通话/短信正常运行判定未完成，整体不计 PASS。 |
| T5 | NOT RUN | T4 正常 SIM 确认流程失败后按计划停止；未执行 API 阻断、约 12 秒来电提醒或解除阻断后的恢复验证。 |
| T6 | NOT RUN | 同一中断规则；未执行不接来电、Madoka 飞行模式下的来电、主叫回铃保持或恢复网络后的下一通来电验证。 |

本轮被测构建：

- SoulGem：D 仍使用 `ghcr.io/kirikira/gsm2sip:sha-f67071f`，代码提交 `f67071f4646fdf78233bf80b8af5d721db8c1061`；没有把计划文档提交当作已部署服务器镜像。
- Homura：`51e9a8298e6ca04e7f72af9356d9320fec8bbd6b`；最终 APK SHA-256：`84b3b58e020858d52c7321037d8bc92e941c806801b37674afa374f0b1437504`。
- Madoka：`105c4697ff4732287659bf3742dad79268e62927`，包含推送撤销修复 `ecd689faf551e4b48a6621129c570ea37af55046`；FCM 与空 relay APK 均由该提交构建。FCM 最终 APK SHA-256：`ca048089a28dc5cb3c1daf51d4953cc93c0887e4fe540f2e7818fff1d9f15eea`；空 relay 最终 APK SHA-256：`e71fff93b3bd674a2593194141ed4b6685d26e9f227ae41bf90d3e9760267855`。

三个最终 Android APK 使用同一 debug keystore，versionCode 均为 `1000000000`，签名证书 SHA-256 均为 `2c613d0386c319b54abd14796fee004ea44681bfe3b69ddd33f7062cd3845f28`。本轮在 `SIM_CONFIRM_FAILED` 后停止，没有继续实验机测试；未把已 journaled 的 SMS、已修复的推送撤销子项或未运行的后续用例扩大记为整体通过。报告不记录私人 endpoint URL、token 或一次性配对码。

## 第五轮补测（2026-10-23）

按计划提交 `985b1d3c4cfb1a77f877e882784cd8e88b5aeaef` 补测：已知实验机限制记 `BLOCKED（环境）`，只中断依赖该限制的用例；不依赖 Homura 的 T5 可用只连接 D 的 synthetic gateway 通过生产 API 独立验证。前四轮原始结果保留不变；T1/T2/T3 本轮未重跑。

| 测项 | 结果 | 关键证据 |
| --- | --- | --- |
| T1 | N/A（沿用，未重跑） | 沿用既有模拟器不满足语音及 Magisk 前提的判定。 |
| T2 | PASS（沿用，未重跑） | 沿用第二轮同一码、两角色加入的计时证据。 |
| T3 | PASS（沿用，未重跑） | 沿用第三轮 synthetic gateway → D → Kyubey → FCM → Madoka Doze 通知证据及其范围限制。 |
| T4 | BLOCKED（环境；撤销子项沿用 PASS） | 推送撤销子项沿用第四轮的 `push=withdrawn` 及该设备登记由 1 降为 0 的 PASS。补测正常通话/短信需要 Homura 恢复运行，但新 Homura 已安装、启动并尝试确认 SIM 后，界面原文为 `SIM 确认失败：SIM_MAPPING_CHANGED（Changed SIM binding requires a newer server revision）`；未能恢复，故通话/短信补测为环境阻塞，不计 PASS。 |
| T5 | PASS（独立验证通知提示） | Madoka 在 D 的 `push_endpoints` 中有 1 条登记，预检时 RTC 能力最近更新时间距检查约 11 秒。重建仅连接 D 的 synthetic gateway，SIM revision 为 1，heartbeat/capability 成功。服务器主机临时使用 OUTPUT + INPUT DROP 阻断 API 的 host 映射 `127.0.0.1:18100`；root curl 探测返回 exit `28`、HTTP `000`。阻断期间通过 D 容器私网 `172.17.0.2:8080` 调用同一生产 `/v1/rtc/control/incoming`，返回 HTTP 201。首次 UI 树检查在 API 请求后约 16.4 秒，主页面无提示文案，但系统通知已存在精确标题“有来电，但连不上服务器”及正文“网络恢复后可在通话记录中查看这通来电。”；按通知提示判定通过。 |
| T6 | BLOCKED（环境） | Homura 的同一 `SIM_MAPPING_CHANGED` 确认错误使运行及 SIM 前提未满足；未执行 T6 的来电、不接/断网时主叫回铃保持或恢复网络后下一通来电验证。 |

T5 验证的是 **synthetic gateway → D → Kyubey → FCM → Madoka 的 API 不可达通知提示**，没有执行 Homura 相关动作，不声称蜂窝/HAL 音频或扬声器实播。约 16.4 秒是首次 UI 树检查时点，不是通知首次出现时间；本次未测定精确的约 12 秒首现延迟。提示位于系统通知，不是应用主页面文案。

T5 结束后已恢复防火墙规则并确认无残留；synthetic call 已 `ended`，gateway capability 已 disabled、session 已 revoked，临时 credential 已删除。私有远端证据为 `/home/ubuntu/gsm-webrtc-validation/round5-20261023/private/t5-result.json` 与同目录 `t5-notification-evidence.txt`；本报告仅记录关键事实，不提交私有证据文件、token 或 endpoint URL。

本轮构建：D 仍使用 `ghcr.io/kirikira/gsm2sip:sha-f67071f`；Madoka 沿用第四轮 `105c4697ff4732287659bf3742dad79268e62927` 的 FCM APK，versionCode `1000000000`。Homura 使用 `6c1691b` debug APK，与既有构建同 keystore，versionCode `1000000000`；其 SIM 确认错误按上述环境阻塞记录。
