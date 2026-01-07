package rounds

import (
	"github.com/btcsuite/btcd/btcec/v2"
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

// ClientJoinRequestEvent is an event triggered when a client sends a request to
// join the current round.
type ClientJoinRequestEvent struct {
	// ClientID is the identifier of the client making the join request.
	// This should be used to correlate responses back to the client.
	ClientID clientconn.ClientID

	// Request contains the client's full join round request.
	Request *types.JoinRoundRequest
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

// BoardingSignaturesTimeoutEvent is sent when the boarding signature collection
// timeout expires. Only AwaitingBoardingSigsState should handle this.
type BoardingSignaturesTimeoutEvent struct{}

// eventSealed marks BoardingSignaturesTimeoutEvent as implementing the sealed
// Event interface.
func (e *BoardingSignaturesTimeoutEvent) eventSealed() {}

// ClientBoardingSignaturesEvent is sent when a client submits their signatures
// for their boarding inputs. Each client must sign all their boarding inputs
// before the round can proceed.
type ClientBoardingSignaturesEvent struct {
	// ClientID identifies which client is submitting signatures.
	ClientID clientconn.ClientID

	// Signatures contains the client's schnorr signatures for their
	// boarding inputs. Each signature is for the collaborative tapscript
	// spending path.
	Signatures []*types.BoardingInputSignature
}

// eventSealed marks ClientBoardingSignaturesEvent as implementing the sealed
// Event interface.
func (e *ClientBoardingSignaturesEvent) eventSealed() {}

// VTXONoncesTimeoutEvent is sent when the VTXO nonce collection timeout
// expires. Only AwaitingVTXONoncesState should handle this.
type VTXONoncesTimeoutEvent struct{}

// eventSealed marks VTXONoncesTimeoutEvent as implementing the sealed Event
// interface.
func (e *VTXONoncesTimeoutEvent) eventSealed() {}

// ClientVTXONoncesEvent is sent when a client submits their MuSig2 nonces for
// VTXO tree transactions. Each client with VTXOs must submit nonces for all
// transactions where they are a cosigner.
type ClientVTXONoncesEvent struct {
	// ClientID identifies which client is submitting nonces.
	ClientID clientconn.ClientID

	// SigningKey is the public key the client uses for signing. This is
	// used to identify which transactions the client is a cosigner for.
	SigningKey *btcec.PublicKey

	// Nonces maps transaction IDs to the client's public nonces for those
	// transactions.
	Nonces map[tree.TxID]tree.Musig2PubNonce
}

// eventSealed marks ClientVTXONoncesEvent as implementing the sealed Event
// interface.
func (e *ClientVTXONoncesEvent) eventSealed() {}

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
