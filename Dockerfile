# syntax=docker/dockerfile:1.7

# ---- UI build ----------------------------------------------------------------
# Pinned to BUILDPLATFORM: the JS bundle is arch-independent, so running this
# stage under QEMU for every target would be pure waste.
FROM --platform=$BUILDPLATFORM node:20-alpine AS ui
WORKDIR /ui

# Install deps first so they cache independently of UI source changes.
COPY ui/package.json ui/package-lock.json ./
RUN npm ci

COPY ui/ ./
RUN npm run build

# ---- Go build ----------------------------------------------------------------
# Also pinned to BUILDPLATFORM — we cross-compile via GOARCH=$TARGETARCH
# instead of running the Go toolchain under emulation.
FROM --platform=$BUILDPLATFORM golang:1.25.1-alpine AS go
WORKDIR /src

ARG TARGETOS
ARG TARGETARCH

# Module cache: keep this layer separate from source copies.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# Everything needed to compile. Generated files (lib/api/gen.go,
# ui/src/api/schema.d.ts) are checked in, so no codegen step here.
COPY cmd/ cmd/
COPY lib/ lib/
COPY api/ api/
COPY ui/embed.go ui/

# The SPA bundle the Go binary embeds via //go:embed all:dist.
COPY --from=ui /ui/dist ui/dist

ENV CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/spacefleet ./cmd/spacefleet

# ---- Runtime -----------------------------------------------------------------
# distroless/static is ~2MB, runs as a non-root user, and has ca-certs for
# outbound HTTPS (needed by the Clerk SDK's JWKS fetch).
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=go /out/spacefleet /spacefleet

EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/spacefleet"]
