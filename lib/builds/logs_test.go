package builds

import (
	"context"
	"errors"
	"strings"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwltypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/google/uuid"

	"github.com/spacefleet/app/ent"
	awsint "github.com/spacefleet/app/lib/aws"
)

// fakeLogsClient implements awsint.LogsClient for the controller-level
// tests. We capture the input so each test can assert on the parameters
// the controller threaded through (group, stream, token, limit).
type fakeLogsClient struct {
	in  *cloudwatchlogs.GetLogEventsInput
	out *cloudwatchlogs.GetLogEventsOutput
	err error
}

func (f *fakeLogsClient) GetLogEvents(_ context.Context, in *cloudwatchlogs.GetLogEventsInput, _ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.GetLogEventsOutput, error) {
	f.in = in
	if f.err != nil {
		return nil, f.err
	}
	return f.out, nil
}

// connectedLogsFixture mirrors connectedFixture from worker_test.go: a
// cloud account with role+region populated so the controller's assume-
// role path has something to thread in.
func connectedLogsFixture(t *testing.T) (*ent.Client, *appFixture) {
	t.Helper()
	client := newTestClient(t)
	fix := newAppFixture(t, client)
	if _, err := client.CloudAccount.UpdateOneID(fix.cloudID).
		SetRoleArn("arn:aws:iam::222222222222:role/SpacefleetIntegrationRole").
		SetAccountID("222222222222").
		SetRegion("us-west-2").
		Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	return client, fix
}

