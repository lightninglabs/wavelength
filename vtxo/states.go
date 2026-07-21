package vtxo

import (
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
)

// VTXOState is a sealed interface for all states in the VTXO state machine.
// Each state implements ProcessEvent to handle incoming events and return
// state transitions.
type VTXOState interface {
	protofsm.State[VTXOEvent, VTXOOutMsg, *VTXOEnvironment]

	// vtxoStateSealed is an unexported method that prevents external
	// packages from implementing VTXOState.
	vtxoStateSealed()
}

// LiveState is the primary active state. The VTXO is live and can be spent
// collaboratively with the operator. This state monitors block epochs to
// detect when the VTXO is approaching expiry.
type LiveState struct {
	// VTXO is the descriptor for this VTXO.
	VTXO *Descriptor

	// LastCheckedHeight is the last block height at which expiry was
	// checked.
	LastCheckedHeight int32
}

// String returns a human-readable state name.
func (s *LiveState) String() string {
	return "Live"
}

// IsTerminal returns false since LiveState is not a terminal state.
func (s *LiveState) IsTerminal() bool {
	return false
}

func (s *LiveState) vtxoStateSealed() {}

// PendingForfeitState indicates the VTXO has been committed to cooperative
// consumption (forfeit) and is awaiting concrete forfeit details from the
// round actor. This state is reached when the VTXO needs to be forfeited —
// whether due to approaching expiry, a leave request, or an in-round send —
// but the round actor has not yet supplied the connector outpoint and
// forfeit parameters needed to build the forfeit transaction.
type PendingForfeitState struct {
	// VTXO is the descriptor for this VTXO.
	VTXO *Descriptor

	// RequestedAtHeight is the block height when the forfeit was
	// requested. Zero if triggered manually rather than by expiry.
	RequestedAtHeight int32
}

// String returns a human-readable state name.
func (s *PendingForfeitState) String() string {
	return "PendingForfeit"
}

// IsTerminal returns false since PendingForfeitState is not terminal.
func (s *PendingForfeitState) IsTerminal() bool {
	return false
}

// vtxoStateSealed marks this as implementing the sealed VTXOState interface.
func (s *PendingForfeitState) vtxoStateSealed() {}

// SpendingState indicates the VTXO has been claimed for an out-of-round (OOR)
// spend operation. The VTXO is unavailable for cooperative forfeit or any
// other operation until the spend completes or is released. This state is
// persisted as VTXOStatusSpending so it survives restarts.
type SpendingState struct {
	// VTXO is the descriptor for this VTXO.
	VTXO *Descriptor

	// LastCheckedHeight tracks expiry monitoring. Even while spending,
	// the VTXO must still escalate to unilateral exit if critical expiry
	// is reached.
	LastCheckedHeight int32
}

// String returns a human-readable state name.
func (s *SpendingState) String() string {
	return "Spending"
}

// IsTerminal returns false since SpendingState is not a terminal state.
func (s *SpendingState) IsTerminal() bool {
	return false
}

// vtxoStateSealed marks this as implementing the sealed VTXOState interface.
func (s *SpendingState) vtxoStateSealed() {}

// SpentState is a terminal state indicating the VTXO was consumed by an
// out-of-round (OOR) transaction. The VTXO actor should be cleaned up.
type SpentState struct {
	// VTXO is the descriptor for this VTXO.
	VTXO *Descriptor
}

// String returns a human-readable state name.
func (s *SpentState) String() string {
	return "Spent"
}

// IsTerminal returns true since SpentState is a terminal state.
func (s *SpentState) IsTerminal() bool {
	return true
}

// vtxoStateSealed marks this as implementing the sealed VTXOState interface.
func (s *SpentState) vtxoStateSealed() {}

// ForfeitingState indicates the VTXO is being forfeited in a round. The VTXO
// actor is waiting for the new commitment transaction to confirm.
type ForfeitingState struct {
	// VTXO is the descriptor for this VTXO.
	VTXO *Descriptor

	// NewRoundID is the round where the refreshed VTXO will be created.
	NewRoundID string

	// ConnectorOutpoint is the connector from the new commitment tx that
	// the forfeit tx spends.
	ConnectorOutpoint wire.OutPoint

	// ForfeitTxID is the txid of the forfeit transaction (set after
	// signing).
	ForfeitTxID chainhash.Hash

	// ForfeitTx is the signed forfeit transaction. Persisted for crash
	// recovery so we can re-broadcast if the round doesn't confirm.
	ForfeitTx *wire.MsgTx
}

// String returns a human-readable state name.
func (s *ForfeitingState) String() string {
	return "Forfeiting"
}

// IsTerminal returns false since ForfeitingState is not terminal.
func (s *ForfeitingState) IsTerminal() bool {
	return false
}

func (s *ForfeitingState) vtxoStateSealed() {}

