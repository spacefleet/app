package builds

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/spacefleet/app/ent"
	awsint "github.com/spacefleet/app/lib/aws"
	"github.com/spacefleet/app/lib/github"
	"github.com/spacefleet/app/lib/pulumi"
)

// fakeOrchestrator stubs *pulumi.Orchestrator. UpAppBuild returns
// canned outputs (or an error); DestroyAppBuild is a no-op. We don't
// exercise the underlying Pulumi runner here — that's covered in
// lib/pulumi tests.
type fakeOrchestrator struct {
	upErr    error
	infraOut pulumi.BuilderInfraOutputs
	appOut   pulumi.AppBuildOutputs
	upCalls  int
}

func (f *fakeOrchestrator) UpAppBuild(_ context.Context, _ pulumi.AccountTarget, _ pulumi.AppRef, _ pulumi.RunOpts) (pulumi.BuilderInfraOutputs, pulumi.AppBuildOutputs, error) {
	f.upCalls++
	if f.upErr != nil {
		return pulumi.BuilderInfraOutputs{}, pulumi.AppBuildOutputs{}, f.upErr
	}
	return f.infraOut, f.appOut, nil
}

func (f *fakeOrchestrator) DestroyAppBuild(_ context.Context, _ pulumi.AccountTarget, _ string, _ pulumi.RunOpts) error {
	return nil
}

type fakeTokenIssuer struct {
	token string
	err   error
}

