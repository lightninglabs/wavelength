//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lightninglabs/wavelength/credit"
	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

// creditTopupPhaseLabel is the progress phase label stamped on the ledger row
// of the Ark top-up transfer that funded a credit-backed pay.
const creditTopupPhaseLabel = "credit_topup"

// creditProjectorOwnsSwapSummary reports whether this wallet's durable credit
// operation, rather than the generic swap monitor or history reconciler, owns
// a swap activity row. CREDIT settlement identifies the rail, while the local
// ownership set proves the operation was admitted through this wallet's credit
// registry.
func (r *Runtime) creditProjectorOwnsSwapSummary(
	summary *swapclientrpc.SwapSummary) bool {

	if summary == nil || summary.GetSettlementType() != swapclientrpc.
		SwapSettlementType_SWAP_SETTLEMENT_TYPE_CREDIT {
		return false
	}

	r.creditOwnerMu.RLock()
	defer r.creditOwnerMu.RUnlock()

	_, ok := r.creditOwnedSwaps[summary.GetPaymentHash()]

	return ok
}

// markCreditProjectorOwned records a successfully admitted local credit-only
// pay so subsequent swap summaries cannot claim its activity row.
func (r *Runtime) markCreditProjectorOwned(paymentHash string) {
	if paymentHash == "" {
		return
	}

	r.creditOwnerMu.Lock()
	defer r.creditOwnerMu.Unlock()

	r.creditOwnedSwaps[paymentHash] = struct{}{}
}

// recordCreditProjectorOwnership adds credit-only pay operations from a full
// durable registry snapshot. Ownership is monotonic because operations remain
// durable after terminal settlement; merging also preserves an admission
// marker while that operation is concurrently becoming visible to the poll.
func (r *Runtime) recordCreditProjectorOwnership(ops []credit.CreditOpSummary) {
	r.creditOwnerMu.Lock()
	defer r.creditOwnerMu.Unlock()

	for _, op := range ops {
		if op.Kind != credit.KindPay || !op.CreditOnly {
			continue
		}

		paymentHash := strings.TrimPrefix(op.OpKey, "pay:")
		if paymentHash != "" && paymentHash != op.OpKey {
			r.creditOwnedSwaps[paymentHash] = struct{}{}
		}
	}
}

// restoreCreditProjectorOwnership synchronously reconstructs wallet-local
// credit activity ownership during the wallet-ready resume phase. It runs
// before activity backfill and the live monitor start, closing the restart
// window in which a credit-only SDK swap could otherwise project its zero Ark
// funding amount before the asynchronous credit poll sees the durable op.
func (r *Runtime) restoreCreditProjectorOwnership(ctx context.Context) {
	if r.deps.CreditRegistry == nil {
		return
	}

	resp, err := r.deps.CreditRegistry.Ask(
		ctx, &credit.ListCreditOpsRequest{PendingOnly: false},
	).Await(ctx).Unpack()
	if err != nil {
		r.deps.resolveLog().WarnS(
			ctx,
			"Credit ownership restore failed",
			err,
		)

		return
	}

	list, ok := resp.(*credit.ListCreditOpsResponse)
	if !ok {
		r.deps.resolveLog().WarnS(
			ctx,
			"Credit ownership restore got unexpected response",
			fmt.Errorf("got %T", resp),
		)

		return
	}

	r.recordCreditProjectorOwnership(list.Ops)
}

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
	r.recordCreditProjectorOwnership(list.Ops)

	for i := range list.Ops {
		op := list.Ops[i]

		if last, seen := projected[op.OpID]; seen && last == op.State {
			continue
		}

		entry, ok := creditEntryFromSummary(op)
		if !ok {
			continue
		}

		// A completed credit-only send is the proof of payment for
		// L402 and MPP buyers, so its terminal row must carry the
		// preimage. A transient lookup failure leaves the op
		// unprojected so the next tick retries.
		if !r.attachCreditPreimage(op, entry) {
			continue
		}

		// Project the credit row into the canonical activity log before
		// fanning it out. Credit-only sends reach the feed only through
		// this poll (never Runtime.emit from the swap monitor), so
		// without this they would be absent from the store and vanish
		// once the read path cuts over to it (issue #774; the #829
		// class of bug). The store suppresses no-op re-projections, so
		// the coarse re-poll of unchanged terminal rows appends no
		// duplicate events.
		repairLegacyReceive := op.Kind == credit.KindReceive &&
			!op.State.IsTerminal()
		if _, err := r.projectCreditEntry(
			r.rootCtx, entry, repairLegacyReceive,
		); err != nil {

			continue
		}

		projected[op.OpID] = op.State

		// Advance in-memory tracking only after the durable projection
		// succeeds. A failed write remains eligible for the next poll
		// and cannot strand an unchanged terminal operation.
		if op.State.IsTerminal() {
			r.clearPending(entry.GetId())
		} else {
			r.trackPendingEntryWithoutTimeout(entry)
		}
	}
}

