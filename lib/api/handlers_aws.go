package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/spacefleet/app/ent"
	awsint "github.com/spacefleet/app/lib/aws"
	"github.com/spacefleet/app/lib/auth"
)

// StartAwsAccount kicks off cloud-account onboarding: mint external ID,
// persist a pending row, return the Quick Create URL. Clerk-only because
// the next step (clicking through to AWS Console) is a browser action;
// a CLI session has nothing to do with it.
func (s *Server) StartAwsAccount(ctx context.Context, req StartAwsAccountRequestObject) (StartAwsAccountResponseObject, error) {
	sess, ok := auth.FromContext(ctx)
	if !ok {
		return errResp[StartAwsAccountdefaultJSONResponse](http.StatusUnauthorized, "unauthorized", "missing session"), nil
	}
	if sess.Source != auth.SourceClerk {
		return errResp[StartAwsAccountdefaultJSONResponse](http.StatusForbidden, "forbidden", "browser session required"), nil
	}
	if s.aws == nil {
		return errResp[StartAwsAccountdefaultJSONResponse](http.StatusServiceUnavailable, "aws_not_configured", "AWS onboarding is not configured on this server"), nil
	}
	if req.Body == nil || req.Body.Label == "" {
		return errResp[StartAwsAccountdefaultJSONResponse](http.StatusBadRequest, "bad_request", "label is required"), nil
	}

	region := ""
	if req.Body.Region != nil {
		region = *req.Body.Region
	}

	res, err := s.aws.Start(ctx, awsint.StartParams{
		OrgSlug: req.Slug,
		Label:   req.Body.Label,
		Region:  region,
	})
	if err != nil {
		if errors.Is(err, awsint.ErrLabelInUse) {
			return errResp[StartAwsAccountdefaultJSONResponse](http.StatusConflict, "label_in_use", err.Error()), nil
		}
		return nil, err
	}
	return StartAwsAccount200JSONResponse{
		Account:           cloudAccountToAPI(res.Account),
		ExternalId:        res.ExternalID,
		QuickCreateUrl:    res.QuickCreateURL,
		PlatformAccountId: s.aws.PlatformAccount(),
	}, nil
}

// CompleteAwsAccount records the role ARN and runs the verification
// probe. Even on verification failure the row is updated (status=error
// + last_verification_error) and returned — the UI uses the row to
// show the customer what went wrong without making them re-onboard.
func (s *Server) CompleteAwsAccount(ctx context.Context, req CompleteAwsAccountRequestObject) (CompleteAwsAccountResponseObject, error) {
	if s.aws == nil {
		return errResp[CompleteAwsAccountdefaultJSONResponse](http.StatusServiceUnavailable, "aws_not_configured", "AWS onboarding is not configured on this server"), nil
	}
	if req.Body == nil || req.Body.RoleArn == "" {
		return errResp[CompleteAwsAccountdefaultJSONResponse](http.StatusBadRequest, "bad_request", "role_arn required"), nil
	}
	row, err := s.aws.Complete(ctx, req.Slug, uuid.UUID(req.Id), req.Body.RoleArn)
	if row != nil {
		// Row got persisted (status=connected or status=error). Surface
		// it to the UI even when verify itself failed, so the customer
		// sees the captured error message inline.
		return CompleteAwsAccount200JSONResponse(cloudAccountToAPI(row)), nil
	}
	switch {
	case errors.Is(err, awsint.ErrAccountNotFound):
		return errResp[CompleteAwsAccountdefaultJSONResponse](http.StatusNotFound, "not_found", err.Error()), nil
	case errors.Is(err, awsint.ErrInvalidRoleARN):
		return errResp[CompleteAwsAccountdefaultJSONResponse](http.StatusBadRequest, "invalid_role_arn", err.Error()), nil
	case errors.Is(err, awsint.ErrAlreadyCompleted):
		return errResp[CompleteAwsAccountdefaultJSONResponse](http.StatusConflict, "already_completed", err.Error()), nil
	}
	return nil, err
}

