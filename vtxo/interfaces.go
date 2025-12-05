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
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

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

	// VTXOStatusRefreshRequested indicates a refresh has been requested but
	// not yet completed via a new round.
	VTXOStatusRefreshRequested

	// VTXOStatusForfeiting indicates the VTXO is being forfeited in a
	// round.
	VTXOStatusForfeiting

	// VTXOStatusForfeited indicates the VTXO has been forfeited
	// (terminal).
	VTXOStatusForfeited

	// VTXOStatusSpent indicates the VTXO was spent via an out-of-round
	// (OOR) transaction (terminal).
	VTXOStatusSpent

	// VTXOStatusExpiring indicates the VTXO is critically close to expiry
	// and has been sent to the chain resolver (terminal for this actor).
	VTXOStatusExpiring

	// VTXOStatusFailed indicates an unrecoverable error (terminal).
	VTXOStatusFailed
)

// String returns a human-readable representation of the VTXO status.
func (s VTXOStatus) String() string {
	switch s {
	case VTXOStatusLive:
		return "live"
	case VTXOStatusRefreshRequested:
		return "refresh_requested"
	case VTXOStatusForfeiting:
		return "forfeiting"
	case VTXOStatusForfeited:
		return "forfeited"
	case VTXOStatusSpent:
		return "spent"
	case VTXOStatusExpiring:
		return "expiring"
	case VTXOStatusFailed:
		return "failed"
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

	// ClientKey is the client's key descriptor for this VTXO.
	ClientKey keychain.KeyDescriptor

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

	// TreeDepth is the depth of this VTXO in the VTXT (used for expiry
	// calculation).
	TreeDepth int

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

	// UpdateVTXOStatus atomically updates a VTXO's status. This is the
	// primary method for state transitions.
	UpdateVTXOStatus(
		ctx context.Context, outpoint wire.OutPoint, status VTXOStatus,
	) error

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
