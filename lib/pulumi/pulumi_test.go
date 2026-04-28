package pulumi

import (
	"strings"
	"testing"
)

func TestBackendConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     BackendConfig
		wantErr string
	}{
		{
			"empty bucket",
			BackendConfig{Region: "us-east-1", KMSKeyARN: "arn:aws:kms:..."},
			"state bucket is empty",
		},
		{
			"empty region",
			BackendConfig{Bucket: "b", KMSKeyARN: "arn:aws:kms:..."},
			"state region is empty",
		},
		{
			"empty kms",
			BackendConfig{Bucket: "b", Region: "us-east-1"},
			"state KMS key ARN is empty",
		},
		{
			"non-arn kms",
			BackendConfig{Bucket: "b", Region: "us-east-1", KMSKeyARN: "not-an-arn"},
			"does not look like an ARN",
		},
		{
			"valid",
			BackendConfig{Bucket: "b", Region: "us-east-1", KMSKeyARN: "arn:aws:kms:us-east-1:111122223333:key/abcd"},
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
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

// TestBackendForBuilderInfra locks in the s3:// path layout and Pulumi
// stack name. The exact format is in BUILD_PIPELINE.md; if it ever
// changes this test breaks loudly so the migration path gets thought
// through (existing state files would otherwise be orphaned).
func TestBackendForBuilderInfra(t *testing.T) {
	cfg := BackendConfig{
		Bucket:    "spacefleet-state",
		Region:    "us-east-1",
		KMSKeyARN: "arn:aws:kms:us-east-1:111122223333:key/abcd",
	}
	got, err := BackendForBuilderInfra(cfg, "org-A", "acct-B")
	if err != nil {
		t.Fatalf("BackendForBuilderInfra: %v", err)
	}
	wantURL := "s3://spacefleet-state/org-A/acct-B/builder-infra?region=us-east-1"
	if got.StateURL != wantURL {
		t.Errorf("StateURL = %q\n want %q", got.StateURL, wantURL)
	}
	wantStack := "acct-B-builder-infra"
	if got.StackName != wantStack {
		t.Errorf("StackName = %q, want %q", got.StackName, wantStack)
	}
	wantSecrets := "awskms://abcd?region=us-east-1"
	if got.SecretsProvider != wantSecrets {
		t.Errorf("SecretsProvider = %q\n want %q", got.SecretsProvider, wantSecrets)
	}
}

func TestKeyIDFromARN(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"arn:aws:kms:us-east-2:111122223333:key/74d6b102-b806-4efd-9fd0-c6eba3eed297", "74d6b102-b806-4efd-9fd0-c6eba3eed297"},
		{"arn:aws:kms:us-east-1:111122223333:alias/spacefleet-state", "alias/spacefleet-state"},
		{"abcd-1234", "abcd-1234"},          // already a key ID
		{"alias/something", "alias/something"}, // already an alias
	}
	for _, tc := range cases {
		if got := keyIDFromARN(tc.in); got != tc.want {
			t.Errorf("keyIDFromARN(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBackendForAppBuild(t *testing.T) {
	cfg := BackendConfig{
		Bucket:    "spacefleet-state",
		Region:    "eu-west-1",
		KMSKeyARN: "arn:aws:kms:eu-west-1:111122223333:key/abcd",
	}
	got, err := BackendForAppBuild(cfg, "org-1", "app-9")
	if err != nil {
		t.Fatalf("BackendForAppBuild: %v", err)
	}
	wantURL := "s3://spacefleet-state/org-1/app-9/build?region=eu-west-1"
	if got.StateURL != wantURL {
		t.Errorf("StateURL = %q\n want %q", got.StateURL, wantURL)
	}
	wantStack := "app-9-build"
	if got.StackName != wantStack {
		t.Errorf("StackName = %q, want %q", got.StackName, wantStack)
	}
}

// TestBackendRejectsBadIDs covers the input-sanitisation cases. We
// don't want a stray slash or whitespace to silently re-shape the s3://
// path; that's the kind of thing that creates orphaned state files
// nobody can find.
func TestBackendRejectsBadIDs(t *testing.T) {
	cfg := BackendConfig{Bucket: "b", Region: "us-east-1", KMSKeyARN: "arn:aws:kms:us-east-1:111122223333:key/abcd"}
	cases := []struct {
		name           string
		orgID, appID   string
		cloudAccountID string
	}{
		{"empty org", "", "acct", ""},
		{"slash in org", "org/with/slash", "acct", ""},
		{"empty cloud account", "org", "", ""},
		{"whitespace in app", "org", "", "app id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.cloudAccountID == "" && tc.appID != "" {
				if _, err := BackendForAppBuild(cfg, tc.orgID, tc.appID); err == nil {
					t.Error("expected error from BackendForAppBuild")
				}
				return
			}
			if _, err := BackendForBuilderInfra(cfg, tc.orgID, tc.cloudAccountID); err == nil {
				t.Error("expected error from BackendForBuilderInfra")
			}
		})
	}
}

func TestRegionFromARN(t *testing.T) {
	cases := []struct {
		arn  string
		want string
	}{
		{"arn:aws:kms:us-east-1:111122223333:key/abcd", "us-east-1"},
		{"arn:aws:kms:eu-west-2:111122223333:alias/x", "eu-west-2"},
		{"arn:aws:iam::111122223333:role/Spacefleet", ""},
		{"not-an-arn", ""},
	}
	for _, tc := range cases {
		t.Run(tc.arn, func(t *testing.T) {
			got := regionFromARN(tc.arn)
			if got != tc.want {
				t.Errorf("regionFromARN(%q) = %q, want %q", tc.arn, got, tc.want)
			}
		})
	}
}
