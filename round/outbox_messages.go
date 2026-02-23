package round

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
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

	// Identifier is the participant key used for the join-auth
	// challenge script and input-0 signature.
	Identifier *btcec.PublicKey

	// BoardingRequests contains all boarding UTXO details for this
	// session. Each confirmed intent contributes exactly one boarding
	// request so the server can register them in a single batch.
	BoardingRequests []types.BoardingRequest

	// VTXORequests specifies the VTXOs the client wants to receive.
	VTXORequests []types.VTXORequest

	// ForfeitRequests specifies the VTXOs the client wants to forfeit.
	ForfeitRequests []*ForfeitRequest

	// LeaveRequests contains VTXOs being exited to on-chain outputs. Each
	// leave request specifies only the on-chain destination output. The
	// server includes these in the batch transaction; any forfeited VTXOs
	// are listed separately in ForfeitRequests.
	LeaveRequests []*LeaveRequest

	// RoundID is optional; when empty it instructs the server to assign
	// a new round. When non-empty, the request is for the specified round.
	RoundID string
}

// ForfeitRequest describes a VTXO that will be forfeited in the round.
type ForfeitRequest struct {
	// VTXOOutpoint identifies the VTXO to forfeit.
	VTXOOutpoint wire.OutPoint
}

// LeaveRequest describes a leave output to be included in the batch
// transaction. This represents a client exiting the Ark by forfeiting an
// existing VTXO and receiving an on-chain output instead of a new VTXO.
type LeaveRequest struct {
	// Output is the on-chain destination output. Contains the value and
	// pkScript for the leave output that will be included in the batch tx.
	Output *wire.TxOut
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

// ForfeitRequestToVTXO is emitted by the FSM when a VTXO must sign a forfeit
// transaction as part of a batch swap. The round actor routes this message to
// the VTXO actor via its service key. The VTXO actor should sign the forfeit
// transaction and respond with ForfeitSignatureResponse.
//
// This message contains all information needed to construct and sign the
// forfeit transaction:
//   - Connector output from new commitment tx (links forfeit atomically)
//   - Server's forfeit address (where forfeited value is paid)
type ForfeitRequestToVTXO struct {
	actor.BaseMessage

	// VTXOOutpoint identifies the VTXO being forfeited.
	VTXOOutpoint wire.OutPoint

	// RoundID is the new round where the refreshed VTXO will be created.
	RoundID string

	// ConnectorOutpoint is the connector output from the new commitment tx
	// that the forfeit tx must spend. This links the forfeit atomically to
	// the new round - the forfeit is only valid if the new round confirms.
	ConnectorOutpoint wire.OutPoint

	// ConnectorPkScript is the scriptPubKey of the connector output.
	ConnectorPkScript []byte

	// ConnectorAmount is the value of the connector output in satoshis.
	ConnectorAmount int64

	// ServerForfeitPkScript is the operator's taproot script where the
	// forfeited VTXO value will be paid.
	ServerForfeitPkScript []byte
}

func (m *ForfeitRequestToVTXO) clientOutMsgSealed() {}

// MessageType returns the message type for logging.
func (m *ForfeitRequestToVTXO) MessageType() string {
	return "ForfeitRequestToVTXO"
}

// ForfeitConfirmedToVTXO is emitted by the FSM when the commitment transaction
// confirms, indicating that the forfeit is final. The round actor routes this
// to old VTXO actors so they can transition to the terminal Forfeited state.
type ForfeitConfirmedToVTXO struct {
	actor.BaseMessage

	// VTXOOutpoint identifies the forfeited VTXO.
	VTXOOutpoint wire.OutPoint

	// CommitmentTxID is the new commitment transaction that confirmed.
	CommitmentTxID chainhash.Hash

	// BlockHeight is the height at which confirmation occurred.
	BlockHeight int32
}

func (m *ForfeitConfirmedToVTXO) clientOutMsgSealed() {}

// MessageType returns the message type for logging.
func (m *ForfeitConfirmedToVTXO) MessageType() string {
	return "ForfeitConfirmedToVTXO"
}

// SubmitVTXOForfeitSigsToServer is emitted by the FSM after collecting all
// forfeit signatures from VTXO actors. This message contains the signatures
// for all VTXOs being refreshed in the round and is sent to the server so it
// can complete the forfeit transactions.
type SubmitVTXOForfeitSigsToServer struct {
	actor.BaseMessage

	// RoundID identifies the round.
	RoundID RoundID

	// ForfeitSigs maps VTXO outpoints to their forfeit transaction
	// signatures. Each signature is the client's schnorr signature for the
	// collaborative 2-of-2 spend from the VTXO.
	ForfeitSigs map[wire.OutPoint]*schnorr.Signature

	// ForfeitTxs maps VTXO outpoints to the built forfeit transactions.
	// The server uses these to broadcast after adding its signature.
	ForfeitTxs map[wire.OutPoint]*wire.MsgTx
}

func (m *SubmitVTXOForfeitSigsToServer) clientOutMsgSealed() {}

// MessageType returns the message type for logging.
func (m *SubmitVTXOForfeitSigsToServer) MessageType() string {
	return "SubmitVTXOForfeitSigsToServer"
}

// ToProto converts SubmitVTXOForfeitSigsToServer to a protobuf message.
// TODO: Implement actual proto conversion once proto definitions are available.
func (m *SubmitVTXOForfeitSigsToServer) ToProto() proto.Message {
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

// VTXOCreatedNotification notifies higher layers (wallet, VTXO manager) that
// new VTXOs are available after successful boarding. This is emitted once the
// commitment transaction confirms and includes the full descriptors (with tree
// paths) so the wallet can resume or unroll on-chain if needed.
//
// Note: TreeDepth is per-VTXO and derivable from ClientVTXO.TreePath.
type VTXOCreatedNotification struct {
	actor.BaseMessage

	// VTXOs are the ClientVTXOs created by this round.
	VTXOs []*ClientVTXO

	// RoundID identifies the round that created these VTXOs.
	RoundID string

	// CommitmentTxID is the txid of the confirmed commitment transaction.
	CommitmentTxID chainhash.Hash

	// BatchExpiry is the absolute block height when the batch expires.
	BatchExpiry int32

	// CreatedHeight is the block height when the commitment tx confirmed.
	CreatedHeight int32
}

// MessageType returns the message type identifier for logging and debugging.
func (m *VTXOCreatedNotification) MessageType() string {
	return "VTXOCreatedNotification"
}

func (m *VTXOCreatedNotification) clientOutMsgSealed() {}

// VTXOManagerMsg implements actormsg.VTXOManagerMsg marker interface.
func (m *VTXOCreatedNotification) VTXOManagerMsg() {}

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
