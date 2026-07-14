package round

import (
	"log/slog"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// ClientEvent is a sealed interface for all events that can be processed by
// the client round interaction state machine. The sealed interface pattern
// prevents external packages from implementing this interface, ensuring type
// safety and exhaustive pattern matching in state transitions.
type ClientEvent interface {
	// clientEventSealed is an unexported method that marks this interface
	// as sealed, preventing external implementations.
	clientEventSealed()
}

// ClientOutMsg is a sealed interface for messages emitted via the FSM outbox.
// These are typically sent to the server or other actors. Separating this from
// ClientEvent improves readability by distinguishing internal FSM events from
// outgoing messages.
type ClientOutMsg interface {
	// clientOutMsgSealed is an unexported method that marks this interface
	// as sealed, preventing external implementations.
	clientOutMsgSealed()
}

// VTXORequestsReceived is an actor message carrying pre-built VTXO requests
// from other actors (e.g., VTXO actor during refresh). The round actor
// translates it into an IntentPackage before sending to the FSM.
//
// NOTE: This is NOT an FSM event — it does not implement clientEventSealed().
// The actor converts it to IntentPackage{VTXOs: req.Requests}.
type VTXORequestsReceived struct {
	actor.BaseMessage

	// Requests are the VTXO requests to include in the next join round
	// request.
	Requests []types.VTXORequest
}

// RoundReceivable implements actormsg.RoundReceivable marker interface.
func (e *VTXORequestsReceived) RoundReceivable() {}

// MessageType returns the message type for logging.
func (e *VTXORequestsReceived) MessageType() string {
	return "VTXORequestsReceived"
}

// IntentRequested is emitted when the FSM is ready to join a round with
// the currently confirmed set of boarding intents. The actor should treat this
// as a batch request containing every confirmed intent.
type IntentRequested struct{}

func (e *IntentRequested) clientEventSealed() {}

// JoinRoundQuoteReceived is emitted when a JoinRoundQuote arrives
// over the mailbox egress. Carries the server's binding per-output
// amounts, the operator fee, and the quote_id that must be echoed
// on accept / reject. Transitions the FSM from IntentSentState to
// QuoteReceivedState so the client can evaluate the fee against
// MaxOperatorFee and decide whether to accept.
type JoinRoundQuoteReceived struct {
	// RoundID is the round the quote belongs to.
	RoundID RoundID

	// Quote is the server-issued quote payload.
	Quote *ClientQuote
}

func (e *JoinRoundQuoteReceived) clientEventSealed() {}

// QuoteAccepted is an internal event fired by QuoteReceivedState
// after the fee-cap check passes. It drives the transition from
// QuoteReceivedState to RoundJoinedState with the emitted
// JoinRoundAcceptOutbox attached to the outbox.
type QuoteAccepted struct {
	// RoundID is the round the quote belongs to.
	RoundID RoundID

	// QuoteID echoes the server's quote_id verbatim.
	QuoteID [32]byte
}

func (e *QuoteAccepted) clientEventSealed() {}

// QuoteRejected is an internal event fired by QuoteReceivedState
// when the fee cap is exceeded or the server's reject_reason on
// the quote is non-OK. Drives the transition to ClientFailedState
// with JoinRoundRejectOutbox emitted on the way out.
type QuoteRejected struct {
	// RoundID is the round the quote belongs to.
	RoundID RoundID

	// QuoteID echoes the server's quote_id verbatim.
	QuoteID [32]byte

	// Reason is a human-readable classifier logged on the
	// client side and echoed to the server's reject_reason
	// field for operator observability.
	Reason string
}

func (e *QuoteRejected) clientEventSealed() {}

// RoundJoined is emitted when the server accepts the client's registration
// and assigns them to a round. This event arrives via the Outbox from the
// server FSM. The accepted outpoints are used to correlate this response to
// the correct pending round when multiple rounds are in-flight concurrently.
type RoundJoined struct {
	// RoundID is the unique identifier for the round.
	RoundID RoundID

	// AcceptedBoardingOutpoints contains the boarding outpoints that were
	// accepted into this round. Used to correlate the response to the
	// correct pending round when multiple boarding rounds are in-flight.
	AcceptedBoardingOutpoints []wire.OutPoint

	// AcceptedVTXOOutpoints contains the VTXO outpoints involved in this
	// round. Used for future operations like forfeit, leave, and refresh
	// that affect VTXOs but may not involve boarding inputs.
	AcceptedVTXOOutpoints []wire.OutPoint
}

func (e *RoundJoined) clientEventSealed() {}

// CommitmentTxBuilt is emitted when the server sends the commitment
// transaction and VTXT paths to the client. This event arrives via the Outbox
// from the server FSM after building the transaction that commits all boarding
// UTXOs.
type CommitmentTxBuilt struct {
	// RoundID identifies which round this commitment transaction belongs
	// to.
	RoundID RoundID

	// Tx is the unsigned commitment transaction as a PSBT. Using PSBT
	// allows the server to include WitnessUtxo for all inputs, which is
	// required for correct Taproot sighash computation (BIP341).
	Tx *psbt.Packet

	// VTXOTreePaths maps commitment transaction output indices to the
	// client's extracted sub-tree from the virtual transaction tree. The
	// server sends only the minimal paths containing transactions needed to
	// reach this client's VTXO leaves, not the full tree (which may contain
	// hundreds of transactions for all participants). Each sub-tree is
	// sufficient for the client to verify their VTXOs and perform
	// unilateral exit if needed.
	VTXOTreePaths map[int]*tree.Tree

	// ForfeitMappings maps each VTXO outpoint to its connector leaf info.
	// This allows VTXO actors to find their connector output and construct
	// the forfeit transaction. Only set when refresh requests are present.
	ForfeitMappings map[wire.OutPoint]*ConnectorLeafInfo

	// TreeOpts holds functional options for VTXO tree deserialization.
	// These are injected by the event router from daemon configuration
	// (e.g., MaxTreeNodes limit) and passed through to
	// roundpb.TreeFromProto during FromProto.
	TreeOpts []roundpb.TreeFromProtoOption

	// TreeCosignKey is the operator's MuSig2 cosigner key for this round's
	// VTXO output tree, derived fresh per round by the server. The client
	// aggregates and validates the tree against this key instead of the
	// operator's global GetInfo key, so an operator key rotation does not
	// change the tree key the client must agree on. Nil when talking to a
	// server that predates this field; callers fall back to the global
	// operator key.
	TreeCosignKey *btcec.PublicKey

	// ConnectorOperatorKey is the operator key this round used to build its
	// connector tree. The client reconstructs the connector tree from it
	// instead of the global operator key. Nil when talking to an older
	// server; callers fall back to the global operator key.
	ConnectorOperatorKey *btcec.PublicKey

	// SweepKey is the operator sweep key for this round's VTXO-tree sweep
	// leaf, delivered per round so the client agrees with the server on the
	// sweep branch even across an operator key rotation. It replaces the
	// global GetInfo sweep key entirely; a well-formed round always carries
	// it.
	SweepKey *btcec.PublicKey

	// SweepDelay is this round's batch-wide absolute-timelock in blocks for
	// the VTXO-tree sweep leaf, delivered per round alongside SweepKey. It
	// replaces the global GetInfo sweep delay and drives batch-expiry
	// computation for VTXOs created in this round.
	SweepDelay uint32

	// ForfeitKey is the operator's dedicated forfeit penalty key for this
	// round, delivered per round so the client builds the forfeit-tx
	// penalty output (a BIP-86 key-spend to this key) the server expects
	// even across an operator key rotation. It replaces the global GetInfo
	// forfeit script entirely; a well-formed round always carries it.
	ForfeitKey *btcec.PublicKey

	// FlowVersion is the per-round flow version the operator stamped on
	// this round. The client records it so both sides agree on the
	// choreography rules the round was conducted under. Validated on
	// receipt; today the only understood value is FlowVersionV1.
	FlowVersion roundpb.FlowVersion
}

func (e *CommitmentTxBuilt) clientEventSealed() {}

// ConnectorLeafInfo contains the information needed to construct a forfeit
// transaction for a specific VTXO. The forfeit tx spends the VTXO and its
// connector output, paying the VTXO value to the operator's forfeit address.
type ConnectorLeafInfo struct {
	// LeafIndex is the position of this connector in the connector tree.
	// Populated by FromProto from the server's ConnectorLeafInfo proto and
	// also set by local tree-building code. Zero is a valid index.
	LeafIndex int

	// ConnectorOutpoint is the outpoint of the connector output in the
	// commitment transaction that this forfeit tx must spend.
	ConnectorOutpoint wire.OutPoint

	// ConnectorPkScript is the scriptPubKey of the connector output.
	ConnectorPkScript []byte

	// ConnectorAmount is the value of the connector output in satoshis.
	// Connectors typically have minimal value (dust limit).
	ConnectorAmount int64

	// VTXOAmount is the value of the VTXO being forfeited. This field is
	// combined with ConnectorAmount when validating that the zero-fee
	// forfeit tx pays the full input value to the penalty output.
	VTXOAmount btcutil.Amount

	// RootOutputIndex is the commitment-tx output index that the connector
	// tree's root transaction spends. It binds this connector leaf's tree
	// to the commitment tx the client is about to sign into.
	RootOutputIndex uint32

	// NumLeaves is the total number of connector leaves in the tree this
	// leaf belongs to. Together with Radix and the operator key it lets the
	// client deterministically reconstruct the connector tree and prove the
	// assigned leaf descends from the commitment tx (wavelength#681).
	NumLeaves uint32

	// Radix is the branching factor used to build the connector tree. It is
	// not derivable from the commitment tx, so the operator supplies it.
	Radix uint32
}

// CommitmentTxValidated is emitted after the client successfully validates
// the commitment transaction and VTXT path. This is a critical security
// checkpoint - the client must verify:
//  1. Boarding UTXO is an input to commitment tx.
//  2. VTXT path is valid and leads to expected VTXOs.
//  3. VTXO amounts and scripts match requests.
type CommitmentTxValidated struct {
	// VTXTTree is the validated tree.
	VTXTTree *tree.Tree
}

func (e *CommitmentTxValidated) clientEventSealed() {}

// GenerateNonces is emitted when the client has validated the vtxt tree, and
// nonces should be generated for all the client trees.
type GenerateNonces struct {
}

func (e *GenerateNonces) clientEventSealed() {}

// NoncesAggregated is emitted when the server sends back the aggregated
// nonces from all participants. This event arrives via the Outbox from the
// server FSM. The server computes aggregated nonces from all participants and
// sends them back to clients, who use them to generate partial signatures in
// the next phase of the MuSig2 signing protocol.
type NoncesAggregated struct {
	// RoundID identifies which round these aggregated nonces belong to.
	RoundID RoundID

	// AggNonces maps transaction IDs to their aggregated MuSig2 public
	// nonces. Each entry corresponds to a transaction in the VTXT that
	// requires signing.
	AggNonces map[tree.TxID]tree.Musig2PubNonce
}

func (e *NoncesAggregated) clientEventSealed() {}

// GeneratePartialSigs is emitted when the client has generated partial
// signatures for all transactions in their VTXT path using the aggregated
// nonces.
type GeneratePartialSigs struct {
	// PartialSigs maps transaction IDs to their MuSig2 partial signatures.
	// Each entry corresponds to a transaction in the client's VTXT path.
	PartialSigs map[chainhash.Hash][]byte

	// SigningKey is the key used for signing.
	SigningKey *btcec.PublicKey
}

func (e *GeneratePartialSigs) clientEventSealed() {}

// OperatorSigned is emitted when the server sends the complete VTXT
// signatures after aggregating all partial signatures. This event arrives via
// the Outbox from the server FSM. After collecting and validating all partial
// signatures from participants, the operator produces complete Schnorr
// signatures for each transaction in the VTXT.
type OperatorSigned struct {
	// RoundID identifies which round these signatures belong to.
	RoundID RoundID

	// AggSigs maps transaction IDs to their complete aggregated Schnorr
	// signatures. Each entry corresponds to a transaction in the VTXT.
	AggSigs map[tree.TxID]*schnorr.Signature
}

func (e *OperatorSigned) clientEventSealed() {}

// AwaitingBoardingSigs is emitted when the server signals it is ready to
// receive boarding signatures from the client. This occurs after VTXO nonce
// aggregation and partial signature collection phases complete.
type AwaitingBoardingSigs struct {
	// RoundID identifies which round is awaiting boarding signatures.
	RoundID RoundID
}

func (e *AwaitingBoardingSigs) clientEventSealed() {}

// BoardingConfirmed is emitted when the commitment transaction has been
// confirmed on-chain with sufficient confirmations.
type BoardingConfirmed struct {
	// TxID is the confirmed commitment transaction ID.
	TxID chainhash.Hash

	// BlockHeight is the height at which the transaction was confirmed.
	BlockHeight int32

	// BlockHash is the hash of the block containing the commitment tx.
	BlockHash chainhash.Hash

	// Confirmations is the number of confirmations.
	Confirmations int32
}

func (e *BoardingConfirmed) clientEventSealed() {}

// RoundFailureCode classifies, in client-domain terms, why a round the client
// had joined failed. It is mapped from the wire roundpb.RoundFailureCode only
// at the RPC boundary (BoardingFailed.FromProto), so the FSM and its consumers
// never traffic in proto types.
type RoundFailureCode uint8

const (
	// RoundFailureUnknown is the default, unclassified failure. The client
	// treats it as a generic recoverable failure and relies on Reason. An
	// unrecognized wire code also maps here.
	RoundFailureUnknown RoundFailureCode = iota

	// RoundFailureInsufficientOperatorFunds indicates the operator could
	// not fund the round's commitment transaction from its own on-chain
	// wallet. The client's inputs were never committed and its
	// forfeit-reserved inputs are released back to the live set, so the
	// originating job (e.g. a cooperative on-chain send) must be failed
	// terminally rather than auto-retried into the same wall.
	RoundFailureInsufficientOperatorFunds
)

// IsTerminalForJob reports whether a failure code should terminally fail the
// job that triggered the round (dropping its persisted intent and marking its
// activity entry failed) rather than leaving it to recoverable replay.
func (c RoundFailureCode) IsTerminalForJob() bool {
	switch c {
	case RoundFailureInsufficientOperatorFunds:
		return true

	default:
		return false
	}
}

// BoardingFailed is emitted when an error occurs during the boarding
// process.
type BoardingFailed struct {
	// RoundID is the server-assigned round id when the failure carries
	// one (ClientRoundFailedResp). It is None for failures that arrive
	// before a round was assigned (e.g. ClientErrorResp). When set, the
	// actor routes the failure deterministically to the matching FSM
	// instead of relying on the sole-round heuristic.
	RoundID fn.Option[RoundID]

	// Reason is a human-readable description of the failure.
	Reason string

	// Error is the underlying error.
	Error error

	// Recoverable indicates if the client can retry or if CSV recovery
	// is needed.
	Recoverable bool

	// FailureCode is the server's typed classification of the failure. It
	// defaults to RoundFailureUnknown for older servers and unclassified
	// failures. Terminal-for-job codes drive the originating job to a
	// terminal failure instead of recoverable replay.
	FailureCode RoundFailureCode
}

func (e *BoardingFailed) clientEventSealed() {}

// RecoveryInitiated is emitted when the client initiates CSV timeout
// recovery to sweep their boarding UTXO back to their wallet.
type RecoveryInitiated struct {
	// Outpoint identifies the boarding UTXO being recovered.
	Outpoint wire.OutPoint

	// SweepTxID is the transaction ID of the sweep transaction.
	SweepTxID chainhash.Hash

	// Reason explains why recovery was initiated.
	Reason string
}

func (e *RecoveryInitiated) clientEventSealed() {}

// RoundComplete is an internal event emitted after a boarding round completes
// successfully. This triggers the FSM to transition back to Idle state to
// process new boarding addresses and intents.
type RoundComplete struct{}

func (e *RoundComplete) clientEventSealed() {}

// IntentPackage is the single FSM event for all pool additions. The actor
// layer converts raw inputs (boarding confirmations, VTXO requests, refresh
// requests, leave requests) into processed intents and sends them to the FSM
// via this unified event. The FSM unpacks the package and appends each item
// to its respective pool.
//
// IntentPackage embeds Intents so that the pool fields (Boarding, VTXOs,
// Forfeits, Leaves) are defined once and shared between event transport
// and state storage.
//
// Examples:
//   - Boarding:  {Boarding: [1], VTXOs: [1]}
//   - Refresh:   {Forfeits: [1], VTXOs: [1]}
//   - Leave:     {Forfeits: [1], Leaves: [1]}
//   - Consolidate N-to-1: {Forfeits: [N], VTXOs: [1]}
//   - Resume:    {Boarding: [N], VTXOs: [M], Forfeits: [K], Leaves: [L]}
type IntentPackage struct {
	Intents
}

// isEmpty returns true if the package contains no intents.
func (e *IntentPackage) isEmpty() bool {
	return len(e.Boarding) == 0 && len(e.Forfeits) == 0 &&
		len(e.VTXOs) == 0 && len(e.Leaves) == 0
}

// logAttributes returns structured logging arguments for the package.
func (e *IntentPackage) logAttributes() []any {
	return []any{
		slog.Int("boarding_intents", len(e.Boarding)),
		slog.Int("vtxo_requests", len(e.VTXOs)),
		slog.Int("forfeits", len(e.Forfeits)),
		slog.Int("leaves", len(e.Leaves)),
	}
}

// clientEventSealed prevents external implementations.
func (e *IntentPackage) clientEventSealed() {}

// ForfeitSignatureResponse is sent from a VTXO actor with its signature for
// the forfeit transaction. This is the response to a forfeit request during
// a batch swap round.
type ForfeitSignatureResponse struct {
	actor.BaseMessage

	// VTXOOutpoint identifies the VTXO being forfeited.
	VTXOOutpoint wire.OutPoint

	// RoundID is the round where the forfeit is being processed.
	RoundID string

	// ForfeitTx is the built forfeit transaction.
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

func (e *ForfeitSignatureResponse) clientEventSealed() {}

// RoundReceivable implements actormsg.RoundReceivable marker interface.
func (e *ForfeitSignatureResponse) RoundReceivable() {}

// MessageType returns the message type for logging.
func (e *ForfeitSignatureResponse) MessageType() string {
	return "ForfeitSignatureResponse"
}

// ForfeitCollectionTimedOut is emitted by the round actor when the forfeit
// signature collection window expires before all expected responses arrive.
// This transitions the round into a failed state to avoid indefinite stalls.
type ForfeitCollectionTimedOut struct {
	// RoundID identifies the round whose forfeit collection timed out.
	RoundID RoundID
}

func (e *ForfeitCollectionTimedOut) clientEventSealed() {}

// RegistrationTimedOut is emitted by the round actor when the registration
// (admission) window expires while the FSM is still parked in IntentSentState,
// i.e. the server never returned a RoundJoined admission watermark for the
// JoinRoundRequest. The FSM consumes this in IntentSentState to fail the round
// (recoverable) and release any forfeit-reserved inputs back to LiveState so
// they are not stranded in pending-forfeit (wavelength#653). The timeout is
// armed when the FSM is still temp-keyed, so the event carries no RoundID.
type RegistrationTimedOut struct{}

func (e *RegistrationTimedOut) clientEventSealed() {}
