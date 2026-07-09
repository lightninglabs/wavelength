//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"time"

	"github.com/lightninglabs/darepo-client/daemonrpc"
)

// defaultTipPollInterval is how often the startup reconcile-at-tip loop polls
// the daemon's best-block height to decide whether the chain backend has
// caught up. Polling GetInfo is cheap (a best-block query), so the cadence is
// tight to minimize the post-restart window in which a settled operation still
// reads PENDING.
const defaultTipPollInterval = 5 * time.Second

// defaultTipReconcileTimeout bounds how long the startup loop waits for the
// backend to settle before reconciling once anyway and handing off to the
// periodic reconciler. It is a backstop against a wallet that never reaches a
// stable tip (for example a stuck sync): the coarse periodic reconciler still
// lands terminal transitions eventually, so the loop must not poll forever.
const defaultTipReconcileTimeout = 2 * time.Minute

// startTipReconcileLoop starts the one-shot startup reconcile-at-tip goroutine.
// It is a no-op without a canonical store (the derive-on-read fallback already
// reflects live state on every read) or without an RPCServer to poll for sync
// progress.
func (r *Runtime) startTipReconcileLoop() {
	if r.deps == nil || r.deps.ActivityStore == nil ||
		r.deps.RPCServer == nil {
		return
	}

	r.wg.Add(1)
	go r.tipReconcileLoop()
}

// tipReconcileLoop waits until the daemon's chain backend has caught up to the
// current tip after startup, then runs a single reconcile pass so a terminal
// transition that landed while the daemon was down — a confirmed boarding
// deposit, a swept exit — surfaces immediately, rather than reading PENDING
// until the periodic reconciler's first tick a full reconcileInterval later.
//
// The startup backfill (backfillActivity in resumeAll) already seeded the
// store, but in lwwallet/lnd modes the wallet is marked ready before the chain
// backend has synced forward, so that backfill re-derives pre-tip state. This
// loop re-derives once more once the backend settles. It is one-shot: after
// the pass (or the timeout backstop) it returns, and the periodic reconciler
// owns steady-state catch-up.
//
// The pass reconciles the same kinds as the periodic reconciler (DEPOSIT /
// EXIT): those are the backfill-only producers with no live source, so they
// are what can read stale after a restart. SEND/RECV self-heal through the
// monitor's live SubscribeSwaps stream as the backend catches up, so the tip
// pass need not re-derive them.
//
// "Caught up" is a heuristic over the only signal the wallet RPC surface
// exposes: the best-block height has stopped advancing across a poll interval
// and the wallet state is READY. This detects the chain backend reaching the
// tip; a small residual wallet-processing lag is covered by the periodic
// reconciler. A precise wallet-synced-to-tip signal is a follow-up (#899).
func (r *Runtime) tipReconcileLoop() {
	defer r.wg.Done()

	ticker := time.NewTicker(r.tipPollInterval)
	defer ticker.Stop()

	deadline := time.NewTimer(r.tipReconcileTimeout)
	defer deadline.Stop()

	// lastHeight is the best-block height seen on the previous poll;
	// observed guards the first comparison so an initial coincidental match
	// against the zero value cannot be mistaken for a settled tip.
	var (
		lastHeight uint32
		observed   bool
	)

	for {
		select {
		case <-r.rootCtx.Done():
			return

		case <-deadline.C:
			// The backend never settled within the window (for
			// example a slow or stuck sync). Reconcile once anyway;
			// the periodic reconciler continues from here.
			r.reconcileActivity(r.rootCtx)

			return

		case <-ticker.C:
			height, ready, ok := r.chainTipStatus(r.rootCtx)
			if !ok {
				continue
			}

			// Caught up once the wallet is usable and the best
			// block has stopped advancing across a poll interval.
			if ready && observed && height == lastHeight {
				r.reconcileActivity(r.rootCtx)

				return
			}

			lastHeight = height
			observed = true
		}
	}
}

// chainTipStatus reads the daemon's current best-block height and whether the
// wallet state is READY. ok is false on an RPC error, so the caller simply
// retries on the next poll rather than treating a transient failure as a
// settled tip.
func (r *Runtime) chainTipStatus(ctx context.Context) (uint32, bool, bool) {
	info, err := r.deps.RPCServer.GetInfo(ctx, &daemonrpc.GetInfoRequest{})
	if err != nil {
		return 0, false, false
	}

	ready := info.GetWalletState() ==
		daemonrpc.WalletState_WALLET_STATE_READY

	return info.GetBlockHeight(), ready, true
}
