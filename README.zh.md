<p align="center">
  <sub>🛟</sub>
</p>

<p align="center">
  <b>Octo Fleet —— OCTO 平台的 runtime &amp; bot 编排服务。</b><br/>
  <sub>管理 fleet 里所有 daemon、bot、matter 派任 —— 从 <code>octo-server</code> 拆出，让每个服务按自己节奏演进。</sub>
</p>

<p align="center">
  <a href="https://github.com/Mininglamp-OSS"><b>🏠 OCTO 主页</b></a> ·
  <a href="#-快速开始"><b>🚀 快速开始</b></a> ·
  <a href="#-架构"><b>🏗 架构</b></a> ·
  <a href="https://github.com/Mininglamp-OSS/octo-server/blob/main/CONTRIBUTING.zh.md"><b>🤝 贡献</b></a>
</p>

<p align="center">
  <a href="./LICENSE"><img src="https://img.shields.io/badge/License-Apache_2.0-blue.svg" alt="License"></a>
  <a href="./README.md"><img src="https://img.shields.io/badge/lang-English-blue.svg" alt="English"></a>
  <img src="https://img.shields.io/badge/go-%3E=1.25-blue.svg" alt="Go 1.25+">
  <img src="https://img.shields.io/badge/status-PoC-orange.svg" alt="PoC">
</p>

---

> 🌐 **语言切换**: [English](README.md) · **简体中文**

# 🛟 Octo Fleet

> **Runtime & bot 编排服务**，OCTO 平台。负责 `agent_runtime` 注册表、`bot` 表、以及 daemon 派任循环 —— 从 `octo-server` 抽出，让 IM 主服务不再背负 agent fleet 相关包袱。

