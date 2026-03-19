package vtxo

import (
	"context"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/round"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// =============================================================================
// MESSAGE SPEC
// =============================================================================
//
// This section documents all message types that flow into and out of the VTXO
// FSM actor. This provides a quick reference similar to a protobuf service
// definition, showing the complete message surface at a glance.
//
// Message flow:
//
//	                 ┌──────────┐
//	 BlockEpochEvent─│          │─ForfeitRequest ──▶ Round
//	                 │          │
//	 ForfeitRequest ─│          │─ForfeitSigSubmit ─▶ Round
//	    (from Round) │          │
//	 ForfeitConfirm ─│ VTXO FSM │─ExpiringNotify ───▶ ChainResolver
//	    (from Round) │          │
//	 SpendReserve   ─│          │─StatusUpdate ─────▶ Persistence
//	 SpendReleased  ─│          │
//	 SpendCompleted ─│          │─TerminatedNotify ─▶ VTXOManager
//	 ForfeitReleased─│          │
//	 ResumeVTXOEvent─│          │
//	    (from Mgr)   └──────────┘
//
// =============================================================================

// InboundEvent documents an event that flows INTO the FSM from an external
// source. Used purely for documentation purposes.
type InboundEvent[E VTXOEvent] struct{}

// OutboundMsg documents a message that flows OUT of the FSM to other actors.
// Used purely for documentation purposes.
type OutboundMsg[M VTXOOutMsg] struct{}

// InternalEvent documents an event used within the FSM for internal state
// transitions. These are typically emitted by one state and consumed by the
// same or a subsequent state. Used purely for documentation purposes.
type InternalEvent[E VTXOEvent] struct{}

// MessageSpec documents all message types supported by the VTXO FSM actor.
// This provides a quick reference showing inbound events (what drives the FSM)
// and outbound messages (what the FSM emits) at a glance.
//
// # Inbound Events
//
// Events are received from external actors and drive state transitions:
//
//   - BlockEpochEvent: From chain source (via VTXO manager), triggers expiry
//     checks on each new block.
//   - PendingForfeitEvent: From round actor, commits the VTXO to cooperative
//     consumption before concrete forfeit details are available.
//   - ForfeitRequestEvent: From round actor, initiates forfeit signing flow.
//   - ForfeitConfirmedEvent: From round actor, confirms forfeit completion.
//   - ResumeVTXOEvent: From VTXO manager, restores state after crash recovery.
//   - VTXOFailedEvent: From any source, signals unrecoverable failure.
//
// # Outbound Messages
//
// Messages are emitted via the FSM outbox and routed to target actors:
//
//   - ForfeitRequest: To round actor, requests VTXO forfeit in next batch.
//   - ForfeitSignatureSubmission: To round actor, submits signed forfeit tx.
//   - ExpiringNotification: To chain resolver, escalates critical expiry.
//   - VTXOStatusUpdate: To persistence layer, updates database state.
//   - VTXOTerminatedNotification: To VTXO manager, signals actor cleanup.
//
// # Internal Events
//
//   - ForfeitSignedEvent: Updates ForfeitTxID after signing (currently unused
//     in production, reserved for round actor acknowledgment flow).
var MessageSpec = struct {
	// -----------------------------------------------------------------
	// INBOUND EVENTS (from external actors → FSM)
	// -----------------------------------------------------------------

	// BlockEpochEvent is received from the chain source (routed via VTXO
	// manager) when a new block is connected. Triggers expiry checks and
	// may cause transitions to PendingForfeit or UnilateralExit if expiry
	// thresholds are crossed.
	//
	// Source: Chain source → VTXO Manager → VTXO Actor
	// Handled in: LiveState, PendingForfeitState, ForfeitingState
	BlockEpochEvent InboundEvent[*BlockEpochEvent]

	// PendingForfeitEvent is received from the round actor after it has
	// accepted this VTXO into a pending cooperative-consumption round, but
	// before it has concrete connector details to sign.
	//
	// Source: Round Actor → VTXO Actor
	// Handled in: LiveState, PendingForfeitState
	PendingForfeitEvent InboundEvent[*round.PendingForfeitEvent]

	// ForfeitRequestEvent is received from the round actor when this VTXO
	// has been selected for inclusion in a batch swap. The FSM should sign
	// the forfeit transaction and submit it back to the round actor.
	//
	// Source: Round Actor → VTXO Actor
	// Handled in: LiveState, PendingForfeitState (signs and transitions
	// to Forfeiting)
	ForfeitRequestEvent InboundEvent[*round.ForfeitRequestEvent]

	// ForfeitConfirmedEvent is received from the round actor when the new
	// commitment transaction has confirmed on-chain. This marks the forfeit
	// as complete and transitions to terminal ForfeitedState.
	//
	// Source: Round Actor → VTXO Actor
	// Handled in: ForfeitingState
	ForfeitConfirmedEvent InboundEvent[*round.ForfeitConfirmedEvent]

	// ResumeVTXOEvent is received from the VTXO manager during crash
	// recovery to restore the FSM to its persisted state.
	//
	// Source: VTXO Manager → VTXO Actor
	// Handled in: All non-terminal states
	ResumeVTXOEvent InboundEvent[*ResumeVTXOEvent]

	// SpendReserveEvent claims a VTXO for an out-of-round (OOR) spend.
	// Only valid from LiveState. Rejected from PendingForfeitState,
	// SpendingState, and all terminal states.
	//
	// Source: VTXO Manager (on behalf of wallet) → VTXO Actor
	// Handled in: LiveState
	SpendReserveEvent InboundEvent[*SpendReserveEvent]

	// SpendReleasedEvent releases a VTXO from spend reservation back
	// to LiveState. Only valid from SpendingState.
	//
	// Source: VTXO Manager (on behalf of wallet) → VTXO Actor
	// Handled in: SpendingState
	SpendReleasedEvent InboundEvent[*SpendReleasedEvent]

	// SpendCompletedEvent marks an OOR spend as finalized. The VTXO
	// transitions to terminal SpentState.
	//
	// Source: VTXO Manager (on behalf of OOR FSM) → VTXO Actor
	// Handled in: SpendingState
	SpendCompletedEvent InboundEvent[*SpendCompletedEvent]

	// ForfeitReleasedEvent releases a VTXO from pending forfeit back
	// to LiveState. Only valid from PendingForfeitState.
	//
	// Source: VTXO Manager (on behalf of wallet) → VTXO Actor
	// Handled in: PendingForfeitState
	ForfeitReleasedEvent InboundEvent[*ForfeitReleasedEvent]

	// VTXOFailedEvent signals an unrecoverable error from any source.
	// Transitions to terminal FailedState.
	//
	// Source: Any actor or internal error path
	// Handled in: All non-terminal states
	VTXOFailedEvent InboundEvent[*VTXOFailedEvent]

	// -----------------------------------------------------------------
	// OUTBOUND MESSAGES (FSM → external actors)
	// -----------------------------------------------------------------

	// ForfeitRequest is sent when the VTXO's expiry status crosses
	// the pending-forfeit threshold, requesting cooperative forfeiture
	// in the next batch swap.
	//
	// Destination: VTXO Actor → Round Actor
	// Emitted from: LiveState (on ExpiryStatusNeedsRefresh)
	ForfeitRequest OutboundMsg[*ForfeitRequest]

	// ForfeitSignatureSubmission is sent to the round actor with the
	// client's signature on the forfeit transaction. The round actor
	// combines this with the operator signature and broadcasts.
	//
	// Destination: VTXO Actor → Round Actor
	// Emitted from: LiveState, PendingForfeitState (on ForfeitRequest)
	ForfeitSignatureSubmission OutboundMsg[*ForfeitSignatureSubmission]

	// ExpiringNotification is sent to the chain resolver when the VTXO
	// reaches critical expiry and must begin unilateral exit. This is a
	// terminal transition for this actor.
	//
	// Destination: VTXO Actor → Chain Resolver
	// Emitted from: LiveState, PendingForfeitState, ForfeitingState
	//               (on ExpiryStatusCritical)
	ExpiringNotification OutboundMsg[*ExpiringNotification]

	// VTXOStatusUpdate is sent to the persistence layer to update the
	// VTXO's status in the database. Emitted on most state transitions.
	//
	// Destination: VTXO Actor → Persistence Layer
	// Emitted from: All state transitions that change VTXOStatus
	VTXOStatusUpdate OutboundMsg[*VTXOStatusUpdate]

	// VTXOTerminatedNotification is sent to the VTXO manager when this
	// actor reaches a terminal state. The manager should clean up the
	// actor reference.
	//
	// Destination: VTXO Actor → VTXO Manager
	// Emitted from: All terminal state transitions
	VTXOTerminatedNotification OutboundMsg[*VTXOTerminatedNotification]

	// -----------------------------------------------------------------
	// INTERNAL EVENTS (within FSM)
	// -----------------------------------------------------------------

	// ForfeitSignedEvent updates the ForfeitTxID after signing. Currently
	// reserved for future round actor acknowledgment flow; not emitted in
	// production code paths.
	//
	// Source: Internal (future: Round Actor acknowledgment)
	// Handled in: ForfeitingState
	ForfeitSignedEvent InternalEvent[*ForfeitSignedEvent]
}{}

// VTXOStateTransition is a type alias for the verbose protofsm.StateTransition
// type used throughout the VTXO FSM. The baselib protofsm uses 3 type
// parameters: InternalEvent, OutboxEvent, and Env. In our case:
//   - InternalEvent = VTXOEvent (events that drive the FSM).
//   - OutboxEvent = VTXOOutMsg (outbox messages emitted by transitions).
//   - Env = *VTXOEnvironment.
type VTXOStateTransition = protofsm.StateTransition[
	VTXOEvent, VTXOOutMsg, *VTXOEnvironment,
]

// VTXOEmittedEvent is a type alias for the verbose protofsm.EmittedEvent type
// used when state transitions emit new events or outbox messages.
type VTXOEmittedEvent = protofsm.EmittedEvent[VTXOEvent, VTXOOutMsg]

// VTXOStateMachine is a type alias for the VTXO FSM.
type VTXOStateMachine = protofsm.StateMachine[
	VTXOEvent, VTXOOutMsg, *VTXOEnvironment,
]

// VTXOStateMachineCfg is a type alias for the VTXO FSM configuration.
type VTXOStateMachineCfg = protofsm.StateMachineCfg[
	VTXOEvent, VTXOOutMsg, *VTXOEnvironment,
]

// VTXOStatus represents the lifecycle state of a VTXO.
type VTXOStatus int

const (
	// VTXOStatusLive indicates the VTXO is active and can be spent.
	VTXOStatusLive VTXOStatus = iota

	// VTXOStatusPendingForfeit indicates the VTXO has been committed to
	// cooperative consumption and is awaiting forfeit details from the
	// round actor.
	VTXOStatusPendingForfeit

	// VTXOStatusForfeiting indicates the VTXO is being forfeited in a
	// round.
	VTXOStatusForfeiting

	// VTXOStatusForfeited indicates the VTXO has been forfeited
	// (terminal).
	VTXOStatusForfeited

	// VTXOStatusSpent indicates the VTXO was spent via an out-of-round
	// (OOR) transaction (terminal).
	VTXOStatusSpent

	// VTXOStatusUnilateralExit indicates the VTXO has reached critical
	// expiry and been sent to the chain resolver for on-chain exit
	// (terminal for this actor).
	VTXOStatusUnilateralExit

	// VTXOStatusFailed indicates an unrecoverable error (terminal).
	VTXOStatusFailed

	// VTXOStatusSpending indicates the VTXO has been claimed for an
	// out-of-round (OOR) spend. The VTXO is unavailable for cooperative
	// forfeit or other operations until the spend completes or is
	// released. Persisted so the claim survives restarts.
	//
	// NOTE: Placed after VTXOStatusFailed to preserve the numeric
	// values of existing statuses used in SQL queries.
	VTXOStatusSpending
)

// String returns a human-readable representation of the VTXO status.
func (s VTXOStatus) String() string {
	switch s {
	case VTXOStatusLive:
		return "live"

	case VTXOStatusPendingForfeit:
		return "pending_forfeit"

	case VTXOStatusForfeiting:
		return "forfeiting"

	case VTXOStatusForfeited:
		return "forfeited"

	case VTXOStatusSpent:
		return "spent"

	case VTXOStatusUnilateralExit:
		return "unilateral_exit"

	case VTXOStatusFailed:
		return "failed"

	case VTXOStatusSpending:
		return "spending"

	default:
		return "unknown"
	}
}

// Descriptor contains all information needed to track and spend a VTXO. This
// is the canonical representation persisted to storage and passed between
// actors.
type Descriptor struct {
	// Outpoint identifies the VTXO's location in the virtual transaction
	// tree.
	Outpoint wire.OutPoint

	// Amount is the value of this VTXO in satoshis.
	Amount btcutil.Amount

	// PkScript is the output script for this VTXO (taproot with
	// collaborative and timeout spend paths).
	PkScript []byte

	// OwnerKey is the client's key descriptor for this VTXO.
	OwnerKey keychain.KeyDescriptor

	// OperatorKey is the operator's public key for collaborative spends.
	OperatorKey *btcec.PublicKey

	// TapScript contains the full tapscript structure for this VTXO,
	// including the internal key and all script paths. This is needed for
	// signing forfeit transactions via the collaborative spend path.
	TapScript *waddrmgr.Tapscript

	// TreePath is the extracted path from the commitment transaction output
	// down to this specific VTXO. Contains only the minimal tree nodes
	// needed for unilateral exit.
	TreePath *tree.Tree

	// RoundID identifies which round created this VTXO.
	RoundID string

	// CommitmentTxID is the txid of the commitment transaction.
	CommitmentTxID chainhash.Hash

	// BatchExpiry is the absolute block height at which the batch expires
	// (operator can sweep via the batch-level timelock).
	BatchExpiry int32

	// RelativeExpiry is the CSV delay for the VTXO's unilateral exit path
	// (blocks from when VTXO is realized on-chain).
	RelativeExpiry uint32

	// TreeDepth is the depth of this VTXO in the VTXT (virtual
	// transaction tree). This is the VTXO's position within the
	// commitment tree and is used for expiry calculation.
	TreeDepth int

	// ChainDepth is the number of OOR checkpoint transactions between
	// this VTXO and the most recent on-chain commitment. A VTXO
	// created directly from a round has ChainDepth 0. Each OOR hop
	// adds one to the chain depth. This is distinct from TreeDepth,
	// which tracks position within the VTXT.
	ChainDepth int

	// CreatedHeight is the block height when this VTXO was created.
	CreatedHeight int32

	// Status is the current lifecycle status of the VTXO.
	Status VTXOStatus
}

// VTXOStore defines the persistence interface for VTXO lifecycle management.
// The store provides per-VTXO operations since each VTXO has its own actor.
// The VTXO manager (parent actor) tracks active VTXOs and routes block epochs.
type VTXOStore interface {
	// SaveVTXO persists a new VTXO to storage. Called when a VTXO actor is
	// created. Returns error if a VTXO with the same outpoint already
	// exists.
	SaveVTXO(ctx context.Context, vtxo *Descriptor) error

	// GetVTXO retrieves a VTXO by its outpoint. Used for actor recovery on
	// startup. Returns error if not found.
	GetVTXO(ctx context.Context, outpoint wire.OutPoint) (
		*Descriptor, error,
	)

	// ListLiveVTXOs returns all VTXOs not in a terminal state. Used during
	// startup to recover active VTXO actors after restart.
	ListLiveVTXOs(ctx context.Context) ([]*Descriptor, error)

	// ListVTXOsByStatus returns all VTXOs matching the given status.
	// This enables querying terminal states (spent, forfeited) that
	// ListLiveVTXOs excludes.
	ListVTXOsByStatus(ctx context.Context, status VTXOStatus) (
		[]*Descriptor, error,
	)

	// UpdateVTXOStatus atomically updates a VTXO's status. This is the
	// primary method for state transitions.
	UpdateVTXOStatus(
		ctx context.Context, outpoint wire.OutPoint, status VTXOStatus,
	) error

	// MarkForfeiting transitions a VTXO to forfeiting state and persists
	// the signed forfeit transaction for crash recovery. Called when
	// entering the forfeit flow before the new round's commitment confirms.
	MarkForfeiting(ctx context.Context, outpoint wire.OutPoint,
		roundID string, forfeitTx *wire.MsgTx) error

	// GetForfeitTx retrieves the persisted forfeit transaction for a VTXO.
	// Used during recovery to restore the ForfeitingState with its tx.
	// Returns nil if no forfeit tx is stored for this outpoint.
	GetForfeitTx(ctx context.Context, outpoint wire.OutPoint) (
		*wire.MsgTx, error,
	)

	// MarkForfeited marks a VTXO as forfeited and records the forfeit
	// transaction ID. This is called when the new round's commitment
	// transaction confirms.
	MarkForfeited(
		ctx context.Context, outpoint wire.OutPoint,
		forfeitTxID chainhash.Hash,
	) error

	// DeleteVTXO removes a VTXO from storage. Used for cleanup after
	// terminal states are reached and the VTXO is no longer needed.
	DeleteVTXO(ctx context.Context, outpoint wire.OutPoint) error
}

// VTXOWallet defines the signing interface for VTXO operations.
type VTXOWallet interface {
	input.Signer
}
