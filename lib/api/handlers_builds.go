package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/spacefleet/app/ent"
	"github.com/spacefleet/app/lib/auth"
	"github.com/spacefleet/app/lib/builds"
)

// CreateBuild starts a new build for an app. Clerk-only — the build
// list page is browser-driven. (CLI builds will land later, gated by
// the same per-app concurrency rules but a CLI auth path.)
func (s *Server) CreateBuild(ctx context.Context, req CreateBuildRequestObject) (CreateBuildResponseObject, error) {
	sess, ok := auth.FromContext(ctx)
	if !ok {
		return errResp[CreateBuilddefaultJSONResponse](http.StatusUnauthorized, "unauthorized", "missing session"), nil
	}
	if sess.Source != auth.SourceClerk {
		return errResp[CreateBuilddefaultJSONResponse](http.StatusForbidden, "forbidden", "browser session required"), nil
	}
	if s.builds == nil {
		return errResp[CreateBuilddefaultJSONResponse](http.StatusServiceUnavailable, "builds_not_configured", "build pipeline is not configured on this server"), nil
	}

	ref := ""
	if req.Body != nil && req.Body.Ref != nil {
		ref = *req.Body.Ref
	}

	row, err := s.builds.Create(ctx, builds.CreateParams{
		OrgSlug:   req.Slug,
		AppSlug:   req.AppSlug,
		Ref:       ref,
		CreatedBy: sess.UserID,
	})
	if err != nil {
		return buildsErrToCreateResp(err), nil
	}
	return CreateBuild202JSONResponse(buildToAPI(row)), nil
}

// ListBuilds returns the builds for an app, newest first.
func (s *Server) ListBuilds(ctx context.Context, req ListBuildsRequestObject) (ListBuildsResponseObject, error) {
	if s.builds == nil {
		return errResp[ListBuildsdefaultJSONResponse](http.StatusServiceUnavailable, "builds_not_configured", "build pipeline is not configured on this server"), nil
	}
	rows, err := s.builds.List(ctx, req.Slug, req.AppSlug)
	if err != nil {
		if errors.Is(err, builds.ErrAppNotFound) {
			return errResp[ListBuildsdefaultJSONResponse](http.StatusNotFound, "not_found", err.Error()), nil
		}
		return nil, err
	}
	out := make([]Build, 0, len(rows))
	for _, r := range rows {
		out = append(out, buildToAPI(r))
	}
	return ListBuilds200JSONResponse{Builds: out}, nil
}

// GetBuildLogs returns one page of CloudWatch log events for a build.
// The UI polls this with the previous response's `next_token` until
// `has_more` is false and the build is terminal. We deliberately don't
// require a Clerk session here for any reason beyond the standard
// `RequireAuth` + `RequireOrg` middleware that wraps every /api/orgs/*
// route — a CLI session can read logs the same as the browser.
func (s *Server) GetBuildLogs(ctx context.Context, req GetBuildLogsRequestObject) (GetBuildLogsResponseObject, error) {
	if s.buildLog == nil {
		return errResp[GetBuildLogsdefaultJSONResponse](http.StatusServiceUnavailable, "logs_not_configured", "build log streaming is not configured on this server"), nil
	}

	params := builds.FetchParams{
		OrgSlug: req.Slug,
		AppSlug: req.AppSlug,
		BuildID: uuid.UUID(req.BuildId),
	}
	if req.Params.After != nil {
		params.NextToken = *req.Params.After
	}
	if req.Params.Limit != nil {
		params.Limit = *req.Params.Limit
	}

	res, err := s.buildLog.Fetch(ctx, params)
	if err != nil {
		switch {
		case errors.Is(err, builds.ErrAppNotFound):
			return errResp[GetBuildLogsdefaultJSONResponse](http.StatusNotFound, "not_found", "app not found"), nil
		case errors.Is(err, builds.ErrBuildNotFound):
			return errResp[GetBuildLogsdefaultJSONResponse](http.StatusNotFound, "not_found", "build not found"), nil
		}
		return nil, err
	}

	out := GetBuildLogs200JSONResponse{
		Events:        make([]BuildLogEvent, 0, len(res.Events)),
		HasMore:       res.HasMore,
		BuildTerminal: res.BuildTerminal,
	}
	for _, e := range res.Events {
		out.Events = append(out.Events, BuildLogEvent{
			Timestamp: e.Timestamp,
			Message:   e.Message,
		})
	}
	if res.NextToken != "" {
		v := res.NextToken
		out.NextToken = &v
	}
	return out, nil
}

