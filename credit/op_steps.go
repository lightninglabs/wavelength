package credit

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"

	"github.com/lightninglabs/darepo-client/baselib/actor"
)

// stepQuoting decides whether the pay needs an Ark top-up. A zero top-up means
// the account already holds enough credits, so the FSM jumps straight to
// paying.
func (b *opBehavior) stepQuoting() (bool, error) {
	if b.rec.TopupSat == 0 {
		b.setState(StatePaying)

		return false, nil
	}

	b.setState(StateTopupCreating)

	return false, nil
}

// stepTopupCreating creates (or reuses, by op key) the server ARK_TOPUP funding
// operation and records the server-owned destination the OOR transfer must
// fund. The destination is staged durably before the FSM advances to funding,
// so a crash before the turn commits re-drives from topup_funding (re-issuing
// only the idempotent OOR send) rather than re-creating the credit.
func (b *opBehavior) stepTopupCreating(ctx context.Context,
	ax actor.Exec[creditTx]) (bool, error) {

	acctKey, err := b.accountKey(ctx)
	if err != nil {
		return false, err
	}

	res, err := b.cfg.Server.CreateCredit(
		ctx, acctKey, b.rec.OpKey, SourceArkTopUp,
		uint64(b.rec.TopupSat), "",
	)
	if err != nil {
		return false, fmt.Errorf("create top-up credit: %w", err)
	}
	if res.State.IsTerminalFailure() {
		b.failOp(
			ctx, fmt.Sprintf("top-up credit ended in %s",
				res.State),
		)

		return false, nil
	}
	if len(res.DestinationPubkey) == 0 {
		// A non-failure response with no destination is a deterministic
		// server-contract violation, not a transient condition: fail
		// terminally rather than redeliver forever.
		b.failOp(ctx, "top-up credit missing destination pubkey")

		return false, nil
	}

	b.rec.ServerOpID = res.OperationID
	b.rec.DestinationPubkey = append(
		[]byte(nil), res.DestinationPubkey...,
	)
	b.setState(StateTopupFunding)

	if err := b.checkpoint(ctx, ax); err != nil {
		return false, err
	}

	return false, nil
}

// stepTopupFunding submits the OOR transfer that funds the top-up. The op key
// is the OOR idempotency key, so a re-issued send never produces a second
// transfer.
func (b *opBehavior) stepTopupFunding(ctx context.Context) (bool, error) {
	sessionID, err := b.cfg.Daemon.SendOOR(
		ctx, b.rec.DestinationPubkey, uint64(b.rec.TopupSat),
		b.rec.OpKey,
	)
	if err != nil {
		return false, fmt.Errorf("fund top-up via OOR: %w", err)
	}

	b.rec.OORSessionID = sessionID
	b.setState(StateTopupAwaitingCredit)
	b.logger(ctx).InfoS(ctx, "Credit top-up funded, awaiting credit",
		slog.String("op_id", b.cfg.OpID),
		slog.String("oor_session_id", sessionID),
		slog.Int64("topup_sat", b.rec.TopupSat),
	)

	return false, nil
}

// stepAwaitingCredit reconciles against the server ledger: the top-up is done
// once the server marks the funding operation CREDITED. It gates only on the
// funding op reaching CREDITED, not on AvailableSat clearing the StartPay cap
// (which can be the sentinel max for a must-use-credit pay and would park the
// operation forever); StartPay reserves what it needs and surfaces any
// shortfall as a deterministic pay failure.
func (b *opBehavior) stepAwaitingCredit(ctx context.Context) (bool, error) {
	snapshot, err := b.listCredits(ctx)
	if err != nil {
		return false, err
	}

	state, found := serverOpState(snapshot, b.rec.ServerOpID)
	if found && state.IsTerminalFailure() {
		b.failOp(ctx, fmt.Sprintf("top-up funding ended in %s", state))

		return false, nil
	}
	if found && state == ServerStateCredited {
		b.setState(StatePaying)

		return false, nil
	}

	if b.awaitExhausted() {
		b.failOp(ctx, "top-up credit not finalized within poll cap")

		return false, nil
	}

	// Still awaiting: park on a poll timer.
	return true, nil
}

