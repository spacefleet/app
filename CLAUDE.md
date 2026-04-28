# CLAUDE.md

Spacefleet is a Go backend + React SPA that ship as a single binary. The Go program serves `/api/*` and the embedded Vite build from the same origin in production. A shared OpenAPI spec drives both server stubs and the TypeScript client (and a separate CLI project outside this repo).

If a CLAUDE.custom.md file exists in the root of the project, read that first and then continue with the rest of this document.

## Architecture essentials

- **Entrypoint**: [cmd/spacefleet/main.go](cmd/spacefleet/main.go) — loads `.env`, builds `*http.Server` via [lib/server/server.go](lib/server/server.go), handles graceful shutdown.
- **Routing**: [lib/server/routes.go](lib/server/routes.go) mounts three things on one `*http.ServeMux`:
  1. Generated `/api/*` handlers behind `RequireAuth` → `RequireOrg` middleware.
  2. `/config.js` — emits `window.appConfig` with the Clerk publishable key (non-secret values only).
  3. `/` → [ui/embed.go](ui/embed.go) which serves the embedded SPA and falls back to `index.html` for any non-`/api/` path (client-side routing).
- **Auth**: Clerk session JWTs. [lib/auth/middleware.go](lib/auth/middleware.go) verifies the `Authorization` header; `publicAPIPaths` in `routes.go` controls the bypass list (`/api/health` today). `RequireOrg` enforces that the session's active org slug matches the `/api/orgs/{slug}/...` segment — any new org-scoped routes must follow that URL shape to be protected automatically.
- **Frontend**: Vite + React 18 + TS, React Router v7, Tailwind v4, Clerk React SDK. The typed API client lives in [ui/src/api/client.ts](ui/src/api/client.ts) and attaches a Clerk token via middleware (`ApiAuthBinder` wires `setAuthTokenProvider` once Clerk loads).

## UI components

shadcn/ui is welcome as a starting point for components when it saves boilerplate — the project is scaffolded for it: `@/*` alias, `cn()` in [ui/src/lib/utils.ts](ui/src/lib/utils.ts), `lucide-react` installed, and [ui/components.json](ui/components.json) configured. Add components with `cd ui && npx shadcn add <name>` (they land in `ui/src/components/ui/`). The first `shadcn add` will also inject the base CSS variables into [ui/src/index.css](ui/src/index.css). Feel free to adapt generated components to fit the design — they're owned code, not a library.

**Brand: sharp corners, no border radius.** Every rectangular element renders with square corners. Don't add `rounded-*` classes to new components; the Tailwind radius scale is overridden to zero in [ui/src/index.css](ui/src/index.css) as a safety net (so shadcn-generated `rounded-md` silently resolves to 0), and Clerk's `borderRadius` variable is set to 0 in [ui/src/main.tsx](ui/src/main.tsx). `rounded-full` is still allowed — it doesn't use the radius scale, and circular avatars/status dots are fine.

## The OpenAPI contract is the source of truth

[api/openapi.yaml](api/openapi.yaml) generates:
- [lib/api/gen.go](lib/api/gen.go) (Go types + `StrictServerInterface`) via `oapi-codegen` — configured in [lib/api/cfg.yaml](lib/api/cfg.yaml), invoked via `go:generate` in [lib/api/doc.go](lib/api/doc.go).
- [ui/src/api/schema.d.ts](ui/src/api/schema.d.ts) via `openapi-typescript`.

Workflow for a new or changed endpoint:
1. Edit `api/openapi.yaml`.
2. Run `make gen`.
3. Implement the new method on `*Server` in [lib/api/handlers.go](lib/api/handlers.go). The build will break until you do — that's the intended gate.
4. Call it from the UI via `api.GET("/api/...", ...)` — types flow through automatically.

Never edit `gen.go` or `schema.d.ts` by hand.

## Dev workflow

Three terminals:
```sh
make dev      # Air live-reload on :8080 (HTTP API)
make worker   # River-backed background-job worker (builds, destroys)
make ui-dev   # Vite on :5173, proxies /api/* and /config.js to :8080
```
Open <http://localhost:5173>. Same-origin in dev via the Vite proxy, same-origin in prod via the embedded binary — no CORS either way.

Backing services (optional today, wired into `.env.example`):
```sh
make services-up   # Postgres + Redis via docker-compose
```

## Two processes: `serve` and `worker`

The binary dispatches by subcommand: `spacefleet serve` runs the HTTP API, `spacefleet worker` runs the River-backed job worker, `spacefleet migrate` is the SQL migration tool. Default (no subcommand) is `serve`.

- The HTTP API is stateless — scale horizontally.
- The worker is the only place build dispatch / pulumi state mutation happens. It applies River's bundled migrations on startup. Reattaches to in-flight builds across restarts (phase 5).
- Both read the same `.env`. The Pulumi CLI must be on `$PATH` for the worker (the runtime Docker image installs it; locally `brew install pulumi`).

## Common commands

| Task | Command |
| --- | --- |
| Regenerate Go + TS from spec | `make gen` |
| Go tests | `make test` |
| Go vet / fmt | `make vet` / `make fmt` |
| UI typecheck | `cd ui && npm run typecheck` |
| Production build | `make build` (UI → `ui/dist` → embedded → `bin/spacefleet`) |
| Wipe everything | `make clean` |

No JS test runner is configured — UI verification is typecheck-only for now.

## Gotchas

- **Empty `ui/dist` breaks Go builds.** `//go:embed all:dist` needs at least one file. `make ui-build` keeps a `.gitkeep` in place; if you wiped `ui/dist/`, run `make ui-build` before `go build`.
- **Middleware order is reversed.** `oapi-codegen` applies the `Middlewares` slice last-to-first, so in `routes.go` the *last* entry wraps outermost. `RequireAuth` runs before `RequireOrg` because of that.
- **`window.appConfig` only ships non-secrets.** Anything added to `appConfigHandler` is visible to every browser — don't put server-side keys there.
- **New `/api/*` routes need `make gen` first.** If a request returns HTML, the route isn't mounted — you either forgot to regenerate or the handler isn't registered.
- **Air's `exclude_dir` skips `ui/`.** Changing TS/TSX won't restart the Go server, which is what you want — Vite HMR handles the UI side.

## Project layout

```
spacefleet-app/
├── api/openapi.yaml         # shared contract (drives Go + TS + external CLI)
├── cmd/spacefleet/          # main.go (subcommand dispatch) + serve.go + worker.go + migrate.go
├── docs/self-hosting.md     # operator-facing setup guide
├── lib/
│   ├── api/                 # gen.go (generated) + handlers.go (hand-written)
│   ├── auth/                # Clerk middleware, session helpers
│   ├── config/              # env loading
│   ├── pulumi/              # Automation API wrapper (s3 backend, KMS provider, runner)
│   ├── queue/               # River wrapper (pgxpool, migrations, worker registry)
│   └── server/              # http.Server, request logging, route mounting
├── scripts/bootstrap-state.sh  # idempotent bootstrap of Pulumi state bucket + KMS key
├── ui/
│   ├── embed.go             # //go:embed all:dist
│   ├── src/api/             # generated schema + openapi-fetch client
│   ├── src/components/      # ApiAuthBinder, Layout, RequireAuth, RequireOrganization
│   ├── src/routes/          # page-level components
│   └── vite.config.ts       # /api + /config.js proxy to :8080
├── Makefile
├── docker-compose.yml       # Postgres + Redis for local dev
├── .tool-versions           # pulumi CLI version pin (asdf-style)
└── .air.toml
```
