package round

import (
	"log/slog"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightninglabs/taproot-assets/proof"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
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

// ResumeBoardingIntents is emitted to instruct the FSM to resume attempting to
// join a round with previously submitted and confirmed-but-not-adopted
// intents.
type ResumeBoardingIntents struct {
	// Boarding contains the collected boarding intents to include in the
	// next round.
	Boarding []BoardingIntent

	// VTXOs contains the collected VTXO requests to include in the next
	// round.
	VTXOs []types.VTXORequest
}

// isEmpty returns true if there are no boarding intents or VTXO requests
// to resume.
func (e *ResumeBoardingIntents) isEmpty() bool {
	return len(e.Boarding) == 0 && len(e.VTXOs) == 0
}

// logAttributes returns a map of attributes for logging purposes.
func (e *ResumeBoardingIntents) logAttributes() []slog.Attr {
	return []slog.Attr{
		slog.Int("boarding_intents", len(e.Boarding)),
		slog.Int("vtxo_requests", len(e.VTXOs)),
	}
}

func (e *ResumeBoardingIntents) clientEventSealed() {}

// BoardingUTXOConfirmed is emitted when the boarding UTXO has received
// sufficient confirmations and is ready to be used for boarding.
type BoardingUTXOConfirmed struct {
	// Outpoint identifies the confirmed boarding UTXO.
	Outpoint wire.OutPoint

	// Address is the boarding address for this UTXO. Contains the keys,
	// tapscript, and exit delay needed to build the Request.
	Address wallet.BoardingAddress

	// BlockHeight is the height at which the UTXO was confirmed.
	BlockHeight int32

	// BlockHash is the hash of the block containing the transaction.
	BlockHash chainhash.Hash

	// Confirmations is the number of confirmations the UTXO has.
	Confirmations int32

	// Tx is the confirmed transaction containing the boarding UTXO. This
	// allows the FSM to extract output details without additional chain
	// queries.
	Tx *wire.MsgTx

	// TxProof is the optional SPV proof for this boarding UTXO. Includes
	// merkle proof, block header, and output construction details. None if
	// the proof hasn't been constructed yet.
	TxProof fn.Option[proof.TxProof]
}

func (e *BoardingUTXOConfirmed) clientEventSealed() {}

// VTXORequestsReceived is emitted when the client submits VTXO requests that
// should be included in the next round registration. This event can be sent
// from both internal sources (e.g., wallet) and external actors (e.g., VTXO
// actor requesting a new VTXO during refresh).
type VTXORequestsReceived struct {
	actor.BaseMessage

	// Requests are the VTXO requests to include in the next join round
	// request.
	Requests []types.VTXORequest
}

// clientEventSealed prevents external implementations.
func (e *VTXORequestsReceived) clientEventSealed() {}

// RoundReceivable implements actormsg.RoundReceivable marker interface.
func (e *VTXORequestsReceived) RoundReceivable() {}

// MessageType returns the message type for logging.
func (e *VTXORequestsReceived) MessageType() string {
	return "VTXORequestsReceived"
}

// RegistrationRequested is emitted when the FSM is ready to join a round with
// the currently confirmed set of boarding intents. The actor should treat this
// as a batch request containing every confirmed intent.
type RegistrationRequested struct{}

func (e *RegistrationRequested) clientEventSealed() {}

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
}

func (e *CommitmentTxBuilt) clientEventSealed() {}

// ConnectorLeafInfo contains the information needed to construct a forfeit
// transaction for a specific VTXO. The forfeit tx spends the VTXO and its
// connector output, paying the VTXO value to the operator's forfeit address.
type ConnectorLeafInfo struct {
	// LeafIndex is the position of this connector in the connector tree.
	LeafIndex int

	// ConnectorOutpoint is the outpoint of the connector output in the
	// commitment transaction that this forfeit tx must spend.
	ConnectorOutpoint wire.OutPoint

	// ConnectorPkScript is the scriptPubKey of the connector output.
	ConnectorPkScript []byte

	// ConnectorAmount is the value of the connector output in satoshis.
	// Connectors typically have minimal value (dust limit).
	ConnectorAmount int64

	// VTXOAmount is the value of the VTXO being forfeited. The forfeit tx's
	// penalty output must equal this amount. This field enables validation
	// that prevents value theft by ensuring the correct amount is forfeited.
	VTXOAmount btcutil.Amount
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

// BoardingFailed is emitted when an error occurs during the boarding
// process.
type BoardingFailed struct {
	// Reason is a human-readable description of the failure.
	Reason string

	// Error is the underlying error.
	Error error

	// Recoverable indicates if the client can retry or if CSV recovery
	// is needed.
	Recoverable bool
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

// RefreshVTXORequest is sent from a VTXO actor when its VTXO is approaching
// expiry and needs to be refreshed in a new round. The round actor should
// queue this VTXO for inclusion in the next batch swap.
//
// This request contains all information needed to build both the server's
// RefreshRequest (for the connector tree) and a VTXORequest (for the new VTXO
// in the VTXT). The same client key is typically reused for the new VTXO.
type RefreshVTXORequest struct {
	actor.BaseMessage

	// VTXOOutpoint identifies the VTXO to refresh.
	VTXOOutpoint wire.OutPoint

	// Amount is the VTXO value in satoshis.
	Amount int64

	// NewVTXOKey is the client's public key for the new VTXO. This is
	// typically the same as the old VTXO's key but could be fresh.
	NewVTXOKey *btcec.PublicKey

	// PkScript is the output script for the new VTXO.
	PkScript []byte

	// OperatorKey is the operator's public key for the new VTXO.
	OperatorKey *btcec.PublicKey

	// Expiry is the CSV delay for the new VTXO's unilateral exit path.
	Expiry uint32

	// SigningKey is the key descriptor for signing the new VTXO's tree.
	SigningKey keychain.KeyDescriptor
}

func (e *RefreshVTXORequest) clientEventSealed() {}

// RoundReceivable implements actormsg.RoundReceivable marker interface.
func (e *RefreshVTXORequest) RoundReceivable() {}

// MessageType returns the message type for logging.
func (e *RefreshVTXORequest) MessageType() string {
	return "RefreshVTXORequest"
}

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

	// Signature is the client's signature for the forfeit tx.
	Signature []byte
}

func (e *ForfeitSignatureResponse) clientEventSealed() {}

// RoundReceivable implements actormsg.RoundReceivable marker interface.
func (e *ForfeitSignatureResponse) RoundReceivable() {}

// MessageType returns the message type for logging.
func (e *ForfeitSignatureResponse) MessageType() string {
	return "ForfeitSignatureResponse"
}
