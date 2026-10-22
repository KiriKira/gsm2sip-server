# 0.4.1 计划：节点加入体验与 FCM 唤醒

状态：已记录，未开始。三端同步发布，沿用固定 versionCode `1000000000`。

## 0. 命名：项目统一叫 mahogsm

项目已不依赖 SIP，整体名称改为 **mahogsm**（三端仍是 SoulGem / Homura / Madoka）。0.4.1 起新增的标识一律用 mahogsm，
不再出现 gsm2sip：链接 scheme、管理命令、文档标题、新的环境变量与日志前缀。

为了直接覆盖升级与回退，下列已发布的标识保持不变：Android 包名 `com.callagent.host` / `com.callagent.gateway`、
Magisk 模块 id `sip-gsm-gateway`、数据库名与用户、数据卷与 `/data` 布局。仓库名和镜像名
（`ghcr.io/kirikira/gsm2sip*`）是否改名、何时改名单独决定；若改名，镜像在过渡期按两个名字同时发布。

## 起因（2026-10-22 新节点部署）

在新节点上先让 Homura 加入，25 秒后再在 Madoka 添加该节点，Madoka 报"该服务器上没有同一台旧手机"并回滚配对。
两台手机属于同一个 owner。原因是时序：Homura 加入节点后只完成配对，要到下一轮节点同步（约 30 秒，退避时更久）
才向新节点心跳上报 `install_id`；Madoka 添加节点时只查一次 `/v1/gateways`，看不到 `install_id` 就判定不是同一台旧手机。
过一会儿重试即成功，服务器数据不需要处理（失败那次留下的 client 设备行会话已撤销，无影响）。

另一个痛点：同一个节点要为两部手机各生成一个配对码，还要先 `owner create`，多节点时再用
`scripts/mesh-join-link.py` 拼链接。

## 1. 修复加入时序（必须）

- **Homura**：`MeshPairing.add` 成功后立即对该节点做一次同步（心跳带 `install_id`、SIM 状态），不等控制循环；
  "已同步"只在这次心跳成功后显示。
- **Madoka**：`HostNodes.add` 在新节点上看不到同一 `install_id` 时，不立即失败，在有界时间内（约 45 秒）重试
  `/v1/gateways`，界面显示"等待旧手机加入该服务器…"；超时再失败，并区分"旧手机还没在该服务器上报身份"和
  "该服务器上是另一台旧手机"两种提示。
- 回归测试：旧手机加入后立即添加主机节点成功；`install_id` 延迟出现时等待后成功；不同 `install_id` 仍拒绝。

## 2. 一个配对码给两部手机

- 服务器：配对码新增 `role='any'`，在有效期内**网关和主机各可兑换一次**（同一角色第二次兑换拒绝）。
  兑换时角色由请求里的 `role` 决定（现在的配对码角色由码本身决定，请求里的 `role` 被忽略）。
  - 迁移只做加法：放宽 `pairing_codes.role` 的 CHECK，加按角色记录兑换的表或列；不改已有行。
  - 回退兼容：0.4.0 服务器看到 `any` 码时角色不匹配，按拒绝处理，失败即关闭，不会误配。
  - 现有按角色的码保留，旧流程不变。
- `mahogsm-admin pairing-code create`（管理命令改名，见第 0 节）不带 `--role` 时生成 `any` 码，默认有效期缩短到 15 分钟。
- 两个 App 兑换时带上自己的角色（Homura=`gateway`，Madoka=`client`）。

## 3. 链接与二维码

- 链接改为 `mahogsm://join`（两个 App 注册新 scheme，旧的 `gsm2sip://join` 不再保留），不再带 `role`，每个节点一项 `URL#CODE`；
  两个 App 扫同一个码，各自以自己的角色加入。未配对时第一个节点作为主节点，其余加为节点；已配对时全部加为节点。
