package client

import (
	"context"
	"time"
)

// acquire reserves active admission or a bounded waiting place. No connection
// is opened until this succeeds. QueueTimeout only applies while waiting; it
// must not become the lifetime deadline of an admitted stream. Admission is not
// FIFO: a new caller may win an available slot before a waiting caller.
func (c *Client) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return opError(ctx, "admit", err)
	}
	select {
	case c.slots <- struct{}{}:
		return nil
	default:
	}
	select {
	case c.waiters <- struct{}{}:
		defer func() { <-c.waiters }()
	default:
		return opError(ctx, "admit", ErrBusy)
	}
	timer := time.NewTimer(c.config.QueueTimeout)
	defer timer.Stop()
	select {
	case c.slots <- struct{}{}:
		// A ready slot and deadline can race. Never retain admission after
		// observing cancellation or expiry of the waiting budget.
		if err := ctx.Err(); err != nil {
			<-c.slots
			return opError(ctx, "admit", err)
		}
		select {
		case <-timer.C:
			<-c.slots
			return opError(ctx, "admit", ErrBusy)
		default:
			return nil
		}
	case <-ctx.Done():
		return opError(ctx, "admit", ctx.Err())
	case <-timer.C:
		return opError(ctx, "admit", ErrBusy)
	}
}
