package pulumi

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func TestNewRunnerValidates(t *testing.T) {
	good := Backend{StateURL: "s3://b/x?region=us-east-1", StackName: "x", SecretsProvider: "passphrase"}
	noopProgram := Program(func(*pulumi.Context) error { return nil })

	cases := []struct {
		name string
		cfg  RunnerConfig
		ok   bool
	}{
		{"valid", RunnerConfig{Backend: good, Program: noopProgram}, true},
		{"empty state url", RunnerConfig{Backend: Backend{StackName: "x"}, Program: noopProgram}, false},
		{"empty stack", RunnerConfig{Backend: Backend{StateURL: "s3://b"}, Program: noopProgram}, false},
		{"nil program", RunnerConfig{Backend: good}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewRunner(tc.cfg)
			gotOK := err == nil
			if gotOK != tc.ok {
				t.Errorf("NewRunner ok = %v, want %v (err=%v)", gotOK, tc.ok, err)
			}
		})
	}
}

// TestUpAgainstFileBackend exercises the full Automation API path:
// upsert a stack, run a no-op inline program, observe outputs. We use
// the local `file://` backend so we don't need S3 or AWS creds; the
// state-path code paths are covered separately by the BackendFor*
// tests.
//
// This is the proof that lib/pulumi can drive Pulumi end-to-end. If
// pulumi isn't on $PATH the test skips with a clear message — CI will
// have it installed via .tool-versions or the Dockerfile.
func TestUpAgainstFileBackend(t *testing.T) {
	if _, err := exec.LookPath("pulumi"); err != nil {
		t.Skip("pulumi CLI not on $PATH; skipping integration test")
	}

	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	pulumiHome := filepath.Join(dir, "pulumi-home")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.MkdirAll(pulumiHome, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	cfg := RunnerConfig{
		Backend: Backend{
			StateURL:        "file://" + stateDir,
			StackName:       "spacefleet-test",
			SecretsProvider: "passphrase",
		},
		Program: func(ctx *pulumi.Context) error {
			ctx.Export("hello", pulumi.String("world"))
			return nil
		},
		EnvVars: map[string]string{
			"PULUMI_HOME":              pulumiHome,
			"PULUMI_CONFIG_PASSPHRASE": "test-passphrase",
		},
	}
	r, err := NewRunner(cfg)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	// Pulumi's first run downloads plugins and warms a workspace; give
	// it a generous timeout but still bound it so a hung CLI doesn't
	// hold the suite.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	res, err := r.Up(ctx)
	if err != nil {
		t.Fatalf("Up: %v\nstdout: %s\nstderr: %s", err, res.StdOut, res.StdErr)
	}

	hello, ok := res.Outputs["hello"]
	if !ok {
		t.Fatalf("expected output hello, got %v", res.Outputs)
	}
	if v, _ := hello.Value.(string); v != "world" {
		t.Errorf("hello = %v, want world", hello.Value)
	}

	// Re-running the same program should be a no-op. This proves the
	// reconcile-on-every-build property the BUILD_PIPELINE doc relies on.
	if _, err := r.Up(ctx); err != nil {
		t.Fatalf("second Up: %v", err)
	}
}

// TestUpFailsWhenCLIMissing simulates a missing pulumi binary by
// shadowing PATH. The error should be specific and actionable.
func TestUpFailsWhenCLIMissing(t *testing.T) {
	emptyPATH := t.TempDir()
	t.Setenv("PATH", emptyPATH)

	r, err := NewRunner(RunnerConfig{
		Backend: Backend{StateURL: "file:///tmp", StackName: "x", SecretsProvider: "passphrase"},
		Program: func(*pulumi.Context) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = r.Up(ctx)
	if err == nil {
		t.Fatal("expected error when pulumi CLI is missing")
	}
	if !strings.Contains(err.Error(), "pulumi") {
		t.Errorf("err = %v, want mention of pulumi", err)
	}
}
