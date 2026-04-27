package builds

import (
	"context"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/spacefleet/app/ent"
	awsint "github.com/spacefleet/app/lib/aws"
)

// fakeECSForPoller is a tiny ECS test double that returns a preset
// task status for any DescribeTasks call.
type fakeECSForPoller struct {
	stopCalled bool
	status     awsint.TaskStatus
}

func (f *fakeECSForPoller) RunTask(_ context.Context, _ *ecs.RunTaskInput, _ ...func(*ecs.Options)) (*ecs.RunTaskOutput, error) {
	return nil, nil
}

func (f *fakeECSForPoller) DescribeTasks(_ context.Context, _ *ecs.DescribeTasksInput, _ ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error) {
	t := ecstypes.Task{
		LastStatus:    awssdk.String(f.status.LastStatus),
		DesiredStatus: awssdk.String(f.status.DesiredStatus),
		StoppedReason: awssdk.String(f.status.StoppedReason),
	}
	if f.status.StopCode != "" {
		t.StopCode = ecstypes.TaskStopCode(f.status.StopCode)
	}
	if f.status.ExitCode != nil {
		t.Containers = []ecstypes.Container{{ExitCode: f.status.ExitCode}}
	}
	return &ecs.DescribeTasksOutput{Tasks: []ecstypes.Task{t}}, nil
}

func (f *fakeECSForPoller) StopTask(_ context.Context, _ *ecs.StopTaskInput, _ ...func(*ecs.Options)) (*ecs.StopTaskOutput, error) {
	f.stopCalled = true
	return &ecs.StopTaskOutput{}, nil
}

type fakeSecretsForPoller struct {
	deleteCalled bool
}

func (fakeSecretsForPoller) CreateSecret(_ context.Context, _ *secretsmanager.CreateSecretInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.CreateSecretOutput, error) {
	return nil, nil
}

func (fakeSecretsForPoller) PutSecretValue(_ context.Context, _ *secretsmanager.PutSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.PutSecretValueOutput, error) {
	return nil, nil
}

func (f *fakeSecretsForPoller) DeleteSecret(_ context.Context, _ *secretsmanager.DeleteSecretInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.DeleteSecretOutput, error) {
	f.deleteCalled = true
	return &secretsmanager.DeleteSecretOutput{}, nil
}

