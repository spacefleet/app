package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der})
}

func TestNewAppRequiresIDAndSlug(t *testing.T) {
	pem := testKeyPEM(t)
	if _, err := NewApp(0, "slug", pem); err == nil {
		t.Fatal("expected error for missing id")
	}
	if _, err := NewApp(123, "", pem); err == nil {
		t.Fatal("expected error for missing slug")
	}
	if _, err := NewApp(123, "slug", []byte("not a key")); err == nil {
		t.Fatal("expected error for malformed pem")
	}
}

// TestSignedJWTVerifies confirms the JWT we mint has correct shape, claims,
// and a signature that verifies under the matching public key.
func TestSignedJWTVerifies(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	now := time.Now()
	exp := now.Add(5 * time.Minute)
	tok, err := signAppJWT(42, now, exp, key)
	if err != nil {
		t.Fatalf("signAppJWT: %v", err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 jwt parts, got %d", len(parts))
	}
	signing := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	sum := sha256.Sum256([]byte(signing))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("verify signature: %v", err)
	}

	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	if claims.Iss != "42" {
		t.Errorf("iss = %q, want %q", claims.Iss, "42")
	}
	if claims.Exp != exp.Unix() {
		t.Errorf("exp = %d, want %d", claims.Exp, exp.Unix())
	}
	if claims.Iat > now.Unix() {
		t.Errorf("iat = %d, expected <= %d (skew tolerance subtracted)", claims.Iat, now.Unix())
	}
}

func TestInstallURLEmbedsState(t *testing.T) {
	app, err := NewApp(99, "spacefleet-test", testKeyPEM(t))
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	url := app.InstallURL("abc-state")
	wantPrefix := "https://github.com/apps/spacefleet-test/installations/new?state=abc-state"
	if url != wantPrefix {
		t.Errorf("InstallURL = %q, want %q", url, wantPrefix)
	}
}

// TestInstallationTokenExchangeUsesAppJWT runs the App against a fake API
// server and verifies that the access-tokens call presents a Bearer JWT
// (App-level auth), not an installation token.
func TestInstallationTokenExchangeUsesAppJWT(t *testing.T) {
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations/77/access_tokens" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method %s", r.Method)
		}
		sawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"ghs_abc","expires_at":"2030-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	app, err := NewApp(7, "slug", testKeyPEM(t))
	if err != nil {
		t.Fatal(err)
	}
	app.SetAPIBase(srv.URL)

	tok, err := app.InstallationToken(context.Background(), 77)
	if err != nil {
		t.Fatalf("InstallationToken: %v", err)
	}
	if tok.Token != "ghs_abc" {
		t.Errorf("token = %q", tok.Token)
	}
	if !strings.HasPrefix(sawAuth, "Bearer ") {
		t.Errorf("Authorization = %q, want Bearer prefix", sawAuth)
	}
	// The bearer payload is the App's JWT — decoded iss should match the
	// app id, not the installation id.
	parts := strings.SplitN(strings.TrimPrefix(sawAuth, "Bearer "), ".", 3)
	if len(parts) != 3 {
		t.Fatalf("malformed bearer: %q", sawAuth)
	}
	body, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Iss string `json:"iss"`
	}
	_ = json.Unmarshal(body, &claims)
	if claims.Iss != strconv.Itoa(7) {
		t.Errorf("issuer = %q, want %q", claims.Iss, "7")
	}
}

// TestDeleteInstallationTreatsMissingAsSuccess locks in the contract that
// a missing install on GitHub doesn't propagate as an error — same end
// state as a successful delete, so callers can be idempotent.
func TestDeleteInstallationTreatsMissingAsSuccess(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   bool // wantErr
	}{
		{"deleted", http.StatusNoContent, false},
		{"already gone", http.StatusNotFound, false},
		{"forbidden", http.StatusForbidden, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete {
					t.Errorf("method = %s, want DELETE", r.Method)
				}
				if r.URL.Path != "/app/installations/55" {
					t.Errorf("path = %s", r.URL.Path)
				}
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			app, _ := NewApp(1, "s", testKeyPEM(t))
			app.SetAPIBase(srv.URL)
			err := app.DeleteInstallation(context.Background(), 55)
			if (err != nil) != tc.want {
				t.Errorf("err = %v, wantErr = %v", err, tc.want)
			}
		})
	}
}

// TestListInstallationRepositoriesPaginates exercises the paging loop
// against a fake server that returns two pages.
func TestListInstallationRepositoriesPaginates(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/1/access_tokens":
			_, _ = w.Write([]byte(`{"token":"ghs","expires_at":"2030-01-01T00:00:00Z"}`))
		case "/installation/repositories":
			calls++
			page := r.URL.Query().Get("page")
			if r.Header.Get("Authorization") != "token ghs" {
				t.Errorf("expected installation-token auth, got %q", r.Header.Get("Authorization"))
			}
			switch page {
			case "1":
				// Full page — caller must request another.
				w.Write([]byte(makeRepoPage(100)))
			case "2":
				// Partial page — caller stops.
				w.Write([]byte(makeRepoPage(3)))
			default:
				t.Errorf("unexpected page %q", page)
			}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	app, _ := NewApp(1, "s", testKeyPEM(t))
	app.SetAPIBase(srv.URL)

	repos, err := app.ListInstallationRepositories(context.Background(), 1)
	if err != nil {
		t.Fatalf("ListInstallationRepositories: %v", err)
	}
	if len(repos) != 103 {
		t.Errorf("got %d repos, want 103", len(repos))
	}
	if calls != 2 {
		t.Errorf("got %d page calls, want 2", calls)
	}
}

func makeRepoPage(n int) string {
	var b strings.Builder
	b.WriteString(`{"total_count":` + strconv.Itoa(n) + `,"repositories":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"id":` + strconv.Itoa(i) + `,"name":"r","full_name":"o/r","private":true,"default_branch":"main","html_url":"https://github.com/o/r"}`)
	}
	b.WriteString("]}")
	return b.String()
}
