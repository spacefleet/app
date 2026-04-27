// Tests for builder/entrypoint.sh.
//
// This is a black-box driver that runs the real shell script with
// every external command (aws, git, kaniko) replaced by a tiny script
// on a test-controlled PATH. A local httptest.Server captures every
// webhook POST so we can assert the bodies and recompute the HMAC
// signatures from outside the script.
//
// The fakes are intentionally minimal — we don't try to recreate aws
// CLI, git, or Kaniko, only the bits the entrypoint actually invokes.
// Each test injects different fake behavior by writing arg-pattern
// scripts into a temp PATH dir.

package builder_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// entrypointPath returns the absolute path to entrypoint.sh, located
// relative to this test file. Tests can't rely on cwd because go test
// runs from the package dir but builders elsewhere may not.
func entrypointPath(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Join(wd, "entrypoint.sh")
}

// capturedEvent is one webhook POST as the fake control plane saw it.
type capturedEvent struct {
	Timestamp string
	Signature string
	Body      []byte
	// Parsed mirrors Body decoded as the {stage,status,data?} envelope
	// the API expects.
	Parsed struct {
		Stage  string         `json:"stage"`
		Status string         `json:"status"`
		Data   map[string]any `json:"data,omitempty"`
	}
}

// fakeCP starts a local server that records every POST to /events.
// Returns the URL to use in SPACEFLEET_WEBHOOK_URL plus accessors for
// the captured events.
func fakeCP(t *testing.T) (url string, snapshot func() []capturedEvent) {
	t.Helper()
	var (
		mu     sync.Mutex
		events []capturedEvent
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		ev := capturedEvent{
			Timestamp: r.Header.Get("X-Spacefleet-Timestamp"),
			Signature: r.Header.Get("X-Spacefleet-Signature"),
			Body:      body,
		}
		if err := json.Unmarshal(body, &ev.Parsed); err != nil {
			http.Error(w, "bad json: "+err.Error(), 400)
			return
		}
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/events", func() []capturedEvent {
		mu.Lock()
		defer mu.Unlock()
		out := make([]capturedEvent, len(events))
		copy(out, events)
		return out
	}
}

