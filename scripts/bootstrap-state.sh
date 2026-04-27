#!/usr/bin/env bash
# bootstrap-state.sh — provision the Pulumi state backend for one
# Spacefleet installation in the control-plane AWS account.
#
# What it creates (idempotently):
#   1. An S3 bucket with versioning enabled and SSE-KMS using the key below.
#   2. A KMS CMK with auto-rotation enabled, alias spacefleet/state-<bucket>.
#   3. A bucket policy that requires aws:SecureTransport for all access.
#
# Reading from the BUILD_PIPELINE plan:
#   - One bucket per Spacefleet installation.
#   - Per-cloud-account KMS keys come later; for v1 a single per-installation
#     key is fine and we wire it through SPACEFLEET_STATE_KMS_KEY_ARN.
#
# Inputs (env vars or flags; flags win):
#   --bucket    -b    target bucket name (required)
#   --region    -r    target region                     [default: us-east-1]
#   --alias     -a    KMS alias (no "alias/" prefix)    [default: spacefleet/state]
#
# Authentication: relies on the operator's existing AWS CLI config — same
# default-chain you'd use for any aws-cli command. The script never reads
# or writes credentials.
#
# Re-running with the same args is a no-op + prints the existing values.
set -euo pipefail

usage() {
  cat <<EOF
usage: $(basename "$0") --bucket <name> [--region us-east-1] [--alias spacefleet/state]

  Required:
    -b, --bucket   S3 bucket name (must be globally unique).

  Optional:
    -r, --region   AWS region        [default: us-east-1]
    -a, --alias    KMS alias suffix  [default: spacefleet/state]

Re-running is safe: existing resources are reused.
EOF
}

BUCKET=""
REGION="${SPACEFLEET_STATE_BUCKET_REGION:-us-east-1}"
ALIAS_SUFFIX="spacefleet/state"

while [[ $# -gt 0 ]]; do
  case "$1" in
    -b|--bucket) BUCKET="$2"; shift 2 ;;
    -r|--region) REGION="$2"; shift 2 ;;
    -a|--alias)  ALIAS_SUFFIX="$2"; shift 2 ;;
    -h|--help)   usage; exit 0 ;;
    *) echo "unknown arg: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [[ -z "${BUCKET}" ]]; then
  echo "error: --bucket is required" >&2
  usage >&2
  exit 2
fi

if ! command -v aws >/dev/null 2>&1; then
  echo "error: aws CLI not found on \$PATH (install via brew install awscli)" >&2
  exit 1
fi

if ! aws sts get-caller-identity >/dev/null 2>&1; then
  echo "error: aws sts get-caller-identity failed — are credentials configured?" >&2
  exit 1
fi

ACCOUNT_ID="$(aws sts get-caller-identity --query Account --output text)"
ALIAS_NAME="alias/${ALIAS_SUFFIX}"

echo "==> account: ${ACCOUNT_ID}"
echo "==> region:  ${REGION}"
echo "==> bucket:  ${BUCKET}"
echo "==> kms:     ${ALIAS_NAME}"
echo

# ---- 1. KMS key -------------------------------------------------------------
KEY_ID="$(aws kms describe-key --key-id "${ALIAS_NAME}" --region "${REGION}" \
  --query KeyMetadata.KeyId --output text 2>/dev/null || true)"

if [[ -z "${KEY_ID}" ]]; then
  echo "==> creating KMS key"
  KEY_ID="$(aws kms create-key --region "${REGION}" \
    --description "Spacefleet Pulumi state encryption (${BUCKET})" \
    --tags TagKey=spacefleet,TagValue=true \
    --query KeyMetadata.KeyId --output text)"
  aws kms create-alias --region "${REGION}" \
    --alias-name "${ALIAS_NAME}" --target-key-id "${KEY_ID}"
  aws kms enable-key-rotation --region "${REGION}" --key-id "${KEY_ID}"
  echo "    created key ${KEY_ID} with alias ${ALIAS_NAME}"
else
  echo "==> reusing existing KMS key ${KEY_ID}"
fi

KEY_ARN="$(aws kms describe-key --key-id "${KEY_ID}" --region "${REGION}" \
  --query KeyMetadata.Arn --output text)"

# ---- 2. S3 bucket -----------------------------------------------------------
if aws s3api head-bucket --bucket "${BUCKET}" 2>/dev/null; then
  echo "==> reusing existing bucket ${BUCKET}"
else
  echo "==> creating bucket ${BUCKET}"
  if [[ "${REGION}" == "us-east-1" ]]; then
    aws s3api create-bucket --bucket "${BUCKET}" --region "${REGION}"
  else
    aws s3api create-bucket --bucket "${BUCKET}" --region "${REGION}" \
      --create-bucket-configuration LocationConstraint="${REGION}"
  fi
fi

# Block all public access.
aws s3api put-public-access-block --bucket "${BUCKET}" \
  --public-access-block-configuration '{
    "BlockPublicAcls": true,
    "IgnorePublicAcls": true,
    "BlockPublicPolicy": true,
    "RestrictPublicBuckets": true
  }'

# Versioning is non-negotiable; state-file recovery depends on it.
aws s3api put-bucket-versioning --bucket "${BUCKET}" \
  --versioning-configuration Status=Enabled

# Default encryption with the KMS key.
aws s3api put-bucket-encryption --bucket "${BUCKET}" \
  --server-side-encryption-configuration "{
    \"Rules\": [{
      \"ApplyServerSideEncryptionByDefault\": {
        \"SSEAlgorithm\": \"aws:kms\",
        \"KMSMasterKeyID\": \"${KEY_ARN}\"
      },
      \"BucketKeyEnabled\": true
    }]
  }"

# TLS-only bucket policy. Cheap belt-and-suspenders against accidental
# misuse from inside a misconfigured network.
aws s3api put-bucket-policy --bucket "${BUCKET}" \
  --policy "{
    \"Version\": \"2012-10-17\",
    \"Statement\": [{
      \"Sid\": \"DenyInsecureTransport\",
      \"Effect\": \"Deny\",
      \"Principal\": \"*\",
      \"Action\": \"s3:*\",
      \"Resource\": [
        \"arn:aws:s3:::${BUCKET}\",
        \"arn:aws:s3:::${BUCKET}/*\"
      ],
      \"Condition\": {
        \"Bool\": { \"aws:SecureTransport\": \"false\" }
      }
    }]
  }"

echo
echo "==> done. Add to your .env:"
echo
cat <<EOF
SPACEFLEET_STATE_BUCKET=${BUCKET}
SPACEFLEET_STATE_BUCKET_REGION=${REGION}
SPACEFLEET_STATE_KMS_KEY_ARN=${KEY_ARN}
EOF
