package credit

import (
	"github.com/lightninglabs/wavelength/baselib/protofsm"
)

// CreditState is the per-operation protofsm state interface. Every concrete
// credit FSM state implements it. The InternalEvent type is CreditEvent, the
// OutboxEvent type is CreditOutMsg, and the environment is *opBehavior — the
// durable per-operation behavior, which carries the operation record, the
// server/daemon surfaces, and the resume-snapshot fields a transition reads and
// writes. Aliasing the protofsm generics here keeps the concrete state,
// transition, and emitted types short throughout the package, mirroring round's
// interfaces.go.
type CreditState = protofsm.State[CreditEvent, CreditOutMsg, *opBehavior]

// CreditTransition is the state transition returned by a credit ProcessEvent:
// the next state plus an optional set of emitted events.
type CreditTransition = protofsm.StateTransition[
	CreditEvent, CreditOutMsg, *opBehavior,
]

// CreditEmittedEvent bundles the internal events that re-drive this FSM and the
// outbox events that bubble up to the driving actor for dispatch.
type CreditEmittedEvent = protofsm.EmittedEvent[CreditEvent, CreditOutMsg]

// The concrete FSM states. Each is a zero-sized marker: the mutable operation
// data lives on *opBehavior (b.rec plus the resume-snapshot fields), so a
// transition reads and writes the record through the environment rather than
// carrying it in the state value. The String form is the value persisted in the
// credit_operations.state column, so it must match the State constants in
// state.go exactly and stay stable across versions.
type (
	// quotingState decides whether a pay needs an Ark top-up.
	quotingState struct{}

	// topupCreatingState creates the server ARK_TOPUP funding operation.
	topupCreatingState struct{}

	// topupFundingState submits the OOR transfer that funds the top-up.
	topupFundingState struct{}

	// topupAwaitingCreditState waits for the server to mark the top-up
	// CREDITED.
	topupAwaitingCreditState struct{}

	// payingState starts the credit or mixed pay.
	payingState struct{}

	// payAwaitingSettlementState reconciles a credit-only pay to
	// settlement.
	payAwaitingSettlementState struct{}

	// receiveCreatingState creates the server receive invoice.
	receiveCreatingState struct{}

	// awaitingSettlementState waits for a receive to be CREDITED, then
	// evaluates the auto-redeem watermark.
	awaitingSettlementState struct{}

	// redeemReservingState allocates a wallet-owned redemption destination
	// and checkpoints it before the reservation.
	redeemReservingState struct{}

	// redeemSubmittingState requests the server redemption against the
	// checkpointed destination.
	redeemSubmittingState struct{}

	// awaitingOORState waits for the redeemed vTXO to land locally.
	awaitingOORState struct{}

	// completedState is the terminal success state.
	completedState struct{}

	// failedState is the terminal failure state.
	failedState struct{}
)

// Compile-time checks that every state satisfies the protofsm State interface.
var (
	_ CreditState = (*quotingState)(nil)
	_ CreditState = (*topupCreatingState)(nil)
	_ CreditState = (*topupFundingState)(nil)
	_ CreditState = (*topupAwaitingCreditState)(nil)
	_ CreditState = (*payingState)(nil)
	_ CreditState = (*payAwaitingSettlementState)(nil)
	_ CreditState = (*receiveCreatingState)(nil)
	_ CreditState = (*awaitingSettlementState)(nil)
	_ CreditState = (*redeemReservingState)(nil)
	_ CreditState = (*redeemSubmittingState)(nil)
	_ CreditState = (*awaitingOORState)(nil)
	_ CreditState = (*completedState)(nil)
	_ CreditState = (*failedState)(nil)
)

// String returns the persisted state string for each state.
func (quotingState) String() string { return string(StateQuoting) }
func (topupCreatingState) String() string {
	return string(StateTopupCreating)
}
func (topupFundingState) String() string { return string(StateTopupFunding) }
func (topupAwaitingCreditState) String() string {
	return string(StateTopupAwaitingCredit)
}
func (payingState) String() string { return string(StatePaying) }
func (payAwaitingSettlementState) String() string {
	return string(StatePayAwaitingSettlement)
}
func (receiveCreatingState) String() string {
	return string(StateReceiveCreating)
}
func (awaitingSettlementState) String() string {
	return string(StateAwaitingSettlement)
}
func (redeemReservingState) String() string {
	return string(StateRedeemReserving)
}
func (redeemSubmittingState) String() string {
	return string(StateRedeemSubmitting)
}
func (awaitingOORState) String() string { return string(StateAwaitingOOR) }
func (completedState) String() string   { return string(StateCompleted) }
func (failedState) String() string      { return string(StateFailed) }

// IsTerminal reports whether the state halts further FSM work. Only the two
// terminal states return true; every other state either advances or parks on a
// poll until a server or chain signal moves it forward.
func (quotingState) IsTerminal() bool               { return false }
func (topupCreatingState) IsTerminal() bool         { return false }
func (topupFundingState) IsTerminal() bool          { return false }
func (topupAwaitingCreditState) IsTerminal() bool   { return false }
func (payingState) IsTerminal() bool                { return false }
func (payAwaitingSettlementState) IsTerminal() bool { return false }
func (receiveCreatingState) IsTerminal() bool       { return false }
func (awaitingSettlementState) IsTerminal() bool    { return false }
func (redeemReservingState) IsTerminal() bool       { return false }
func (redeemSubmittingState) IsTerminal() bool      { return false }
func (awaitingOORState) IsTerminal() bool           { return false }
func (completedState) IsTerminal() bool             { return true }
func (failedState) IsTerminal() bool                { return true }

// decodeCreditState reconstructs the typed protofsm state from a persisted
// state string, so a resumed operation re-enters the FSM exactly where its
// durable row left off. The bool reports whether the string was recognized: a
// known state returns (state, true), while an unrecognized string returns
// (failed, false). The caller distinguishes a genuinely failed row, which is a
// terminal no-op, from a corrupt row, which must be driven to a persisted
// failure so it terminal-commits rather than restoring on every boot.
func decodeCreditState(s State) (CreditState, bool) {
	switch s {
	case StateQuoting:
		return &quotingState{}, true

	case StateTopupCreating:
		return &topupCreatingState{}, true

	case StateTopupFunding:
		return &topupFundingState{}, true

	case StateTopupAwaitingCredit:
		return &topupAwaitingCreditState{}, true

	case StatePaying:
		return &payingState{}, true

	case StatePayAwaitingSettlement:
		return &payAwaitingSettlementState{}, true

	case StateReceiveCreating:
		return &receiveCreatingState{}, true

	case StateAwaitingSettlement:
		return &awaitingSettlementState{}, true

	case StateRedeemReserving:
		return &redeemReservingState{}, true

	case StateRedeemSubmitting:
		return &redeemSubmittingState{}, true

	case StateAwaitingOOR:
		return &awaitingOORState{}, true

	case StateCompleted:
		return &completedState{}, true

	case StateFailed:
		return &failedState{}, true

	default:
		return &failedState{}, false
	}
}
