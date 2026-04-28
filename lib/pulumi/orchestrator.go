package pulumi

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/spacefleet/app/infra/stacks/appbuild"
	"github.com/spacefleet/app/infra/stacks/builderinfra"
)

// Orchestrator is the high-level driver for the per-cloud-account and
// per-app Pulumi stacks. The build worker calls UpAppBuild on every
// build so reconciliation is part of the build lifecycle; the dev CLI
// (cmd/spacefleet-infra) calls the same methods directly.
//
// Orchestrator depends on:
//   - BackendConfig: Pulumi state backend (s3 + KMS), set once at startup
//   - BuilderImage: the digest-pinned builder image, baked into the
//     binary at release or set via SPACEFLEET_BUILDER_IMAGE for dev
//
// Cross-account credentials are not minted here. The orchestrator
// passes the customer's role ARN + external ID to the runner as Pulumi
// config; pulumi-aws's default provider does the AssumeRole itself,
// using the worker's platform creds as the source. The S3 backend
// (state + KMS) keeps using the worker's default creds — those are the
// platform-account creds that own the bucket.
//
// One Orchestrator per worker / CLI process is fine; methods are safe
// for concurrent use as long as they target different stacks.
type Orchestrator struct {
	backend      BackendConfig
	builderImage string
}

// NewOrchestrator validates inputs once at construction. The backend
// config is required; the builder image is required (its absence would
// leak a "didn't pin" bug to a Pulumi error 30s into Up).
func NewOrchestrator(backend BackendConfig, builderImage string) (*Orchestrator, error) {
	if err := backend.Validate(); err != nil {
		return nil, err
	}
	if builderImage == "" {
		return nil, errors.New("orchestrator: builder image required (set SPACEFLEET_BUILDER_IMAGE or rely on -ldflags default)")
	}
	return &Orchestrator{backend: backend, builderImage: builderImage}, nil
}

// AccountTarget is what the orchestrator needs to act on a customer's
// account. Constructed by the caller from an ent.CloudAccount row +
// the install-wide config (the orchestrator stays ent-free).
//
// OrgID is the Spacefleet org's identifier — currently we use the
// Clerk org slug since that's what scopes everything else in the app.
// Naming follows BUILD_PIPELINE.md's "<org-id>/<cloud-account-id>/..."
// path scheme.
type AccountTarget struct {
	OrgID          string // Spacefleet org slug (Clerk slug today)
	CloudAccountID string // Spacefleet cloud-account row id (uuid string)
	AWSAccountID   string // 12-digit AWS account ID
	RoleARN        string // arn:aws:iam::<account>:role/SpacefleetIntegrationRole
	ExternalID     string // from the cloud-account row's secret column
	Region         string // optional; falls back to "us-east-1"
}

// Validate is the explicit precondition check the orchestrator runs at
// the top of every method. We return well-named errors rather than
// letting Pulumi fail downstream with something cryptic.
func (t AccountTarget) Validate() error {
	if t.OrgID == "" {
		return errors.New("orchestrator: target.OrgID required")
	}
	if t.CloudAccountID == "" {
		return errors.New("orchestrator: target.CloudAccountID required")
	}
	if t.AWSAccountID == "" {
		return errors.New("orchestrator: target.AWSAccountID required (complete onboarding first)")
	}
	if t.RoleARN == "" {
		return errors.New("orchestrator: target.RoleARN required (complete onboarding first)")
	}
	if t.ExternalID == "" {
		return errors.New("orchestrator: target.ExternalID required")
	}
	return nil
}

// resolvedRegion returns the region the stack should provision in. We
// pick "us-east-1" as the fallback because it's where AWS makes the
// largest number of services available and has the lowest egress
// pricing in customer-account terms.
func (t AccountTarget) resolvedRegion() string {
	if t.Region != "" {
		return t.Region
	}
	return "us-east-1"
}

// RunOpts threads Pulumi's progress streams through to the caller so
// the worker can capture Up output for the reconcile stage. The CLI
// passes os.Stdout/os.Stderr; tests pass nil.
type RunOpts struct {
	Stdout io.Writer
	Stderr io.Writer
}