// writeFake writes an executable shell script into dir/name.
func writeFake(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

// fakes holds the canned scripts the entrypoint will resolve via PATH
// or KANIKO_BIN. Each field is the *body* of the corresponding script;
// the test harness wraps it with a shebang.
type fakes struct {
	// awsScript is the body of a fake `aws` binary. It receives the
	// usual aws argv ("secretsmanager get-secret-value --region ...
	// --query SecretString --output text"). Default echoes a fixed
	// token; tests override it to simulate failures.
	awsScript string
	// gitScript is the body of a fake `git`. The real entrypoint runs
	// a sequence of git subcommands; the script can branch on $1/$@.
	gitScript string
	// kanikoScript is the body of the fake kaniko executor. It must
	// honour --digest-file by writing a sha256 into that path.
	kanikoScript string
}

// defaultFakes returns scripts that all succeed and produce plausible
// outputs. Tests start from this and selectively override.
func defaultFakes() fakes {
	return fakes{
		awsScript: `#!/bin/sh
# fake aws — echoes a fixed token regardless of args.
printf 'ghp_FAKE_INSTALL_TOKEN'
`,
		gitScript: `#!/bin/sh
# fake git — accepts every subcommand as a no-op. Real git would
# create files; we don't need them because kaniko is faked too.
exit 0
`,
		kanikoScript: `#!/bin/sh
# fake kaniko — writes a fixed digest to whatever --digest-file points at.
digest_file=""
while [ $# -gt 0 ]; do
  case "$1" in
    --digest-file) digest_file="$2"; shift 2 ;;
    --digest-file=*) digest_file="${1#*=}"; shift ;;
    *) shift ;;
  esac
done
if [ -n "$digest_file" ]; then
  printf 'sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd' > "$digest_file"
fi
exit 0
`,
	}
}

// runEntrypoint executes the real entrypoint.sh with the given env and
// fake commands shadowing aws/git/kaniko. Returns the combined output
// and the exit error (nil on exit 0).
func runEntrypoint(t *testing.T, env map[string]string, f fakes) (output string, err error) {
	t.Helper()

	binDir := t.TempDir()
	writeFake(t, binDir, "aws", f.awsScript)
	writeFake(t, binDir, "git", f.gitScript)

	kanikoDir := t.TempDir()
	kanikoPath := filepath.Join(kanikoDir, "kaniko")
	writeFake(t, kanikoDir, "kaniko", f.kanikoScript)

	workdir := t.TempDir()

	// Build the env. We start from a clean, predictable PATH so the
	// fakes are guaranteed to win, then add openssl/jq/curl/date/awk
	// from the real system PATH (the entrypoint depends on them).
	cmd := exec.Command("bash", entrypointPath(t))
	cmd.Env = []string{
		"PATH=" + binDir + ":" + os.Getenv("PATH"),
		"KANIKO_BIN=" + kanikoPath,
		"SPACEFLEET_WORKDIR=" + workdir,
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	out, runErr := cmd.CombinedOutput()
	return string(out), runErr
}

// validEnv returns a complete required-env map. Tests selectively
// override fields by mutating the returned map.
func validEnv(webhookURL string) map[string]string {
	return map[string]string{
		"SPACEFLEET_BUILD_ID":       "build-1234",
		"SPACEFLEET_WEBHOOK_URL":    webhookURL,
		"SPACEFLEET_WEBHOOK_SECRET": "test-secret-32-bytes-of-entropy!",
		"GITHUB_TOKEN_SECRET_ARN":   "arn:aws:secretsmanager:us-east-1:111122223333:secret:spacefleet/builds/app-x/tok-abc",
		"REPO_FULL_NAME":            "spacefleet/example-app",
		"COMMIT_SHA":                "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		"ECR_REPO":                  "111122223333.dkr.ecr.us-east-1.amazonaws.com/spacefleet-app-x",
		"ECR_CACHE_REPO":            "111122223333.dkr.ecr.us-east-1.amazonaws.com/spacefleet-app-x-cache",
		"AWS_REGION":                "us-east-1",
	}
}

// TestMissingRequiredEnv loops over every required var, blanks it,
// and asserts the script exits non-zero with a message naming the
// var. We do this in one table-driven test so adding a new required
// var to the script auto-extends the matrix below.
func TestMissingRequiredEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("entrypoint.sh is bash-only")
	}

	url, _ := fakeCP(t)
	for _, v := range []string{
		"SPACEFLEET_BUILD_ID",
		"SPACEFLEET_WEBHOOK_URL",
		"SPACEFLEET_WEBHOOK_SECRET",
		"GITHUB_TOKEN_SECRET_ARN",
		"REPO_FULL_NAME",
		"COMMIT_SHA",
		"ECR_REPO",
		"ECR_CACHE_REPO",
		"AWS_REGION",
	} {
		t.Run(v, func(t *testing.T) {
			env := validEnv(url)
			delete(env, v)
			out, err := runEntrypoint(t, env, defaultFakes())
			if err == nil {
				t.Fatalf("expected non-zero exit, got success.\n%s", out)
			}
			if !strings.Contains(out, "missing required env") {
				t.Errorf("output missing 'missing required env' marker:\n%s", out)
			}
			if !strings.Contains(out, v) {
				t.Errorf("output should name the missing var %q:\n%s", v, out)
			}
		})
	}
}

// TestHappyPath runs the script end-to-end with all-success fakes and
// verifies every emitted event: their order, statuses, payload shape,
// and HMAC signatures.
func TestHappyPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("entrypoint.sh is bash-only")
	}

	url, snapshot := fakeCP(t)
	env := validEnv(url)
	out, err := runEntrypoint(t, env, defaultFakes())
	if err != nil {
		t.Fatalf("entrypoint failed: %v\n%s", err, out)
	}

	events := snapshot()
	want := []struct{ stage, status string }{
		{"clone", "running"},
		{"clone", "succeeded"},
		{"build", "running"},
		{"build", "succeeded"},
		{"push", "running"},
		{"push", "succeeded"},
	}
	if len(events) != len(want) {
		t.Fatalf("got %d events, want %d. events: %s", len(events), len(want), formatEvents(events))
	}
	for i, w := range want {
		if events[i].Parsed.Stage != w.stage || events[i].Parsed.Status != w.status {
			t.Errorf("event[%d] = %s/%s, want %s/%s", i,
				events[i].Parsed.Stage, events[i].Parsed.Status, w.stage, w.status)
		}
	}

	// Last event carries the image data.
	last := events[len(events)-1]
	if got, want := last.Parsed.Data["image_uri"], env["ECR_REPO"]+":"+env["COMMIT_SHA"]; got != want {
		t.Errorf("push.succeeded image_uri = %q, want %q", got, want)
	}
	if got, ok := last.Parsed.Data["image_digest"].(string); !ok || !strings.HasPrefix(got, "sha256:") {
		t.Errorf("push.succeeded image_digest = %v, want sha256: prefix", last.Parsed.Data["image_digest"])
	}

	// Every event must verify against the per-build secret.
	for i, ev := range events {
		assertSignatureValid(t, ev, env["SPACEFLEET_WEBHOOK_SECRET"], i)
	}
}

