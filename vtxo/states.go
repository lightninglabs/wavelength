package vtxo

import (
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
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

// UnilateralExitState is a terminal state indicating the VTXO has reached
// critical expiry and has been sent to the chain resolver for unilateral
// on-chain exit handling. The chain resolver takes over from this point.
type UnilateralExitState struct {
	// VTXO is the descriptor for this VTXO.
	VTXO *Descriptor

	// Reason explains why the VTXO is being unilaterally exited.
	Reason string
}

// String returns a human-readable state name.
func (s *UnilateralExitState) String() string {
	return "UnilateralExit"
}

// IsTerminal returns true since UnilateralExitState is a terminal state.
func (s *UnilateralExitState) IsTerminal() bool {
	return true
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