func (f *fakeTokenIssuer) IssueInstallationToken(_ context.Context, _ string, _ int64) (*github.AccessToken, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &github.AccessToken{Token: f.token, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// fakeECSForWorker captures RunTask calls so the test can assert on the
// container env override the worker assembled.
type fakeECSForWorker struct {
	runIn   *ecs.RunTaskInput
	taskARN string
	runErr  error
}

func (f *fakeECSForWorker) RunTask(_ context.Context, in *ecs.RunTaskInput, _ ...func(*ecs.Options)) (*ecs.RunTaskOutput, error) {
	f.runIn = in
	if f.runErr != nil {
		return nil, f.runErr
	}
	return &ecs.RunTaskOutput{
		Tasks: []ecstypes.Task{{TaskArn: awssdk.String(f.taskARN)}},
	}, nil
}

func (f *fakeECSForWorker) DescribeTasks(_ context.Context, _ *ecs.DescribeTasksInput, _ ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error) {
	return &ecs.DescribeTasksOutput{}, nil
}

func (f *fakeECSForWorker) StopTask(_ context.Context, _ *ecs.StopTaskInput, _ ...func(*ecs.Options)) (*ecs.StopTaskOutput, error) {
	return &ecs.StopTaskOutput{}, nil
}

// fakeSecretsForWorker satisfies awsint.SecretsClient using the SDK's
// real input/output types.
type fakeSecretsForWorker struct {
	createIn  *secretsmanager.CreateSecretInput
	createARN string
	createErr error
	deleteIn  *secretsmanager.DeleteSecretInput
}

func (f *fakeSecretsForWorker) CreateSecret(_ context.Context, in *secretsmanager.CreateSecretInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.CreateSecretOutput, error) {
	f.createIn = in
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &secretsmanager.CreateSecretOutput{ARN: awssdk.String(f.createARN)}, nil
}

func (f *fakeSecretsForWorker) PutSecretValue(_ context.Context, _ *secretsmanager.PutSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.PutSecretValueOutput, error) {
	return &secretsmanager.PutSecretValueOutput{ARN: awssdk.String(f.createARN)}, nil
}

func (f *fakeSecretsForWorker) DeleteSecret(_ context.Context, in *secretsmanager.DeleteSecretInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.DeleteSecretOutput, error) {
	f.deleteIn = in
	return &secretsmanager.DeleteSecretOutput{}, nil
}

// connectedFixture seeds a connected cloud account with role + external
// id so the worker's AssumeRoleEnv path actually has values to thread
// in. Builds on the bare appFixture from service_test.go.
func connectedFixture(t *testing.T) (*ent.Client, *appFixture) {
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

func newWorkerForTest(t *testing.T, client *ent.Client, orch Orchestrator, ecsClient *fakeECSForWorker, secretsClient *fakeSecretsForWorker, gh TokenIssuer) *Worker {
	t.Helper()
	w, err := NewWorker(WorkerConfig{
		Ent:          client,
		DB:           rawDBFromClient(t, client),
		Orchestrator: orch,
		Verifier:     &fakeVerifier{},
		GitHub:       gh,
		ECSClient: func(_ context.Context, _ awsint.SessionCreds) (awsint.ECSClient, error) {
			return ecsClient, nil
		},
		SecretsClient: func(_ context.Context, _ awsint.SessionCreds) (awsint.SecretsClient, error) {
			return secretsClient, nil
		},
		PublicURL:    "https://app.spacefleet.test",
		BuildTimeout: 60 * time.Minute,
		Stdout:       io.Discard,
		Stderr:       io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestWorker_HappyPath(t *testing.T) {
	client, fix := connectedFixture(t)

	row, err := client.Build.Create().
		SetAppID(fix.app.ID).
		SetSourceRef("main").
		SetSourceSha(strings.Repeat("a", 40)).
		SetWebhookSecret("secret").
		SetCreatedBy("user").
		Save(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	orch := &fakeOrchestrator{
		infraOut: pulumi.BuilderInfraOutputs{
			ClusterARN:       "arn:aws:ecs:us-west-2:222222222222:cluster/spacefleet-builds",
			SubnetID:         "subnet-1",
			SecurityGroupID:  "sg-1",
			ExecutionRoleARN: "arn:aws:iam::222222222222:role/spacefleet-ecs-execution",
		},
		appOut: pulumi.AppBuildOutputs{
			ECRRepoURI:        "222222222222.dkr.ecr.us-west-2.amazonaws.com/spacefleet-x",
			ECRCacheRepoURI:   "222222222222.dkr.ecr.us-west-2.amazonaws.com/spacefleet-x-cache",
			TaskDefinitionARN: "arn:aws:ecs:us-west-2:222222222222:task-definition/spacefleet-build-x:3",
			LogGroupName:      "/spacefleet/builds/x",
		},
	}
	ecsClient := &fakeECSForWorker{taskARN: "arn:aws:ecs:us-west-2:222222222222:task/spacefleet-builds/abcd"}
	secretsClient := &fakeSecretsForWorker{createARN: "arn:aws:secretsmanager:us-west-2:222222222222:secret:spacefleet/builds/x/y/github-token-aBc"}
	gh := &fakeTokenIssuer{token: "ghs_token"}

	w := newWorkerForTest(t, client, orch, ecsClient, secretsClient, gh)

	if err := w.Work(context.Background(), &river.Job[BuildJobArgs]{Args: BuildJobArgs{BuildID: row.ID}}); err != nil {
		t.Fatalf("Work: %v", err)
	}

	got, err := client.Build.Get(context.Background(), row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != BuildStatusRunning {
		t.Errorf("status = %q, want running", got.Status)
	}
	if got.FargateTaskArn != ecsClient.taskARN {
		t.Errorf("fargate_task_arn = %q", got.FargateTaskArn)
	}
	if got.LogGroup != "/spacefleet/builds/x" {
		t.Errorf("log_group = %q", got.LogGroup)
	}
	if got.StartedAt == nil {
		t.Error("expected started_at")
	}

	// Stage events: reconcile{run/ok}, prepare{run/ok}, dispatch{run/ok} = 6
	if len(got.Stages) != 6 {
		t.Fatalf("stages = %d (%+v)", len(got.Stages), got.Stages)
	}
	wantSeq := []struct{ name, status string }{
		{StageReconcile, StatusRunning},
		{StageReconcile, StatusSucceeded},
		{StagePrepare, StatusRunning},
		{StagePrepare, StatusSucceeded},
		{StageDispatch, StatusRunning},
		{StageDispatch, StatusSucceeded},
	}
	for i, w := range wantSeq {
		if got.Stages[i].Name != w.name || got.Stages[i].Status != w.status {
			t.Errorf("stage[%d] = %s/%s, want %s/%s", i, got.Stages[i].Name, got.Stages[i].Status, w.name, w.status)
		}
	}

	// Container env should carry the values the entrypoint expects.
	overrides := ecsClient.runIn.Overrides.ContainerOverrides[0].Environment
	envMap := map[string]string{}
	for _, kv := range overrides {
		envMap[*kv.Name] = *kv.Value
	}
	if envMap["SPACEFLEET_BUILD_ID"] != row.ID.String() {
		t.Errorf("SPACEFLEET_BUILD_ID = %q", envMap["SPACEFLEET_BUILD_ID"])
	}
	if envMap["SPACEFLEET_WEBHOOK_SECRET"] != "secret" {
		t.Errorf("webhook secret leaked wrong value")
	}
	wantWebhookURL := "https://app.spacefleet.test/api/internal/builds/" + row.ID.String() + "/events"
	if envMap["SPACEFLEET_WEBHOOK_URL"] != wantWebhookURL {
		t.Errorf("webhook url = %q", envMap["SPACEFLEET_WEBHOOK_URL"])
	}
	if envMap["GITHUB_TOKEN_SECRET_ARN"] != secretsClient.createARN {
		t.Errorf("secret arn mismatch: %q vs %q", envMap["GITHUB_TOKEN_SECRET_ARN"], secretsClient.createARN)
	}
	if envMap["COMMIT_SHA"] != row.SourceSha {
		t.Errorf("commit sha = %q", envMap["COMMIT_SHA"])
	}
}

func TestWorker_PerAppSerialization(t *testing.T) {
	// Two builds for the same app: the second should snooze. We use
	// JobSnooze's exposed sentinel via errors.As — River returns its
	// own error type from JobSnooze.
	client, fix := connectedFixture(t)
	ctx := context.Background()

	mk := func() *ent.Build {
		row, err := client.Build.Create().
			SetAppID(fix.app.ID).
			SetSourceRef("main").
			SetSourceSha(strings.Repeat("a", 40)).
			SetWebhookSecret("s").
			SetCreatedBy("u").
			Save(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	first := mk()
	second := mk()

	// Promote first to running directly so the worker's promoteToRunning
	// for second sees a sibling.
	if _, err := client.Build.UpdateOneID(first.ID).SetStatus(BuildStatusRunning).Save(ctx); err != nil {
		t.Fatal(err)
	}

	w := newWorkerForTest(t, client, &fakeOrchestrator{}, &fakeECSForWorker{}, &fakeSecretsForWorker{}, &fakeTokenIssuer{})
	err := w.Work(ctx, &river.Job[BuildJobArgs]{Args: BuildJobArgs{BuildID: second.ID}})
	if err == nil {
		t.Fatal("expected JobSnooze, got nil")
	}
	// River's JobSnooze returns a *JobSnoozeError. We just confirm the
	// returned error references "snooze" so the assertion is portable
	// across River minor versions.
	if !strings.Contains(strings.ToLower(err.Error()), "snooze") {
		t.Errorf("expected snooze-style error, got %v", err)
	}

	// Sanity: second is still queued — no row mutation happened.
	got, _ := client.Build.Get(ctx, second.ID)
	if got.Status != BuildStatusQueued {
		t.Errorf("second.status = %q, want queued", got.Status)
	}
}

func TestWorker_ReconcileFailureRecorded(t *testing.T) {
	client, fix := connectedFixture(t)
	row, _ := client.Build.Create().
		SetAppID(fix.app.ID).
		SetSourceRef("main").
		SetSourceSha(strings.Repeat("a", 40)).
		SetWebhookSecret("s").
		SetCreatedBy("u").
		Save(context.Background())

	orch := &fakeOrchestrator{upErr: errors.New("AssumeRole denied")}
	w := newWorkerForTest(t, client, orch, &fakeECSForWorker{}, &fakeSecretsForWorker{}, &fakeTokenIssuer{})

	if err := w.Work(context.Background(), &river.Job[BuildJobArgs]{Args: BuildJobArgs{BuildID: row.ID}}); err != nil {
		t.Fatalf("Work returned err (should record + return nil): %v", err)
	}

	got, err := client.Build.Get(context.Background(), row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != BuildStatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.ErrorMessage, "AssumeRole denied") {
		t.Errorf("error_message = %q", got.ErrorMessage)
	}
	// Stages: reconcile{run, fail} = 2
	if len(got.Stages) != 2 {
		t.Fatalf("stages = %d", len(got.Stages))
	}
	if got.Stages[1].Status != StatusFailed || got.Stages[1].Name != StageReconcile {
		t.Errorf("last stage = %+v", got.Stages[1])
	}
}

func TestWorker_DispatchFailureCleansUpSecret(t *testing.T) {
	client, fix := connectedFixture(t)
	row, _ := client.Build.Create().
		SetAppID(fix.app.ID).
		SetSourceRef("main").
		SetSourceSha(strings.Repeat("a", 40)).
		SetWebhookSecret("s").
		SetCreatedBy("u").
		Save(context.Background())

	orch := &fakeOrchestrator{
		infraOut: pulumi.BuilderInfraOutputs{
			ClusterARN: "arn:aws:ecs:us-west-2:222:cluster/x", SubnetID: "s", SecurityGroupID: "sg", ExecutionRoleARN: "arn",
		},
		appOut: pulumi.AppBuildOutputs{TaskDefinitionARN: "arn:aws:ecs:us-west-2:222:task-definition/x:1", ECRRepoURI: "u", ECRCacheRepoURI: "c", LogGroupName: "lg"},
	}
	ecsClient := &fakeECSForWorker{runErr: errors.New("RESOURCE:CPU")}
	secretsClient := &fakeSecretsForWorker{createARN: "arn:aws:secretsmanager:us-west-2:222:secret:spacefleet/builds/x/y/github-token-z"}

	w := newWorkerForTest(t, client, orch, ecsClient, secretsClient, &fakeTokenIssuer{token: "tok"})
	if err := w.Work(context.Background(), &river.Job[BuildJobArgs]{Args: BuildJobArgs{BuildID: row.ID}}); err != nil {
		t.Fatal(err)
	}

	got, _ := client.Build.Get(context.Background(), row.ID)
	if got.Status != BuildStatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if secretsClient.deleteIn == nil {
		t.Error("expected secret cleanup on dispatch failure")
	}
}

// TestWorker_RunTaskUsesBuildIDClientToken pins the contract that a
// retry of the dispatch path can't spawn a duplicate Fargate task: the
// build ID is passed to ECS as ClientToken so the second RunTask call
// returns the original task instead of creating a new one.
func TestWorker_RunTaskUsesBuildIDClientToken(t *testing.T) {
	client, fix := connectedFixture(t)
	row, _ := client.Build.Create().
		SetAppID(fix.app.ID).
		SetSourceRef("main").
		SetSourceSha(strings.Repeat("a", 40)).
		SetWebhookSecret("s").
		SetCreatedBy("u").
		Save(context.Background())

	orch := &fakeOrchestrator{
		infraOut: pulumi.BuilderInfraOutputs{ClusterARN: "arn:aws:ecs:us-west-2:222:cluster/x", SubnetID: "s", SecurityGroupID: "sg", ExecutionRoleARN: "arn"},
		appOut:   pulumi.AppBuildOutputs{TaskDefinitionARN: "arn:aws:ecs:us-west-2:222:task-definition/x:1", ECRRepoURI: "u", ECRCacheRepoURI: "c", LogGroupName: "lg"},
	}
	ecsClient := &fakeECSForWorker{taskARN: "arn:aws:ecs:us-west-2:222:task/x/abcd"}
	secretsClient := &fakeSecretsForWorker{createARN: "arn:aws:secretsmanager:us-west-2:222:secret:spacefleet/builds/x/y/github-token-z"}

	w := newWorkerForTest(t, client, orch, ecsClient, secretsClient, &fakeTokenIssuer{token: "tok"})
	if err := w.Work(context.Background(), &river.Job[BuildJobArgs]{Args: BuildJobArgs{BuildID: row.ID}}); err != nil {
		t.Fatal(err)
	}

	if ecsClient.runIn == nil || ecsClient.runIn.ClientToken == nil {
		t.Fatal("RunTask: ClientToken not set")
	}
	if got := *ecsClient.runIn.ClientToken; got != row.ID.String() {
		t.Errorf("ClientToken = %q, want build id %q", got, row.ID)
	}
}

// TestWorker_RetryAfterDispatchSkipsRedispatch verifies that a second
// invocation of Work on a build whose FargateTaskArn is already
// persisted is a no-op — no second token mint, no second RunTask. This
// is the recovery-from-River-retry path.
func TestWorker_RetryAfterDispatchSkipsRedispatch(t *testing.T) {
	client, fix := connectedFixture(t)
	row, _ := client.Build.Create().
		SetAppID(fix.app.ID).
		SetSourceRef("main").
		SetSourceSha(strings.Repeat("a", 40)).
		SetWebhookSecret("s").
		SetCreatedBy("u").
		Save(context.Background())

	// Simulate a previous successful dispatch that persisted the ARN.
	if _, err := client.Build.UpdateOneID(row.ID).
		SetStatus(BuildStatusRunning).
		SetFargateTaskArn("arn:aws:ecs:us-west-2:222:task/x/already-running").
		Save(context.Background()); err != nil {
		t.Fatal(err)
	}

	orch := &fakeOrchestrator{}
	ecsClient := &fakeECSForWorker{}
	secretsClient := &fakeSecretsForWorker{}
	gh := &fakeTokenIssuer{token: "tok"}

	w := newWorkerForTest(t, client, orch, ecsClient, secretsClient, gh)
	if err := w.Work(context.Background(), &river.Job[BuildJobArgs]{Args: BuildJobArgs{BuildID: row.ID}}); err != nil {
		t.Fatal(err)
	}

	if orch.upCalls != 0 {
		t.Errorf("orchestrator called on retry: %d times", orch.upCalls)
	}
	if ecsClient.runIn != nil {
		t.Error("RunTask called on retry — duplicate dispatch leaked")
	}
	if secretsClient.createIn != nil {
		t.Error("PutBuildTokenSecret called on retry — token re-minted needlessly")
	}
}

func TestWorker_AdvisoryLockKeyDeterministic(t *testing.T) {
	a := uuid.New()
	if advisoryLockKey(a) != advisoryLockKey(a) {
		t.Error("advisoryLockKey not deterministic")
	}
	b := uuid.New()
	// Probabilistically distinct; FNV-64 has negligible collision risk.
	if advisoryLockKey(a) == advisoryLockKey(b) {
		t.Error("advisoryLockKey collided on two random UUIDs (very unlikely)")
	}
}
