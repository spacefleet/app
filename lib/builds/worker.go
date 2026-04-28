package builds

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/spacefleet/app/ent"
	"github.com/spacefleet/app/ent/schema"
	"github.com/spacefleet/app/infra/stacks/appbuild"
	awsint "github.com/spacefleet/app/lib/aws"
	"github.com/spacefleet/app/lib/github"
	"github.com/spacefleet/app/lib/pulumi"
)

// SnoozeInterval is how long the BuildJob sleeps when another build for
// the same app is already running. River's JobSnooze re-enqueues the
// job with this delay; we keep it short so a finished build doesn't
// leave a queued one waiting much past its predecessor.
const SnoozeInterval = 15 * time.Second

// Top-level build statuses persisted on builds.status.
const (
	BuildStatusQueued    = "queued"
	BuildStatusRunning   = "running"
	BuildStatusSucceeded = "succeeded"
	BuildStatusFailed    = "failed"
)

// errAppBusy is the sentinel the promotion path returns when another
// build is in flight for the same app. The Work handler converts it to
// river.JobSnooze.
var errAppBusy = errors.New("another build is running for this app")

// TokenIssuer is what the worker needs from lib/github to mint an
// installation access token at dispatch time. Defining the interface
// here keeps github.Service substitutable in tests.
type TokenIssuer interface {
	IssueInstallationToken(ctx context.Context, orgSlug string, installationID int64) (*github.AccessToken, error)
}

// CredentialIssuer mints short-lived AWS creds for an assumed role.
// lib/aws.Verifier satisfies this; tests fake it.
type CredentialIssuer interface {
	AssumeRoleEnv(ctx context.Context, roleARN, externalID, region, sessionName string) (map[string]string, error)
}

// Orchestrator is the subset of *pulumi.Orchestrator we use here.
// Defining it as an interface lets us swap a stub in worker tests.
type Orchestrator interface {
	UpAppBuild(ctx context.Context, t pulumi.AccountTarget, app pulumi.AppRef, opts pulumi.RunOpts) (pulumi.BuilderInfraOutputs, pulumi.AppBuildOutputs, error)
	DestroyAppBuild(ctx context.Context, t pulumi.AccountTarget, appID string, opts pulumi.RunOpts) error
}

// ECSFactory builds an ECS client from a freshly-assumed session. The
// worker rebuilds the client per build so credentials don't outlive
// their ~1h STS window.
type ECSFactory func(ctx context.Context, c awsint.SessionCreds) (awsint.ECSClient, error)

// SecretsFactory mirrors ECSFactory for Secrets Manager.
type SecretsFactory func(ctx context.Context, c awsint.SessionCreds) (awsint.SecretsClient, error)

// WorkerConfig collects every dependency the build worker needs. Big
// struct, but all fields are required (excluding the optional Stdout/
// Stderr knobs) — keeping them in one place beats a 10-arg constructor.
type WorkerConfig struct {
	Ent           *ent.Client
	DB            *sql.DB
	Orchestrator  Orchestrator
	Verifier      CredentialIssuer
	GitHub        TokenIssuer
	ECSClient     ECSFactory
	SecretsClient SecretsFactory

	// PublicURL is the externally-reachable base URL for this
	// installation; webhooks from the builder Fargate task POST here.
	PublicURL string

	// BuildTimeout is the absolute hard ceiling per build (60m default).
	// The poller enforces this; the worker only stamps the start.
	BuildTimeout time.Duration

	// Stdout/Stderr receive pulumi up output for the reconcile stage.
	// Optional — pass nil to discard.
	Stdout io.Writer
	Stderr io.Writer
}