`octo-fleet` 是一个 Go 小服务，跟 [`octo-server`](https://github.com/Mininglamp-OSS/octo-server) 和 [`octo-matter`](https://github.com/Mininglamp-OSS/octo-matter) 并肩部署。它持有"哪些 daemon 在线、daemon 上跑了哪些 bot、各家 provider runtime（Claude Code / Codex / OpenClaw / Hermes）当前状态"的权威数据。

## 🌟 为什么需要 Octo Fleet

- **后端服务零互调。** `octo-server` / `octo-fleet` / `octo-matter` 之间**没有任何**直接 HTTP 调用，所有信任链通过 server 的单一 JWT issuer（JWKS 本地验签）走通。替换或新增服务变成 5 分钟改配置的事。
- **bot_token 永不离开 server。** 浏览器只看到 `bot_uid`，daemon 用自己的 daemon-scope JWT 直接去 server 拉 token。fleet 只存编排元数据。
- **Daemon 主动 pull，永远不 push。** 心跳响应里带 `managed_bots` + 可选 `bot.provision`；daemon 自己去 matter 拉 task。不用 webhook / outbox / HMAC 那一套。
- **稳健 rollback。** 从 `octo-server/modules/runtime` 切到 fleet 走 env 开关（server 上 `LEGACY_RUNTIME_ROUTES=true` 可恢复旧路由）；fleet 用独立 MySQL schema (`octo_fleet`)，数据与 server 物理隔离。

## 🚀 快速开始

### 1. 前置依赖

- [`octo-server`](https://github.com/Mininglamp-OSS/octo-server) 跑在 `:8090`，已加载 `auth_jwt` 模块（首次启动自动生成密钥对）
- MySQL 8 + Redis（跟 `octo-server` 共用同一实例即可）

### 2. 建 fleet 库

```bash
mysql -uroot -p -e "CREATE DATABASE IF NOT EXISTS octo_fleet \
  CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"
```

### 3. 构建 & 运行

```bash
make build
make run     # 默认监听 :8092
```

首次启动会自动跑 schema 迁移。

### 4. 验证

```bash
curl http://localhost:8092/v1/ping
# {"status":200}
```

## ⚙️ 配置

Fleet 配置从 `configs/fleet.yaml` 读，加上以下环境变量覆盖：

| 环境变量 | 用途 | 默认 |
|---|---|---|
| `OCTO_MATTER_URL` | matter 服务的 base URL（bot feed proxy 用） | 未设 → bot feed 返回空 |
| `NOTIFY_INTERNAL_TOKEN` | 兼容老 `/v1/internal/*` 回调的 X-Internal-Token | 未设 → 这些路由 401 |

完整字段（`addr` / `db.mysqlAddr` / `db.redisAddr` / `external.baseURL` / `auth.serverJwksURL`）见 `configs/fleet.yaml`。

## 🏗 架构

```
              ┌────────────────────────────┐
              │  octo-server :8090         │ ← 唯一 JWT issuer
              │  ├─ /v1/auth/token          │
              │  ├─ /v1/bot/mint            │
              │  ├─ /v1/bot/:uid/token      │
              │  └─ /.well-known/jwks.json  │
              └────────────────────────────┘
                    ▲ JWKS 拉取（带缓存）
                    │
              ┌─────┴────────────┬──────────────┐
              │                  │              │
       octo-fleet :8092    octo-matter :8080
       ├─ runtime/bot      ├─ matter/timeline
       ├─ heartbeat        ├─ matter_bot_task
       └─ managed_bots     └─ daemon endpoints
              ▲                  ▲
              │ JWT (web 作用域)  │ JWT (daemon 作用域)
              │                  │
        ┌─────┴────┐       ┌─────┴────────┐
        │ browser  │       │  daemon-cli  │
        └──────────┘       └──────────────┘
```

跨服务契约的完整文档：[`octo-daemon-cli/plan.md`](https://github.com/Mininglamp-OSS/octo-daemon-cli/blob/feat/agent-runtime/plan.md)、[`octo-web spec`](https://github.com/Mininglamp-OSS/octo-web/blob/feat/agent-runtime/docs/superpowers/specs/2026-06-01-octo-fleet-extraction-design.md)。

## 📦 各组件职责分布

| 关注点 | 归属 | 备注 |
|---|---|---|
| 用户 / IM 账号 / bot 凭据 | `octo-server` | bot_token 留 `robot.bot_token`，daemon 走 JWT 拉 |
| JWT issuer + JWKS | `octo-server`, `modules/auth_jwt` | RS256，kid 跨重启稳定 |
| Runtime 注册 + bot 编排元数据 | `octo-fleet`（本仓库） | 独立 `octo_fleet` schema |
| Matter + bot_task 队列 | `octo-matter` | INSERT bot_task 跟 timeline 同事务 |
| Daemon（拉任务、跑 agent） | `octo-daemon-cli` | 同时绑 OCTO_FLEET_URL + OCTO_SERVER_URL + OCTO_MATTER_URL |
| Web UI | `octo-web` | 浏览器编排 bot 创建 3 步走 |

## 🔌 HTTP API

### Daemon 端点（JWT scope=daemon）
- `POST /v1/daemon/register`
- `POST /v1/daemon/heartbeat` — 返回 `pending_command`（bot.provision）+ `managed_bots`
- `POST /v1/daemon/deregister`
- `POST /v1/daemon/{ping,upgrade,bots,bot-tasks}/...` 各类 ack

### Web 端点（JWT scope=web）
- `GET  /v1/runtimes` — 列空间内注册的 runtime
- `POST /v1/runtimes/bots` — 建 bot（draft 状态）
- `POST /v1/runtimes/bots/:id/mint` — patch server mint 出来的 `bot_uid`，触发 provision
- `GET  /v1/runtimes/bots[/:id[/feed]]` — 各种读 API
- `DELETE /v1/runtimes/bots/:id` — 归档

## 🚧 PoC 状态

这是 fleet 拆分 PoC 的一部分（PR-A → PR-B → PR-C）。已在本地完整 e2e 跑通。生产前还要补：

- [ ] `internal/auth` 和 bot 创建链路的单测
- [ ] 清理 legacy `bot_task.go`（PR-B.3 已废弃，代码留作 rollback）
- [ ] 去掉 fleet→matter 的 bot feed proxy，改成浏览器直接调 matter
- [ ] 接入 CI lint + golangci-lint

## 📜 协议

Apache License 2.0 — 见 [`LICENSE`](./LICENSE)。
