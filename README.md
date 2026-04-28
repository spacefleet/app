# SpaceFleet

SpaceFleet is a developer platform for easily deploying apps in your own cloud.

This project is the main app that's hosted at https://spacefleet.dev 

If you're just looking for a way to get started with SpaceFleet and deploy your own app, you probably want to start with the [SpaceFleet CLI]().

## Development guide

### Prerequisites

- Go 1.25+ (project uses the `tool` directive added in 1.24)
- Node 20+ and npm
- [Air](https://github.com/air-verse/air) for `make dev` hot reload — `go install github.com/air-verse/air@latest`
- [Pulumi CLI](https://www.pulumi.com/docs/install/) — required by the worker process for the build pipeline. `brew install pulumi` on macOS. Version is pinned in [`.tool-versions`](.tool-versions); asdf-style version managers will read it automatically.

### Setup

```sh
make ui-install   # npm install inside ui/
make gen          # regenerate Go + TS code from api/openapi.yaml (optional on a fresh clone; already checked in)
```

Copy the `.env.example` file and configure for your environment
```sh
cp .env.example .env
```

### Development

Start development containers for Postgres and Redis using Docker Compose
```sh
docker-compose up -d
```

Spacefleet runs as two long-lived processes — `serve` for the HTTP API and `worker` for River-backed background jobs (builds, app-destroys). In dev you run both alongside the Vite dev server:

**Terminal 1 — Go backend (port 8080, live reload):**

```sh
make dev
```

**Terminal 2 — Worker process:**

```sh
make worker
```

**Terminal 3 — Vite dev server (port 5173, HMR):**

```sh
make ui-dev
```

Then open <http://localhost:5173>. Vite proxies `/api/*` to the Go server on `:8080`, so your React code calls same-origin paths (`/api/health`, `/api/ping`) and everything just works. No CORS, no config.

> In production it's two processes from the same binary on one or more hosts — Go serves the embedded SPA and handles `/api/*` itself, and the worker drains the River queue. See [`docs/self-hosting.md`](docs/self-hosting.md) for production wiring.

### Editing the API

1. Edit [`api/openapi.yaml`](api/openapi.yaml).
2. Run `make gen` — this regenerates:
   - [`lib/api/gen.go`](lib/api/gen.go) — Go types + `StrictServerInterface`
   - [`ui/src/api/schema.d.ts`](ui/src/api/schema.d.ts) — TS types consumed by `openapi-fetch`
3. Implement any new methods on `api.Server` in [`lib/api/handlers.go`](lib/api/handlers.go). The generated `StrictServerInterface` will fail to compile until you do — that's intentional.
4. Call the new endpoint from the UI via the typed client:

   ```ts
   import { api } from "./api/client";
   const { data, error } = await api.GET("/api/ping", {
     params: { query: { name: "crew" } },
   });
   ```

### Testing

```sh
make test        # go test ./...
make vet         # go vet ./...
make fmt         # go fmt ./...
```

There's no JS test runner configured yet. Type-check the UI with:

```sh
cd ui && npm run typecheck
```
