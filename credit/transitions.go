package credit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/metrics"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// emit builds a state transition to next carrying the given outbox directives.
// The credit FSM never chains via protofsm internal events: the durable
// per-operation actor re-drives one step at a time so it can flush a
// stageRecord checkpoint between a state that records a server identifier and
// the next state that depends on it, and between a state that performs a side
// effect and the awaiting state whose first ledger read follows it. A
// transition with no outbox is a plain advance the actor continues from; a
// parkOp stops the turn; a terminal NextState ends the FSM.
func emit(next CreditState, out ...CreditOutMsg) (*CreditTransition, error) {
	t := &CreditTransition{NextState: next}
	if len(out) > 0 {
		t.NewEvents = fn.Some(CreditEmittedEvent{Outbox: out})
	}

	return t, nil
}

// fail records a deterministic terminal failure and transitions to the terminal
// failed state. A deterministic failure (bad server contract, empty allocation,
// corrupt row) must terminal-fail rather than redeliver forever.
func (b *opBehavior) fail(ctx context.Context, reason string) (
	*CreditTransition, error) {

	b.logger(ctx).WarnS(ctx, "Credit operation failed",
		nil,
		slog.String("op_id", b.cfg.OpID),
		slog.String("op_key", b.rec.OpKey),
		slog.String("reason", reason),
	)
	b.rec.LastError = reason

	return emit(&failedState{})
}

// ProcessEvent for quotingState decides whether the pay needs an Ark top-up. A
// zero top-up means the account already holds enough credits, so the FSM jumps
// straight to paying.
func (quotingState) ProcessEvent(_ context.Context, _ CreditEvent,
	b *opBehavior) (*CreditTransition, error) {

	if b.rec.TopupSat == 0 {
		return emit(&payingState{})
	}

	return emit(&topupCreatingState{})
}

// ProcessEvent for topupCreatingState creates (or reuses, by op key) the server
// ARK_TOPUP funding operation and records the server-owned destination the OOR
// transfer must fund. It advances to funding and emits a stageRecord so the
// destination is durably checkpointed before the OOR send: a crash before the
// turn commits then re-drives from funding (re-issuing only the idempotent OOR
// send) rather than re-creating the credit.
func (topupCreatingState) ProcessEvent(ctx context.Context, _ CreditEvent,
	b *opBehavior) (*CreditTransition, error) {

	acctKey, err := b.accountKey(ctx)
	if err != nil {
		return nil, err
	}

	res, err := b.cfg.Server.CreateCredit(
		ctx, acctKey, b.rec.OpKey, SourceArkTopUp,
		uint64(b.rec.TopupSat), "",
	)
	if errors.Is(err, ErrTopUpRejected) {
		return b.fail(ctx, fmt.Sprintf("top-up rejected: %v", err))
	}
	if err != nil {
		return nil, fmt.Errorf("create top-up credit: %w", err)
	}
	if res.State.IsTerminalFailure() {
		return b.fail(
			ctx, fmt.Sprintf("top-up credit ended in %s",
				res.State),
		)
	}
	if len(res.DestinationPubkey) == 0 {
		return b.fail(ctx, "top-up credit missing destination pubkey")
	}

	b.rec.ServerOpID = res.OperationID
	b.rec.DestinationPubkey = append([]byte(nil), res.DestinationPubkey...)

	return emit(&topupFundingState{}, &stageRecord{})
}

// isFundingAdmissionRejected reports a daemon send rejection that happens
// before any input is selected or locked: insufficient spendable funds
// (ResourceExhausted), liquidity held by another in-flight operation
// (Aborted), or a selection whose change would fall below the minimum VTXO
// amount (OutOfRange). None guarantees by itself that this operation has not
// already funded the top-up, since the lock may belong to its own earlier
// attempt.
func isFundingAdmissionRejected(err error) bool {
	switch status.Code(err) {
	case codes.ResourceExhausted, codes.Aborted, codes.OutOfRange:
		return true

	default:
		return false
	}
}

