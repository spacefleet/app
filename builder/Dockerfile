# syntax=docker/dockerfile:1.7
#
# Spacefleet builder image. Pulls Kaniko's executor + ECR credential
# helpers from the upstream `kaniko-debug` image and lays them on top of
# Alpine so we can `apk add` the runtime dependencies the entrypoint
# script needs (aws CLI, git, curl, jq, openssl, bash). Plain
# `kaniko-debug` is distroless and has no package manager — that's why
# this image deviates from the BUILD_PIPELINE plan's "FROM
# kaniko-debug" sketch. The deviation is documented in
# BUILD_PIPELINE.md > Builder image.
#
# Targets linux/amd64 only for v1 (multi-arch is out of scope per
# BUILD_PIPELINE.md > v1 scope). Kaniko's binaries are statically
# compiled Go, so they run unchanged on Alpine/musl.

# Kaniko's executor image. Pinned by digest so a re-tag of v1.23.2-debug
# upstream can't silently change the binary that lands in our builder.
# Bumping Kaniko = update both the human-readable tag (for code review)
# and the digest (for reproducibility).
ARG KANIKO_IMAGE=gcr.io/kaniko-project/executor:v1.23.2-debug@sha256:c3109d5926a997b100c4343944e06c6b30a6804b2f9abe0994d3de6ef92b028e

FROM ${KANIKO_IMAGE} AS kaniko

FROM alpine:3.20

# Runtime dependencies. Versions float within the Alpine 3.20 channel;
# we pin the base image tag, which is enough determinism for v1.
#   aws-cli       — Secrets Manager fetch + ECR helper fallback
#   bash          — entrypoint.sh requires bash arrays + ERR trap
#   ca-certificates — outbound TLS to GitHub, ECR, AWS APIs
#   curl          — webhook POSTs back to the control plane
#   git           — shallow fetch of the customer's repo
#   jq            — JSON building / parsing in entrypoint.sh
#   openssl       — HMAC-SHA256 webhook signing
#   tini          — PID 1 reaper so any kaniko-spawned children get reaped
RUN apk add --no-cache \
      aws-cli \
      bash \
      ca-certificates \
      curl \
      git \
      jq \
      openssl \
      tini

# Kaniko's executor + the docker-credential-* helpers + its CA bundle
# all live under /kaniko in the upstream image. Copying the whole tree
# keeps it findable by go-containerregistry's keychain (which expects
# /kaniko/.docker/config.json for the ECR helper wiring).
COPY --from=kaniko /kaniko /kaniko

# Kaniko's bin dir on PATH so the entrypoint can spawn `executor`
# without an absolute path. DOCKER_CONFIG points at the bundled
# credential-helper config so ECR auth works without extra setup.
ENV PATH="/kaniko:${PATH}" \
    DOCKER_CONFIG=/kaniko/.docker \
    SSL_CERT_DIR=/kaniko/ssl/certs

# Builds run in a writable directory. /workspace is the convention; the
# entrypoint mkdir's a per-build subdir so concurrent builds in one
# container (we don't currently allow it, but cheap insurance) don't
# trip on each other.
RUN mkdir -p /workspace && chmod 0777 /workspace
WORKDIR /workspace

COPY entrypoint.sh /usr/local/bin/spacefleet-builder
RUN chmod 0755 /usr/local/bin/spacefleet-builder

# tini handles signal forwarding + zombie reaping. Kaniko itself
# doesn't fork much, but git/aws/curl all do, and a clean PID 1 is
# cheap insurance against orphaned processes inside the Fargate task.
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/spacefleet-builder"]
