// Package pulumi is the Spacefleet wrapper around Pulumi's Go Automation
// API. It encapsulates two things callers care about and one thing they
// don't:
//
//   - Backend addressing: every stack lives under
//     s3://<state-bucket>/<org-id>/<scope>/<stack-name>/. Callers pass an
//     intent (org + cloud-account or org + app, stack name) and we build
//     the URL.
//
//   - Cross-account credentials: the worker runs in the control-plane
//     account; Pulumi resources land in the customer's account. We fan
//     out STS credentials into env vars Pulumi's AWS provider picks up
//     (AWS_ACCESS_KEY_ID/SECRET_ACCESS_KEY/SESSION_TOKEN). The worker
//     assumes the customer's role once per stack run and the resulting
//     short-lived credentials are scoped to that run only.
//
//   - The Pulumi CLI: Automation API spawns `pulumi` as a subprocess,
//     so the host needs the binary on $PATH. Our Dockerfile installs
//     it; local dev requires `brew install pulumi`. We don't reimplement
//     anything Pulumi already does.
//
// The high-level entry point is [Orchestrator]: it wires Backend +
// inline programs (infra/stacks/{builderinfra,appbuild}) + cross-account
// credentials together and exposes Up/Destroy methods the worker and the
// dev CLI both call.
package pulumi

import (
	"errors"
	"fmt"
	"strings"
)

// ProjectName is the Pulumi project name we use for every stack. Pulumi
// requires *some* project name; making it a single shared value keeps
// the state path predictable: the project doesn't appear in the s3://
// URL we configure, only in the JSON inside.
const ProjectName = "spacefleet"

// Stack identifiers in the BUILD_PIPELINE doc:
//
//	<org-id>/<cloud-account-id>/builder-infra
//	<org-id>/<app-id>/build
//
// These constants name the stack-name portion (the last segment); the
// path layout uses them as both the path component and the Pulumi stack
// name to keep one-to-one mapping.
const (
	StackBuilderInfra = "builder-infra"
	StackAppBuild     = "build"
)

// Backend describes the s3:// URL and the awskms:// secrets provider
// for a single stack. Callers feed this into NewRunner; tests assert
// on the stringified form to lock in path generation.
type Backend struct {
	StateURL        string // s3://bucket/<scope>/<stack>?region=...
	SecretsProvider string // awskms://<key-arn>?region=...
	StackName       string // the Pulumi stack name (last path segment)
}

// BackendConfig is the system-wide backend wiring read from env. It
// applies uniformly across all stacks for one Spacefleet installation.
type BackendConfig struct {
	Bucket    string // SPACEFLEET_STATE_BUCKET
	Region    string // SPACEFLEET_STATE_BUCKET_REGION
	KMSKeyARN string // SPACEFLEET_STATE_KMS_KEY_ARN (per-installation in v1; per-cloud-account in a future phase)
}

// Validate returns a precise error for the first missing field. The
// worker calls this at startup so an operator sees one clear message
// instead of a Pulumi error 30s into a build.
func (b BackendConfig) Validate() error {
	if b.Bucket == "" {
		return errors.New("pulumi: state bucket is empty (SPACEFLEET_STATE_BUCKET)")
	}
	if b.Region == "" {
		return errors.New("pulumi: state region is empty (SPACEFLEET_STATE_BUCKET_REGION)")
	}
	if b.KMSKeyARN == "" {
		return errors.New("pulumi: state KMS key ARN is empty (SPACEFLEET_STATE_KMS_KEY_ARN)")
	}
	if !strings.HasPrefix(b.KMSKeyARN, "arn:") {
		return fmt.Errorf("pulumi: state KMS key ARN does not look like an ARN: %q", b.KMSKeyARN)
	}
	return nil
}

// BackendForBuilderInfra builds the Backend for the per-cloud-account
// builder-infra stack (one VPC + cluster + execution role per cloud
// account).
//
// Path: s3://<bucket>/<org>/<cloud-account>/builder-infra
// Stack name: <cloud-account>-builder-infra
//
// The stack name combines the path components into a single
// Pulumi-legal identifier — Pulumi stack names can't contain `/`.
func BackendForBuilderInfra(b BackendConfig, orgID, cloudAccountID string) (Backend, error) {
	if err := b.Validate(); err != nil {
		return Backend{}, err
	}
	if err := requireID("orgID", orgID); err != nil {
		return Backend{}, err
	}
	if err := requireID("cloudAccountID", cloudAccountID); err != nil {
		return Backend{}, err
	}
	return Backend{
		StateURL:        stateURL(b, fmt.Sprintf("%s/%s/%s", orgID, cloudAccountID, StackBuilderInfra)),
		SecretsProvider: secretsProvider(b),
		StackName:       fmt.Sprintf("%s-%s", cloudAccountID, StackBuilderInfra),
	}, nil
}

// BackendForAppBuild builds the Backend for the per-app app-build
// stack (one ECR repo + builder task role per app).
//
// Path: s3://<bucket>/<org>/<app-id>/build
// Stack name: <app-id>-build
func BackendForAppBuild(b BackendConfig, orgID, appID string) (Backend, error) {
	if err := b.Validate(); err != nil {
		return Backend{}, err
	}
	if err := requireID("orgID", orgID); err != nil {
		return Backend{}, err
	}
	if err := requireID("appID", appID); err != nil {
		return Backend{}, err
	}
	return Backend{
		StateURL:        stateURL(b, fmt.Sprintf("%s/%s/%s", orgID, appID, StackAppBuild)),
		SecretsProvider: secretsProvider(b),
		StackName:       fmt.Sprintf("%s-%s", appID, StackAppBuild),
	}, nil
}

// stateURL returns Pulumi's s3:// backend URL for a given key prefix.
// Pulumi parses `?region=` to know which region to talk to S3 in — we
// always pin it explicitly so the SDK doesn't fall back to the default
// chain (which can vary by host).
func stateURL(b BackendConfig, prefix string) string {
	return fmt.Sprintf("s3://%s/%s?region=%s", b.Bucket, prefix, b.Region)
}

// secretsProvider returns the awskms:// URL Pulumi uses to encrypt
// stack-secret values. The region in the URL is the *KMS* key's
// region, parsed from the ARN.
func secretsProvider(b BackendConfig) string {
	region := regionFromARN(b.KMSKeyARN)
	if region == "" {
		region = b.Region
	}
	return fmt.Sprintf("awskms://%s?region=%s", b.KMSKeyARN, region)
}

// regionFromARN pulls the region out of an IAM/KMS-style ARN. Returns
// "" if the input doesn't have the expected six-segment shape; the
// caller falls back to the bucket region in that case.
func regionFromARN(arn string) string {
	parts := strings.Split(arn, ":")
	if len(parts) < 4 {
		return ""
	}
	return parts[3]
}

func requireID(name, val string) error {
	if val == "" {
		return fmt.Errorf("pulumi: %s is empty", name)
	}
	if strings.ContainsAny(val, "/ \t\n") {
		return fmt.Errorf("pulumi: %s contains illegal characters: %q", name, val)
	}
	return nil
}
