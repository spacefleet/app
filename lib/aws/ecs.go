package aws

import (
	"context"
	"errors"
	"fmt"
	"sort"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

// SessionCreds is what the build worker hands the AWS helpers after it
// assumes the customer's integration role. We accept the env-shaped map
// produced by Verifier.AssumeRoleEnv directly so the caller doesn't have
// to reach for two different shapes.
type SessionCreds struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Region          string
}

// SessionCredsFromEnv parses Verifier.AssumeRoleEnv's output back into a
// strongly-typed value. Returns a typed error when any required key is
// missing — that's a programmer mistake, not a customer mistake.
func SessionCredsFromEnv(env map[string]string) (SessionCreds, error) {
	c := SessionCreds{
		AccessKeyID:     env["AWS_ACCESS_KEY_ID"],
		SecretAccessKey: env["AWS_SECRET_ACCESS_KEY"],
		SessionToken:    env["AWS_SESSION_TOKEN"],
		Region:          env["AWS_REGION"],
	}
	if c.AccessKeyID == "" || c.SecretAccessKey == "" || c.SessionToken == "" {
		return SessionCreds{}, errors.New("aws: incomplete session creds (missing access key, secret, or session token)")
	}
	if c.Region == "" {
		return SessionCreds{}, errors.New("aws: incomplete session creds (missing region)")
	}
	return c, nil
}

// ConfigFromCreds returns an aws.Config that uses the provided
// short-lived credentials. We bypass the default credential chain
// entirely so a misconfigured worker host can't accidentally fall back
// to its own IAM role mid-call.
func ConfigFromCreds(ctx context.Context, c SessionCreds) (awssdk.Config, error) {
	if c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return awssdk.Config{}, errors.New("aws: empty access key or secret")
	}
	if c.Region == "" {
		return awssdk.Config{}, errors.New("aws: region required")
	}
	provider := credentials.NewStaticCredentialsProvider(c.AccessKeyID, c.SecretAccessKey, c.SessionToken)
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithCredentialsProvider(provider),
		awsconfig.WithRegion(c.Region),
		// Block the SDK from reading shared profiles/IMDS — the assumed
		// session is the only credential we want in play.
		awsconfig.WithSharedConfigFiles([]string{}),
		awsconfig.WithSharedCredentialsFiles([]string{}),
	)
	if err != nil {
		return awssdk.Config{}, fmt.Errorf("aws: load config: %w", err)
	}
	return cfg, nil
}

// ECSClient is the narrow surface the build worker calls into. Defining
// it here keeps callers from importing aws-sdk-go-v2/service/ecs/* and
// gives tests a small thing to fake.
type ECSClient interface {
	RunTask(ctx context.Context, params *ecs.RunTaskInput, optFns ...func(*ecs.Options)) (*ecs.RunTaskOutput, error)
	DescribeTasks(ctx context.Context, params *ecs.DescribeTasksInput, optFns ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error)
	StopTask(ctx context.Context, params *ecs.StopTaskInput, optFns ...func(*ecs.Options)) (*ecs.StopTaskOutput, error)
}

// NewECSClient builds an ECS client bound to the assumed-role session.
// Real implementation; tests substitute via the ECSClient interface.
func NewECSClient(ctx context.Context, c SessionCreds) (*ecs.Client, error) {
	cfg, err := ConfigFromCreds(ctx, c)
	if err != nil {
		return nil, err
	}
	return ecs.NewFromConfig(cfg), nil
}

// RunBuildTaskParams is the dispatcher's input. We collect everything in
// one struct rather than a long arg list so the worker's call site stays
// readable.
//
// Subnet + SecurityGroup come from the builder-infra outputs; TaskDef
// + ContainerName from app-build outputs; Env is the per-build values
// (commit SHA, webhook URL, GitHub-token secret ARN, ECR repos).
type RunBuildTaskParams struct {
	ClusterARN     string
	TaskDefinition string // task-def family or full ARN — RunTask accepts either
	ContainerName  string
	Subnet         string
	SecurityGroup  string
	Env            map[string]string

	// StartedBy is logged on the task; we use the build ID so an
	// operator can map a Fargate task back to a Spacefleet build
	// without round-tripping through the database.
	StartedBy string

	// ClientToken makes RunTask idempotent. ECS returns the original
	// task on subsequent calls with the same token + identical params.
	// The worker passes the build ID so a River retry after a partial
	// dispatch (e.g. ARN persist failed) doesn't spawn a second task.
	// Optional — omit for one-shot calls. Max 64 chars, ASCII 33-126.
	ClientToken string
}

