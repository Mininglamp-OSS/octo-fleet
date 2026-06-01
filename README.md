# octo-fleet — runtime/bot orchestration service

Independent backend split out of `octo-server` per spec
`docs/superpowers/specs/2026-06-01-octo-fleet-extraction-design.md`
(see also `/Users/caster/octo/octo-daemon-cli/plan.md` for the
overall cross-service plan).

## Why this exists

`octo-server` was carrying both IM/user concerns *and* runtime/bot
orchestration. The runtime side has different scaling, ownership, and
deployment characteristics. Splitting it lets each service evolve
independently and removes a tangle of cross-module dependencies inside
the monolith.

## Service boundaries

The trust chain is JWT-based — `octo-server` is the only JWT issuer.
`octo-fleet` and `octo-matter` fetch the server's public key from
`/.well-known/jwks.json` and verify tokens locally. **No service makes
HTTP calls to another service** — daemon and browser orchestrate.

```
            ┌─────────────────┐
            │  octo-server    │ ← JWT issuer (RS256)
            │  :8090          │   + bot mint + bot_token
            └─────────────────┘
                    ▲
                    │ jwks.json (pull, cached)
                    │
            ┌─────────────────┐
            │  octo-fleet     │ ← this service
            │  :8092          │   runtime / bot / bot_task CRUD
            └─────────────────┘
              ▲              ▲
              │ JWT (web)    │ JWT (daemon)
              │              │
        ┌─────────┐    ┌──────────────┐
        │ browser │    │  daemon-cli  │
        └─────────┘    └──────────────┘
```

## Local dev

```bash
# 1. Ensure server is up at :8090 with auth_jwt module loaded
#    (server self-generates RSA keypair to ~/.octo-server/jwt-priv.pem
#    on first start, exposes /.well-known/jwks.json)

# 2. Create the fleet schema (one-time)
docker exec testenv-mysql-1 mysql -uroot -pdemo \
  -e "CREATE DATABASE IF NOT EXISTS octo_fleet CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"

# 3. Build & run
make run

# Fleet listens on :8092. SQL migrations apply on first start.
```

## Daemon hookup

Set both URL env vars (back-compat: if OCTO_FLEET_URL is unset, daemon
stays on legacy api-key + server path):

```bash
OCTO_FLEET_URL=http://127.0.0.1:8092 \
OCTO_SERVER_URL=http://127.0.0.1:8090 \
./octo-daemon start
```

When `OCTO_FLEET_URL` is set, daemon:
- Exchanges its api-key for a JWT against server `/v1/auth/token`
- Sends JWT to fleet for all `/v1/daemon/*` calls
- Sends JWT to server for `/v1/bot/:uid/token` lookups
- Refreshes JWT 5min before expiry (default TTL 30 days)

## Web hookup

The browser does session → JWT exchange automatically. `vite.config.ts`
proxies `/api/v1/runtimes/*` and `/api/v1/daemon/*` to fleet :8092; all
other `/api/v1/*` routes still go to server :8090.

`packages/dmworkbase/src/Service/APIClient.ts` injects
`Authorization: Bearer <JWT>` on requests matching `/runtimes` or
`/daemon` URL prefixes. JWT is fetched and cached in-memory by
`getFleetJWT()`.

## What changed in octo-server

- New module `modules/auth_jwt/` contains:
  - `POST /v1/auth/token`        — exchange session/api-key for JWT
  - `GET  /.well-known/jwks.json` — public key for verifiers
  - `POST /v1/bot/mint`           — web-callable, mints bot OBO
  - `GET  /v1/bot/:uid/token`     — daemon-callable, returns bot_token
- `modules/runtime/` HTTP routes deprecated via `LEGACY_RUNTIME_ROUTES`
  env (default false → 404). Schema migrations still apply so
  re-enable is one env var away.

## Known scope gaps (deferred work)

- **Bot creation flow not fully refactored**: fleet's `POST /v1/runtimes/bots`
  currently inserts a draft row but does not yet trigger the full
  3-step orchestration (web → fleet draft → web → server mint → web →
  fleet patch). Bot list / get / archive work; create needs a follow-up.
- **bot_task table lives in fleet temporarily**. PR-B will move it to
  `octo-matter` per the original plan (so matter can write the queue
  in the same transaction as the timeline entry).
- **server's `modules/runtime/` code retained**: routes are off but
  files remain. PR-C deletes them once fleet stability is proven.
