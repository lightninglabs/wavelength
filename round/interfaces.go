package round

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/wallet"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// ClientStateTransition is a type alias for the verbose protofsm.StateTransition
// type used throughout the client round FSM. The baselib protofsm uses 3
// type parameters: InternalEvent, OutboxEvent, and Env. In our case:
//   - InternalEvent = ClientEvent (events that drive the FSM).
//   - OutboxEvent = ClientOutMsg (outbox messages emitted by transitions).
//   - Env = *ClientEnvironment.
//
//nolint:ll
type ClientStateTransition = protofsm.StateTransition[
	ClientEvent, ClientOutMsg, *ClientEnvironment,
]

// ClientEmittedEvent is a type alias for the verbose protofsm.EmittedEvent type
// used when state transitions emit new events or outbox messages.
type ClientEmittedEvent = protofsm.EmittedEvent[ClientEvent, ClientOutMsg]

// ClientStateMachine is a type alias for the client round FSM.
type ClientStateMachine = protofsm.StateMachine[
	ClientEvent, ClientOutMsg, *ClientEnvironment,
]

// ClientStateMachineCfg is a type alias for the client FSM configuration.
type ClientStateMachineCfg = protofsm.StateMachineCfg[
	ClientEvent, ClientOutMsg, *ClientEnvironment,
]

// Musig2PubNonce is an alias for the MuSig2 public nonce type from lib.
type Musig2PubNonce = tree.Musig2PubNonce

// RoundID is a unique identifier for rounds, using UUID v7 for time-ordering.
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
	return fmt.Sprintf("round(%v)", hex.EncodeToString(id[12:16]))
}

// ParseRoundID parses a RoundID from its string representation.
func ParseRoundID(s string) (RoundID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return RoundID{}, err
	}

	return RoundID(id), nil
}

// SignerKey is the 33-byte compressed public key used to identify a signer
// in MuSig2 sessions and client tree mappings.
type SignerKey = [33]byte

// SigningKeyHex is an alias for SignerKey, compatible with the server's
// route.Vertex type which is also a 33-byte compressed public key.
type SigningKeyHex = SignerKey

// NewSignerKey creates a SignerKey from a public key.
func NewSignerKey(pk *btcec.PublicKey) SignerKey {
	var key SignerKey
	copy(key[:], pk.SerializeCompressed())
	return key
}

// Type aliases for wallet types. The wallet package owns the canonical
// definitions of boarding-related types. These aliases allow round package
// code to use the types without import clutter.
type (
	// BoardingAddress is a type alias for wallet.BoardingAddress. The
	// wallet package owns the canonical definition of boarding addresses
	// including the tapscript, keys, and exit delay. The round package
	// uses this type directly without extension.
	BoardingAddress = wallet.BoardingAddress

	// BoardingChainInfo tracks the chain related information for a given
	// boarding intent.
	BoardingChainInfo = wallet.BoardingChainInfo

	// WalletBoardingIntent is the wallet's boarding intent type.
	WalletBoardingIntent = wallet.BoardingIntent
)

// BoardingStatus type alias and constants from wallet package. The wallet
// owns the lifecycle of boarding intents; round just reacts to confirmations.
type BoardingStatus = wallet.BoardingStatus

const (
	// BoardingStatusConfirmed indicates that the boarding UTXO has reached
	// the required number of confirmations and is ready to be included in a
	// round registration.
	BoardingStatusConfirmed = wallet.BoardingStatusConfirmed

	// BoardingStatusAdopted indicates that the boarding intent has been
	// included in a round that has been frozen/finalized on disk (round
	// checkpoint saved). The intent can no longer be used in other rounds.
	BoardingStatusAdopted = wallet.BoardingStatusAdopted

	// BoardingStatusFailed indicates that the boarding process failed for
	// this intent. This could be due to validation errors, server
	// rejection, or timeout. Recovery may be possible via CSV timeout path.
	BoardingStatusFailed = wallet.BoardingStatusFailed

	// BoardingStatusExpired indicates that the boarding UTXO's CSV timeout
	// has elapsed. The client can now spend via the unilateral timeout path
	// to recover funds.
	BoardingStatusExpired = wallet.BoardingStatusExpired

	// BoardingStatusSwept indicates that the boarding UTXO has been spent
	// via the CSV timeout path to recover funds.
	BoardingStatusSwept = wallet.BoardingStatusSwept
)