// BuilderInfraOutputs collapses the inline program's exports into a
// strongly-typed struct. Fields here line up with builderinfra's
// OutputKeys; if a key is missing the orchestrator returns an error so
// a future schema mismatch fails loudly.
type BuilderInfraOutputs struct {
	ClusterARN       string
	ClusterName      string
	VpcID            string
	SubnetID         string
	SecurityGroupID  string
	ExecutionRoleARN string
	LogGroupPrefix   string
}

// AppRef identifies an app to the orchestrator. ID anchors UUID-named
// resources (IAM role, log group, secret path, task family); Slug
// drives the human-readable ECR repo name. Both come from the apps
// row; the orchestrator stays ent-free so the caller passes them in.
type AppRef struct {
	ID   string
	Slug string
}

// Validate is the precondition check for AppRef. Lives here rather
// than on the worker side so the dev CLI gets the same checks.
func (a AppRef) Validate() error {
	if a.ID == "" {
		return errors.New("orchestrator: app.ID required")
	}
	if a.Slug == "" {
		return errors.New("orchestrator: app.Slug required")
	}
	return nil
}

// AppBuildOutputs is the caller-facing return type from UpAppBuild.
// Names match appbuild's OutputKeys.
type AppBuildOutputs struct {
	ECRRepoURI        string
	ECRRepoName       string
	ECRCacheRepoURI   string
	ECRCacheRepoName  string
	TaskRoleARN       string
	TaskDefinitionARN string
	LogGroupName      string
}

// UpBuilderInfra brings the per-cloud-account stack to its desired
// state. Idempotent: re-running after a successful Up reports "no
// changes" in seconds. The orchestrator returns the parsed outputs so
// the caller (UpAppBuild, the dev CLI's --no-app form) can chain into
// the next step.
func (o *Orchestrator) UpBuilderInfra(ctx context.Context, t AccountTarget, opts RunOpts) (BuilderInfraOutputs, error) {
	if err := t.Validate(); err != nil {
		return BuilderInfraOutputs{}, err
	}
	region := t.resolvedRegion()

	backend, err := BackendForBuilderInfra(o.backend, t.OrgID, t.CloudAccountID)
	if err != nil {
		return BuilderInfraOutputs{}, err
	}

	runner, err := NewRunner(RunnerConfig{
		Backend: backend,
		Program: builderinfra.Program(builderinfra.Inputs{
			OrgID:          t.OrgID,
			CloudAccountID: t.CloudAccountID,
			Region:         region,
		}),
		AWSConfig: awsConfigFor(t, region),
		Stdout:    opts.Stdout,
		Stderr:    opts.Stderr,
	})
	if err != nil {
		return BuilderInfraOutputs{}, err
	}

	res, err := runner.Up(ctx)
	if err != nil {
		return BuilderInfraOutputs{}, err
	}

	out := BuilderInfraOutputs{}
	if err := readOutput(res.Outputs, builderinfra.OutputClusterARN, &out.ClusterARN); err != nil {
		return BuilderInfraOutputs{}, err
	}
	if err := readOutput(res.Outputs, builderinfra.OutputClusterName, &out.ClusterName); err != nil {
		return BuilderInfraOutputs{}, err
	}
	if err := readOutput(res.Outputs, builderinfra.OutputVpcID, &out.VpcID); err != nil {
		return BuilderInfraOutputs{}, err
	}
	if err := readOutput(res.Outputs, builderinfra.OutputSubnetID, &out.SubnetID); err != nil {
		return BuilderInfraOutputs{}, err
	}
	if err := readOutput(res.Outputs, builderinfra.OutputSecurityGroupID, &out.SecurityGroupID); err != nil {
		return BuilderInfraOutputs{}, err
	}
	if err := readOutput(res.Outputs, builderinfra.OutputExecutionRoleARN, &out.ExecutionRoleARN); err != nil {
		return BuilderInfraOutputs{}, err
	}
	if err := readOutput(res.Outputs, builderinfra.OutputLogGroupPrefix, &out.LogGroupPrefix); err != nil {
		return BuilderInfraOutputs{}, err
	}
	return out, nil
}

