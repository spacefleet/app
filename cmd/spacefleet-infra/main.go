// spacefleet-infra is a developer CLI for driving the Pulumi
// orchestrator outside the worker process. It's the smallest possible
// harness around lib/pulumi.Orchestrator: load config, open ent, look
// up a cloud account by id, dispatch up/destroy.
//
// Usage:
//
//	spacefleet-infra up      --cloud-account=<id> [--app=<id>]
//	spacefleet-infra destroy --cloud-account=<id> [--app=<id>]
//
// Without --app, operates on the per-cloud-account "builder-infra"
// stack only. With --app, operates on the per-app "app-build" stack
// (which always reconciles builder-infra first on `up`).
//
// The CLI reads the same .env / env vars as `spacefleet serve`:
// DATABASE_URL, SPACEFLEET_STATE_BUCKET / _REGION / _KMS_KEY_ARN,
// SPACEFLEET_BUILDER_IMAGE (or the binary's -ldflags default),
// AWS_PLATFORM_ACCOUNT_ID, plus the standard AWS SDK chain for the
// platform-side credentials we use to AssumeRole into the customer
// account.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/google/uuid"
	"github.com/joho/godotenv"

	"github.com/spacefleet/app/ent"
	"github.com/spacefleet/app/lib/config"
	"github.com/spacefleet/app/lib/db"
	libpulumi "github.com/spacefleet/app/lib/pulumi"
)

func main() {
	_ = godotenv.Load()

	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "up":
		runUp(args)
	case "destroy":
		runDestroy(args)
	case "help", "-h", "--help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "spacefleet-infra: unknown subcommand %q\n\n", cmd)
		usage(os.Stderr)
		os.Exit(2)
	}
}

// commonFlags returns the flag set every subcommand shares. Returning
// the parsed values via pointers keeps the call sites symmetric.
func commonFlags(name string) (*flag.FlagSet, *string, *string) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	cloudAccount := fs.String("cloud-account", "", "Spacefleet cloud-account id (uuid)")
	app := fs.String("app", "", "Spacefleet app id (uuid); omit to operate on the cloud-account's shared builder-infra stack")
	return fs, cloudAccount, app
}

func runUp(args []string) {
	fs, cloudAccount, app := commonFlags("up")
	_ = fs.Parse(args)
	if *cloudAccount == "" {
		fmt.Fprintln(os.Stderr, "spacefleet-infra up: --cloud-account is required")
		os.Exit(2)
	}

	ctx, stop := signalCtx()
	defer stop()

	deps, err := setup(ctx)
	if err != nil {
		log.Fatalf("spacefleet-infra: %v", err)
	}
	defer deps.Close()

	target, err := deps.target(ctx, *cloudAccount)
	if err != nil {
		log.Fatalf("spacefleet-infra: %v", err)
	}

	opts := libpulumi.RunOpts{Stdout: os.Stdout, Stderr: os.Stderr}
	if *app == "" {
		out, err := deps.orch.UpBuilderInfra(ctx, target, opts)
		if err != nil {
			log.Fatalf("spacefleet-infra up builder-infra: %v", err)
		}
		fmt.Println()
		fmt.Println("builder-infra outputs:")
		fmt.Printf("  cluster_arn:        %s\n", out.ClusterARN)
		fmt.Printf("  cluster_name:       %s\n", out.ClusterName)
		fmt.Printf("  vpc_id:             %s\n", out.VpcID)
		fmt.Printf("  subnet_id:          %s\n", out.SubnetID)
		fmt.Printf("  security_group_id:  %s\n", out.SecurityGroupID)
		fmt.Printf("  execution_role_arn: %s\n", out.ExecutionRoleARN)
		fmt.Printf("  log_group_prefix:   %s\n", out.LogGroupPrefix)
		return
	}

	appID, err := uuid.Parse(*app)
	if err != nil {
		log.Fatalf("spacefleet-infra: --app must be a uuid: %v", err)
	}
	row, err := deps.entCli.App.Get(ctx, appID)
	if err != nil {
		log.Fatalf("spacefleet-infra: load app %s: %v", appID, err)
	}

	infra, appOut, err := deps.orch.UpAppBuild(ctx, target, libpulumi.AppRef{ID: row.ID.String(), Slug: row.Slug}, opts)
	if err != nil {
		log.Fatalf("spacefleet-infra up app-build: %v", err)
	}
	fmt.Println()
	fmt.Println("builder-infra outputs (from reconcile):")
	fmt.Printf("  cluster_arn:        %s\n", infra.ClusterARN)
	fmt.Printf("  execution_role_arn: %s\n", infra.ExecutionRoleARN)
	fmt.Println()
	fmt.Println("app-build outputs:")
	fmt.Printf("  ecr_repo_uri:        %s\n", appOut.ECRRepoURI)
	fmt.Printf("  ecr_cache_repo_uri:  %s\n", appOut.ECRCacheRepoURI)
	fmt.Printf("  task_role_arn:       %s\n", appOut.TaskRoleARN)
	fmt.Printf("  task_definition_arn: %s\n", appOut.TaskDefinitionARN)
	fmt.Printf("  log_group_name:      %s\n", appOut.LogGroupName)
}

