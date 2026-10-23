# 0.4.1 联测验证（2026-10-23）

| 测项 | 结果 | 关键证据 |
| --- | --- | --- |
| T1 | FAIL（首个升级后断言未通过；基线条件不完整） | 服务端先升级到 `sha-4ee9118` 并返回 `ready`。Homura 以 `adb install -r` 覆盖升级至 `0.4.1-dev`，未卸载；升级后状态页为“未运行”，仍显示“诊断并启动”。没有点击该按钮。截图 SHA-256：`5da6c2864fceb9d68995fb4ed63a16ba4d4a6c21ef0b44a9ff5e719cf26320ae`。检查到升级前保存的 `autoconnect=false`，且模拟器 `/data/adb/modules` 不存在，`CAPTURE_AUDIO_OUTPUT` 未授予；因此这个模拟器没有处于可验证自动恢复的已启用语音基线，不能据此认定为 0.4.1 回归。首个断言未通过后按计划停止，未执行重启。 |
| T2 | NOT RUN | 按计划在 T1 停止。 |
| T3 | NOT RUN | 按计划在 T1 停止。 |
| T4 | NOT RUN | 按计划在 T1 停止。 |
| T5 | NOT RUN | 按计划在 T1 停止。 |

## 被测构建

- SoulGem：服务器分支 `claude/charming-franklin-8piri8`，`4ee9118a0a0fa06d17db94538f74e352cf2f4f50`；镜像 `ghcr.io/kirikira/gsm2sip:sha-4ee9118`，本机镜像 ID `sha256:7b17f85e632bb2bf38134af57d442ec06239d99daac4c6f59656563ad939d540`。升级后 `/readyz` 为 `{"status":"ready"}`。旧数据卷升级前已备份，实验机路径 `/home/ubuntu/gsm-webrtc-validation/round3-041/pre-041-data-volume.tar.gz`，SHA-256 `94542d62b45daf85e97dd6ec0a9094b5b49b57b7d365bd0ad17dd0f59e6aa6cf`。
- Homura：网关分支 head `a353f4d9e6cd84cd626088f478307aa208ec8282`，APK SHA-256 `eddf1605af569227a11f6de9c436a3d4bcf54e3f788704fd85e8c433b3e7d116`，versionCode `1000000000`。
- Madoka：主机分支 head `a20d0b4923bd2f4dcdb0ec170df3f7b2c29bdbfc`。临时 CI run [39902835740](https://github.com/KiriKira/gsm2sip-client-android/actions/runs/39902835740) 成功，使用 Secret 写入 Firebase 配置并设中继地址；APK versionCode `1000000000`。另成功构建不带 Firebase 配置及中继地址的 T4 APK，但未安装。两个客户端构建的证书 SHA-256 均为 `2c613d0386c319b54abd14796fee004ea44681bfe3b69ddd33f7062cd3845f28`。

临时 CI 分支与 GitHub artifact 已删除；Firebase JSON 未提交。除 T1 记录外，没有宣称其余用例通过。
