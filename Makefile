.PHONY: run build test fmt vet tidy dev clean gen ui-install ui-dev ui-build services-up services-down services-logs services-reset migrate-up migrate-status

BINARY := bin/spacefleet
PKG    := ./cmd/spacefleet

run:
	go run $(PKG)

# Full production build: UI bundle + Go binary (with UI embedded).
build: ui-build
	go build -o $(BINARY) $(PKG)

test:
	go test ./...

fmt:
	go fmt ./...

vet:
	go vet ./...

tidy:
	go mod tidy

# Regenerate the ent client, Go server stubs, and TS client types.
gen:
	go generate ./ent/...
	go generate ./lib/api/...
	cd ui && npm run gen:api

# Apply pending migrations from db/migrations/ against $DATABASE_URL.
migrate-up:
	go run $(PKG) migrate up

# Show applied vs pending migrations.
migrate-status:
	go run $(PKG) migrate status

# Dev backend only. Run `make ui-dev` in a second terminal for the React dev server.
dev:
	air

ui-install:
	cd ui && npm install

# Vite dev server on :5173, proxies /api/* to the Go backend on :8080.
ui-dev:
	cd ui && npm run dev

ui-build:
	cd ui && npm run build
	@touch ui/dist/.gitkeep

clean:
	rm -rf bin tmp ui/dist ui/node_modules

# Start Postgres + Redis in the background.
services-up:
	docker compose up -d

services-down:
	docker compose down

services-logs:
	docker compose logs -f

# Wipe Postgres + Redis data volumes. Destructive.
services-reset:
	docker compose down -v