func runDestroy(args []string) {
	fs, cloudAccount, app := commonFlags("destroy")
	_ = fs.Parse(args)
	if *cloudAccount == "" {
		fmt.Fprintln(os.Stderr, "spacefleet-infra destroy: --cloud-account is required")
		os.Exit(2)
	}

	ctx, stop := signalCtx()
	defer stop()

	deps, err := setup(ctx)
	if err != nil {
		log.Fatalf("spacefleet-infra: %v", err)
	}
	defer deps.Close()

	target, err := deps.target(ctx, *cloudAccount)
	if err != nil {
		log.Fatalf("spacefleet-infra: %v", err)
	}

	opts := libpulumi.RunOpts{Stdout: os.Stdout, Stderr: os.Stderr}
	if *app == "" {
		// Destroying builder-infra is destructive: it tears down the
		// cluster + role used by every per-app stack in this account.
		// The CLI is a dev tool; the worker will never call this path.
		// Confirm before proceeding to keep mistakes loud.
		if !confirmDestructive(fmt.Sprintf("destroy builder-infra for cloud account %s", *cloudAccount)) {
			fmt.Println("aborted.")
			return
		}
		if err := deps.orch.DestroyBuilderInfra(ctx, target, opts); err != nil {
			log.Fatalf("spacefleet-infra destroy builder-infra: %v", err)
		}
		fmt.Println("builder-infra destroyed.")
		return
	}

	if _, err := uuid.Parse(*app); err != nil {
		log.Fatalf("spacefleet-infra: --app must be a uuid: %v", err)
	}
	if err := deps.orch.DestroyAppBuild(ctx, target, *app, opts); err != nil {
		log.Fatalf("spacefleet-infra destroy app-build: %v", err)
	}
	fmt.Printf("app-build for app %s destroyed.\n", *app)
}

// deps bundles the runtime objects every subcommand needs. Built once
// per invocation in [setup] and torn down on Close.
type deps struct {
	cfg     *config.Config
	entCli  *ent.Client
	closeDB func() error
	orch    *libpulumi.Orchestrator
}

