# Spacefleet builder image

This is the container image Spacefleet runs as a Fargate task in the
customer's AWS account to clone a repo, build a Docker image with
Kaniko, and push the result to the customer's ECR. It speaks back to the
control plane via signed webhooks. Source of truth for the broader
design is [BUILD_PIPELINE.md](../BUILD_PIPELINE.md); this file documents
the runtime contract the worker (phase 5) needs to satisfy when
dispatching builds.

## Build locally

```sh
make builder-image
```

Produces `spacefleet-builder:dev` on your local Docker daemon. Override
with `IMAGE=...` for a custom tag.

## Env-var contract

All variables are required unless noted. The worker passes them via
`RunTask` `containerOverrides.environment`.

| Var | Description |
| --- | --- |
| `SPACEFLEET_BUILD_ID` | Opaque build identifier (uuid). Used in workdir + log lines. |
| `SPACEFLEET_WEBHOOK_URL` | Full URL to POST stage events to (e.g. `https://app.spacefleet.io/api/internal/builds/<id>/events`). |
| `SPACEFLEET_WEBHOOK_SECRET` | Per-build HMAC key (~32 bytes plaintext). |
| `GITHUB_TOKEN_SECRET_ARN` | Secrets Manager ARN holding the GitHub installation token (plaintext SecretString). |
| `REPO_FULL_NAME` | `owner/repo`, no `.git` suffix. |
| `COMMIT_SHA` | 40-char commit sha to check out. |
| `ECR_REPO` | Image destination, e.g. `<acct>.dkr.ecr.<region>.amazonaws.com/spacefleet-<app>`. |
| `ECR_CACHE_REPO` | Kaniko cache destination (same shape). |
| `AWS_REGION` | AWS region the task runs in. `AWS_DEFAULT_REGION` is also accepted. |

ECS task-role credentials are read by Kaniko from the metadata endpoint
automatically — the entrypoint doesn't pass anything explicit.

## Stages

The builder emits three stages, each as a `running` then a terminal
event (`succeeded` or `failed`):

1. **`clone`** — fetches the GitHub token from Secrets Manager, shallow
   clones `REPO_FULL_NAME` at `COMMIT_SHA`.
2. **`build`** — runs Kaniko against `./Dockerfile` in the cloned tree,
   pushing to `ECR_REPO:COMMIT_SHA` with `ECR_CACHE_REPO` for layer
   reuse.
3. **`push`** — reporting only; Kaniko has already pushed by the time
   we get here. The terminal event includes
   `data.image_uri` and `data.image_digest`.

If the script crashes unexpectedly mid-stage, the ERR trap posts a
`failed` event for whichever stage was active before exiting non-zero.
The control plane's polling backstop catches cases where even that
fails (e.g. webhook URL unreachable).

## Webhook signing

Every POST is signed with the per-build secret:

```
signed_payload = "<unix-seconds>.<raw-body>"
signature      = hex(HMAC-SHA256(SPACEFLEET_WEBHOOK_SECRET, signed_payload))
```

Sent as `X-Spacefleet-Timestamp` + `X-Spacefleet-Signature`. The control
plane rejects events outside a 5-minute timestamp drift window — see
BUILD_PIPELINE.md > Webhook freshness vs build duration.

## Local smoke test

You can exercise the script outside Docker by stubbing the externals on
PATH and pointing `SPACEFLEET_WEBHOOK_URL` at a local server:

```sh
# fake aws/git/kaniko on a private PATH, point WEBHOOK_URL at a local server
KANIKO_BIN=$(pwd)/test-fakes/kaniko \
SPACEFLEET_WORKDIR=$(mktemp -d) \
  SPACEFLEET_BUILD_ID=test \
  SPACEFLEET_WEBHOOK_URL=http://localhost:9000/events \
  SPACEFLEET_WEBHOOK_SECRET=secret \
  ... \
  ./entrypoint.sh
```

The Go test driver in `entrypoint_test.go` does exactly this and is the
canonical source for what a passing run looks like. Run it with:

```sh
go test ./builder/...
```
