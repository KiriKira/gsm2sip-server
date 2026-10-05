# gsm2sip-server

Go + PostgreSQL 转发服务，把主机的短信任务投递到旧手机网关，并把网关的持久化事件安全同步回主机。一个 owner 可配对多个独立 client；每台 host 有自己的 API 会话、SIP endpoint/AOR、事件 durable receipt 和短信写请求幂等范围。短信控制核心包括一次性配对码、短时 access 与轮换 refresh token、owner/device 隔离、SIM 两阶段绑定、heartbeat、短信 command claim、事件批量事务与 durable ACK、按 owner 分页的消息事件流。

`GET /v1/clients` 让 client 查看同 owner 已配对设备及 `platform/state/is_self`。`POST /pairings/claim` 的平台标签是开放展示标识（如 `android`、`windows`、`linux`），不构成系统白名单或能力声明。`GET /v1/ws` 仍是 authenticated wake-only WebSocket，只发 `{"protocol_version":1,"type":"sync_required"}`；owner 的短信事件变化唤醒 owner clients，呼入 participant 变化只唤醒对应 client。客户端分别通过 HTTPS 恢复短信游标和读取自己的 `/calls` 视图。frame 不带事件、命令、来电数据或游标；FCM/APNs、呼入 push 与完整背压仍待实现。

SIP 凭据配置/加密恢复、一次性呼叫意图、Asterisk ARI/Stasis 编排、设备通话占位、呼入 pending/ready/expiry 和呼叫历史已接入。一个蜂窝呼入可为多个 active client 快照各自的 participant 状态；只有真正到达 Up 的已认证 SIP channel 决定 winner，其他 host 得到 `ended/answered_elsewhere`。拒接一台设备不会结束其它候选，所有候选结束或呼入超时才释放 gateway 通话 slot。配置缺失或 ARI 未连接时拒绝创建呼叫；实际双 SIM 音频和公网联调仍需真机验收。参见 [运行说明](docs/operations.md)。

平台标签不会授予 host root、短信或蜂窝控制权限。Windows 的 host UI 与媒体验证尚未实现；`windows` 可用于未来版本配对识别，但不能当作 Windows 客户端已交付。MagiskVM 或其他 Android 运行环境也必须单独验证 SIM/Telecom 账户与实际媒体路径；当前契约不承诺 raw PCM、特定 root 接口或短信必达。

- [服务端计划](PLAN.md)
- [原始协议](docs/protocol-v1.md)
- [服务端 wire addendum](docs/server-wire-addendum.md)
- [OpenAPI](openapi/openapi.yaml)
- [跨端路线图](docs/roadmap.md)
- [功能缺口与弱网恢复审查](docs/network-and-feature-status.md)
- [实施审查记录](docs/IMPLEMENTATION-REVIEW-2026-10-03.md)

## 备份与恢复

提供完整 PostgreSQL 快照与校验清单，支持恢复到新的空数据库。恢复和执行状态隔离在同一事务中完成，历史待发送短信不会因恢复自动重发。支持旧迁移版本的备份；使用方式见 [备份与恢复](docs/operations.md#postgresql-backup-and-isolated-restore)。

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
