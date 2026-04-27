.PHONY: run build test fmt vet tidy dev worker clean gen ui-install ui-dev ui-build services-up services-down services-logs services-reset migrate-up migrate-status bootstrap-state infra-build builder-image

BINARY := bin/spacefleet
PKG    := ./cmd/spacefleet
INFRA_BINARY := bin/spacefleet-infra
INFRA_PKG    := ./cmd/spacefleet-infra

# Builder image — built locally for end-to-end tests against a real
# AWS account. The release pipeline (.github/workflows/ci.yml) is what
# publishes the canonical digest-pinned image to GHCR; this target is
# for development.
BUILDER_IMAGE ?= spacefleet-builder:dev
BUILDER_PLATFORM ?= linux/amd64

run:
	go run $(PKG) serve

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

# Long-lived worker process. Drives River-backed jobs (build, destroy_app).
# Run alongside `make dev` in a second terminal in development.
worker:
	go run $(PKG) worker

# Provision the Pulumi state backend (S3 bucket + KMS key) in the
# control-plane AWS account this CLI is configured for. Idempotent.
# Required before the worker can dispatch builds.
bootstrap-state:
	@if [ -z "$(BUCKET)" ]; then echo "usage: make bootstrap-state BUCKET=<name> [REGION=us-east-1]"; exit 2; fi
	./scripts/bootstrap-state.sh --bucket "$(BUCKET)" $(if $(REGION),--region $(REGION),)

# Build the dev-only spacefleet-infra CLI for driving Pulumi stacks
# end-to-end against a connected AWS account. Phase 2 demos use this;
# the worker (phase 5) calls the same Orchestrator in-process.
infra-build:
	go build -o $(INFRA_BINARY) $(INFRA_PKG)

# Build the builder Docker image locally for smoke testing. Defaults
# to linux/amd64 since v1 builders are amd64-only; override
# BUILDER_PLATFORM=linux/arm64 for laptop-arch local runs that don't
# need to push to ECR (the customer-side ECS task always runs amd64).
#
#   make builder-image                            # spacefleet-builder:dev (amd64)
#   make builder-image BUILDER_IMAGE=foo:bar      # custom tag
#   make builder-image BUILDER_PLATFORM=linux/arm64  # for native runs on M-series
builder-image:
	docker buildx build \
		--platform $(BUILDER_PLATFORM) \
		--load \
		-t $(BUILDER_IMAGE) \
		-f builder/Dockerfile \
		builder

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
