package builds

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/spacefleet/app/ent"
	entapp "github.com/spacefleet/app/ent/app"
	awsint "github.com/spacefleet/app/lib/aws"
)

// LogsClientFactory mints a CloudWatch Logs client from short-lived
// session credentials. The HTTP layer wires this to
// awsint.NewLogsClient; tests provide a stub.
type LogsClientFactory func(ctx context.Context, c awsint.SessionCreds) (awsint.LogsClient, error)

// LogsConfig collects the dependencies for the logs controller. Same
// shape as the webhook config: an ent client, a credential issuer, a
// client factory.
type LogsConfig struct {
	Ent        *ent.Client
	Verifier   CredentialIssuer
	LogsClient LogsClientFactory

	// MaxLimit caps how many events the API will request per call,
	// regardless of what the caller asked for. CloudWatch's hard limit
	// is 10,000 events / 1MB; we use a smaller default to keep the
	// pollable response size predictable. Pass 0 to use the package
	// default (1000).
	MaxLimit int32
}

// DefaultLogsLimit is the per-page event count when the caller doesn't
// pass one. 1000 is comfortably under CloudWatch's 10K cap and keeps a
// single response under ~1MB even for a chatty Kaniko build.
const DefaultLogsLimit int32 = 1000

func (c LogsConfig) Validate() error {
	if c.Ent == nil {
		return errors.New("logs: Ent client required")
	}
	if c.Verifier == nil {
		return errors.New("logs: Verifier required")
	}
	if c.LogsClient == nil {
		return errors.New("logs: LogsClient factory required")
	}
	return nil
}

// LogsController serves one build-logs page per Fetch call. Stateless:
// pagination is driven by the caller passing the previous response's
// NextStartTimeMs back on each call.
type LogsController struct {
	cfg LogsConfig
}

// NewLogsController validates config and returns a ready controller.
func NewLogsController(cfg LogsConfig) (*LogsController, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.MaxLimit <= 0 {
		cfg.MaxLimit = DefaultLogsLimit
	}
	return &LogsController{cfg: cfg}, nil
}

// FetchParams is the input to Fetch. OrgSlug+AppSlug+BuildID identify
// the build; StartTimeMs/Limit are the pagination knobs.
type FetchParams struct {
	OrgSlug     string
	AppSlug     string
	BuildID     uuid.UUID
	StartTimeMs int64 // unix-millis cursor; 0 = start from head
	Limit       int32 // 0 = use default
}

// FetchResult is the API-layer-friendly shape: events + next cursor +
// flag. We carry the build's terminal status here too so the UI can
// stop polling once the build is done *and* the stream is drained.
type FetchResult struct {
	Events          []awsint.LogEvent
	NextStartTimeMs int64
	HasMore         bool

	// BuildTerminal is true when the build's row says succeeded/failed.
	// Combined with HasMore=false, this is the signal the UI uses to
	// stop polling.
	BuildTerminal bool
}

// Fetch is the controller's single public method. It:
//
//  1. Loads the build (scoped to org+app so a leaked UUID can't escape
//     the tenant boundary).
//  2. If the build hasn't dispatched yet (no log_group/log_stream),
//     returns an empty page with no token so the UI keeps polling.
//  3. Assumes the customer's integration role and pulls one page of
//     events from CloudWatch.
//
// The returned FetchResult.NextStartTimeMs is the highest event
// timestamp observed (or the caller's input when no events came
// back). Pass it back as StartTimeMs on the next call.
func (l *LogsController) Fetch(ctx context.Context, p FetchParams) (FetchResult, error) {
	if p.OrgSlug == "" || p.AppSlug == "" {
		return FetchResult{}, errors.New("logs: org and app required")
	}
	if p.BuildID == uuid.Nil {
		return FetchResult{}, errors.New("logs: build id required")
	}

	build, app, err := l.lookupBuildScoped(ctx, p.OrgSlug, p.AppSlug, p.BuildID)
	if err != nil {
		return FetchResult{}, err
	}

	terminal := build.Status == BuildStatusSucceeded || build.Status == BuildStatusFailed

	// Pre-dispatch builds (queued, or running but not past dispatch yet)
	// don't have a log group/stream recorded. Return an empty page; the
	// UI keeps polling until the worker stamps the row.
	if build.LogGroup == "" || build.LogStream == "" {
		return FetchResult{BuildTerminal: terminal}, nil
	}

	ca, err := l.cfg.Ent.CloudAccount.Get(ctx, app.CloudAccountID)
	if err != nil {
		return FetchResult{}, fmt.Errorf("logs: load cloud account: %w", err)
	}
	region := ca.Region
	if region == "" {
		region = "us-east-1"
	}

	envMap, err := l.cfg.Verifier.AssumeRoleEnv(ctx, ca.RoleArn, ca.ExternalID, region, "spacefleet-logs-"+shortID(build.ID))
	if err != nil {
		return FetchResult{}, fmt.Errorf("logs: assume role: %w", err)
	}
	creds, err := awsint.SessionCredsFromEnv(envMap)
	if err != nil {
		return FetchResult{}, fmt.Errorf("logs: %w", err)
	}

	client, err := l.cfg.LogsClient(ctx, creds)
	if err != nil {
		return FetchResult{}, fmt.Errorf("logs: client: %w", err)
	}

	limit := p.Limit
	if limit <= 0 || limit > l.cfg.MaxLimit {
		limit = l.cfg.MaxLimit
	}

	page, err := awsint.GetBuildLogEvents(ctx, client, awsint.GetBuildLogEventsParams{
		LogGroupName:  build.LogGroup,
		LogStreamName: build.LogStream,
		StartTimeMs:   p.StartTimeMs,
		Limit:         limit,
	})
	if err != nil {
		return FetchResult{}, fmt.Errorf("logs: get events: %w", err)
	}

	return FetchResult{
		Events:          page.Events,
		NextStartTimeMs: page.NextStartTimeMs,
		HasMore:         page.HasMore,
		BuildTerminal:   terminal,
	}, nil
}

// lookupBuildScoped finds (build, app) by (org, app slug, build id) so a
// guess at another app's UUID can't return data the caller shouldn't
// see. Mirrors what Service.Get does, duplicated here to keep the
// controller from depending on the full Service surface.
func (l *LogsController) lookupBuildScoped(ctx context.Context, orgSlug, appSlug string, buildID uuid.UUID) (*ent.Build, *ent.App, error) {
	app, err := l.cfg.Ent.App.Query().
		Where(
			entapp.OrgSlugEQ(orgSlug),
			entapp.SlugEQ(appSlug),
		).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, nil, ErrAppNotFound
		}
		return nil, nil, err
	}
	build, err := l.cfg.Ent.Build.Get(ctx, buildID)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, nil, ErrBuildNotFound
		}
		return nil, nil, err
	}
	if build.AppID != app.ID {
		// Wrong tenant — return ErrBuildNotFound (not a "forbidden") so
		// we don't leak the existence of a build belonging to another
		// app/org.
		return nil, nil, ErrBuildNotFound
	}
	return build, app, nil
}
