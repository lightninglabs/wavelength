package round

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
)

// ClientState is a sealed interface for all states in the client round
// interaction state machine. Each state implements ProcessEvent to handle
// events and transition to the next state. This FSM handles the client's
// participation in Ark rounds, including boarding, refresh, and offboard
// operations.
//
// The baselib protofsm.State interface has 3 type parameters:
//   - InternalEvent = ClientEvent.
//   - OutboxEvent = ClientOutMsg.
//   - Env = *ClientEnvironment.
type ClientState interface {
	protofsm.State[ClientEvent, ClientOutMsg, *ClientEnvironment]

	// clientStateSealed is an unexported method that marks this interface
	// as sealed, preventing external implementations.
	clientStateSealed()
}

// Idle is the initial state. No active boarding process is running.
type Idle struct{}

func (s *Idle) String() string {
	return "Idle"
}

func (s *Idle) IsTerminal() bool {
	return false
}

func (s *Idle) clientStateSealed() {}

// PendingRoundAssembly tracks all active boarding intents that have been
// funded on-chain but not yet fully confirmed. Intents are keyed by their
// on-chain outpoint for efficient lookup when confirmation events arrive. Once
// all intents reach the required confirmations, the FSM transitions to round
// registration.
//
// This state tracks four independent pools: boarding inputs, forfeit inputs,
// VTXO outputs, and leave outputs. The pools are validated at registration
// time by checking sum(inputs) >= sum(outputs) + fees.
type PendingRoundAssembly struct {
	// RequestedRoundID binds claim-only registration to the exact open
	// operator round returned by claim preflight. It is empty for every
	// ordinary intent path.
	RequestedRoundID string

	// Boarding contains the collected boarding intents to include in the
	// next round.
	Boarding []BoardingIntent

	// VTXOs contains the collected VTXO requests to include in the next
	// round.
	VTXOs []types.VTXORequest

	// Forfeits tracks VTXOs being forfeited as inputs to this round.
	// Decoupled from outputs to enable many-to-many operations.
	Forfeits []types.ForfeitRequest

	// Leaves tracks on-chain exit outputs for this round. Decoupled from
	// forfeit inputs to enable many-to-many operations.
	Leaves []*types.LeaveRequest

	// Claims tracks independently authorized expired-VTXO replacements.
	Claims []VTXOClaimIntent
}

func (s *PendingRoundAssembly) String() string {
	return "PendingRoundAssembly"
}

func (s *PendingRoundAssembly) IsTerminal() bool {
	return false
}

func (s *PendingRoundAssembly) clientStateSealed() {}

// IntentSentState indicates the client has sent a JoinRoundRequest
// to the server and is waiting for confirmation.
type IntentSentState struct {
	// Intents contains all the client's intents for this round.
	Intents Intents

	// AdmittedRoundID is the server-assigned RoundID echoed in the
	// RoundJoined admission ack. Zero until the ack arrives. Once
	// set, every server-pushed event carrying a RoundID
	// (JoinRoundQuoteReceived in IntentSentState; CommitmentTxBuilt
	// in RoundJoinedState) is cross-checked against it so a hostile
	// or buggy server cannot route a payload from one of the
	// client's rounds onto another. The actor layer's routing map
	// is keyed by this same RoundID after re-keying, so under
	// normal operation the values agree by construction; the FSM
	// assertion is defense-in-depth against future routing
	// regressions.
	AdmittedRoundID RoundID
}

func (s *IntentSentState) String() string {
	return "IntentSent"
}

func (s *IntentSentState) IsTerminal() bool {
	return false
}

func (s *IntentSentState) clientStateSealed() {}