// RunBuildTask calls ECS RunTask with FARGATE / awsvpc and the provided
// container env overrides. Returns the running task's ARN — that's what
// the build worker stores so it can DescribeTasks later.
func RunBuildTask(ctx context.Context, c ECSClient, p RunBuildTaskParams) (string, error) {
	if c == nil {
		return "", errors.New("aws: nil ECS client")
	}
	if p.ClusterARN == "" {
		return "", errors.New("aws: ClusterARN required")
	}
	if p.TaskDefinition == "" {
		return "", errors.New("aws: TaskDefinition required")
	}
	if p.ContainerName == "" {
		return "", errors.New("aws: ContainerName required")
	}
	if p.Subnet == "" || p.SecurityGroup == "" {
		return "", errors.New("aws: Subnet and SecurityGroup required")
	}

	overrides := make([]ecstypes.KeyValuePair, 0, len(p.Env))
	for _, k := range sortedKeys(p.Env) {
		overrides = append(overrides, ecstypes.KeyValuePair{
			Name:  awssdk.String(k),
			Value: awssdk.String(p.Env[k]),
		})
	}

	input := &ecs.RunTaskInput{
		Cluster:        awssdk.String(p.ClusterARN),
		TaskDefinition: awssdk.String(p.TaskDefinition),
		LaunchType:     ecstypes.LaunchTypeFargate,
		Count:          awssdk.Int32(1),
		StartedBy:      maybeStartedBy(p.StartedBy),
		NetworkConfiguration: &ecstypes.NetworkConfiguration{
			AwsvpcConfiguration: &ecstypes.AwsVpcConfiguration{
				Subnets:        []string{p.Subnet},
				SecurityGroups: []string{p.SecurityGroup},
				// The builder VPC has no NAT gateway — public IP is
				// the cheapest path to GitHub/ECR/STS.
				AssignPublicIp: ecstypes.AssignPublicIpEnabled,
			},
		},
		Overrides: &ecstypes.TaskOverride{
			ContainerOverrides: []ecstypes.ContainerOverride{
				{
					Name:        awssdk.String(p.ContainerName),
					Environment: overrides,
				},
			},
		},
	}
	if p.ClientToken != "" {
		input.ClientToken = awssdk.String(p.ClientToken)
	}

	out, err := c.RunTask(ctx, input)
	if err != nil {
		return "", fmt.Errorf("aws: ecs run task: %w", err)
	}
	if len(out.Failures) > 0 {
		f := out.Failures[0]
		return "", fmt.Errorf("aws: ecs run task: %s: %s", strDeref(f.Reason), strDeref(f.Detail))
	}
	if len(out.Tasks) == 0 {
		return "", errors.New("aws: ecs run task: no tasks and no failures returned")
	}
	if out.Tasks[0].TaskArn == nil {
		return "", errors.New("aws: ecs run task: task arn missing")
	}
	return *out.Tasks[0].TaskArn, nil
}

// TaskStatus is the worker's view of a Fargate task. We expose only the
// fields the polling loop reads — ECS's DescribeTasks shape is enormous.
type TaskStatus struct {
	LastStatus    string // PROVISIONING | PENDING | RUNNING | DEACTIVATING | STOPPING | DEPROVISIONING | STOPPED
	DesiredStatus string
	StoppedReason string
	StopCode      string
	ExitCode      *int32 // nil until the container exits
}

// IsTerminal reports whether the task has reached a final ECS state.
// Once true, the task won't transition again — the worker can record
// the result and stop polling.
func (t TaskStatus) IsTerminal() bool { return t.LastStatus == "STOPPED" }

// DescribeBuildTask fetches the current state of a single task. Returns
// a "task not found" error when ECS has never heard of the task ARN
// (typo, wrong region, expired retention).
func DescribeBuildTask(ctx context.Context, c ECSClient, clusterARN, taskARN string) (TaskStatus, error) {
	if c == nil {
		return TaskStatus{}, errors.New("aws: nil ECS client")
	}
	if clusterARN == "" || taskARN == "" {
		return TaskStatus{}, errors.New("aws: cluster and task arn required")
	}
	out, err := c.DescribeTasks(ctx, &ecs.DescribeTasksInput{
		Cluster: awssdk.String(clusterARN),
		Tasks:   []string{taskARN},
	})
	if err != nil {
		return TaskStatus{}, fmt.Errorf("aws: ecs describe tasks: %w", err)
	}
	if len(out.Failures) > 0 {
		f := out.Failures[0]
		return TaskStatus{}, fmt.Errorf("aws: ecs describe tasks: %s: %s", strDeref(f.Reason), strDeref(f.Detail))
	}
	if len(out.Tasks) == 0 {
		return TaskStatus{}, errors.New("aws: ecs describe tasks: not found")
	}
	t := out.Tasks[0]
	st := TaskStatus{
		LastStatus:    strDeref(t.LastStatus),
		DesiredStatus: strDeref(t.DesiredStatus),
		StoppedReason: strDeref(t.StoppedReason),
		StopCode:      string(t.StopCode),
	}
	for _, container := range t.Containers {
		if container.ExitCode != nil {
			code := *container.ExitCode
			st.ExitCode = &code
			break
		}
	}
	return st, nil
}

// StopBuildTask asks ECS to terminate a still-running task. Used for
// the 60-minute hard timeout. Reason is captured on the stopped task so
// an operator inspecting CloudTrail can see "this was a Spacefleet
// timeout" rather than guessing.
func StopBuildTask(ctx context.Context, c ECSClient, clusterARN, taskARN, reason string) error {
	if c == nil {
		return errors.New("aws: nil ECS client")
	}
	if clusterARN == "" || taskARN == "" {
		return errors.New("aws: cluster and task arn required")
	}
	if reason == "" {
		reason = "spacefleet: stop requested"
	}
	if _, err := c.StopTask(ctx, &ecs.StopTaskInput{
		Cluster: awssdk.String(clusterARN),
		Task:    awssdk.String(taskARN),
		Reason:  awssdk.String(reason),
	}); err != nil {
		return fmt.Errorf("aws: ecs stop task: %w", err)
	}
	return nil
}

func strDeref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func maybeStartedBy(s string) *string {
	if s == "" {
		return nil
	}
	// AWS caps StartedBy at 36 chars; truncate so a long build ID +
	// prefix doesn't 400 the RunTask call.
	const limit = 36
	if len(s) > limit {
		s = s[:limit]
	}
	return &s
}

// sortedKeys returns the map keys in deterministic order. ECS doesn't
// require sorted env vars, but tests assert on the env override array
// and a stable order keeps them readable.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
