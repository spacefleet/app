// Package aws implements AWS cloud-account onboarding: minting external
// IDs, building CloudFormation Quick Create URLs, and verifying that a
// customer's IAM role can be assumed cross-account.
//
// We deliberately don't pull the AWS SDK into the rest of the app — only
// this package imports it. Callers receive *ent.CloudAccount rows and
// pre-built URLs, not AWS clients.
package aws

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"net/url"
	"strings"
)

// externalIDLen is the byte count behind each external ID. 32 bytes
// becomes a 52-char base32 string — enough entropy that brute-forcing
// the trust policy is hopeless, short enough to read aloud if a customer
// needs to.
const externalIDLen = 32

// newExternalID returns an unpadded base32-uppercase external ID. The
// CFN template's MinLength is 32, so we trim the padding to keep the
// shape predictable across regenerations.
func newExternalID() (string, error) {
	buf := make([]byte, externalIDLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding)
	return enc.EncodeToString(buf), nil
}

// QuickCreateParams are the inputs that feed into a CloudFormation Quick
// Create URL. Region is optional; when set it pins the customer to that
// region's console (the role itself is global, but the stack lives in
// one region).
type QuickCreateParams struct {
	TemplateURL     string
	StackName       string
	PlatformAccount string
	ExternalID      string
	Region          string
}

// QuickCreateURL builds the AWS Console URL that pre-fills the role
// trust-policy parameters and drops the customer at the "Create stack"
// confirmation. The customer just clicks "I acknowledge IAM resources
// will be created" and Create.
//
// We intentionally rely on AWS Console's documented quickcreate fragment
// — it's a regular AWS UX, no scraping or undocumented surface.
func QuickCreateURL(p QuickCreateParams) (string, error) {
	if p.TemplateURL == "" {
		return "", errors.New("aws: TemplateURL required")
	}
	if p.PlatformAccount == "" {
		return "", errors.New("aws: PlatformAccount required")
	}
	if p.ExternalID == "" {
		return "", errors.New("aws: ExternalID required")
	}
	if p.StackName == "" {
		p.StackName = "spacefleet-integration"
	}

	// AWS expects the params as `param_<Name>=<value>` query keys. The
	// host varies by region: us-east-1 has no region-prefix on the
	// console URL, others do.
	host := "console.aws.amazon.com"
	if p.Region != "" && p.Region != "us-east-1" {
		host = p.Region + ".console.aws.amazon.com"
	}

	q := url.Values{}
	q.Set("templateURL", p.TemplateURL)
	q.Set("stackName", p.StackName)
	q.Set("param_SpacefleetAccountId", p.PlatformAccount)
	q.Set("param_ExternalId", p.ExternalID)

	// CloudFormation's quickcreate page is a fragment route in the
	// SPA-shaped console. Query lives after the hash.
	return "https://" + host + "/cloudformation/home?region=" + url.QueryEscape(regionOrDefault(p.Region)) +
		"#/stacks/quickcreate?" + q.Encode(), nil
}

func regionOrDefault(r string) string {
	if r == "" {
		return "us-east-1"
	}
	return r
}

// AccountIDFromRoleARN returns the 12-digit account ID embedded in a
// canonical IAM role ARN ("arn:aws:iam::123456789012:role/Name"). Any
// other shape returns an empty string — the caller should treat that
// as a bad-input case.
func AccountIDFromRoleARN(arn string) string {
	parts := strings.Split(arn, ":")
	if len(parts) < 6 {
		return ""
	}
	if parts[0] != "arn" || parts[2] != "iam" {
		return ""
	}
	if !strings.HasPrefix(parts[5], "role/") {
		return ""
	}
	id := parts[4]
	if len(id) != 12 {
		return ""
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			return ""
		}
	}
	return id
}