// UpAppBuild brings both stacks to desired state for one app. Always
// reconciles builder-infra first — the BUILD_PIPELINE doc's
// "self-heal on every build" property requires this. If builder-infra
// is already up the call is a few-second no-op.
//
// app carries both the UUID (for state-path + UUID-named resources)
// and the slug (for the human-readable ECR repo name). It's separate
// from AccountTarget because some orchestrator calls (UpBuilderInfra,
// DestroyBuilderInfra) don't need it.
func (o *Orchestrator) UpAppBuild(ctx context.Context, t AccountTarget, app AppRef, opts RunOpts) (BuilderInfraOutputs, AppBuildOutputs, error) {
	if err := app.Validate(); err != nil {
		return BuilderInfraOutputs{}, AppBuildOutputs{}, err
	}

	infraOut, err := o.UpBuilderInfra(ctx, t, opts)
	if err != nil {
		return BuilderInfraOutputs{}, AppBuildOutputs{}, fmt.Errorf("orchestrator: builder-infra: %w", err)
	}

	region := t.resolvedRegion()

	backend, err := BackendForAppBuild(o.backend, t.OrgID, app.ID)
	if err != nil {
		return infraOut, AppBuildOutputs{}, err
	}

	runner, err := NewRunner(RunnerConfig{
		Backend: backend,
		Program: appbuild.Program(appbuild.Inputs{
			OrgID:            t.OrgID,
			OrgSlug:          t.OrgID,
			CloudAccountID:   t.CloudAccountID,
			AppID:            app.ID,
			AppSlug:          app.Slug,
			Region:           region,
			BuilderImage:     o.builderImage,
			ExecutionRoleARN: infraOut.ExecutionRoleARN,
			AWSAccountID:     t.AWSAccountID,
		}),
		AWSConfig: awsConfigFor(t, region),
		Stdout:    opts.Stdout,
		Stderr:    opts.Stderr,
	})
	if err != nil {
		return infraOut, AppBuildOutputs{}, err
	}

	res, err := runner.Up(ctx)
	if err != nil {
		return infraOut, AppBuildOutputs{}, err
	}

	out := AppBuildOutputs{}
	if err := readOutput(res.Outputs, appbuild.OutputECRRepoURI, &out.ECRRepoURI); err != nil {
		return infraOut, AppBuildOutputs{}, err
	}
	if err := readOutput(res.Outputs, appbuild.OutputECRRepoName, &out.ECRRepoName); err != nil {
		return infraOut, AppBuildOutputs{}, err
	}
	if err := readOutput(res.Outputs, appbuild.OutputECRCacheRepoURI, &out.ECRCacheRepoURI); err != nil {
		return infraOut, AppBuildOutputs{}, err
	}
	if err := readOutput(res.Outputs, appbuild.OutputECRCacheRepoName, &out.ECRCacheRepoName); err != nil {
		return infraOut, AppBuildOutputs{}, err
	}
	if err := readOutput(res.Outputs, appbuild.OutputTaskRoleARN, &out.TaskRoleARN); err != nil {
		return infraOut, AppBuildOutputs{}, err
	}
	if err := readOutput(res.Outputs, appbuild.OutputTaskDefinitionARN, &out.TaskDefinitionARN); err != nil {
		return infraOut, AppBuildOutputs{}, err
	}
	if err := readOutput(res.Outputs, appbuild.OutputLogGroupName, &out.LogGroupName); err != nil {
		return infraOut, AppBuildOutputs{}, err
	}
	return infraOut, out, nil
}

