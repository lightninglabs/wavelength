//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"fmt"
	"strings"
	"time"

	"github.com/lightninglabs/wavelength/credit"
	"github.com/lightninglabs/wavelength/rpc/walletdkrpc"
)

// creditProjectInterval is how often the projector polls the credit registry
// for durable credit-operation state. The poll is coarse on purpose: a
// credit-only pay or a credit receive flipping to terminal a few seconds after
// the server settles it is acceptable, and a coarse tick keeps background load
// low. Live subscribers also receive the original pending row synchronously at
// Send/Recv time, so the projector only has to surface the eventual terminal
// transition.
const creditProjectInterval = 5 * time.Second

// creditCounterparty is the synthetic counterparty label the wallet uses for
// credit-backed rows, matching the pending entries the router and receiver
// emit so the projected terminal update lands on the same row.
const creditCounterparty = "credit"

// startCreditProjectorLoop spawns the background goroutine that projects
// durable credit-operation terminal state onto wallet rows. It is a no-op when
// no credit registry was published, so non-credit builds and deployments pay
// nothing.
func (r *Runtime) startCreditProjectorLoop() {
	if r.deps.CreditRegistry == nil {
		return
	}

	r.wg.Add(1)
	go r.creditProjectorLoop()
}

// creditProjectorLoop polls the credit registry on a coarse tick and projects
// each credit operation onto a WalletEntry. Credit-only pays and credit
// receives have no swap session feeding the monitor loop, so this poll is the
// only path that transitions their pending wallet rows to a terminal status.
// Mixed pays are deliberately skipped: their shared payment-hash row stays
// owned by the swap monitor, which is the single terminal authority for the
// Lightning leg. The loop only exits on rootCtx cancellation (daemon
// shutdown).
func (r *Runtime) creditProjectorLoop() {
	defer r.wg.Done()

	// projected remembers the last FSM state emitted for each op id so the
	// loop only fans out a WalletEntry when an operation's state actually
	// changed. It is process-local: on restart it starts empty, so the
	// first poll re-emits the durable state of every in-flight operation
	// (re-tracking pending rows and projecting any that already settled
	// while the daemon was down).
	projected := make(map[string]credit.State)

	ticker := time.NewTicker(creditProjectInterval)
	defer ticker.Stop()

	// Project once immediately so a restart reconciles in-flight credit
	// operations without waiting a full tick.
	r.pollCreditOps(projected)

	for {
		select {
		case <-r.rootCtx.Done():
			return

		case <-ticker.C:
			r.pollCreditOps(projected)
		}
	}
}

// pollCreditOps asks the registry for the full credit-operation snapshot and
// projects every operation whose state changed since the last poll. Transient
// errors are logged and dropped; the next tick retries.
func (r *Runtime) pollCreditOps(projected map[string]credit.State) {
	log := r.deps.resolveLog()

	resp, err := r.deps.CreditRegistry.Ask(
		r.rootCtx, &credit.ListCreditOpsRequest{PendingOnly: false},
	).Await(r.rootCtx).Unpack()
	if err != nil {
		if r.rootCtx.Err() == nil {
			log.WarnS(r.rootCtx, "Credit projector list failed",
				err,
			)
		}

		return
	}

	list, ok := resp.(*credit.ListCreditOpsResponse)
	if !ok {
		log.WarnS(r.rootCtx, "Credit projector got unexpected response",
			fmt.Errorf("got %T", resp),
		)

		return
	}

	for i := range list.Ops {
		op := list.Ops[i]

		if last, seen := projected[op.OpID]; seen && last == op.State {
			continue
		}

		entry, ok := creditEntryFromSummary(op)
		if !ok {
			continue
		}

		projected[op.OpID] = op.State

		// A terminal op clears wallet-local pending tracking so the
		// deadline watcher stops ageing the row; a still-pending op is
		// re-tracked so it survives a restart and appears in List
		// snapshots even though the runtime's pending map is in-memory.
		if op.State.IsTerminal() {
			r.clearPending(entry.GetId())
		} else {
			r.trackPendingEntryWithoutTimeout(entry)
		}

		// Project the credit row into the canonical activity log before
		// fanning it out. Credit-only sends reach the feed only through
		// this poll (never Runtime.emit from the swap monitor), so
		// without this they would be absent from the store and vanish
		// once the read path cuts over to it (issue #774; the #829
		// class of bug). The store suppresses no-op re-projections, so
		// the coarse re-poll of unchanged terminal rows appends no
		// duplicate events.
		r.projectAndEmit(r.rootCtx, entry)
	}
}