// QuoteReceivedState is entered after the server fans out a
// JoinRoundQuote for this client. The client inspects the quote's
// operator fee against MaxOperatorFee, verifies that the quote has
// RejectReason == QUOTE_OK, and emits either JoinRoundAcceptOutbox
// (advances to RoundJoinedState waiting for the commitment-tx push)
// or JoinRoundRejectOutbox (advances to ClientFailedState). A quote
// received in any other state is ignored — the quote_id binds the
// quote to a specific seal pass, and stale passes are dropped
// server-side; client-side we just defend against late deliveries.
type QuoteReceivedState struct {
	// RoundID is the round ID echoed from the quote.
	RoundID RoundID

	// Quote is the server-issued quote for this pass.
	Quote *ClientQuote

	// Intents preserves the client's original intents so the FSM
	// can thread them forward to RoundJoinedState on accept.
	Intents Intents
}

func (s *QuoteReceivedState) String() string {
	return "QuoteReceived"
}

func (s *QuoteReceivedState) IsTerminal() bool {
	return false
}

func (s *QuoteReceivedState) clientStateSealed() {}

// VTXOQuoteEntry mirrors roundpb.VTXOQuote on the client side. In
// addition to the server-decided amount it carries the pkScript and
// recipient key the server echoes from the intent, which
// evaluateQuote uses to cross-check that the quote positions agree
// with the intent the client sent. Under the #270 trust model these
// echoes are authoritative evidence that the server respected the
// client's fixed-output layout; a quote with a mismatched echo is
// rejected before the FSM commits to signing.
type VTXOQuoteEntry struct {
	// PkScript is the expected VTXO pkScript, echoed from the
	// intent. evaluateQuote requires this to match the
	// positionally-corresponding VTXORequest's EffectivePkScript.
	PkScript []byte

	// AmountSat is the server-decided amount (sats). For non-
	// change outputs this must equal the intent's target amount;
	// for the single IsChange=true output it is the residual
	// (Σin − Σ(fixed targets) − operator_fee_sat).
	AmountSat int64

	// RecipientKey is the compressed MuSig2 signing key the
	// intent listed for this position. evaluateQuote requires
	// this to match the intent's SigningKey.PubKey so the
	// server cannot silently rebind an intent slot to a different
	// recipient under the positional-index correlation.
	RecipientKey []byte
}

// LeaveQuoteEntry mirrors roundpb.LeaveQuote on the client side.
// The pkScript echo lets evaluateQuote cross-check positional
// agreement with the intent's LeaveRequests the same way
// VTXOQuoteEntry.PkScript does for VTXORequests.
type LeaveQuoteEntry struct {
	// PkScript is the on-chain pkScript, echoed from the intent's
	// LeaveRequest.Output.PkScript.
	PkScript []byte

	// AmountSat is the server-decided amount (sats). Non-change
	// leaves must equal the intent target; the single designated
	// IsChange=true output absorbs the residual.
	AmountSat int64
}

// VTXOClaimQuoteEntry mirrors roundpb.VTXOClaimQuote. Every field is checked
// against the local claim intent before the client accepts the quote.
type VTXOClaimQuoteEntry struct {
	SourceOutpoint        wire.OutPoint
	PkScript              []byte
	PolicyTemplate        []byte
	AmountSat             int64
	ReplacementSigningKey []byte
}

