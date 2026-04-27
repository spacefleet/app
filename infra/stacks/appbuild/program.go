// Package appbuild is the inline Pulumi program for the per-app
// "app-build" stack. One instance per Spacefleet app: ECR repo + cache
// repo + builder task role + per-app log group + ECS task definition
// (Fargate, awsvpc, single Kaniko container).
//
// All inputs the program needs from the per-cloud-account
// "builder-infra" stack — cluster ARN, subnet, SG, execution role —
// arrive via [Inputs]. The orchestrator (lib/pulumi.Orchestrator) reads
// builder-infra outputs after Up and threads them in here. We avoid
// pulumi.StackReference because the two stacks live under different s3
// backend prefixes; passing values through Inputs is simpler than
// re-shaping the backend layout.
//
// The task definition is a fixed shape per app. Per-build differences
// (commit SHA, webhook URL, GitHub-token secret ARN, etc.) are passed
// at RunTask time via containerOverrides.environment, never by
// re-registering task defs. This keeps ECS clean — one TaskDefinition
// revision per app, not per build.
package appbuild

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/cloudwatch"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/ecr"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/ecs"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/iam"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const (
	// LogGroupRetentionDays is the per-app log retention. 30 is a
	// pragmatic middle ground — long enough to debug a failure
	// reported a few weeks later, short enough not to balloon CW Logs
	// storage costs.
	LogGroupRetentionDays = 30

	// TaskCPU / TaskMemory are Fargate sizing for the builder. Kaniko
	// is memory-hungry for layer-heavy images; 2 vCPU / 4 GiB is the
	// minimum we trust. Bumped at the cost of customer's bill.
	TaskCPU    = "2048"
	TaskMemory = "4096"

	ContainerName = "builder"
)

// OutputKeys are the names under which this program publishes outputs.
// Same pattern as builderinfra: constants live alongside the program so
// the orchestrator and tests share one source of truth.
const (
	OutputECRRepoURI        = "ecrRepoUri"
	OutputECRRepoName       = "ecrRepoName"
	OutputECRCacheRepoURI   = "ecrCacheRepoUri"
	OutputECRCacheRepoName  = "ecrCacheRepoName"
	OutputTaskRoleARN       = "taskRoleArn"
	OutputTaskDefinitionARN = "taskDefinitionArn"
	OutputLogGroupName      = "logGroupName"
)

// Inputs are everything the program needs. The cluster/subnet/SG/exec
// role come from builder-infra; the builder image is the
// digest-pinned reference baked into the binary at release.
type Inputs struct {
	OrgID          string
	CloudAccountID string
	AppID          string
	Region         string

	// BuilderImage is the digest-pinned image reference Fargate pulls.
	// Format: ghcr.io/spacefleet/spacefleet-app/builder:<tag>@sha256:...
	// We don't validate the @sha256 suffix here — the orchestrator
	// does, and a missing pin is a config-time failure, not a Pulumi
	// failure.
	BuilderImage string

	// ExecutionRoleARN is from builder-infra. Shared across all per-app
	// task definitions in this account.
	ExecutionRoleARN string

	// AWSAccountID is the 12-digit account the role policies' resource
	// ARNs scope to. We could leave it as `*` but a tight ARN gives
	// the customer a clear "what does this role have access to in my
	// account" answer.
	AWSAccountID string
}

// Validate checks every required field. Same defensive-recheck role as
// builderinfra.Inputs.Validate: the orchestrator validates earlier but
// we don't trust callers from tests / the dev CLI.
func (in Inputs) Validate() error {
	if in.OrgID == "" {
		return errors.New("appbuild: OrgID required")
	}
	if in.CloudAccountID == "" {
		return errors.New("appbuild: CloudAccountID required")
	}
	if in.AppID == "" {
		return errors.New("appbuild: AppID required")
	}
	if in.Region == "" {
		return errors.New("appbuild: Region required")
	}
	if in.BuilderImage == "" {
		return errors.New("appbuild: BuilderImage required")
	}
	if in.ExecutionRoleARN == "" {
		return errors.New("appbuild: ExecutionRoleARN required (from builder-infra outputs)")
	}
	if in.AWSAccountID == "" {
		return errors.New("appbuild: AWSAccountID required")
	}
	return nil
}

// EcrRepoName returns the per-app ECR repo name. Public — the
// orchestrator and the dispatcher (phase 5) need to compute this for
// containerOverrides without round-tripping through Pulumi outputs.
func EcrRepoName(appID string) string {
	return "spacefleet-" + appID
}

// EcrCacheRepoName returns the per-app cache repo name (same prefix +
// "-cache"). Kaniko writes layer cache here; we keep it in a separate
// repo rather than mixing tags so a `force_delete` of either doesn't
// surprise the operator.
func EcrCacheRepoName(appID string) string {
	return "spacefleet-" + appID + "-cache"
}

