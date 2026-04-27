package builds

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/spacefleet/app/ent"
	"github.com/spacefleet/app/ent/build"
	awsint "github.com/spacefleet/app/lib/aws"
)

// PollInterval is how often the poller scans the table for in-flight
// builds. 30s matches the planning doc; faster polling buys little and
// burns ECS DescribeTasks quota.
const PollInterval = 30 * time.Second

// ProvisioningTimeout is the cutoff after which a still-PROVISIONING
// task is presumed stuck (image pull failure, capacity exhausted, etc.)
// 5 minutes is enough for ECS to settle even on a cold cluster.
const ProvisioningTimeout = 5 * time.Minute

// Poller watches every build with status=running and applies the
// backstop rules:
//   - if the Fargate task has STOPPED but no terminal webhook fired,
//     mark the build failed with the stop reason
//   - if PROVISIONING > 5min, presume stuck, mark failed
//   - if total elapsed > BuildTimeout, StopTask + mark failed
//   - on every terminal transition, delete the GitHub-token secret
//
// Reattach is automatic: the poller queries from the table on every
// tick, so a worker restart picks up exactly the builds that were
// running before the crash.
type Poller struct {
	cfg     PollerConfig
	logger  *slog.Logger
	mu      sync.Mutex
	tracked map[uuid.UUID]time.Time // when we first saw each build at PROVISIONING
}

// PollerConfig threads in the same dependencies the worker uses, plus
// the tick interval (overridable for tests).
type PollerConfig struct {
	Ent           *ent.Client
	DB            *sql.DB
	Verifier      CredentialIssuer
	ECSClient     ECSFactory
	SecretsClient SecretsFactory

	BuildTimeout time.Duration

	// Interval overrides PollInterval. Zero -> use PollInterval.
	Interval time.Duration

	Logger *slog.Logger
	Stderr io.Writer
}

// Validate fails fast on missing dependencies.
func (c PollerConfig) Validate() error {
	if c.Ent == nil {
		return errors.New("poller: Ent client required")
	}
	if c.DB == nil {
		return errors.New("poller: DB required")
	}
	if c.Verifier == nil {
		return errors.New("poller: Verifier required")
	}
	if c.ECSClient == nil {
		return errors.New("poller: ECSClient factory required")
	}
	if c.SecretsClient == nil {
		return errors.New("poller: SecretsClient factory required")
	}
	if c.BuildTimeout <= 0 {
		return errors.New("poller: BuildTimeout must be > 0")
	}
	return nil
}

// NewPoller validates config and constructs a Poller.
func NewPoller(cfg PollerConfig) (*Poller, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = PollInterval
	}
	return &Poller{
		cfg:     cfg,
		logger:  cfg.Logger,
		tracked: make(map[uuid.UUID]time.Time),
	}, nil
}

// Run blocks until ctx fires. Ticks every Interval; each tick is one
// scan + per-build poll. Errors during a tick are logged but never
// abort the loop — a transient AWS hiccup shouldn't kill build
// supervision.
func (p *Poller) Run(ctx context.Context) {
	t := time.NewTicker(p.cfg.Interval)
	defer t.Stop()

	// Run one tick immediately so reattach happens at startup, not
	// after the first interval. Critical for "worker restarted while a
	// build was running" — we don't want to leave the build orphaned
	// for 30s before the first poll.
	p.tickOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.tickOnce(ctx)
		}
	}
}

func (p *Poller) tickOnce(ctx context.Context) {
	rows, err := p.cfg.Ent.Build.Query().
		Where(build.StatusEQ(BuildStatusRunning)).
		All(ctx)
	if err != nil {
		p.logger.ErrorContext(ctx, "poller: list running builds", "err", err)
		return
	}
	// Drop entries from `tracked` that are no longer running so the
	// map doesn't grow unbounded across worker uptime.
	p.gcTracked(rows)

	for _, b := range rows {
		if err := p.checkOne(ctx, b); err != nil {
			p.logger.ErrorContext(ctx, "poller: check build", "build_id", b.ID, "err", err)
		}
	}
}