// ClientQuote is the client-side view of a server-issued
// JoinRoundQuote. Mirrors roundpb.JoinRoundQuote with the fields
// the FSM actually reasons about: the fee cap check against
// env.MaxOperatorFee, per-output echo cross-check against the
// intent, and the quote_id echoed on accept / reject. The per-
// output slices are indexed positionally to match the intent's
// VTXORequests / LeaveRequests order.
type ClientQuote struct {
	// QuoteID is the 32-byte server-derived identifier for this
	// pass's quote. Echoed verbatim on accept/reject so the
	// server can drop stale responses after a reseal.
	QuoteID [32]byte

	// SealPass is the zero-indexed pass number.
	SealPass uint32

	// OperatorFeeSat is the total operator fee (sats) the server
	// is charging this client for this pass. Compared against
	// env.MaxOperatorFee before the FSM signs a quote.
	OperatorFeeSat int64

	// VTXOQuotes holds the server-decided per-VTXO quote
	// entries, indexed by the intent's positional order. Each
	// entry carries the echoed pkScript and recipient key in
	// addition to the amount so the client can verify the server
	// preserved the intent's fixed-output layout.
	VTXOQuotes []VTXOQuoteEntry

	// LeaveQuotes holds the server-decided per-leave quote
	// entries, indexed by position. Each entry carries the
	// echoed pkScript in addition to the amount.
	LeaveQuotes []LeaveQuoteEntry

	// ClaimQuotes holds one fixed, zero-fee replacement quote per claim
	// input, in request order.
	ClaimQuotes []VTXOClaimQuoteEntry

	// QuoteExpiresAt is the unix timestamp (seconds) after which
	// the server will treat this client as timed out on the
	// current pass.
	QuoteExpiresAt int64

	// RejectReason is the server-side classification of why this
	// quote was emitted as a rejection. `QUOTE_OK` is the success
	// case; any other named value causes the client to transition
	// to ClientFailedState without signing. Decoder-side validation
	// in `FromProto` rejects unknown enum values outright, so this
	// field is guaranteed to hold a name the proto descriptor
	// recognizes.
	RejectReason roundpb.QuoteReason
}

// RoundJoinedState indicates the client has been accepted into a round and
// is waiting for the commitment transaction.
type RoundJoinedState struct {
	// RoundID is the unique identifier assigned by the server for this
	// round.
	RoundID RoundID

	// Intents contains all intents participating in this round.
	Intents Intents

	// Quote is the server-issued seal-time quote the client accepted
	// on the way into this state. Nil for pre-#270 test harnesses that
	// bypass the quote handshake. When set, downstream states use the
	// quote's amount slices as the authoritative expected amounts when
	// validating the server's commitment transaction.
	Quote *ClientQuote
}

func (s *RoundJoinedState) String() string {
	return "RoundJoined"
}

func (s *RoundJoinedState) IsTerminal() bool {
	return false
}

func (s *RoundJoinedState) clientStateSealed() {}

// CommitmentTxReceivedState indicates the client has received the commitment
// transaction and VTXT paths and must now validate them before proceeding.
type CommitmentTxReceivedState struct {
	// RoundID is the unique identifier for this round.
	RoundID RoundID

	// CommitmentTx is the unsigned commitment transaction as a PSBT.
	CommitmentTx *psbt.Packet

	// TxID is the transaction ID of the commitment transaction.
	TxID chainhash.Hash

	// VTXOTreePaths maps commitment tx output indices to VTXO tree paths.
	VTXOTreePaths map[int]*tree.Tree

	// TreeCosignKey is the operator's per-round VTXO-tree MuSig2 cosigner
	// key delivered with the commitment tx. Used to validate the VTXO tree
	// (and, via the extracted client trees, sign it) instead of the global
	// operator key. Nil when the server predates the field; the FSM then
	// falls back to the global operator key.
	TreeCosignKey *btcec.PublicKey

	// ConnectorOperatorKey is the operator key this round used to build its
	// connector tree. Used to reconstruct/validate the connector ancestry
	// instead of the global operator key. Nil for older servers (fallback).
	ConnectorOperatorKey *btcec.PublicKey

	// SweepKey is the operator sweep key for this round's VTXO-tree sweep
	// leaf, delivered with the commitment tx. It replaces the global
	// GetInfo sweep key for this round's tree reconstruction.
	SweepKey *btcec.PublicKey

	// SweepDelay is this round's batch-wide absolute-timelock in blocks for
	// the VTXO-tree sweep leaf, delivered with the commitment tx. It
	// replaces the global GetInfo sweep delay and drives batch-expiry
	// computation for VTXOs created in this round.
	SweepDelay uint32

	// FlowVersion is this round's flow version, carried from the
	// operator's stamp on CommitmentTxBuilt so the checkpointed round
	// records the choreography rules it was conducted under. Validated
	// on receipt; an unknown version fails the round closed before join.
	FlowVersion roundpb.FlowVersion

	// ForfeitKey is the operator's dedicated forfeit penalty key for this
	// round, delivered with the commitment tx. The forfeit-tx penalty
	// output script is a BIP-86 key-spend to this key; it replaces the
	// global GetInfo forfeit script for this round.
	ForfeitKey *btcec.PublicKey

	// Intents contains all the client's intents for this round.
	Intents Intents

	// ClientTrees maps signer keys (compressed pubkeys) to the client's
	// extracted sub-tree for that VTXO.
	ClientTrees map[SignerKey]*tree.Tree

	// Quote is the server-issued seal-time quote the client accepted
	// for this round. When non-nil, VTXO and leave output validation
	// uses the quote's positional amount slices as the expected
	// amounts (the server is the amount authority under #270) rather
	// than the intent target amounts. Nil for pre-#270 harness paths
	// that bypass the quote handshake.
	Quote *ClientQuote
}

