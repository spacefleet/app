# syntax=docker/dockerfile:1.7

# Pulumi CLI version. Release CI passes this from .tool-versions so the
# runtime image always matches local dev (asdf reads .tool-versions).
# The default below is only consumed by ad-hoc `docker build` runs that
# don't override it — keep it in lockstep with .tool-versions.
ARG PULUMI_VERSION=3.232.0

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
FROM --platform=$BUILDPLATFORM golang:1.25.8-alpine AS go
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
COPY ent/ ent/
COPY ui/embed.go ui/

# The SPA bundle the Go binary embeds via //go:embed all:dist.
COPY --from=ui /ui/dist ui/dist

ENV CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH

# BUILDER_IMAGE_REF is the digest-pinned builder image this binary
# defaults to dispatching builds with. Release CI sets it after the
# builder image is pushed; stock `docker build` leaves it empty and the
# binary then requires SPACEFLEET_BUILDER_IMAGE at runtime. See
# lib/config.DefaultBuilderImage and BUILD_PIPELINE.md > Builder image.
ARG BUILDER_IMAGE_REF=""
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath \
      -ldflags="-s -w -X github.com/spacefleet/app/lib/config.DefaultBuilderImage=${BUILDER_IMAGE_REF}" \
      -o /out/spacefleet ./cmd/spacefleet

# ---- Pulumi CLI fetcher ------------------------------------------------------
# Fetch the Pulumi CLI tarball for the target platform once, in its own
# stage, so the layer caches across rebuilds and we don't have curl in
# the runtime layer.
FROM --platform=$TARGETPLATFORM alpine:3.20 AS pulumi
ARG PULUMI_VERSION
ARG TARGETARCH
RUN apk add --no-cache curl tar
RUN case "$TARGETARCH" in \
      amd64) PULUMI_ARCH=x64 ;; \
      arm64) PULUMI_ARCH=arm64 ;; \
      *) echo "unsupported arch: $TARGETARCH" >&2; exit 1 ;; \
    esac && \
    curl -fsSL "https://get.pulumi.com/releases/sdk/pulumi-v${PULUMI_VERSION}-linux-${PULUMI_ARCH}.tar.gz" \
      | tar -xz -C /tmp && \
    mv /tmp/pulumi /opt/pulumi

# ---- Runtime -----------------------------------------------------------------
# debian:bookworm-slim instead of distroless/static — the worker process
# spawns the pulumi CLI as a subprocess, which in turn spawns provider
# plugins. distroless/static doesn't ship a shell or libc that pulumi's
# plugin loader expects. bookworm-slim is ~75MB, runs as a non-root user
# we add explicitly, and stays small enough not to regret.
FROM debian:bookworm-slim

# ca-certificates: outbound HTTPS to GitHub, AWS, Clerk, ECR.
# git: required by Pulumi's git-source backend (not used by us today,
# but Pulumi calls it on `pulumi new` and a few other paths).
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git \
    && rm -rf /var/lib/apt/lists/*

# Pulumi CLI on $PATH. We copy only the bin directory; the rest of the
# tarball is plugins we don't need shipped (Pulumi downloads provider
# plugins on demand into PULUMI_HOME).
COPY --from=pulumi /opt/pulumi/ /usr/local/bin/

RUN useradd --system --create-home --uid 65532 --user-group spacefleet
USER spacefleet
WORKDIR /home/spacefleet

COPY --from=go --chown=spacefleet:spacefleet /out/spacefleet /usr/local/bin/spacefleet

EXPOSE 8080
# Default to `serve`; deployments override this to `worker` for the
# background-job process. Existing setups that ran `./spacefleet` still
# get the HTTP server.
ENTRYPOINT ["/usr/local/bin/spacefleet"]
CMD ["serve"]