// GetBuild returns one build's full state including the stages timeline.
// The UI polls this every couple of seconds while the build is non-
// terminal.
func (s *Server) GetBuild(ctx context.Context, req GetBuildRequestObject) (GetBuildResponseObject, error) {
	if s.builds == nil {
		return errResp[GetBuilddefaultJSONResponse](http.StatusServiceUnavailable, "builds_not_configured", "build pipeline is not configured on this server"), nil
	}
	row, err := s.builds.Get(ctx, req.Slug, req.AppSlug, uuid.UUID(req.BuildId))
	if err != nil {
		switch {
		case errors.Is(err, builds.ErrAppNotFound):
			return errResp[GetBuilddefaultJSONResponse](http.StatusNotFound, "not_found", "app not found"), nil
		case errors.Is(err, builds.ErrBuildNotFound):
			return errResp[GetBuilddefaultJSONResponse](http.StatusNotFound, "not_found", "build not found"), nil
		}
		return nil, err
	}
	return GetBuild200JSONResponse(buildToAPI(row)), nil
}

// buildsErrToCreateResp maps service-level errors to specific 4xx
// responses. Anything we don't sentinel falls through as 500.
func buildsErrToCreateResp(err error) CreateBuildResponseObject {
	switch {
	case errors.Is(err, builds.ErrAppNotFound):
		return errResp[CreateBuilddefaultJSONResponse](http.StatusNotFound, "app_not_found", err.Error())
	case errors.Is(err, builds.ErrAppDeleting):
		return errResp[CreateBuilddefaultJSONResponse](http.StatusConflict, "app_deleting", err.Error())
	case errors.Is(err, builds.ErrRefNotResolvable):
		return errResp[CreateBuilddefaultJSONResponse](http.StatusBadRequest, "ref_not_found", err.Error())
	case errors.Is(err, builds.ErrGitHubNotConfigured):
		return errResp[CreateBuilddefaultJSONResponse](http.StatusServiceUnavailable, "github_not_configured", err.Error())
	}
	return errResp[CreateBuilddefaultJSONResponse](http.StatusInternalServerError, "internal_error", err.Error())
}

// buildToAPI converts an ent row to the API shape. Stages get serialized
// as their wire-shape array.
func buildToAPI(row *ent.Build) Build {
	out := Build{
		Id:        row.ID,
		AppId:     row.AppID,
		SourceRef: row.SourceRef,
		SourceSha: row.SourceSha,
		Status:    BuildStatus(row.Status),
		Stages:    stagesToAPI(row),
		CreatedBy: row.CreatedBy,
		CreatedAt: row.CreatedAt,
	}
	if row.ImageURI != "" {
		v := row.ImageURI
		out.ImageUri = &v
	}
	if row.ImageDigest != "" {
		v := row.ImageDigest
		out.ImageDigest = &v
	}
	if row.FargateTaskArn != "" {
		v := row.FargateTaskArn
		out.FargateTaskArn = &v
	}
	if row.LogGroup != "" {
		v := row.LogGroup
		out.LogGroup = &v
	}
	if row.LogStream != "" {
		v := row.LogStream
		out.LogStream = &v
	}
	if row.ErrorMessage != "" {
		v := row.ErrorMessage
		out.ErrorMessage = &v
	}
	if row.StartedAt != nil {
		out.StartedAt = row.StartedAt
	}
	if row.EndedAt != nil {
		out.EndedAt = row.EndedAt
	}
	return out
}

func stagesToAPI(row *ent.Build) []BuildStage {
	out := make([]BuildStage, 0, len(row.Stages))
	for _, ev := range row.Stages {
		s := BuildStage{
			Name:   ev.Name,
			Status: BuildStageStatus(ev.Status),
			At:     ev.At,
		}
		if len(ev.Data) > 0 {
			data := make(map[string]interface{}, len(ev.Data))
			for k, v := range ev.Data {
				data[k] = v
			}
			s.Data = &data
		}
		out = append(out, s)
	}
	return out
}
