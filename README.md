# SoulGem

SoulGem 是三端系统的服务端：主机 App **Madoka**、旧手机网关 App **Homura** 通过它配对、同步短信并建立通话。
（仓库、镜像、二进制和数据库名仍沿用 gsm2sip，升级和回退不用改部署。）

**0.4.0**：通话只走 WebRTC，SIP、Asterisk 与 ARI 已彻底移除。呼叫状态、来电多主机抢接的 winner 和网关通话占用
都由 Go/PostgreSQL 维护，音频走 ICE/DTLS-SRTP/Opus，需要两部手机都能到达的 TURN（一体镜像自带）。
从 0.4 之前升级、以及回退到上一版的方法见 [0.4 发布说明](docs/release-0.4.md)；三端联合验收计划见
[0.4 联合测试计划](docs/plan-0.4-joint-test.md)。

[用预构建镜像部署](docs/deploy-images.md)：服务器不需要编译。一体镜像 `ghcr.io/kirikira/gsm2sip` 一条 `docker run`
即可（含 PostgreSQL 与 TURN），也可用 `deploy/compose.yaml` 分镜像部署（amd64/arm64）。
[自动构建与 Release](docs/releases.md)：`main` 推送发布预览版，版本标签发布正式版，包含 Linux amd64/arm64 部署包。

Go + PostgreSQL 转发服务，把主机的短信任务投递到旧手机网关，并把网关的持久化事件安全同步回主机。一个 owner
可配对多个独立 client；每台 host 有自己的 API 会话、事件 durable receipt 和短信写请求幂等范围。短信控制核心包括
一次性配对码、短时 access 与轮换 refresh token、owner/device 隔离、SIM 两阶段绑定、heartbeat、短信 command claim、
事件批量事务与 durable ACK、按 owner 分页的消息事件流。

`GET /v1/clients` 让 client 查看同 owner 已配对设备及 `platform/state/is_self`。`POST /pairings/claim` 的平台标签是
开放展示标识（如 `android`、`windows`、`linux`），不构成系统白名单或能力声明。`GET /v1/ws` 是 authenticated
wake-only WebSocket，只发 `{"protocol_version":1,"type":"sync_required"}`；owner 的短信事件变化唤醒 owner clients，
通话变化只唤醒相关设备。frame 不带事件、命令、来电数据或游标。

通话控制见 [RTC 呼叫控制 v1](docs/rtc-call-control-v1.md)：主机预留出站呼叫，网关在媒体协商完成后才拨号；
来电同时邀请所有在线、已声明 RTC 能力的主机，由网关在全局账本里选定唯一 winner，其他主机得到
`ended/answered_elsewhere`。控制租约过期时已拨出的通话标为 `unknown` 并保留占用，直到网关确认蜂窝结束。

平台标签不会授予 host root、短信或蜂窝控制权限。MagiskVM 或其他 Android 运行环境也必须单独验证 SIM/Telecom
账户与实际媒体路径；当前契约不承诺特定 root 接口或短信必达。

