package pulumi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optdestroy"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optup"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// Program is a Pulumi inline program. Implementors live in
// infra/stacks/<name>/program.go and are registered with the runner by
// the orchestrator that knows how to map (cloud_account_id, app_id) to
// the right one. We type-alias rather than reuse pulumi.RunFunc directly
// so future hooks (pre/post-up callbacks, dependency injection) have a
// place to land.
type Program func(ctx *pulumi.Context) error

// RunnerConfig is what the worker hands NewRunner. Everything required
// to run one stack lives here: which backend (state + secrets), which
// stack to operate on, what program to run, what AWS provider config
// to set.
//
// AWSConfig is set as Pulumi config on the stack so pulumi-aws's default
// provider does its own AssumeRole into the customer account. The S3
// backend (state + secrets) deliberately uses the worker's default
// creds chain — those are the platform-account creds that own the
// state bucket. Threading customer creds via env vars instead would
// shadow the platform creds and break backend access.
//
// EnvVars is for non-AWS Pulumi runtime knobs (PULUMI_HOME,
// PULUMI_CONFIG_PASSPHRASE for tests, etc.). Do not put AWS_* in here.
type RunnerConfig struct {
	Backend   Backend
	Program   Program
	AWSConfig AWSConfig
	EnvVars   map[string]string

	// Stdout/Stderr receive Pulumi's progress streams. Phase 5 wires
	// these into the build's CloudWatch log group; for phase 1 the
	// caller can pass nil and Pulumi runs silently.
	Stdout io.Writer
	Stderr io.Writer
}

// AWSConfig is the bundle of config keys that drive pulumi-aws's
// default provider. When RoleARN is set, the provider does an
// STS AssumeRole using the worker's default creds as the source — that
// is, platform creds AssumeRole into the customer's role, exactly the
// chain the BUILD_PIPELINE doc describes.
//
// The fields map onto Pulumi config keys: `aws:region`,
// `aws:assumeRoles[0].roleArn`, `aws:assumeRoles[0].externalId`,
// `aws:assumeRoles[0].sessionName`. pulumi-aws v7 dropped the singular
// `assumeRole` block in favour of the `assumeRoles` array (each element
// is one hop in a role-chain). v1 only ever needs one hop, so we set
// element 0. Leaving RoleARN empty skips the assume-role keys (useful
// for the file:// backend tests).
type AWSConfig struct {
	Region      string
	RoleARN     string
	ExternalID  string
	SessionName string
}

// IsZero reports whether AWSConfig has nothing to set. Used by the
// runner to skip the SetAllConfig call entirely when the caller is
// running against a non-AWS backend (e.g., the file:// integration
// test).
func (a AWSConfig) IsZero() bool {
	return a == AWSConfig{}
}

// Runner is what the orchestrator calls. One Runner per stack run; it's
// not goroutine-safe (Pulumi acquires the s3:// stack lock for the
// duration of an Up call, so concurrent Ups on the same stack would
// fail anyway).
type Runner struct {
	cfg RunnerConfig
}

// NewRunner constructs a Runner. Validates RunnerConfig but does not
// reach for the network — that happens on Up/Destroy.
func NewRunner(cfg RunnerConfig) (*Runner, error) {
	if cfg.Backend.StateURL == "" {
		return nil, errors.New("pulumi: backend state URL is empty")
	}
	if cfg.Backend.StackName == "" {
		return nil, errors.New("pulumi: backend stack name is empty")
	}
	if cfg.Program == nil {
		return nil, errors.New("pulumi: program is nil")
	}
	return &Runner{cfg: cfg}, nil
}

// Up runs `pulumi up` against the configured stack. Returns the result
// (outputs, summary, captured stdout) or the error Pulumi reported.
//
// Idempotent by design: re-running with the same program against the
// same stack reports "no changes" and exits in seconds. The reconciler
// stage of every build relies on this property.
func (r *Runner) Up(ctx context.Context) (auto.UpResult, error) {
	if err := assertCLIPresent(); err != nil {
		return auto.UpResult{}, err
	}
	stack, err := r.upsertStack(ctx)
	if err != nil {
		return auto.UpResult{}, err
	}
	if err := r.applyAWSConfig(ctx, stack); err != nil {
		return auto.UpResult{}, err
	}
	opts := []optup.Option{}
	if r.cfg.Stdout != nil {
		opts = append(opts, optup.ProgressStreams(r.cfg.Stdout))
	}
	if r.cfg.Stderr != nil {
		opts = append(opts, optup.ErrorProgressStreams(r.cfg.Stderr))
	}
	res, err := stack.Up(ctx, opts...)
	if err != nil {
		return res, fmt.Errorf("pulumi up %s: %w", r.cfg.Backend.StackName, err)
	}
	return res, nil
}

