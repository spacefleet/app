// Package github implements just enough of the GitHub App protocol to fetch
// source for the build pipeline: minting App-level JWTs, exchanging them
// for short-lived installation access tokens, and calling the REST API
// using those tokens.
//
// We deliberately avoid pulling in go-github — the surface we need is
// small, the dependency is large, and keeping it minimal makes it easier
// to audit what data we send to GitHub.
package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// DefaultAPIBase is the public GitHub REST API root. Tests override this.
const DefaultAPIBase = "https://api.github.com"

// jwtTTL is how long an App-level JWT stays valid. GitHub caps this at 10
// minutes; we use 9 to give a little clock-skew headroom.
const jwtTTL = 9 * time.Minute

// App carries the credentials needed to act as a GitHub App. Construct via
// NewApp; the parsed RSA key is cached so each JWT mint doesn't re-parse
// the PEM.
type App struct {
	id      int64
	slug    string
	apiBase string
	httpc   *http.Client
	key     *rsa.PrivateKey

	mu     sync.Mutex
	jwt    string
	jwtExp time.Time
}

// NewApp parses the App's PEM-encoded RSA private key and returns a ready
// client. id and slug must both be non-zero — slug is required to build
// install URLs even though the API itself only needs id.
func NewApp(id int64, slug string, privateKeyPEM []byte) (*App, error) {
	if id == 0 {
		return nil, errors.New("github app: id required")
	}
	if slug == "" {
		return nil, errors.New("github app: slug required")
	}
	key, err := parseRSAPrivateKey(privateKeyPEM)
	if err != nil {
		return nil, err
	}
	return &App{
		id:      id,
		slug:    slug,
		apiBase: DefaultAPIBase,
		httpc:   &http.Client{Timeout: 15 * time.Second},
		key:     key,
	}, nil
}

// Slug returns the GitHub App slug — needed by the install-URL builder.
func (a *App) Slug() string { return a.slug }

// SetAPIBase overrides the base URL for tests pointing at a fake server.
func (a *App) SetAPIBase(base string) { a.apiBase = base }

// jwtToken returns a cached App JWT, minting a fresh one when within 30s
// of expiry. Caching matters because every API call burns one mint
// otherwise, and JWT signing is the only meaningfully CPU-bound op here.
func (a *App) jwtToken() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.jwt != "" && time.Until(a.jwtExp) > 30*time.Second {
		return a.jwt, nil
	}
	now := time.Now()
	exp := now.Add(jwtTTL)
	tok, err := signAppJWT(a.id, now, exp, a.key)
	if err != nil {
		return "", err
	}
	a.jwt = tok
	a.jwtExp = exp
	return tok, nil
}

// signAppJWT mints an RS256-signed JWT for App-level auth. Claims are the
// minimal trio GitHub requires (iat, exp, iss=app id).
func signAppJWT(appID int64, iat, exp time.Time, key *rsa.PrivateKey) (string, error) {
	header := []byte(`{"alg":"RS256","typ":"JWT"}`)
	payload, err := json.Marshal(map[string]any{
		"iat": iat.Unix() - 30, // tolerate small clock skew on github's side
		"exp": exp.Unix(),
		"iss": strconv.FormatInt(appID, 10),
	})
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(payload)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("sign jwt: %w", err)
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// parseRSAPrivateKey accepts either PKCS#1 ("RSA PRIVATE KEY") or PKCS#8
// ("PRIVATE KEY") PEM blocks. GitHub generates PKCS#1 by default but some
// tooling re-encodes to PKCS#8.
func parseRSAPrivateKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("github app: private key not PEM-encoded")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("github app: PKCS#8 key is not RSA")
		}
		return rsaKey, nil
	default:
		return nil, fmt.Errorf("github app: unsupported PEM type %q", block.Type)
	}
}

// Installation is the minimal projection of GitHub's installation object —
// the fields we persist when an install completes.
type Installation struct {
	ID        int64
	Account   Account
	Suspended bool
}

// Account identifies the GitHub user or organization that owns an install.
type Account struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}

type installationResp struct {
	ID          int64      `json:"id"`
	Account     Account    `json:"account"`
	SuspendedAt *time.Time `json:"suspended_at"`
}

// DeleteInstallation uninstalls the App from the target account on GitHub.
// 404 is treated as success — already gone is the same end state. Any
// other non-2xx surfaces as APIError so callers can decide.
func (a *App) DeleteInstallation(ctx context.Context, installationID int64) error {
	path := fmt.Sprintf("/app/installations/%d", installationID)
	err := a.doApp(ctx, http.MethodDelete, path, nil, nil)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return nil
		}
		return err
	}
	return nil
}

// GetInstallation fetches a single installation by ID using App-level auth.
// We call this right after a successful install handshake to discover the
// account that owns the installation (login + type).
func (a *App) GetInstallation(ctx context.Context, installationID int64) (*Installation, error) {
	var resp installationResp
	path := fmt.Sprintf("/app/installations/%d", installationID)
	if err := a.doApp(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	return &Installation{
		ID:        resp.ID,
		Account:   resp.Account,
		Suspended: resp.SuspendedAt != nil,
	}, nil
}

// AccessToken is the short-lived credential used by builders (and our own
// API calls below) to clone source and read repo metadata.
type AccessToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// InstallationToken exchanges an App JWT for a 1h installation access
// token. The token grants the union of the permissions the App was
// granted at install time, scoped to that one installation's repos.
func (a *App) InstallationToken(ctx context.Context, installationID int64) (*AccessToken, error) {
	var tok AccessToken
	path := fmt.Sprintf("/app/installations/%d/access_tokens", installationID)
	if err := a.doApp(ctx, http.MethodPost, path, nil, &tok); err != nil {
		return nil, err
	}
	return &tok, nil
}

// Repository is the subset of the repo object we surface to callers. The
// full GitHub object has ~80 fields; we keep only what the UI and builder
// actually need.
type Repository struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
	HTMLURL       string `json:"html_url"`
}

