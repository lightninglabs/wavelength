package rounds

import (
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo/clientconn"
)

// Event is a sealed interface for all events that can be processed by the
// round state machine. The sealed interface pattern prevents external packages
// from implementing this interface, ensuring type safety and exhaustive pattern
// matching in state transitions.
type Event interface {
	// eventSealed is an unexported method that marks this interface as
	// sealed, preventing external implementations.
	eventSealed()
}

// ClientJoinIntentEvent is an event triggered when a client sends a request to
// join the current round.
type ClientJoinIntentEvent struct {
	// ClientID is the identifier of the client making the join request.
	// This should be used to correlate responses back to the client.
	ClientID clientconn.ClientID

	// Request contains the client's full join round request.
	Request *types.JoinRoundRequest

	// CurrentBlockHeight is the server's best-known height at the time the
	// request is processed. This is used for join-auth freshness checks.
	CurrentBlockHeight uint32
}

// eventSealed marks ClientJoinIntentEvent as implementing the sealed Event
// interface.
func (e *ClientJoinIntentEvent) eventSealed() {}

// SealEvent is an event that tells the FSM to seal the current batch and
// transition to commitment building. After this point, the round will not
// accept new join requests.
type SealEvent struct{}

// eventSealed marks SealEvent as implementing the sealed Event interface.
func (e *SealEvent) eventSealed() {}

// BuildBatchTxEvent triggers commitment transaction PSBT construction. It is
// sent as an internal event when transitioning to BatchBuildingState.
type BuildBatchTxEvent struct{}

// eventSealed marks BuildBatchTxEvent as implementing the sealed Event
// interface.
func (e *BuildBatchTxEvent) eventSealed() {}

// RegistrationTimeoutEvent is sent when the registration phase timeout expires.
// Only IntentCollectingState should handle this event.
type RegistrationTimeoutEvent struct{}

// eventSealed marks RegistrationTimeoutEvent as implementing the sealed Event
// interface.
func (e *RegistrationTimeoutEvent) eventSealed() {}

// TickEvent is delivered on a configurable cadence
// (RoundsConfig.RoundTickInterval) while a round is in
// IntentCollectingState. Unlike RegistrationTimeoutEvent, the tick is
// scheduled at round creation (so it can advance an empty round) and is
// gated by participants + the configured SealPredicate rather than
// unconditionally sealing. The handler in IntentCollectingState uses
// the event to either: skip (no clients yet), skip (predicate
// rejects), or seal via the same SealEvent path the registration
// timeout uses.
type TickEvent struct{}

// eventSealed marks TickEvent as implementing the sealed Event interface.
func (e *TickEvent) eventSealed() {}

// PrepareClientNotificationsEvent is an internal event that triggers sending
// batch information to clients. It is emitted after the PSBT is built.
type PrepareClientNotificationsEvent struct{}

// eventSealed marks PrepareClientNotificationsEvent as implementing the sealed
// Event interface.
func (e *PrepareClientNotificationsEvent) eventSealed() {}

// InputSignaturesTimeoutEvent is sent when the input signature collection
// timeout expires. Only AwaitingInputSigsState should handle this.
type InputSignaturesTimeoutEvent struct{}

// eventSealed marks InputSignaturesTimeoutEvent as implementing the sealed
// Event interface.
func (e *InputSignaturesTimeoutEvent) eventSealed() {}

// ClientInputSignaturesEvent is sent when a client submits their signatures
// for their boarding inputs. Each client must sign all their boarding inputs
// before the round can proceed.
//
// This event is used by two dispatch routes: SubmitForfeitSigs populates
// Signatures (boarding input sigs), while SubmitVTXOForfeitSigs populates
// ForfeitTxs (VTXO forfeit tx sigs). The server may receive those routes as
// separate deliveries, so the FSM accepts partial submissions and accumulates
// them until the client has provided every required artifact.
type ClientInputSignaturesEvent struct {
	// ClientID identifies which client is submitting signatures.
	ClientID clientconn.ClientID

	// Signatures contains the client's schnorr signatures for their
	// boarding inputs. Each signature is for the collaborative tapscript
	// spending path.
	Signatures []*types.BoardingInputSignature

	// ForfeitTxs contains the client's forfeit transactions with their
	// VTXO input signatures. The server will validate these and add its
	// own signatures to complete them.
	ForfeitTxs []*types.ForfeitTxSig
}

// eventSealed marks ClientInputSignaturesEvent as implementing the sealed
// Event interface.
func (e *ClientInputSignaturesEvent) eventSealed() {}

// ClientQuoteAcceptEvent is the server-side handle of an inbound
// JoinRoundAccept message. Only QuoteSentState handles this event;
// any other state treats it as stale and ignores it. Acceptance is
// explicit: the client echoes the quote_id to confirm it is
// accepting the active pass's offer (stale quote_ids from a prior
// pass are rejected by the handler so a laggy client cannot force
// the operator to build over a quote the client did not actually
// commit to).
type ClientQuoteAcceptEvent struct {
	// ClientID identifies the client accepting the quote.
	ClientID clientconn.ClientID

	// QuoteID echoes the 32-byte quote identifier the client is
	// accepting. Must equal the quote_id the server issued to this
	// client in the current pass; any other value is dropped.
	QuoteID [32]byte
}

// eventSealed marks ClientQuoteAcceptEvent as implementing the sealed
// Event interface.
func (e *ClientQuoteAcceptEvent) eventSealed() {}