func (s *CommitmentTxReceivedState) String() string {
	return "CommitmentTxReceived"
}

func (s *CommitmentTxReceivedState) IsTerminal() bool {
	return false
}

func (s *CommitmentTxReceivedState) clientStateSealed() {}

// CommitmentTxValidatedState indicates the client has validated the VTXT
// and is ready to participate in MuSig2 signing.
type CommitmentTxValidatedState struct {
	// RoundID is the unique identifier for this round.
	RoundID RoundID

	// CommitmentTx is the unsigned commitment transaction as a PSBT.
	CommitmentTx *psbt.Packet

	// VTXOTreePaths maps commitment tx output indices to VTXO tree paths.
	VTXOTreePaths map[int]*tree.Tree

	// SweepDelay is this round's batch-wide absolute-timelock in blocks
	// for the VTXO-tree sweep leaf, carried through the signing ceremony
	// so batch expiry can be computed on confirmation. Delivered per round
	// (not a global operator term).
	SweepDelay uint32

	// FlowVersion is this round's flow version, carried from the
	// operator's stamp on CommitmentTxBuilt so the checkpointed round
	// records the choreography rules it was conducted under. Validated
	// on receipt; an unknown version fails the round closed before join.
	FlowVersion roundpb.FlowVersion

	// ForfeitKey is the operator's dedicated forfeit penalty key for this
	// round, carried through the signing ceremony so the forfeit-tx
	// penalty output (a BIP-86 key-spend to this key) can be built and
	// validated. Delivered per round (not a global operator term).
	ForfeitKey *btcec.PublicKey

	// Intents contains all the client's intents for this round.
	Intents Intents

	// ClientTrees maps signer keys (compressed pubkeys) to the client's
	// extracted sub-tree for that VTXO.
	ClientTrees map[SignerKey]*tree.Tree

	// BoardingInputIndices maps each boarding intent's outpoint to its
	// position in the commitment transaction inputs. Used for signing.
	BoardingInputIndices map[wire.OutPoint]int

	// ForfeitMappings maps VTXO outpoints to their connector info for
	// refresh rounds. Empty for boarding-only rounds. Carried forward
	// through MuSig2 signing states until forfeit collection.
	ForfeitMappings map[wire.OutPoint]*ConnectorLeafInfo
}

func (s *CommitmentTxValidatedState) String() string {
	return "CommitmentTxValidated"
}

func (s *CommitmentTxValidatedState) IsTerminal() bool {
	return false
}

func (s *CommitmentTxValidatedState) clientStateSealed() {}

