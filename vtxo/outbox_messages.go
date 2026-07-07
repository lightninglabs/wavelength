package vtxo

import (
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/types"
)

// VTXOOutMsg is a sealed interface for messages emitted via the FSM outbox.
// These messages are sent to other actors (round actor, chain resolver) or
// used for persistence updates.
type VTXOOutMsg interface {
	vtxoOutMsgSealed()
}

// RefreshUrgency indicates how urgent a refresh request is.
type RefreshUrgency int

const (
	// RefreshUrgencyNormal indicates the VTXO has plenty of time remaining.
	RefreshUrgencyNormal RefreshUrgency = iota

	// RefreshUrgencyElevated indicates the VTXO should be refreshed soon.
	RefreshUrgencyElevated

	// RefreshUrgencyCritical indicates the VTXO must be refreshed
	// immediately.
	RefreshUrgencyCritical
)

// String returns a human-readable representation of the urgency level.
func (u RefreshUrgency) String() string {
	switch u {
	case RefreshUrgencyNormal:
		return "normal"

	case RefreshUrgencyElevated:
		return "elevated"

	case RefreshUrgencyCritical:
		return "critical"

	default:
		return "unknown"
	}
}

// ForfeitRequest is sent when a VTXO needs to be cooperatively forfeited.
// Routed through the manager to the round actor.
type ForfeitRequest struct {
	actor.BaseMessage

	// VTXOOutpoint identifies the VTXO to forfeit.
	VTXOOutpoint wire.OutPoint

	// LastCheckedHeight is the block height observed by the FSM at
	// the moment this request was emitted. The outbox processor
	// uses it to compute remaining-blocks for the operator fee
	// quote without reading the actor's current state (which has
	// already transitioned out of LiveState by the time the outbox
	// runs). Zero means "unknown"; the quoter then falls back to
	// the server's SweepDelay default.
	LastCheckedHeight int32
}

func (m *ForfeitRequest) vtxoOutMsgSealed() {}

// ExpiringNotification is sent to the chain resolver when a VTXO is critically
// close to expiry and needs unilateral exit handling. The chain resolver
// should begin the on-chain unrolling process.
type ExpiringNotification struct {
	actor.BaseMessage

	// VTXO is the full descriptor of the expiring VTXO.
	VTXO *Descriptor

	// BlocksRemaining is how many blocks until batch expiry.
	BlocksRemaining int32

	// Reason explains why the VTXO is being sent to chain resolver.
	Reason string
}

func (m ExpiringNotification) vtxoOutMsgSealed() {}

// MessageType returns the message type for logging.
func (m ExpiringNotification) MessageType() string {
	return "ExpiringNotification"
}

// ForfeitSignatureSubmission is sent to the round actor with the forfeit
// transaction signature.
type ForfeitSignatureSubmission struct {
	actor.BaseMessage

	// VTXOOutpoint identifies the VTXO being forfeited.
	VTXOOutpoint wire.OutPoint

	// RoundID is the round where the forfeit is being processed.
	RoundID string

	// ForfeitTx is the signed forfeit transaction.
	ForfeitTx *wire.MsgTx

	// Signature is the client's schnorr signature for the forfeit tx.
	Signature *schnorr.Signature

	// ParticipantVTXOSigs carries keyed non-operator signatures for custom
	// spend paths that require multiple client-side participants.
	ParticipantVTXOSigs []*types.ForfeitParticipantSig

	// SpendPath is the canonical arkscript spend path used for the VTXO
	// input of the forfeit transaction.
	SpendPath *arkscript.SpendPath
}

func (m *ForfeitSignatureSubmission) vtxoOutMsgSealed() {}

// VTXOStatusUpdate is a persistence request to update VTXO status. This is
// emitted on state transitions to keep the database in sync.
type VTXOStatusUpdate struct {
	actor.BaseMessage

	// Outpoint identifies the VTXO.
	Outpoint wire.OutPoint

	// NewStatus is the new status to set.
	NewStatus VTXOStatus

	// RoundID is the round ID for forfeiting state transitions. Empty for
	// other status updates.
	RoundID string

	// ForfeitTx is the signed forfeit transaction for forfeiting state
	// transitions. Nil for other status updates. Persisted for crash
	// recovery.
	ForfeitTx *wire.MsgTx

	// ReleaseSpendReservation, when true, instructs the persistence layer
	// to delete this outpoint's durable spending-reservation row in the
	// same transaction as the status update. Set on transitions that move
	// the VTXO out of SpendingState so the row never outlives the spend.
	ReleaseSpendReservation bool
}

func (m *VTXOStatusUpdate) vtxoOutMsgSealed() {}

// VTXOTerminatedNotification notifies the VTXO manager that this VTXO actor
// has reached a terminal state and can be cleaned up.
type VTXOTerminatedNotification struct {
	actor.BaseMessage

	// VTXOOutpoint identifies the VTXO.
	VTXOOutpoint wire.OutPoint

	// FinalState is the terminal state reached.
	FinalState string

	// Reason explains why the VTXO terminated.
	Reason string
}

func (m *VTXOTerminatedNotification) vtxoOutMsgSealed() {}