// stepPaying starts the credit or mixed pay. StartPay is idempotent by payment
// hash, so a re-issued call reuses the already-started pay session. A
// credit-only pay then awaits settlement against the server ledger; a mixed pay
// hands terminal authority to the swap monitor and completes on hand-off.
func (b *opBehavior) stepPaying(ctx context.Context) (bool, error) {
	err := b.cfg.Server.StartPay(
		ctx, b.rec.Invoice, uint64(b.rec.MaxFeeSat),
		uint64(b.rec.MaxCreditSat),
	)
	if err != nil {
		return false, fmt.Errorf("start pay: %w", err)
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
		b.setState(StatePayAwaitingSettlement)

		return false, nil
	}

	b.setState(StateCompleted)

	return false, nil
}

// stepPayAwaitingSettlement reconciles a credit-only pay against the server
// ledger by matching the invoice payment hash to the server pay operation: the
// pay is done once the server marks it DEBITED (success), and fails once the
// server marks it RELEASED/FAILED/EXPIRED. Until then it parks on a poll timer.
func (b *opBehavior) stepPayAwaitingSettlement(ctx context.Context) (bool,
	error) {

	snapshot, err := b.listCredits(ctx)
	if err != nil {
		return false, err
	}

	state, found := serverOpStateByPaymentHash(snapshot, b.rec.PaymentHash)
	if found && state == ServerStateDebited {
		b.logger(ctx).InfoS(ctx, "Credit pay settled",
			slog.String("op_id", b.cfg.OpID),
		)
		b.setState(StateCompleted)

		return false, nil
	}
	if found && state.IsTerminalFailure() {
		b.failOp(ctx, fmt.Sprintf("credit pay ended in %s", state))

		return false, nil
	}

	if b.awaitExhausted() {
		b.failOp(ctx, "credit pay not settled within poll cap")

		return false, nil
	}

	return true, nil
}

// stepReceiveCreating creates the server-owned Lightning receive invoice that
// credits the account on settlement.
func (b *opBehavior) stepReceiveCreating(ctx context.Context) (bool, error) {
	acctKey, err := b.accountKey(ctx)
	if err != nil {
		return false, err
	}

	res, err := b.cfg.Server.CreateCredit(
		ctx, acctKey, b.rec.OpKey, SourceLightningReceive,
		uint64(b.rec.AmountSat), b.receiveMemo,
	)
	if err != nil {
		return false, fmt.Errorf("create receive credit: %w", err)
	}
	if res.State.IsTerminalFailure() {
		b.failOp(
			ctx, fmt.Sprintf("receive credit ended in %s",
				res.State),
		)

		return false, nil
	}
	if res.Invoice == "" {
		// A non-failure response with no invoice is a deterministic
		// server-contract violation: fail terminally rather than
		// redeliver forever.
		b.failOp(ctx, "receive credit missing invoice")

		return false, nil
	}

	b.rec.ServerOpID = res.OperationID
	b.rec.Invoice = res.Invoice
	if len(res.PaymentHash) > 0 {
		b.rec.PaymentHash = append([]byte(nil), res.PaymentHash...)
	}
	b.setState(StateAwaitingSettlement)
	b.logger(ctx).InfoS(ctx, "Credit receive invoice created",
		slog.String("op_id", b.cfg.OpID),
	)

	return false, nil
}

