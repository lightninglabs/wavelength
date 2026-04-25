package rounds

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// StateTransition is a type alias for the verbose protofsm.StateTransition
// type used throughout the round FSM. This makes function signatures more
// readable and easier to maintain. The baselib protofsm uses 3
// type parameters: InternalEvent, OutboxEvent, and Env. In our case:
//   - InternalEvent = Event (events that drive the FSM).
//   - OutboxEvent = OutboxEvent (outbox messages emitted by transitions).
//   - Env = *Environment.
type StateTransition = protofsm.StateTransition[
	Event, OutboxEvent, *Environment,
]

// EmittedEvent is a type alias for the verbose protofsm.EmittedEvent type
// used when state transitions emit new events or outbox messages. This
// improves readability of state transition return values.
type EmittedEvent = protofsm.EmittedEvent[Event, OutboxEvent]

// StateMachine is a type alias for the server rounds FSM.
type StateMachine = protofsm.StateMachine[
	Event, OutboxEvent, *Environment,
]

// StateMachineCfg is a type alias for the server FSM configuration.
type StateMachineCfg = protofsm.StateMachineCfg[
	Event, OutboxEvent, *Environment,
]

// RoundID is a type alias for round identifiers.
type RoundID uuid.UUID

// NewRoundID generates a new unique RoundID using cryptographic randomness.
func NewRoundID() (RoundID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return RoundID{}, err
	}

	return RoundID(id), nil
}

// String returns the full string representation of the RoundID.
func (id RoundID) String() string {
	return uuid.UUID(id).String()
}

// LogPrefix returns a short string representation of the RoundID for logging.
// It uses the last 4 bytes (32 bits) of the UUIDv7, which are high-entropy
// random bits.
func (id RoundID) LogPrefix() string {
	// Last 4 bytes = 32 bits of pure randomness.
	return fmt.Sprintf("round(%v)", hex.EncodeToString(id[12:16]))
}

// RoundFSM wraps a state machine instance for a specific round.
type RoundFSM struct {
	// FSM is the state machine for this round.
	FSM *StateMachine

	// RoundID is the unique identifier for this round.
	RoundID RoundID
}

// BoardingInputLocker provides thread-safe locking of boarding inputs
// across concurrent rounds to prevent double-spending.
type BoardingInputLocker interface {
	// Lock attempts to lock a boarding input for the specified round.
	// Returns an error if the input is already locked by another round.
	Lock(ctx context.Context, outpoint *wire.OutPoint,
		roundID RoundID) error

	// Unlock releases the lock on a boarding input for the specified round.
	// Only the round that locked the input can unlock it.
	Unlock(ctx context.Context, outpoint *wire.OutPoint,
		roundID RoundID) error

	// IsLocked checks if an input is locked and returns the locking round
	// ID if it is locked.
	IsLocked(ctx context.Context,
		outpoint *wire.OutPoint) (bool, RoundID, error)
}

// VTXOEventPublisher publishes VTXO lifecycle events to the indexer.
// Used by the rounds actor to notify registered receive-script holders
// when their VTXOs are created in a confirmed round.
type VTXOEventPublisher interface {
	// PublishVTXOCreated publishes a VTXO_CREATED event for a
	// confirmed round VTXO output. The batchExpiry must be
	// an absolute block height (confirmation_height +
	// sweep_delay).
	PublishVTXOCreated(ctx context.Context, pkScript []byte,
		outpoint wire.OutPoint, valueSat int64,
		roundID string, batchExpiry int32,
		relativeExpiry uint32,
		origin arkrpc.VTXOOrigin,
		commitmentTxid []byte) error
}

// ChainSource provides access to blockchain data for UTXO validation.
type ChainSource interface {
	// GetUTXO fetches the UTXO for the given outpoint. Returns an error
	// if the UTXO doesn't exist or has been spent.
	GetUTXO(outpoint wire.OutPoint) (*UTXO, error)
}

// UTXO represents a UTXO along with its confirmation count.
type UTXO struct {
	// Output is the transaction output.
	Output *wire.TxOut

	// Confirmations is the number of confirmations for this UTXO.
	Confirmations int64
}

// FundingOpts carries optional UTXO lease parameters for FundPsbt.
type FundingOpts struct {
	// LockID is a 32-byte identifier for the UTXO lease. When set,
	// FundPsbt passes this as CustomLockId to LND. Using a
	// deterministic ID (e.g. derived from the round ID) allows the
	// caller to release leases explicitly on failure.
	LockID [32]byte

	// LockDuration is how long LND should hold the UTXO lease.
	// When zero, LND uses its default (10 minutes).
	LockDuration time.Duration
}