// Destroy runs `pulumi destroy` against the configured stack. Same
// shape as Up: idempotent, returns Pulumi's result (or the error it
// reported). Used by the orchestrator's DestroyAppBuild /
// DestroyBuilderInfra paths and by the dev CLI.
func (r *Runner) Destroy(ctx context.Context) (auto.DestroyResult, error) {
	if err := assertCLIPresent(); err != nil {
		return auto.DestroyResult{}, err
	}
	stack, err := r.upsertStack(ctx)
	if err != nil {
		return auto.DestroyResult{}, err
	}
	if err := r.applyAWSConfig(ctx, stack); err != nil {
		return auto.DestroyResult{}, err
	}
	opts := []optdestroy.Option{}
	if r.cfg.Stdout != nil {
		opts = append(opts, optdestroy.ProgressStreams(r.cfg.Stdout))
	}
	if r.cfg.Stderr != nil {
		opts = append(opts, optdestroy.ErrorProgressStreams(r.cfg.Stderr))
	}
	res, err := stack.Destroy(ctx, opts...)
	if err != nil {
		return res, fmt.Errorf("pulumi destroy %s: %w", r.cfg.Backend.StackName, err)
	}
	return res, nil
}

// upsertStack creates or selects the configured stack against a fresh
// LocalWorkspace. Each call gets its own workspace (= its own temp dir
// with Pulumi.yaml/Pulumi.<stack>.yaml) so concurrent stack runs from
// the worker don't trample each other's settings files.
//
// We seed Pulumi.<stack>.yaml with the secrets provider URL via
// auto.Stacks. auto.SecretsProvider only adds `--secrets-provider` to
// the initial `pulumi stack init` call; on every subsequent run the
// fresh tempdir has no Pulumi.<stack>.yaml, so pulumi up falls back to
// the passphrase secrets manager and demands PULUMI_CONFIG_PASSPHRASE.
// Writing the URL into the seed yaml makes pulumi pull the
// awskms-wrapped data key from the backend state file every run.
func (r *Runner) upsertStack(ctx context.Context) (auto.Stack, error) {
	project := workspace.Project{
		Name:    tokens.PackageName(ProjectName),
		Runtime: workspace.NewProjectRuntimeInfo("go", nil),
		Backend: &workspace.ProjectBackend{URL: r.cfg.Backend.StateURL},
	}
	opts := []auto.LocalWorkspaceOption{
		auto.Project(project),
		auto.SecretsProvider(r.cfg.Backend.SecretsProvider),
		auto.Stacks(map[string]workspace.ProjectStack{
			r.cfg.Backend.StackName: {
				SecretsProvider: r.cfg.Backend.SecretsProvider,
			},
		}),
	}
	if len(r.cfg.EnvVars) > 0 {
		opts = append(opts, auto.EnvVars(r.cfg.EnvVars))
	}

	return auto.UpsertStackInlineSource(
		ctx,
		r.cfg.Backend.StackName,
		ProjectName,
		pulumi.RunFunc(r.cfg.Program),
		opts...,
	)
}

// assertCLIPresent surfaces the "you forgot to install pulumi" error
// up front instead of letting the SDK's first subprocess call fail with
// a confusing exec error mid-Up.
func assertCLIPresent() error {
	if _, err := exec.LookPath("pulumi"); err != nil {
		return fmt.Errorf("pulumi: CLI not found on $PATH (install via brew install pulumi or use the runtime Docker image)")
	}
	return nil
}

// applyAWSConfig writes the AWS provider config to the stack so
// pulumi-aws's default provider does the cross-account AssumeRole
// itself. The S3 backend has already been opened with the worker's
// default creds at this point — that's deliberate; the backend lives
// in the platform account, the resources land in the customer account.
func (r *Runner) applyAWSConfig(ctx context.Context, stack auto.Stack) error {
	if r.cfg.AWSConfig.IsZero() {
		return nil
	}
	cfg := auto.ConfigMap{}
	if r.cfg.AWSConfig.Region != "" {
		cfg["aws:region"] = auto.ConfigValue{Value: r.cfg.AWSConfig.Region}
	}
	if r.cfg.AWSConfig.RoleARN != "" {
		cfg["aws:assumeRoles[0].roleArn"] = auto.ConfigValue{Value: r.cfg.AWSConfig.RoleARN}
		if r.cfg.AWSConfig.ExternalID != "" {
			cfg["aws:assumeRoles[0].externalId"] = auto.ConfigValue{Value: r.cfg.AWSConfig.ExternalID}
		}
		if r.cfg.AWSConfig.SessionName != "" {
			cfg["aws:assumeRoles[0].sessionName"] = auto.ConfigValue{Value: r.cfg.AWSConfig.SessionName}
		}
	}
	if len(cfg) == 0 {
		return nil
	}
	// Path: true makes Pulumi interpret structured keys
	// (aws:assumeRoles[0].roleArn) as paths into objects/arrays, which is
	// what the aws provider's `assumeRoles` array requires. Flat keys
	// like `aws:region` parse the same way under path mode, so we can
	// use one call for both.
	if err := stack.SetAllConfigWithOptions(ctx, cfg, &auto.ConfigOptions{Path: true}); err != nil {
		return fmt.Errorf("pulumi: set aws config: %w", err)
	}
	return nil
}