func (p *Poller) gcTracked(running []*ent.Build) {
	live := make(map[uuid.UUID]struct{}, len(running))
	for _, b := range running {
		live[b.ID] = struct{}{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for id := range p.tracked {
		if _, ok := live[id]; !ok {
			delete(p.tracked, id)
		}
	}
}

func (p *Poller) checkOne(ctx context.Context, b *ent.Build) error {
	// Skip builds the dispatcher hasn't reached yet (no task ARN
	// stored). The build is "running" because the worker promoted it
	// before reconcile/prepare/dispatch; the task hasn't been started.
	if b.FargateTaskArn == "" {
		return nil
	}

	// Hard ceiling: if the build has been around longer than
	// BuildTimeout, stop the task and mark it failed regardless of
	// what ECS thinks the status is.
	if b.StartedAt != nil && time.Since(*b.StartedAt) > p.cfg.BuildTimeout {
		return p.timeoutBuild(ctx, b)
	}

	app, err := p.cfg.Ent.App.Get(ctx, b.AppID)
	if err != nil {
		return fmt.Errorf("load app: %w", err)
	}
	ca, err := p.cfg.Ent.CloudAccount.Get(ctx, app.CloudAccountID)
	if err != nil {
		return fmt.Errorf("load cloud account: %w", err)
	}

	region := ca.Region
	if region == "" {
		region = "us-east-1"
	}
	envMap, err := p.cfg.Verifier.AssumeRoleEnv(ctx, ca.RoleArn, ca.ExternalID, region, "spacefleet-poll-"+shortID(b.ID))
	if err != nil {
		return fmt.Errorf("assume role: %w", err)
	}
	creds, err := awsint.SessionCredsFromEnv(envMap)
	if err != nil {
		return err
	}
	ecsClient, err := p.cfg.ECSClient(ctx, creds)
	if err != nil {
		return fmt.Errorf("ecs client: %w", err)
	}

	// Look up the cluster ARN. We don't store it on the build; we
	// recompute it via the same builder-infra naming convention as
	// the orchestrator. Cheap to re-derive — it's just "spacefleet-
	// builds" cluster prefix in the customer's account, but the ARN
	// includes account+region. We instead read it back from the
	// task ARN, which embeds cluster + task id.
	clusterARN, ok := clusterARNFromTaskARN(b.FargateTaskArn)
	if !ok {
		return fmt.Errorf("malformed task arn: %s", b.FargateTaskArn)
	}

	st, err := awsint.DescribeBuildTask(ctx, ecsClient, clusterARN, b.FargateTaskArn)
	if err != nil {
		// Transient AWS error — log and try again next tick. Don't
		// fail the build over a network blip.
		return fmt.Errorf("describe task: %w", err)
	}

	now := time.Now().UTC()

	switch {
	case st.IsTerminal():
		// Did the webhook already mark this build terminal? If yes,
		// we just need to clean up the secret. (Status flips to
		// 'succeeded'/'failed' before the secret cleanup runs in the
		// webhook handler, so the build won't be in our running
		// list — but a race window exists.)
		if err := p.handleTerminalTask(ctx, b, st); err != nil {
			return err
		}
	case st.LastStatus == "PROVISIONING":
		p.mu.Lock()
		first, seen := p.tracked[b.ID]
		if !seen {
			p.tracked[b.ID] = now
			first = now
		}
		p.mu.Unlock()
		if now.Sub(first) > ProvisioningTimeout {
			return p.markFailed(ctx, b, fmt.Sprintf("task stuck in PROVISIONING for %s", now.Sub(first).Truncate(time.Second)))
		}
	default:
		// RUNNING / PENDING / DEACTIVATING — nothing to do. Webhooks
		// will drive the build forward; we're just on standby.
	}
	return nil
}

// handleTerminalTask reconciles a STOPPED ECS task with our build row.
// If the webhook already moved status off "running" (succeeded/failed),
// we just delete the GitHub-token secret. Otherwise we infer the
// outcome from the task's exit code and stop reason.
func (p *Poller) handleTerminalTask(ctx context.Context, b *ent.Build, st awsint.TaskStatus) error {
	// Refresh the row in case the webhook already moved it.
	current, err := p.cfg.Ent.Build.Get(ctx, b.ID)
	if err != nil {
		return err
	}
	if current.Status != BuildStatusRunning {
		// Webhook already concluded; just clean up the secret.
		return p.cleanupSecret(ctx, current)
	}

	reason := buildStopReason(st)
	if err := p.markFailed(ctx, current, reason); err != nil {
		return err
	}
	return p.cleanupSecret(ctx, current)
}

func buildStopReason(st awsint.TaskStatus) string {
	if st.StoppedReason != "" {
		if st.ExitCode != nil {
			return fmt.Sprintf("task stopped (%s, exit %d)", st.StoppedReason, *st.ExitCode)
		}
		return "task stopped: " + st.StoppedReason
	}
	if st.StopCode != "" {
		return "task stopped: " + st.StopCode
	}
	return "task stopped"
}

// markFailed flips a running build to failed with an error message.
// Idempotent: a second call with the same row is a no-op (status check
// guards re-writes).
func (p *Poller) markFailed(ctx context.Context, b *ent.Build, reason string) error {
	now := time.Now().UTC()
	if err := AppendStage(ctx, p.cfg.DB, b.ID, StageFailed("backstop", reason)); err != nil {
		// Stage append is best-effort here — we still want to flip
		// the row.
		p.logger.ErrorContext(ctx, "poller: append backstop stage", "build_id", b.ID, "err", err)
	}
	_, err := p.cfg.Ent.Build.Update().
		Where(build.IDEQ(b.ID), build.StatusEQ(BuildStatusRunning)).
		SetStatus(BuildStatusFailed).
		SetErrorMessage(truncate(reason, 4000)).
		SetEndedAt(now).
		Save(ctx)
	return err
}

// timeoutBuild stops a still-running task (60min ceiling reached) and
// marks the build failed.
func (p *Poller) timeoutBuild(ctx context.Context, b *ent.Build) error {
	app, err := p.cfg.Ent.App.Get(ctx, b.AppID)
	if err != nil {
		return err
	}
	ca, err := p.cfg.Ent.CloudAccount.Get(ctx, app.CloudAccountID)
	if err != nil {
		return err
	}
	region := ca.Region
	if region == "" {
		region = "us-east-1"
	}
	envMap, err := p.cfg.Verifier.AssumeRoleEnv(ctx, ca.RoleArn, ca.ExternalID, region, "spacefleet-stop-"+shortID(b.ID))
	if err != nil {
		return err
	}
	creds, err := awsint.SessionCredsFromEnv(envMap)
	if err != nil {
		return err
	}
	ecsClient, err := p.cfg.ECSClient(ctx, creds)
	if err != nil {
		return err
	}
	clusterARN, ok := clusterARNFromTaskARN(b.FargateTaskArn)
	if !ok {
		return fmt.Errorf("malformed task arn: %s", b.FargateTaskArn)
	}
	if err := awsint.StopBuildTask(ctx, ecsClient, clusterARN, b.FargateTaskArn, "spacefleet build timeout"); err != nil {
		// Don't bail — the row should still flip to failed even if
		// stop fails. Worst case the task burns a few more cents.
		p.logger.ErrorContext(ctx, "poller: stop task on timeout", "build_id", b.ID, "err", err)
	}
	if err := p.markFailed(ctx, b, "build exceeded timeout"); err != nil {
		return err
	}
	return p.cleanupSecret(ctx, b)
}

// cleanupSecret deletes the per-build GitHub-token secret. Idempotent;
// 404 is success.
func (p *Poller) cleanupSecret(ctx context.Context, b *ent.Build) error {
	app, err := p.cfg.Ent.App.Get(ctx, b.AppID)
	if err != nil {
		return err
	}
	ca, err := p.cfg.Ent.CloudAccount.Get(ctx, app.CloudAccountID)
	if err != nil {
		return err
	}
	region := ca.Region
	if region == "" {
		region = "us-east-1"
	}
	envMap, err := p.cfg.Verifier.AssumeRoleEnv(ctx, ca.RoleArn, ca.ExternalID, region, "spacefleet-cleanup-"+shortID(b.ID))
	if err != nil {
		return err
	}
	creds, err := awsint.SessionCredsFromEnv(envMap)
	if err != nil {
		return err
	}
	secretsClient, err := p.cfg.SecretsClient(ctx, creds)
	if err != nil {
		return err
	}
	// We only know the name pattern, not the ARN. The pattern includes
	// the account ID and region, but Delete accepts the friendly name
	// too — Secrets Manager resolves either.
	name := awsint.BuildTokenSecretName(app.ID.String(), b.ID.String())
	return awsint.DeleteBuildTokenSecret(ctx, secretsClient, name)
}

// clusterARNFromTaskARN extracts the cluster ARN from a task ARN.
// ECS task ARNs come in two shapes:
//
//	arn:aws:ecs:<region>:<acct>:task/<cluster>/<task-id>   (new)
//	arn:aws:ecs:<region>:<acct>:task/<task-id>             (legacy)
//
// We support the new shape (clusters always have a name) and return
// false for legacy ARNs — those don't carry the cluster.
func clusterARNFromTaskARN(taskARN string) (string, bool) {
	parts := strings.SplitN(taskARN, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" {
		return "", false
	}
	tail := parts[5] // "task/<cluster>/<task-id>" or "task/<task-id>"
	tailParts := strings.SplitN(tail, "/", 3)
	if len(tailParts) != 3 || tailParts[0] != "task" {
		return "", false
	}
	cluster := tailParts[1]
	region := parts[3]
	acct := parts[4]
	return fmt.Sprintf("arn:aws:ecs:%s:%s:cluster/%s", region, acct, cluster), true
}
