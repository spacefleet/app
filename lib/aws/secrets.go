package aws

import (
	"context"
	"errors"
	"fmt"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

// SecretsClient is the narrow surface the build worker uses. We expose
// just the three calls we actually make so tests can fake them without
// re-implementing the full SDK shape.
type SecretsClient interface {
	CreateSecret(ctx context.Context, params *secretsmanager.CreateSecretInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.CreateSecretOutput, error)
	PutSecretValue(ctx context.Context, params *secretsmanager.PutSecretValueInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.PutSecretValueOutput, error)
	DeleteSecret(ctx context.Context, params *secretsmanager.DeleteSecretInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.DeleteSecretOutput, error)
}

// NewSecretsClient builds a Secrets Manager client bound to assumed-role
// creds. Same shape as NewECSClient: lib/aws.Verifier mints the creds,
// the worker hands them here.
func NewSecretsClient(ctx context.Context, c SessionCreds) (*secretsmanager.Client, error) {
	cfg, err := ConfigFromCreds(ctx, c)
	if err != nil {
		return nil, err
	}
	return secretsmanager.NewFromConfig(cfg), nil
}

// BuildTokenSecretName returns the per-build Secrets Manager name where
// the GitHub installation token lives. The pattern matches the IAM
// policy in infra/stacks/appbuild/program.go's builderTaskRolePolicy
// (`spacefleet/builds/<app-id>/*`) — change one, change both.
func BuildTokenSecretName(appID, buildID string) string {
	return fmt.Sprintf("spacefleet/builds/%s/%s/github-token", appID, buildID)
}

// PutBuildTokenSecret writes the GitHub installation token to a
// per-build Secrets Manager secret in the customer's account. Returns
// the secret's ARN — that's what we hand to ECS via env var so the
// builder can fetch it.
//
// Idempotent across retries: if the secret already exists (a previous
// dispatch crashed before recording the ARN), we PutSecretValue to
// overwrite. Only the latest version is what AWSCURRENT points at, and
// that's the value the builder reads.
func PutBuildTokenSecret(ctx context.Context, c SecretsClient, appID, buildID, token string) (string, error) {
	if c == nil {
		return "", errors.New("aws: nil secrets client")
	}
	if appID == "" || buildID == "" {
		return "", errors.New("aws: appID and buildID required")
	}
	if token == "" {
		return "", errors.New("aws: token required")
	}
	name := BuildTokenSecretName(appID, buildID)

	out, err := c.CreateSecret(ctx, &secretsmanager.CreateSecretInput{
		Name:         awssdk.String(name),
		SecretString: awssdk.String(token),
		Description:  awssdk.String("Spacefleet build " + buildID + " — GitHub installation token. Auto-deleted on build end."),
	})
	if err == nil {
		if out.ARN == nil {
			return "", errors.New("aws: create secret returned no ARN")
		}
		return *out.ARN, nil
	}

	// CreateSecret failed. The two cases we tolerate are:
	//   - ResourceExistsException: a previous attempt for this build
	//     created the secret already; we PutSecretValue to refresh.
	//   - InvalidRequestException with "scheduled for deletion": a
	//     previous build with the same id was deleted; we restore +
	//     PutSecretValue. Should be rare in practice (build IDs are
	//     UUIDs) but covering it removes a flake mode.
	var existsErr *smtypes.ResourceExistsException
	if errors.As(err, &existsErr) {
		return updateExistingSecret(ctx, c, name, token)
	}
	return "", fmt.Errorf("aws: create build token secret: %w", err)
}

func updateExistingSecret(ctx context.Context, c SecretsClient, name, token string) (string, error) {
	out, err := c.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{
		SecretId:     awssdk.String(name),
		SecretString: awssdk.String(token),
	})
	if err != nil {
		return "", fmt.Errorf("aws: put existing build token secret: %w", err)
	}
	if out.ARN == nil {
		return "", errors.New("aws: put secret returned no ARN")
	}
	return *out.ARN, nil
}

// DeleteBuildTokenSecret removes the per-build secret with no recovery
// window. We pass ForceDeleteWithoutRecovery so the row is gone
// immediately — keeping a 30-day recovery window for a token that
// stopped being valid the second the build ended just adds spend.
//
// 404s are treated as success — already-gone is the same end state.
func DeleteBuildTokenSecret(ctx context.Context, c SecretsClient, secretARN string) error {
	if c == nil {
		return errors.New("aws: nil secrets client")
	}
	if secretARN == "" {
		return errors.New("aws: secret ARN required")
	}
	_, err := c.DeleteSecret(ctx, &secretsmanager.DeleteSecretInput{
		SecretId:                   awssdk.String(secretARN),
		ForceDeleteWithoutRecovery: awssdk.Bool(true),
	})
	if err == nil {
		return nil
	}
	var nf *smtypes.ResourceNotFoundException
	if errors.As(err, &nf) {
		return nil
	}
	return fmt.Errorf("aws: delete build token secret: %w", err)
}