type listReposResp struct {
	TotalCount   int          `json:"total_count"`
	Repositories []Repository `json:"repositories"`
}

// ListInstallationRepositories enumerates every repo the install can see.
// Pages until exhausted; GitHub caps at 100 per page so a 1000-repo
// install costs 10 round trips. Acceptable for the v1 "show me the repos
// I can build" UI; we'll revisit if it's a real bottleneck.
func (a *App) ListInstallationRepositories(ctx context.Context, installationID int64) ([]Repository, error) {
	tok, err := a.InstallationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}
	const perPage = 100
	var out []Repository
	for page := 1; ; page++ {
		q := url.Values{}
		q.Set("per_page", strconv.Itoa(perPage))
		q.Set("page", strconv.Itoa(page))
		var resp listReposResp
		if err := a.doToken(ctx, tok.Token, http.MethodGet, "/installation/repositories?"+q.Encode(), nil, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Repositories...)
		if len(resp.Repositories) < perPage {
			return out, nil
		}
	}
}

// GetInstallationRepository fetches a single repository accessible to the
// named installation. We use the installation token rather than App-JWT
// auth so a successful response proves the installation grants the App
// access to this repo — the same property ListInstallationRepositories
// has, but in O(1) instead of O(repos/100).
//
// Returns ErrInstallNotFound when GitHub responds 404 (either the repo
// doesn't exist, or it isn't in this installation — both look the same
// from outside, and that's exactly the failure mode the apps service
// needs to refuse the create).
func (a *App) GetInstallationRepository(ctx context.Context, installationID int64, fullName string) (*Repository, error) {
	tok, err := a.InstallationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}
	var repo Repository
	path := "/repos/" + fullName
	if err := a.doToken(ctx, tok.Token, http.MethodGet, path, nil, &repo); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return nil, ErrRepoNotFound
		}
		return nil, err
	}
	return &repo, nil
}

// ErrRepoNotFound is returned when a single-repo lookup fails because
// either the repo doesn't exist or the installation can't see it.
var ErrRepoNotFound = errors.New("repository not found or not accessible to installation")

// ErrRefNotFound is returned when a ref (branch, tag, or SHA) doesn't
// resolve. Callers map this to a 4xx — the user gave us something the
// repo doesn't contain.
var ErrRefNotFound = errors.New("ref not found in repository")

// ResolveCommit takes whatever the user typed — a branch name, a tag,
// a short or long SHA — and returns the full 40-char commit SHA. Uses
// GitHub's commits endpoint, which accepts any valid ref shape.
//
// We use GET /repos/{owner}/{repo}/commits/{ref} (not /git/refs) because
// it handles all four shapes in one call: a long SHA echoes itself, a
// short SHA expands, a branch resolves to its tip, a tag resolves to
// the commit it points to (annotated or lightweight).
func (a *App) ResolveCommit(ctx context.Context, installationID int64, fullName, ref string) (string, error) {
	if ref == "" {
		return "", errors.New("github: ref required")
	}
	tok, err := a.InstallationToken(ctx, installationID)
	if err != nil {
		return "", err
	}
	var resp struct {
		SHA string `json:"sha"`
	}
	path := "/repos/" + fullName + "/commits/" + url.PathEscape(ref)
	if err := a.doToken(ctx, tok.Token, http.MethodGet, path, nil, &resp); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			if apiErr.Status == http.StatusNotFound || apiErr.Status == http.StatusUnprocessableEntity {
				return "", ErrRefNotFound
			}
		}
		return "", err
	}
	if resp.SHA == "" {
		return "", ErrRefNotFound
	}
	return resp.SHA, nil
}

// InstallURL returns the user-facing URL where a customer installs the App
// onto repos. The state is round-tripped back to the setup callback.
func (a *App) InstallURL(state string) string {
	q := url.Values{}
	q.Set("state", state)
	return fmt.Sprintf("https://github.com/apps/%s/installations/new?%s", url.PathEscape(a.slug), q.Encode())
}

// doApp issues an App-JWT-authenticated request. Used for the few endpoints
// that don't take an installation token (GET /app/installations/{id},
// POST /app/installations/{id}/access_tokens).
func (a *App) doApp(ctx context.Context, method, path string, body, out any) error {
	tok, err := a.jwtToken()
	if err != nil {
		return err
	}
	return a.do(ctx, method, path, "Bearer "+tok, body, out)
}

// doToken issues a request authenticated with an installation access
// token — i.e., scoped to one install.
func (a *App) doToken(ctx context.Context, token, method, path string, body, out any) error {
	return a.do(ctx, method, path, "token "+token, body, out)
}

// do is the shared HTTP plumbing. We surface non-2xx as an APIError that
// carries the GitHub-supplied message, so callers can render something
// useful instead of a bare 500.
func (a *App) do(ctx context.Context, method, path, auth string, body, out any) error {
	var bodyReader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.apiBase+path, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "spacefleet")
	req.Header.Set("Authorization", auth)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := a.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return parseAPIError(resp.StatusCode, raw)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// APIError captures GitHub's error envelope so handlers can decide what
// status to surface. Status holds the raw HTTP code from GitHub.
type APIError struct {
	Status  int
	Message string
	Body    string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("github api %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("github api %d", e.Status)
}

func parseAPIError(status int, body []byte) error {
	var env struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &env)
	return &APIError{Status: status, Message: env.Message, Body: string(body)}
}
