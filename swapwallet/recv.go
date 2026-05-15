//go:build walletrpc && swapruntime

package swapwallet

import (
	"context"
	"fmt"

	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletrpc"
)

// receiver drives wallet-layer Recv flows. It is a thin composition over
// the existing swap subserver's StartReceive RPC; the swap subsystem owns
// the invoice signing (with a daemon-managed payment-scoped auth key via
// PR #337), persistence, and lifecycle. swapwallet only normalizes the
// response into a flat WalletEntry and tracks the pending row for the
// wallet-level deadline watcher.
type receiver struct {
	deps    *Deps
	runtime *Runtime
}

// newReceiver constructs a receiver bound to the runtime that will track
// the resulting pending entry.
func newReceiver(deps *Deps, runtime *Runtime) *receiver {
	return &receiver{deps: deps, runtime: runtime}
}

// Recv opens a swap-in session via the daemon-owned swap subserver and
// returns the daemon-signed BOLT-11 invoice plus the initial WalletEntry.
// Background lifecycle (waiting for the LN payer, claiming the vHTLC,
// terminal transitions) is owned by the swap subsystem; the wallet layer
// observes those transitions through the monitor loop and projects them
// onto the WalletEntry shape.
func (r *receiver) Recv(ctx context.Context, req *walletrpc.RecvRequest) (
	*walletrpc.RecvResponse, error) {

	if r == nil || r.deps == nil || r.deps.SwapService == nil {
		return nil, ErrSwapBackendUnavailable
	}

	amt := req.GetAmtSat()
	if amt == 0 {
		return nil, ErrAmountRequired
	}
	// amount_sat in swapclientrpc is int64; reject values that overflow
	// the signed range so we never silently submit a wrapped amount.
	if amt > (1<<63)-1 {
		return nil, fmt.Errorf("%w: amt_sat exceeds int64 range",
			ErrAmountInvalid)
	}

	startResp, err := r.deps.SwapService.StartReceive(
		ctx, &swapclientrpc.StartReceiveRequest{
			AmountSat: int64(amt),
		},
	)
	if err != nil {
		return nil, fmt.Errorf("start receive: %w", err)
	}

	entry := swapEntryFromSummary(
		startResp.GetSwap(), req.GetMemo(), startResp.GetPaymentHash(),
	)

	// Override the kind to RECV regardless of how the SDK reports
	// direction so the row is unambiguous even if the underlying summary
	// is partially populated immediately after StartReceive returns.
	entry.Kind = walletrpc.EntryKind_ENTRY_KIND_RECV

	r.runtime.registerRecvIntent(entry.GetId())

	if entry.Status == walletrpc.EntryStatus_ENTRY_STATUS_PENDING {
		r.runtime.trackPending(
			entry.GetId(), entry.GetKind(),
			unixToTime(
				entry.GetCreatedAtUnix(),
			),
		)
	}

	return &walletrpc.RecvResponse{
		Invoice: startResp.GetInvoice(),
		Entry:   entry,
	}, nil
}