// ClientQuoteRejectEvent is the server-side handle of an inbound
// JoinRoundReject message. Only QuoteSentState handles this event;
// any other state treats it as a stale message and ignores it.
type ClientQuoteRejectEvent struct {
	// ClientID identifies the client sending the rejection.
	ClientID clientconn.ClientID

	// QuoteID echoes the 32-byte quote identifier the client is
	// rejecting. Mismatches (including all-zero) are dropped by the
	// handler; only a reject whose QuoteID matches the currently
	// active quote for this client flips it to QuoteRejected.
	QuoteID [32]byte

	// Reason is a free-form client-supplied explanation for the
	// rejection (e.g. "fee exceeds cap", "policy mismatch"). Logged
	// for operator observability; does not drive server behavior.
	Reason string
}

// eventSealed marks ClientQuoteRejectEvent as implementing the sealed
// Event interface.
func (e *ClientQuoteRejectEvent) eventSealed() {}

// QuoteTimeoutEvent fires per-client at QuoteTTL after the quote was
// sent. Only QuoteSentState handles it. Clients that have already
// transitioned out of QuotePending by the time the timer fires are
// treated as no-ops (idempotent).
type QuoteTimeoutEvent struct {
	// ClientID identifies the client whose quote window expired.
	ClientID clientconn.ClientID

	// QuoteID is the 32-byte identifier of the quote that expired.
	// The handler validates this against the currently active quote
	// for the client; a mismatch (e.g. a late-firing timer after a
	// reseal) is ignored so stale timers do not disrupt a fresh
	// pass.
	QuoteID [32]byte
}

// eventSealed marks QuoteTimeoutEvent as implementing the sealed
// Event interface.
func (e *QuoteTimeoutEvent) eventSealed() {}

// AllQuotesResolvedEvent is an internal sentinel emitted by the
// QuoteSentState event handler once every pending client has reached
// a terminal status. Drives the post-wait transition (advance /
// reseal / finalize-at-cap) as a single explicit event rather than
// side-effecting the transition inline with whichever real client
// event resolved the last pending status.
type AllQuotesResolvedEvent struct{}

// eventSealed marks AllQuotesResolvedEvent as implementing the sealed
// Event interface.
func (e *AllQuotesResolvedEvent) eventSealed() {}

// VTXONoncesTimeoutEvent is sent when the VTXO nonce collection timeout
// expires. Only AwaitingVTXONoncesState should handle this.
type VTXONoncesTimeoutEvent struct{}

// eventSealed marks VTXONoncesTimeoutEvent as implementing the sealed Event
// interface.
func (e *VTXONoncesTimeoutEvent) eventSealed() {}

// ClientVTXONoncesEvent is sent when a client submits their MuSig2 nonces for
// VTXO tree transactions. Nonces are grouped by signing key so a client can
// submit all of its keys in a single message.
type ClientVTXONoncesEvent struct {
	// ClientID identifies which client is submitting nonces.
	ClientID clientconn.ClientID

	// Nonces maps signing key hex -> txid -> public nonce.
	Nonces map[SigningKeyHex]map[tree.TxID]tree.Musig2PubNonce
}

// eventSealed marks ClientVTXONoncesEvent as implementing the sealed Event
// interface.
func (e *ClientVTXONoncesEvent) eventSealed() {}

// VTXOSignaturesTimeoutEvent is sent when the VTXO partial signature collection
// timeout expires. Only AwaitingVTXOSignaturesState should handle this.
type VTXOSignaturesTimeoutEvent struct{}

// eventSealed marks VTXOSignaturesTimeoutEvent as implementing the sealed Event
// interface.
func (e *VTXOSignaturesTimeoutEvent) eventSealed() {}

// ClientVTXOPartialSigsEvent is sent when a client submits their MuSig2 partial
// signatures for VTXO tree transactions. Signatures are grouped by signing key
// so a client can submit all of its keys in a single message.
type ClientVTXOPartialSigsEvent struct {
	// ClientID identifies which client is submitting signatures.
	ClientID clientconn.ClientID

	// Signatures maps signing key hex -> txid -> partial signature.
	Signatures map[SigningKeyHex]map[tree.TxID]*musig2.PartialSignature
}

// eventSealed marks ClientVTXOPartialSigsEvent as implementing the sealed Event
// interface.
func (e *ClientVTXOPartialSigsEvent) eventSealed() {}

// ServerSignInputsEvent is an internal event that triggers the server to sign
// all inputs in the PSBT. This includes signing the operator's part of the
// collaborative spend path for boarding inputs, and finalizing wallet inputs.
type ServerSignInputsEvent struct{}

// eventSealed marks ServerSignInputsEvent as implementing the sealed Event
// interface.
func (e *ServerSignInputsEvent) eventSealed() {}

// TransactionConfirmedEvent is sent when the commitment transaction has been
// confirmed on-chain with the required number of confirmations.
type TransactionConfirmedEvent struct {
	// BlockHeight is the height of the block containing the transaction.
	BlockHeight int32

	// BlockHash is the hash of the block containing the transaction.
	BlockHash chainhash.Hash

	// NumConfs is the number of confirmations at the time of notification.
	NumConfs uint32
}

// eventSealed marks TransactionConfirmedEvent as implementing the sealed Event
// interface.
func (e *TransactionConfirmedEvent) eventSealed() {}
