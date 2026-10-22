# 用预构建镜像部署 SoulGem

服务器上不编译任何东西，只拉取镜像运行，支持 amd64 与 arm64。0.4 起通话只走 WebRTC：
不再需要 Asterisk、SIP 端口、SIP 证书或 ARI 密码。

| 镜像 | 内容 |
|---|---|
| `ghcr.io/kirikira/gsm2sip` | **一体镜像**：PostgreSQL 17、TURN 中继、API 与 worker，一个容器一个数据卷 |
| `ghcr.io/kirikira/gsm2sip-server` | API、worker、管理 CLI（distroless，静态二进制） |

镜像名沿用旧名，升级和回退不用改部署命令。标签：`main`（main 分支最新构建）、`sha-<7位提交>`
（固定某次构建）、版本标签 `vX.Y.Z` 与 `latest`。每次推送前，CI 都在对应架构上启动一体镜像
（含一次重启和中继进程退出）和 [deploy/compose.yaml](../deploy/compose.yaml)，检查 `/readyz`、
`/v1/capabilities`、TURN 与管理 CLI。

## 最简单：一体镜像

家里或 Tailscale 内的节点，不用填任何地址：

```sh
docker run -d --name gsm2sip --restart unless-stopped --network host \
  -v gsm2sip-data:/data ghcr.io/kirikira/gsm2sip:main
```

两部手机里填 `http://<这台机器的局域网 IP、Tailscale IP 或主机名>:8080`。

- 首次启动自动生成数据库密码与 `SECRETS_ENCRYPTION_KEY`，保存在数据卷的 `/data/secrets/`。
  备份时备份整个数据卷，并把 `docker exec gsm2sip cat /data/secrets/secrets_encryption_key` 另存一份。
  也可以用 `-e SECRETS_ENCRYPTION_KEY=...` 自己提供，但之后不能再更换。
- 容器自带 TURN 中继：TURN 密钥由安装密钥派生，监听 `0.0.0.0:3478`（UDP/TCP），中继端口 49160–49259/udp，
  对外地址自动探测。云安全组/路由器要放行这些端口；端口可用 `TURN_LISTEN_PORT`、`TURN_RELAY_MIN_PORT`、
  `TURN_RELAY_MAX_PORT` 修改。探测不到公网地址时设 `PUBLIC_IP`（或 `TURN_PUBLIC_IP`）。
- 已有外部 TURN 时设 `RTC_TURN_URLS` 与 `RTC_TURN_SHARED_SECRET`，容器就不再启动自带中继。
- 其他端口：`HTTP_PORT`（默认 8080）；内部 PostgreSQL（`PG_PORT`，默认 55432）只监听 127.0.0.1。

不用 `--network host`（例如 `-p` 端口映射）时，容器看不到宿主机网卡，要设 `PUBLIC_IP` 为手机访问的地址，
并且 TURN 端口映射两侧必须相同：

```sh
docker run -d --name gsm2sip --restart unless-stopped \
  -v gsm2sip-data:/data -p 127.0.0.1:8080:8080 \
  -p 3478:3478/udp -p 3478:3478/tcp -p 49160-49259:49160-49259/udp \
  -e PUBLIC_IP=203.0.113.10 \
  ghcr.io/kirikira/gsm2sip:main
```

明文 `http://` 时短信和令牌不加密（通话音频始终是 DTLS-SRTP），只在你信任的网络里使用；对公网提供服务时
在前面放 HTTPS 反向代理。

- 管理命令：`docker exec gsm2sip mahogsm-admin owner create --name Home`，配对码同下。
- 升级：`docker pull ghcr.io/kirikira/gsm2sip:main`，然后删除并用同样参数重建容器（数据在卷里）。
- 从 0.4 之前升级：删掉旧的 `SIP_*`、`ARI_*`、`RTC_MEDIA_ENABLED`、`RTC_CONTROL_ENABLED` 参数和 SIP/RTP 端口映射
  （留着也只会在日志里提示被忽略），加上 TURN 端口。回退见 [release-0.4.md](release-0.4.md)。

## 分镜像：Docker Compose

