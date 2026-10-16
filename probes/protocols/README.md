# 本地 WebRTC 协议验收

独立 Go module，不向生产 API 引入 Pion 或启动 TURN。不修改生产配置、数据库或 Android UI。
它测试实际网络协议及实验 WSS handler；没有生产会话/呼叫协调器适配，不代表 WebRTC 已启用。

## 执行

需要 Go 1.24、Python 3、OpenSSL、可访问本机 socket 的 Docker；首次使用需要通过可信代理下载 Go 模块及固定镜像。
FFmpeg 只在重新生成原创音调 fixture 时需要，普通运行使用已固定的 fixture。

```sh
cd probes/protocols
python3 -m unittest test_runner.py
python3 run.py
```

运行器：

1. 校验固定的 Opus fixtures；保留 Go checksum database/go.sum 验证，拉取按 digest 固定的 coturn 4.6.3。
2. 使用正常系统/代理 CA 编译 race 测试二进制，随后只为测试子进程加入临时测试 CA；不修改系统证书库。
3. 创建没有外网路由、没有公开端口的内部 Docker 网络及两个 coturn：可信证书和另一 CA 的证书。
   临时生成 REST 密钥/凭据、证书和私钥，不复用生产秘密。以工作区 UID 运行，文件系统只读、no-new-privileges，
   只保留该固定镜像文件 capability 所要求的 NET_BIND_SERVICE。
4. 执行所有具名协议检查，必须全部有实际 pass，无失败/跳过。进程退出 0 或任意几个测试通过都不足以通过。
5. 保存不含凭据/SDP 的结果与测试事件，删除本次容器、网络、私钥和配置；清理失败也返回失败。

结果：`artifacts/result.json`、`artifacts/tests.jsonl`，测试二进制也在 artifacts，均被 Git 忽略。
成功退出 0，任何失败/前置条件缺失/检查不完整退出 1，不把缺少 Docker/TURN 当跳过。

## 覆盖的真实链路

- 两个真实 Pion PeerConnection，ICE + DTLS-SRTP，双向各至少 20 个解密后的 Opus RTP payload 与对端原始 fixture 一致。
- 直连及强制 TURN relay 的 UDP/TCP/TLS；同时断言两端选中的 local/remote candidate 都是 relay，不能回退 host 后冒充中继通过。
- 真实 STUN Binding 事务与 XOR-MAPPED-ADDRESS。
- 无效 TURN 密码、过期 TURN REST 凭据、不可信 TLS CA、TLS 主机名不匹配被拒绝。
- SDP 指纹被篡改：实际交换证书/建立 ICE 后明确拒绝对应 DTLS 身份，不把普通超时当安全检查通过。
  跨端重建 SessionDescription，避免 Pion 的本地 parsed 缓存让篡改测试失效；保留拒绝事件，避免瞬时 Failed 随后 Closed 被轮询漏掉。
- 使用主仓库的 WebSocketHandler/Service/reducer，通过真实、验证证书的 WSS 交换 SDP，建立双向媒体；
  ICE restart 更换 ICE 凭据并恢复媒体，业务 call_id/leg_id/attempt_id 不变，不新建 PeerConnection 或蜂窝呼叫。

音调 fixture 为自行生成的 1 kHz/1.5 kHz 正弦音，由 FFmpeg/libopus 编码并独立解码验证后固定哈希。
Pion 不解码音频：本测试验证 Opus 协商、有效已编码音频的传输和解密完整性，**不是 Android native PCM 验收**。
需要重新生成时执行 `python3 generate_fixtures.py`，检查工具版本和解码结果后单独审阅 pin 变更，不自动接受新哈希。

## 边界

当前 13 项（含子测试）真实协议检查和 3 项运行器证据检查通过。主仓库另有 PostgreSQL、WSS 鉴权/撤销/队列、
原 wake-only `/v1/ws` 和 HTTPS 业务回归。Android 命令 codec 使用同一套六份 fixtures，测试字节上限和严格解析。

本测试只使用本机及内部网络；结果明确保留 `android_native/cellular_audio/public_network_matrix=NOT_RUN`。
没有验证 Android WebRTC SDK、自定义 ADM、蜂窝 PCM、真实 SIM/IVR DTMF、公网运营商网络、防火墙穿透、
TURN 费用/容量、长通话凭据轮换或 24/72 小时运行。原有 SIP/RTP/DTMF 单元回归不能替代这些检查。

下一步可用有 ADB 的 64 位 Android 云手机运行网关仓库 `probes/webrtc/tools/run_probe.py`，
先检查 native SDK/双向合成 PCM/资源释放。真实蜂窝上下行及双 SIM 仍需具有对应蜂窝硬件和已验证 Magisk 音频路由的网关。
无影云手机不作为已支持蜂窝 PCM 的假设；具体系统、ABI、ADB 和权限能力到设备后读取验证。
