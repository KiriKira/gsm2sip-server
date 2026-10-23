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
