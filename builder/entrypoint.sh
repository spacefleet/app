#!/usr/bin/env bash
# Spacefleet builder entrypoint — runs inside the per-build Fargate task.
#
# Contract (env vars, all required):
#   SPACEFLEET_BUILD_ID         opaque build identifier (uuid)
#   SPACEFLEET_WEBHOOK_URL      full URL to POST stage events to
#   SPACEFLEET_WEBHOOK_SECRET   per-build HMAC key (plaintext, ~32 bytes)
#   GITHUB_TOKEN_SECRET_ARN     Secrets Manager ARN holding the install token
#   REPO_FULL_NAME              "owner/repo" (no .git suffix)
#   COMMIT_SHA                  full 40-char commit sha to build
#   ECR_REPO                    image dest, e.g. <acct>.dkr.ecr.<region>.amazonaws.com/<name>
#   ECR_CACHE_REPO              kaniko cache dest (same shape as ECR_REPO)
#   AWS_REGION (or AWS_DEFAULT_REGION)
#
# Stages emitted: clone, build, push. Each transitions running -> succeeded
# or running -> failed. On unexpected exit the ERR trap posts <stage> failed
# for whichever stage was active. See BUILD_PIPELINE.md for the full
# webhook scheme; this script implements the builder side of it.
#
# Test affordances: KANIKO_BIN and SPACEFLEET_WORKDIR can be overridden so
# the Go test driver in builder/entrypoint_test.go can mock kaniko and run
# in a temp dir without /workspace.

set -Eeuo pipefail

readonly REQUIRED_ENV=(
  SPACEFLEET_BUILD_ID
  SPACEFLEET_WEBHOOK_URL
  SPACEFLEET_WEBHOOK_SECRET
  GITHUB_TOKEN_SECRET_ARN
  REPO_FULL_NAME
  COMMIT_SHA
  ECR_REPO
  ECR_CACHE_REPO
)

# Mutable globals the helpers + ERR trap rely on. `current_stage` is the
# linchpin: stage_start/stage_done update it so on_error knows which event
# to attribute a crash to.
current_stage=""

log()  { printf '[builder] %s\n' "$*" >&2; }
die()  { log "ERROR: $*"; exit 1; }

