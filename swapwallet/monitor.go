//go:build walletrpc && swapruntime

package swapwallet

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletrpc"
	"google.golang.org/grpc/metadata"
)

// monitorBackoffMin is the smallest backoff used after a SubscribeSwaps
// failure. Subsequent failures double the backoff up to monitorBackoffMax.
const (
	monitorBackoffMin = 500 * time.Millisecond
	monitorBackoffMax = 30 * time.Second
)

// startMonitorLoop spawns the background goroutine that consumes swap
// updates from the in-process swap subserver and fans them out to wallet
// subscribers as normalized WalletEntry rows. Resilient to transient
// upstream errors: failures back off exponentially up to monitorBackoffMax.
func (r *Runtime) startMonitorLoop() {
	r.wg.Add(1)
	go r.monitorLoop()
}

// monitorLoop is the runtime's swap-update consumer. It subscribes to
// SwapService.SubscribeSwaps in-process via the inlineSwapStream shim,
// normalizes each update into a WalletEntry, projects it onto the runtime's
// canonical-id intent map, and fans the result out to every channel
// registered through subscribe(). The loop survives transient upstream
// errors with bounded exponential backoff and only exits on rootCtx
// cancellation (daemon shutdown).
func (r *Runtime) monitorLoop() {
	defer r.wg.Done()

	log := r.deps.resolveLog()
	if r.deps.SwapService == nil {
		log.WarnS(r.rootCtx, "Monitor loop disabled",
			ErrSwapBackendUnavailable,
		)

		return
	}

	backoff := monitorBackoffMin
	for {
		if r.rootCtx.Err() != nil {
			return
		}

		err := r.runOneSubscription()
		switch {
		case err == nil:
			// Clean stream end: reset backoff before the next
			// attempt so a flaky upstream that briefly drops the
			// connection does not penalize the next try.
			backoff = monitorBackoffMin

		case errors.Is(err, context.Canceled),
			errors.Is(err, io.EOF):

			// Shutdown path: don't log noise.

		default:
			log.WarnS(
				r.rootCtx,
				"Swap subscribe failed; backing off",
				err,
			)

			select {
			case <-r.rootCtx.Done():
				return

			case <-time.After(backoff):
			}

			backoff *= 2
			if backoff > monitorBackoffMax {
				backoff = monitorBackoffMax
			}
		}
	}
}

// runOneSubscription invokes SubscribeSwaps once and forwards every
// pushed update to fanOutSwapUpdate. The method returns when the swap
// service ends the stream (clean nil), when rootCtx is canceled, or
// when the inline stream forwards an error from Send.
func (r *Runtime) runOneSubscription() error {
	stream := newInlineSwapStream(r.rootCtx, r.fanOutSwapUpdate)

	// SubscribeSwaps is implemented as a server-streaming gRPC handler;
	// in-process we call it as a plain Go method with our inline stream
	// adapter. The call blocks until the service returns, which happens
	// when our stream's Send returns an error (rootCtx canceled) or the
	// service shuts down.
	err := r.deps.SwapService.SubscribeSwaps(
		&swapclientrpc.SubscribeSwapsRequest{
			// Snapshot existing rows on each (re)subscribe so a
			// long-running daemon's view stays consistent across
			// transient swap-side restarts.
			IncludeExisting: true,
		},
		stream,
	)

	// rootCtx cancellation maps to context.Canceled; treat io.EOF as a
	// clean end as well so callers don't trip the backoff on it.
	if errors.Is(err, context.Canceled) || err == io.EOF {
		return nil
	}

	return err
}

