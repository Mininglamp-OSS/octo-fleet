<p align="center">
  <sub>🛟</sub>
</p>

<p align="center">
  <b>Octo Fleet — the runtime &amp; bot orchestration service for OCTO.</b><br/>
  <sub>Manages every daemon, bot, and matter dispatch in your fleet — split out of <code>octo-server</code> so each service evolves on its own clock.</sub>
</p>

<p align="center">
  <a href="https://github.com/Mininglamp-OSS"><b>🏠 OCTO Home</b></a> ·
  <a href="#-quickstart"><b>🚀 Quickstart</b></a> ·
  <a href="#-architecture"><b>🏗 Architecture</b></a> ·
  <a href="https://github.com/Mininglamp-OSS/octo-server/blob/main/CONTRIBUTING.md"><b>🤝 Contributing</b></a>
</p>

<p align="center">
  <a href="./LICENSE"><img src="https://img.shields.io/badge/License-Apache_2.0-blue.svg" alt="License"></a>
  <a href="./README.zh.md"><img src="https://img.shields.io/badge/lang-简体中文-red.svg" alt="简体中文"></a>
  <img src="https://img.shields.io/badge/go-%3E=1.25-blue.svg" alt="Go 1.25+">
  <img src="https://img.shields.io/badge/status-PoC-orange.svg" alt="PoC">
</p>

---

> 🌐 **Read in**: **English** · [简体中文](README.zh.md)

# 🛟 Octo Fleet

> **Runtime & bot orchestration** for the OCTO platform. Owns the `agent_runtime` registry, the `bot` table, and the daemon dispatch loop — extracted from `octo-server` so the IM monolith stops carrying agent-fleet concerns.

