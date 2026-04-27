// Spacefleet's binary is multi-process by subcommand: `serve` runs the
// HTTP API, `worker` drives River-backed background jobs, `migrate`
// applies SQL. The default (no subcommand) is `serve` — every existing
// shell script and Dockerfile that runs `./spacefleet` keeps working.
package main

import (
	"fmt"
	"os"

	"github.com/joho/godotenv"
)

func main() {
	// .env is optional — in prod, env vars come from the deployment environment.
	_ = godotenv.Load()

	cmd, args := parseArgs(os.Args[1:])
	switch cmd {
	case "serve":
		runServe(args)
	case "worker":
		runWorker(args)
	case "migrate":
		runMigrate(args)
	case "help", "-h", "--help":
		printUsage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "spacefleet: unknown subcommand %q\n\n", cmd)
		printUsage(os.Stderr)
		os.Exit(2)
	}
}

// parseArgs returns the subcommand and any remaining arguments. With no
// arguments at all, defaults to `serve` so `./spacefleet` (no args) is
// still a valid invocation — that's what existing Dockerfiles use.
func parseArgs(args []string) (cmd string, rest []string) {
	if len(args) == 0 {
		return "serve", nil
	}
	return args[0], args[1:]
}

func printUsage(w *os.File) {
	fmt.Fprintln(w, "usage: spacefleet [serve|worker|migrate] [args...]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  serve    run the HTTP API (default)")
	fmt.Fprintln(w, "  worker   run the River background-job worker")
	fmt.Fprintln(w, "  migrate  apply or inspect SQL migrations (subcommands: up, status)")
}