// creditEntryFromSummary projects one credit-operation summary onto a
// WalletEntry. It returns ok=false for operations the wallet does not surface
// as activity through this path: mixed pays (the swap monitor owns their
// terminal), redemptions (wallet-internal auto-redeem), and any operation
// whose correlating id cannot be recovered.
func creditEntryFromSummary(op credit.CreditOpSummary) (
	*walletdkrpc.WalletEntry, bool) {

	var (
		id          string
		kind        walletdkrpc.EntryKind
		phase       walletdkrpc.WalletEntryPhase
		phaseLabel  string
		paymentHash string
	)

	switch op.Kind {
	case credit.KindPay:
		// Only credit-only pays are owned by the projector. A mixed pay
		// shares its payment-hash row with a swap session that the
		// monitor loop drives to terminal, so projecting it here would
		// race the swap FSM.
		if !op.CreditOnly {
			return nil, false
		}

		// The pending pay row is keyed by the payment-hash hex, which
		// the op key carries verbatim as "pay:<payment_hash_hex>".
		id = strings.TrimPrefix(op.OpKey, "pay:")
		paymentHash = id
		kind = walletdkrpc.EntryKind_ENTRY_KIND_SEND
		phase = walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_SETTLING
		phaseLabel = "settling"

	case credit.KindReceive:
		// The pending receive row is keyed by the op id.
		id = op.OpID
		kind = walletdkrpc.EntryKind_ENTRY_KIND_RECV
		phase = walletdkrpc.
			WalletEntryPhase_WALLET_ENTRY_PHASE_WAITING_FOR_PAYMENT
		phaseLabel = "waiting_for_payment"

	default:
		// Redeem and any future kinds are wallet-internal and not
		// surfaced as user activity.
		return nil, false
	}

	if id == "" {
		return nil, false
	}

	// A SEND is an outflow, so it carries a negative amount to match the
	// sign convention of every other outgoing row (normal swap sends are
	// normalized to negative by swapEntryFromSummary). Credit-only sends
	// reach the feed only through this projector, so without the flip a
	// sub-dust pay renders as positive and looks like an incoming transfer
	// (issue #829).
	amountSat := op.AmountSat
	if kind == walletdkrpc.EntryKind_ENTRY_KIND_SEND {
		amountSat = -op.AmountSat
	}

	now := nowUnix()
	entry := &walletdkrpc.WalletEntry{
		Id:            id,
		Kind:          kind,
		Status:        walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		AmountSat:     amountSat,
		Counterparty:  creditCounterparty,
		UpdatedAtUnix: now,
		Progress: &walletdkrpc.WalletEntryProgress{
			Phase:       phase,
			PhaseLabel:  phaseLabel,
			PaymentHash: paymentHash,
		},
	}

	switch op.State {
	case credit.StateCompleted:
		entry.Status = walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE
		entry.Progress.Phase = walletdkrpc.
			WalletEntryPhase_WALLET_ENTRY_PHASE_CONFIRMED
		entry.Progress.PhaseLabel = "confirmed"

	case credit.StateFailed:
		entry.Status = walletdkrpc.EntryStatus_ENTRY_STATUS_FAILED
		entry.Progress.Phase = walletdkrpc.
			WalletEntryPhase_WALLET_ENTRY_PHASE_FAILED
		entry.Progress.PhaseLabel = "failed"
		entry.FailureReason = op.LastError
		entry.FailureCode = walletdkrpc.
			EntryFailureCode_ENTRY_FAILURE_CODE_FAILED.Enum()
	}

	return entry, true
}