// setup opens the database, loads the verifier, and constructs the
// orchestrator. Returns a deps with a Close() the caller must call.
func setup(ctx context.Context) (*deps, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if !cfg.StateBackendConfigured() {
		return nil, errors.New("state backend not configured (set SPACEFLEET_STATE_BUCKET / _REGION / _KMS_KEY_ARN; see make bootstrap-state)")
	}
	if cfg.AWSPlatformAccountID == "" {
		return nil, errors.New("AWS_PLATFORM_ACCOUNT_ID is required (the platform's own AWS account ID)")
	}
	if cfg.BuilderImage == "" {
		return nil, errors.New("builder image not set (SPACEFLEET_BUILDER_IMAGE or -ldflags default required)")
	}

	sqlDB, entCli, err := db.Open(cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}

	orch, err := libpulumi.NewOrchestrator(libpulumi.BackendConfig{
		Bucket:    cfg.StateBucket,
		Region:    cfg.StateBucketRegion,
		KMSKeyARN: cfg.StateKMSKeyARN,
	}, cfg.BuilderImage)
	if err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("orchestrator: %w", err)
	}

	return &deps{
		cfg:     cfg,
		entCli:  entCli,
		closeDB: sqlDB.Close,
		orch:    orch,
	}, nil
}

func (d *deps) Close() {
	if d.entCli != nil {
		_ = d.entCli.Close()
	}
	if d.closeDB != nil {
		_ = d.closeDB()
	}
}

// target loads a cloud-account row by id and turns it into an
// AccountTarget the orchestrator can act on. Bails with a clear error
// if the account isn't connected (no role ARN yet) or missing fields.
func (d *deps) target(ctx context.Context, idStr string) (libpulumi.AccountTarget, error) {
	id, err := uuid.Parse(idStr)
	if err != nil {
		return libpulumi.AccountTarget{}, fmt.Errorf("parse cloud-account id: %w", err)
	}
	row, err := d.entCli.CloudAccount.Get(ctx, id)
	if err != nil {
		if ent.IsNotFound(err) {
			return libpulumi.AccountTarget{}, fmt.Errorf("cloud account %s not found", id)
		}
		return libpulumi.AccountTarget{}, err
	}
	if row.RoleArn == "" {
		return libpulumi.AccountTarget{}, fmt.Errorf("cloud account %s has no role ARN — complete onboarding first", id)
	}
	if row.AccountID == "" {
		return libpulumi.AccountTarget{}, fmt.Errorf("cloud account %s has no AWS account ID — complete onboarding first", id)
	}
	if row.ExternalID == "" {
		return libpulumi.AccountTarget{}, fmt.Errorf("cloud account %s has no external ID (data corruption?)", id)
	}
	return libpulumi.AccountTarget{
		OrgID:          row.OrgSlug,
		CloudAccountID: row.ID.String(),
		AWSAccountID:   row.AccountID,
		RoleARN:        row.RoleArn,
		ExternalID:     row.ExternalID,
		Region:         row.Region,
	}, nil
}

// signalCtx wires SIGINT/SIGTERM to ctx cancellation so a Ctrl-C
// during a long Pulumi run exits cleanly. Pulumi forwards cancellation
// to its child process, which interrupts the in-flight stack op.
func signalCtx() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-ch:
			fmt.Fprintln(os.Stderr, "\nspacefleet-infra: interrupt received, cancelling...")
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// confirmDestructive prompts the user to confirm a destructive action
// by typing "yes". Returns true on yes; false on anything else
// including EOF (pipe-closed). Lives in the CLI, not the orchestrator,
// because the orchestrator is also used by automated paths (the
// worker's destroy_app job) where prompts are wrong.
func confirmDestructive(message string) bool {
	fmt.Fprintf(os.Stderr, "\nDestructive: %s\nType 'yes' to confirm: ", message)
	var resp string
	if _, err := fmt.Fscanln(os.Stdin, &resp); err != nil {
		return false
	}
	return resp == "yes"
}

func usage(w *os.File) {
	fmt.Fprintln(w, "usage: spacefleet-infra <up|destroy> --cloud-account=<id> [--app=<id>]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  up        bring stacks to desired state (idempotent)")
	fmt.Fprintln(w, "  destroy   tear down stacks")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Without --app, operates on the per-cloud-account builder-infra stack.")
	fmt.Fprintln(w, "With --app=<uuid>, operates on the per-app app-build stack.")
}
