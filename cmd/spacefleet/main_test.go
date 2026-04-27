package main

import "testing"

// TestParseArgsDefaultsToServe locks in the contract that an empty
// argv defaults to `serve`. The Dockerfile relies on this — its
// ENTRYPOINT is just /spacefleet with no subcommand.
func TestParseArgsDefaultsToServe(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"empty", nil, "serve"},
		{"explicit serve", []string{"serve"}, "serve"},
		{"worker", []string{"worker"}, "worker"},
		{"migrate", []string{"migrate", "up"}, "migrate"},
		{"unknown bubbles through", []string{"banana"}, "banana"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, _ := parseArgs(tc.args)
			if cmd != tc.want {
				t.Errorf("parseArgs(%v) cmd = %q, want %q", tc.args, cmd, tc.want)
			}
		})
	}
}

// TestParseArgsForwardsRest confirms migrate-style subcommands receive
// their tail args.
func TestParseArgsForwardsRest(t *testing.T) {
	_, rest := parseArgs([]string{"migrate", "up", "--dry-run"})
	if len(rest) != 2 || rest[0] != "up" || rest[1] != "--dry-run" {
		t.Errorf("rest = %v, want [up --dry-run]", rest)
	}
}
