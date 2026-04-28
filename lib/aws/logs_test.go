package aws

import (
	"context"
	"errors"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwltypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
)

// fakeLogs records the GetLogEvents input and serves a preprogrammed
// response or error. Mirrors the fakeECS / fakeSecrets pattern used by
// the rest of lib/aws.
type fakeLogs struct {
	in  *cloudwatchlogs.GetLogEventsInput
	out *cloudwatchlogs.GetLogEventsOutput
	err error
}

func (f *fakeLogs) GetLogEvents(_ context.Context, in *cloudwatchlogs.GetLogEventsInput, _ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.GetLogEventsOutput, error) {
	f.in = in
	if f.err != nil {
		return nil, f.err
	}
	return f.out, nil
}

func TestGetBuildLogEvents_FirstPageStartsFromHead(t *testing.T) {
	c := &fakeLogs{
		out: &cloudwatchlogs.GetLogEventsOutput{
			Events: []cwltypes.OutputLogEvent{{
				Timestamp: awssdk.Int64(1700000000000),
				Message:   awssdk.String("hello"),
			}},
			NextForwardToken: awssdk.String("f/123"),
		},
	}
	res, err := GetBuildLogEvents(context.Background(), c, GetBuildLogEventsParams{
		LogGroupName:  "/spacefleet/builds/app",
		LogStreamName: "builder/x/task",
		Limit:         100,
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if c.in == nil {
		t.Fatal("GetLogEvents not called")
	}
	if c.in.StartFromHead == nil || !*c.in.StartFromHead {
		t.Error("expected StartFromHead=true on every page")
	}
	if c.in.StartTime != nil {
		t.Errorf("expected no StartTime on first page, got %v", *c.in.StartTime)
	}
	if c.in.NextToken != nil {
		t.Errorf("we no longer use the forward-token cursor: %v", *c.in.NextToken)
	}
	if len(res.Events) != 1 || res.Events[0].Message != "hello" {
		t.Errorf("events = %+v", res.Events)
	}
	if res.NextStartTimeMs != 1700000000000 {
		t.Errorf("next start = %d want 1700000000000", res.NextStartTimeMs)
	}
	if res.HasMore {
		t.Error("expected HasMore=false (1 event < limit 100)")
	}
}

func TestGetBuildLogEvents_StartTimeBumpsByOne(t *testing.T) {
	// The cursor is exclusive: an event at exactly StartTimeMs was
	// already returned on the previous poll, so we ask CW for events
	// strictly after it.
	c := &fakeLogs{
		out: &cloudwatchlogs.GetLogEventsOutput{
			Events: []cwltypes.OutputLogEvent{{
				Timestamp: awssdk.Int64(1700000005000),
				Message:   awssdk.String("next"),
			}},
		},
	}
	res, err := GetBuildLogEvents(context.Background(), c, GetBuildLogEventsParams{
		LogGroupName:  "/spacefleet/builds/app",
		LogStreamName: "builder/x/task",
		StartTimeMs:   1700000004000,
		Limit:         100,
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if c.in.StartTime == nil || *c.in.StartTime != 1700000004001 {
		t.Errorf("StartTime not bumped: %v", c.in.StartTime)
	}
	if c.in.StartFromHead == nil || !*c.in.StartFromHead {
		t.Error("StartFromHead must stay true so events arrive oldest-first")
	}
	if res.NextStartTimeMs != 1700000005000 {
		t.Errorf("next start = %d want 1700000005000", res.NextStartTimeMs)
	}
}

func TestGetBuildLogEvents_EmptyPagePreservesCursor(t *testing.T) {
	// If CloudWatch returns no events, the cursor doesn't move: the
	// next poll re-asks for events past the same timestamp until new
	// events arrive.
	c := &fakeLogs{out: &cloudwatchlogs.GetLogEventsOutput{}}
	res, err := GetBuildLogEvents(context.Background(), c, GetBuildLogEventsParams{
		LogGroupName:  "/spacefleet/builds/app",
		LogStreamName: "builder/x/task",
		StartTimeMs:   1700000000000,
		Limit:         100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.HasMore {
		t.Error("expected HasMore=false on empty page")
	}
	if res.NextStartTimeMs != 1700000000000 {
		t.Errorf("cursor should round-trip: %d", res.NextStartTimeMs)
	}
}

func TestGetBuildLogEvents_FullPageSignalsHasMore(t *testing.T) {
	// HasMore=true tells the controller "the page filled up; keep
	// polling at the active cadence."
	events := make([]cwltypes.OutputLogEvent, 50)
	for i := range events {
		events[i] = cwltypes.OutputLogEvent{
			Timestamp: awssdk.Int64(int64(1700000000000 + i)),
			Message:   awssdk.String("line"),
		}
	}
	c := &fakeLogs{out: &cloudwatchlogs.GetLogEventsOutput{Events: events}}
	res, err := GetBuildLogEvents(context.Background(), c, GetBuildLogEventsParams{
		LogGroupName:  "g",
		LogStreamName: "s",
		Limit:         50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.HasMore {
		t.Error("expected HasMore=true on saturated page")
	}
}

func TestGetBuildLogEvents_LimitForwarded(t *testing.T) {
	c := &fakeLogs{out: &cloudwatchlogs.GetLogEventsOutput{}}
	_, err := GetBuildLogEvents(context.Background(), c, GetBuildLogEventsParams{
		LogGroupName:  "g",
		LogStreamName: "s",
		Limit:         500,
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.in.Limit == nil || *c.in.Limit != 500 {
		t.Errorf("limit = %v", c.in.Limit)
	}
}

func TestGetBuildLogEvents_StreamNotFoundPreservesCursor(t *testing.T) {
	// ResourceNotFoundException is the gap between RunTask and the first
	// awslogs PutLogEvents. Surface as empty + the caller's cursor so
	// they don't lose their place.
	c := &fakeLogs{err: &cwltypes.ResourceNotFoundException{Message: awssdk.String("nope")}}
	res, err := GetBuildLogEvents(context.Background(), c, GetBuildLogEventsParams{
		LogGroupName:  "g",
		LogStreamName: "s",
		StartTimeMs:   1700000000000,
	})
	if err != nil {
		t.Fatalf("expected nil error on ResourceNotFoundException, got %v", err)
	}
	if len(res.Events) != 0 || res.HasMore {
		t.Errorf("expected empty page, got %+v", res)
	}
	if res.NextStartTimeMs != 1700000000000 {
		t.Errorf("cursor should round-trip across stream-missing: %d", res.NextStartTimeMs)
	}
}

func TestGetBuildLogEvents_OtherErrorBubbles(t *testing.T) {
	c := &fakeLogs{err: errors.New("throttled")}
	if _, err := GetBuildLogEvents(context.Background(), c, GetBuildLogEventsParams{
		LogGroupName:  "g",
		LogStreamName: "s",
	}); err == nil {
		t.Fatal("expected error")
	}
}

func TestGetBuildLogEvents_RequiresGroupAndStream(t *testing.T) {
	if _, err := GetBuildLogEvents(context.Background(), &fakeLogs{}, GetBuildLogEventsParams{}); err == nil {
		t.Fatal("expected error on empty params")
	}
	if _, err := GetBuildLogEvents(context.Background(), nil, GetBuildLogEventsParams{LogGroupName: "g", LogStreamName: "s"}); err == nil {
		t.Fatal("expected error on nil client")
	}
}
