package queue

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/spacefleet/app/lib/builds"
)

// DestroyAppArgs is the River job that tears down an app's per-app
// Pulumi stack. The HTTP API enqueues; the worker process runs
// `pulumi destroy` against the stack and drops the apps row when
// teardown succeeds.
//
// Keep the args minimal — anything else the worker needs is
// reachable from the apps row by ID.
type DestroyAppArgs struct {
	AppID uuid.UUID `json:"app_id"`
}

// Kind is River's stable identifier for this job. Don't rename it once
// jobs are enqueued in production — River matches kind strings to
// workers.
func (DestroyAppArgs) Kind() string { return "destroy_app" }

// EnqueueDestroyApp is what the HTTP API uses. Wraps Client.Insert so
// the apps service doesn't need to import river/rivertype.
func (c *Client) EnqueueDestroyApp(ctx context.Context, appID uuid.UUID) error {
	if _, err := c.Insert(ctx, DestroyAppArgs{AppID: appID}); err != nil {
		return err
	}
	return nil
}

// EnqueueBuild satisfies builds.JobEnqueuer. The build worker
// (lib/builds.Worker) consumes the args.
func (c *Client) EnqueueBuild(ctx context.Context, buildID uuid.UUID) error {
	if _, err := c.Insert(ctx, builds.BuildJobArgs{BuildID: buildID}); err != nil {
		return err
	}
	return nil
}

// RegisterBuildWorker wires the build worker into the registry. We keep
// registration calls localized to one file so the panic surface
// (river.AddWorker panics on duplicate types) is small and obvious.
func RegisterBuildWorker(workers *Workers, w river.Worker[builds.BuildJobArgs]) {
	river.AddWorker(workers.Inner(), w)
}

// DestroyAppWorker is the River worker that tears down an app's
// per-app Pulumi stack. The Run closure carries the actual destroy
// logic so this file stays free of pulumi/ent imports.
type DestroyAppWorker struct {
	river.WorkerDefaults[DestroyAppArgs]
	Run func(ctx context.Context, appID uuid.UUID) error
}

// Work delegates to the bound Run function.
func (w *DestroyAppWorker) Work(ctx context.Context, job *river.Job[DestroyAppArgs]) error {
	if w.Run == nil {
		return errors.New("destroy_app worker has no Run func configured")
	}
	return w.Run(ctx, job.Args.AppID)
}

// RegisterDestroyAppWorker wires the destroy worker. Call once from
// cmd/spacefleet/worker.go.
func RegisterDestroyAppWorker(workers *Workers, run func(ctx context.Context, appID uuid.UUID) error) {
	river.AddWorker(workers.Inner(), &DestroyAppWorker{Run: run})
}
