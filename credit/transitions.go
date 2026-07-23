package credit

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"

	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// emit builds a state transition to next carrying the given outbox directives.
// The credit FSM never chains via protofsm internal events: the durable
// per-operation actor re-drives one step at a time so it can flush a
// stageRecord checkpoint between a state that records a server identifier and
// the next state that depends on it. A transition with no outbox is a plain
// advance the actor continues from; a parkOp stops the turn; a terminal
// NextState ends the FSM.
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

// ProcessEvent for topupFundingState submits the OOR transfer that funds the
// top-up. The op key is the OOR idempotency key, so a re-issued send never
// produces a second transfer.
func (topupFundingState) ProcessEvent(ctx context.Context, _ CreditEvent,
	b *opBehavior) (*CreditTransition, error) {

	sessionID, err := b.cfg.Daemon.SendOOR(
		ctx, b.rec.DestinationPubkey, uint64(b.rec.TopupSat),
		b.rec.OpKey,
	)
	if err != nil {
		return nil, fmt.Errorf("fund top-up via OOR: %w", err)
	}

	b.rec.OORSessionID = sessionID
	b.logger(ctx).InfoS(ctx, "Credit top-up funded, awaiting credit",
		slog.String("op_id", b.cfg.OpID),
		slog.String("oor_session_id", sessionID),
		slog.Int64("topup_sat", b.rec.TopupSat),
	)

	return emit(&topupAwaitingCreditState{})
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
		return nil, err
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
// pay session. A credit-only pay then awaits settlement against the server
// ledger; a mixed pay hands terminal authority to the swap monitor and
// completes on hand-off.
func (payingState) ProcessEvent(ctx context.Context, _ CreditEvent,
	b *opBehavior) (*CreditTransition, error) {

	err := b.cfg.Server.StartPay(
		ctx, b.rec.Invoice, uint64(b.rec.MaxFeeSat),
		uint64(b.rec.RoutingFeeBudgetSat), uint64(b.rec.MaxCreditSat),
	)
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
		return emit(&payAwaitingSettlementState{})
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
		return nil, err
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

	return emit(&awaitingSettlementState{})
}

// ProcessEvent for awaitingSettlementState reconciles a receive against the
// server ledger: the receive is done once the server marks the operation
// CREDITED. On settlement it evaluates the wallet-owned auto-redeem watermark
// and, if the earmark-adjusted available balance now clears the threshold,
// emits a triggerRedeem so the registry can materialize the credits — folding
// the auto-redeem decision into the receive state machine instead of a periodic
// background sweep. The wait is bounded by the server-reported terminal states
// and the optional poll cap, so an unpaid invoice the server expires fails the
// operation deterministically.
func (awaitingSettlementState) ProcessEvent(ctx context.Context, _ CreditEvent,
	b *opBehavior) (*CreditTransition, error) {

	snapshot, err := b.listCredits(ctx)
	if err != nil {
		return nil, err
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

	if b.awaitExhausted() {
		return b.fail(ctx, "receive credit not settled within poll cap")
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
// key, so a re-issued reservation reuses the same server operation.
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
	if err != nil {
		return nil, fmt.Errorf("reserve redemption: %w", err)
	}
	if res.State.IsTerminalFailure() {
		return b.fail(
			ctx, fmt.Sprintf("redemption ended in %s", res.State),
		)
	}

	b.rec.ServerOpID = res.OperationID

	return emit(&awaitingOORState{})
}

// ProcessEvent for awaitingOORState reconciles against both the server ledger
// and the chain: the redemption fails if the server releases or fails the
// reservation, and is done once the redeemed VTXO lands at the wallet-owned
// destination.
func (awaitingOORState) ProcessEvent(ctx context.Context, _ CreditEvent,
	b *opBehavior) (*CreditTransition, error) {

	snapshot, err := b.listCredits(ctx)
	if err != nil {
		return nil, err
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
// when the available balance does not strictly exceed the threshold, or when
// the earmark provider errors (fail-safe: never redeem credits an in-flight
// wallet operation may be about to spend). The registry still applies the
// no-pending-pay/redeem interlock before admitting the redeem.
func (b *opBehavior) redeemWatermarkCleared(ctx context.Context,
	snapshot *CreditSnapshot) (uint64, bool) {

	if !b.cfg.AutoRedeemEnabled || snapshot == nil {
		return 0, false
	}

	threshold := b.cfg.MinRedeemSat
	if threshold == 0 {
		dust, err := b.cfg.Daemon.DustLimit(ctx)
		if err != nil {
			b.logger(ctx).DebugS(ctx, "Skipping auto-redeem: dust "+
				"limit unavailable",
				slog.String("op_id", b.cfg.OpID),
				slog.String("err", err.Error()),
			)

			return 0, false
		}
		threshold = dust
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

	if available <= threshold {
		return 0, false
	}

	return available, true
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
