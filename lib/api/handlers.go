package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	awsint "github.com/spacefleet/app/lib/aws"
	"github.com/spacefleet/app/lib/auth"
	"github.com/spacefleet/app/lib/cli"
	"github.com/spacefleet/app/lib/github"
)

type Server struct {
	cli *cli.Service
	gh  *github.Service
	aws *awsint.Service
}

// NewServer accepts the runtime services this API depends on. Any of
// them may be nil — features whose service is missing return a clear
// "not configured" error instead of panicking, which keeps route-level
// tests usable without a database, GitHub App, or AWS credentials.
func NewServer(cliSvc *cli.Service, ghSvc *github.Service, awsSvc *awsint.Service) *Server {
	return &Server{cli: cliSvc, gh: ghSvc, aws: awsSvc}
}

var _ StrictServerInterface = (*Server)(nil)

func (s *Server) GetHealth(_ context.Context, _ GetHealthRequestObject) (GetHealthResponseObject, error) {
	return GetHealth200JSONResponse{Status: Ok}, nil
}

func (s *Server) GetPing(_ context.Context, req GetPingRequestObject) (GetPingResponseObject, error) {
	name := "world"
	if req.Params.Name != nil && *req.Params.Name != "" {
		name = *req.Params.Name
	}
	return GetPing200JSONResponse{Message: fmt.Sprintf("hello, %s", name)}, nil
}

// ApproveCliAuth records the browser-confirmed approval and returns a
// one-time exchange code for the CLI's localhost callback. Clerk-only: we
// never let a CLI token be used to mint another CLI token.
func (s *Server) ApproveCliAuth(ctx context.Context, req ApproveCliAuthRequestObject) (ApproveCliAuthResponseObject, error) {
	sess, ok := auth.FromContext(ctx)
	if !ok {
		return errResp[ApproveCliAuthdefaultJSONResponse](http.StatusUnauthorized, "unauthorized", "missing session"), nil
	}
	if sess.Source != auth.SourceClerk {
		return errResp[ApproveCliAuthdefaultJSONResponse](http.StatusForbidden, "forbidden", "browser session required"), nil
	}
	if req.Body == nil || req.Body.Name == "" || req.Body.Challenge == "" {
		return errResp[ApproveCliAuthdefaultJSONResponse](http.StatusBadRequest, "bad_request", "name and challenge required"), nil
	}

	code, err := s.cli.CreateCode(ctx, sess.UserID, req.Body.Name, req.Body.Challenge)
	if err != nil {
		return nil, err
	}
	return ApproveCliAuth200JSONResponse{Code: code}, nil
}

// ExchangeCliAuth is the public CLI-side completion of the approval flow.
// It consumes the one-time code, verifies the PKCE proof, and issues the
// actual bearer token — which is returned in plaintext exactly once.
func (s *Server) ExchangeCliAuth(ctx context.Context, req ExchangeCliAuthRequestObject) (ExchangeCliAuthResponseObject, error) {
	if req.Body == nil || req.Body.Code == "" || req.Body.Verifier == "" {
		return errResp[ExchangeCliAuthdefaultJSONResponse](http.StatusBadRequest, "bad_request", "code and verifier required"), nil
	}
	record, err := s.cli.ConsumeCode(ctx, req.Body.Code, req.Body.Verifier)
	if err != nil {
		if isCliUserError(err) {
			return errResp[ExchangeCliAuthdefaultJSONResponse](http.StatusUnauthorized, "invalid_code", err.Error()), nil
		}
		return nil, err
	}
	plaintext, token, err := s.cli.IssueToken(ctx, record.UserID, record.Name)
	if err != nil {
		return nil, err
	}
	return ExchangeCliAuth200JSONResponse{
		Token:     plaintext,
		Name:      token.Name,
		ExpiresAt: token.ExpiresAt,
	}, nil
}

// ListCliTokens returns the caller's tokens — metadata only, no plaintext.
// Works for either Clerk or CLI sessions; a CLI can inspect its own tokens.
func (s *Server) ListCliTokens(ctx context.Context, _ ListCliTokensRequestObject) (ListCliTokensResponseObject, error) {
	sess, ok := auth.FromContext(ctx)
	if !ok {
		return errResp[ListCliTokensdefaultJSONResponse](http.StatusUnauthorized, "unauthorized", "missing session"), nil
	}
	rows, err := s.cli.ListTokens(ctx, sess.UserID)
	if err != nil {
		return nil, err
	}
	out := make([]CliToken, 0, len(rows))
	for _, t := range rows {
		out = append(out, CliToken{
			Id:         t.ID,
			Name:       t.Name,
			CreatedAt:  t.CreatedAt,
			ExpiresAt:  t.ExpiresAt,
			LastUsedAt: t.LastUsedAt,
			RevokedAt:  t.RevokedAt,
		})
	}
	return ListCliTokens200JSONResponse{Tokens: out}, nil
}

// RevokeCliToken marks one of the caller's tokens revoked. Idempotent.
func (s *Server) RevokeCliToken(ctx context.Context, req RevokeCliTokenRequestObject) (RevokeCliTokenResponseObject, error) {
	sess, ok := auth.FromContext(ctx)
	if !ok {
		return errResp[RevokeCliTokendefaultJSONResponse](http.StatusUnauthorized, "unauthorized", "missing session"), nil
	}
	if err := s.cli.RevokeToken(ctx, sess.UserID, uuid.UUID(req.Id)); err != nil {
		if errors.Is(err, cli.ErrInvalidToken) {
			return errResp[RevokeCliTokendefaultJSONResponse](http.StatusNotFound, "not_found", "token not found"), nil
		}
		return nil, err
	}
	return RevokeCliToken204Response{}, nil
}

// CliWhoami echoes the current session — a tiny smoke-test endpoint for
// both the browser and the CLI.
func (s *Server) CliWhoami(ctx context.Context, _ CliWhoamiRequestObject) (CliWhoamiResponseObject, error) {
	sess, ok := auth.FromContext(ctx)
	if !ok {
		return errResp[CliWhoamidefaultJSONResponse](http.StatusUnauthorized, "unauthorized", "missing session"), nil
	}
	return CliWhoami200JSONResponse{
		UserId: sess.UserID,
		Source: CliWhoamiSource(sess.Source),
	}, nil
}

// isCliUserError returns true when the error is a user-facing "bad input"
// condition rather than an internal failure — so we surface 4xx instead of
// 5xx to the caller.
func isCliUserError(err error) bool {
	switch {
	case errors.Is(err, cli.ErrInvalidCode),
		errors.Is(err, cli.ErrCodeExpired),
		errors.Is(err, cli.ErrCodeConsumed),
		errors.Is(err, cli.ErrPKCEMismatch),
		errors.Is(err, cli.ErrInvalidToken),
		errors.Is(err, cli.ErrTokenExpired),
		errors.Is(err, cli.ErrTokenRevoked):
		return true
	}
	return false
}

// errResp is a small generic helper so each handler can return its specific
// typed default response without repeating the struct literal. T is bound
// to the per-operation `*defaultJSONResponse` struct.
type defaultResp interface {
	~struct {
		Body       Error
		StatusCode int
	}
}

func errResp[T defaultResp](status int, code, msg string) T {
	return T{Body: Error{Code: code, Message: msg}, StatusCode: status}
}
