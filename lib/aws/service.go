package aws

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/spacefleet/app/ent"
	"github.com/spacefleet/app/ent/cloudaccount"
)

const (
	ProviderAWS = "aws"

	StatusPending   = "pending"
	StatusConnected = "connected"
	StatusError     = "error"
	StatusDisabled  = "disabled"
)

var (
	ErrAccountNotFound  = errors.New("cloud account not found")
	ErrLabelInUse       = errors.New("label already used in this org")
	ErrInvalidRoleARN   = errors.New("role arn must look like arn:aws:iam::<account>:role/<name>")
	ErrAccountMismatch  = errors.New("role arn account does not match the assumed-role account")
	ErrAlreadyCompleted = errors.New("cloud account already completed")
)

// Service is the persistence-aware coordinator for AWS onboarding. It
// owns the ent.Client and the verifier; routes don't touch either
// directly. Verifier may be nil when AWS creds aren't loaded — start/
// list/delete still work, only the verify path 5xx's.
type Service struct {
	ent             *ent.Client
	verifier        *Verifier
	platformAccount string
	templateURL     string
}

func NewService(entClient *ent.Client, verifier *Verifier, platformAccount, templateURL string) *Service {
	return &Service{
		ent:             entClient,
		verifier:        verifier,
		platformAccount: platformAccount,
		templateURL:     templateURL,
	}
}

// PlatformAccount is the AWS account ID Spacefleet runs in — the
// principal that customer trust policies grant AssumeRole to. Surfaced
// to the UI so the connect flow can show "you're trusting account X".
func (s *Service) PlatformAccount() string { return s.platformAccount }

// StartParams names the new cloud account being created. Region is
// optional; only used to pin the Quick Create URL to a specific console
// region.
type StartParams struct {
	OrgSlug string
	Label   string
	Region  string
}

// StartResult is what the start endpoint hands the UI: the persisted
// row, the external ID (visible exactly once), and the URL to open.
type StartResult struct {
	Account        *ent.CloudAccount
	ExternalID     string
	QuickCreateURL string
}

// Start creates a pending cloud account and the Quick Create URL the
// customer will follow. The external ID is generated here, persisted as
// part of the row, and returned to the caller for one-time display.
func (s *Service) Start(ctx context.Context, p StartParams) (*StartResult, error) {
	if p.Label == "" {
		return nil, errors.New("label required")
	}

	extID, err := newExternalID()
	if err != nil {
		return nil, err
	}

	row, err := s.ent.CloudAccount.Create().
		SetOrgSlug(p.OrgSlug).
		SetProvider(ProviderAWS).
		SetLabel(p.Label).
		SetExternalID(extID).
		SetRegion(p.Region).
		SetStatus(StatusPending).
		Save(ctx)
	if err != nil {
		if ent.IsConstraintError(err) {
			return nil, ErrLabelInUse
		}
		return nil, err
	}

	qcURL, err := QuickCreateURL(QuickCreateParams{
		TemplateURL:     s.templateURL,
		StackName:       "spacefleet-" + p.Label,
		PlatformAccount: s.platformAccount,
		ExternalID:      extID,
		Region:          p.Region,
	})
	if err != nil {
		return nil, err
	}

	return &StartResult{
		Account:        row,
		ExternalID:     extID,
		QuickCreateURL: qcURL,
	}, nil
}

// Complete records the role ARN the customer pasted back, runs the
// verification probe, and flips status to connected on success. On
// failure the row is updated with last_verification_error and the
// status moves to "error" — the customer can fix their stack and call
// Verify again without re-starting onboarding.
func (s *Service) Complete(ctx context.Context, orgSlug string, id uuid.UUID, roleARN string) (*ent.CloudAccount, error) {
	row, err := s.find(ctx, orgSlug, id)
	if err != nil {
		return nil, err
	}
	if row.Status == StatusConnected {
		return nil, ErrAlreadyCompleted
	}

	arnAccount := AccountIDFromRoleARN(roleARN)
	if arnAccount == "" {
		return nil, ErrInvalidRoleARN
	}

	verifyErr := s.runVerify(ctx, roleARN, row.ExternalID, arnAccount)

	upd := s.ent.CloudAccount.UpdateOneID(row.ID).
		SetRoleArn(roleARN).
		SetAccountID(arnAccount)
	now := time.Now()
	if verifyErr != nil {
		upd = upd.SetStatus(StatusError).
			SetLastVerificationError(verifyErr.Error()).
			ClearLastVerifiedAt()
	} else {
		upd = upd.SetStatus(StatusConnected).
			SetLastVerifiedAt(now).
			SetLastVerificationError("")
	}

	saved, err := upd.Save(ctx)
	if err != nil {
		return nil, err
	}
	if verifyErr != nil {
		return saved, verifyErr
	}
	return saved, nil
}