// BoardingIntent captures one confirmed boarding input plus its requested VTXO
// outputs. This type embeds the wallet's BoardingIntent and adds round-specific
// fields for tracking VTXO templates and round assignment.
type BoardingIntent struct {
	// Embed wallet's BoardingIntent which contains the core boarding data:
	// Address, Outpoint, ChainInfo, and Status.
	wallet.BoardingIntent

	// BoardingRequest is the original boarding request details. It targets
	// a boarding address by outpoint and includes additional metadata.
	BoardingRequest types.BoardingRequest

	// VtxoTemplate is the template for the VTXO(s) requested as part of
	// boarding.
	//
	// TODO(roasbeef): add auxleaf for tap here.
	VtxoTemplate []types.VTXORequest

	// RoundID is the identifier of the round this intent was assigned to.
	// None until the client joins a round.
	RoundID fn.Option[RoundID]
}

// ConfInfo contains chain information about when a round's commitment
// transaction was confirmed. This is populated after the commitment tx is
// broadcast and confirmed on-chain.
type ConfInfo struct {
	// Height is the block height at which the commitment tx was confirmed.
	Height int32

	// BlockHash is the hash of the block containing the commitment tx.
	BlockHash chainhash.Hash
}

// Round represents a complete Ark round from the client's perspective. A round
// coordinates one or more actions (boarding, refresh, offboard) that are
// batched together in a single commitment transaction.
type Round struct {
	// RoundID is the unique identifier assigned by the server when the
	// client joins this round.
	RoundID RoundID

	// StartHeight is the block height when this round was created. This is
	// used as a HeightHint for confirmation registration when the round is
	// restored from disk, ensuring the chain backend scans from the correct
	// starting point.
	StartHeight uint32

	// ConfInfo contains chain information about when the round's commitment
	// transaction was confirmed. None until the commitment tx is confirmed
	// on-chain.
	ConfInfo fn.Option[ConfInfo]

	// CommitmentTx is the commitment transaction as a PSBT that anchors all
	// VTXOs. Using PSBT allows storing prevout info for all inputs. None
	// until the server constructs it.
	CommitmentTx fn.Option[*psbt.Packet]

	// VTXOTreePaths maps commitment tx output indices to VTXO tree paths.
	// Each path contains only the minimal tree needed to reach the client's
	// VTXO leaves. This is ephemeral and only kept in memory during
	// signing. After confirmation, only per-VTXO paths are persisted for
	// unilateral exit.
	VTXOTreePaths fn.Option[map[int]*tree.Tree]

	// BoardingIntents contains all boarding intents participating in this
	// round.
	BoardingIntents []BoardingIntent

	// Future extensions:
	// Refreshes []*RefreshIntent
	// Offboards []*OffboardIntent
}

