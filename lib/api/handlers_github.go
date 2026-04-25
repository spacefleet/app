package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/spacefleet/app/ent"
	"github.com/spacefleet/app/lib/auth"
	"github.com/spacefleet/app/lib/github"
)

// StartGithubInstall hands the SPA the URL to send the user to. Clerk-only
// because the install belongs to a person — a CLI session has no UI to
// click through with.
func (s *Server) StartGithubInstall(ctx context.Context, req StartGithubInstallRequestObject) (StartGithubInstallResponseObject, error) {
	sess, ok := auth.FromContext(ctx)
	if !ok {
		return errResp[StartGithubInstalldefaultJSONResponse](http.StatusUnauthorized, "unauthorized", "missing session"), nil
	}
	if sess.Source != auth.SourceClerk {
		return errResp[StartGithubInstalldefaultJSONResponse](http.StatusForbidden, "forbidden", "browser session required"), nil
	}
	if s.gh == nil {
		return errResp[StartGithubInstalldefaultJSONResponse](http.StatusServiceUnavailable, "github_not_configured", "github app is not configured on this server"), nil
	}
	state, err := s.gh.CreateInstallState(ctx, req.Slug, sess.UserID)
	if err != nil {
		return nil, err
	}
	return StartGithubInstall200JSONResponse{Url: s.gh.App().InstallURL(state)}, nil
}

// CompleteGithubInstall is called by the SPA setup-callback page. The user
// must be the same one who started the install — a stolen state in a
// different browser session is rejected.
func (s *Server) CompleteGithubInstall(ctx context.Context, req CompleteGithubInstallRequestObject) (CompleteGithubInstallResponseObject, error) {
	sess, ok := auth.FromContext(ctx)
	if !ok {
		return errResp[CompleteGithubInstalldefaultJSONResponse](http.StatusUnauthorized, "unauthorized", "missing session"), nil
	}
	if sess.Source != auth.SourceClerk {
		return errResp[CompleteGithubInstalldefaultJSONResponse](http.StatusForbidden, "forbidden", "browser session required"), nil
	}
	if s.gh == nil {
		return errResp[CompleteGithubInstalldefaultJSONResponse](http.StatusServiceUnavailable, "github_not_configured", "github app is not configured on this server"), nil
	}
	if req.Body == nil || req.Body.State == "" || req.Body.InstallationId == 0 {
		return errResp[CompleteGithubInstalldefaultJSONResponse](http.StatusBadRequest, "bad_request", "state and installation_id required"), nil
	}
	row, err := s.gh.CompleteInstall(ctx, req.Body.State, sess.UserID, req.Body.InstallationId)
	if err != nil {
		switch {
		case errors.Is(err, github.ErrInvalidState):
			return errResp[CompleteGithubInstalldefaultJSONResponse](http.StatusUnauthorized, "invalid_state", err.Error()), nil
		case errors.Is(err, github.ErrStateExpired):
			return errResp[CompleteGithubInstalldefaultJSONResponse](http.StatusUnauthorized, "state_expired", err.Error()), nil
		case errors.Is(err, github.ErrStateConsumed):
			return errResp[CompleteGithubInstalldefaultJSONResponse](http.StatusConflict, "state_used", err.Error()), nil
		case errors.Is(err, github.ErrStateUserMismatch):
			return errResp[CompleteGithubInstalldefaultJSONResponse](http.StatusForbidden, "state_user_mismatch", err.Error()), nil
		}
		var apiErr *github.APIError
		if errors.As(err, &apiErr) {
			return errResp[CompleteGithubInstalldefaultJSONResponse](http.StatusBadGateway, "github_error", apiErr.Error()), nil
		}
		return nil, err
	}
	return CompleteGithubInstall200JSONResponse{
		OrgSlug:      row.OrgSlug,
		Installation: installationToAPI(row),
	}, nil
}

// ListGithubInstallations returns everything connected to the org. Empty
// list is fine — the UI shows a "connect" CTA in that case.
func (s *Server) ListGithubInstallations(ctx context.Context, req ListGithubInstallationsRequestObject) (ListGithubInstallationsResponseObject, error) {
	if s.gh == nil {
		return errResp[ListGithubInstallationsdefaultJSONResponse](http.StatusServiceUnavailable, "github_not_configured", "github app is not configured on this server"), nil
	}
	rows, err := s.gh.ListInstallations(ctx, req.Slug)
	if err != nil {
		return nil, err
	}
	out := make([]GithubInstallation, 0, len(rows))
	for _, r := range rows {
		out = append(out, installationToAPI(r))
	}
	return ListGithubInstallations200JSONResponse{Installations: out}, nil
}

// DeleteGithubInstallation uninstalls the App on GitHub and drops our
// row. Idempotent — a missing row or a 404 from GitHub both return 204,
// since the end state is the same and clients shouldn't have to
// distinguish. GitHub failures other than 404 surface as 502 so users
// see a real error instead of an opaque 500.
func (s *Server) DeleteGithubInstallation(ctx context.Context, req DeleteGithubInstallationRequestObject) (DeleteGithubInstallationResponseObject, error) {
	if s.gh == nil {
		return errResp[DeleteGithubInstallationdefaultJSONResponse](http.StatusServiceUnavailable, "github_not_configured", "github app is not configured on this server"), nil
	}
	err := s.gh.DeleteInstallation(ctx, req.Slug, req.InstallationId)
	if err == nil || errors.Is(err, github.ErrInstallNotFound) {
		return DeleteGithubInstallation204Response{}, nil
	}
	var apiErr *github.APIError
	if errors.As(err, &apiErr) {
		return errResp[DeleteGithubInstallationdefaultJSONResponse](http.StatusBadGateway, "github_error", apiErr.Error()), nil
	}
	return nil, err
}

// ListGithubInstallationRepositories proves the auth chain — fetches a
// fresh installation token internally and returns the visible repos.
func (s *Server) ListGithubInstallationRepositories(ctx context.Context, req ListGithubInstallationRepositoriesRequestObject) (ListGithubInstallationRepositoriesResponseObject, error) {
	if s.gh == nil {
		return errResp[ListGithubInstallationRepositoriesdefaultJSONResponse](http.StatusServiceUnavailable, "github_not_configured", "github app is not configured on this server"), nil
	}
	repos, err := s.gh.ListRepositories(ctx, req.Slug, req.InstallationId)
	if err != nil {
		if errors.Is(err, github.ErrInstallNotFound) {
			return errResp[ListGithubInstallationRepositoriesdefaultJSONResponse](http.StatusNotFound, "not_found", "installation not found"), nil
		}
		var apiErr *github.APIError
		if errors.As(err, &apiErr) {
			return errResp[ListGithubInstallationRepositoriesdefaultJSONResponse](http.StatusBadGateway, "github_error", apiErr.Error()), nil
		}
		return nil, err
	}
	out := make([]GithubRepository, 0, len(repos))
	for _, r := range repos {
		out = append(out, GithubRepository{
			Id:            r.ID,
			Name:          r.Name,
			FullName:      r.FullName,
			Private:       r.Private,
			DefaultBranch: r.DefaultBranch,
			HtmlUrl:       r.HTMLURL,
		})
	}
	return ListGithubInstallationRepositories200JSONResponse{Repositories: out}, nil
}

func installationToAPI(row *ent.GithubInstallation) GithubInstallation {
	return GithubInstallation{
		Id:             row.ID,
		InstallationId: row.InstallationID,
		AccountLogin:   row.AccountLogin,
		AccountType:    row.AccountType,
		AccountId:      row.AccountID,
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
		SuspendedAt:    row.SuspendedAt,
	}
}
