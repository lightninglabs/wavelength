package round

import (
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"google.golang.org/protobuf/proto"
)

// JoinRoundRequest is sent from client to server to request joining a round.
// This implements ClientEvent and is emitted via Outbox.
type JoinRoundRequest struct {
	actor.BaseMessage

	// BoardingRequests contains all boarding UTXO details for this
	// session. Each confirmed intent contributes exactly one boarding
	// request so the server can register them in a single batch.
	BoardingRequests []types.BoardingRequest

	// VTXORequests specifies the VTXOs the client wants to receive.
	VTXORequests []types.VTXORequest
}

func (m *JoinRoundRequest) clientOutMsgSealed() {}

// SubmitNoncesRequest is sent from client to server with MuSig2 nonces.
// This implements ClientOutMsg and is emitted via Outbox.
type SubmitNoncesRequest struct {
	actor.BaseMessage

	// RoundID identifies the round.
	RoundID RoundID

	// Nonces maps signing keys to their per-transaction MuSig2 public
	// nonces. The outer map is keyed by signing key (one per VTXO), and
	// the inner map is keyed by transaction ID. This structure matches
	// the server's expected format where nonces are grouped by cosigner.
	Nonces map[SignerKey]map[tree.TxID]tree.Musig2PubNonce
}

func (m *SubmitNoncesRequest) clientOutMsgSealed() {}

// SubmitPartialSigRequest is sent from client to server with partial
// signatures. This implements ClientEvent and is emitted via Outbox.
type SubmitPartialSigRequest struct {
	actor.BaseMessage

	// RoundID identifies the round.
	RoundID RoundID

	// Signatures maps signing keys to their per-transaction MuSig2 partial
	// signatures. The outer map is keyed by signing key (one per VTXO), and
	// the inner map is keyed by transaction ID. This structure matches the
	// server's expected format where signatures are grouped by cosigner.
	Signatures map[SignerKey]map[tree.TxID]*musig2.PartialSignature
}

func (m *SubmitPartialSigRequest) clientOutMsgSealed() {}

// SubmitForfeitSigRequest is sent from client to server with the boarding input
// signature. This implements ClientEvent and is emitted via Outbox.
type SubmitForfeitSigRequest struct {
	actor.BaseMessage

	// RoundID identifies the round.
	RoundID RoundID

	// Signatures contains structured boarding input signatures. Each
	// signature includes the input index, outpoint, and schnorr signature
	// for the collaborative tapscript spend path.
	Signatures []*types.BoardingInputSignature
}

func (m *SubmitForfeitSigRequest) clientOutMsgSealed() {}

// ToProto converts JoinRoundRequest to a protobuf message.
// TODO: Implement actual proto conversion once proto definitions are available.
func (m *JoinRoundRequest) ToProto() proto.Message {
	// Placeholder: return nil for now. This will be replaced with actual
	// proto message construction:
	// return &pb.JoinRoundRequest{...}
	return nil
}

// ToProto converts SubmitNoncesRequest to a protobuf message.
// TODO: Implement actual proto conversion once proto definitions are available.
func (m *SubmitNoncesRequest) ToProto() proto.Message {
	// Placeholder: return nil for now. This will be replaced with actual
	// proto message construction:
	// return &pb.SubmitNoncesRequest{...}
	return nil
}

// ToProto converts SubmitPartialSigRequest to a protobuf message.
// TODO: Implement actual proto conversion once proto definitions are available.
func (m *SubmitPartialSigRequest) ToProto() proto.Message {
	// Placeholder: return nil for now. This will be replaced with actual
	// proto message construction:
	// return &pb.SubmitPartialSigRequest{...}
	return nil
}

// ToProto converts SubmitForfeitSigRequest to a protobuf message.
// TODO: Implement actual proto conversion once proto definitions are available.
func (m *SubmitForfeitSigRequest) ToProto() proto.Message {
	// Placeholder: return nil for now. This will be replaced with actual
	// proto message construction:
	// return &pb.SubmitForfeitSigRequest{...}
	return nil
}

// RegisterConfirmationRequest is emitted by the FSM to request chain monitoring
// for a transaction. The actor will complete this message with the NotifyActor
// field before sending to ChainSource.
//
// This implements ClientEvent so it can be emitted via Outbox. The actor will
// convert this to a chainsource.RegisterConfRequest and add the NotifyActor
// field pointing to itself.
type RegisterConfirmationRequest struct {
	actor.BaseMessage

	// CallerID is a unique identifier for this monitoring request. This is
	// used by ChainSource to construct the service key for the dedicated
	// confirmation actor.
	CallerID string

	// PkScript is the public key script to monitor.
	PkScript []byte

	// Txid is optional and, if set, instructs the monitoring backend to
	// watch for confirmations of the specific transaction.
	Txid *chainhash.Hash

	// TargetConfs is the number of confirmations to wait for.
	TargetConfs uint32

	// HeightHint is an optional height hint indicating the earliest block
	// that could contain the transaction. Set to 0 if unknown.
	HeightHint uint32
}

func (m *RegisterConfirmationRequest) clientOutMsgSealed() {}

// VTXOCreatedNotification notifies higher layers (wallet) that new VTXOs are
// available after successful boarding. This is emitted once the commitment
// transaction confirms and includes the full descriptors (with tree paths) so
// the wallet can resume or unroll on-chain if needed.
type VTXOCreatedNotification struct {
	actor.BaseMessage

	VTXOs []*ClientVTXO
}

// MessageType returns the message type identifier for logging and debugging.
func (m *VTXOCreatedNotification) MessageType() string {
	return "VTXOCreatedNotification"
}

func (m *VTXOCreatedNotification) clientOutMsgSealed() {}

// RoundCompletedNotification is emitted when a round FSM reaches ConfirmedState
// which signals the actor to perform cleanup (remove from activeRounds,
// finalize storage). This replaces the need for manual state inspection via
// checkRoundCompletion().
type RoundCompletedNotification struct {
	actor.BaseMessage

	// RoundID identifies the completed round.
	RoundID RoundID

	// TxID is the confirmed commitment transaction ID.
	TxID chainhash.Hash

	// ConfInfo contains the block height and hash at which the commitment
	// tx was confirmed.
	ConfInfo ConfInfo
}

func (m *RoundCompletedNotification) clientOutMsgSealed() {}

// RoundCheckpointedNotification is emitted by the primary FSM when it reaches
// InputSigSentState. This signals that a round has been checkpointed to
// storage and should be migrated to a dedicated round FSM. This replaces the
// need for manual state inspection via checkPrimaryFSMForNewRound().
type RoundCheckpointedNotification struct {
	actor.BaseMessage

	// RoundID identifies the checkpointed round to migrate.
	RoundID RoundID
}

func (m *RoundCheckpointedNotification) clientOutMsgSealed() {}

// RoundFailedNotification is emitted when a round FSM transitions to
// ClientFailedState. This notifies higher layers (actor, wallet) of the
// failure so they can update UI, trigger recovery flows, or clean up
// resources. The server may also be notified to abort the round.
type RoundFailedNotification struct {
	actor.BaseMessage

	// RoundID identifies the failed round. None if the failure occurred
	// before a round was assigned.
	RoundID fn.Option[RoundID]

	// Reason is a human-readable description of the failure.
	Reason string

	// Recoverable indicates if the client can retry the round or if CSV
	// recovery is needed.
	Recoverable bool

	// OriginalError contains the underlying error for logging/debugging.
	OriginalError error
}

func (m *RoundFailedNotification) clientOutMsgSealed() {}
