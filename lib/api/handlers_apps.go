package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/spacefleet/app/ent"
	"github.com/spacefleet/app/lib/apps"
	"github.com/spacefleet/app/lib/auth"
	"github.com/spacefleet/app/lib/github"
)

// CreateApp registers a new app for the org. Clerk-only because the
// create flow is a UI form — a CLI session has no way to drive the
// repo picker.
func (s *Server) CreateApp(ctx context.Context, req CreateAppRequestObject) (CreateAppResponseObject, error) {
	sess, ok := auth.FromContext(ctx)
	if !ok {
		return errResp[CreateAppdefaultJSONResponse](http.StatusUnauthorized, "unauthorized", "missing session"), nil
	}
	if sess.Source != auth.SourceClerk {
		return errResp[CreateAppdefaultJSONResponse](http.StatusForbidden, "forbidden", "browser session required"), nil
	}
	if s.apps == nil {
		return errResp[CreateAppdefaultJSONResponse](http.StatusServiceUnavailable, "apps_not_configured", "apps service is not configured on this server"), nil
	}
	if req.Body == nil {
		return errResp[CreateAppdefaultJSONResponse](http.StatusBadRequest, "bad_request", "request body required"), nil
	}

	row, err := s.apps.Create(ctx, apps.CreateParams{
		OrgSlug:              req.Slug,
		Name:                 req.Body.Name,
		CloudAccountID:       uuid.UUID(req.Body.CloudAccountId),
		GithubInstallationID: uuid.UUID(req.Body.GithubInstallationId),
		GithubRepoFullName:   req.Body.GithubRepoFullName,
		CreatedBy:            sess.UserID,
	})
	if err != nil {
		return appsErrToCreateResp(err), nil
	}
	return CreateApp201JSONResponse(appToAPI(row)), nil
}

// ListApps returns every app in the org, newest first.
func (s *Server) ListApps(ctx context.Context, req ListAppsRequestObject) (ListAppsResponseObject, error) {
	if s.apps == nil {
		return errResp[ListAppsdefaultJSONResponse](http.StatusServiceUnavailable, "apps_not_configured", "apps service is not configured on this server"), nil
	}
	rows, err := s.apps.List(ctx, req.Slug)
	if err != nil {
		return nil, err
	}
	out := make([]App, 0, len(rows))
	for _, r := range rows {
		out = append(out, appToAPI(r))
	}
	return ListApps200JSONResponse{Apps: out}, nil
}

// GetApp returns one app by slug.
func (s *Server) GetApp(ctx context.Context, req GetAppRequestObject) (GetAppResponseObject, error) {
	if s.apps == nil {
		return errResp[GetAppdefaultJSONResponse](http.StatusServiceUnavailable, "apps_not_configured", "apps service is not configured on this server"), nil
	}
	row, err := s.apps.Get(ctx, req.Slug, req.AppSlug)
	if err != nil {
		if errors.Is(err, apps.ErrAppNotFound) {
			return errResp[GetAppdefaultJSONResponse](http.StatusNotFound, "not_found", err.Error()), nil
		}
		return nil, err
	}
	return GetApp200JSONResponse(appToAPI(row)), nil
}

// DeleteApp drops the app row, optionally tearing down the per-app AWS
// stack via a queued destroy_app job. 200 means the row is gone; 202
// means the destroy job was enqueued and the row will disappear once
// the worker finishes.
func (s *Server) DeleteApp(ctx context.Context, req DeleteAppRequestObject) (DeleteAppResponseObject, error) {
	if s.apps == nil {
		return errResp[DeleteAppdefaultJSONResponse](http.StatusServiceUnavailable, "apps_not_configured", "apps service is not configured on this server"), nil
	}

	destroy := false
	if req.Body != nil && req.Body.DestroyResources != nil {
		destroy = *req.Body.DestroyResources
	}

	res, err := s.apps.Delete(ctx, apps.DeleteParams{
		OrgSlug:          req.Slug,
		Slug:             req.AppSlug,
		DestroyResources: destroy,
	})
	if err != nil {
		return appsErrToDeleteResp(err), nil
	}
	if res.Enqueued {
		// Re-fetch the row so the response carries the deleting_at
		// marker. Cheap (single-row read) and saves us from
		// returning a stale snapshot from before the marker was set.
		row, err := s.apps.Get(ctx, req.Slug, req.AppSlug)
		if err != nil {
			return nil, err
		}
		return DeleteApp202JSONResponse(appToAPI(row)), nil
	}
	return DeleteApp200Response{}, nil
}