// dispatchedBuild stamps a build row with realistic log_group/log_stream
// values so Fetch reaches the CloudWatch path. The status arg controls
// whether the build is still running or has reached terminal.
func dispatchedBuild(t *testing.T, client *ent.Client, fix *appFixture, status string) *ent.Build {
	t.Helper()
	b, err := client.Build.Create().
		SetAppID(fix.app.ID).
		SetSourceRef("main").
		SetSourceSha(strings.Repeat("a", 40)).
		SetWebhookSecret("test-secret").
		SetCreatedBy("user").
		SetStatus(status).
		SetLogGroup("/spacefleet/builds/" + fix.app.ID.String()).
		SetLogStream("builder/builder/abc-123").
		Save(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// newLogsControllerForTest builds a controller wired to a fakeLogsClient
// (so the test can assert on the SDK call) and our existing fakeVerifier
// (so AssumeRoleEnv returns a stub creds map without touching AWS).
func newLogsControllerForTest(t *testing.T, client *ent.Client, fc *fakeLogsClient) *LogsController {
	t.Helper()
	c, err := NewLogsController(LogsConfig{
		Ent:      client,
		Verifier: &fakeVerifier{},
		LogsClient: func(_ context.Context, _ awsint.SessionCreds) (awsint.LogsClient, error) {
			return fc, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLogsController_HappyPath(t *testing.T) {
	client, fix := connectedLogsFixture(t)
	build := dispatchedBuild(t, client, fix, BuildStatusRunning)

	fc := &fakeLogsClient{
		out: &cloudwatchlogs.GetLogEventsOutput{
			Events: []cwltypes.OutputLogEvent{
				{Timestamp: awssdk.Int64(1700000000000), Message: awssdk.String("starting kaniko")},
				{Timestamp: awssdk.Int64(1700000001000), Message: awssdk.String("building stage 1/3")},
			},
		},
	}
	ctrl := newLogsControllerForTest(t, client, fc)

	res, err := ctrl.Fetch(context.Background(), FetchParams{
		OrgSlug: "acme",
		AppSlug: "app",
		BuildID: build.ID,
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if fc.in == nil {
		t.Fatal("CloudWatch GetLogEvents not called")
	}
	if got := awssdk.ToString(fc.in.LogGroupName); got != build.LogGroup {
		t.Errorf("log group = %q want %q", got, build.LogGroup)
	}
	if got := awssdk.ToString(fc.in.LogStreamName); got != build.LogStream {
		t.Errorf("log stream = %q want %q", got, build.LogStream)
	}
	if fc.in.StartFromHead == nil || !*fc.in.StartFromHead {
		t.Error("expected StartFromHead=true on every page (chronological order)")
	}
	if fc.in.StartTime != nil {
		t.Errorf("expected no StartTime on first page, got %v", *fc.in.StartTime)
	}
	if got := awssdk.ToInt32(fc.in.Limit); got != DefaultLogsLimit {
		t.Errorf("limit = %d want default %d", got, DefaultLogsLimit)
	}
	if len(res.Events) != 2 {
		t.Errorf("events len = %d", len(res.Events))
	}
	if res.Events[0].Message != "starting kaniko" {
		t.Errorf("event 0 = %+v", res.Events[0])
	}
	if res.NextStartTimeMs != 1700000001000 {
		t.Errorf("next cursor = %d want 1700000001000 (max event timestamp)", res.NextStartTimeMs)
	}
	if res.HasMore {
		t.Error("HasMore=false: 2 events is far below the default limit")
	}
	if res.BuildTerminal {
		t.Error("BuildTerminal should be false for running build")
	}
}

func TestLogsController_ThreadsStartTime(t *testing.T) {
	client, fix := connectedLogsFixture(t)
	build := dispatchedBuild(t, client, fix, BuildStatusRunning)

	fc := &fakeLogsClient{out: &cloudwatchlogs.GetLogEventsOutput{}}
	ctrl := newLogsControllerForTest(t, client, fc)

	res, err := ctrl.Fetch(context.Background(), FetchParams{
		OrgSlug:     "acme",
		AppSlug:     "app",
		BuildID:     build.ID,
		StartTimeMs: 1700000005000,
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if fc.in.StartTime == nil || *fc.in.StartTime != 1700000005001 {
		t.Errorf("StartTime not threaded (with +1 bump): %v", fc.in.StartTime)
	}
	if fc.in.StartFromHead == nil || !*fc.in.StartFromHead {
		t.Error("StartFromHead must stay true even when paging forward by time")
	}
	if res.HasMore {
		t.Error("expected HasMore=false on empty page")
	}
	if res.NextStartTimeMs != 1700000005000 {
		t.Errorf("cursor should round-trip on empty page: %d", res.NextStartTimeMs)
	}
}

func TestLogsController_PreDispatchReturnsEmpty(t *testing.T) {
	// A queued build hasn't had log_group/log_stream stamped yet. The
	// controller should return an empty page rather than 5xx-ing or
	// reaching out to AWS.
	client, fix := connectedLogsFixture(t)
	build, err := client.Build.Create().
		SetAppID(fix.app.ID).
		SetSourceRef("main").
		SetSourceSha(strings.Repeat("a", 40)).
		SetWebhookSecret("s").
		SetCreatedBy("u").
		SetStatus(BuildStatusQueued).
		Save(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	fc := &fakeLogsClient{}
	ctrl := newLogsControllerForTest(t, client, fc)

	res, err := ctrl.Fetch(context.Background(), FetchParams{
		OrgSlug: "acme",
		AppSlug: "app",
		BuildID: build.ID,
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if fc.in != nil {
		t.Error("expected no CloudWatch call for pre-dispatch build")
	}
	if len(res.Events) != 0 || res.NextStartTimeMs != 0 || res.HasMore {
		t.Errorf("expected empty result, got %+v", res)
	}
	if res.BuildTerminal {
		t.Error("queued build should not report BuildTerminal=true")
	}
}

func TestLogsController_TerminalBuildReportsBuildTerminal(t *testing.T) {
	client, fix := connectedLogsFixture(t)
	build := dispatchedBuild(t, client, fix, BuildStatusSucceeded)

	fc := &fakeLogsClient{out: &cloudwatchlogs.GetLogEventsOutput{}}
	ctrl := newLogsControllerForTest(t, client, fc)

	res, err := ctrl.Fetch(context.Background(), FetchParams{
		OrgSlug:     "acme",
		AppSlug:     "app",
		BuildID:     build.ID,
		StartTimeMs: 1700000010000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.BuildTerminal {
		t.Error("expected BuildTerminal=true for succeeded build")
	}
	if res.HasMore {
		t.Error("expected HasMore=false on empty drained page")
	}
	if res.NextStartTimeMs != 1700000010000 {
		t.Errorf("cursor should round-trip on terminal+empty page: %d", res.NextStartTimeMs)
	}
}

func TestLogsController_StreamMissingTreatedAsEmpty(t *testing.T) {
	// CloudWatch returns ResourceNotFoundException between RunTask and
	// the first builder log line. The controller should expose this as
	// an empty page so the UI keeps polling without seeing an error.
	client, fix := connectedLogsFixture(t)
	build := dispatchedBuild(t, client, fix, BuildStatusRunning)

	fc := &fakeLogsClient{err: &cwltypes.ResourceNotFoundException{Message: awssdk.String("nope")}}
	ctrl := newLogsControllerForTest(t, client, fc)

	res, err := ctrl.Fetch(context.Background(), FetchParams{
		OrgSlug: "acme",
		AppSlug: "app",
		BuildID: build.ID,
	})
	if err != nil {
		t.Fatalf("expected nil error on stream-missing, got %v", err)
	}
	if len(res.Events) != 0 || res.HasMore {
		t.Errorf("expected empty page, got %+v", res)
	}
}

func TestLogsController_AssumeRoleErrorBubbles(t *testing.T) {
	client, fix := connectedLogsFixture(t)
	build := dispatchedBuild(t, client, fix, BuildStatusRunning)

	ctrl, err := NewLogsController(LogsConfig{
		Ent:      client,
		Verifier: &fakeVerifier{err: errors.New("denied")},
		LogsClient: func(_ context.Context, _ awsint.SessionCreds) (awsint.LogsClient, error) {
			return &fakeLogsClient{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := ctrl.Fetch(context.Background(), FetchParams{
		OrgSlug: "acme",
		AppSlug: "app",
		BuildID: build.ID,
	}); err == nil {
		t.Fatal("expected error on assume-role failure")
	}
}

func TestLogsController_BuildScopedToOrgAndApp(t *testing.T) {
	client, fix := connectedLogsFixture(t)
	build := dispatchedBuild(t, client, fix, BuildStatusRunning)

	fc := &fakeLogsClient{out: &cloudwatchlogs.GetLogEventsOutput{}}
	ctrl := newLogsControllerForTest(t, client, fc)

	// Wrong app slug: should surface as ErrAppNotFound, not leak
	// information.
	if _, err := ctrl.Fetch(context.Background(), FetchParams{
		OrgSlug: "acme",
		AppSlug: "other-app",
		BuildID: build.ID,
	}); !errors.Is(err, ErrAppNotFound) {
		t.Errorf("wrong-app err = %v want ErrAppNotFound", err)
	}

	// Right app, but a build id that doesn't exist: ErrBuildNotFound.
	if _, err := ctrl.Fetch(context.Background(), FetchParams{
		OrgSlug: "acme",
		AppSlug: "app",
		BuildID: uuid.New(),
	}); !errors.Is(err, ErrBuildNotFound) {
		t.Errorf("missing-build err = %v want ErrBuildNotFound", err)
	}
}

func TestLogsController_LimitClampedToMax(t *testing.T) {
	client, fix := connectedLogsFixture(t)
	build := dispatchedBuild(t, client, fix, BuildStatusRunning)

	fc := &fakeLogsClient{out: &cloudwatchlogs.GetLogEventsOutput{}}
	ctrl, err := NewLogsController(LogsConfig{
		Ent:      client,
		Verifier: &fakeVerifier{},
		LogsClient: func(_ context.Context, _ awsint.SessionCreds) (awsint.LogsClient, error) {
			return fc, nil
		},
		MaxLimit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := ctrl.Fetch(context.Background(), FetchParams{
		OrgSlug: "acme",
		AppSlug: "app",
		BuildID: build.ID,
		Limit:   10000,
	}); err != nil {
		t.Fatal(err)
	}
	if got := awssdk.ToInt32(fc.in.Limit); got != 100 {
		t.Errorf("limit = %d want clamped to 100", got)
	}
}

func TestLogsController_Validate(t *testing.T) {
	if _, err := NewLogsController(LogsConfig{}); err == nil {
		t.Error("expected error on empty config")
	}
	if _, err := NewLogsController(LogsConfig{Ent: &ent.Client{}, Verifier: &fakeVerifier{}}); err == nil {
		t.Error("expected error on missing LogsClient factory")
	}
}
