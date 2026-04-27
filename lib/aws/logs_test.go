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
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if c.in == nil {
		t.Fatal("GetLogEvents not called")
	}
	if c.in.StartFromHead == nil || !*c.in.StartFromHead {
		t.Error("expected StartFromHead=true on first page")
	}
	if c.in.NextToken != nil {
		t.Errorf("expected no NextToken on first page, got %v", *c.in.NextToken)
	}
	if len(res.Events) != 1 || res.Events[0].Message != "hello" {
		t.Errorf("events = %+v", res.Events)
	}
	if res.NextToken != "f/123" {
		t.Errorf("token = %q", res.NextToken)
	}
	if !res.HasMore {
		t.Error("expected HasMore=true on first page (no prior token to compare)")
	}
}

func TestGetBuildLogEvents_NextTokenSkipsStartFromHead(t *testing.T) {
	c := &fakeLogs{
		out: &cloudwatchlogs.GetLogEventsOutput{
			Events:           []cwltypes.OutputLogEvent{},
			NextForwardToken: awssdk.String("f/abc"),
		},
	}
	_, err := GetBuildLogEvents(context.Background(), c, GetBuildLogEventsParams{
		LogGroupName:  "/spacefleet/builds/app",
		LogStreamName: "builder/x/task",
		NextToken:     "f/abc",
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if c.in.StartFromHead != nil {
		t.Error("StartFromHead must be nil when NextToken is supplied")
	}
	if c.in.NextToken == nil || *c.in.NextToken != "f/abc" {
		t.Errorf("NextToken not threaded through: %v", c.in.NextToken)
	}
}

func TestGetBuildLogEvents_TailPageHasNoMore(t *testing.T) {
	// CloudWatch returns the same NextForwardToken once the caller has
	// drained the stream — that's how we detect "stop polling for now."
	c := &fakeLogs{
		out: &cloudwatchlogs.GetLogEventsOutput{
			NextForwardToken: awssdk.String("f/same"),
		},
	}
	res, err := GetBuildLogEvents(context.Background(), c, GetBuildLogEventsParams{
		LogGroupName:  "/spacefleet/builds/app",
		LogStreamName: "builder/x/task",
		NextToken:     "f/same",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.HasMore {
		t.Error("expected HasMore=false on tail page")
	}
	if res.NextToken != "f/same" {
		t.Errorf("token should round-trip: %q", res.NextToken)
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

func TestGetBuildLogEvents_StreamNotFoundReturnsEmpty(t *testing.T) {
	c := &fakeLogs{err: &cwltypes.ResourceNotFoundException{Message: awssdk.String("nope")}}
	res, err := GetBuildLogEvents(context.Background(), c, GetBuildLogEventsParams{
		LogGroupName:  "g",
		LogStreamName: "s",
	})
	if err != nil {
		t.Fatalf("expected nil error on ResourceNotFoundException, got %v", err)
	}
	if len(res.Events) != 0 || res.NextToken != "" || res.HasMore {
		t.Errorf("expected empty result, got %+v", res)
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