- [三端架构与远程控制链路（总览、数据流、协议演进、后续优先级）](docs/architecture.md)
- [多节点（mesh）：多台独立服务器、按优先级自动切换、跨节点去重](docs/mesh.md)
- [服务端计划](PLAN.md)
- [原始协议](docs/protocol-v1.md)
- [服务端 wire addendum](docs/server-wire-addendum.md)
- [OpenAPI](openapi/openapi.yaml)
- [跨端路线图](docs/roadmap.md)
- [0.4 发布说明：升级与回退](docs/release-0.4.md)
- [0.4 三端联合测试计划](docs/plan-0.4-joint-test.md)
- [三端 WebRTC 通话迁移计划（历史）](docs/plan-webrtc.md)
- [功能缺口与弱网恢复审查](docs/network-and-feature-status.md)
- [两端 KVM 界面截图与原始验证结果](docs/ui-verification/README.md)
- [Android 虚拟机使用指南：启动、停止、截图与 Magisk 验证](https://github.com/KiriKira/gsm2sip/blob/main/docs/android-virtual-machine.md)
- [实施审查记录](docs/IMPLEMENTATION-REVIEW-2026-10-03.md)

## 备份与恢复

提供完整 PostgreSQL 快照与校验清单，支持恢复到新的空数据库。恢复和执行状态隔离在同一事务中完成，历史待发送短信不会因恢复自动重发。支持旧迁移版本的备份；使用方式见 [备份与恢复](docs/operations.md#postgresql-backup-and-isolated-restore)。

## 本地运行

需要 Docker Compose v2。开发 Compose 将 PostgreSQL 仅放在内部网络，用 Caddy 把 API 映射到 `127.0.0.1:8080`。数据库密码仅用于本地演示；把 `.env.example` 复制为 `.env` 并换成自己的本地密码：

```sh
cp .env.example .env
# 按 docs/operations.md 生成并保存加密键
docker compose up --build -d
curl http://127.0.0.1:8080/healthz
curl http://127.0.0.1:8080/readyz
```

生成一个让两部手机加入的链接和二维码（没有账户时自动创建）：

```sh
docker compose run --rm --no-deps --entrypoint /usr/local/bin/mahogsm-admin api \
  join create --url http://手机访问这台服务器的地址:8080
```

旧手机和主机各扫一次，各自以自己的身份加入；码只显示一次，服务端只保存其 SHA-256，每种手机只能用一次，默认 15 分钟过期。
两端配对后会通过 HTTPS 完成 SIM 本地确认和短信任务。不要把 `.env`、链接或码提交到版本库。

`docker compose logs -f api worker` 查看本地服务日志。`docker compose down` 保留本地数据库卷；`docker compose down -v` 会删除它。

## 本地 Go 开发

服务端要求 Go 1.24 或更新版本，PostgreSQL 17。API、worker 和 admin CLI 启动时会安全地串行应用 SQL migrations；数据库连接启用 `synchronous_commit=on`，网关事件只有在事务提交后才获得 ACK。

```sh
export DATABASE_URL='postgresql://USER:PASSWORD@127.0.0.1:5432/gsm2sip?sslmode=disable'
go test ./...
go run ./cmd/api
```

运行 `go run ./cmd/worker` 启动未领取短信任务的 TTL 清理器；运行 `go run ./cmd/admin join create --url http://127.0.0.1:8080` 生成两部手机共用的加入链接，或用 `owner create`、`pairing-code create --role gateway|client` 单独管理。

PostgreSQL 集成测试会在 `TEST_DATABASE_URL` 指定的测试数据库中为每个测试创建随机 schema，并在结束时删除。它不会清空数据库或使用 `DATABASE_URL`：

```sh
TEST_DATABASE_URL='postgresql://USER:PASSWORD@127.0.0.1:5432/gsmtest?sslmode=disable' go test ./...
python3 -m pip install -r requirements-contract.txt
python3 scripts/check_contract.py
```

## 部署边界

`compose.yaml` 是本地开发骨架，不是公网生产配置。公网部署应提供 TLS、托管或持久化 PostgreSQL、备份与受限管理员访问，并使用受信任的 `DATABASE_URL`。生产客户端使用 `wss://`，保留默认 CA 和主机名验证；Caddy 可以在公网终止 TLS，再通过私有网络转发到 API 的 HTTP listener。明文 `ws://` 仅适用于本机开发和测试。如果构建网络使用 HTTPS 检查代理，可通过 `CODEX_PROXY_CERT` 环境变量向 BuildKit 提供 CA 文件；该 CA 只在依赖下载步骤挂载，TLS 校验保持开启。

数据库 `/readyz` 只验证 PostgreSQL，不能表示电话已就绪。呼叫控制另行检查网关在线、RTC 能力声明和 SIM 的可用状态；媒体协商完成也不能代替真机音频验收。服务端事件事务提交后才 ACK；主机另行确认 SQLite durable cursor，确认不会删除事件。刷新响应丢失可用原 token 和持久化请求键恢复当前令牌代际，直到其自然到期。
