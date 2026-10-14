# 用预构建镜像部署

服务器上不编译任何东西，只拉取镜像运行。需要 Docker 和 Docker Compose v2，支持 amd64 与 arm64。

| 镜像 | 内容 |
|---|---|
| `ghcr.io/kirikira/gsm2sip-server` | API、worker、管理 CLI（distroless，静态二进制） |
| `ghcr.io/kirikira/gsm2sip-asterisk` | 固定版本的 Asterisk 22.11.0 与 gsm2sip 配置 |

标签：`main`（main 分支最新构建）、`sha-<7位提交>`（固定某次构建）、版本标签 `vX.Y.Z` 与 `latest`。
每次推送前，CI 都在对应架构上用 [deploy/compose.yaml](../deploy/compose.yaml) 启动整套服务并检查
`/readyz`、`/v1/capabilities`、Asterisk 健康状态与管理 CLI。

## 部署

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

API 只监听 `127.0.0.1:8080`，由你的 HTTPS 反向代理或 cloudflared 对外提供；两个 App 只接受 HTTPS。
ARI（8088）不对外开放。

## 账户与配对码

```sh
A="docker compose run --rm --no-deps --entrypoint /usr/local/bin/gsm2sip-admin api"
$A owner create --name Home
$A pairing-code create --owner-id OWNER_UUID --role gateway --ttl 60m
$A pairing-code create --owner-id OWNER_UUID --role client --ttl 60m
```

## 升级与维护

- 升级：`docker compose pull && docker compose up -d`。要固定版本，把 `GSM2SIP_VERSION` 设为 `sha-xxxxxxx`。
- 证书续期后执行 `docker compose restart asterisk`（Asterisk 只在启动时读取证书）。
- 不要执行 `docker compose down -v`，它会删除数据库卷。备份见 [operations.md](operations.md)。