// ForfeitSignaturesCollectingState indicates the client is waiting for forfeit
// signatures from VTXO actors after completing VTXO tree signing. This state
// is entered when the round includes refresh or leave requests (VTXOs being
// rolled over or exited). The FSM waits until all expected forfeit signatures
// are collected, then submits them to the server and transitions to boarding
// input signing.
//
// The forfeit flow ensures atomic refresh: old VTXOs are forfeited (locked to
// the new commitment tx via connectors) before new VTXOs become valid. This
// prevents double-spending while preserving client custody.
type ForfeitSignaturesCollectingState struct {
	// RoundID is the unique identifier for this round.
	RoundID RoundID

	// CommitmentTx is the unsigned commitment transaction as a PSBT.
	CommitmentTx *psbt.Packet

	// VTXOTreePaths maps commitment tx output indices to VTXO tree paths.
	VTXOTreePaths map[int]*tree.Tree

	// SweepDelay is this round's batch-wide absolute-timelock in blocks
	// for the VTXO-tree sweep leaf, carried through the signing ceremony
	// so batch expiry can be computed on confirmation. Delivered per round
	// (not a global operator term).
	SweepDelay uint32

	// FlowVersion is this round's flow version, carried from the
	// operator's stamp on CommitmentTxBuilt so the checkpointed round
	// records the choreography rules it was conducted under. Validated
	// on receipt; an unknown version fails the round closed before join.
	FlowVersion roundpb.FlowVersion

	// ForfeitKey is the operator's dedicated forfeit penalty key for this
	// round, carried through the signing ceremony so the forfeit-tx
	// penalty output (a BIP-86 key-spend to this key) can be built and
	// validated. Delivered per round (not a global operator term).
	ForfeitKey *btcec.PublicKey

	// Intents contains all the client's intents for this round.
	Intents Intents

	// ClientTrees maps signer keys (compressed pubkeys) to the client's
	// extracted sub-tree for that VTXO.
	ClientTrees map[SignerKey]*tree.Tree

	// BoardingInputIndices maps each boarding intent's outpoint to its
	// position in the commitment transaction inputs. Used for signing.
	BoardingInputIndices map[wire.OutPoint]int

	// ExpectedForfeits maps VTXO outpoints to their connector info. These
	// are the VTXOs we're waiting for forfeit signatures from.
	ExpectedForfeits map[wire.OutPoint]*ConnectorLeafInfo

	// CollectedForfeits maps VTXO outpoints to their forfeit responses.
	// When len(CollectedForfeits) == len(ExpectedForfeits), we proceed.
	CollectedForfeits map[wire.OutPoint]*ForfeitSignatureResponse
}

func (s *ForfeitSignaturesCollectingState) String() string {
	return "ForfeitSignaturesCollecting"
}

func (s *ForfeitSignaturesCollectingState) IsTerminal() bool {
	return false
}

func (s *ForfeitSignaturesCollectingState) clientStateSealed() {}

// NoncesSentState indicates the client has sent nonces to the server and
// is waiting for aggregated nonces.
type NoncesSentState struct {
	// RoundID is the unique identifier for this round.
	RoundID RoundID

	// CommitmentTx is the unsigned commitment transaction as a PSBT.
	CommitmentTx *psbt.Packet

	// VTXOTreePaths maps commitment tx output indices to VTXO tree paths.
	VTXOTreePaths map[int]*tree.Tree

	// SweepDelay is this round's batch-wide absolute-timelock in blocks
	// for the VTXO-tree sweep leaf, carried through the signing ceremony
	// so batch expiry can be computed on confirmation. Delivered per round
	// (not a global operator term).
	SweepDelay uint32

	// FlowVersion is this round's flow version, carried from the
	// operator's stamp on CommitmentTxBuilt so the checkpointed round
	// records the choreography rules it was conducted under. Validated
	// on receipt; an unknown version fails the round closed before join.
	FlowVersion roundpb.FlowVersion

	// ForfeitKey is the operator's dedicated forfeit penalty key for this
	// round, carried through the signing ceremony so the forfeit-tx
	// penalty output (a BIP-86 key-spend to this key) can be built and
	// validated. Delivered per round (not a global operator term).
	ForfeitKey *btcec.PublicKey

	// Intents contains all the client's intents for this round.
	Intents Intents

	// ClientTrees maps signer keys (compressed pubkeys) to the client's
	// extracted sub-tree for that VTXO.
	ClientTrees map[SignerKey]*tree.Tree

	// Musig2Sessions maps signer keys (compressed pubkeys) to the MuSig2
	// signing session for that VTXO.
	Musig2Sessions map[SignerKey]*tree.SignerSession

	// BoardingInputIndices maps each boarding intent's outpoint to its
	// position in the commitment transaction inputs. Used for signing.
	BoardingInputIndices map[wire.OutPoint]int

	// ForfeitMappings maps VTXO outpoints to their connector info for
	// refresh rounds. Carried forward until forfeit collection after
	// VTXO tree signing.
	ForfeitMappings map[wire.OutPoint]*ConnectorLeafInfo
}

