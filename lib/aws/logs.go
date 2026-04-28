package aws

import (
	"context"
	"errors"
	"fmt"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwltypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
)

// LogsClient is the narrow CloudWatch Logs surface the build-logs path
// uses. Defining the interface here keeps the SDK type out of callers
// and gives tests a small thing to fake.
type LogsClient interface {
	GetLogEvents(ctx context.Context, params *cloudwatchlogs.GetLogEventsInput, optFns ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.GetLogEventsOutput, error)
}

// NewLogsClient builds a CloudWatch Logs client bound to the assumed
// short-lived session. Same shape as NewECSClient/NewSecretsClient: the
// caller mints SessionCreds via lib/aws.Verifier and hands them in.
func NewLogsClient(ctx context.Context, c SessionCreds) (*cloudwatchlogs.Client, error) {
	cfg, err := ConfigFromCreds(ctx, c)
	if err != nil {
		return nil, err
	}
	return cloudwatchlogs.NewFromConfig(cfg), nil
}

// LogEvent is the trimmed view we expose to callers — just the parts the
// UI cares about. CloudWatch's OutputLogEvent has more fields (ingestion
// time) but they're not interesting to render.
type LogEvent struct {
	Timestamp int64  `json:"timestamp"` // unix milliseconds, from the producer's POV
	Message   string `json:"message"`
}

// GetBuildLogEventsParams is the input shape. StartTimeMs is the
// "after" cursor in unix-millis: 0 means "from the head of the stream";
// non-zero means "events with timestamp strictly greater than this."
//
// We deliberately do *not* use CloudWatch's NextForwardToken across
// calls. In practice that token does not reliably advance to pick up
// newly-written events while a builder is actively producing logs —
// the symptom we hit was an empty page returned for the entire 2-3
// minute Kaniko run, then everything flooding in once the container
// exited. Timestamp-based pagination sidesteps that quirk: each poll
// asks CloudWatch directly for events past the highest timestamp the
// caller has seen, and CloudWatch reads from its index instead of
// chasing a stale token.
type GetBuildLogEventsParams struct {
	LogGroupName  string
	LogStreamName string

	// StartTimeMs is the unix-milliseconds floor (exclusive) for the
	// next batch. Pass back the previous response's NextStartTimeMs.
	// 0 starts from the head of the stream.
	StartTimeMs int64

	// Limit caps events per call. 0 falls back to CloudWatch's default
	// (10,000 events / 1MB).
	Limit int32
}

// GetBuildLogEventsResult is one page of events plus the cursor to
// resume from. NextStartTimeMs is the highest timestamp we observed
// (or the caller's input when no events came back) — the caller round-
// trips it on the next call. HasMore is a hint that the page was
// saturated and more events are likely available right now, so the
// caller can poll faster instead of backing off.
type GetBuildLogEventsResult struct {
	Events          []LogEvent
	NextStartTimeMs int64
	HasMore         bool
}

// GetBuildLogEvents pulls one page of log events using a timestamp
// cursor. Always asks CloudWatch with StartFromHead=true so events
// arrive in chronological order; bounds the lower edge with startTime
// when the caller has a non-zero cursor.
//
// ResourceNotFoundException is treated as "stream not yet visible" —
// the normal state between RunTask and the first builder log line —
// and reported as an empty page that preserves the caller's cursor so
// the next poll picks up exactly where they left off.
func GetBuildLogEvents(ctx context.Context, c LogsClient, p GetBuildLogEventsParams) (GetBuildLogEventsResult, error) {
	if c == nil {
		return GetBuildLogEventsResult{}, errors.New("aws: nil logs client")
	}
	if p.LogGroupName == "" || p.LogStreamName == "" {
		return GetBuildLogEventsResult{}, errors.New("aws: log group and stream required")
	}

	in := &cloudwatchlogs.GetLogEventsInput{
		LogGroupName:  awssdk.String(p.LogGroupName),
		LogStreamName: awssdk.String(p.LogStreamName),
		StartFromHead: awssdk.Bool(true),
	}
	if p.Limit > 0 {
		in.Limit = awssdk.Int32(p.Limit)
	}
	if p.StartTimeMs > 0 {
		// CloudWatch's startTime is inclusive. We already returned the
		// event at exactly StartTimeMs on the previous poll, so bump by
		// one to avoid duplicating it. Loses any events that arrived in
		// the same millisecond as the previous batch's tail — acceptable
		// for human-readable build logs where lines are tens of ms apart.
		in.StartTime = awssdk.Int64(p.StartTimeMs + 1)
	}

	out, err := c.GetLogEvents(ctx, in)
	if err != nil {
		var nf *cwltypes.ResourceNotFoundException
		if errors.As(err, &nf) {
			return GetBuildLogEventsResult{NextStartTimeMs: p.StartTimeMs}, nil
		}
		return GetBuildLogEventsResult{}, fmt.Errorf("aws: get log events: %w", err)
	}

	events := make([]LogEvent, 0, len(out.Events))
	maxTs := p.StartTimeMs
	for _, e := range out.Events {
		var ts int64
		if e.Timestamp != nil {
			ts = *e.Timestamp
		}
		var msg string
		if e.Message != nil {
			msg = *e.Message
		}
		events = append(events, LogEvent{Timestamp: ts, Message: msg})
		if ts > maxTs {
			maxTs = ts
		}
	}

	// HasMore = "the page was saturated, there's likely more right now."
	// The caller polls fast in that case and backs off on a partial page.
	limit := p.Limit
	if limit <= 0 {
		limit = 10000
	}
	hasMore := int32(len(events)) >= limit

	return GetBuildLogEventsResult{
		Events:          events,
		NextStartTimeMs: maxTs,
		HasMore:         hasMore,
	}, nil
}