```sh
mkdir -p /opt/gsm2sip && cd /opt/gsm2sip
curl -fsSLO https://raw.githubusercontent.com/KiriKira/gsm2sip-server/main/deploy/compose.yaml
curl -fsSL https://raw.githubusercontent.com/KiriKira/gsm2sip-server/main/deploy/env.example -o .env
chmod 600 .env
```

按 `.env` 里的注释填写：

- `POSTGRES_PASSWORD`：`openssl rand -hex 24`。
- `SECRETS_ENCRYPTION_KEY`：`openssl rand -base64 32`，生成后不能再改，和数据库一起备份。
- 通话需要两部手机都能到达的 TURN：`RTC_TURN_URLS`、`RTC_TURN_SHARED_SECRET`，以及 TURN 中继的
  `TURN_PUBLIC_IP`/`TURN_PRIVATE_IP`/`TURN_REALM`。用同一个 `.env` 启动 [deploy/turn](../deploy/turn/compose.yaml)：
  `docker compose --env-file .env -f turn/compose.yaml up -d`（Linux 主机网络）。

启动并检查：

```sh
docker compose up -d
curl http://127.0.0.1:8080/readyz
curl http://127.0.0.1:8080/v1/capabilities     # 应包含 "mesh" 和 "rtc_call_control": true
```

API 默认只监听 `127.0.0.1:8080`，由 HTTPS 反向代理对外提供；要让手机在局域网或 Tailscale 内用
`http://` 直连，设 `HTTP_BIND_ADDR=0.0.0.0`（或局域网/Tailscale IP）。

### 让两部手机加入

一体镜像把下面的 `$A` 换成 `docker exec -it gsm2sip mahogsm-admin`。

```sh
A="docker compose run --rm --no-deps --entrypoint /usr/local/bin/mahogsm-admin api"
$A join create --url https://手机访问这台服务器的地址
```

命令打印一个 `mahogsm://join` 链接和终端二维码（深色终端背景下扫）：旧手机（Homura）和主手机（Madoka）各扫一次，
各自以自己的身份加入；同一个码每种手机只能用一次，默认 15 分钟过期（`--ttl` 1m–1h）。服务器还没有账户时自动创建
（`--name` 设名称），只有一个账户时直接使用，有多个时要 `--owner-id`。也可设环境变量 `PUBLIC_API_URL` 代替 `--url`。

多节点：先在其他节点上各运行一次 `join create`，再在优先级最高的节点上把它们并进同一个二维码：
`$A join create --url https://a.example.com --also https://b.example.com=B节点的join_code`。
已配对的手机扫码后把其余节点加为节点。链接和二维码在码用掉或过期前属于机密。

仍可用 `owner create` 与 `pairing-code create --role gateway|client` 为单部手机生成配对码。

### 推送唤醒（FCM，可选）

把 Firebase 服务账号 JSON 放到服务器上，只读挂入容器，并设 `FCM_SERVICE_ACCOUNT_FILE` 为容器内路径：

- 一体镜像：文件权限 600 即可（容器启动时以 root 读取后交给 API），compose 里加
  `volumes: ["/opt/mahogsm/fcm-service-account.json:/run/secrets/fcm.json:ro"]` 与
  `environment: {FCM_SERVICE_ACCOUNT_FILE: /run/secrets/fcm.json}`；`docker run` 用相同的 `-v` 和 `-e`。
- 分镜像：API 以 nonroot（uid 65532）运行，先 `chown 65532:65532` 并 `chmod 400` 这个文件，再在 api 服务里加同样的挂载，
  `.env` 里设 `FCM_SERVICE_ACCOUNT_FILE=/run/secrets/fcm.json`。

所有节点用同一个服务账号。启动日志出现 `push wake-ups enabled` 即生效，`/v1/capabilities` 的 `push` 为 `true`；
文件缺失或密钥错误时只记录错误并关闭推送，主手机照常使用原来的唤醒通道，不会多耗电。

### 升级与维护

- 升级：`docker compose pull && docker compose up -d`。要固定版本，把 `GSM2SIP_VERSION` 设为 `sha-xxxxxxx` 或 `v0.4.0`。
- 从 0.4 之前升级：换成新的 `compose.yaml` 后执行 `docker compose up -d --remove-orphans`，停掉旧的 asterisk 容器；
  数据库原样保留。
- 不要执行 `docker compose down -v`，它会删除数据库卷。备份见 [operations.md](operations.md)。
