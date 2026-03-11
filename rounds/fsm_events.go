package rounds

import (
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
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

// ClientJoinRequestEvent is an event triggered when a client sends a request to
// join the current round.
type ClientJoinRequestEvent struct {
	// ClientID is the identifier of the client making the join request.
	// This should be used to correlate responses back to the client.
	ClientID clientconn.ClientID

	// Request contains the client's full join round request.
	Request *types.JoinRoundRequest

	// CurrentBlockHeight is the server's best-known height at the time the
	// request is processed. This is used for join-auth freshness checks.
	CurrentBlockHeight uint32
}

// eventSealed marks ClientJoinRequestEvent as implementing the sealed Event
// interface.
func (e *ClientJoinRequestEvent) eventSealed() {}

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
// Only RegistrationState should handle this event.
type RegistrationTimeoutEvent struct{}

// eventSealed marks RegistrationTimeoutEvent as implementing the sealed Event
// interface.
func (e *RegistrationTimeoutEvent) eventSealed() {}

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
// ForfeitTxs (VTXO forfeit tx sigs). The FSM handler validates counts
// for both fields against the client's registration, so a nil/empty
// field is accepted when the client has zero entries of that kind.
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

// SignAndFinalizeSucceededEvent is sent by the OutboxHandler after it has
// successfully signed all boarding inputs, completed forfeit transactions,
// and finalized the PSBT. It carries the results back to the FSM so the
// next state can emit the persistence outbox request.
type SignAndFinalizeSucceededEvent struct {
	// FinalTx is the fully signed commitment transaction.
	FinalTx *wire.MsgTx

	// ForfeitInfos maps forfeited VTXO outpoints to forfeit metadata
	// produced during forfeit transaction completion.
	ForfeitInfos map[wire.OutPoint]*ForfeitInfo
}

// eventSealed marks SignAndFinalizeSucceededEvent as implementing the
// sealed Event interface.
func (e *SignAndFinalizeSucceededEvent) eventSealed() {}

// SignAndFinalizeFailedEvent is sent by the OutboxHandler when signing
// or finalization fails.
type SignAndFinalizeFailedEvent struct {
	// Reason describes why the signing or finalization failed.
	Reason string
}

// eventSealed marks SignAndFinalizeFailedEvent as implementing the
// sealed Event interface.
func (e *SignAndFinalizeFailedEvent) eventSealed() {}

// PersistServerSigningSucceededEvent is sent by the OutboxHandler after it
// has successfully persisted the round and VTXOs following server signing.
type PersistServerSigningSucceededEvent struct{}

// eventSealed marks PersistServerSigningSucceededEvent as implementing the
// sealed Event interface.
func (e *PersistServerSigningSucceededEvent) eventSealed() {}

// PersistServerSigningFailedEvent is sent by the OutboxHandler when
// persisting the round or VTXOs after server signing fails.
type PersistServerSigningFailedEvent struct {
	// Reason describes why the persistence failed.
	Reason string
}

// eventSealed marks PersistServerSigningFailedEvent as implementing the
// sealed Event interface.
func (e *PersistServerSigningFailedEvent) eventSealed() {}

// ConfirmRoundSucceededEvent is sent by the OutboxHandler after it has
// successfully persisted all round confirmation data (VTXOs marked live,
// forfeits recorded, round marked confirmed).
type ConfirmRoundSucceededEvent struct{}

// eventSealed marks ConfirmRoundSucceededEvent as implementing the sealed
// Event interface.
func (e *ConfirmRoundSucceededEvent) eventSealed() {}

// ConfirmRoundFailedEvent is sent by the OutboxHandler when persisting round
// confirmation data fails.
type ConfirmRoundFailedEvent struct {
	// Reason describes why the confirmation persistence failed.
	Reason string
}

// eventSealed marks ConfirmRoundFailedEvent as implementing the sealed Event
// interface.
func (e *ConfirmRoundFailedEvent) eventSealed() {}
