# gsm2sip-server

Go + PostgreSQL 转发服务，把主机的短信任务投递到旧手机网关，并把网关的持久化事件安全同步回主机。短信控制核心已实现：一次性配对码、短时 access 与轮换 refresh token、device role/owner 隔离、SIM 两阶段绑定、heartbeat、短信 command claim、事件批量事务与 durable ACK、按 owner 分页的消息事件流。

`GET /v1/ws` 已提供 authenticated wake-only WebSocket：连接建立时和对应 durable marker 改变时只发送 `{"protocol_version":1,"type":"sync_required"}`，客户端再通过 HTTPS 读取 durable cursor/command。它不传事件、命令或来电数据，不等价于 call-ready，FCM、呼入 push 和完整背压仍待实现；消息事件当前保留在数据库中。断线和重复唤醒仍以 HTTPS 同步恢复。

新增 SIP 凭据配置/加密恢复、一次性呼叫意图、Asterisk ARI/Stasis 编排、设备通话占位、呼入 pending/ready/expiry 和呼叫历史。配置缺失或 ARI 未连接时拒绝创建呼叫；实际双 SIM 音频和公网联调仍需真机验收。参见 [运行说明](docs/operations.md)。

网关后续按 Magisk 通用能力接口实现账户映射和数字音频适配，取消机型白名单及 API 31 整体语音门槛。短信路径无需 root；语音能力按实际接口验证，详见实施审查中的 Magisk 补充。

- [服务端计划](PLAN.md)
- [原始协议](docs/protocol-v1.md)
- [服务端 wire addendum](docs/server-wire-addendum.md)
- [OpenAPI](openapi/openapi.yaml)
- [跨端路线图](docs/roadmap.md)
- [功能缺口与弱网恢复审查](docs/network-and-feature-status.md)
- [实施审查记录](docs/IMPLEMENTATION-REVIEW-2026-10-03.md)

## 本地运行

需要 Docker Compose v2。开发 Compose 将 PostgreSQL 仅放在内部网络，用 Caddy 把 API 映射到 `127.0.0.1:8080`。数据库密码仅用于本地演示；把 `.env.example` 复制为 `.env` 并换成自己的本地密码：

```sh
cp .env.example .env
# 按 docs/operations.md 生成并保存加密键、配置 TLS 证书与 ARI 凭据
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

`compose.yaml` 是本地开发骨架，不是公网生产配置。公网部署应提供 TLS、托管或持久化 PostgreSQL、备份与受限管理员访问，并使用受信任的 `DATABASE_URL`。生产客户端使用 `wss://`，保留默认 CA 和主机名验证；Caddy 可以在公网终止 TLS，再通过私有网络转发到 API 的 HTTP listener。明文 `ws://` 仅适用于本机开发和测试。如果构建网络使用 HTTPS 检查代理，可通过 `CODEX_PROXY_CERT` 环境变量向 BuildKit 提供 CA 文件；该 CA 只在依赖下载步骤挂载，TLS 校验保持开启。

数据库 `/readyz` 只验证 PostgreSQL，不能表示电话已就绪。呼叫控制另行检查 ARI 连接、设备和 SIM 的可用状态；SIP REGISTER 也不能代替数字音频验收。服务端事件事务提交后才 ACK；主机另行确认 SQLite durable cursor，确认不会删除事件。刷新响应丢失可用原 token 和持久化请求键恢复当前令牌代际，直到其自然到期。