- 服务端生成临时二维码：`mahogsm-admin join create --url https://公网地址 [--ttl 15m] [--name Home]`
  - 没有 owner 时自动创建（单用户部署不再需要先 `owner create`）；已有唯一 owner 时直接使用；有多个时要求 `--owner-id`。
  - 输出链接和终端二维码（ANSI 半块字符，`docker exec -it <容器> mahogsm-admin join create ...` 直接扫），可选 `--png` 写文件。
  - 服务器不知道自己的公网地址，所以 `--url` 必填，也可由环境变量 `PUBLIC_API_URL` 提供默认值。
  - 多节点：`--also https://vps.example.com=CODE` 把其他节点各自生成的码并入同一个链接；`scripts/mesh-join-link.py` 同步改为生成新链接。
- 链接在码用掉或过期前属于机密，二维码只在终端显示，不写日志。

## 4. 顺序与提示

- 推荐流程写进 `docs/deploy-images.md` 和 `docs/mesh.md`：新节点 `join create` → Homura 扫码 → Madoka 扫码。
  第 1 节的等待让主机先扫也能成功（会等到旧手机加入）。
- Homura、Madoka 加入失败时提示具体原因：码过期、该角色已用过、服务器不支持多节点、等待旧手机超时。

## 5. FCM 唤醒（Madoka）

现在主机靠 WSS 唤醒推送加 8 秒空闲轮询收来电和短信。GMS 设备改用 FCM 高优先级消息唤醒，降低耗电并提高 Doze 下来电的及时性。

- 已就绪：Firebase 项目，Android 应用包名 `com.callagent.host`；`google-services.json` 以 base64 存在
  `KiriKira/gsm2sip-client-android` 的 Actions secret `GOOGLE_SERVICES_JSON_BASE64`。
- 待提供：Firebase 服务账号 JSON（项目设置 → 服务账号 → 生成新的私钥），放到**每台**服务器节点上，例如
  `/opt/mahogsm/fcm-service-account.json`（权限 600），由 compose 只读挂入容器，`FCM_SERVICE_ACCOUNT_FILE` 指向它。
  推送令牌属于这个 Firebase 项目，所以所有节点都要用同一个服务账号。
- **SoulGem**：
  - 新表保存推送令牌（主机设备 × 令牌，按设备覆盖、撤销设备时删除），新接口 `PUT /v1/devices/self/push-token`。
  - 来电振铃、新短信事件时，向该 owner 的主机发高优先级 data 消息：只含唤醒类型和事件游标，不含号码、正文或通话 ID，
    主机收到后照常走 HTTPS 拉取。同一主机短时间内的多次唤醒合并发送。
  - 用服务账号签 JWT 换取访问令牌，直接调用 FCM HTTP v1，不引入 Firebase SDK；令牌失效（`UNREGISTERED`）时删除。
  - 未配置 `FCM_SERVICE_ACCOUNT_FILE` 时完全不启用，行为与 0.4.0 相同；`/v1/capabilities` 报告是否支持推送。
- **Madoka**：
  - 接入 `firebase-messaging`；构建时有 `GOOGLE_SERVICES_JSON_BASE64` 才启用 FCM，没有（例如 fork 或 PR 构建）就构建无 FCM 版本。
  - 拿到令牌后向每个已配对节点注册，令牌轮换时重新注册。
  - 收到消息后调用现有的唤醒入口（`requestCallSyncFromWakeHint` 与短信同步），不在消息里处理业务。
  - 没有 GMS 或节点不支持推送时，保持 WSS 加轮询；有 FCM 时把空闲轮询放宽，具体间隔按实测定。
- **Homura** 不改：旧手机常驻前台服务，不需要推送。
- 验收：单测；Codex 在带 Google Play 的模拟器镜像上测熄屏和 Doze（`dumpsys deviceidle force-idle`）下来电响铃时延与短信通知；
  无 GMS 镜像上行为与 0.4.0 一致；服务器未配置凭据时不发推送。

## 6. 验收

- 三端单测与 CI；Codex 联测：新节点只用一个链接或二维码完成两部手机加入（先网关、先主机两种顺序）；
  码过期、同一角色重复兑换被拒；旧的按角色码仍可用；把服务器回退到 0.4.0 后 `any` 码被拒、其余功能正常。
