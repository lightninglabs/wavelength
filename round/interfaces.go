package round

import (
	"context"
	"encoding/hex"
	"fmt"
	"slices"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	"github.com/lightninglabs/wavelength/wallet"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// ClientStateTransition is a type alias for the verbose
// protofsm.StateTransition type used throughout the client round FSM. The
// baselib protofsm uses 3 type parameters: InternalEvent, OutboxEvent, and Env.
// In our case:
//   - InternalEvent = ClientEvent (events that drive the FSM).
//   - OutboxEvent = ClientOutMsg (outbox messages emitted by transitions).
//   - Env = *ClientEnvironment.
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

// roundIDLen is the wire byte length of a RoundID (a UUID). Derived from
// the type so it tracks RoundID automatically. Used to validate raw
// round_id byte slices decoded off the wire.
const roundIDLen = len(RoundID{})

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

// RoundKey is an interface for identifying rounds in the actor's map. It can
// be either a TempRoundKey (client-generated before server assigns ID) or a
// RoundID (server-assigned). This enables concurrent rounds to be tracked
// before they receive their official RoundIDs.
type RoundKey interface {
	// KeyString returns a unique string representation for map keying.
	KeyString() string

	// IsTemp returns true if this is a temporary client-generated key.
	IsTemp() bool
}

// TempRoundKey is a client-generated temporary identifier used for rounds
// before the server assigns a RoundID. It uses UUIDv7 for time-ordering and
// uniqueness.
type TempRoundKey uuid.UUID

// NewTempRoundKey generates a new temporary round key.
func NewTempRoundKey() (TempRoundKey, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return TempRoundKey{}, err
	}

	return TempRoundKey(id), nil
}

// KeyString implements RoundKey.
func (k TempRoundKey) KeyString() string {
	return "temp:" + uuid.UUID(k).String()
}

// IsTemp implements RoundKey.
func (k TempRoundKey) IsTemp() bool {
	return true
}

// String returns the string representation.
func (k TempRoundKey) String() string {
	return uuid.UUID(k).String()
}

// LogPrefix returns a short string for logging.
func (k TempRoundKey) LogPrefix() string {
	return fmt.Sprintf("temp(%v)", hex.EncodeToString(k[12:16]))
}

// Ensure RoundID implements RoundKey.
var _ RoundKey = RoundID{}

// RoundKeyStr is a type alias for the string representation of a RoundKey.
// Used as the key type in maps to avoid using raw strings, providing better
// type safety and documentation.
type RoundKeyStr string

// KeyString implements RoundKey for RoundID.
func (id RoundID) KeyString() string {
	return uuid.UUID(id).String()
}

