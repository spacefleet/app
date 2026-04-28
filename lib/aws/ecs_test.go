package aws

import (
	"context"
	"errors"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

// fakeECS captures the inputs to RunTask/DescribeTasks/StopTask so the
// tests can assert on the assembled SDK request without spinning up a
// real ECS endpoint. Each method respects a per-call response/error
// slot so tests can preprogram the outcome.
type fakeECS struct {
	runIn       *ecs.RunTaskInput
	runOut      *ecs.RunTaskOutput
	runErr      error
	describeIn  *ecs.DescribeTasksInput
	describeOut *ecs.DescribeTasksOutput
	describeErr error
	stopIn      *ecs.StopTaskInput
	stopErr     error
}

func (f *fakeECS) RunTask(_ context.Context, in *ecs.RunTaskInput, _ ...func(*ecs.Options)) (*ecs.RunTaskOutput, error) {
	f.runIn = in
	if f.runErr != nil {
		return nil, f.runErr
	}
	return f.runOut, nil
}

func (f *fakeECS) DescribeTasks(_ context.Context, in *ecs.DescribeTasksInput, _ ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error) {
	f.describeIn = in
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	return f.describeOut, nil
}

func (f *fakeECS) StopTask(_ context.Context, in *ecs.StopTaskInput, _ ...func(*ecs.Options)) (*ecs.StopTaskOutput, error) {
	f.stopIn = in
	if f.stopErr != nil {
		return nil, f.stopErr
	}
	return &ecs.StopTaskOutput{}, nil
}

func TestSessionCredsFromEnv(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		c, err := SessionCredsFromEnv(map[string]string{
			"AWS_ACCESS_KEY_ID":     "AKIA",
			"AWS_SECRET_ACCESS_KEY": "secret",
			"AWS_SESSION_TOKEN":     "token",
			"AWS_REGION":            "us-west-2",
		})
		if err != nil {
			t.Fatal(err)
		}
		if c.Region != "us-west-2" {
			t.Errorf("region = %q", c.Region)
		}
	})
	t.Run("missing access key", func(t *testing.T) {
		_, err := SessionCredsFromEnv(map[string]string{
			"AWS_SECRET_ACCESS_KEY": "secret",
			"AWS_SESSION_TOKEN":     "token",
			"AWS_REGION":            "us-east-1",
		})
		if err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("missing region", func(t *testing.T) {
		_, err := SessionCredsFromEnv(map[string]string{
			"AWS_ACCESS_KEY_ID":     "AKIA",
			"AWS_SECRET_ACCESS_KEY": "secret",
			"AWS_SESSION_TOKEN":     "token",
		})
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestRunBuildTask_HappyPath(t *testing.T) {
	c := &fakeECS{
		runOut: &ecs.RunTaskOutput{
			Tasks: []ecstypes.Task{{TaskArn: awssdk.String("arn:aws:ecs:us-west-2:111:task/abc")}},
		},
	}
	arn, err := RunBuildTask(context.Background(), c, RunBuildTaskParams{
		ClusterARN:     "arn:aws:ecs:::cluster/x",
		TaskDefinition: "spacefleet-build-app1",
		ContainerName:  "builder",
		Subnet:         "subnet-1",
		SecurityGroup:  "sg-1",
		Env:            map[string]string{"FOO": "bar", "AAA": "1"},
		StartedBy:      "spacefleet-build-very-long-very-long-very-long",
	})
	if err != nil {
		t.Fatalf("RunBuildTask: %v", err)
	}
	if arn != "arn:aws:ecs:us-west-2:111:task/abc" {
		t.Errorf("task arn = %q", arn)
	}
	if c.runIn == nil {
		t.Fatal("RunTask not called")
	}
	if c.runIn.LaunchType != ecstypes.LaunchTypeFargate {
		t.Error("expected FARGATE launch type")
	}
	netCfg := c.runIn.NetworkConfiguration.AwsvpcConfiguration
	if netCfg.AssignPublicIp != ecstypes.AssignPublicIpEnabled {
		t.Error("expected AssignPublicIp ENABLED — builder VPC has no NAT")
	}
	if got := netCfg.Subnets; len(got) != 1 || got[0] != "subnet-1" {
		t.Errorf("subnets = %v", got)
	}
	if c.runIn.StartedBy != nil && len(*c.runIn.StartedBy) > 36 {
		t.Errorf("StartedBy not truncated: len=%d", len(*c.runIn.StartedBy))
	}

	// Env overrides should be sorted by key for stable assertions.
	overrides := c.runIn.Overrides.ContainerOverrides[0].Environment
	if len(overrides) != 2 {
		t.Fatalf("overrides len = %d", len(overrides))
	}
	if *overrides[0].Name != "AAA" || *overrides[1].Name != "FOO" {
		t.Errorf("env overrides not sorted: %v %v", *overrides[0].Name, *overrides[1].Name)
	}
}

func TestRunBuildTask_FailureSurface(t *testing.T) {
	t.Run("aws error", func(t *testing.T) {
		c := &fakeECS{runErr: errors.New("boom")}
		if _, err := RunBuildTask(context.Background(), c, validRunParams()); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("Failures slice populated", func(t *testing.T) {
		c := &fakeECS{
			runOut: &ecs.RunTaskOutput{
				Failures: []ecstypes.Failure{{
					Reason: awssdk.String("RESOURCE:CPU"),
					Detail: awssdk.String("no capacity"),
				}},
			},
		}
		_, err := RunBuildTask(context.Background(), c, validRunParams())
		if err == nil || !contains(err.Error(), "RESOURCE:CPU") {
			t.Fatalf("err = %v, want RESOURCE:CPU", err)
		}
	})
	t.Run("missing required fields", func(t *testing.T) {
		_, err := RunBuildTask(context.Background(), &fakeECS{}, RunBuildTaskParams{})
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestDescribeBuildTask(t *testing.T) {
	t.Run("running", func(t *testing.T) {
		c := &fakeECS{
			describeOut: &ecs.DescribeTasksOutput{
				Tasks: []ecstypes.Task{{
					LastStatus:    awssdk.String("RUNNING"),
					DesiredStatus: awssdk.String("RUNNING"),
				}},
			},
		}
		st, err := DescribeBuildTask(context.Background(), c, "cluster", "task")
		if err != nil {
			t.Fatal(err)
		}
		if st.LastStatus != "RUNNING" || st.IsTerminal() {
			t.Errorf("unexpected st: %+v", st)
		}
	})
	t.Run("stopped with exit code", func(t *testing.T) {
		var code int32 = 137
		c := &fakeECS{
			describeOut: &ecs.DescribeTasksOutput{
				Tasks: []ecstypes.Task{{
					LastStatus:    awssdk.String("STOPPED"),
					StoppedReason: awssdk.String("Essential container exited"),
					StopCode:      ecstypes.TaskStopCodeEssentialContainerExited,
					Containers: []ecstypes.Container{{
						ExitCode: &code,
					}},
				}},
			},
		}
		st, err := DescribeBuildTask(context.Background(), c, "cluster", "task")
		if err != nil {
			t.Fatal(err)
		}
		if !st.IsTerminal() {
			t.Error("expected terminal")
		}
		if st.ExitCode == nil || *st.ExitCode != 137 {
			t.Errorf("exit code = %v", st.ExitCode)
		}
		if st.StopCode == "" {
			t.Errorf("stop code missing")
		}
	})
	t.Run("not found", func(t *testing.T) {
		c := &fakeECS{describeOut: &ecs.DescribeTasksOutput{}}
		_, err := DescribeBuildTask(context.Background(), c, "cluster", "task")
		if err == nil {
			t.Error("expected not-found error")
		}
	})
}

func TestStopBuildTask(t *testing.T) {
	c := &fakeECS{}
	if err := StopBuildTask(context.Background(), c, "cluster", "task", "timeout"); err != nil {
		t.Fatal(err)
	}
	if c.stopIn == nil || *c.stopIn.Reason != "timeout" {
		t.Errorf("stop input = %+v", c.stopIn)
	}
}

func validRunParams() RunBuildTaskParams {
	return RunBuildTaskParams{
		ClusterARN:     "arn:aws:ecs:::cluster/x",
		TaskDefinition: "spacefleet-build-x",
		ContainerName:  "builder",
		Subnet:         "subnet-1",
		SecurityGroup:  "sg-1",
		Env:            map[string]string{"K": "v"},
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