// LogGroupName returns the per-app CloudWatch log group. Path matches
// the namespace builder-infra established (`/spacefleet/builds`) so a
// single IAM `Resource: ".../<namespace>/*"` covers every app.
func LogGroupName(appID string) string {
	return "/spacefleet/builds/" + appID
}

// TaskFamily returns the ECS TaskDefinition family. Family +
// auto-incrementing revision is how ECS dedups; we always reference
// the latest revision via the exported ARN.
func TaskFamily(appID string) string {
	return "spacefleet-build-" + appID
}

// BuilderTaskRoleName returns the IAM role name for this app's builder
// task. AWS allows IAM role names up to 64 chars; "spacefleet-builder-"
// + a UUID (36 chars) = 55, fits comfortably.
func BuilderTaskRoleName(appID string) string {
	return "spacefleet-builder-" + appID
}

// Program returns the inline Pulumi program for app-build. Reads
// nothing from Pulumi config; everything comes from [Inputs] via
// closure.
func Program(in Inputs) func(*pulumi.Context) error {
	return func(ctx *pulumi.Context) error {
		if err := in.Validate(); err != nil {
			return err
		}
		tags := commonTags(in)

		repo, err := ecr.NewRepository(ctx, "ecr-repo", &ecr.RepositoryArgs{
			Name:               pulumi.String(EcrRepoName(in.AppID)),
			ImageTagMutability: pulumi.String("MUTABLE"),
			ForceDelete:        pulumi.Bool(true),
			Tags:               tags,
		})
		if err != nil {
			return fmt.Errorf("ecr repo: %w", err)
		}

		cacheRepo, err := ecr.NewRepository(ctx, "ecr-cache-repo", &ecr.RepositoryArgs{
			Name:               pulumi.String(EcrCacheRepoName(in.AppID)),
			ImageTagMutability: pulumi.String("MUTABLE"),
			ForceDelete:        pulumi.Bool(true),
			Tags:               tags,
		})
		if err != nil {
			return fmt.Errorf("ecr cache repo: %w", err)
		}

		logGroup, err := cloudwatch.NewLogGroup(ctx, "log-group", &cloudwatch.LogGroupArgs{
			Name:            pulumi.String(LogGroupName(in.AppID)),
			RetentionInDays: pulumi.Int(LogGroupRetentionDays),
			Tags:            tags,
		})
		if err != nil {
			return fmt.Errorf("log group: %w", err)
		}

		taskRole, err := iam.NewRole(ctx, "builder-task-role", &iam.RoleArgs{
			Name:             pulumi.String(BuilderTaskRoleName(in.AppID)),
			AssumeRolePolicy: pulumi.String(ecsTasksAssumeRolePolicy),
			Tags:             tags,
		})
		if err != nil {
			return fmt.Errorf("task role: %w", err)
		}

		taskPolicy := pulumi.All(repo.Arn, cacheRepo.Arn, logGroup.Arn).ApplyT(func(args []any) (string, error) {
			repoArn, _ := args[0].(string)
			cacheArn, _ := args[1].(string)
			logArn, _ := args[2].(string)
			return builderTaskRolePolicy(in.AppID, in.AWSAccountID, repoArn, cacheArn, logArn)
		}).(pulumi.StringOutput)

		if _, err := iam.NewRolePolicy(ctx, "builder-task-policy", &iam.RolePolicyArgs{
			Role:   taskRole.Name,
			Name:   pulumi.String("spacefleet-builder-policy"),
			Policy: taskPolicy,
		}); err != nil {
			return fmt.Errorf("task policy: %w", err)
		}

		containerDefs := pulumi.All(in.BuilderImage, in.Region, logGroup.Name).ApplyT(func(args []any) (string, error) {
			image, _ := args[0].(string)
			region, _ := args[1].(string)
			lgName, _ := args[2].(string)
			return containerDefinitionsJSON(image, region, lgName)
		}).(pulumi.StringOutput)

		taskDef, err := ecs.NewTaskDefinition(ctx, "builder-task-def", &ecs.TaskDefinitionArgs{
			Family:                  pulumi.String(TaskFamily(in.AppID)),
			RequiresCompatibilities: pulumi.StringArray{pulumi.String("FARGATE")},
			NetworkMode:             pulumi.String("awsvpc"),
			Cpu:                     pulumi.String(TaskCPU),
			Memory:                  pulumi.String(TaskMemory),
			ExecutionRoleArn:        pulumi.String(in.ExecutionRoleARN),
			TaskRoleArn:             taskRole.Arn,
			ContainerDefinitions:    containerDefs,
			Tags:                    tags,
		})
		if err != nil {
			return fmt.Errorf("task definition: %w", err)
		}

		ctx.Export(OutputECRRepoURI, repo.RepositoryUrl)
		ctx.Export(OutputECRRepoName, repo.Name)
		ctx.Export(OutputECRCacheRepoURI, cacheRepo.RepositoryUrl)
		ctx.Export(OutputECRCacheRepoName, cacheRepo.Name)
		ctx.Export(OutputTaskRoleARN, taskRole.Arn)
		ctx.Export(OutputTaskDefinitionARN, taskDef.Arn)
		ctx.Export(OutputLogGroupName, logGroup.Name)
		return nil
	}
}