// runningBuildFixture seeds a build with status=running and a sane
// task ARN — the shape the poller expects to find.
func runningBuildFixture(t *testing.T) (*ent.Client, *appFixture, *ent.Build) {
	t.Helper()
	client, fix := connectedFixture(t)
	taskARN := "arn:aws:ecs:us-west-2:222222222222:task/spacefleet-builds/abcd1234"
	row, err := client.Build.Create().
		SetAppID(fix.app.ID).
		SetSourceRef("main").
		SetSourceSha(strings.Repeat("a", 40)).
		SetWebhookSecret("s").
		SetCreatedBy("u").
		SetStatus(BuildStatusRunning).
		SetFargateTaskArn(taskARN).
		SetStartedAt(time.Now()).
		Save(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return client, fix, row
}

func newPollerForTest(t *testing.T, client *ent.Client, ecsClient awsint.ECSClient, secretsClient awsint.SecretsClient) *Poller {
	t.Helper()
	p, err := NewPoller(PollerConfig{
		Ent:      client,
		DB:       rawDBFromClient(t, client),
		Verifier: &fakeVerifier{},
		ECSClient: func(_ context.Context, _ awsint.SessionCreds) (awsint.ECSClient, error) {
			return ecsClient, nil
		},
		SecretsClient: func(_ context.Context, _ awsint.SessionCreds) (awsint.SecretsClient, error) {
			return secretsClient, nil
		},
		BuildTimeout: 60 * time.Minute,
		Interval:     50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPoller_StoppedTaskMarksFailed(t *testing.T) {
	client, _, row := runningBuildFixture(t)
	var code int32 = 137
	ecsClient := &fakeECSForPoller{status: awsint.TaskStatus{
		LastStatus:    "STOPPED",
		StoppedReason: "Essential container exited",
		StopCode:      "EssentialContainerExited",
		ExitCode:      &code,
	}}
	secretsClient := &fakeSecretsForPoller{}
	p := newPollerForTest(t, client, ecsClient, secretsClient)

	p.tickOnce(context.Background())

	got, err := client.Build.Get(context.Background(), row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != BuildStatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.ErrorMessage, "Essential container exited") {
		t.Errorf("error_message = %q", got.ErrorMessage)
	}
	if !secretsClient.deleteCalled {
		t.Error("expected secret cleanup")
	}
}

func TestPoller_RunningTaskNoChange(t *testing.T) {
	client, _, row := runningBuildFixture(t)
	ecsClient := &fakeECSForPoller{status: awsint.TaskStatus{LastStatus: "RUNNING"}}
	p := newPollerForTest(t, client, ecsClient, &fakeSecretsForPoller{})
	p.tickOnce(context.Background())

	got, _ := client.Build.Get(context.Background(), row.ID)
	if got.Status != BuildStatusRunning {
		t.Errorf("status = %q (RUNNING task should not flip)", got.Status)
	}
}

func TestPoller_TimeoutStopsTask(t *testing.T) {
	client, _, row := runningBuildFixture(t)
	old := time.Now().Add(-2 * time.Hour)
	if _, err := client.Build.UpdateOneID(row.ID).SetStartedAt(old).Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	ecsClient := &fakeECSForPoller{status: awsint.TaskStatus{LastStatus: "RUNNING"}}
	secretsClient := &fakeSecretsForPoller{}
	p := newPollerForTest(t, client, ecsClient, secretsClient)
	p.tickOnce(context.Background())

	got, _ := client.Build.Get(context.Background(), row.ID)
	if got.Status != BuildStatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if !ecsClient.stopCalled {
		t.Error("expected StopTask call on timeout")
	}
	if !strings.Contains(got.ErrorMessage, "timeout") {
		t.Errorf("error_message = %q", got.ErrorMessage)
	}
}

func TestPoller_ProvisioningStuckMarksFailed(t *testing.T) {
	client, _, row := runningBuildFixture(t)
	ecsClient := &fakeECSForPoller{status: awsint.TaskStatus{LastStatus: "PROVISIONING"}}
	secretsClient := &fakeSecretsForPoller{}
	p := newPollerForTest(t, client, ecsClient, secretsClient)

	// First tick — record first-seen-at.
	p.tickOnce(context.Background())
	got, _ := client.Build.Get(context.Background(), row.ID)
	if got.Status != BuildStatusRunning {
		t.Errorf("first tick status = %q (should still be running)", got.Status)
	}

	// Backdate the tracked first-seen by manipulating the poller's
	// internal map. Using the real 5-minute timeout would make the
	// test slow; this is the cheapest faithful test.
	p.mu.Lock()
	p.tracked[row.ID] = time.Now().Add(-10 * time.Minute)
	p.mu.Unlock()

	p.tickOnce(context.Background())
	got, _ = client.Build.Get(context.Background(), row.ID)
	if got.Status != BuildStatusFailed {
		t.Errorf("status = %q, want failed (provisioning stuck)", got.Status)
	}
	if !strings.Contains(got.ErrorMessage, "PROVISIONING") {
		t.Errorf("error_message = %q", got.ErrorMessage)
	}
}

func TestPoller_NoTaskARNSkipped(t *testing.T) {
	// A build promoted to running but pre-dispatch has no task ARN
	// stored. The poller must skip it (the worker is still mid-flight).
	client, fix := connectedFixture(t)
	row, err := client.Build.Create().
		SetAppID(fix.app.ID).
		SetSourceRef("main").
		SetSourceSha(strings.Repeat("a", 40)).
		SetWebhookSecret("s").
		SetCreatedBy("u").
		SetStatus(BuildStatusRunning).
		// no fargate_task_arn
		Save(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	ecsClient := &fakeECSForPoller{}
	p := newPollerForTest(t, client, ecsClient, &fakeSecretsForPoller{})
	p.tickOnce(context.Background())

	got, _ := client.Build.Get(context.Background(), row.ID)
	if got.Status != BuildStatusRunning {
		t.Errorf("status = %q (no-arn build should be skipped)", got.Status)
	}
}

func TestClusterARNFromTaskARN(t *testing.T) {
	cases := []struct {
		taskARN string
		want    string
		ok      bool
	}{
		{
			"arn:aws:ecs:us-west-2:222:task/spacefleet-builds/abcd",
			"arn:aws:ecs:us-west-2:222:cluster/spacefleet-builds",
			true,
		},
		{"arn:aws:ecs:us-west-2:222:task/abcd", "", false}, // legacy shape
		{"not-an-arn", "", false},
	}
	for _, tc := range cases {
		got, ok := clusterARNFromTaskARN(tc.taskARN)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%q -> %q, %v; want %q, %v", tc.taskARN, got, ok, tc.want, tc.ok)
		}
	}
}