# require_env errors out with a clear message if any contract var is missing.
# Runs before we know which stage we're in, so failures here surface only
# via the task's exit code (the polling backstop on the control plane
# catches it via DescribeTasks).
require_env() {
  local missing=()
  local v
  for v in "${REQUIRED_ENV[@]}"; do
    if [[ -z "${!v:-}" ]]; then
      missing+=("$v")
    fi
  done
  if [[ -z "${AWS_REGION:-}" && -n "${AWS_DEFAULT_REGION:-}" ]]; then
    export AWS_REGION="$AWS_DEFAULT_REGION"
  fi
  if [[ -z "${AWS_REGION:-}" ]]; then
    missing+=("AWS_REGION")
  fi
  if (( ${#missing[@]} > 0 )); then
    die "missing required env: ${missing[*]}"
  fi
}

# hmac_sign computes hex(HMAC-SHA256(secret, "<timestamp>.<body>")) using
# openssl. Reads body on stdin to avoid arg-list quoting issues with
# JSON containing single quotes / shell metacharacters.
hmac_sign() {
  local timestamp="$1" secret="$2"
  # openssl appends "(stdin)= <hex>" to its output; awk grabs the hex.
  { printf '%s.' "$timestamp"; cat; } \
    | openssl dgst -sha256 -hmac "$secret" \
    | awk '{print $NF}'
}

# build_event_body emits the JSON request body for one stage event.
# `data` is optional; when present it must already be a JSON value
# (object/string/etc.) — we splat it via --argjson.
build_event_body() {
  local stage="$1" status="$2" data="${3:-}"
  if [[ -n "$data" ]]; then
    jq -cn --arg stage "$stage" --arg status "$status" --argjson data "$data" \
      '{stage:$stage, status:$status, data:$data}'
  else
    jq -cn --arg stage "$stage" --arg status "$status" \
      '{stage:$stage, status:$status}'
  fi
}

# post_event POSTs one stage event to the control plane. Returns 0 on
# 2xx, non-zero on any HTTP error or transport failure. The caller decides
# whether a failure here is fatal (it isn't, in the trap path).
post_event() {
  local stage="$1" status="$2" data="${3:-}"
  local body timestamp signature
  body="$(build_event_body "$stage" "$status" "$data")"
  timestamp="$(date -u +%s)"
  signature="$(printf '%s' "$body" | hmac_sign "$timestamp" "$SPACEFLEET_WEBHOOK_SECRET")"
  curl -fsS \
    --max-time 30 \
    --retry 3 --retry-delay 2 --retry-connrefused \
    -H 'Content-Type: application/json' \
    -H "X-Spacefleet-Timestamp: ${timestamp}" \
    -H "X-Spacefleet-Signature: ${signature}" \
    --data-binary "$body" \
    "$SPACEFLEET_WEBHOOK_URL" >/dev/null
}

stage_start()    { current_stage="$1"; post_event "$1" "running"; }
stage_succeed()  { post_event "$1" "succeeded" "${2:-}"; current_stage=""; }
stage_fail()     {
  local err_json
  err_json="$(jq -cn --arg e "$2" '{error:$e}')"
  post_event "$1" "failed" "$err_json" || log "warning: failed to post stage_fail for $1"
  current_stage=""
}

# on_error is the safety net: any unexpected non-zero exit inside the
# script lands here, attributes the failure to whatever stage was active,
# and propagates the original exit code so ECS sees a non-zero stop.
on_error() {
  local rc=$?
  trap - ERR  # disarm so post_event failures don't recurse.
  if [[ -n "$current_stage" ]]; then
    stage_fail "$current_stage" "builder exited unexpectedly (rc=${rc})"
  fi
  exit "$rc"
}

# fetch_github_token pulls the GitHub installation token the worker
# wrote to Secrets Manager just before dispatching this task. We treat
# an empty SecretString as fatal — it almost certainly means the worker
# wrote the wrong format.
fetch_github_token() {
  local token
  token="$(aws secretsmanager get-secret-value \
    --region "$AWS_REGION" \
    --secret-id "$GITHUB_TOKEN_SECRET_ARN" \
    --query SecretString --output text)"
  if [[ -z "$token" || "$token" == "None" ]]; then
    die "Secrets Manager returned empty SecretString for ${GITHUB_TOKEN_SECRET_ARN}"
  fi
  printf '%s' "$token"
}

# clone_repo does a shallow fetch of the exact commit SHA. We use
# http.extraHeader rather than embedding the token in the remote URL so
# the token doesn't appear in /proc/<pid>/cmdline, `git remote -v`, or
# error messages git might print on failure.
clone_repo() {
  local repo_dir="$1" token="$2"
  mkdir -p "$repo_dir"
  (
    cd "$repo_dir"
    git init -q
    git remote add origin "https://github.com/${REPO_FULL_NAME}.git"
    local auth
    auth="Authorization: Basic $(printf 'x-access-token:%s' "$token" | base64 | tr -d '\n')"
    git -c "http.extraHeader=${auth}" \
        fetch --depth 1 --no-tags origin "${COMMIT_SHA}"
    git checkout -q FETCH_HEAD
  )
}

# run_kaniko shells out to the executor. Kaniko reads task-role creds
# from the ECS metadata endpoint (AWS_CONTAINER_CREDENTIALS_RELATIVE_URI)
# automatically — no explicit auth wiring required here. Writes the
# resulting image digest to a file we then read back.
run_kaniko() {
  local context="$1" digest_file="$2"
  local kaniko_bin="${KANIKO_BIN:-/kaniko/executor}"
  "$kaniko_bin" \
    --context "dir://${context}" \
    --dockerfile Dockerfile \
    --destination "${ECR_REPO}:${COMMIT_SHA}" \
    --cache=true \
    --cache-repo "${ECR_CACHE_REPO}" \
    --digest-file "$digest_file" \
    --log-format text
}

main() {
  require_env
  trap on_error ERR

  local workdir_base="${SPACEFLEET_WORKDIR:-/workspace}"
  local workdir="${workdir_base}/${SPACEFLEET_BUILD_ID}"
  mkdir -p "$workdir"

  # --- clone -------------------------------------------------------------
  stage_start "clone"
  local token
  token="$(fetch_github_token)"
  clone_repo "${workdir}/repo" "$token"
  unset token  # token's job is done; don't keep it alive across kaniko
  stage_succeed "clone"

  # --- build (kaniko) ----------------------------------------------------
  stage_start "build"
  local digest_file="${workdir}/digest"
  run_kaniko "${workdir}/repo" "$digest_file"
  local digest
  digest="$(cat "$digest_file")"
  if [[ -z "$digest" ]]; then
    stage_fail "build" "kaniko did not write a digest"
    exit 1
  fi
  stage_succeed "build"

  # --- push (reporting only — kaniko already pushed) ---------------------
  stage_start "push"
  local image_uri="${ECR_REPO}:${COMMIT_SHA}"
  local push_data
  push_data="$(jq -cn --arg uri "$image_uri" --arg digest "$digest" \
    '{image_uri:$uri, image_digest:$digest}')"
  stage_succeed "push" "$push_data"

  log "build complete: ${image_uri}@${digest}"
}

main "$@"