// ProcessEvent for topupFundingState submits the OOR transfer that funds the
// top-up. The op key is the OOR idempotency key, so a re-issued send never
// produces a second transfer.
//
// The daemon can refuse the send for lack of spendable funds, because the
// liquidity is locked, or because the top-up would leave sub-minimum change.
// Retrying would leave the operation pending until funds appear and then pay
// an invoice the caller gave up on long before. The refusal is therefore
// terminal, unless this operation's own earlier send was accepted and only its
// response was lost: that attempt holds the very locks that cause the refusal,
// so an existing-only probe on the same key decides whether to adopt the
// transfer or fail.
func (topupFundingState) ProcessEvent(ctx context.Context, _ CreditEvent,
	b *opBehavior) (*CreditTransition, error) {

	sessionID, err := b.cfg.Daemon.SendOOR(
		ctx, b.rec.DestinationPubkey, uint64(b.rec.TopupSat),
		b.rec.OpKey, false,
	)
	if err != nil && isFundingAdmissionRejected(err) {
		rejection := err
		sessionID, err = b.cfg.Daemon.SendOOR(
			ctx, b.rec.DestinationPubkey, uint64(b.rec.TopupSat),
			b.rec.OpKey, true,
		)
		if status.Code(err) == codes.NotFound {
			return b.fail(
				ctx, fmt.Sprintf("fund top-up via OOR "+
					"rejected: %v", rejection),
			)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("fund top-up via OOR: %w", err)
	}

	b.rec.OORSessionID = sessionID
	b.logger(ctx).InfoS(ctx, "Credit top-up funded, awaiting credit",
		slog.String("op_id", b.cfg.OpID),
		slog.String("oor_session_id", sessionID),
		slog.Int64("topup_sat", b.rec.TopupSat),
	)

	// Checkpoint the advance before the awaiting state reads the server
	// ledger: a failed read is a postpone, and redelivery has to resume in
	// the awaiting state rather than re-issue the OOR send.
	return emit(&topupAwaitingCreditState{}, &stageRecord{})
}

// ProcessEvent for topupAwaitingCreditState reconciles against the server
// ledger: the top-up is done once the server marks the funding operation
// CREDITED. It gates only on the funding op reaching CREDITED, not on
// AvailableSat clearing the StartPay cap (which can be the sentinel max for a
// must-use-credit pay and would park the operation forever); StartPay reserves
// what it needs and surfaces any shortfall as a deterministic pay failure.
func (topupAwaitingCreditState) ProcessEvent(ctx context.Context, _ CreditEvent,
	b *opBehavior) (*CreditTransition, error) {

	snapshot, err := b.listCredits(ctx)
	if err != nil {
		return nil, b.postponeListCreditsFetch(ctx, err)
	}

	state, found := serverOpState(snapshot, b.rec.ServerOpID)
	if found && state.IsTerminalFailure() {
		return b.fail(
			ctx, fmt.Sprintf("top-up funding ended in %s", state),
		)
	}
	if found && state == ServerStateCredited {
		return emit(&payingState{})
	}

	if b.awaitExhausted() {
		return b.fail(
			ctx, "top-up credit not finalized within poll cap",
		)
	}

	return emit(&topupAwaitingCreditState{}, &parkOp{})
}

// ProcessEvent for payingState starts the credit or mixed pay. StartPay is
// idempotent by payment hash, so a re-issued call reuses the already-started
// pay session. A pay the swap client refuses outright is terminal for the
// operation: retrying a deterministic rejection would leave it pending forever
// instead of surfacing FAILED to the caller. Transport and other ambiguous
// errors stay retryable. A credit-only pay then awaits settlement against the
// server ledger; a mixed pay hands terminal authority to the swap monitor and
// completes on hand-off.
func (payingState) ProcessEvent(ctx context.Context, _ CreditEvent,
	b *opBehavior) (*CreditTransition, error) {

	err := b.cfg.Server.StartPay(
		ctx, b.rec.Invoice, uint64(b.rec.MaxFeeSat),
		uint64(b.rec.MaxCreditSat),
	)
	if errors.Is(err, ErrPayRejected) {
		return b.fail(ctx, fmt.Sprintf("start pay rejected: %v", err))
	}
	if err != nil {
		return nil, fmt.Errorf("start pay: %w", err)
	}

	b.logger(ctx).InfoS(ctx, "Credit pay started",
		slog.String("op_id", b.cfg.OpID),
		slog.Int64("max_credit_sat", b.rec.MaxCreditSat),
	)

	// A credit-only pay has no Lightning swap leg, so the credit FSM is the
	// sole authority for its outcome: reconcile it to settlement against
	// the server ledger instead of declaring success the instant StartPay
	// is accepted. A mixed pay's outcome is owned by the swap monitor, so
	// it still completes on hand-off. The pay payment hash is already
	// persisted (set at admission), so it correlates the pay operation in
	// ListCredits across a restart.
	if b.creditOnly && len(b.rec.PaymentHash) > 0 {

		// Checkpoint the advance before the awaiting state reads the
		// server ledger: a failed read is a postpone, and redelivery
		// has to resume in the awaiting state rather than re-issue
		// StartPay.
		return emit(&payAwaitingSettlementState{}, &stageRecord{})
	}

	return emit(&completedState{})
}

// ProcessEvent for payAwaitingSettlementState reconciles a credit-only pay
// against the server ledger by matching the invoice payment hash to the server
// pay operation: the pay is done once the server marks it DEBITED (success),
// and fails once the server marks it RELEASED/FAILED/EXPIRED. Until then it
// parks on a poll timer.
func (payAwaitingSettlementState) ProcessEvent(ctx context.Context,
	_ CreditEvent, b *opBehavior) (*CreditTransition, error) {

	snapshot, err := b.listCredits(ctx)
	if err != nil {
		return nil, b.postponeListCreditsFetch(ctx, err)
	}

	state, found := serverOpStateByPaymentHash(snapshot, b.rec.PaymentHash)
	if found && state == ServerStateDebited {
		b.logger(ctx).InfoS(ctx, "Credit pay settled",
			slog.String("op_id", b.cfg.OpID),
		)

		return emit(&completedState{})
	}
	if found && state.IsTerminalFailure() {
		return b.fail(ctx, fmt.Sprintf("credit pay ended in %s", state))
	}

	if b.awaitExhausted() {
		return b.fail(ctx, "credit pay not settled within poll cap")
	}

	return emit(&payAwaitingSettlementState{}, &parkOp{})
}

// ProcessEvent for receiveCreatingState creates the server-owned Lightning
// receive invoice that credits the account on settlement. In steady state the
// registry pre-creates the invoice at admission and the spawned child enters at
// awaiting settlement; this state is the resume/fallback path.
func (receiveCreatingState) ProcessEvent(ctx context.Context, _ CreditEvent,
	b *opBehavior) (*CreditTransition, error) {

	acctKey, err := b.accountKey(ctx)
	if err != nil {
		return nil, err
	}

	res, err := b.cfg.Server.CreateCredit(
		ctx, acctKey, b.rec.OpKey, SourceLightningReceive,
		uint64(b.rec.AmountSat), b.receiveMemo,
	)
	if err != nil {
		return nil, fmt.Errorf("create receive credit: %w", err)
	}
	if res.State.IsTerminalFailure() {
		return b.fail(
			ctx, fmt.Sprintf("receive credit ended in %s",
				res.State),
		)
	}
	if res.Invoice == "" {
		return b.fail(ctx, "receive credit missing invoice")
	}

	b.rec.ServerOpID = res.OperationID
	b.rec.Invoice = res.Invoice
	if len(res.PaymentHash) > 0 {
		b.rec.PaymentHash = append([]byte(nil), res.PaymentHash...)
	}
	b.logger(ctx).InfoS(ctx, "Credit receive invoice created",
		slog.String("op_id", b.cfg.OpID),
	)

	// Checkpoint the created invoice before the awaiting state reads the
	// server ledger: a failed read is a postpone, and redelivery has to
	// resume in the awaiting state rather than create the credit again.
	return emit(&awaitingSettlementState{}, &stageRecord{})
}

// ProcessEvent for awaitingSettlementState reconciles a receive against the
// server ledger: the receive is done once the server marks the operation
// CREDITED. On settlement it evaluates the wallet-owned auto-redeem watermark
// and, if the earmark-adjusted available balance now clears the threshold,
// emits a triggerRedeem so the registry can materialize the credits — folding
// the auto-redeem decision into the receive state machine instead of a periodic
// background sweep. The server owns the invoice lifecycle, so only its
// CREDITED/EXPIRED/FAILED/RELEASED state can terminate the receive. A client
// poll count cannot prove that a still-payable invoice failed.
func (awaitingSettlementState) ProcessEvent(ctx context.Context, _ CreditEvent,
	b *opBehavior) (*CreditTransition, error) {

	snapshot, err := b.listCredits(ctx)
	if err != nil {
		return nil, b.postponeListCreditsFetch(ctx, err)
	}

	state, found := serverOpState(snapshot, b.rec.ServerOpID)
	if found && state.IsTerminalFailure() {
		return b.fail(
			ctx, fmt.Sprintf("receive funding ended in %s", state),
		)
	}
	if found && state == ServerStateCredited {
		// The receive settled and credited the account. Consider an
		// auto-redeem: when the earmark-adjusted available balance
		// clears the watermark, signal the registry, which arbitrates
		// the no-pending-pay/redeem interlock and admits the redeem.
		// The receive only signals intent, so it never blocks on the
		// redeem.
		var out []CreditOutMsg
		if available, ok := b.redeemWatermarkCleared(
			ctx, snapshot,
		); ok {

			out = append(
				out, &triggerRedeem{
					AvailableSat: available,
				},
			)
		}

		return emit(&completedState{}, out...)
	}

	return emit(&awaitingSettlementState{}, &parkOp{})
}

// ProcessEvent for redeemReservingState allocates a wallet-owned destination
// for the redemption payout. The destination is checkpointed (stageRecord)
// before the FSM advances to submitting, so a crash between the allocation and
// the RedeemCredit reservation re-drives against the same destination the
// server reservation will bind to, rather than allocating a fresh script the
// chain-watch would never match. A resumed operation that already recorded a
// destination skips straight to submitting.
func (redeemReservingState) ProcessEvent(ctx context.Context, _ CreditEvent,
	b *opBehavior) (*CreditTransition, error) {

	if len(b.rec.DestinationPubkey) > 0 && len(b.redeemPkScript) > 0 {

		// Destination already allocated and checkpointed (resume):
		// advance to submitting without re-allocating or re-staging.
		return emit(&redeemSubmittingState{})
	}

	pubKey, pkScript, err := b.cfg.Daemon.AllocateReceiveScript(
		ctx, "credit-redeem",
	)
	if err != nil {
		return nil, fmt.Errorf("allocate redeem destination: %w", err)
	}
	if len(pubKey) == 0 || len(pkScript) == 0 {
		return b.fail(ctx, "redeem destination is empty")
	}

	b.rec.DestinationPubkey = append([]byte(nil), pubKey...)
	b.redeemPkScript = append([]byte(nil), pkScript...)

	return emit(&redeemSubmittingState{}, &stageRecord{})
}

// ProcessEvent for redeemSubmittingState reserves the redemption with the
// server against the checkpointed destination. The op key is the idempotency
// key, so a re-issued reservation reuses the same server operation. A
// redemption the server refuses before creating anything is terminal for the
// operation: retrying a deterministic rejection would leave it pending forever
// instead of surfacing FAILED to the caller. Transport and other ambiguous
// errors stay retryable.
func (redeemSubmittingState) ProcessEvent(ctx context.Context, _ CreditEvent,
	b *opBehavior) (*CreditTransition, error) {

	acctKey, err := b.accountKey(ctx)
	if err != nil {
		return nil, err
	}

	res, err := b.cfg.Server.RedeemCredit(
		ctx, acctKey, b.rec.OpKey, uint64(b.rec.AmountSat),
		b.rec.DestinationPubkey,
	)
	if errors.Is(err, ErrRedeemRejected) {
		return b.fail(ctx, fmt.Sprintf("redeem rejected: %v", err))
	}
	if err != nil {
		return nil, fmt.Errorf("reserve redemption: %w", err)
	}
	if res.State.IsTerminalFailure() {
		return b.fail(
			ctx, fmt.Sprintf("redemption ended in %s", res.State),
		)
	}

	b.rec.ServerOpID = res.OperationID

	// Checkpoint the reservation before the awaiting state reads the server
	// ledger: a failed read is a postpone, and redelivery has to resume in
	// the awaiting state rather than re-issue the reservation.
	return emit(&awaitingOORState{}, &stageRecord{})
}

// ProcessEvent for awaitingOORState reconciles against both the server ledger
// and the chain: the redemption fails if the server releases or fails the
// reservation, and is done once the redeemed VTXO lands at the wallet-owned
// destination.
func (awaitingOORState) ProcessEvent(ctx context.Context, _ CreditEvent,
	b *opBehavior) (*CreditTransition, error) {

	snapshot, err := b.listCredits(ctx)
	if err != nil {
		return nil, b.postponeListCreditsFetch(ctx, err)
	}
	if state, found := serverOpState(
		snapshot, b.rec.ServerOpID,
	); found && state.IsTerminalFailure() {
		return b.fail(ctx, fmt.Sprintf("redemption ended in %s", state))
	}

	found, amountSat, err := b.cfg.Daemon.FindLiveVTXOByPkScript(
		ctx, b.redeemPkScript,
	)
	if err != nil {
		return nil, fmt.Errorf("find redeemed vtxo: %w", err)
	}
	if found && amountSat > 0 {
		// The redemption destination is freshly allocated per op, so
		// the live vTXO at it is this redemption's payout. Reconcile
		// its amount against the reserved amount: surface any
		// divergence and record what actually materialized, so the
		// completed op (and the wallet entry projected from it)
		// reflects the value that landed rather than the amount we
		// requested. The redemption is already home in the wallet, so
		// we never terminal-fail on a divergence; failing would mismark
		// a settled redemption.
		if amountSat != b.rec.AmountSat {
			b.
				logger(ctx).
				WarnS(
					ctx,
					"Redeemed vtxo amount differs "+
						"from reservation",
					nil,
					slog.String("op_id", b.cfg.OpID),
					slog.Int64(
						"reserved_sat", b.rec.AmountSat,
					),
					slog.Int64("landed_sat", amountSat),
				)
			b.rec.AmountSat = amountSat
		}

		b.logger(ctx).InfoS(ctx, "Credit redemption landed",
			slog.String("op_id", b.cfg.OpID),
			slog.Int64("amount_sat", amountSat),
		)

		return emit(&completedState{})
	}

	if b.awaitExhausted() {
		return b.fail(ctx, "redeemed vtxo not seen within poll cap")
	}

	return emit(&awaitingOORState{}, &parkOp{})
}

// ProcessEvent for completedState is a terminal self-loop: a terminal operation
// performs no further work and emits no events.
func (s completedState) ProcessEvent(_ context.Context, _ CreditEvent,
	_ *opBehavior) (*CreditTransition, error) {

	return emit(&s)
}

// ProcessEvent for failedState is a terminal self-loop.
func (s failedState) ProcessEvent(_ context.Context, _ CreditEvent,
	_ *opBehavior) (*CreditTransition, error) {

	return emit(&s)
}

// redeemWatermarkCleared reports whether the wallet-owned auto-redeem watermark
// is cleared for the supplied account snapshot, and the earmark-adjusted
// available balance to redeem. It returns false when auto-redeem is disabled,
// when the available balance is below the threshold, or when the earmark
// provider or live-floor lookup errors (fail-safe: never redeem credits an
// in-flight wallet operation may be about to spend, and never materialize an
// invalid VTXO). The registry still applies the no-pending-pay/redeem interlock
// before admitting the redeem.
func (b *opBehavior) redeemWatermarkCleared(ctx context.Context,
	snapshot *CreditSnapshot) (uint64, bool) {

	if !b.cfg.AutoRedeemEnabled || snapshot == nil {
		return 0, false
	}

	available := snapshot.AvailableSat
	if b.cfg.Earmark != nil {
		if earmarkFn := b.cfg.Earmark.Load(); earmarkFn != nil {
			earmarked, err := (*earmarkFn)(ctx)
			if err != nil {
				b.
					logger(ctx).
					DebugS(
						ctx,
						"Skipping auto-redeem: "+
							"earmark unavailable",
						slog.String("op_id", b.cfg.OpID),
						slog.String("err", err.Error()),
					)

				return 0, false
			}
			if earmarked >= available {
				available = 0
			} else {
				available -= earmarked
			}
		}
	}

	// A configured watermark can rule the decision out without consulting
	// the operator. Equality continues because the live floor may allow an
	// exact-threshold payout.
	if available < b.cfg.MinRedeemSat {
		return 0, false
	}

	floor, err := b.cfg.Daemon.VTXOFloor(ctx)
	if err != nil || floor == 0 {
		if err == nil {
			err = fmt.Errorf("operator returned a zero VTXO floor")
		}

		b.logger(ctx).DebugS(ctx, "Skipping auto-redeem: VTXO "+
			"floor unavailable",
			slog.String("op_id", b.cfg.OpID),
			slog.String("err", err.Error()),
		)

		return 0, false
	}
	threshold := max(b.cfg.MinRedeemSat, floor)

	if available < threshold {
		return 0, false
	}

	return available, true
}

// listCreditsPostponeHorizon bounds how long one delivery keeps retrying a
// failed server-ledger read before it is failed for real. A postpone never
// consumes a delivery attempt, so without a bound a read that fails for good
// would re-enqueue the resume forever; with it the delivery reaches the
// ordinary failure path and the dead-letter table, which the delivery metric
// surfaces.
const listCreditsPostponeHorizon = 10 * time.Minute

// postponeListCreditsFetch turns a failed server-ledger read in an awaiting
// state into a postpone request, leaving every other failure untouched.
//
// The read changes nothing durable: every transition into an awaiting state
// checkpoints the advance (stageRecord) before that state's first ListCredits,
// so the side effect that preceded the read is already recorded and a
// redelivery resumes in the awaiting state rather than re-running it. The right
// response to a failed read is therefore "not now", not "this operation
// failed". Nacking instead spends one of the delivery's few attempts, and that
// budget is shorter than a swapd deploy or a degraded endpoint easily lasts, so
// the resume that keeps the operation alive is deleted from the mailbox while
// the server goes on to settle the operation from its own sweep.
//
// Only the reconciliation read at the top of an awaiting state is classified
// here. A Stage or Commit failure after the FSM advanced in memory keeps its
// commitFailed reload semantics: the redelivered turn has to re-read durable
// truth, so it must stay an ordinary error.
//
// Not every read failure is worth waiting on. Transport and server-side
// failures are: a swapd deploy, a degraded endpoint, or a slow RPC timeout is
// exactly the window this path exists to ride out, so they postpone. A
// deterministic client-side failure — a canceled context, or a validation-style
// status such as InvalidArgument, NotFound, or Unimplemented — is not repaired
// by waiting, so it keeps the ordinary failure path and reaches the
// dead-letter table as a visible error rather than a forever-parked resume. The
// classification is transientLedgerReadError, a deny-list: an error it does not
// recognize still postpones, so an unfamiliar transient failure never silently
// spends the delivery's attempt budget.
//
// The horizon is measured from the durable row's own enqueue time, not from
// behavior state. A delivery with no timestamp (a directly-invoked behavior in
// a test, or a store that does not report one) postpones without a horizon,
// since absence means "no horizon information", not "infinitely old".
func (b *opBehavior) postponeListCreditsFetch(ctx context.Context,
	err error) error {

	if !transientLedgerReadError(err) {
		// Waiting cannot repair a deterministic failure, so let the
		// ordinary retry policy run out and dead-letter the delivery.
		// No postpone metric is emitted: this is not a postpone
		// decision, and the dead-letter counter is where it surfaces.
		b.logger(ctx).WarnS(ctx, "Credit reconciliation read failed "+
			"with a deterministic error; not postponing", err,
			slog.String("op_id", b.cfg.OpID),
		)

		return err
	}

	horizon := listCreditsPostponeHorizon
	enqueuedAt, ok := actor.DeliveryEnqueuedAt(ctx)
	if ok && time.Since(enqueuedAt) >= horizon {
		b.logger(ctx).WarnS(ctx, "Credit reconciliation exceeded its "+
			"postpone horizon; failing the delivery", err,
			slog.String("op_id", b.cfg.OpID),
			slog.Time("enqueued_at", enqueuedAt),
			slog.Duration("horizon", horizon),
		)
		metrics.EmitDeliveryPostpone(
			ctx, b.cfg.MetricsSink, b.logger(ctx),
			metrics.PostponeCauseServerError, true,
		)

		return err
	}

	metrics.EmitDeliveryPostpone(
		ctx, b.cfg.MetricsSink, b.logger(ctx),
		metrics.PostponeCauseServerError, false,
	)

	return fmt.Errorf("%w: %w", err, actor.Postpone(b.reconcileBackoff()))
}

// transientLedgerReadError reports whether a failed server-ledger read is worth
// postponing. The classification is deliberately a deny-list: anything not
// recognized below is treated as transient, because the cost of guessing wrong
// is asymmetric. A transient failure that is not postponed spends the
// delivery's attempt budget and dead-letters the only driver the operation has;
// a deterministic failure that is postponed merely waits until its horizon.
//
// Two kinds of failure are denied. A canceled context is the client's own
// decision to stop the turn (shutdown, or a lease that is going away), not
// evidence about the operator's ledger, so re-parking the delivery would only
// make the actor retry work it has already abandoned. A validation-style gRPC
// status is a deterministic answer about the request or the surface itself and
// will not change on the next poll.
//
// DeadlineExceeded is not denied: it is what a slow endpoint's own RPC timeout
// looks like, and nacking there would dead-letter an operation whose server is
// merely slow.
func transientLedgerReadError(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}

	switch status.Code(err) {
	case codes.Canceled, codes.InvalidArgument, codes.NotFound,
		codes.AlreadyExists, codes.PermissionDenied,
		codes.Unauthenticated, codes.FailedPrecondition,
		codes.OutOfRange, codes.Unimplemented:
		return false

	default:
		return true
	}
}