// creditLookupWarnAfter is how many consecutive transient preimage lookup
// failures for one send are logged at Debug before the log escalates to Warn.
// The poll runs every creditProjectInterval, so the default surfaces a stuck
// lookup after roughly fifteen seconds.
const creditLookupWarnAfter = 3

// attachCreditPreimage stamps the payment preimage onto the terminal row of a
// credit-only send. The durable swap summary keyed by the same payment hash is
// the source. It returns false only when the lookup failed transiently
// (Unavailable or a deadline) and the caller should retry on the next poll.
//
// A missing swap row or an empty preimage is final rather than retried,
// because the credit op cannot reach Completed before the swap row holds the
// preimage. The credit FSM only leaves payingState after
// CreditServer.StartPay returns (credit/transitions.go:160), and only then
// polls the server ledger for DEBITED (credit/transitions.go:200). StartPay
// runs the SDK pay session synchronously up to SwapCreated
// (sdk/swaps/in_swap.go:499), whose createSwap credit branch sets the preimage
// and persists the swap row in one mutateAndPersist
// (sdk/swaps/in_swap.go:612-617), before StartPay can return. A credit-only
// pay therefore never completes ahead of its preimage, and a miss here means
// the row is genuinely unavailable, which is logged at Warn. Any other lookup
// error is likewise final and projects without a preimage.
func (r *Runtime) attachCreditPreimage(op credit.CreditOpSummary,
	entry *wavewalletrpc.WalletEntry) bool {

	if op.Kind != credit.KindPay || op.State != credit.StateCompleted ||
		r.deps.SwapService == nil {
		return true
	}

	log := r.deps.resolveLog()
	hash := entry.GetProgress().GetPaymentHash()

	ctx, cancel := context.WithTimeout(r.rootCtx, r.creditLookupTimeout)
	defer cancel()

	resp, err := r.deps.SwapService.GetSwap(
		ctx, &swapclientrpc.GetSwapRequest{
			PaymentHash: hash,
		},
	)
	switch {
	case err == nil:
		delete(r.creditLookupFails, hash)

		entry.Progress.Preimage = resp.GetSwap().GetPreimage()
		if entry.Progress.Preimage == "" {
			log.WarnS(r.rootCtx, "Completed credit send has no "+
				"preimage", fmt.Errorf("payment hash %s", hash))
		}

		return true

	case r.rootCtx.Err() != nil:
		return false

	case isTransientLookupErr(err):
		r.creditLookupFails[hash]++
		if r.creditLookupFails[hash] >= creditLookupWarnAfter {
			log.WarnS(r.rootCtx, "Credit projector preimage "+
				"lookup keeps failing", err)
		} else {
			log.DebugS(r.rootCtx, "Credit projector preimage "+
				"lookup failed", err)
		}

		return false
	}

	// The remaining codes (NotFound included) will not heal by retrying,
	// so project the terminal row without a preimage.
	delete(r.creditLookupFails, hash)
	log.WarnS(r.rootCtx, "Completed credit send has no preimage", err)

	return true
}

// isTransientLookupErr reports whether a swap-summary lookup error is worth
// retrying on the next poll.
func isTransientLookupErr(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded:
		return true

	default:
		return false
	}
}

// projectCreditEntry projects one credit operation while preserving the
// global project-and-emit ordering. A resumed receive gets one narrowly scoped
// compatibility attempt to reopen an activity row carrying the exact legacy
// poll-cap failure; all other rows use the ordinary monotonic projector.
func (r *Runtime) projectCreditEntry(ctx context.Context,
	entry *wavewalletrpc.WalletEntry, repairLegacyReceive bool) (int64,
	error) {

	if !repairLegacyReceive || r.deps == nil ||
		r.deps.ActivityStore == nil {
		return r.projectEmitLocked(ctx, entry)
	}

	r.projectMu.Lock()
	defer r.projectMu.Unlock()

	seq, effective, repaired, err := r.repairLegacyCreditReceiveActivity(
		ctx, entry,
	)
	if err != nil {
		r.deps.resolveLog().WarnS(
			ctx,
			"Legacy credit receive activity repair failed",
			err,
		)

		return 0, err
	}
	if !repaired {
		seq, effective, err = r.project(ctx, entry)
		if err != nil {
			return 0, err
		}
	}
	if seq > 0 {
		r.emit(seq, effective)
	}

	return seq, nil
}