// Validate fails fast on missing dependencies rather than letting a nil
// pointer surface mid-build. Called once at construction.
func (c WorkerConfig) Validate() error {
	if c.Ent == nil {
		return errors.New("worker: Ent client required")
	}
	if c.DB == nil {
		return errors.New("worker: DB required")
	}
	if c.Orchestrator == nil {
		return errors.New("worker: Orchestrator required")
	}
	if c.Verifier == nil {
		return errors.New("worker: Verifier required")
	}
	if c.GitHub == nil {
		return errors.New("worker: GitHub required")
	}
	if c.ECSClient == nil {
		return errors.New("worker: ECSClient factory required")
	}
	if c.SecretsClient == nil {
		return errors.New("worker: SecretsClient factory required")
	}
	if c.PublicURL == "" {
		return errors.New("worker: PublicURL required (set SPACEFLEET_PUBLIC_URL)")
	}
	if c.BuildTimeout <= 0 {
		return errors.New("worker: BuildTimeout must be > 0")
	}
	return nil
}

// Worker drives one build job through reconcile -> prepare -> dispatch
// and then exits. The polling backstop (in poller.go) takes over once
// the Fargate task is dispatched.
//
// Embeds river.WorkerDefaults so we satisfy river.Worker[BuildJobArgs]
// without writing every optional method (Middleware, NextRetry, etc.).
type Worker struct {
	river.WorkerDefaults[BuildJobArgs]
	cfg WorkerConfig
}

// NewWorker validates the config and returns a ready-to-register Worker.
func NewWorker(cfg WorkerConfig) (*Worker, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Worker{cfg: cfg}, nil
}

// Work is the River job entrypoint. Returns river.JobSnooze when
// another build is in flight; nil after dispatch succeeds; an error
// when something the framework should retry hit (rare — most failures
// are recorded on the row and we return nil).
func (w *Worker) Work(ctx context.Context, job *river.Job[BuildJobArgs]) error {
	buildID := job.Args.BuildID
	if buildID == uuid.Nil {
		return errors.New("worker: empty BuildID")
	}

	// 1. Try to atomically promote queued -> running. If another build
	//    is in flight for the same app, snooze and try again later.
	if err := w.promoteToRunning(ctx, buildID); err != nil {
		if errors.Is(err, errAppBusy) {
			return river.JobSnooze(SnoozeInterval)
		}
		return fmt.Errorf("worker: promote: %w", err)
	}

	// 2. Run the synchronous half of the build. Any failure here is
	//    recorded on the row and the build is marked failed; we then
	//    return nil so River doesn't retry. (We don't retry build
	//    dispatch automatically — the user can click Build again.)
	if err := w.runDispatch(ctx, buildID); err != nil {
		w.recordDispatchFailure(ctx, buildID, err)
		return nil
	}
	return nil
}

// BuildJobArgs is the River job payload. We keep it minimal; the worker
// looks the rest up by ID.
type BuildJobArgs struct {
	BuildID uuid.UUID `json:"build_id"`
}

// Kind names the job for River's worker registry. Don't rename — River
// matches Kind strings to workers.
func (BuildJobArgs) Kind() string { return "build" }