// fanOutSwapUpdate translates one SubscribeSwapsResponse into a
// WalletEntry, projects the canonical id, applies the deadline overlay,
// updates the pending tracker, and emits to subscribers. Returns an error
// only when rootCtx has been canceled; transient errors do not stop the
// loop.
func (r *Runtime) fanOutSwapUpdate(
	resp *swapclientrpc.SubscribeSwapsResponse) error {

	if r.rootCtx.Err() != nil {
		return r.rootCtx.Err()
	}

	summary := resp.GetSwap()
	if summary == nil {
		return nil
	}

	// The monitor consumes summaries for swaps in both directions, so we
	// let direction-derivation pick the kind here (callers that need a
	// pinned kind — router.sendInvoice and recv.go — pass it explicitly).
	entry := swapEntryFromSummary(
		summary, "", summary.GetPaymentHash(),
		walletrpc.EntryKind_ENTRY_KIND_UNSPECIFIED,
	)
	entry.Id = r.resolveCanonicalID(
		entry.GetId(), summary.GetPaymentHash(), "", nil, "",
	)

	if ov, ok := r.overlayFor(entry.GetId()); ok {
		entry.Status = ov.status
		if ov.failureReason != "" {
			entry.FailureReason = ov.failureReason
		}
	}

	// Keep the pending tracker in sync so the deadline watcher does not
	// keep ageing an entry the swap subsystem has already terminated.
	switch entry.GetStatus() {
	case walletrpc.EntryStatus_ENTRY_STATUS_PENDING:
		r.trackPending(
			entry.GetId(), entry.GetKind(),
			unixToTime(
				entry.GetCreatedAtUnix(),
			),
		)

	case walletrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		walletrpc.EntryStatus_ENTRY_STATUS_FAILED:

		r.clearPending(entry.GetId())
	}

	r.emit(entry)

	return nil
}

// inlineSwapStream adapts SwapClientService.SubscribeSwaps's
// server-streaming gRPC server interface into a plain Go callback so the
// daemon can consume swap updates in-process without dialing itself. Every
// Send delegates to the registered callback; metadata operations are
// no-ops (the in-process consumer has no transport).
type inlineSwapStream struct {
	ctx context.Context //nolint:containedctx

	// send is invoked once per pushed SubscribeSwapsResponse. Returning
	// an error from send causes the gRPC handler to terminate its
	// streaming loop, which is how we unwind on rootCtx cancellation.
	send func(*swapclientrpc.SubscribeSwapsResponse) error
}

// newInlineSwapStream constructs an in-process server-stream adapter
// bound to the given context and per-message callback.
func newInlineSwapStream(ctx context.Context,
	send func(*swapclientrpc.SubscribeSwapsResponse) error,
) *inlineSwapStream {

	return &inlineSwapStream{ctx: ctx, send: send}
}

// Send forwards the response to the callback. When the callback returns
// an error (typically rootCtx cancellation) the gRPC service-side loop
// observes it and terminates.
func (s *inlineSwapStream) Send(
	resp *swapclientrpc.SubscribeSwapsResponse) error {

	return s.send(resp)
}

// Context returns the stream context. The swap service uses this for
// cancellation signalling.
func (s *inlineSwapStream) Context() context.Context {
	return s.ctx
}

// SetHeader is a no-op for the in-process consumer. The wallet layer
// does not propagate gRPC metadata.
func (s *inlineSwapStream) SetHeader(metadata.MD) error { return nil }

// SendHeader is a no-op for the in-process consumer.
func (s *inlineSwapStream) SendHeader(metadata.MD) error { return nil }

// SetTrailer is a no-op for the in-process consumer.
func (s *inlineSwapStream) SetTrailer(metadata.MD) {}

// SendMsg satisfies the grpc.ServerStream interface. In our wiring all
// pushes go through Send; SendMsg is only invoked by the generic
// gRPC framework codepaths we do not exercise here.
func (s *inlineSwapStream) SendMsg(m any) error {
	resp, ok := m.(*swapclientrpc.SubscribeSwapsResponse)
	if !ok {
		return errors.New("inlineSwapStream: unexpected message type")
	}

	return s.Send(resp)
}

// RecvMsg satisfies the grpc.ServerStream interface. The wallet-layer
// consumer never receives on the stream because SubscribeSwaps is
// server-streaming only.
func (s *inlineSwapStream) RecvMsg(any) error {
	return io.EOF
}