// TestAWSFailureEmitsCloneFailed simulates Secrets Manager unreachable.
// The ERR trap should attribute the crash to the active stage (clone)
// and post a `clone failed` event before exiting non-zero.
func TestAWSFailureEmitsCloneFailed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("entrypoint.sh is bash-only")
	}

	url, snapshot := fakeCP(t)
	f := defaultFakes()
	f.awsScript = `#!/bin/sh
echo "fake aws: simulated permission denied" >&2
exit 1
`

	out, err := runEntrypoint(t, validEnv(url), f)
	if err == nil {
		t.Fatalf("expected non-zero exit, got success.\n%s", out)
	}

	events := snapshot()
	if len(events) < 2 {
		t.Fatalf("expected at least 2 events (clone running + clone failed), got %d.\n%s", len(events), formatEvents(events))
	}
	if events[0].Parsed.Stage != "clone" || events[0].Parsed.Status != "running" {
		t.Errorf("event[0] = %s/%s, want clone/running", events[0].Parsed.Stage, events[0].Parsed.Status)
	}
	last := events[len(events)-1]
	if last.Parsed.Stage != "clone" || last.Parsed.Status != "failed" {
		t.Errorf("last event = %s/%s, want clone/failed", last.Parsed.Stage, last.Parsed.Status)
	}
	if last.Parsed.Data["error"] == nil {
		t.Errorf("last event missing data.error: %s", last.Body)
	}
}

// TestEmptySecretFailsCleanly is the "Secrets Manager returned None"
// case — different from a transport failure; the aws CLI exits zero
// but the SecretString is empty. The script must catch this and post
// clone failed rather than propagating an empty token to git.
func TestEmptySecretFailsCleanly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("entrypoint.sh is bash-only")
	}

	url, snapshot := fakeCP(t)
	f := defaultFakes()
	f.awsScript = `#!/bin/sh
# Mimic aws CLI returning the literal "None" sentinel for an absent SecretString.
printf 'None'
`

	out, err := runEntrypoint(t, validEnv(url), f)
	if err == nil {
		t.Fatalf("expected non-zero exit, got success.\n%s", out)
	}

	events := snapshot()
	if len(events) == 0 || events[len(events)-1].Parsed.Status != "failed" {
		t.Fatalf("expected a failed event, got: %s", formatEvents(events))
	}
}

// TestGitFailureEmitsCloneFailed covers the "GitHub returned 404" /
// "ref doesn't exist" case via a fake git that exits non-zero on fetch.
func TestGitFailureEmitsCloneFailed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("entrypoint.sh is bash-only")
	}

	url, snapshot := fakeCP(t)
	f := defaultFakes()
	f.gitScript = `#!/bin/sh
# Init/remote-add succeed; fetch fails. Mirrors GitHub's "Repository not found".
case "$*" in
  *fetch*)
    echo "fatal: repository 'https://github.com/x/y.git' not found" >&2
    exit 128 ;;
  *) exit 0 ;;
esac
`

	out, err := runEntrypoint(t, validEnv(url), f)
	if err == nil {
		t.Fatalf("expected non-zero exit, got success.\n%s", out)
	}

	events := snapshot()
	last := events[len(events)-1]
	if last.Parsed.Stage != "clone" || last.Parsed.Status != "failed" {
		t.Errorf("last event = %s/%s, want clone/failed.\n%s",
			last.Parsed.Stage, last.Parsed.Status, formatEvents(events))
	}
}

// TestKanikoFailureEmitsBuildFailed covers a Dockerfile error or push
// rejection from kaniko. The clone stage should have completed cleanly
// before kaniko fires.
func TestKanikoFailureEmitsBuildFailed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("entrypoint.sh is bash-only")
	}

	url, snapshot := fakeCP(t)
	f := defaultFakes()
	f.kanikoScript = `#!/bin/sh
echo "kaniko: error building image: /Dockerfile not found" >&2
exit 1
`

	out, err := runEntrypoint(t, validEnv(url), f)
	if err == nil {
		t.Fatalf("expected non-zero exit, got success.\n%s", out)
	}

	events := snapshot()

	// Sequence we expect: clone running, clone succeeded, build running, build failed.
	gotStages := []string{}
	for _, ev := range events {
		gotStages = append(gotStages, ev.Parsed.Stage+"/"+ev.Parsed.Status)
	}
	want := []string{"clone/running", "clone/succeeded", "build/running", "build/failed"}
	if !equalSlices(gotStages, want) {
		t.Errorf("event sequence = %v, want %v", gotStages, want)
	}
}