// appsErrToCreateResp maps service-level sentinels to the right 4xx
// for the create endpoint. Anything we don't explicitly handle
// surfaces as a 500 via `return nil, err` to the strict handler.
func appsErrToCreateResp(err error) CreateAppResponseObject {
	switch {
	case errors.Is(err, apps.ErrInvalidName):
		return errResp[CreateAppdefaultJSONResponse](http.StatusBadRequest, "invalid_name", err.Error())
	case errors.Is(err, apps.ErrSlugReserved):
		return errResp[CreateAppdefaultJSONResponse](http.StatusBadRequest, "slug_reserved", err.Error())
	case errors.Is(err, apps.ErrCloudAccountMissing):
		return errResp[CreateAppdefaultJSONResponse](http.StatusBadRequest, "cloud_account_missing", err.Error())
	case errors.Is(err, apps.ErrCloudAccountNotReady):
		return errResp[CreateAppdefaultJSONResponse](http.StatusBadRequest, "cloud_account_not_ready", err.Error())
	case errors.Is(err, apps.ErrInstallationMissing):
		return errResp[CreateAppdefaultJSONResponse](http.StatusBadRequest, "installation_missing", err.Error())
	case errors.Is(err, github.ErrInstallNotFound):
		return errResp[CreateAppdefaultJSONResponse](http.StatusBadRequest, "installation_missing", err.Error())
	case errors.Is(err, github.ErrRepoNotFound):
		return errResp[CreateAppdefaultJSONResponse](http.StatusBadRequest, "repo_not_accessible", err.Error())
	case errors.Is(err, apps.ErrSlugSpaceExhausted):
		return errResp[CreateAppdefaultJSONResponse](http.StatusConflict, "slug_exhausted", err.Error())
	}
	var apiErr *github.APIError
	if errors.As(err, &apiErr) {
		return errResp[CreateAppdefaultJSONResponse](http.StatusBadGateway, "github_error", apiErr.Error())
	}
	// Generic bad-request fallback for the validation-style errors
	// we don't sentinel'd above (e.g. "github_repo_full_name must
	// look like owner/repo"). These come back from the service as
	// plain errors but should still be 4xx — we surface them with
	// the message intact.
	return errResp[CreateAppdefaultJSONResponse](http.StatusBadRequest, "bad_request", err.Error())
}

// appsErrToDeleteResp maps delete-time sentinels.
func appsErrToDeleteResp(err error) DeleteAppResponseObject {
	switch {
	case errors.Is(err, apps.ErrAppNotFound):
		return errResp[DeleteAppdefaultJSONResponse](http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, apps.ErrBuildsRunning):
		return errResp[DeleteAppdefaultJSONResponse](http.StatusConflict, "builds_running", err.Error())
	case errors.Is(err, apps.ErrAlreadyDeleting):
		return errResp[DeleteAppdefaultJSONResponse](http.StatusConflict, "already_deleting", err.Error())
	case errors.Is(err, apps.ErrDestroyNotConfigured):
		return errResp[DeleteAppdefaultJSONResponse](http.StatusServiceUnavailable, "destroy_not_configured", err.Error())
	}
	return errResp[DeleteAppdefaultJSONResponse](http.StatusInternalServerError, "internal_error", err.Error())
}

// appToAPI converts an ent row to the API shape. Every field is
// non-pointer except deleting_at, which is nullable in both shapes.
func appToAPI(row *ent.App) App {
	out := App{
		Id:                   row.ID,
		OrgSlug:              row.OrgSlug,
		Name:                 row.Name,
		Slug:                 row.Slug,
		CloudAccountId:       row.CloudAccountID,
		GithubInstallationId: row.GithubInstallationID,
		GithubRepoFullName:   row.GithubRepoFullName,
		DefaultBranch:        row.DefaultBranch,
		CreatedBy:            row.CreatedBy,
		CreatedAt:            row.CreatedAt,
		UpdatedAt:            row.UpdatedAt,
	}
	if row.DeletingAt != nil {
		out.DeletingAt = row.DeletingAt
	}
	return out
}
