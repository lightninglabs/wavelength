package credit

import (
	"github.com/lightninglabs/wavelength/db"
)

// OpKind identifies the credit operation family driven by one per-operation
// actor. It maps 1:1 onto db.CreditOpKind.
type OpKind = db.CreditOpKind

const (
	// KindPay is a sub-dust / shortfall pay (optional Ark top-up, then a
	// credit or mixed pay).
	KindPay = db.CreditOpKindPay

	// KindReceive is a server-owned Lightning receive that credits the
	// account.
	KindReceive = db.CreditOpKindReceive

	// KindRedeem materializes available credits back into an Ark vTXO.
	KindRedeem = db.CreditOpKindRedeem
)

// State is the per-operation FSM state. The string form is persisted in the
// credit_operations.state column and rendered back to status callers, so the
// values must stay stable across versions.
type State string

const (
	// StateQuoting means the pay operation knows its shortfall/top-up plan
	// but has not yet created a server top-up.
	StateQuoting State = "quoting"

	// StateTopupCreating means CreateCredit(ARK_TOPUP) has been requested
	// and the server destination is being recorded.
	StateTopupCreating State = "topup_creating"

	// StateTopupFunding means the delegated OOR transfer that funds the
	// top-up has been (or is being) admitted to the OOR registry.
	StateTopupFunding State = "topup_funding"

	// StateTopupAwaitingCredit means the OOR transfer is in flight and the
	// operation is waiting for the server ledger to report the top-up as
	// CREDITED.
	StateTopupAwaitingCredit State = "topup_awaiting_credit"

	// StatePaying means enough credits are durably available and StartPay
	// has been (or is being) triggered.
	StatePaying State = "paying"

	// StatePayAwaitingSettlement means StartPay was accepted and the
	// operation is waiting for the server ledger to finalize the credit pay
	// as debited (success) or released/failed (failure). It is only entered
	// for credit-only pays whose server response carried a reconcilable pay
	// operation id; mixed pays hand terminal authority to the swap monitor.
	StatePayAwaitingSettlement State = "pay_awaiting_settlement"

	// StateReceiveCreating means CreateCredit(LIGHTNING_RECEIVE) has been
	// requested and the server invoice is being recorded.
	StateReceiveCreating State = "receive_creating"

	// StateAwaitingSettlement means the receive invoice is outstanding and
	// the operation is waiting for the server ledger to report it CREDITED.
	StateAwaitingSettlement State = "awaiting_settlement"

	// StateRedeemReserving means a wallet-owned redemption destination is
	// being allocated and durably checkpointed before the server
	// reservation is requested against it.
	StateRedeemReserving State = "redeem_reserving"

	// StateRedeemSubmitting means the redemption destination is durably
	// recorded and RedeemCredit is being requested (or reused, by op key)
	// against it. Splitting it out from reserving keeps the destination
	// checkpoint strictly before the reservation effect, so a crash
	// re-drives against the destination the chain-watch will match.
	StateRedeemSubmitting State = "redeem_submitting"

	// StateAwaitingOOR means the redemption OOR is in flight and the
	// operation is waiting for the redeemed vTXO to land locally.
	StateAwaitingOOR State = "awaiting_oor"

	// StateCompleted is the terminal success state.
	StateCompleted State = "completed"

	// StateFailed is the terminal failure state.
	StateFailed State = "failed"
)

// IsTerminal reports whether no further work should run for this state.
func (s State) IsTerminal() bool {
	return s == StateCompleted || s == StateFailed
}

// Status maps an FSM state onto the coordinator-facing db.CreditOpStatus used
// by the partial-unique dedup index and the boot-time non-terminal restore.
func (s State) Status() db.CreditOpStatus {
	switch s {
	case StateCompleted:
		return db.CreditOpStatusCompleted

	case StateFailed:
		return db.CreditOpStatusFailed

	default:
		return db.CreditOpStatusPending
	}
}

// initialState returns the FSM entry state for one operation kind.
func initialState(kind OpKind) State {
	switch kind {
	case KindPay:
		return StateQuoting

	case KindReceive:
		return StateReceiveCreating

	case KindRedeem:
		return StateRedeemReserving

	default:
		return StateFailed
	}
}