// WalletController provides PSBT funding and signing operations. This
// interface wraps the subset of lnd's WalletController that we need for
// commitment transaction construction. It also embeds input.Signer for signing
// individual inputs like boarding inputs with operator keys.
type WalletController interface {
	input.Signer

	// FundPsbt performs coin selection and adds wallet inputs to fund
	// the outputs in the PSBT. It also adds a change output if needed.
	// Returns the change output index (-1 if no change) and the list
	// of wallet outpoints that were leased during coin selection.
	FundPsbt(ctx context.Context, packet *psbt.Packet,
		minConfs int32, feeRate chainfee.SatPerKWeight,
		account string,
		opts *FundingOpts) (int32, []wire.OutPoint, error)

	// ReleaseInputs releases UTXO leases acquired by a prior
	// FundPsbt call. The lockID must match the one used during
	// funding.
	ReleaseInputs(ctx context.Context, lockID [32]byte,
		outpoints []wire.OutPoint) error

	// FinalizePsbt signs all wallet-controlled inputs and finalizes the
	// PSBT, making it ready for broadcast. Returns the finalized raw
	// transaction.
	FinalizePsbt(ctx context.Context,
		packet *psbt.Packet) (*wire.MsgTx, error)
}

// RoundStore provides persistent storage for rounds.
type RoundStore interface {
	// PersistRound saves a completed round to persistent storage. This
	// should be called after all signatures have been collected and the
	// transaction is ready for broadcast.
	PersistRound(ctx context.Context, round *Round) error

	// MarkRoundConfirmed marks a pending round as confirmed with the block
	// details for the broadcast commitment transaction.
	MarkRoundConfirmed(ctx context.Context, roundID RoundID,
		blockHeight int32, blockHash chainhash.Hash) error

	// LoadPendingRounds returns all rounds that have been finalized but not
	// yet confirmed on-chain. These rounds need to be reloaded into memory
	// on restart so we can continue tracking them until confirmation.
	LoadPendingRounds(ctx context.Context) ([]*Round, error)
}

// Round contains all data needed to persist a completed round.
type Round struct {
	// RoundID is the unique identifier for this round.
	RoundID RoundID

	// FinalTx is the fully signed commitment transaction.
	FinalTx *wire.MsgTx

	// VTXOTrees maps commitment tx output indices to their VTXO trees.
	VTXOTrees map[int]*tree.Tree

	// ConnectorDescriptors describe connector outputs for this round.
	ConnectorDescriptors []*ConnectorTreeDescriptor

	// ForfeitInfos maps forfeited VTXO outpoints to forfeit metadata.
	ForfeitInfos map[wire.OutPoint]*ForfeitInfo

	// ClientRegistrations contains client registration data.
	ClientRegistrations map[clientconn.ClientID]*ClientRegistration

	// SweepKey is the operator public key used in VTXO sweep timeout
	// scripts. Required to reconstruct sweep scripts for unilateral exits.
	SweepKey *btcec.PublicKey

	// CSVDelay is the relative timelock in blocks for the VTXO sweep
	// timeout path. Required to reconstruct sweep scripts.
	CSVDelay uint32

	// ChangeOutputIdx is the FinalTx output index where FundPsbt put
	// the wallet change, or -1 when no change output was added.
	// Persisted so a rounds-actor restart can restore the ledger
	// attribution data on the reloaded FinalizedState; without it,
	// the UTXO diff classifier would mis-attribute the change
	// output as external_deposit on top of the round's capital
	// commitment ledger leg.
	ChangeOutputIdx int32

	// ConnectorOutputIndices is the sorted set of FinalTx output
	// indices that hold operator-controlled connector outputs
	// (dust outputs spent by forfeit transactions). Persisted
	// alongside ChangeOutputIdx so the classifier sees the full
	// round-attributable output set after restart.
	ConnectorOutputIndices []int32
}

// VTXOStatus represents the lifecycle state of a VTXO.
type VTXOStatus string

const (
	// VTXOStatusPending indicates the VTXO's commitment transaction has
	// been broadcast but not yet confirmed on-chain.
	VTXOStatusPending VTXOStatus = "pending"

	// VTXOStatusLive indicates the VTXO's commitment transaction has been
	// confirmed and the VTXO is now spendable.
	VTXOStatusLive VTXOStatus = "live"

	// VTXOStatusForfeited indicates the VTXO has been forfeited in a
	// subsequent round.
	VTXOStatusForfeited VTXOStatus = "forfeited"

	// VTXOStatusUnrolledByClient indicates the VTXO was revealed on-chain
	// by a recognized client-owned path before the server spent it via
	// forfeit or OOR. It must no longer be eligible for those cooperative
	// spend paths.
	VTXOStatusUnrolledByClient VTXOStatus = "unrolled_by_client"
)

