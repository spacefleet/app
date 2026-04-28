package pulumi

import (
	"context"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
)

var goodBackend = BackendConfig{
	Bucket:    "spacefleet-state",
	Region:    "us-east-1",
	KMSKeyARN: "arn:aws:kms:us-east-1:111122223333:key/abcd",
}

func TestNewOrchestratorValidates(t *testing.T) {
	cases := []struct {
		name    string
		backend BackendConfig
		image   string
		wantErr string
	}{
		{
			"valid",
			goodBackend,
			"ghcr.io/spacefleet/spacefleet-app/builder:v1@sha256:abc",
			"",
		},
		{
			"bad backend",
			BackendConfig{Bucket: "b"},
			"img@sha256:x",
			"region",
		},
		{
			"empty image",
			goodBackend,
			"",
			"builder image required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewOrchestrator(tc.backend, tc.image)
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("expected no error, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestAccountTargetValidate(t *testing.T) {
	good := AccountTarget{
		OrgID:          "org",
		CloudAccountID: "ca",
		AWSAccountID:   "111122223333",
		RoleARN:        "arn:aws:iam::111122223333:role/Spacefleet",
		ExternalID:     "abc123",
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("good target should validate: %v", err)
	}

	cases := []struct {
		name    string
		mutate  func(*AccountTarget)
		wantErr string
	}{
		{"empty org", func(t *AccountTarget) { t.OrgID = "" }, "OrgID"},
		{"empty cloud account", func(t *AccountTarget) { t.CloudAccountID = "" }, "CloudAccountID"},
		{"empty aws account", func(t *AccountTarget) { t.AWSAccountID = "" }, "AWSAccountID"},
		{"empty role arn", func(t *AccountTarget) { t.RoleARN = "" }, "RoleARN"},
		{"empty external id", func(t *AccountTarget) { t.ExternalID = "" }, "ExternalID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := good
			tc.mutate(&target)
			err := target.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestAccountTargetResolvedRegion(t *testing.T) {
	t1 := AccountTarget{Region: "eu-west-1"}
	if got := t1.resolvedRegion(); got != "eu-west-1" {
		t.Errorf("resolvedRegion = %q, want eu-west-1", got)
	}
	t2 := AccountTarget{}
	if got := t2.resolvedRegion(); got != "us-east-1" {
		t.Errorf("resolvedRegion fallback = %q, want us-east-1", got)
	}
}

func TestSessionNameClampsToSTSLimit(t *testing.T) {
	// STS RoleSessionName has a 64-char limit. CloudAccountID is a
	// uuid (36 chars), prefix "spacefleet-" is 11; nominal length 47
	// — under the limit. We still test the clamp because future
	// renames could push us over.
	target := AccountTarget{CloudAccountID: strings.Repeat("x", 80)}
	got := sessionName(target)
	if len(got) > 64 {
		t.Errorf("sessionName too long: %d, %q", len(got), got)
	}
	if !strings.HasPrefix(got, "spacefleet-") {
		t.Errorf("sessionName missing prefix: %q", got)
	}
}

// TestReadOutputErrors exercises the missing-output and wrong-type
// branches. Keeps the orchestrator's failure modes specific instead
// of "Pulumi did something weird."
func TestReadOutputErrors(t *testing.T) {
	outputs := auto.OutputMap{
		"present-string":  {Value: "hello"},
		"present-non-str": {Value: 42},
	}
	var dst string

	if err := readOutput(outputs, "present-string", &dst); err != nil {
		t.Errorf("expected success on present string, got %v", err)
	}
	if dst != "hello" {
		t.Errorf("dst = %q, want hello", dst)
	}

	err := readOutput(outputs, "missing", &dst)
	if err == nil || !strings.Contains(err.Error(), "missing stack output") {
		t.Errorf("missing key err = %v", err)
	}

	err = readOutput(outputs, "present-non-str", &dst)
	if err == nil || !strings.Contains(err.Error(), "not a string") {
		t.Errorf("non-string err = %v", err)
	}
}

// TestUpAppBuildRequiresApp confirms the orchestrator catches the
// caller-side mistake of dropping app id or slug; we shouldn't fall
// through to running builder-infra successfully and then fail with
// something opaque on app-build.
func TestUpAppBuildRequiresApp(t *testing.T) {
	o, err := NewOrchestrator(goodBackend, "img@sha256:x")
	if err != nil {
		t.Fatalf("NewOrchestrator: %v", err)
	}
	target := AccountTarget{
		OrgID:          "org",
		CloudAccountID: "ca",
		AWSAccountID:   "111122223333",
		RoleARN:        "arn:aws:iam::111122223333:role/Spacefleet",
		ExternalID:     "abc123",
	}
	cases := []struct {
		name string
		app  AppRef
		want string
	}{
		{"empty", AppRef{}, "app.ID required"},
		{"no slug", AppRef{ID: "app-uuid"}, "app.Slug required"},
		{"no id", AppRef{Slug: "api"}, "app.ID required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := o.UpAppBuild(context.Background(), target, tc.app, RunOpts{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want substring %q", err, tc.want)
			}
		})
	}
}

// TestDestroyAppBuildRequiresAppID is the destroy-side mirror.
func TestDestroyAppBuildRequiresAppID(t *testing.T) {
	o, err := NewOrchestrator(goodBackend, "img@sha256:x")
	if err != nil {
		t.Fatalf("NewOrchestrator: %v", err)
	}
	target := AccountTarget{
		OrgID:          "org",
		CloudAccountID: "ca",
		AWSAccountID:   "111122223333",
		RoleARN:        "arn:aws:iam::111122223333:role/Spacefleet",
		ExternalID:     "abc123",
	}
	err = o.DestroyAppBuild(context.Background(), target, "", RunOpts{})
	if err == nil || !strings.Contains(err.Error(), "appID required") {
		t.Errorf("expected appID required error, got %v", err)
	}
}

// TestAWSConfigForThreadsTargetFields locks in the mapping from
// AccountTarget to the AWSConfig the runner sets as Pulumi config —
// the seam that replaced the old "mint env vars and shove them at
// Pulumi" path.
func TestAWSConfigForThreadsTargetFields(t *testing.T) {
	target := AccountTarget{
		OrgID:          "org",
		CloudAccountID: "ca-uuid",
		AWSAccountID:   "111122223333",
		RoleARN:        "arn:aws:iam::111122223333:role/Spacefleet",
		ExternalID:     "abc123",
		Region:         "eu-west-1",
	}
	got := awsConfigFor(target, target.resolvedRegion())
	if got.Region != "eu-west-1" {
		t.Errorf("Region = %q, want eu-west-1", got.Region)
	}
	if got.RoleARN != target.RoleARN {
		t.Errorf("RoleARN = %q, want %q", got.RoleARN, target.RoleARN)
	}
	if got.ExternalID != "abc123" {
		t.Errorf("ExternalID = %q, want abc123", got.ExternalID)
	}
	if !strings.HasPrefix(got.SessionName, "spacefleet-") {
		t.Errorf("SessionName = %q, want spacefleet- prefix", got.SessionName)
	}
}

// TestUpBuilderInfraValidatesTargetEarly confirms target validation
// fires before any expensive call (Pulumi spin-up).
func TestUpBuilderInfraValidatesTargetEarly(t *testing.T) {
	o, err := NewOrchestrator(goodBackend, "img@sha256:x")
	if err != nil {
		t.Fatalf("NewOrchestrator: %v", err)
	}
	_, err = o.UpBuilderInfra(context.Background(), AccountTarget{}, RunOpts{})
	if err == nil {
		t.Fatal("expected validation error on empty target")
	}
	if !strings.Contains(err.Error(), "target.") {
		t.Errorf("err = %v, want target.* validation error", err)
	}
}
