package vtxo

import (
	"context"
	"errors"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightninglabs/darepo-client/lib/types"
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

// Ancestry describes one rooted commitment-tree fragment that contributes
// ancestry to a VTXO. The canonical type lives in lib/types so that both
// round.ClientVTXO and vtxo.Descriptor can carry the same multi-fragment
// ancestry without an import cycle (vtxo already imports round). This
// alias keeps the legacy vtxo.Ancestry symbol working for callers.
type Ancestry = types.Ancestry

// Descriptor contains all information needed to track and spend a VTXO. This
// is the canonical representation persisted to storage and passed between
// actors.
type Descriptor struct {
	// Outpoint identifies the VTXO's location in the virtual transaction
	// tree.
	Outpoint wire.OutPoint

	// Amount is the value of this VTXO in satoshis.
	Amount btcutil.Amount

	// PolicyTemplate is the semantic arkscript policy for this
	// VTXO. This is the authoritative representation for
	// ownership and spend semantics.
	PolicyTemplate []byte

	// PkScript is the output script for this VTXO (taproot with
	// collaborative and timeout spend paths).
	PkScript []byte

	// ClientKey is the client's key descriptor for this VTXO.
	ClientKey keychain.KeyDescriptor

	// OperatorKey is the operator's public key for collaborative spends.
	OperatorKey *btcec.PublicKey

	// TapScript contains the full tapscript structure for this VTXO,
	// including the internal key and all script paths. This is needed for
	// signing forfeit transactions via the collaborative spend path.
	TapScript *waddrmgr.Tapscript

	// Ancestry is the set of rooted tree fragments required to claim this
	// VTXO unilaterally on-chain. A round-direct VTXO and same-commitment
	// OOR VTXOs have len(Ancestry) == 1; cross-commitment multi-input OOR
	// VTXOs have one entry per distinct contributing commitment tx. The
	// per-entry tree fragments are minimal extracted paths, not whole
	// trees, so size scales with depth, not with batch fan-out.
	Ancestry []Ancestry

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

	// ChainDepth is the number of OOR checkpoint transactions between
	// this VTXO and the most recent on-chain commitment. A VTXO
	// created directly from a round has ChainDepth 0. Each OOR hop
	// adds one to the chain depth. This is distinct from per-Ancestry
	// TreeDepth, which tracks position within a single commitment tree.
	ChainDepth int

	// CreatedHeight is the block height when this VTXO was created.
	CreatedHeight int32

	// Status is the current lifecycle status of the VTXO.
	Status VTXOStatus

	// ConstructionVersion is the per-VTXO construction version: the rules
	// under which this VTXO was built and must be spent/exited. It is
	// stamped at creation and never changes. Today the only understood
	// value is ConstructionVersionV1; a future, genuinely different
	// construction is added additively here. Versions are zero-indexed, so
	// ConstructionVersionV1 is the Go zero value: an unstamped descriptor
	// reads as V1 with no separate unset sentinel. The value is validated
	// at the ingress edge (where it is adopted from the operator); the db
	// stamps/reads it verbatim without validating.
	//
	// TODO(v2): the operator stamps construction_version on the indexer
	// VTXO wire message (arkrpc.VTXO), but the client materialization path
	// does not yet read it onto this field (the IncomingVTXOEvent the
	// client actually materializes from carries no version field), so a
	// freshly materialized VTXO is stamped V1 at persistence. When a second
	// construction exists, add the field to IncomingVTXOEvent and thread it
	// here so the receive path mirrors the operator's value.
	ConstructionVersion arkrpc.ConstructionVersion
}

// MaxTreeDepth returns the largest TreeDepth across the Descriptor's
// Ancestry. This is the worst-case position within the deepest commitment
// tree fragment that contributes ancestry to this VTXO and drives expiry
// timing decisions. Returns 0 for descriptors with no ancestry.
func (d *Descriptor) MaxTreeDepth() int {
	if d == nil {
		return 0
	}

	var deepest int
	for _, a := range d.Ancestry {
		if int(a.TreeDepth) > deepest {
			deepest = int(a.TreeDepth)
		}
	}

	return deepest
}

// PrimaryAncestry returns the first Ancestry entry, or nil if none exist.
// Convenience helper for code paths that pre-date multi-tree support and
// only ever care about one tree fragment (round-direct VTXOs and the
// common single-commitment OOR case).
func (d *Descriptor) PrimaryAncestry() *Ancestry {
	if d == nil || len(d.Ancestry) == 0 {
		return nil
	}

	return &d.Ancestry[0]
}

// ErrVTXONotFound is returned by VTXOStore.GetVTXO when the store has no
// record of the requested outpoint. It is the domain-level miss signal, so
// callers match on it rather than a persistence-layer error like
// sql.ErrNoRows: the manager decides how a missing VTXO reads (e.g. a declined
// force-unroll) without depending on how the store is backed.
var ErrVTXONotFound = errors.New("vtxo not found")

// VTXOStore defines the persistence interface for VTXO lifecycle management.
// The store provides per-VTXO operations since each VTXO has its own actor.
// The VTXO manager (parent actor) tracks active VTXOs and routes block epochs.
//
//nolint:interfacebloat
type VTXOStore interface {
	// SaveVTXO persists a new VTXO to storage. Called when a VTXO actor is
	// created. Returns error if a VTXO with the same outpoint already
	// exists.
	SaveVTXO(ctx context.Context, vtxo *Descriptor) error

	// GetVTXO retrieves a VTXO by its outpoint. Used for actor recovery on
	// startup. Returns ErrVTXONotFound if the outpoint is not stored.
	GetVTXO(ctx context.Context,
		outpoint wire.OutPoint) (*Descriptor, error)

	// ListLiveVTXOs returns all VTXOs not in a terminal state. Used during
	// startup to recover active VTXO actors after restart.
	ListLiveVTXOs(ctx context.Context) ([]*Descriptor, error)

	// ListVTXOsByStatus returns all VTXOs matching the given status.
	// This enables querying terminal states (spent, forfeited) that
	// ListLiveVTXOs excludes.
	ListVTXOsByStatus(ctx context.Context,
		status VTXOStatus) ([]*Descriptor, error)

	// ListSelectionCandidatesByStatus returns the lightweight
	// (outpoint, amount, pkScript) projection coin selection runs on.
	// Selection happens on every payment and needs only these fields,
	// so this avoids decoding full descriptors on the hot path.
	ListSelectionCandidatesByStatus(ctx context.Context,
		status VTXOStatus) ([]SelectedVTXO, error)

	// UpdateVTXOStatus atomically updates a VTXO's status. This is the
	// primary method for state transitions.
	UpdateVTXOStatus(ctx context.Context, outpoint wire.OutPoint,
		status VTXOStatus) error

	// UpdateVTXOStatusReleasingReservation updates a VTXO's status and
	// deletes its spending-reservation row in a single transaction. Used
	// for transitions that move a VTXO out of SpendingState (completed,
	// released, or escalated to unilateral exit) so the durable index can
	// never retain a stale row that would mask a future orphan on the same
	// outpoint.
	UpdateVTXOStatusReleasingReservation(ctx context.Context,
		outpoint wire.OutPoint, status VTXOStatus) error

	// MarkForfeiting transitions a VTXO to forfeiting state and persists
	// the signed forfeit transaction for crash recovery. Called when
	// entering the forfeit flow before the new round's commitment confirms.
	MarkForfeiting(ctx context.Context, outpoint wire.OutPoint,
		roundID string, forfeitTx *wire.MsgTx) error

	// GetForfeitTx retrieves the persisted forfeit transaction for a VTXO.
	// Used during recovery to restore the ForfeitingState with its tx.
	// Returns nil if no forfeit tx is stored for this outpoint.
	GetForfeitTx(ctx context.Context,
		outpoint wire.OutPoint) (*wire.MsgTx, error)

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

// SpendingReservationStore is the subset of the durable spending-reservation
// index the VTXO manager needs for its startup orphan sweep: it lists live
// reservations so the sweep can distinguish orphaned Spending VTXOs from
// in-flight ones. Row deletion is not part of this interface because it
// happens atomically with the VTXO status change inside the VTXO actor's
// transition (see VTXOStore.UpdateVTXOStatusReleasingReservation). It is
// intentionally narrow so the vtxo package does not import the concrete db
// type or the oor package.
type SpendingReservationStore interface {
	// ListReservedOutpoints returns every outpoint currently reserved by a
	// live spend owner. Used by the startup sweep to distinguish orphaned
	// Spending VTXOs (no reservation row) from in-flight ones.
	ListReservedOutpoints(ctx context.Context) ([]wire.OutPoint, error)
}

// VTXOWallet defines the signing interface for VTXO operations.
type VTXOWallet interface {
	input.Signer
}