// commonTags mirrors builderinfra.commonTags but with stack=app-build
// and the app id as a tag. An operator listing resources by app
// shouldn't have to know the cluster's role policies.
func commonTags(in Inputs) pulumi.StringMap {
	return pulumi.StringMap{
		"spacefleet:managed":       pulumi.String("true"),
		"spacefleet:org":           pulumi.String(in.OrgID),
		"spacefleet:cloud_account": pulumi.String(in.CloudAccountID),
		"spacefleet:app":           pulumi.String(in.AppID),
		"spacefleet:stack":         pulumi.String("app-build"),
	}
}

// builderTaskRolePolicy returns the policy JSON the per-app builder
// task assumes. Scoped tightly to:
//   - this app's ECR repos (push, pull, layer ops)
//   - this app's CW log group + the global GetAuthorizationToken (no
//     resource scoping; AWS API limitation)
//   - secrets under spacefleet/builds/<app-id>/* (the per-build GitHub
//     token secret pattern from BUILD_PIPELINE.md)
func builderTaskRolePolicy(appID, awsAccount, repoARN, cacheARN, logGroupARN string) (string, error) {
	doc := map[string]any{
		"Version": "2012-10-17",
		"Statement": []any{
			map[string]any{
				"Sid":      "EcrAuthGlobal",
				"Effect":   "Allow",
				"Action":   []string{"ecr:GetAuthorizationToken"},
				"Resource": "*",
			},
			map[string]any{
				"Sid":    "EcrRepoOps",
				"Effect": "Allow",
				"Action": []string{
					"ecr:BatchCheckLayerAvailability",
					"ecr:PutImage",
					"ecr:UploadLayerPart",
					"ecr:InitiateLayerUpload",
					"ecr:CompleteLayerUpload",
					"ecr:BatchGetImage",
					"ecr:GetDownloadUrlForLayer",
				},
				"Resource": []string{repoARN, cacheARN},
			},
			map[string]any{
				"Sid":    "WriteOwnLogs",
				"Effect": "Allow",
				"Action": []string{
					"logs:CreateLogStream",
					"logs:PutLogEvents",
				},
				"Resource": []string{
					logGroupARN,
					logGroupARN + ":*",
				},
			},
			map[string]any{
				"Sid":    "ReadOwnSecrets",
				"Effect": "Allow",
				"Action": []string{"secretsmanager:GetSecretValue"},
				"Resource": fmt.Sprintf(
					"arn:aws:secretsmanager:*:%s:secret:spacefleet/builds/%s/*",
					awsAccount, appID,
				),
			},
		},
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// containerDefinitionsJSON builds the ECS-format container-definitions
// JSON for the single builder container. We don't bake env vars in
// here — every per-build value (commit SHA, webhook URL, etc.) is
// passed via RunTask containerOverrides.environment at dispatch time.
func containerDefinitionsJSON(image, region, logGroupName string) (string, error) {
	if !strings.Contains(image, "@sha256:") {
		// Bubble up — calling Pulumi with an un-pinned image is a
		// config-time bug. The orchestrator's validation should have
		// caught it; this is the tripwire.
		return "", fmt.Errorf("appbuild: builder image must be digest-pinned (got %q)", image)
	}
	def := []map[string]any{
		{
			"name":      ContainerName,
			"image":     image,
			"essential": true,
			"logConfiguration": map[string]any{
				"logDriver": "awslogs",
				"options": map[string]string{
					"awslogs-group":         logGroupName,
					"awslogs-region":        region,
					"awslogs-stream-prefix": "builder",
				},
			},
		},
	}
	out, err := json.Marshal(def)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ecsTasksAssumeRolePolicy is duplicated from builderinfra.go on
// purpose — these packages don't import each other. The string is
// short and keeping them in lockstep is cheap; a future refactor can
// extract to a shared `infra/iampolicies` package if a third role joins
// the party.
const ecsTasksAssumeRolePolicy = `{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": { "Service": "ecs-tasks.amazonaws.com" },
      "Action": "sts:AssumeRole"
    }
  ]
}`