// repairLegacyCreditReceiveActivity atomically reopens the user-visible
// activity row only when it is a failed receive carrying the exact legacy
// local poll-cap error. The store keeps the normal terminal-to-pending guard
// for every other row and appends the corrective pending event to history.
func (r *Runtime) repairLegacyCreditReceiveActivity(ctx context.Context,
	entry *wavewalletrpc.WalletEntry) (int64, *wavewalletrpc.WalletEntry,
	bool, error) {

	existingRow, err := r.deps.ActivityStore.GetEntry(ctx, entry.GetId())
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, entry, false, nil

	case err != nil:
		return 0, nil, false, fmt.Errorf("read legacy credit receive "+
			"activity: %w", err)
	}

	existing, err := rowToWalletEntry(existingRow)
	if err != nil {
		return 0, nil, false, fmt.Errorf("decode legacy credit "+
			"receive activity: %w", err)
	}
	if existing.GetKind() != wavewalletrpc.EntryKind_ENTRY_KIND_RECV ||
		existing.GetStatus() != wavewalletrpc.
			EntryStatus_ENTRY_STATUS_FAILED ||
		existing.GetFailureReason() != credit.LegacyReceivePollCapError {
		return 0, entry, false, nil
	}

	effective := mergeActivityContext(existing, entry)
	projection, err := entryToProjection(effective)
	if err != nil {
		return 0, nil, false, fmt.Errorf("encode legacy credit "+
			"receive activity: %w", err)
	}

	seq, err := r.deps.ActivityStore.RepairCreditReceivePollCap(
		ctx, projection,
		int64(wavewalletrpc.EntryStatus_ENTRY_STATUS_FAILED),
		credit.LegacyReceivePollCapError,
	)
	if err != nil {
		return 0, nil, false, err
	}

	return seq, effective, seq > 0, nil
}

// creditEntryFromSummary projects one credit-operation summary onto a
// WalletEntry. It returns ok=false for operations the wallet does not surface
// as activity through this path: mixed pays (the swap monitor owns their
// terminal), redemptions (wallet-internal auto-redeem), and any operation
// whose correlating id cannot be recovered.
func creditEntryFromSummary(op credit.CreditOpSummary) (
	*wavewalletrpc.WalletEntry, bool) {

	var (
		id          string
		kind        wavewalletrpc.EntryKind
		phase       wavewalletrpc.WalletEntryPhase
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
		kind = wavewalletrpc.EntryKind_ENTRY_KIND_SEND
		phase = wavewalletrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_SETTLING
		phaseLabel = "settling"

	case credit.KindReceive:
		// The pending receive row is keyed by the op id.
		id = op.OpID
		kind = wavewalletrpc.EntryKind_ENTRY_KIND_RECV
		phase = wavewalletrpc.
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
	if kind == wavewalletrpc.EntryKind_ENTRY_KIND_SEND {
		amountSat = -op.AmountSat
	}

	now := nowUnix()
	entry := &wavewalletrpc.WalletEntry{
		Id:            id,
		Kind:          kind,
		Status:        wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING,
		AmountSat:     amountSat,
		Counterparty:  creditCounterparty,
		UpdatedAtUnix: now,
		Progress: &wavewalletrpc.WalletEntryProgress{
			Phase:       phase,
			PhaseLabel:  phaseLabel,
			PaymentHash: paymentHash,
		},
	}

	switch op.State {
	case credit.StateCompleted:
		entry.Status = wavewalletrpc.EntryStatus_ENTRY_STATUS_COMPLETE
		entry.Progress.Phase = wavewalletrpc.
			WalletEntryPhase_WALLET_ENTRY_PHASE_CONFIRMED
		entry.Progress.PhaseLabel = "confirmed"

	case credit.StateFailed:
		entry.Status = wavewalletrpc.EntryStatus_ENTRY_STATUS_FAILED
		entry.Progress.Phase = wavewalletrpc.
			WalletEntryPhase_WALLET_ENTRY_PHASE_FAILED
		entry.Progress.PhaseLabel = "failed"
		entry.FailureReason = op.LastError
		entry.FailureCode = wavewalletrpc.
			EntryFailureCode_ENTRY_FAILURE_CODE_FAILED.Enum()
	}

	return entry, true
}
