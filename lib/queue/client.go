package queue

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// Client wraps a *river.Client[pgx.Tx]. We keep the wrapper thin so we
// can swap drivers later (River's roadmap mentions database/sql support)
// without touching every caller.
//
// Client is safe for concurrent use; River's own client is.
type Client struct {
	inner *river.Client[pgx.Tx]
	mode  bool
}

// Insert enqueues a job using the client's pool. Wraps River's Insert
// so callers don't have to import river or rivertype.
func (c *Client) Insert(ctx context.Context, args river.JobArgs) (*rivertype.JobInsertResult, error) {
	if c == nil {
		return nil, errors.New("queue: nil client")
	}
	res, err := c.inner.Insert(ctx, args, nil)
	if err != nil {
		return nil, fmt.Errorf("queue: insert %s: %w", args.Kind(), err)
	}
	return res, nil
}

// Start begins fetching and working jobs. Returns an error if the
// client wasn't built with WorkerMode. The caller's ctx is what River
// passes to each Worker.Work — cancel ctx to drain.
func (c *Client) Start(ctx context.Context) error {
	if !c.mode {
		return errors.New("queue: insert-only client cannot Start")
	}
	return c.inner.Start(ctx)
}

// Stop blocks until in-flight jobs settle or ctx fires. Mirrors
// river.Client.Stop. Safe to call on insert-only clients (no-op).
func (c *Client) Stop(ctx context.Context) error {
	if !c.mode {
		return nil
	}
	return c.inner.Stop(ctx)
}
