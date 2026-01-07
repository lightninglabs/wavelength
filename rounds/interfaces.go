package rounds

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
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

// WalletController provides PSBT funding and signing operations. This
// interface wraps the subset of lnd's WalletController that we need for
// commitment transaction construction. It also embeds input.Signer for signing
// individual inputs like boarding inputs with operator keys.
type WalletController interface {
	input.Signer

	// FundPsbt performs coin selection and adds wallet inputs to fund the
	// outputs in the PSBT. It also adds a change output if needed.
	// Returns the change output index (-1 if no change).
	FundPsbt(ctx context.Context, packet *psbt.Packet,
		minConfs int32, feeRate chainfee.SatPerKWeight,
		account string) (changeIndex int32, err error)

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

	// ClientRegistrations contains client registration data.
	ClientRegistrations map[clientconn.ClientID]*ClientRegistration
}

// VTXOStatus represents the lifecycle state of a VTXO.
type VTXOStatus string

const (
	// VTXOStatusUnconfirmed indicates the VTXO's commitment transaction has
	// been broadcast but not yet confirmed on-chain.
	VTXOStatusUnconfirmed VTXOStatus = "unconfirmed"

	// VTXOStatusLive indicates the VTXO's commitment transaction has been
	// confirmed and the VTXO is now spendable.
	VTXOStatusLive VTXOStatus = "live"
)

// VTXO represents a Virtual Transaction Output that exists within a VTXO tree.
// It contains all the information needed to identify and spend the VTXO.
type VTXO struct {
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
}

// VTXOStore provides persistent storage for VTXOs. The actor uses this to
// persist VTXOs when they are created and update their status when the
// commitment transaction is confirmed.
type VTXOStore interface {
	// PersistVTXOs saves a batch of newly created VTXOs to storage. These
	// VTXOs are in unconfirmed state until the commitment transaction is
	// confirmed on-chain.
	PersistVTXOs(ctx context.Context, vtxos []*VTXO) error

	// MarkVTXOsLive updates the status of all VTXOs for a given round to
	// "live" after the commitment transaction has been confirmed.
	MarkVTXOsLive(ctx context.Context, roundID RoundID) error
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
	r.log.Errorf("FSM error: %v", err)
}

// Compile-time check that loggingErrorReporter implements ErrorReporter.
var _ protofsm.ErrorReporter = (*loggingErrorReporter)(nil)
