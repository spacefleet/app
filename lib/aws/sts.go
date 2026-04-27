package aws

import (
	"context"
	"errors"
	"fmt"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// AssumeRoleEnv assumes the customer's role and returns the env vars
// the AWS SDK (and Pulumi's AWS provider) read to authenticate. Used by
// the Pulumi orchestrator to fan short-lived creds into the workspace
// for one stack run.
//
// region pins AWS_REGION on the resulting set; pass the region the
// stack should provision against (the cloud account's, falling back to
// "us-east-1" for STS-only operations).
//
// sessionName is the human-readable name AWS attaches to the assumed
// session — it shows up in CloudTrail. Keep it short and prefixed with
// "spacefleet-" so an operator scanning their trail can spot us.
//
// The returned credentials live ~1h by default (STS default for
// AssumeRole). Pulumi's stack runs comfortably within that window;
// long-running deploys (which we don't have today) would need
// re-fetching.
func (v *Verifier) AssumeRoleEnv(ctx context.Context, roleARN, externalID, region, sessionName string) (map[string]string, error) {
	if roleARN == "" {
		return nil, errors.New("aws assume: role arn required")
	}
	if externalID == "" {
		return nil, errors.New("aws assume: external id required")
	}
	if sessionName == "" {
		sessionName = "spacefleet"
	}

	stsBase := sts.NewFromConfig(v.cfg)
	provider := stscreds.NewAssumeRoleProvider(stsBase, roleARN, func(o *stscreds.AssumeRoleOptions) {
		o.ExternalID = awssdk.String(externalID)
		o.RoleSessionName = sessionName
	})

	creds, err := provider.Retrieve(ctx)
	if err != nil {
		return nil, fmt.Errorf("aws assume: retrieve credentials: %w", err)
	}

	env := map[string]string{
		"AWS_ACCESS_KEY_ID":     creds.AccessKeyID,
		"AWS_SECRET_ACCESS_KEY": creds.SecretAccessKey,
		"AWS_SESSION_TOKEN":     creds.SessionToken,
	}
	// Region: prefer caller's, fall back to the verifier's loaded
	// region, then us-east-1 (STS-default). Pulumi's AWS provider
	// requires AWS_REGION to be set.
	switch {
	case region != "":
		env["AWS_REGION"] = region
		env["AWS_DEFAULT_REGION"] = region
	case v.cfg.Region != "":
		env["AWS_REGION"] = v.cfg.Region
		env["AWS_DEFAULT_REGION"] = v.cfg.Region
	default:
		env["AWS_REGION"] = "us-east-1"
		env["AWS_DEFAULT_REGION"] = "us-east-1"
	}
	// Force the AWS SDK to ignore any host-side credential profile or
	// shared-config file that might be set in the worker's env. We
	// pass full creds + region; nothing else should be consulted.
	env["AWS_SDK_LOAD_CONFIG"] = "0"
	return env, nil
}

// stsClient narrows what the assume-role flow needs out of the SDK so
// tests can fake it without spinning up the full AWS layer.
type stsClient interface {
	GetCallerIdentity(ctx context.Context, in *sts.GetCallerIdentityInput, opts ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

// Verifier confirms that a (role_arn, external_id) pair actually allows
// us to assume into the customer's account. It does the cheapest thing
// that proves the chain end-to-end: AssumeRole → GetCallerIdentity. Any
// error means we can't act in their account today.
type Verifier struct {
	platformAccount string
	cfg             awssdk.Config
}

// NewVerifier loads the platform's default AWS credentials chain (env,
// IAM role, profile, etc.) once at startup. The credentials are the
// principal customer trust policies grant `sts:AssumeRole` to.
func NewVerifier(ctx context.Context, platformAccount string) (*Verifier, error) {
	if platformAccount == "" {
		return nil, errors.New("aws verifier: platform account required")
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("aws verifier: load default config: %w", err)
	}
	return &Verifier{platformAccount: platformAccount, cfg: cfg}, nil
}

// VerifyResult captures what we learned from a successful probe. Account
// is the assumed-role session's account ID — must match the role ARN's
// embedded account, otherwise the customer pasted something weird.
type VerifyResult struct {
	Account string
	Arn     string
}

// Verify assumes the customer's role with the given external ID and
// calls GetCallerIdentity on the resulting session. Errors are returned
// verbatim so the UI can show what went wrong — typically AccessDenied
// (trust policy mismatch / wrong external ID), or no-such-role.
func (v *Verifier) Verify(ctx context.Context, roleARN, externalID string) (*VerifyResult, error) {
	if roleARN == "" {
		return nil, errors.New("aws verifier: role arn required")
	}
	if externalID == "" {
		return nil, errors.New("aws verifier: external id required")
	}

	stsBase := sts.NewFromConfig(v.cfg)
	provider := stscreds.NewAssumeRoleProvider(stsBase, roleARN, func(o *stscreds.AssumeRoleOptions) {
		o.ExternalID = awssdk.String(externalID)
		o.RoleSessionName = "spacefleet-onboarding-probe"
	})

	assumed := awssdk.Config{
		Region:      v.cfg.Region,
		Credentials: awssdk.NewCredentialsCache(provider),
	}
	if assumed.Region == "" {
		// STS is global, but the SDK insists on a region for signing.
		assumed.Region = "us-east-1"
	}

	out, err := sts.NewFromConfig(assumed).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return nil, fmt.Errorf("assume + identity: %w", err)
	}

	res := &VerifyResult{}
	if out.Account != nil {
		res.Account = *out.Account
	}
	if out.Arn != nil {
		res.Arn = *out.Arn
	}
	return res, nil
}