// VTXO represents a Virtual Transaction Output that exists within a VTXO tree.
// It contains all the information needed to identify and spend the VTXO.
type VTXO struct {
	// Outpoint uniquely identifies this VTXO on-chain. This is computed
	// from the VTXO's position in the tree structure.
	Outpoint wire.OutPoint

	// RoundID is the identifier of the round that created this VTXO.
	RoundID RoundID

	// BatchOutputIndex is the index of the batch output in the commitment
	// transaction that roots the VTXO tree containing this VTXO.
	BatchOutputIndex int

	// Descriptor contains the VTXO specification (amount, script,
	// cosigner).
	Descriptor *tree.VTXODescriptor

	// Status is the current lifecycle state of the VTXO.
	Status VTXOStatus

	// BatchExpiry is the absolute block height at which this VTXO's
	// source batch becomes sweepable by the operator. Computed at load
	// time as `source_round.confirmation_height +
	// source_round.csv_delay`. Zero when the source round is not yet
	// confirmed or when the VTXO was not loaded through a store path
	// that populates it. The seal-time fee builder reads this to
	// derive `remainingBlocks = BatchExpiry - currentHeight` for the
	// liquidity-fee leg.
	BatchExpiry uint32
}

// VTXOStore provides the rounds projection over persistent VTXO data.
//
// This interface intentionally includes round/tree/forfeit semantics that are
// not part of the generic OOR-focused `vtxo.Store` yet. Keeping this
// projection explicit avoids accidental coupling until both subsystems share
// one canonical storage model.
type VTXOStore interface {
	// PersistVTXOs saves a batch of newly created VTXOs to storage. These
	// VTXOs are in unconfirmed state until the commitment transaction is
	// confirmed on-chain.
	PersistVTXOs(ctx context.Context, vtxos []*VTXO) error

	// MarkVTXOsLive updates the status of all VTXOs for a given round to
	// "live" after the commitment transaction has been confirmed.
	MarkVTXOsLive(ctx context.Context, roundID RoundID) error

	// MarkVTXOsExpired marks the given VTXOs as expired. This is used
	// when the operator sweeps an expired batch, making the entire
	// presigned tree (and all VTXOs in it) unspendable.
	MarkVTXOsExpired(ctx context.Context,
		outpoints []wire.OutPoint) error

	// MarkVTXOForfeit marks a VTXO as forfeited and stores the forfeit
	// metadata.
	MarkVTXOForfeit(ctx context.Context, outpoint wire.OutPoint,
		info *ForfeitInfo) error

	// MarkVTXOUnrolledByClient marks a live VTXO as no longer eligible
	// for cooperative forfeit or OOR handling because a recognized
	// client-owned on-chain path has already revealed it.
	MarkVTXOUnrolledByClient(ctx context.Context,
		outpoint wire.OutPoint) error

	// GetVTXO retrieves a VTXO by its outpoint. Returns nil and no error
	// if the VTXO doesn't exist.
	GetVTXO(ctx context.Context, outpoint wire.OutPoint) (*VTXO, error)

	// GetForfeitInfo retrieves forfeit metadata for a VTXO. Returns nil
	// and no error if the forfeit info doesn't exist.
	GetForfeitInfo(ctx context.Context,
		outpoint wire.OutPoint) (*ForfeitInfo, error)

	// LockVTXO locks VTXOs for forfeit in the specified round. This exists
	// as a temporary compatibility path while rounds migrates fully to the
	// shared VTXOLocker interface.
	LockVTXO(ctx context.Context, roundID RoundID,
		outpoints ...wire.OutPoint) error

	// UnlockVTXO releases locks previously acquired via LockVTXO.
	UnlockVTXO(ctx context.Context, roundID RoundID,
		outpoints ...wire.OutPoint) error

	// UnlockStaleVTXOs releases locks on VTXOs that are locked by rounds
	// not in the provided list of active round IDs. This is used on
	// startup to clean up stale locks from crashed rounds.
	UnlockStaleVTXOs(ctx context.Context,
		activeRoundIDs []RoundID) error
}

// loggingErrorReporter implements protofsm.ErrorReporter by logging errors
// using a given logger.
type loggingErrorReporter struct {
	log btclog.Logger
}

// newLoggingErrorReporter creates an error reporter that logs errors with the
// given logger.
func newLoggingErrorReporter(log btclog.Logger) *loggingErrorReporter {
	return &loggingErrorReporter{log: log}
}

// ReportError logs the error using the configured logger.
func (r *loggingErrorReporter) ReportError(err error) {
	r.log.ErrorS(context.Background(), "FSM error", err)
}

// Compile-time check that loggingErrorReporter implements ErrorReporter.
var _ protofsm.ErrorReporter = (*loggingErrorReporter)(nil)
