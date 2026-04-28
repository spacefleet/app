package appbuild

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestInputsValidate(t *testing.T) {
	good := Inputs{
		OrgID:            "org",
		OrgSlug:          "acme",
		CloudAccountID:   "ca",
		AppID:            "app-uuid",
		AppSlug:          "api",
		Region:           "us-east-1",
		BuilderImage:     "ghcr.io/spacefleet/spacefleet-app/builder:v1@sha256:abc",
		ExecutionRoleARN: "arn:aws:iam::111122223333:role/spacefleet-ecs-execution",
		AWSAccountID:     "111122223333",
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("expected good inputs to validate: %v", err)
	}

	cases := []struct {
		name    string
		mutate  func(*Inputs)
		wantErr string
	}{
		{"missing org", func(in *Inputs) { in.OrgID = "" }, "OrgID required"},
		{"missing org slug", func(in *Inputs) { in.OrgSlug = "" }, "OrgSlug required"},
		{"missing cloud account", func(in *Inputs) { in.CloudAccountID = "" }, "CloudAccountID required"},
		{"missing app", func(in *Inputs) { in.AppID = "" }, "AppID required"},
		{"missing app slug", func(in *Inputs) { in.AppSlug = "" }, "AppSlug required"},
		{"missing region", func(in *Inputs) { in.Region = "" }, "Region required"},
		{"missing image", func(in *Inputs) { in.BuilderImage = "" }, "BuilderImage required"},
		{"missing exec role", func(in *Inputs) { in.ExecutionRoleARN = "" }, "ExecutionRoleARN required"},
		{"missing aws account", func(in *Inputs) { in.AWSAccountID = "" }, "AWSAccountID required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := good
			tc.mutate(&in)
			err := in.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

// TestNameHelpers locks in resource-naming conventions. The onboarding
// CFN policy scopes ECR perms to repository/spacefleet/* and other
// per-app perms to spacefleet-*; drift here would surface as opaque
// AccessDenied during a customer's first build.
func TestNameHelpers(t *testing.T) {
	app := "5f8f5d22-1c6e-4b1c-9bbe-c0fd2a9c3a31"
	org := "acme"
	slug := "api"
	if got := EcrRepoName(org, slug); got != "spacefleet/acme/api" {
		t.Errorf("EcrRepoName = %q, want spacefleet/acme/api", got)
	}
	if got := EcrCacheRepoName(org, slug); got != "spacefleet/acme/api-cache" {
		t.Errorf("EcrCacheRepoName = %q, want spacefleet/acme/api-cache", got)
	}
	if got := LogGroupName(app); got != "/spacefleet/builds/"+app {
		t.Errorf("LogGroupName = %q, want /spacefleet/builds/%s", got, app)
	}
	if got := TaskFamily(app); got != "spacefleet-build-"+app {
		t.Errorf("TaskFamily = %q, want spacefleet-build-%s", got, app)
	}
	if got := BuilderTaskRoleName(app); got != "spacefleet-builder-"+app {
		t.Errorf("BuilderTaskRoleName = %q, want spacefleet-builder-%s", got, app)
	}
	// IAM role names have a 64-char limit. UUID is 36 chars, prefix
	// "spacefleet-builder-" is 19 chars; total 55. Headroom for any
	// future longer-uuid choice.
	if got := BuilderTaskRoleName(app); len(got) > 64 {
		t.Errorf("BuilderTaskRoleName too long (%d > 64): %q", len(got), got)
	}
}

// TestContainerDefinitionsRejectsUnpinnedImage confirms the tripwire:
// a builder image without a digest pin should be rejected by the
// inline program at run time. The orchestrator validates earlier; this
// is the last line of defense.
func TestContainerDefinitionsRejectsUnpinnedImage(t *testing.T) {
	cases := []struct {
		name  string
		image string
		ok    bool
	}{
		{"with digest", "ghcr.io/spacefleet/spacefleet-app/builder:v1@sha256:" + strings.Repeat("a", 64), true},
		{"tag only", "ghcr.io/spacefleet/spacefleet-app/builder:v1", false},
		{"no tag, no digest", "ghcr.io/spacefleet/spacefleet-app/builder", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := containerDefinitionsJSON(tc.image, "us-east-1", "/spacefleet/builds/x")
			if tc.ok && err != nil {
				t.Errorf("expected success, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Error("expected rejection, got nil")
			}
		})
	}
}

// TestContainerDefinitionsShape locks in the JSON the ECS API expects.
// awslogs-group / -region / -stream-prefix are read by Fargate when
// the task starts; a typo here breaks log delivery silently.
func TestContainerDefinitionsShape(t *testing.T) {
	out, err := containerDefinitionsJSON(
		"ghcr.io/spacefleet/spacefleet-app/builder:v1@sha256:"+strings.Repeat("a", 64),
		"us-east-1",
		"/spacefleet/builds/abc",
	)
	if err != nil {
		t.Fatalf("containerDefinitionsJSON: %v", err)
	}
	var defs []map[string]any
	if err := json.Unmarshal([]byte(out), &defs); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, out)
	}
	if len(defs) != 1 {
		t.Fatalf("expected 1 container def, got %d", len(defs))
	}
	c := defs[0]
	if c["name"] != ContainerName {
		t.Errorf("name = %v, want %s", c["name"], ContainerName)
	}
	if c["essential"] != true {
		t.Error("essential must be true (only container in task)")
	}
	logCfg, ok := c["logConfiguration"].(map[string]any)
	if !ok {
		t.Fatal("logConfiguration missing or wrong shape")
	}
	if logCfg["logDriver"] != "awslogs" {
		t.Errorf("logDriver = %v, want awslogs", logCfg["logDriver"])
	}
	opts, ok := logCfg["options"].(map[string]any)
	if !ok {
		t.Fatal("logConfiguration.options missing")
	}
	for _, k := range []string{"awslogs-group", "awslogs-region", "awslogs-stream-prefix"} {
		if v, ok := opts[k]; !ok || v == "" {
			t.Errorf("option %q missing or empty", k)
		}
	}
	// Force-flush interval is the difference between "logs land in
	// CloudWatch within a second" and "logs land when the container
	// exits" — locking in the value here keeps a future drive-by edit
	// from silently regressing the live-tail UX.
	if got := opts["awslogs-force-flush-interval-seconds"]; got != "1" {
		t.Errorf("awslogs-force-flush-interval-seconds = %v, want \"1\"", got)
	}
}

// TestBuilderTaskRolePolicyShape verifies the policy generator emits
// the right resource-scoping. Wrong ARNs here surface in production
// as opaque AccessDenied errors mid-build.
func TestBuilderTaskRolePolicyShape(t *testing.T) {
	out, err := builderTaskRolePolicy(
		"app-uuid",
		"111122223333",
		"arn:aws:ecr:us-east-1:111122223333:repository/spacefleet-app-uuid",
		"arn:aws:ecr:us-east-1:111122223333:repository/spacefleet-app-uuid-cache",
		"arn:aws:logs:us-east-1:111122223333:log-group:/spacefleet/builds/app-uuid",
	)
	if err != nil {
		t.Fatalf("builderTaskRolePolicy: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, out)
	}
	statements, ok := doc["Statement"].([]any)
	if !ok {
		t.Fatal("Statement is not an array")
	}
	if len(statements) != 4 {
		t.Errorf("expected 4 statements (auth, repo, logs, secrets), got %d", len(statements))
	}
	// The policy must mention this app's secret prefix exactly once
	// — a missing scope and the role can read every secret in the
	// account.
	if !strings.Contains(out, "secret:spacefleet/builds/app-uuid/*") {
		t.Errorf("policy missing app-scoped secret resource:\n%s", out)
	}
	if !strings.Contains(out, "spacefleet-app-uuid-cache") {
		t.Errorf("policy missing cache repo ARN:\n%s", out)
	}
}

// TestOutputKeysUnique mirrors builderinfra's check.
func TestOutputKeysUnique(t *testing.T) {
	keys := []string{
		OutputECRRepoURI,
		OutputECRRepoName,
		OutputECRCacheRepoURI,
		OutputECRCacheRepoName,
		OutputTaskRoleARN,
		OutputTaskDefinitionARN,
		OutputLogGroupName,
	}
	seen := map[string]bool{}
	for _, k := range keys {
		if k == "" {
			t.Error("output key is empty")
		}
		if seen[k] {
			t.Errorf("duplicate output key %q", k)
		}
		seen[k] = true
	}
}
