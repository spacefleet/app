# Self-hosting Spacefleet

Spacefleet is open source end-to-end. Hosted Spacefleet is the same codebase with us operating the control plane; running it yourself uses the same images, config surface, and migration story. This doc is the operator's checklist for standing up a self-hosted install.

> Status: this doc covers everything required for the *build pipeline* phase (apps + builds, no deploys yet). Sections will grow as deploys, environments, and runtime infra land.

## Prerequisites

- **AWS account** for the control plane. This is the account that:
  - Holds the Pulumi state bucket + KMS key.
  - Customer IAM trust policies will allow `sts:AssumeRole` from.
  - Talks to customer cloud accounts via STS.
- **GitHub App** registered against your domain. Hosted Spacefleet operates a public App; self-hosters register their own. The App needs:
  - Repository permissions: `Contents: Read`, `Metadata: Read`, `Pull requests: Read`.
  - Subscribe to events: `push`, `pull_request`, `installation`, `installation_repositories`.
  - Webhook URL: `https://<your-host>/api/github/webhook`.
- **Postgres 14+** for application data and the River job queue.
- **Pulumi CLI** on every host that runs `spacefleet worker`. The runtime Docker image ships with it; for bare-metal self-hosters, `brew install pulumi` (mac) or follow [pulumi.com/docs/install](https://www.pulumi.com/docs/install/).
- **Public reachability** of the Spacefleet HTTP service. Builder Fargate tasks running in customer accounts POST webhook events back to your install — the URL configured in `SPACEFLEET_PUBLIC_URL` must be reachable from the public internet.

## Two processes

Spacefleet ships as a single binary, but in production you run two long-lived processes from it:

```sh
spacefleet serve     # HTTP API on $ADDR (default :8080)
spacefleet worker    # River-backed background jobs (builds, destroys, …)
```

Both use the same `.env`. The HTTP server is stateless; the worker holds short-lived state for in-flight builds and reattaches on restart.

In development:

```sh
make services-up   # Postgres + Redis via docker-compose
make dev           # serve, with Air live-reload, on :8080
make worker        # in a second terminal
make ui-dev        # Vite dev server on :5173, in a third terminal
```

In Docker / Kubernetes: deploy the same image twice, with command override `serve` and `worker` respectively. Scale `serve` horizontally; scale `worker` based on build queue depth (typically a single replica is fine until you have many concurrent builds).

## One-time bootstrap

### 1. Create the Pulumi state backend

`make bootstrap-state` creates an S3 bucket + KMS key in your control-plane AWS account:

```sh
make bootstrap-state BUCKET=spacefleet-state-acme REGION=us-east-1
```

The script is idempotent — re-running with the same args is safe. It prints the env vars to copy into `.env` at the end.

What it provisions:
- An S3 bucket with versioning enabled and SSE-KMS using the key below. Public access is blocked.
- A customer-managed KMS key with auto-rotation enabled.
- A bucket policy that requires `aws:SecureTransport`.

You provide:
- `--bucket` (required): a globally-unique S3 bucket name. We recommend the form `spacefleet-state-<your-org>`.
- `--region` (default `us-east-1`).
- `--alias` (default `spacefleet/state`): the KMS alias suffix.

### 2. Set required env vars

Copy `.env.example` to `.env` and fill in the values. The build pipeline needs:

| Var | Purpose |
| --- | --- |
| `DATABASE_URL` | Postgres connection string (used by both processes). |
| `SPACEFLEET_PUBLIC_URL` | Externally-reachable base URL of your install. Webhooks from builder tasks POST here. |
| `SPACEFLEET_STATE_BUCKET` | From `make bootstrap-state`. |
| `SPACEFLEET_STATE_BUCKET_REGION` | From `make bootstrap-state`. |
| `SPACEFLEET_STATE_KMS_KEY_ARN` | From `make bootstrap-state`. |
| `AWS_PLATFORM_ACCOUNT_ID` | The 12-digit ID of your control-plane AWS account. |
| `AWS_CFN_TEMPLATE_URL` | Public URL of the onboarding CloudFormation template. Defaults to ours; fork and host your own if you want. |
| `GITHUB_APP_ID`, `GITHUB_APP_SLUG`, `GITHUB_APP_PRIVATE_KEY[_PATH]`, `GITHUB_APP_WEBHOOK_SECRET` | Your GitHub App's credentials. |
| `CLERK_PUBLISHABLE_KEY`, `CLERK_SECRET_KEY` | Auth provider; you need a Clerk project. |

Optional knobs:

| Var | Default | Purpose |
| --- | --- | --- |
| `SPACEFLEET_BUILDER_IMAGE` | digest baked at release | Override the builder image (local dev / forks). |
| `SPACEFLEET_WORKER_CONCURRENCY` | `4` | Max concurrent River jobs. |
| `SPACEFLEET_BUILD_TIMEOUT` | `60m` | Hard ceiling per build before `StopTask` fires. |

### 3. Apply migrations

```sh
spacefleet migrate up
```

This applies ent's atlas-style SQL migrations. The worker applies River's bundled migrations on its own startup; you don't run them by hand.

### 4. Start the processes

```sh
spacefleet serve   # one or more replicas
spacefleet worker  # one replica is plenty for v1
```

The worker logs `worker: river migrations up to date` on startup once it's done, then heartbeats every 30s. Watch for either of those before assuming the worker is healthy.

## Builder image

The runtime builder (Kaniko + entrypoint script that clones, builds, pushes, and posts webhooks back to the control plane) is published as a public package on GHCR:

```
ghcr.io/spacefleet/spacefleet-app/builder:<release-tag>
```

The Spacefleet binary has the digest of the matching builder image baked in via `-ldflags` at release time, so a stock `spacefleet` always knows exactly which builder it expects. Self-hosters who don't fork the binary can leave `SPACEFLEET_BUILDER_IMAGE` unset.

If you fork the builder, set `SPACEFLEET_BUILDER_IMAGE` to your digest-pinned reference, e.g.:

```
SPACEFLEET_BUILDER_IMAGE=ghcr.io/your-org/spacefleet-builder:v1.2.3@sha256:abcdef...
```

We strongly recommend pinning by digest (`@sha256:...`) — a re-tagged release otherwise silently changes what every customer's account runs.

## What you don't have to think about

These are things hosted Spacefleet does that self-hosters get for free:

- **The state bucket and KMS key are pre-provisioned in our control-plane account** for hosted; for self-hosters, `make bootstrap-state` creates them in yours.
- **The GitHub App is operated by us** for hosted; self-hosters register their own.
- **The builder image is publicly available** on GHCR — no registry credentials required, regardless of who operates the install.

## Things to flag

- **Redis is in `docker-compose.yml` but unused by the build pipeline.** It's wired for future caching; no action required from operators today.
- **AWS credentials** for the control plane process come from the standard AWS SDK chain (env vars, profile, IAM role for the EC2/ECS task). The worker uses these creds to AssumeRole into customer accounts; the role you grant Spacefleet must have `sts:AssumeRole` on `arn:aws:iam::*:role/spacefleet-*` and not much else.
- **Network reachability for webhooks.** Builder tasks running inside customer AWS accounts POST events back to `SPACEFLEET_PUBLIC_URL`. If you're behind a corporate firewall, expose the install via an ingress that accepts public HTTPS — this is the *one* path that has to be reachable from outside.
