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
// stack to operate on, what program to run, what AWS env to use.
//
// EnvVars lets the orchestrator inject AWS_ACCESS_KEY_ID/SECRET/SESSION_TOKEN
// from a freshly-minted AssumeRole session — Pulumi's AWS provider
// reads them out of the workspace env and never sees the worker's own
// long-lived creds.
type RunnerConfig struct {
	Backend Backend
	Program Program
	EnvVars map[string]string

	// Stdout/Stderr receive Pulumi's progress streams. Phase 5 wires
	// these into the build's CloudWatch log group; for phase 1 the
	// caller can pass nil and Pulumi runs silently.
	Stdout io.Writer
	Stderr io.Writer
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
func (r *Runner) upsertStack(ctx context.Context) (auto.Stack, error) {
	project := workspace.Project{
		Name:    tokens.PackageName(ProjectName),
		Runtime: workspace.NewProjectRuntimeInfo("go", nil),
		Backend: &workspace.ProjectBackend{URL: r.cfg.Backend.StateURL},
	}
	opts := []auto.LocalWorkspaceOption{
		auto.Project(project),
		auto.SecretsProvider(r.cfg.Backend.SecretsProvider),
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