// Verify re-runs the probe against an existing account. Used by the
// "verify" button in the UI and (eventually) a daily scheduler.
func (s *Service) Verify(ctx context.Context, orgSlug string, id uuid.UUID) (*ent.CloudAccount, error) {
	row, err := s.find(ctx, orgSlug, id)
	if err != nil {
		return nil, err
	}
	if row.RoleArn == "" {
		return nil, errors.New("cloud account has no role arn yet — complete onboarding first")
	}

	verifyErr := s.runVerify(ctx, row.RoleArn, row.ExternalID, row.AccountID)

	upd := s.ent.CloudAccount.UpdateOneID(row.ID)
	now := time.Now()
	if verifyErr != nil {
		upd = upd.SetStatus(StatusError).SetLastVerificationError(verifyErr.Error())
	} else {
		upd = upd.SetStatus(StatusConnected).
			SetLastVerifiedAt(now).
			SetLastVerificationError("")
	}
	saved, err := upd.Save(ctx)
	if err != nil {
		return nil, err
	}
	if verifyErr != nil {
		return saved, verifyErr
	}
	return saved, nil
}

// runVerify dispatches the actual STS probe. Returns nil on success, an
// error on failure. If the verifier isn't configured, that's a deploy-
// time problem, not a customer problem — surface it as a 500-shaped
// error rather than recording it on the row.
func (s *Service) runVerify(ctx context.Context, roleARN, externalID, expectedAccount string) error {
	if s.verifier == nil {
		return errors.New("aws verifier not configured")
	}
	res, err := s.verifier.Verify(ctx, roleARN, externalID)
	if err != nil {
		return err
	}
	if expectedAccount != "" && res.Account != "" && res.Account != expectedAccount {
		return ErrAccountMismatch
	}
	return nil
}

// List returns every cloud account an org has created, newest first.
// External IDs are *not* hidden in the row — the caller is responsible
// for choosing whether to surface them; pending rows that the user
// abandoned mid-onboard will still need the ID to retry.
func (s *Service) List(ctx context.Context, orgSlug string) ([]*ent.CloudAccount, error) {
	return s.ent.CloudAccount.Query().
		Where(cloudaccount.OrgSlugEQ(orgSlug)).
		Order(ent.Desc(cloudaccount.FieldCreatedAt)).
		All(ctx)
}

// Get returns a single account, scoped to org so cross-org leakage is
// impossible by ID guess.
func (s *Service) Get(ctx context.Context, orgSlug string, id uuid.UUID) (*ent.CloudAccount, error) {
	return s.find(ctx, orgSlug, id)
}

// Delete drops Spacefleet's record. We don't try to delete the
// CloudFormation stack on the customer's side — they own that, and a
// failed-permissions delete would leave the system in a worse state
// than just walking away.
func (s *Service) Delete(ctx context.Context, orgSlug string, id uuid.UUID) error {
	if _, err := s.find(ctx, orgSlug, id); err != nil {
		return err
	}
	return s.ent.CloudAccount.DeleteOneID(id).Exec(ctx)
}

func (s *Service) find(ctx context.Context, orgSlug string, id uuid.UUID) (*ent.CloudAccount, error) {
	row, err := s.ent.CloudAccount.Query().
		Where(
			cloudaccount.IDEQ(id),
			cloudaccount.OrgSlugEQ(orgSlug),
		).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrAccountNotFound
		}
		return nil, err
	}
	return row, nil
}