// reconcileBackoff returns how long a postponed reconciliation waits before it
// is redelivered. Reusing the poll cadence keeps a postponed delivery from
// re-driving the operation faster than the poll timer that would otherwise have
// done it.
func (b *opBehavior) reconcileBackoff() time.Duration {
	if b.cfg.PollInterval > 0 {
		return b.cfg.PollInterval
	}

	return DefaultPollInterval
}

// listCredits fetches the server-authoritative account snapshot for this
// operation's account.
func (b *opBehavior) listCredits(ctx context.Context) (*CreditSnapshot, error) {
	acctKey, err := b.accountKey(ctx)
	if err != nil {
		return nil, err
	}

	snapshot, err := b.cfg.Server.ListCredits(ctx, acctKey)
	if err != nil {
		return nil, fmt.Errorf("list credits: %w", err)
	}

	return snapshot, nil
}

// serverOpState returns the server state of one operation id from a snapshot.
func serverOpState(snapshot *CreditSnapshot,
	serverOpID string) (ServerCreditState, bool) {

	if snapshot == nil || serverOpID == "" {
		return "", false
	}

	for _, op := range snapshot.Operations {
		if op.OperationID == serverOpID {
			return op.State, true
		}
	}

	return "", false
}

// serverOpStateByPaymentHash returns the server state of the operation bound to
// a payment hash. A credit-only pay is an outbound payment, so the wallet holds
// no other operation for that payee invoice hash and the match is unambiguous.
func serverOpStateByPaymentHash(snapshot *CreditSnapshot,
	paymentHash []byte) (ServerCreditState, bool) {

	if snapshot == nil || len(paymentHash) == 0 {
		return "", false
	}

	for _, op := range snapshot.Operations {
		if bytes.Equal(op.PaymentHash, paymentHash) {
			return op.State, true
		}
	}

	return "", false
}
