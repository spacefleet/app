// Package builderinfra is the inline Pulumi program for the
// per-cloud-account "builder-infra" stack. One instance per connected
// AWS account: VPC + public subnet + IGW + ECS cluster + execution
// role + log-group prefix.
//
// Sizing is deliberately minimal — no NAT gateway, single AZ, single
// public subnet. Build tasks get assignPublicIp=ENABLED for outbound
// reachability (GitHub, ECR Public, AWS APIs). No inbound is allowed
// regardless. The public-IP charge is on the order of $0.005 per task,
// negligible at build durations; a NAT gateway would add ~$32/mo idle.
//
// The program is intentionally a closure over [Inputs]: the orchestrator
// builds Inputs (from a CloudAccount row + config), passes them into
// [Program], and the resulting `func(*pulumi.Context) error` is what the
// Pulumi auto-API invokes. There is no Pulumi config or stack-config
// indirection — everything the program needs is in the closure.
//
// Outputs are exported via ctx.Export and read back by the orchestrator
// after Up; the per-app "app-build" stack receives them via its own
// Inputs.
package builderinfra

import (
	"errors"
	"fmt"
	"maps"

	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/ec2"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/ecs"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/iam"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// CIDRBlock is the IPv4 range the builder VPC owns. /16 is far more
// than we need for a single public subnet, but it leaves room for the
// future "default-vpc" (runtime apps) without overlapping. We never
// peer; this is just to keep the customer's CIDR plan tidy.
const (
	CIDRBlock          = "10.100.0.0/16"
	BuilderSubnetCIDR  = "10.100.0.0/24"
	BuilderClusterName = "spacefleet-builds"
	ExecutionRoleName  = "spacefleet-ecs-execution"
	LogGroupNamespace  = "/spacefleet/builds"
)

// OutputKeys are the names under which the program publishes outputs
// via ctx.Export. The orchestrator reads back by these names so the
// constants live in one place — drift between program and consumer is
// caught at compile time when this file is the source of truth.
const (
	OutputClusterARN       = "clusterArn"
	OutputClusterName      = "clusterName"
	OutputVpcID            = "vpcId"
	OutputSubnetID         = "subnetId"
	OutputSecurityGroupID  = "securityGroupId"
	OutputExecutionRoleARN = "executionRoleArn"
	OutputLogGroupPrefix   = "logGroupPrefix"
)

// Inputs are everything the program needs that isn't a Pulumi-managed
// constant. The orchestrator constructs Inputs from a [CloudAccount]
// row + the install-wide config and passes the result to [Program].
type Inputs struct {
	// OrgID is the Spacefleet org that owns this builder infra. Used
	// only as a tag value — no resource name embeds it (per-cloud-account
	// resources are scoped by the role/permissions, not by name).
	OrgID string

	// CloudAccountID is the Spacefleet UUID for the connected AWS
	// account. Used as a tag value. The 12-digit AWS account ID is
	// known by the provider via assumed-role creds, not encoded in
	// resource names.
	CloudAccountID string

	// Region is the AWS region the VPC + cluster live in. Pulumi's
	// default provider picks it up from AWS_REGION; we still pass it
	// here so the program can fail closed if the env was forgotten.
	Region string
}

// Validate returns the first input field that's missing. The
// orchestrator validates earlier in the chain; this is a defensive
// re-check so a misuse of [Program] from a test doesn't manifest as a
// confusing AWS-side error half a minute into Up.
func (in Inputs) Validate() error {
	if in.OrgID == "" {
		return errors.New("builderinfra: OrgID required")
	}
	if in.CloudAccountID == "" {
		return errors.New("builderinfra: CloudAccountID required")
	}
	if in.Region == "" {
		return errors.New("builderinfra: Region required")
	}
	return nil
}

// Program returns the inline Pulumi program for builder-infra. The
// orchestrator hands this to [pulumi.NewRunner] which drives Pulumi's
// auto API.
//
// All resources are tagged with `spacefleet:managed=true`, the org id,
// and the cloud-account id. The `spacefleet:managed` tag is the
// affordance an operator uses to find what we created; we never key
// behavior off it ourselves.
func Program(in Inputs) func(*pulumi.Context) error {
	return func(ctx *pulumi.Context) error {
		if err := in.Validate(); err != nil {
			return err
		}
		tags := commonTags(in)

		vpc, err := ec2.NewVpc(ctx, "builder-vpc", &ec2.VpcArgs{
			CidrBlock:          pulumi.String(CIDRBlock),
			EnableDnsHostnames: pulumi.Bool(true),
			EnableDnsSupport:   pulumi.Bool(true),
			Tags:               nameTag(tags, "spacefleet-builder-vpc"),
		})
		if err != nil {
			return fmt.Errorf("vpc: %w", err)
		}

		igw, err := ec2.NewInternetGateway(ctx, "builder-igw", &ec2.InternetGatewayArgs{
			VpcId: vpc.ID(),
			Tags:  nameTag(tags, "spacefleet-builder-igw"),
		})
		if err != nil {
			return fmt.Errorf("igw: %w", err)
		}

		// Single public subnet. No AZ pinning — we let the AWS provider
		// pick the first AZ in the region. If we ever need
		// AZ-resilience for builds we'd add a second subnet and run
		// RunTask with both subnet IDs.
		subnet, err := ec2.NewSubnet(ctx, "builder-subnet", &ec2.SubnetArgs{
			VpcId:               vpc.ID(),
			CidrBlock:           pulumi.String(BuilderSubnetCIDR),
			MapPublicIpOnLaunch: pulumi.Bool(true),
			Tags:                nameTag(tags, "spacefleet-builder-subnet"),
		})
		if err != nil {
			return fmt.Errorf("subnet: %w", err)
		}

		routeTable, err := ec2.NewRouteTable(ctx, "builder-rt", &ec2.RouteTableArgs{
			VpcId: vpc.ID(),
			Tags:  nameTag(tags, "spacefleet-builder-rt"),
		})
		if err != nil {
			return fmt.Errorf("route table: %w", err)
		}

		if _, err := ec2.NewRoute(ctx, "builder-default-route", &ec2.RouteArgs{
			RouteTableId:         routeTable.ID(),
			DestinationCidrBlock: pulumi.String("0.0.0.0/0"),
			GatewayId:            igw.ID(),
		}); err != nil {
			return fmt.Errorf("default route: %w", err)
		}

		if _, err := ec2.NewRouteTableAssociation(ctx, "builder-rta", &ec2.RouteTableAssociationArgs{
			SubnetId:     subnet.ID(),
			RouteTableId: routeTable.ID(),
		}); err != nil {
			return fmt.Errorf("route table assoc: %w", err)
		}

		// Egress-only security group: outbound any, no inbound. The
		// public IP on each task is reachable in principle but the SG
		// blocks inbound — defense in depth, since we don't actually
		// expect inbound but the public-IP allocation is a side effect
		// of needing outbound without a NAT gateway.
		sg, err := ec2.NewSecurityGroup(ctx, "builder-sg", &ec2.SecurityGroupArgs{
			Name:        pulumi.String("spacefleet-builder-sg"),
			Description: pulumi.String("Spacefleet builder tasks: outbound only"),
			VpcId:       vpc.ID(),
			Egress: ec2.SecurityGroupEgressArray{
				&ec2.SecurityGroupEgressArgs{
					Protocol:   pulumi.String("-1"),
					FromPort:   pulumi.Int(0),
					ToPort:     pulumi.Int(0),
					CidrBlocks: pulumi.StringArray{pulumi.String("0.0.0.0/0")},
				},
			},
			Tags: nameTag(tags, "spacefleet-builder-sg"),
		})
		if err != nil {
			return fmt.Errorf("security group: %w", err)
		}

		cluster, err := ecs.NewCluster(ctx, "builder-cluster", &ecs.ClusterArgs{
			Name: pulumi.String(BuilderClusterName),
			Tags: tags,
		})
		if err != nil {
			return fmt.Errorf("cluster: %w", err)
		}

		// The execution role is shared across all per-app task
		// definitions in this account. It only needs:
		//   - logs:CreateLogStream / PutLogEvents on the namespace
		//   - secretsmanager:GetSecretValue on per-build secrets
		//
		// The builder image lives on public GHCR (no auth) for v1, so
		// no ECR pull permission and no registry-auth secret. Document
		// in BUILD_PIPELINE.md if that ever changes.
		execRole, err := iam.NewRole(ctx, "builder-exec-role", &iam.RoleArgs{
			Name:             pulumi.String(ExecutionRoleName),
			AssumeRolePolicy: pulumi.String(ecsTasksAssumeRolePolicy),
			Tags:             tags,
		})
		if err != nil {
			return fmt.Errorf("execution role: %w", err)
		}

		execPolicy := pulumi.Sprintf(executionRolePolicyTemplate, LogGroupNamespace)
		if _, err := iam.NewRolePolicy(ctx, "builder-exec-policy", &iam.RolePolicyArgs{
			Role:   execRole.Name,
			Name:   pulumi.String("spacefleet-ecs-execution-policy"),
			Policy: execPolicy,
		}); err != nil {
			return fmt.Errorf("execution role policy: %w", err)
		}

		// Per-app log groups are created by the app-build stack. Here
		// we only export the shared namespace as a string so the
		// orchestrator (and a curious operator) sees one consistent
		// prefix for everything the build pipeline writes.

		ctx.Export(OutputVpcID, vpc.ID())
		ctx.Export(OutputSubnetID, subnet.ID())
		ctx.Export(OutputSecurityGroupID, sg.ID())
		ctx.Export(OutputClusterARN, cluster.Arn)
		ctx.Export(OutputClusterName, cluster.Name)
		ctx.Export(OutputExecutionRoleARN, execRole.Arn)
		ctx.Export(OutputLogGroupPrefix, pulumi.String(LogGroupNamespace))

		return nil
	}
}

// commonTags is the tag set every builder-infra resource carries. The
// `spacefleet:managed` tag is what we tell operators to grep for if
// they want to find / clean up Spacefleet-created resources.
func commonTags(in Inputs) pulumi.StringMap {
	return pulumi.StringMap{
		"spacefleet:managed":       pulumi.String("true"),
		"spacefleet:org":           pulumi.String(in.OrgID),
		"spacefleet:cloud_account": pulumi.String(in.CloudAccountID),
		"spacefleet:stack":         pulumi.String("builder-infra"),
	}
}

// nameTag clones a tag map and adds Name=<name>. We don't mutate the
// caller's map because Pulumi's StringMap is a typed map of pulumi
// inputs and reusing it across resources can confuse the engine's
// dependency tracking.
func nameTag(base pulumi.StringMap, name string) pulumi.StringMap {
	out := pulumi.StringMap{}
	maps.Copy(out, base)
	out["Name"] = pulumi.String(name)
	return out
}

// ecsTasksAssumeRolePolicy is the trust policy every ECS task role
// uses. The principal has to be ecs-tasks.amazonaws.com — task roles
// are different from service-linked roles in that respect.
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

// executionRolePolicyTemplate is rendered with %s = log group namespace
// (e.g., "/spacefleet/builds"). The Resource ARN restricts log writes
// to the namespace; secret access is wildcarded under
// `arn:aws:secretsmanager:*:*:secret:spacefleet/builds/*` because the
// per-build secret name embeds the app id at dispatch time and we
// don't know it here.
//
// Note: CreateLogGroup is intentionally absent — the per-app log group
// is provisioned by the app-build stack (with retention set), not at
// task launch.
const executionRolePolicyTemplate = `{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "WriteBuildLogs",
      "Effect": "Allow",
      "Action": [
        "logs:CreateLogStream",
        "logs:PutLogEvents"
      ],
      "Resource": "arn:aws:logs:*:*:log-group:%s/*"
    },
    {
      "Sid": "ReadBuildSecrets",
      "Effect": "Allow",
      "Action": ["secretsmanager:GetSecretValue"],
      "Resource": "arn:aws:secretsmanager:*:*:secret:spacefleet/builds/*"
    }
  ]
}`
