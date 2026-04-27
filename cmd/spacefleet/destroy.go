package main

import (
	"context"
	"fmt"
	"os"

	"github.com/google/uuid"

	"github.com/spacefleet/app/ent"
	"github.com/spacefleet/app/lib/pulumi"
)

// newDestroyAppRunner returns a closure suitable for
// queue.RegisterDestroyAppWorker. It runs `pulumi destroy` against the
// per-app stack, then drops the apps row when teardown succeeds. The
// per-cloud-account `builder-infra` stack is shared and is *not*
// destroyed here — that's only torn down on cloud-account disconnect.
//
// Idempotent across retries: if the apps row is already gone when we
// run, we treat it as success (something else cleaned up). If pulumi
// destroy fails, we leave deleting_at set so the user can retry from
// the UI; we don't roll back the marker.
func newDestroyAppRunner(client *ent.Client, orchestrator *pulumi.Orchestrator) func(ctx context.Context, appID uuid.UUID) error {
	return func(ctx context.Context, appID uuid.UUID) error {
		app, err := client.App.Get(ctx, appID)
		if err != nil {
			if ent.IsNotFound(err) {
				// Already gone — call it success.
				return nil
			}
			return fmt.Errorf("destroy_app: load app: %w", err)
		}
		ca, err := client.CloudAccount.Get(ctx, app.CloudAccountID)
		if err != nil {
			return fmt.Errorf("destroy_app: load cloud account: %w", err)
		}

		target := pulumi.AccountTarget{
			OrgID:          app.OrgSlug,
			CloudAccountID: ca.ID.String(),
			AWSAccountID:   ca.AccountID,
			RoleARN:        ca.RoleArn,
			ExternalID:     ca.ExternalID,
			Region:         ca.Region,
		}
		if err := orchestrator.DestroyAppBuild(ctx, target, app.ID.String(), pulumi.RunOpts{
			Stdout: os.Stdout,
			Stderr: os.Stderr,
		}); err != nil {
			return fmt.Errorf("destroy_app: pulumi destroy: %w", err)
		}

		// Teardown succeeded — drop the row. Cascading FKs on builds
		// will clean up build history; the customer's audit trail in
		// CloudWatch logs (30-day retention) survives.
		if err := client.App.DeleteOneID(app.ID).Exec(ctx); err != nil && !ent.IsNotFound(err) {
			return fmt.Errorf("destroy_app: delete row: %w", err)
		}
		return nil
	}
}
