# gsm2sip-server

Go + PostgreSQL 转发服务，把主机的短信任务投递到旧手机网关，并把网关的持久化事件安全同步回主机。M1/M2 核心已实现：一次性配对码、短时 access 与轮换 refresh token、device role/owner 隔离、SIM 两阶段绑定、heartbeat、短信 command claim、事件批量事务与 durable ACK、按 owner 分页的消息事件流。

WSS/FCM 唤醒、通知投递、长期保留与完整背压仍待后续实施；当前三端使用 HTTPS 轮询。

通话控制与 Asterisk ARI 仍未实现。SIP 配置明确返回 `available:false`；`/calls` 与 `/call-intents` 返回 `503 CALLING_NOT_READY`。部署骨架中没有声称 Asterisk 已联调。

网关后续按 Magisk 通用能力接口实现账户映射和数字音频适配，取消机型白名单及 API 31 整体语音门槛。此适配不改变本仓库的通话 readiness；详见实施审查中的 Magisk 补充。

- [服务端计划](PLAN.md)
- [原始协议](docs/protocol-v1.md)
- [服务端 wire addendum](docs/server-wire-addendum.md)
- [OpenAPI](openapi/openapi.yaml)
- [跨端路线图](docs/roadmap.md)
- [实施审查记录](docs/IMPLEMENTATION-REVIEW-2026-10-03.md)

## 本地运行

需要 Docker Compose v2。开发 Compose 将 PostgreSQL 仅放在内部网络，用 Caddy 把 API 映射到 `127.0.0.1:8080`。数据库密码仅用于本地演示；把 `.env.example` 复制为 `.env` 并换成自己的本地密码：

```sh
cp .env.example .env
docker compose up --build -d
curl http://127.0.0.1:8080/healthz
curl http://127.0.0.1:8080/readyz
```

创建 owner，再分别生成给旧手机 gateway 和主机 client 的短时配对码：

```sh
docker compose run --rm --no-deps --entrypoint /usr/local/bin/gsm2sip-admin api \
  owner create --name "Home"

docker compose run --rm --no-deps --entrypoint /usr/local/bin/gsm2sip-admin api \
  pairing-code create --owner-id OWNER_UUID --role gateway --ttl 10m

docker compose run --rm --no-deps --entrypoint /usr/local/bin/gsm2sip-admin api \
  pairing-code create --owner-id OWNER_UUID --role client --ttl 10m
```

CLI 只会在创建配对码时显示一次明文 code；服务端只保存其 SHA-256。把 gateway code 输入旧手机，把 client code 输入主机。两端配对后会通过 HTTPS 完成 SIM 本地确认和短信任务；配对码的作用时间由 `--ttl` 限制。不要把 `.env` 或配对码提交到版本库。

`docker compose logs -f api worker` 查看本地服务日志。`docker compose down` 保留本地数据库卷；`docker compose down -v` 会删除它。

## 本地 Go 开发

服务端要求 Go 1.24 或更新版本，PostgreSQL 17。API、worker 和 admin CLI 启动时会安全地串行应用 SQL migrations；数据库连接启用 `synchronous_commit=on`，网关事件只有在事务提交后才获得 ACK。

```sh
export DATABASE_URL='postgresql://USER:PASSWORD@127.0.0.1:5432/gsm2sip?sslmode=disable'
go test ./...
go run ./cmd/api
```

运行 `go run ./cmd/worker` 启动未领取短信任务的 TTL 清理器；运行 `go run ./cmd/admin owner create --name Home` 或 `go run ./cmd/admin pairing-code create --owner-id UUID --role client` 管理 owner 与一次性配对码。

PostgreSQL 集成测试会在 `TEST_DATABASE_URL` 指定的测试数据库中为每个测试创建随机 schema，并在结束时删除。它不会清空数据库或使用 `DATABASE_URL`：

```sh
TEST_DATABASE_URL='postgresql://USER:PASSWORD@127.0.0.1:5432/gsmtest?sslmode=disable' go test ./...
python3 -m pip install -r requirements-contract.txt
python3 scripts/check_contract.py
```

## 部署边界

`compose.yaml` 是本地开发骨架，不是公网生产配置。公网部署应提供 TLS、托管或持久化 PostgreSQL、备份与受限管理员访问，并使用受信任的 `DATABASE_URL`。如果构建网络使用 HTTPS 检查代理，可通过 `CODEX_PROXY_CERT` 环境变量向 BuildKit 提供 CA 文件；该 CA 只在依赖下载步骤挂载，TLS 校验保持开启。

Asterisk/ARI 尚无可运行的呼叫桥。部署或监控系统不要把数据库 `/readyz` 当成呼叫就绪探针；目前它只验证 PostgreSQL 可用。呼叫端点会明确拒绝请求，直到 SIP/ARI 能力实现并通过设备联调。