// DestroyAppBuild tears down a per-app stack. Builder-infra stays —
// it's shared. ECR ForceDelete=true is what keeps this from failing
// when images are present.
func (o *Orchestrator) DestroyAppBuild(ctx context.Context, t AccountTarget, appID string, opts RunOpts) error {
	if err := t.Validate(); err != nil {
		return err
	}
	if appID == "" {
		return errors.New("orchestrator: appID required")
	}
	region := t.resolvedRegion()

	backend, err := BackendForAppBuild(o.backend, t.OrgID, appID)
	if err != nil {
		return err
	}

	// Destroy reads state from the s3 backend and walks resources in
	// reverse — it never invokes the inline program for resource
	// computation. We pass a noop program rather than re-running the
	// app-build factory because the factory's Validate would reject
	// optional fields (AWSAccountID, ExecutionRoleARN) that the
	// caller may not have on hand at destroy time (e.g., running
	// destroy after the cloud-account row has been wiped).
	runner, err := NewRunner(RunnerConfig{
		Backend:   backend,
		Program:   noopProgram,
		AWSConfig: awsConfigFor(t, region),
		Stdout:    opts.Stdout,
		Stderr:    opts.Stderr,
	})
	if err != nil {
		return err
	}

	if _, err := runner.Destroy(ctx); err != nil {
		return err
	}
	return nil
}

// DestroyBuilderInfra tears down the per-cloud-account stack. Should
// be called only when no app stacks remain — the orchestrator doesn't
// enforce this; the caller (cloud-account-disconnect handler, dev CLI)
// is responsible for sequencing.
func (o *Orchestrator) DestroyBuilderInfra(ctx context.Context, t AccountTarget, opts RunOpts) error {
	if err := t.Validate(); err != nil {
		return err
	}
	region := t.resolvedRegion()

	backend, err := BackendForBuilderInfra(o.backend, t.OrgID, t.CloudAccountID)
	if err != nil {
		return err
	}

	// Same noop-program rationale as DestroyAppBuild — see comment
	// there. Pulumi destroy reads state and removes resources;
	// re-running the inline program would be wasted work and would
	// re-impose the program's input validation.
	runner, err := NewRunner(RunnerConfig{
		Backend:   backend,
		Program:   noopProgram,
		AWSConfig: awsConfigFor(t, region),
		Stdout:    opts.Stdout,
		Stderr:    opts.Stderr,
	})
	if err != nil {
		return err
	}

	if _, err := runner.Destroy(ctx); err != nil {
		return err
	}
	return nil
}

// noopProgram is the inline program used for destroy. Pulumi requires
// some program when constructing a workspace; the destroy path doesn't
// invoke it for resource computation.
var noopProgram Program = func(*pulumi.Context) error { return nil }

// awsConfigFor builds the AWSConfig the runner threads onto the stack
// as Pulumi config. pulumi-aws's default provider reads these keys and
// performs the AssumeRole itself using the worker's platform creds —
// see runner.AWSConfig for why we don't mint env-var creds.
func awsConfigFor(t AccountTarget, region string) AWSConfig {
	return AWSConfig{
		Region:      region,
		RoleARN:     t.RoleARN,
		ExternalID:  t.ExternalID,
		SessionName: sessionName(t),
	}
}

// sessionName returns the human-readable AssumeRole session name. Goes
// into CloudTrail. Keep it short and Spacefleet-prefixed so an
// operator scanning their audit log can find us.
func sessionName(t AccountTarget) string {
	const limit = 64 // STS RoleSessionName max length
	name := "spacefleet-" + t.CloudAccountID
	if len(name) > limit {
		name = name[:limit]
	}
	return name
}

// readOutput pulls one named output from auto.UpResult.Outputs into a
// string pointer. Missing or non-string outputs become a typed error
// so the orchestrator's caller knows exactly which output is wrong.
func readOutput(outputs auto.OutputMap, key string, dest *string) error {
	v, ok := outputs[key]
	if !ok {
		return fmt.Errorf("orchestrator: missing stack output %q", key)
	}
	s, ok := v.Value.(string)
	if !ok {
		return fmt.Errorf("orchestrator: stack output %q is not a string (got %T)", key, v.Value)
	}
	*dest = s
	return nil
}
