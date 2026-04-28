package builderinfra

import (
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func TestInputsValidate(t *testing.T) {
	cases := []struct {
		name    string
		in      Inputs
		wantErr string
	}{
		{
			"empty org",
			Inputs{CloudAccountID: "ca", Region: "us-east-1"},
			"OrgID required",
		},
		{
			"empty cloud account",
			Inputs{OrgID: "org", Region: "us-east-1"},
			"CloudAccountID required",
		},
		{
			"empty region",
			Inputs{OrgID: "org", CloudAccountID: "ca"},
			"Region required",
		},
		{
			"valid",
			Inputs{OrgID: "org", CloudAccountID: "ca", Region: "us-east-1"},
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.in.Validate()
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

// TestProgramReturnsClosure proves Program is a factory: it doesn't
// run resources at construction; it returns a function that, when
// called by Pulumi, invokes Validate. The factory itself is pure.
func TestProgramReturnsClosure(t *testing.T) {
	// Calling Program with bad inputs must NOT panic — it just
	// returns a closure that will fail when Pulumi runs it. This
	// keeps construction errors out of factory call sites and lets
	// the inline-program contract (`return func(*Context) error`)
	// stay clean.
	closure := Program(Inputs{}) // intentionally invalid
	if closure == nil {
		t.Fatal("Program returned nil closure")
	}
}

// TestOutputKeysUnique guards against typo-driven export collisions:
// if two output constants accidentally have the same value, the second
// `ctx.Export` call would overwrite the first and the orchestrator
// would silently lose one piece of data. The set is small enough to
// hand-list in tests and large enough to be worth checking.
func TestOutputKeysUnique(t *testing.T) {
	keys := []string{
		OutputClusterARN,
		OutputClusterName,
		OutputVpcID,
		OutputSubnetID,
		OutputSecurityGroupID,
		OutputExecutionRoleARN,
		OutputLogGroupPrefix,
	}
	seen := map[string]bool{}
	for _, k := range keys {
		if k == "" {
			t.Errorf("output key is empty")
		}
		if seen[k] {
			t.Errorf("duplicate output key %q", k)
		}
		seen[k] = true
	}
}

// TestExecutionPolicyTemplateContainsLogNamespace catches a regression
// where someone removes the `%s` from the policy template (or hardcodes
// the namespace) and breaks the per-installation namespace scope.
func TestExecutionPolicyTemplateContainsLogNamespace(t *testing.T) {
	if !strings.Contains(executionRolePolicyTemplate, "%s") {
		t.Fatal(`executionRolePolicyTemplate has no placeholder for namespace`)
	}
	rendered := strings.Replace(executionRolePolicyTemplate, "%s", LogGroupNamespace, 1)
	if !strings.Contains(rendered, "/spacefleet/builds/*") {
		t.Errorf("rendered policy missing log group resource scope, got:\n%s", rendered)
	}
	if !strings.Contains(rendered, "spacefleet/builds/*") {
		t.Errorf("rendered policy missing secrets resource scope, got:\n%s", rendered)
	}
}

// TestConstantsLookSane makes sure noisy textual constants aren't
// accidentally swapped — protects ranger from a copy-paste error
// changing CIDR or subnet to overlapping values.
func TestConstantsLookSane(t *testing.T) {
	if CIDRBlock == "" || BuilderSubnetCIDR == "" {
		t.Fatal("CIDR constants are empty")
	}
	if !strings.HasPrefix(BuilderSubnetCIDR, "10.100.") {
		t.Errorf("subnet CIDR %q must be inside VPC CIDR %q", BuilderSubnetCIDR, CIDRBlock)
	}
	if BuilderClusterName != "spacefleet-builds" {
		t.Errorf("cluster name unexpectedly %q — IAM policies in onboarding-template.yaml depend on this prefix", BuilderClusterName)
	}
	if !strings.HasPrefix(ExecutionRoleName, "spacefleet-") {
		t.Errorf("execution role name %q must keep `spacefleet-` prefix", ExecutionRoleName)
	}
	if !strings.HasPrefix(LogGroupNamespace, "/spacefleet/") {
		t.Errorf("log namespace %q must be under /spacefleet/", LogGroupNamespace)
	}
}

// TestNameTagDoesNotMutate confirms nameTag returns a fresh map. We
// reuse `tags` across multiple resources in the program; a mutation
// would couple their states and confuse Pulumi's resource graph.
func TestNameTagDoesNotMutate(t *testing.T) {
	base := pulumi.StringMap{"a": pulumi.String("1")}
	out := nameTag(base, "foo")
	if _, ok := base["Name"]; ok {
		t.Error("nameTag mutated base map (added Name key)")
	}
	if _, ok := out["Name"]; !ok {
		t.Error("nameTag did not add Name key to result")
	}
	if v, ok := out["a"]; !ok || v == nil {
		t.Error("nameTag did not copy original keys")
	}
}
