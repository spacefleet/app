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

// GetBuildLogEventsParams is the input shape. NextToken is opaque to us:
// CloudWatch returns one and we round-trip it back on the next call.
//
// StartFromHead=true asks CloudWatch for the *oldest* events first when
// no NextToken is given. That's what we want for a build log — the user
// wants to see the build from the start, not the end. Once a token is
// in play, StartFromHead is ignored by the API.
type GetBuildLogEventsParams struct {
	LogGroupName  string
	LogStreamName string
	NextToken     string
	Limit         int32 // 0 means "use CloudWatch default" (10,000 events / 1MB)
}

// GetBuildLogEventsResult bundles a page of events with the pagination
// state. HasMore reflects whether NextForwardToken differs from the one
// the caller passed in — CloudWatch returns the same token on a tail
// page, which is how the consumer knows to stop polling.
type GetBuildLogEventsResult struct {
	Events    []LogEvent
	NextToken string
	HasMore   bool
}

// GetBuildLogEvents pulls one page of log events. Wraps
// CloudWatchLogs.GetLogEvents and unwraps to the LogEvent shape.
//
// Behavior we need to be deliberate about:
//
//   - The first call has no NextToken. We pass StartFromHead=true so we
//     get the oldest events. (Without it, CloudWatch returns the tail.)
//   - A subsequent call passes the previous NextToken. CloudWatch keeps
//     returning the same NextForwardToken once we hit the end of the
//     stream, so callers detect "no new events" by comparing tokens.
//   - ResourceNotFoundException is treated as "stream not yet visible"
//     and reported as an empty result with no token; that's the normal
//     state between RunTask and the first builder log line.
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
	}
	if p.Limit > 0 {
		in.Limit = awssdk.Int32(p.Limit)
	}
	if p.NextToken == "" {
		// Oldest events first. The token-driven follow-up calls inherit
		// direction from the token itself, so we only set this on the
		// first page.
		in.StartFromHead = awssdk.Bool(true)
	} else {
		in.NextToken = awssdk.String(p.NextToken)
	}

	out, err := c.GetLogEvents(ctx, in)
	if err != nil {
		// A missing stream is the common "task hasn't logged yet" case;
		// treat as empty rather than surfacing the error and forcing
		// every caller to handle it.
		var nf *cwltypes.ResourceNotFoundException
		if errors.As(err, &nf) {
			return GetBuildLogEventsResult{}, nil
		}
		return GetBuildLogEventsResult{}, fmt.Errorf("aws: get log events: %w", err)
	}

	events := make([]LogEvent, 0, len(out.Events))
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
	}

	next := strDeref(out.NextForwardToken)
	// CloudWatch returns the *same* NextForwardToken once the caller has
	// consumed every event. Comparing to the caller's previous token is
	// how we surface "stream is fully drained" without an extra round-
	// trip.
	hasMore := next != "" && next != p.NextToken

	return GetBuildLogEventsResult{
		Events:    events,
		NextToken: next,
		HasMore:   hasMore,
	}, nil
}
