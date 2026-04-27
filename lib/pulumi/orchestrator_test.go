package pulumi

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
)

// fakeCreds is a hand-rolled stand-in for lib/aws.Verifier. We hold a
// hook so individual tests can flip behavior without instantiating
// real STS plumbing.
type fakeCreds struct {
	calls   []fakeCredsCall
	respond func(roleARN, externalID, region, sessionName string) (map[string]string, error)
}

type fakeCredsCall struct {
	RoleARN, ExternalID, Region, SessionName string
}

func (f *fakeCreds) AssumeRoleEnv(_ context.Context, roleARN, externalID, region, sessionName string) (map[string]string, error) {
	f.calls = append(f.calls, fakeCredsCall{roleARN, externalID, region, sessionName})
	if f.respond != nil {
		return f.respond(roleARN, externalID, region, sessionName)
	}
	return map[string]string{
		"AWS_ACCESS_KEY_ID":     "AKIA",
		"AWS_SECRET_ACCESS_KEY": "secret",
		"AWS_SESSION_TOKEN":     "session",
		"AWS_REGION":            region,
	}, nil
}

var goodBackend = BackendConfig{
	Bucket:    "spacefleet-state",
	Region:    "us-east-1",
	KMSKeyARN: "arn:aws:kms:us-east-1:111122223333:key/abcd",
}

func TestNewOrchestratorValidates(t *testing.T) {
	creds := &fakeCreds{}

	cases := []struct {
		name    string
		backend BackendConfig
		creds   Credentials
		image   string
		wantErr string
	}{
		{
			"valid",
			goodBackend,
			creds,
			"ghcr.io/spacefleet/spacefleet-app/builder:v1@sha256:abc",
			"",
		},
		{
			"bad backend",
			BackendConfig{Bucket: "b"},
			creds,
			"img@sha256:x",
			"region",
		},
		{
			"nil creds",
			goodBackend,
			nil,
			"img@sha256:x",
			"credentials provider",
		},
		{
			"empty image",
			goodBackend,
			creds,
			"",
			"builder image required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewOrchestrator(tc.backend, tc.creds, tc.image)
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

// TestUpAppBuildRequiresAppID confirms the orchestrator catches the
// caller-side mistake of dropping the app id; we shouldn't fall
// through to running builder-infra successfully and then fail with
// something opaque on app-build.
func TestUpAppBuildRequiresAppID(t *testing.T) {
	o, err := NewOrchestrator(goodBackend, &fakeCreds{}, "img@sha256:x")
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
	_, _, err = o.UpAppBuild(context.Background(), target, "", RunOpts{})
	if err == nil || !strings.Contains(err.Error(), "appID required") {
		t.Errorf("expected appID required error, got %v", err)
	}
}

// TestDestroyAppBuildRequiresAppID is the destroy-side mirror.
func TestDestroyAppBuildRequiresAppID(t *testing.T) {
	o, err := NewOrchestrator(goodBackend, &fakeCreds{}, "img@sha256:x")
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

// TestUpBuilderInfraSurfacesAssumeRoleErrors exercises the failure
// branch where AssumeRole fails (e.g., trust policy denies). We don't
// want the orchestrator to swallow the cause.
func TestUpBuilderInfraSurfacesAssumeRoleErrors(t *testing.T) {
	creds := &fakeCreds{
		respond: func(string, string, string, string) (map[string]string, error) {
			return nil, errors.New("AccessDenied: AssumeRole forbidden")
		},
	}
	o, err := NewOrchestrator(goodBackend, creds, "img@sha256:x")
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
	_, err = o.UpBuilderInfra(context.Background(), target, RunOpts{})
	if err == nil {
		t.Fatal("expected error from assume role failure")
	}
	if !strings.Contains(err.Error(), "assume role") {
		t.Errorf("err = %v, want substring 'assume role'", err)
	}
	if !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("err = %v, want underlying error to be wrapped", err)
	}
}

// TestUpBuilderInfraValidatesTargetEarly confirms target validation
// fires before any expensive call (AssumeRole, Pulumi spin-up).
func TestUpBuilderInfraValidatesTargetEarly(t *testing.T) {
	creds := &fakeCreds{}
	o, err := NewOrchestrator(goodBackend, creds, "img@sha256:x")
	if err != nil {
		t.Fatalf("NewOrchestrator: %v", err)
	}
	_, err = o.UpBuilderInfra(context.Background(), AccountTarget{}, RunOpts{})
	if err == nil {
		t.Fatal("expected validation error on empty target")
	}
	if len(creds.calls) != 0 {
		t.Errorf("AssumeRole was called despite invalid target: %v", creds.calls)
	}
}