// IsTemp implements RoundKey for RoundID.
func (id RoundID) IsTemp() bool {
	return false
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

// VTXOIntent is a client's request for a VTXO output in a round. It
// carries the output metadata but no signing key — the FSM derives
// signing keys at registration time. This is the type used by callers
// (wallet, vtxo actor) to request VTXOs.
type VTXOIntent struct {
	// Amount is the amount of satoshis to lock in the VTXO.
	Amount btcutil.Amount

	// PkScript is the output script of the VTXO.
	// PolicyTemplate is the semantic arkscript policy for this VTXO.
	PolicyTemplate []byte

	PkScript []byte

	// Expiry is the CSV delay for the unilateral exit path.
	Expiry uint32

	// OwnerKey is the owner's key descriptor for the VTXO.
	OwnerKey keychain.KeyDescriptor

	// OperatorKey is the operator's public key for collaborative
	// spends.
	OperatorKey *btcec.PublicKey
}

// RoundVTXORequest pairs a VTXOIntent with the ephemeral signing key
// derived for this round's MuSig2 tree construction.
type RoundVTXORequest struct {
	VTXOIntent

	// SigningKey is the MuSig2 tree signing key derived for this
	// round. It is ephemeral, must not be reused across rounds,
	// and can be discarded after batch confirmation.
	SigningKey keychain.KeyDescriptor
}

// ToVTXORequest converts a RoundVTXORequest to a types.VTXORequest
// for wire encoding and DB persistence.
func (r RoundVTXORequest) ToVTXORequest() types.VTXORequest {
	return types.VTXORequest{
		Amount:      r.Amount,
		PkScript:    r.PkScript,
		Expiry:      r.Expiry,
		ClientKey:   r.OwnerKey.PubKey,
		OwnerKey:    r.OwnerKey,
		OperatorKey: r.OperatorKey,
		SigningKey:  r.SigningKey,
	}
}

// Intents holds the accumulated round participation data after signing
// keys have been derived. All VTXO requests carry their signing keys.
type Intents struct {
	// Boarding contains all boarding intents to include in the round.
	Boarding []BoardingIntent

	// VTXOs is the VTXO requests for the round.
	VTXOs []types.VTXORequest

	// Leaves contains the leave requests for VTXOs being exited to on-chain
	// outputs. Each leave forfeits a VTXO and creates an on-chain output
	// in the batch transaction instead of a new VTXO.
	Leaves []*types.LeaveRequest

	// Forfeits contains the VTXOs being forfeited as inputs. Each entry
	// carries the outpoint and an optional embedded Amount hint. The
	// canonical amount is always looked up from VTXOStore at registration
	// time; the embedded Amount is only used as a fallback when no store
	// is available (e.g., test environments).
	Forfeits []types.ForfeitRequest

	// QuotedLeaveAmounts optionally carries the server-authoritative
	// leave output amounts (positional, matching Leaves) captured at
	// QuoteAccepted time. Downstream accounting (fee computation,
	// VTXOCreatedNotification emission) uses these over the
	// pre-fee Leaves[i].Output.Value when present. Nil when no seal-
	// time quote was accepted (pre-#270 harness paths).
	QuotedLeaveAmounts []int64
}

// Clone creates a copy of the Intents.
func (i *Intents) Clone() Intents {
	return Intents{
		Boarding:           slices.Clone(i.Boarding),
		VTXOs:              slices.Clone(i.VTXOs),
		Leaves:             slices.Clone(i.Leaves),
		Forfeits:           slices.Clone(i.Forfeits),
		QuotedLeaveAmounts: slices.Clone(i.QuotedLeaveAmounts),
	}
}

// LeaveAmount returns the authoritative output value (sats) for
// the leave at position i. When the quote-time override is present
// it takes precedence; otherwise the intent target is returned so
// harness paths that bypass the seal-time handshake keep working.
func (i *Intents) LeaveAmount(idx int) int64 {
	if idx < len(i.QuotedLeaveAmounts) {
		return i.QuotedLeaveAmounts[idx]
	}
	if idx >= len(i.Leaves) {
		return 0
	}
	leave := i.Leaves[idx]
	if leave == nil || leave.Output == nil {
		return 0
	}

	return leave.Output.Value
}

// BoardingIntent captures one confirmed boarding input plus its requested VTXO
// outputs. This type embeds the wallet's BoardingIntent and adds round-specific
// fields for tracking VTXO templates and round assignment.
type BoardingIntent struct {
	// Embed wallet's BoardingIntent which contains the core boarding data:
	// Address, Outpoint, ChainInfo, and Status.
	wallet.BoardingIntent

	// Request is the original boarding request details. It targets
	// a boarding address by outpoint and includes additional metadata.
	Request types.BoardingRequest
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

	// Intents contains all intents participating in this round.
	Intents Intents

	// FlowVersion is the per-round flow version describing the choreography
	// rules the round was conducted under. The operator's value is
	// validated on receipt in CommitmentTxBuilt.FromProto (an unknown
	// version fails the round closed before it is joined), threaded from
	// CommitmentTxBuilt.FlowVersion through the FSM state onto this field
	// at checkpoint, and stamped/read back verbatim by the db (validation
	// lives at the ingress edge, not the store). Versions are zero-indexed,
	// so V1 is the Go zero value: an unstamped round reads as V1 with no
	// separate unset sentinel.
	FlowVersion roundpb.FlowVersion

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
	FetchState(ctx context.Context, roundID RoundID) (*Round, ClientState,
		error)

	// LookupRoundByCommitmentTx finds the round associated with a
	// commitment transaction TXID. Used to route commitment tx
	// confirmations to the correct round FSM.
	LookupRoundByCommitmentTx(ctx context.Context,
		txid chainhash.Hash) (*Round, error)

	// ListActiveRounds returns all rounds that are in progress (commitment
	// tx broadcast but not yet confirmed or expired).
	ListActiveRounds(ctx context.Context) ([]*Round, error)

	// FinalizeRound marks a round as complete and archives it. The ConfInfo
	// contains the block height and hash at which the commitment tx was
	// confirmed.
	FinalizeRound(ctx context.Context, roundID RoundID, txid chainhash.Hash,
		confInfo ConfInfo) error

	// FailForfeitIntents terminally fails the pending send intents anchored
	// to the given forfeited VTXO outpoints, recording the reason and typed
	// failure code. It is the terminal-failure counterpart to the anchor
	// clear CommitState performs on the success path: when a round fails
	// terminally (e.g. the operator cannot fund the commitment tx), the
	// originating job is marked failed so restart replay does not re-submit
	// the same forfeit inputs into the same wall, and the activity entry
	// surfaces as failed rather than stuck pending. It is a no-op for an
	// empty outpoint set.
	FailForfeitIntents(ctx context.Context, outpoints []wire.OutPoint,
		reason string, code RoundFailureCode) error
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
	// PolicyTemplate is the semantic arkscript policy for this VTXO.
	PolicyTemplate []byte

	PkScript []byte

	// Expiry is the CSV delay for the unilateral exit path.
	Expiry uint32

	// OwnerKey is the client's owner key descriptor for this VTXO.
	OwnerKey keychain.KeyDescriptor

	// OperatorKey is the operator's public key for collaborative spends.
	OperatorKey *btcec.PublicKey

	// Ancestry is the set of rooted commitment-tree fragments required
	// to claim this VTXO unilaterally on-chain. Round-direct VTXOs
	// have len(Ancestry) == 1; cross-round multi-input OOR VTXOs that
	// flow through here (rare but supported by the persistence layer)
	// can carry one entry per distinct contributing commitment tx.
	// Each entry carries its own TreePath, CommitmentTxID, InputIndices,
	// and TreeDepth.
	Ancestry []types.Ancestry

	// RoundID identifies which round created this VTXO. Empty if the VTXO
	// is being used in a context where the round is not yet known (e.g.,
	// during FSM state transitions before persistence).
	RoundID fn.Option[RoundID]

	// Origin is the classification the wallet stamped on the
	// source VTXORequest at intent-composition time
	// (boarding / refresh / in-round transfer). Used by
	// emitVTXOsReceived to route the ledger VTXOReceivedMsg to
	// the correct Source so ledger account movements match the
	// real funding flow. Unknown means "do not emit a ledger
	// event"; downstream safety net against a composition path
	// that forgot to tag.
	Origin types.VTXOOrigin

	// CommitmentTxID is the txid of the round's commitment
	// transaction. Populated at confirmation time.
	CommitmentTxID chainhash.Hash

	// BatchExpiry is the absolute block height at which the
	// round's sweep window expires. Populated at confirmation
	// time from BlockHeight + SweepDelay.
	BatchExpiry int32

	// CreatedHeight is the block height at which the commitment
	// transaction was confirmed. Populated at confirmation time.
	CreatedHeight int32
}

// OwnedScriptChecker determines whether a pkScript belongs to the local
// wallet. Implementations typically check against the persisted set of
// registered receive scripts (the same store used for OOR receives).
type OwnedScriptChecker interface {
	// IsOwnedScript returns whether the pkScript is registered as
	// an owned receive script in the local wallet. Returns an
	// error if the store lookup fails for reasons other than the
	// script not being found.
	IsOwnedScript(ctx context.Context, pkScript []byte) fn.Result[bool]
}

// OwnedScriptRegistrar registers a pkScript as locally owned. The
// round actor calls this when building VTXO intents (boarding,
// refresh, change) so the OwnedScriptChecker can recognize them
// when the round confirms.
type OwnedScriptRegistrar interface {
	// RegisterOwnedScript persists a pkScript + owner key as
	// locally owned.
	RegisterOwnedScript(ctx context.Context, pkScript []byte,
		ownerKey keychain.KeyDescriptor) error
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

	// DeriveNextKey derives the next key in the specified key family for
	// use as a VTXO signing key.
	DeriveNextKey(ctx context.Context,
		family keychain.KeyFamily) (*keychain.KeyDescriptor, error)
}
