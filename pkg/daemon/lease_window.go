package daemon

import "context"

type leaseWindowKey struct{}

type requestLeaseWindow struct {
	ended  <-chan struct{}
	client context.Context
}

// WithLeaseWindow carries the client's original context and the end of its lease.
// An idempotent request may outlive its transport, but ends with its lease.
func WithLeaseWindow(client context.Context, ended <-chan struct{}) context.Context {
	return context.WithValue(client, leaseWindowKey{}, requestLeaseWindow{ended: ended, client: client})
}

func leaseEnded(ctx context.Context) <-chan struct{} {
	window, _ := ctx.Value(leaseWindowKey{}).(requestLeaseWindow)
	return window.ended
}

// clientContext is ctx without the lease's end: the context of the
// client's request, which ends only when the client goes (or, over TCP
// with a request id, after the grace). ctx itself when no lease is on it.
func clientContext(ctx context.Context) context.Context {
	if v, ok := ctx.Value(leaseWindowKey{}).(requestLeaseWindow); ok {
		return v.client
	}
	return ctx
}

// ContextUntilLeaseEnds returns a child of ctx that is cancelled as soon as lease
// e ends. cancel releases the watch.
func ContextUntilLeaseEnds(ctx context.Context, ended <-chan struct{}) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-ended:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}