// RoundStore provides persistence for round FSM state. Follows the protofsm
// StateMachineStore pattern where round data and FSM state are persisted
// atomically to enable restart recovery.
//
// Rounds coordinate multiple boarding intents (and future: refreshes,
// offboards) into a single commitment transaction.
//
// At the "point of no return" (after sending partial signatures), the
// FSM state must be checkpointed to allow recovery if the server
// broadcasts the commitment transaction.
type RoundStore interface {
	// CommitState atomically persists both the round data and FSM state.
	// This should be called at the "point of no return" when the client
	// has sent partial signatures and the server may broadcast.
	//
	// The round parameter contains the round data (commitment tx, intents,
	// tree). The state parameter is the current FSM state. Both are
	// persisted atomically so restart can recover to the exact state.
	CommitState(ctx context.Context, round *Round, state ClientState) error

	// FetchState retrieves a round and its FSM state by round ID. Returns
	// (round, state, err). Both round data and FSM state are returned
	// together to ensure consistency.
	//
	// Returns error if round doesn't exist.
	FetchState(ctx context.Context, roundID RoundID) (
		*Round, ClientState, error,
	)

	// LookupRoundByCommitmentTx finds the round associated with a
	// commitment transaction TXID. Used to route commitment tx
	// confirmations to the correct round FSM.
	LookupRoundByCommitmentTx(
		ctx context.Context, txid chainhash.Hash,
	) (*Round, error)

	// ListActiveRounds returns all rounds that are in progress (commitment
	// tx broadcast but not yet confirmed or expired).
	ListActiveRounds(ctx context.Context) ([]*Round, error)

	// FinalizeRound marks a round as complete and archives it. The ConfInfo
	// contains the block height and hash at which the commitment tx was
	// confirmed.
	FinalizeRound(
		ctx context.Context, roundID RoundID, txid chainhash.Hash,
		confInfo ConfInfo,
	) error
}

// ClientVTXO represents a Virtual UTXO owned by the client, including all
// information needed to spend it either collaboratively (with operator) or
// unilaterally (after timelock expires).
type ClientVTXO struct {
	// Outpoint identifies the VTXO's location in the virtual transaction
	// tree.
	Outpoint wire.OutPoint

	// Amount is the value of this VTXO in satoshis.
	Amount btcutil.Amount

	// PkScript is the output script for this VTXO (taproot with
	// collaborative and timeout spend paths).
	PkScript []byte

	// Expiry is the CSV delay for the unilateral exit path.
	Expiry uint32

	// ClientKey is the client's key descriptor for this VTXO.
	ClientKey keychain.KeyDescriptor

	// OperatorKey is the operator's public key for collaborative spends.
	OperatorKey *btcec.PublicKey

	// TreePath is the extracted path from the commitment transaction output
	// down to this specific VTXO. Contains only the minimal tree nodes
	// needed for unilateral exit (extracted via ExtractPathForCosigner).
	TreePath *tree.Tree

	// RoundID identifies which round created this VTXO. Empty if the VTXO
	// is being used in a context where the round is not yet known (e.g.,
	// during FSM state transitions before persistence).
	RoundID fn.Option[RoundID]
}

// VTXOStore defines the storage interface for off-chain balance management.
// VTXOs (Virtual Transaction Outputs) are created when rounds complete
// successfully and represent the client's off-chain balance.
type VTXOStore interface {
	// SaveVTXOs persists one or more VTXOs after a round confirms. Each
	// VTXO includes its extracted tree path for unilateral exit.
	SaveVTXOs(ctx context.Context, vtxos []*ClientVTXO) error

	// ListVTXOs returns all VTXOs currently owned by the client.
	ListVTXOs(ctx context.Context) ([]*ClientVTXO, error)

	// GetVTXO retrieves a specific VTXO by its outpoint. Returns an error
	// if not found.
	GetVTXO(ctx context.Context,
		outpoint wire.OutPoint) (*ClientVTXO, error)

	// MarkVTXOSpent marks a VTXO as spent (either via OOR transaction or
	// forfeit). This prevents double-spending.
	MarkVTXOSpent(ctx context.Context, outpoint wire.OutPoint) error
}

// ClientWallet defines the interface for client wallet operations used by the
// round actor. This provides MuSig2 signing capabilities needed for round
// participation. Boarding address creation is handled by the wallet actor.
//
// NOTE: input.Signer already embeds input.MuSig2Signer, so we only need to
// embed Signer here.
type ClientWallet interface {
	input.Signer
}