// stepAwaitingSettlement reconciles against the server ledger: the receive is
// done once the server marks the operation CREDITED. The wait is bounded by the
// server-reported terminal states (and the optional poll cap), so an unpaid
// invoice that the server expires fails the operation deterministically.
func (b *opBehavior) stepAwaitingSettlement(ctx context.Context) (bool, error) {
	snapshot, err := b.listCredits(ctx)
	if err != nil {
		return false, err
	}

	state, found := serverOpState(snapshot, b.rec.ServerOpID)
	if found && state.IsTerminalFailure() {
		b.failOp(ctx, fmt.Sprintf("receive funding ended in %s", state))

		return false, nil
	}
	if found && state == ServerStateCredited {
		b.setState(StateCompleted)

		return false, nil
	}

	if b.awaitExhausted() {
		b.failOp(ctx, "receive credit not settled within poll cap")

		return false, nil
	}

	return true, nil
}

// stepRedeemReserving allocates a wallet-owned destination and reserves the
// redemption with the server. The destination is staged durably before the
// RedeemCredit reservation, so a crash between the reservation and the commit
// re-drives against the same destination the server reservation is bound to,
// rather than allocating a fresh script the chain-watch would never match. The
// op key is the idempotency key, so a re-issued reservation reuses the same
// server operation.
func (b *opBehavior) stepRedeemReserving(ctx context.Context,
	ax actor.Exec[creditTx]) (bool, error) {

	acctKey, err := b.accountKey(ctx)
	if err != nil {
		return false, err
	}

	if len(b.rec.DestinationPubkey) == 0 || len(b.redeemPkScript) == 0 {
		pubKey, pkScript, err := b.cfg.Daemon.AllocateReceiveScript(
			ctx, "credit-redeem",
		)
		if err != nil {
			return false, fmt.Errorf("allocate redeem "+
				"destination: %w", err)
		}
		if len(pubKey) == 0 || len(pkScript) == 0 {
			// An empty allocation cannot be redeemed against; the
			// auto-redeem sweep re-tries with a fresh op key later,
			// so fail terminally rather than wedge this operation.
			b.failOp(ctx, "redeem destination is empty")

			return false, nil
		}

		b.rec.DestinationPubkey = append([]byte(nil), pubKey...)
		b.redeemPkScript = append([]byte(nil), pkScript...)

		if err := b.checkpoint(ctx, ax); err != nil {
			return false, err
		}
	}

	res, err := b.cfg.Server.RedeemCredit(
		ctx, acctKey, b.rec.OpKey, uint64(b.rec.AmountSat),
		b.rec.DestinationPubkey,
	)
	if err != nil {
		return false, fmt.Errorf("reserve redemption: %w", err)
	}
	if res.State.IsTerminalFailure() {
		b.failOp(ctx, fmt.Sprintf("redemption ended in %s", res.State))

		return false, nil
	}

	b.rec.ServerOpID = res.OperationID
	b.setState(StateAwaitingOOR)

	return false, nil
}

// stepAwaitingOOR reconciles against both the server ledger and the chain: the
// redemption fails if the server releases or fails the reservation, and is done
// once the redeemed VTXO lands at the wallet-owned destination.
func (b *opBehavior) stepAwaitingOOR(ctx context.Context) (bool, error) {
	snapshot, err := b.listCredits(ctx)
	if err != nil {
		return false, err
	}
	if state, found := serverOpState(
		snapshot, b.rec.ServerOpID,
	); found && state.IsTerminalFailure() {

		b.failOp(ctx, fmt.Sprintf("redemption ended in %s", state))

		return false, nil
	}

	found, amountSat, err := b.cfg.Daemon.FindLiveVTXOByPkScript(
		ctx, b.redeemPkScript,
	)
	if err != nil {
		return false, fmt.Errorf("find redeemed vtxo: %w", err)
	}
	if found {
		b.logger(ctx).InfoS(ctx, "Credit redemption landed",
			slog.String("op_id", b.cfg.OpID),
			slog.Int64("amount_sat", amountSat),
		)
		b.setState(StateCompleted)

		return false, nil
	}

	if b.awaitExhausted() {
		b.failOp(ctx, "redeemed vtxo not seen within poll cap")

		return false, nil
	}

	return true, nil
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