func (s *NoncesSentState) String() string {
	return "NoncesSent"
}

func (s *NoncesSentState) IsTerminal() bool {
	return false
}

func (s *NoncesSentState) clientStateSealed() {}

// NoncesAggregatedState indicates the client has received aggregated nonces
// and is ready to generate partial signatures.
type NoncesAggregatedState struct {
	// RoundID is the unique identifier for this round.
	RoundID RoundID

	// CommitmentTx is the unsigned commitment transaction as a PSBT.
	CommitmentTx *psbt.Packet

	// VTXOTreePaths maps commitment tx output indices to VTXO tree paths.
	VTXOTreePaths map[int]*tree.Tree

	// SweepDelay is this round's batch-wide absolute-timelock in blocks
	// for the VTXO-tree sweep leaf, carried through the signing ceremony
	// so batch expiry can be computed on confirmation. Delivered per round
	// (not a global operator term).
	SweepDelay uint32

	// FlowVersion is this round's flow version, carried from the
	// operator's stamp on CommitmentTxBuilt so the checkpointed round
	// records the choreography rules it was conducted under. Validated
	// on receipt; an unknown version fails the round closed before join.
	FlowVersion roundpb.FlowVersion

	// ForfeitKey is the operator's dedicated forfeit penalty key for this
	// round, carried through the signing ceremony so the forfeit-tx
	// penalty output (a BIP-86 key-spend to this key) can be built and
	// validated. Delivered per round (not a global operator term).
	ForfeitKey *btcec.PublicKey

	// Intents contains all the client's intents for this round.
	Intents Intents

	// ClientTrees maps signer keys (compressed pubkeys) to the client's
	// extracted sub-tree for that VTXO.
	ClientTrees map[SignerKey]*tree.Tree

	// Musig2Sessions maps signer keys (compressed pubkeys) to the MuSig2
	// signing session for that VTXO.
	Musig2Sessions map[SignerKey]*tree.SignerSession

	// AggNonces maps transaction IDs to aggregated MuSig2 public nonces.
	AggNonces map[tree.TxID]tree.Musig2PubNonce

	// BoardingInputIndices maps each boarding intent's outpoint to its
	// position in the commitment transaction inputs. Used for signing.
	BoardingInputIndices map[wire.OutPoint]int

	// ForfeitMappings maps VTXO outpoints to their connector info for
	// refresh rounds. Carried forward until forfeit collection after
	// VTXO tree signing.
	ForfeitMappings map[wire.OutPoint]*ConnectorLeafInfo
}

func (s *NoncesAggregatedState) String() string {
	return "NoncesAggregated"
}

func (s *NoncesAggregatedState) IsTerminal() bool {
	return false
}

func (s *NoncesAggregatedState) clientStateSealed() {}