`octo-fleet` is the small Go service that every OCTO deployment runs
alongside [`octo-server`](https://github.com/Mininglamp-OSS/octo-server)
and [`octo-matter`](https://github.com/Mininglamp-OSS/octo-matter). It
holds the source of truth for which daemons are alive, which bots they
host, and which provider runtimes (Claude Code, Codex, OpenClaw,
Hermes) are running where.

## 🌟 Why Octo Fleet

- **Zero-cross-talk between backends.** `octo-server`, `octo-fleet`, and `octo-matter` make **zero** HTTP calls to each other — all trust flows through a single JWT issuer (server) verified locally via JWKS. Adding/replacing a service stays a 5-minute config change.
- **bot_token never leaves server.** Browser only sees `bot_uid`; daemon fetches the token directly from server with its daemon-scope JWT. Fleet stores orchestration metadata only.
- **Daemon pulls, never gets pushed.** Heartbeat returns `managed_bots` + any pending `bot.provision` command; daemon polls matter for tasks. No webhook / outbox / HMAC ceremony.
- **Drop-in rollback path.** Cutover from `octo-server/modules/runtime` to fleet is gated by env (`LEGACY_RUNTIME_ROUTES=true` on server re-enables the old routes); fleet runs on its own MySQL schema (`octo_fleet`) so data lives apart.

## 🚀 Quickstart

### 1. Prerequisites

- Running [`octo-server`](https://github.com/Mininglamp-OSS/octo-server) at `:8090` with the `auth_jwt` module loaded (it auto-generates a keypair on first start).
- MySQL 8 + Redis (the same instances `octo-server` uses are fine).

### 2. Create the fleet schema

```bash
mysql -uroot -p -e "CREATE DATABASE IF NOT EXISTS octo_fleet \
  CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"
```

### 3. Build & run

```bash
make build
make run     # listens on :8092 by default
```

Schema migrations apply automatically on the first start.

### 4. Verify

```bash
curl http://localhost:8092/v1/ping
# {"status":200}
```

## ⚙️ Configuration

Fleet reads its config from `configs/fleet.yaml` plus the following env overrides:

| Env | Purpose | Default |
|-----|---------|---------|
| `NOTIFY_INTERNAL_TOKEN` | Shared X-Internal-Token for legacy `/v1/internal/*` callbacks | unset → those routes 401 |

See `configs/fleet.yaml` for the full schema (`addr`, `db.mysqlAddr`, `db.redisAddr`, `external.baseURL`, `auth.serverJwksURL`).

## 🏗 Architecture

```
              ┌────────────────────────────┐
              │  octo-server :8090         │ ← only JWT issuer
              │  ├─ /v1/auth/token          │
              │  ├─ /v1/bot/mint            │
              │  ├─ /v1/bot/:uid/token      │
              │  └─ /.well-known/jwks.json  │
              └────────────────────────────┘
                    ▲ JWKS pull (cached)
                    │
              ┌─────┴────────────┬──────────────┐
              │                  │              │
       octo-fleet :8092    octo-matter :8080
       ├─ runtime/bot      ├─ matter/timeline
       ├─ heartbeat        ├─ matter_bot_task
       └─ managed_bots     └─ daemon endpoints
              ▲                  ▲
              │ JWT (web)        │ JWT (daemon)
              │                  │
        ┌─────┴────┐       ┌─────┴────────┐
        │ browser  │       │  daemon-cli  │
        └──────────┘       └──────────────┘
```

The cross-service contract is documented in
[`/Users/caster/octo/octo-daemon-cli/plan.md`](https://github.com/Mininglamp-OSS/octo-daemon-cli/blob/feat/agent-runtime/plan.md)
and the spec at
[`octo-web/docs/superpowers/specs/2026-06-01-octo-fleet-extraction-design.md`](https://github.com/Mininglamp-OSS/octo-web/blob/feat/agent-runtime/docs/superpowers/specs/2026-06-01-octo-fleet-extraction-design.md).

## 📦 What lives where

| Concern | Owner | Notes |
|---------|-------|-------|
| User / IM account / bot credential | `octo-server` | bot_token stays in `robot.bot_token`; daemon fetches via JWT |
| JWT issuer + JWKS | `octo-server`, `modules/auth_jwt` | RS256, kid stable across restarts |
| Runtime registry + bot orchestration metadata | `octo-fleet` (this repo) | own `octo_fleet` schema |
| Matter + bot_task queue | `octo-matter` | INSERT bot_task same-tx as comment |
| Daemon (pull tasks, run agents) | `octo-daemon-cli` | binds OCTO_FLEET_URL + OCTO_SERVER_URL + OCTO_MATTER_URL |
| Web UI | `octo-web` | three-step bot mint orchestration in browser |

## 🔌 HTTP API

### Daemon endpoints (JWT scope=daemon)
- `POST   /v1/runtimes` — register
- `POST   /v1/runtimes/{runtime_id}/heartbeat` — liveness; returns pending upgrade task / bot.provision command + `managed_bots`
- `POST   /v1/runtimes/_deregister` — deregister
- `GET    /v1/runtimes/{runtime_id}/events` — SSE reverse-dispatch stream
- `GET    /v1/bots/{bot_id}/provision` — fetch the bot.provision payload (workspace_id, bot_uid, claim_token; **never** `bot_token`)
- `POST   /v1/bots/{bot_id}/ack` — ack a provision command
- `GET    /v1/providers` — list active agent providers
- `POST   /v1/upgrades/{task_id}/report` — report upgrade task result

### Web endpoints (JWT scope=web)
- `GET    /v1/runtimes` — list registered runtimes in space
- `DELETE /v1/runtimes/{runtime_id}` — remove a runtime
- `POST   /v1/bots` — create bot (draft)
- `POST   /v1/bots/{bot_id}/mint` — patch with server-minted `bot_uid` to start provision
- `GET    /v1/bots` · `GET /v1/bots/{bot_id}` — list / read (bot feed moved to matter: `GET /matter/api/v1/bots/:bot_uid/feed`)
- `DELETE /v1/bots/{bot_id}` — archive
- `POST   /v1/upgrades` — init an upgrade task
- `GET    /v1/upgrades/{task_id}` — get upgrade task status

### Admin endpoints (X-Runtime-Admin-Token)
- `POST   /v1/runtime_latest_versions` — upsert a component's latest version + release metadata

## 🚧 PoC status

This is part of the **fleet-extraction PoC** (PR-A → PR-B → PR-C). It's
running in local dev and the full e2e is verified end-to-end. Before
production:

- [ ] Unit tests for `internal/auth` and bot creation flow
- [ ] Clean up legacy `bot_task.go` (PR-B.3 deprecated the table; code retained for rollback)
- [ ] Wire CI lint + golangci-lint

## 📜 License

Apache License 2.0 — see [`LICENSE`](./LICENSE).
