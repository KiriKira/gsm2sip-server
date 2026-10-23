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