// PartialSigsSentState indicates the client has sent partial signatures
// to the server and is waiting for the complete VTXT signatures.
type PartialSigsSentState struct {
	// RoundID is the unique identifier for this round.
	RoundID RoundID

	// CommitmentTx is the unsigned commitment transaction as a PSBT.
	CommitmentTx *psbt.Packet

	// VTXOTreePaths maps commitment tx output indices to VTXO tree paths.
	VTXOTreePaths map[int]*tree.Tree

	// SweepDelay is this round's batch-wide absolute-timelock in blocks
	// for the VTXO-tree sweep leaf, carried through the signing ceremony
	// so batch expiry can be computed on confirmation. Delivered per round
	// (not a global operator term).
	SweepDelay uint32

	// FlowVersion is this round's flow version, carried from the
	// operator's stamp on CommitmentTxBuilt so the checkpointed round
	// records the choreography rules it was conducted under. Validated
	// on receipt; an unknown version fails the round closed before join.
	FlowVersion roundpb.FlowVersion

	// ForfeitKey is the operator's dedicated forfeit penalty key for this
	// round, carried through the signing ceremony so the forfeit-tx
	// penalty output (a BIP-86 key-spend to this key) can be built and
	// validated. Delivered per round (not a global operator term).
	ForfeitKey *btcec.PublicKey

	// Intents contains all the client's intents for this round.
	Intents Intents

	// ClientTrees maps signer keys (compressed pubkeys) to the client's
	// extracted sub-tree for that VTXO.
	ClientTrees map[SignerKey]*tree.Tree

	// Musig2Sessions maps signer keys (compressed pubkeys) to the MuSig2
	// signing session for that VTXO.
	Musig2Sessions map[SignerKey]*tree.SignerSession

	// BoardingInputIndices maps each boarding intent's outpoint to its
	// position in the commitment transaction inputs. Used for signing.
	BoardingInputIndices map[wire.OutPoint]int

	// ForfeitMappings maps VTXO outpoints to their connector info for
	// refresh rounds. After VTXO tree signature validation, if non-empty,
	// transitions to ForfeitSignaturesCollectingState.
	ForfeitMappings map[wire.OutPoint]*ConnectorLeafInfo
}

func (s *PartialSigsSentState) String() string {
	return "PartialSigsSent"
}

func (s *PartialSigsSentState) IsTerminal() bool {
	return false
}

func (s *PartialSigsSentState) clientStateSealed() {}

// InputSigSentState indicates the client has sent their boarding input
// signature and is waiting for the commitment tx to be broadcast.
type InputSigSentState struct {
	// RoundID is the unique identifier for this round.
	RoundID RoundID

	// CommitmentTx is the unsigned commitment transaction as a PSBT.
	CommitmentTx *psbt.Packet

	// VTXOTreePaths maps commitment tx output indices to VTXO tree paths.
	VTXOTreePaths map[int]*tree.Tree

	// SweepDelay is this round's batch-wide absolute-timelock in blocks
	// for the VTXO-tree sweep leaf, carried through the signing ceremony
	// so batch expiry can be computed on confirmation. Delivered per round
	// (not a global operator term).
	SweepDelay uint32

	// FlowVersion is this round's flow version, carried from the
	// operator's stamp on CommitmentTxBuilt so the checkpointed round
	// records the choreography rules it was conducted under. Validated
	// on receipt; an unknown version fails the round closed before join.
	FlowVersion roundpb.FlowVersion

	// ForfeitKey is the operator's dedicated forfeit penalty key for this
	// round, carried through the signing ceremony so the forfeit-tx
	// penalty output (a BIP-86 key-spend to this key) can be built and
	// validated. Delivered per round (not a global operator term).
	ForfeitKey *btcec.PublicKey

	// Intents contains all the client's intents for this round.
	Intents Intents

	// ClientTrees maps signer keys (compressed pubkeys) to the client's
	// extracted sub-tree for that VTXO.
	ClientTrees map[SignerKey]*tree.Tree

	// InputSigs are the Schnorr signatures for the boarding inputs.
	InputSigs []*types.BoardingInputSignature

	// ForfeitedVTXOs contains outpoints of VTXOs being refreshed. When the
	// round confirms, ForfeitConfirmedToVTXO messages are emitted for each
	// so old VTXO actors can transition to the Forfeited terminal state.
	ForfeitedVTXOs []wire.OutPoint

	// PendingFailure carries a round-failure notification received while
	// forfeit signatures are already out (wavelength#844). The FSM cannot
	// fail the round on the notification alone — the operator may hold
	// fully-signed forfeit txs, so releasing the reservations needs proof
	// the commitment can never confirm — so the failure is parked here
	// while a QueryRoundStatus probe reconciles the round's fate. It is
	// in-memory only: a restart re-enters the reconcile from scratch via
	// the re-armed status-reconcile timeout.
	PendingFailure *BoardingFailed

	// ReconcileProbes counts the QueryRoundStatus probes sent for this
	// round so the re-arm duration can back off exponentially against an
	// operator that never answers (e.g. one predating the status RPC).
	// Like PendingFailure it is in-memory only; a restart resets the
	// backoff, which just means the first post-restart probe fires
	// promptly again.
	ReconcileProbes uint32
}

