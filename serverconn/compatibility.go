package serverconn

import (
	"context"
	"errors"
	"fmt"

	mailboxconn "github.com/lightninglabs/wavelength/mailbox/conn"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
)

// edgeStatusCarrier is implemented by every mailbox edge RPC response
// (SendResponse, PullResponse, AckUpToResponse). Its generated GetStatus
// accessor is nil-safe, but callers dereference other response fields, so
// edgeResponseError also guards against a nil response.
type edgeStatusCarrier interface {
	comparable

	GetStatus() *mailboxpb.Status
}

// edgeResponseError centralizes the version/compatibility check that every edge
// send and receive path would otherwise repeat. It maps an edge RPC outcome to
// the single error a caller should return, or nil when resp is usable:
//
//   - a transport error is wrapped with op;
//   - a nil response (e.g. from a mock edge) is reported as a transport error;
//   - a non-OK mailbox Status becomes a *mailboxconn.StatusError tagged with
//     op, so a permanent version status classifies and surfaces upstream.
//
// It deliberately does not drive the incompatibility transition itself: callers
// pass the returned error to checkPermanentStatus wherever a permanent failure
// must shed the client, so the send paths and the ingress loop keep their
// existing transition points.
func edgeResponseError[T edgeStatusCarrier](op string, resp T,
	err error) error {

	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}

	var nilResp T
	if resp == nilResp {
		return fmt.Errorf("%s: nil response", op)
	}

	if st := resp.GetStatus(); st != nil && !st.Ok {
		return mailboxconn.NewStatusError(op, st)
	}

	return nil
}

// compatibilityError returns the cached permanent version error if the
// connector has transitioned to the terminal INCOMPATIBLE state, or nil while
// it is still COMPATIBLE. Send paths consult this before contacting the edge.
func (a *ServerConnectionActor) compatibilityError() *mailboxconn.StatusError {
	return a.compatErr.Load()
}

// checkPermanentStatus inspects an error returned by an edge operation. If it
// is a permanent version StatusError, it drives the terminal incompatibility
// transition and returns true. Transient and non-status errors return false so
// the existing retry policy still applies.
func (a *ServerConnectionActor) checkPermanentStatus(ctx context.Context,
	err error) bool {

	if err == nil {
		return false
	}

	var statusErr *mailboxconn.StatusError
	if !errors.As(err, &statusErr) || !statusErr.IsPermanentVersion() {
		return false
	}

	a.markIncompatible(ctx, statusErr)

	return true
}

// markIncompatible performs the one-shot transition to the terminal
// INCOMPATIBLE state. It caches the typed error, cancels ingress and heartbeat
// processing asynchronously (so a calling ingress or heartbeat goroutine never
// waits for itself), fails all pending unary waiters with the same error, and
// invokes the optional compatibility callback exactly once. Subsequent calls
// are no-ops.
func (a *ServerConnectionActor) markIncompatible(ctx context.Context,
	statusErr *mailboxconn.StatusError) {

	if statusErr == nil {
		return
	}

	a.compatOnce.Do(func() {
		// Cache the error first so any concurrent send observes the
		// terminal state immediately.
		a.compatErr.Store(statusErr)

		a.log.WarnS(ctx, "Connector became incompatible", statusErr)

		// Cancel ingress and heartbeat without joining: this may run on
		// the ingress goroutine itself. CancelFunc is idempotent.
		if cancel := a.ingressCancel.Load(); cancel != nil {
			(*cancel)()
		}

		// Fail every in-flight unary waiter so no caller blocks on a
		// response that will never arrive.
		a.responseRegistry.FailAll(statusErr)

		// Notify the owner exactly once.
		if a.cfg.OnIncompatible != nil {
			a.cfg.OnIncompatible(statusErr)
		}
	})
}