// ForfeitedState is a terminal state indicating the VTXO has been forfeited
// cooperatively. The round actor determines the disposition of the forfeited
// value (new VTXO in a fresh round, or an on-chain withdrawal output).
type ForfeitedState struct {
	// VTXO is the descriptor for this VTXO.
	VTXO *Descriptor

	// NewRoundID is the round where the forfeit was processed.
	NewRoundID string

	// CommitmentTxID is the new commitment transaction that was confirmed.
	CommitmentTxID chainhash.Hash
}

// String returns a human-readable state name.
func (s *ForfeitedState) String() string {
	return "Forfeited"
}

// IsTerminal returns true since ForfeitedState is a terminal state.
func (s *ForfeitedState) IsTerminal() bool {
	return true
}

func (s *ForfeitedState) vtxoStateSealed() {}

// UnilateralExitState indicates the VTXO has been handed to the chain
// resolver for unilateral on-chain exit handling. The chain resolver
// drives the on-chain unroll from this point.
//
// This state is intentionally NON-terminal. The exit is gated on the
// user's intent to unroll, not on a terminal on-chain event: the
// downstream unroll job may still fail without ever broadcasting (e.g. a
// sub-dust proof tx that cannot meet min relay fee). Keeping the actor
// alive here lets the manager observe the VTXO and either recover it back
// to LiveState on a clean failure (ExitFailedEvent) or retire it to the
// terminal SpentState once the exit confirms on-chain (ExitConfirmedEvent).
// Reaping the actor on intent — as the original terminal design did —
// silently dropped the VTXO from the wallet's live set on a failed exit
// while the operator still considered it live (wavelength#602).
type UnilateralExitState struct {
	// VTXO is the descriptor for this VTXO.
	VTXO *Descriptor

	// Reason explains why the VTXO is being unilaterally exited.
	Reason string

	// LastCheckedHeight carries the block height observed when the VTXO
	// entered exit handling. It seeds LiveState.LastCheckedHeight if the
	// exit is later rolled back, so expiry monitoring resumes from where
	// it left off rather than re-evaluating from zero.
	LastCheckedHeight int32
}

// String returns a human-readable state name.
func (s *UnilateralExitState) String() string {
	return "UnilateralExit"
}

// IsTerminal returns false: the exit is observed rather than fire-and-forget,
// so the actor survives until the exit either confirms on-chain (SpentState)
// or is rolled back to LiveState. See the type doc for the rationale.
func (s *UnilateralExitState) IsTerminal() bool {
	return false
}

func (s *UnilateralExitState) vtxoStateSealed() {}

// FailedState is a terminal state indicating an unrecoverable error occurred.
type FailedState struct {
	// VTXO is the descriptor for this VTXO.
	VTXO *Descriptor

	// Reason is a human-readable description of the failure.
	Reason string

	// Error is the underlying error, if any.
	Error error

	// Recoverable indicates whether the failure might be recoverable.
	Recoverable bool
}

// String returns a human-readable state name.
func (s *FailedState) String() string {
	return fmt.Sprintf("Failed: %s", s.Reason)
}

// IsTerminal returns true since FailedState is a terminal state.
func (s *FailedState) IsTerminal() bool {
	return true
}

func (s *FailedState) vtxoStateSealed() {}

// ExpiredState is a terminal local-chain state. The batch timelock has
// elapsed, so the VTXO must not remain selectable or race the operator's sweep.
// A separate redemption coordinator checks whether the finalized sweep made
// the value eligible for an off-chain reissue.
type ExpiredState struct {
	// VTXO is the descriptor for this VTXO.
	VTXO *Descriptor

	// ObservedHeight is the chain height that established expiry.
	ObservedHeight int32
}

// String returns a human-readable state name.
func (s *ExpiredState) String() string {
	return "Expired"
}

// IsTerminal returns true because expiry monitoring moves to the redemption
// coordinator after this state is reached.
func (s *ExpiredState) IsTerminal() bool {
	return true
}

func (s *ExpiredState) vtxoStateSealed() {}

// RedeemingState is the terminal per-VTXO actor view of an expired claim that
// a round durably adopted. The round and redemption coordinator own recovery.
type RedeemingState struct {
	// VTXO is the descriptor for this VTXO.
	VTXO *Descriptor
}

// String returns a human-readable state name.
func (s *RedeemingState) String() string {
	return "Redeeming"
}

// IsTerminal returns true because the claim-bearing round owns progress.
func (s *RedeemingState) IsTerminal() bool {
	return true
}

func (s *RedeemingState) vtxoStateSealed() {}

// RedeemedState is the terminal state for an expired VTXO whose value was
// reissued into a replacement VTXO.
type RedeemedState struct {
	// VTXO is the descriptor for this VTXO.
	VTXO *Descriptor
}

// String returns a human-readable state name.
func (s *RedeemedState) String() string {
	return "Redeemed"
}

// IsTerminal returns true because the old VTXO has been replaced.
func (s *RedeemedState) IsTerminal() bool {
	return true
}

func (s *RedeemedState) vtxoStateSealed() {}