func (s *InputSigSentState) String() string {
	return "InputSigSent"
}

func (s *InputSigSentState) IsTerminal() bool {
	return false
}

func (s *InputSigSentState) clientStateSealed() {}

// ConfirmedState is a terminal state indicating the boarding process has
// completed successfully. The client now owns VTXOs.
type ConfirmedState struct {
	// TxID is the confirmed commitment transaction ID.
	TxID chainhash.Hash

	// BlockHeight is the height at which the transaction was confirmed.
	BlockHeight int32

	// BlockHash is the hash of the block containing the confirmation.
	BlockHash chainhash.Hash

	// Confirmations is the number of confirmations.
	Confirmations int32

	// VTXOs are the virtual UTXOs created for this client.
	VTXOs []*ClientVTXO
}

func (s *ConfirmedState) String() string {
	return "Confirmed"
}

func (s *ConfirmedState) IsTerminal() bool {
	return true
}

func (s *ConfirmedState) clientStateSealed() {}

// ClientFailedState is a terminal state indicating the boarding process failed.
// The client may be able to retry or initiate CSV recovery.
type ClientFailedState struct {
	// Reason is a human-readable description of the failure.
	Reason string

	// Error is the underlying error that caused the failure.
	Error error

	// Recoverable indicates if the client can retry or if CSV recovery is
	// needed.
	Recoverable bool

	// FailureCode is the server's typed classification of the failure,
	// carried through from BoardingFailed. It defaults to
	// RoundFailureUnknown. A terminal-for-job code (e.g.
	// RoundFailureInsufficientOperatorFunds) tells the pre-signing
	// forfeit-release chokepoint to also fail the originating job instead
	// of leaving it to recoverable replay.
	FailureCode RoundFailureCode
}

func (s *ClientFailedState) String() string {
	return fmt.Sprintf("ClientFailed: %s", s.Reason)
}

func (s *ClientFailedState) IsTerminal() bool {

	// ClientFailedState is NOT terminal - it can recover by accepting
	// IntentPackage events, which transition through Idle.
	return false
}

func (s *ClientFailedState) clientStateSealed() {}

// RecoveryInitiatedState is a semi-terminal state where the client is
// recovering their boarding UTXO via CSV timeout sweep.
type RecoveryInitiatedState struct {
	// Outpoint identifies the boarding UTXO being recovered.
	Outpoint wire.OutPoint

	// SweepTxID is the transaction ID of the sweep transaction.
	SweepTxID chainhash.Hash

	// Reason explains why recovery was initiated.
	Reason string
}

func (s *RecoveryInitiatedState) String() string {
	return "RecoveryInitiated"
}

func (s *RecoveryInitiatedState) IsTerminal() bool {
	return true
}

func (s *RecoveryInitiatedState) clientStateSealed() {}
