.PHONY: run build test fmt vet tidy dev clean gen ui-install ui-dev ui-build services-up services-down services-logs services-reset

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

# Regenerate Go server stubs and TS client types from api/openapi.yaml.
gen:
	go generate ./lib/api/...
	cd ui && npm run gen:api

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