// TestKanikoEmptyDigestEmitsBuildFailed covers the corner where kaniko
// exits 0 but writes an empty digest file. The script must catch this
// and post build/failed before exiting — otherwise the build sits at
// build/running until the polling backstop trips.
func TestKanikoEmptyDigestEmitsBuildFailed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("entrypoint.sh is bash-only")
	}

	url, snapshot := fakeCP(t)
	f := defaultFakes()
	f.kanikoScript = `#!/bin/sh
# Exits 0 but writes nothing to --digest-file.
digest_file=""
while [ $# -gt 0 ]; do
  case "$1" in
    --digest-file) digest_file="$2"; shift 2 ;;
    --digest-file=*) digest_file="${1#*=}"; shift ;;
    *) shift ;;
  esac
done
[ -n "$digest_file" ] && : > "$digest_file"
exit 0
`

	out, err := runEntrypoint(t, validEnv(url), f)
	if err == nil {
		t.Fatalf("expected non-zero exit, got success.\n%s", out)
	}

	events := snapshot()
	last := events[len(events)-1]
	if last.Parsed.Stage != "build" || last.Parsed.Status != "failed" {
		t.Errorf("last event = %s/%s, want build/failed.\n%s",
			last.Parsed.Stage, last.Parsed.Status, formatEvents(events))
	}
	if errStr, _ := last.Parsed.Data["error"].(string); !strings.Contains(errStr, "digest") {
		t.Errorf("expected data.error to mention 'digest', got %q", errStr)
	}
}

// TestSignatureUsesRawBody guards against a subtle regression: if the
// script ever re-marshalled the body for signing (vs. signing the
// exact bytes it sends), the signature would still verify but only by
// coincidence. We check that the signature recomputed over the bytes
// the server received matches the header.
func TestSignatureUsesRawBody(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("entrypoint.sh is bash-only")
	}

	url, snapshot := fakeCP(t)
	env := validEnv(url)
	if _, err := runEntrypoint(t, env, defaultFakes()); err != nil {
		t.Fatalf("entrypoint failed: %v", err)
	}
	for i, ev := range snapshot() {
		assertSignatureValid(t, ev, env["SPACEFLEET_WEBHOOK_SECRET"], i)
	}
}

// TestTimestampIsRecent guards against the date helper drifting (e.g.
// printing milliseconds, a different format) — the server enforces a
// 5-minute drift window, so we just need each timestamp parseable as
// unix-seconds and within a few seconds of "now".
func TestTimestampIsRecent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("entrypoint.sh is bash-only")
	}

	url, snapshot := fakeCP(t)
	before := time.Now().Unix()
	if _, err := runEntrypoint(t, validEnv(url), defaultFakes()); err != nil {
		t.Fatalf("entrypoint failed: %v", err)
	}
	after := time.Now().Unix()
	for i, ev := range snapshot() {
		ts, err := strconv.ParseInt(ev.Timestamp, 10, 64)
		if err != nil {
			t.Errorf("event[%d] timestamp %q not parseable: %v", i, ev.Timestamp, err)
			continue
		}
		if ts < before-5 || ts > after+5 {
			t.Errorf("event[%d] timestamp %d not within [%d, %d]", i, ts, before, after)
		}
	}
}

// assertSignatureValid recomputes hex(HMAC-SHA256(secret, "<ts>.<body>"))
// and compares it to the header. Constant-time compare just to be
// disciplined; failures still print both values for debuggability.
func assertSignatureValid(t *testing.T, ev capturedEvent, secret string, idx int) {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ev.Timestamp))
	mac.Write([]byte("."))
	mac.Write(ev.Body)
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(ev.Signature)) {
		t.Errorf("event[%d] signature mismatch:\n  got:  %s\n  want: %s\n  ts:   %s\n  body: %s",
			idx, ev.Signature, want, ev.Timestamp, ev.Body)
	}
}

func formatEvents(evs []capturedEvent) string {
	var sb strings.Builder
	for i, ev := range evs {
		fmt.Fprintf(&sb, "  [%d] %s %s\n", i, ev.Parsed.Stage, ev.Parsed.Status)
	}
	return sb.String()
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
