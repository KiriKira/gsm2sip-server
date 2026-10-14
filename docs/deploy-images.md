# 用预构建镜像部署

服务器上不编译任何东西，只拉取镜像运行，支持 amd64 与 arm64。

| 镜像 | 内容 |
|---|---|
| `ghcr.io/kirikira/gsm2sip` | **一体镜像**：PostgreSQL 17、Asterisk、API 与 worker，一个容器一个数据卷 |
| `ghcr.io/kirikira/gsm2sip-server` | API、worker、管理 CLI（distroless，静态二进制） |
| `ghcr.io/kirikira/gsm2sip-asterisk` | 固定版本的 Asterisk 22.11.0 与 gsm2sip 配置 |

标签：`main`（main 分支最新构建）、`sha-<7位提交>`（固定某次构建）、版本标签 `vX.Y.Z` 与 `latest`。
每次推送前，CI 都在对应架构上启动一体镜像（含一次重启）和 [deploy/compose.yaml](../deploy/compose.yaml)，
检查 `/readyz`、`/v1/capabilities`、Asterisk 健康状态与管理 CLI。

## 最简单：一体镜像

只收发短信的节点（例如经 cloudflared 对外）：

```sh
docker run -d --name gsm2sip --restart unless-stopped \
  -v gsm2sip-data:/data -p 127.0.0.1:8080:8080 \
  -e SIP_SERVER_NAME=sip.example.com \
  ghcr.io/kirikira/gsm2sip:main
```

家里或 Tailscale 内的节点可以不用证书：`SIP_TRANSPORT=udp` 用明文 SIP/RTP，API 直接用 `http://`。
`SIP_SERVER_NAME` 与 `PUBLIC_IP` 填两部手机都能访问到的地址（局域网 IP，或常开 Tailscale 时的 100.x 地址）：

```sh
docker run -d --name gsm2sip --restart unless-stopped \
  -v gsm2sip-data:/data \
  -p 8080:8080 -p 5060:5060/udp -p 10000-10199:10000-10199/udp \
  -e SIP_TRANSPORT=udp -e SIP_SERVER_NAME=192.168.1.10 -e PUBLIC_IP=192.168.1.10 \
  ghcr.io/kirikira/gsm2sip:main
```

两部手机里的服务器地址填 `http://192.168.1.10:8080`。明文模式下短信、令牌和通话音频都不加密，
只在你信任的网络里使用。切换 `SIP_TRANSPORT` 后，需要在两部手机上重新获取 SIP 配置（旧手机移除再添加该节点）。

对外提供通话的节点，再加公网 IP、SIP/RTP 端口和证书（证书的 SAN 必须包含 `SIP_SERVER_NAME`）：

```sh
docker run -d --name gsm2sip --restart unless-stopped \
  -v gsm2sip-data:/data -p 127.0.0.1:8080:8080 \
  -p 5061:5061/tcp -p 10000-10199:10000-10199/udp \
  -v /path/fullchain.pem:/run/secrets/sip_tls_cert:ro \
  -v /path/privkey.pem:/run/secrets/sip_tls_key:ro \
  -e SIP_SERVER_NAME=sip.example.com -e PUBLIC_IP=203.0.113.10 \
  ghcr.io/kirikira/gsm2sip:main
```

- 首次启动自动生成数据库密码与 `SECRETS_ENCRYPTION_KEY`，保存在数据卷的 `/data/secrets/`；
  没有挂载证书时自动生成自签 SIP 证书并下发给手机。备份时备份整个数据卷，并把
  `docker exec gsm2sip cat /data/secrets/secrets_encryption_key` 另存一份。
  也可以用 `-e SECRETS_ENCRYPTION_KEY=...` 自己提供，但之后不能再更换。
- 挂载的是公网可信证书时无需其他设置；挂载私有 CA 签发的证书时加 `-e SIP_CA_PEM_FILE=/run/secrets/sip_tls_cert`。
- 管理命令：`docker exec gsm2sip gsm2sip-admin owner create --name Home`，配对码同下。
- 升级：`docker pull ghcr.io/kirikira/gsm2sip:main`，然后删除并用同样参数重建容器（数据在卷里）。
- 证书续期后 `docker restart gsm2sip`。

## 分镜像：Docker Compose

### 部署

```sh
mkdir -p /opt/gsm2sip && cd /opt/gsm2sip
curl -fsSLO https://raw.githubusercontent.com/KiriKira/gsm2sip-server/main/deploy/compose.yaml
curl -fsSL https://raw.githubusercontent.com/KiriKira/gsm2sip-server/main/deploy/env.example -o .env
chmod 600 .env
```

按 `.env` 里的注释填写：

- `POSTGRES_PASSWORD`、`ARI_PASSWORD`：`openssl rand -hex 24`。
- `SECRETS_ENCRYPTION_KEY`：`openssl rand -base64 32`，生成后不能再改，和数据库一起备份。
- `SIP_SERVER_NAME` 与证书文件 `ASTERISK_TLS_CERT_FILE`/`ASTERISK_TLS_KEY_FILE`：证书的 SAN 必须包含该名字。
  公网可信证书时 `SIP_CA_PEM_FILE` 留空；自签证书时设为 `/run/secrets/sip_tls_cert`。
- 对外提供通话：`SIP_BIND_ADDR=0.0.0.0`，两个 `ASTERISK_EXTERNAL_*_ADDRESS` 填公网 IP，
  防火墙放行 5061/tcp 与 10000–10199/udp。只收发短信的节点（例如经 cloudflared 对外）保持默认的
  `127.0.0.1`，自签证书即可：

  ```sh
  openssl req -x509 -newkey rsa:2048 -nodes -days 3650 -keyout sip.key -out sip.crt \
    -subj /CN=sip.localhost -addext subjectAltName=DNS:sip.localhost
  ```

启动并检查：

```sh
docker compose up -d
curl http://127.0.0.1:8080/readyz
curl http://127.0.0.1:8080/v1/capabilities     # 输出应包含 "mesh"
```

API 默认只监听 `127.0.0.1:8080`，由反向代理或 cloudflared 对外提供；要让手机在局域网或 Tailscale 内用
`http://` 直连，设 `HTTP_BIND_ADDR=0.0.0.0`（或局域网/Tailscale IP）。不需要证书时设 `SIP_TRANSPORT=udp`、
`SIP_PORT=5060`，两个证书路径设为 `/dev/null`。ARI（8088）不对外开放。

### 账户与配对码

一体镜像把下面的 `$A` 换成 `docker exec gsm2sip gsm2sip-admin`。

```sh
A="docker compose run --rm --no-deps --entrypoint /usr/local/bin/gsm2sip-admin api"
$A owner create --name Home
$A pairing-code create --owner-id OWNER_UUID --role gateway --ttl 60m
$A pairing-code create --owner-id OWNER_UUID --role client --ttl 60m
```

### 升级与维护

- 升级：`docker compose pull && docker compose up -d`。要固定版本，把 `GSM2SIP_VERSION` 设为 `sha-xxxxxxx`。
- 证书续期后执行 `docker compose restart asterisk`（Asterisk 只在启动时读取证书）。
- 不要执行 `docker compose down -v`，它会删除数据库卷。备份见 [operations.md](operations.md)。