// promoteToRunning atomically transitions builds.status from queued to
// running, but only if no other build for the same app is currently
// running. The advisory lock keyed on app_id serializes contending
// workers; the status checks then verify no race condition slipped
// through.
func (w *Worker) promoteToRunning(ctx context.Context, buildID uuid.UUID) error {
	tx, err := w.cfg.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Fetch app id and current status. We need both before we can lock
	// on app id and check for siblings.
	var appID uuid.UUID
	var status string
	if err := tx.QueryRowContext(ctx,
		`SELECT app_id, status FROM builds WHERE id = $1`,
		buildID,
	).Scan(&appID, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("worker: build %s not found", buildID)
		}
		return err
	}
	// If status isn't "queued" the job was already picked up — most
	// likely a duplicate enqueue or a worker restart that already
	// promoted. Returning nil lets the caller proceed; the runDispatch
	// step is idempotent enough (Pulumi reconciles, we re-mint the
	// secret, we re-RunTask only if no task arn yet).
	if status != BuildStatusQueued && status != BuildStatusRunning {
		// Already terminal (succeeded/failed) — caller should bail.
		return fmt.Errorf("worker: build %s in terminal status %q", buildID, status)
	}

	// pg_advisory_xact_lock takes int8. Hash the app UUID into one.
	lockKey := advisoryLockKey(appID)
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
		return fmt.Errorf("acquire app lock: %w", err)
	}

	// Now check sibling builds with the lock held.
	var otherRunning int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM builds WHERE app_id = $1 AND status = 'running' AND id != $2`,
		appID, buildID,
	).Scan(&otherRunning); err != nil {
		return err
	}
	if otherRunning > 0 {
		return errAppBusy
	}

	if status == BuildStatusQueued {
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx,
			`UPDATE builds SET status = 'running', started_at = $1 WHERE id = $2 AND status = 'queued'`,
			now, buildID,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// runDispatch is the body of the build worker: reconcile -> prepare ->
// dispatch. Each stage's running/succeeded/failed events are appended
// to builds.stages as they happen so the UI can show progress.
func (w *Worker) runDispatch(ctx context.Context, buildID uuid.UUID) error {
	b, err := w.cfg.Ent.Build.Get(ctx, buildID)
	if err != nil {
		return fmt.Errorf("load build: %w", err)
	}
	// Short-circuit on retry: if a previous run already persisted a
	// task ARN, the poller is supervising. Don't re-mint tokens or
	// re-dispatch.
	if b.FargateTaskArn != "" {
		return nil
	}
	app, err := w.cfg.Ent.App.Get(ctx, b.AppID)
	if err != nil {
		return fmt.Errorf("load app: %w", err)
	}
	ca, err := w.cfg.Ent.CloudAccount.Get(ctx, app.CloudAccountID)
	if err != nil {
		return fmt.Errorf("load cloud account: %w", err)
	}
	gi, err := w.cfg.Ent.GithubInstallation.Get(ctx, app.GithubInstallationID)
	if err != nil {
		return fmt.Errorf("load installation: %w", err)
	}

	target := pulumi.AccountTarget{
		OrgID:          app.OrgSlug,
		CloudAccountID: ca.ID.String(),
		AWSAccountID:   ca.AccountID,
		RoleARN:        ca.RoleArn,
		ExternalID:     ca.ExternalID,
		Region:         ca.Region,
	}

	// -- reconcile ---------------------------------------------------------
	if err := w.appendStageOrLog(ctx, buildID, StageRunning(StageReconcile)); err != nil {
		return err
	}
	infraOut, appOut, err := w.cfg.Orchestrator.UpAppBuild(ctx, target, pulumi.AppRef{ID: app.ID.String(), Slug: app.Slug}, pulumi.RunOpts{
		Stdout: w.cfg.Stdout,
		Stderr: w.cfg.Stderr,
	})
	if err != nil {
		_ = w.appendStageOrLog(ctx, buildID, StageFailed(StageReconcile, err.Error()))
		return fmt.Errorf("%s: %w", StageReconcile, err)
	}
	_ = w.appendStageOrLog(ctx, buildID, StageSucceeded(StageReconcile, nil))

	// -- prepare -----------------------------------------------------------
	_ = w.appendStageOrLog(ctx, buildID, StageRunning(StagePrepare))

	// Mint a fresh GitHub installation token. The token is valid for
	// 1h, plenty of headroom for a 60m build cap.
	tok, err := w.cfg.GitHub.IssueInstallationToken(ctx, app.OrgSlug, gi.InstallationID)
	if err != nil {
		_ = w.appendStageOrLog(ctx, buildID, StageFailed(StagePrepare, err.Error()))
		return fmt.Errorf("%s: github token: %w", StagePrepare, err)
	}

	// Assume the customer's role; reuse the same env across the
	// secrets put + ECS run-task call so we only pay for one STS hop.
	region := target.Region
	if region == "" {
		region = "us-east-1"
	}
	envMap, err := w.cfg.Verifier.AssumeRoleEnv(ctx, ca.RoleArn, ca.ExternalID, region, "spacefleet-build-"+buildID.String())
	if err != nil {
		_ = w.appendStageOrLog(ctx, buildID, StageFailed(StagePrepare, err.Error()))
		return fmt.Errorf("%s: assume role: %w", StagePrepare, err)
	}
	creds, err := awsint.SessionCredsFromEnv(envMap)
	if err != nil {
		_ = w.appendStageOrLog(ctx, buildID, StageFailed(StagePrepare, err.Error()))
		return fmt.Errorf("%s: %w", StagePrepare, err)
	}

	secretsClient, err := w.cfg.SecretsClient(ctx, creds)
	if err != nil {
		_ = w.appendStageOrLog(ctx, buildID, StageFailed(StagePrepare, err.Error()))
		return fmt.Errorf("%s: secrets client: %w", StagePrepare, err)
	}
	secretARN, err := awsint.PutBuildTokenSecret(ctx, secretsClient, app.ID.String(), buildID.String(), tok.Token)
	if err != nil {
		_ = w.appendStageOrLog(ctx, buildID, StageFailed(StagePrepare, err.Error()))
		return fmt.Errorf("%s: put secret: %w", StagePrepare, err)
	}

	// Defer cleanup of the secret we just wrote. dispatched flips true
	// once RunTask returns successfully; from that point on the poller
	// (or the terminal webhook) owns cleanup. Any failure between here
	// and there — including a panic — releases the secret on its way
	// out instead of orphaning it for the 60-min poller backstop.
	dispatched := false
	defer func() {
		if dispatched {
			return
		}
		cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = awsint.DeleteBuildTokenSecret(cleanCtx, secretsClient, secretARN)
	}()

	_ = w.appendStageOrLog(ctx, buildID, StageSucceeded(StagePrepare, nil))

	// -- dispatch ----------------------------------------------------------
	_ = w.appendStageOrLog(ctx, buildID, StageRunning(StageDispatch))

	ecsClient, err := w.cfg.ECSClient(ctx, creds)
	if err != nil {
		_ = w.appendStageOrLog(ctx, buildID, StageFailed(StageDispatch, err.Error()))
		return fmt.Errorf("%s: ecs client: %w", StageDispatch, err)
	}

	taskARN, err := awsint.RunBuildTask(ctx, ecsClient, awsint.RunBuildTaskParams{
		ClusterARN:     infraOut.ClusterARN,
		TaskDefinition: appOut.TaskDefinitionARN,
		ContainerName:  appbuild.ContainerName,
		Subnet:         infraOut.SubnetID,
		SecurityGroup:  infraOut.SecurityGroupID,
		Env:            buildContainerEnv(b, app, appOut, secretARN, region, w.cfg.PublicURL),
		StartedBy:      "spacefleet-" + shortID(buildID),
		// ClientToken makes RunTask idempotent across River retries —
		// if persisting the ARN below fails and River re-runs the job,
		// ECS returns the same task instead of spawning a duplicate.
		ClientToken: buildID.String(),
	})
	if err != nil {
		_ = w.appendStageOrLog(ctx, buildID, StageFailed(StageDispatch, err.Error()))
		return fmt.Errorf("%s: run task: %w", StageDispatch, err)
	}
	dispatched = true

	if _, err := w.cfg.Ent.Build.UpdateOneID(buildID).
		SetFargateTaskArn(taskARN).
		SetLogGroup(appOut.LogGroupName).
		SetLogStream(taskLogStream(taskARN)).
		Save(ctx); err != nil {
		// We've already dispatched; not persisting the ARN means the
		// poller can't supervise. Surface the error so River retries —
		// the ClientToken on RunTask above guarantees the retry sees
		// the same task rather than spawning a second one.
		return fmt.Errorf("%s: persist task arn: %w", StageDispatch, err)
	}

	_ = w.appendStageOrLog(ctx, buildID, StageSucceeded(StageDispatch, nil))
	return nil
}

// recordDispatchFailure stamps the build as failed when a pre-dispatch
// stage threw. Idempotent: if multiple failure paths flow here we just
// rewrite the same fields.
func (w *Worker) recordDispatchFailure(ctx context.Context, buildID uuid.UUID, err error) {
	now := time.Now().UTC()
	if _, e := w.cfg.Ent.Build.UpdateOneID(buildID).
		SetStatus(BuildStatusFailed).
		SetErrorMessage(truncate(err.Error(), 4000)).
		SetEndedAt(now).
		Save(ctx); e != nil {
		// Nothing else we can do; log via stderr if the caller wired
		// one. River will not retry because Work returned nil.
		if w.cfg.Stderr != nil {
			fmt.Fprintf(w.cfg.Stderr, "worker: persist dispatch failure for %s: %v\n", buildID, e)
		}
	}
}

// appendStageOrLog is a tiny wrapper that swallows append errors after
// logging, so a failure to record one stage doesn't abort the build.
// We still return the error from the *first* call so the caller knows
// to bail before doing real work; subsequent calls are best-effort.
func (w *Worker) appendStageOrLog(ctx context.Context, buildID uuid.UUID, ev schema.StageEvent) error {
	err := AppendStage(ctx, w.cfg.DB, buildID, ev)
	if err != nil && w.cfg.Stderr != nil {
		fmt.Fprintf(w.cfg.Stderr, "worker: append stage %s/%s for %s: %v\n", ev.Name, ev.Status, buildID, err)
	}
	return err
}

// buildContainerEnv assembles the env map handed to the Fargate task
// via containerOverrides.environment. The keys mirror the contract in
// builder/entrypoint.sh — change one, change both.
func buildContainerEnv(b *ent.Build, app *ent.App, appOut pulumi.AppBuildOutputs, secretARN, region, publicURL string) map[string]string {
	webhookURL := strings.TrimRight(publicURL, "/") + "/api/internal/builds/" + b.ID.String() + "/events"
	return map[string]string{
		"SPACEFLEET_BUILD_ID":       b.ID.String(),
		"SPACEFLEET_WEBHOOK_URL":    webhookURL,
		"SPACEFLEET_WEBHOOK_SECRET": b.WebhookSecret,
		"GITHUB_TOKEN_SECRET_ARN":   secretARN,
		"REPO_FULL_NAME":            app.GithubRepoFullName,
		"COMMIT_SHA":                b.SourceSha,
		"ECR_REPO":                  appOut.ECRRepoURI,
		"ECR_CACHE_REPO":            appOut.ECRCacheRepoURI,
		"AWS_REGION":                region,
		"AWS_DEFAULT_REGION":        region,
	}
}

// taskLogStream computes the CloudWatch log stream name the builder
// container will write to. Format mirrors awslogs-stream-prefix +
// container name + task ID, which is how ECS names streams under
// `awslogs-stream-prefix=builder` in the appbuild task definition.
//
// We compute it client-side so the UI can link to logs immediately,
// rather than waiting for the first webhook to confirm where logs
// land.
func taskLogStream(taskARN string) string {
	// task ARN: arn:aws:ecs:<region>:<acct>:task/<cluster>/<task-id>
	// or older: arn:aws:ecs:<region>:<acct>:task/<task-id>
	taskID := taskARN
	if idx := strings.LastIndex(taskARN, "/"); idx >= 0 {
		taskID = taskARN[idx+1:]
	}
	return "builder/" + appbuild.ContainerName + "/" + taskID
}

// advisoryLockKey turns a UUID into the int8 the lock function expects.
// FNV-1a 64-bit gives us a uniform hash; collisions across distinct
// app IDs are rare enough not to matter at our scale (at one collision
// per 2^32 IDs, two apps locking the same key is harmless — they just
// serialize, which is a no-op given they have no shared resources).
func advisoryLockKey(id uuid.UUID) int64 {
	h := fnv.New64a()
	_, _ = h.Write(id[:])
	return int64(h.Sum64())
}

// shortID returns the first 8 chars of a UUID — useful for the
// StartedBy field which has a 36-char limit and benefits from being
// scannable in CloudTrail.
func shortID(id uuid.UUID) string {
	s := id.String()
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}

// truncate caps a string at n bytes. error_message is TEXT (no
// length limit at the SQL layer) but we don't want a 4MB Pulumi error
// living in our DB; 4KB is plenty for the relevant lines.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