// VerifyAwsAccount re-runs the probe against an already-onboarded row.
// Same row-or-error pattern as Complete.
func (s *Server) VerifyAwsAccount(ctx context.Context, req VerifyAwsAccountRequestObject) (VerifyAwsAccountResponseObject, error) {
	if s.aws == nil {
		return errResp[VerifyAwsAccountdefaultJSONResponse](http.StatusServiceUnavailable, "aws_not_configured", "AWS onboarding is not configured on this server"), nil
	}
	row, err := s.aws.Verify(ctx, req.Slug, uuid.UUID(req.Id))
	if row != nil {
		return VerifyAwsAccount200JSONResponse(cloudAccountToAPI(row)), nil
	}
	if errors.Is(err, awsint.ErrAccountNotFound) {
		return errResp[VerifyAwsAccountdefaultJSONResponse](http.StatusNotFound, "not_found", err.Error()), nil
	}
	return nil, err
}

// ListAwsAccounts returns rows for this org, newest first.
func (s *Server) ListAwsAccounts(ctx context.Context, req ListAwsAccountsRequestObject) (ListAwsAccountsResponseObject, error) {
	if s.aws == nil {
		return errResp[ListAwsAccountsdefaultJSONResponse](http.StatusServiceUnavailable, "aws_not_configured", "AWS onboarding is not configured on this server"), nil
	}
	rows, err := s.aws.List(ctx, req.Slug)
	if err != nil {
		return nil, err
	}
	out := make([]CloudAccount, 0, len(rows))
	for _, r := range rows {
		out = append(out, cloudAccountToAPI(r))
	}
	return ListAwsAccounts200JSONResponse{Accounts: out}, nil
}

// GetAwsAccount returns one row.
func (s *Server) GetAwsAccount(ctx context.Context, req GetAwsAccountRequestObject) (GetAwsAccountResponseObject, error) {
	if s.aws == nil {
		return errResp[GetAwsAccountdefaultJSONResponse](http.StatusServiceUnavailable, "aws_not_configured", "AWS onboarding is not configured on this server"), nil
	}
	row, err := s.aws.Get(ctx, req.Slug, uuid.UUID(req.Id))
	if err != nil {
		if errors.Is(err, awsint.ErrAccountNotFound) {
			return errResp[GetAwsAccountdefaultJSONResponse](http.StatusNotFound, "not_found", err.Error()), nil
		}
		return nil, err
	}
	return GetAwsAccount200JSONResponse(cloudAccountToAPI(row)), nil
}

// DeleteAwsAccount drops the row. Idempotent — missing row returns 204
// like a successful delete, since the end state is identical.
func (s *Server) DeleteAwsAccount(ctx context.Context, req DeleteAwsAccountRequestObject) (DeleteAwsAccountResponseObject, error) {
	if s.aws == nil {
		return errResp[DeleteAwsAccountdefaultJSONResponse](http.StatusServiceUnavailable, "aws_not_configured", "AWS onboarding is not configured on this server"), nil
	}
	if err := s.aws.Delete(ctx, req.Slug, uuid.UUID(req.Id)); err != nil {
		if errors.Is(err, awsint.ErrAccountNotFound) {
			return DeleteAwsAccount204Response{}, nil
		}
		return nil, err
	}
	return DeleteAwsAccount204Response{}, nil
}

// cloudAccountToAPI converts an ent row to the API shape. External ID
// is *not* included — surfaced only at start time. Empty strings on
// nullable fields collapse to nil so the JSON null matches the schema.
func cloudAccountToAPI(row *ent.CloudAccount) CloudAccount {
	out := CloudAccount{
		Id:        row.ID,
		Provider:  row.Provider,
		Label:     row.Label,
		Status:    row.Status,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}
	if row.AccountID != "" {
		v := row.AccountID
		out.AccountId = &v
	}
	if row.RoleArn != "" {
		v := row.RoleArn
		out.RoleArn = &v
	}
	if row.Region != "" {
		v := row.Region
		out.Region = &v
	}
	if row.LastVerifiedAt != nil {
		out.LastVerifiedAt = row.LastVerifiedAt
	}
	if row.LastVerificationError != "" {
		v := row.LastVerificationError
		out.LastVerificationError = &v
	}
	return out
}
