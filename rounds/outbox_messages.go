package rounds

import (
	"time"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	"github.com/lightninglabs/darepo-client/rpc/roundpb"
	"github.com/lightninglabs/darepo/clientconn"
	"google.golang.org/protobuf/proto"
)

// Round outbox messages use the roundpb.Method* constants from the
// client submodule as the single source of truth for routing keys.
// The client-side EventRouter registers handlers under the same
// constants, so any mismatch becomes a compile error rather than a
// silent dispatch failure.

// OutboxEvent is a sealed interface for all outbox messages emitted
// by the round FSM. The sealed interface pattern prevents external
// packages from implementing this interface, ensuring type safety
// and exhaustive pattern matching in state transitions.
//
// As in OOR, rounds keep side effects behind explicit outbox messages so
// transitions remain pure and deterministic.
type OutboxEvent interface {
	// outboxEventSealed is an unexported method that marks this interface
	// as sealed, preventing external implementations.
	outboxEventSealed()
}

// ClientErrorResp is an outbox message emitted by the FSM to send
// error responses back to clients via the ClientConnectionActor.
type ClientErrorResp struct {
	// Client is the identifier of the client to send the error to.
	Client clientconn.ClientID

	// ErrorMsg is the error message to send to the client.
	ErrorMsg string
}

// newClientErrorResp creates a new ClientErrorResp for the given client.
func newClientErrorResp(clientID clientconn.ClientID,
	msg string) *ClientErrorResp {

	return &ClientErrorResp{
		Client:   clientID,
		ErrorMsg: msg,
	}
}

// ClientID returns the identifier of the client to send the message to.
func (c *ClientErrorResp) ClientID() clientconn.ClientID {
	return c.Client
}

// ToProto converts ClientErrorResp to the roundpb wire format.
func (c *ClientErrorResp) ToProto() proto.Message {
	return &roundpb.ClientErrorResp{
		ErrorMsg: c.ErrorMsg,
	}
}

// ServiceMethod returns the routing key for client-side ingress
// dispatch.
func (c *ClientErrorResp) ServiceMethod() mailboxrpc.ServiceMethod {
	return mailboxrpc.ServiceMethod{
		Service: roundpb.ServiceName,
		Method:  roundpb.MethodError,
	}
}

// outboxEventSealed marks ClientErrorResp as implementing the sealed
// OutboxEvent interface.
func (c *ClientErrorResp) outboxEventSealed() {}

// ClientSuccessResp is an outbox message emitted by the FSM to send
// a successful join response back to a client via the ClientConnectionActor.
type ClientSuccessResp struct {
	// Client is the identifier of the client to send the response to.
	Client clientconn.ClientID

	// RoundID is the identifier of the round the client has joined.
	RoundID RoundID

	// AcceptedBoardingOutpoints contains the boarding outpoints that were
	// accepted into this round. Used by clients to correlate the response
	// to the correct pending round when multiple rounds are in-flight.
	AcceptedBoardingOutpoints []wire.OutPoint

	// AcceptedVTXOOutpoints contains the VTXO outpoints involved in this
	// round. Used for operations like forfeit, leave, and refresh.
	AcceptedVTXOOutpoints []wire.OutPoint
}

// ClientID returns the identifier of the client to send the message to.
func (c *ClientSuccessResp) ClientID() clientconn.ClientID {
	return c.Client
}

// ToProto converts ClientSuccessResp to the roundpb wire format.
func (c *ClientSuccessResp) ToProto() proto.Message {
	return &roundpb.ClientSuccessResp{
		RoundId: c.RoundID[:],
		AcceptedBoardingOutpoints: roundpb.OutpointsToProto(
			c.AcceptedBoardingOutpoints,
		),
		AcceptedVtxoOutpoints: roundpb.OutpointsToProto(
			c.AcceptedVTXOOutpoints,
		),
	}
}

// ServiceMethod returns the routing key for client-side ingress
// dispatch.
func (c *ClientSuccessResp) ServiceMethod() mailboxrpc.ServiceMethod {
	return mailboxrpc.ServiceMethod{
		Service: roundpb.ServiceName,
		Method:  roundpb.MethodJoinAck,
	}
}

// outboxEventSealed marks ClientSuccessResp as implementing the sealed
// OutboxEvent interface.
func (c *ClientSuccessResp) outboxEventSealed() {}

// ClientAwaitingInputSigsResp is an outbox message sent to clients with
// boarding inputs when the server is ready to receive their boarding
// signatures. This is sent separately from ClientBatchInfo because there may
// be VTXO signing phases between batch construction and boarding signature
// collection.
type ClientAwaitingInputSigsResp struct {
	// Client is the identifier of the client to notify.
	Client clientconn.ClientID

	// RoundID identifies which round is awaiting input signatures.
	RoundID RoundID
}

// ClientID returns the identifier of the client to send the message to.
func (c *ClientAwaitingInputSigsResp) ClientID() clientconn.ClientID {
	return c.Client
}

// ToProto converts ClientAwaitingInputSigsResp to the roundpb wire
// format.
func (c *ClientAwaitingInputSigsResp) ToProto() proto.Message {
	return &roundpb.ClientAwaitingInputSigsResp{
		RoundId: c.RoundID[:],
	}
}

// ServiceMethod returns the routing key for client-side ingress
// dispatch.
func (c *ClientAwaitingInputSigsResp) ServiceMethod() mailboxrpc.ServiceMethod {
	return mailboxrpc.ServiceMethod{
		Service: roundpb.ServiceName,
		Method:  roundpb.MethodAwaitingInputSigs,
	}
}

// outboxEventSealed marks ClientAwaitingInputSigsResp as implementing the
// sealed OutboxEvent interface.
func (c *ClientAwaitingInputSigsResp) outboxEventSealed() {}

// ClientVTXOAggNonces is an outbox message sent to clients with VTXOs after all
// nonces have been collected and aggregated. The client uses these aggregated
// nonces to generate their partial signatures.
type ClientVTXOAggNonces struct {
	// Client is the identifier of the client to send nonces to.
	Client clientconn.ClientID

	// RoundID identifies which round these aggregated nonces belong to.
	RoundID RoundID

	// AggNonces maps transaction IDs to the aggregated public nonces for
	// those transactions. Only includes transactions where this client is
	// a cosigner.
	AggNonces map[tree.TxID]tree.Musig2PubNonce
}

// ClientID returns the identifier of the client to send the message to.
func (c *ClientVTXOAggNonces) ClientID() clientconn.ClientID {
	return c.Client
}

// ToProto converts ClientVTXOAggNonces to the roundpb wire format.
// Nonce keys are hex-encoded TxIDs, values are raw 66-byte public
// nonces.
func (c *ClientVTXOAggNonces) ToProto() proto.Message {
	nonces := make(map[string][]byte, len(c.AggNonces))
	for txID, nonce := range c.AggNonces {
		nonces[roundpb.TxIDToHex(txID)] = nonce[:]
	}

	return &roundpb.ClientVTXOAggNonces{
		RoundId:   c.RoundID[:],
		AggNonces: nonces,
	}
}

// ServiceMethod returns the routing key for client-side ingress
// dispatch.
func (c *ClientVTXOAggNonces) ServiceMethod() mailboxrpc.ServiceMethod {
	return mailboxrpc.ServiceMethod{
		Service: roundpb.ServiceName,
		Method:  roundpb.MethodAggNonces,
	}
}

// outboxEventSealed marks ClientVTXOAggNonces as implementing the sealed
// OutboxEvent interface.
func (c *ClientVTXOAggNonces) outboxEventSealed() {}

// ClientVTXOAggSigs is an outbox message sent to clients with VTXOs after all
// partial signatures have been collected and aggregated into final schnorr
// signatures. The client stores these signatures for their VTXOs.
type ClientVTXOAggSigs struct {
	// Client is the identifier of the client to send signatures to.
	Client clientconn.ClientID

	// RoundID identifies which round these aggregated signatures belong to.
	RoundID RoundID

	// AggSigs maps transaction IDs to the final aggregated schnorr
	// signatures. Only includes transactions where this client is a
	// cosigner.
	AggSigs map[tree.TxID]*schnorr.Signature
}

// ClientID returns the identifier of the client to send the message to.
func (c *ClientVTXOAggSigs) ClientID() clientconn.ClientID {
	return c.Client
}

// ToProto converts ClientVTXOAggSigs to the roundpb wire format.
// Signature keys are hex-encoded TxIDs, values are raw 64-byte
// schnorr signatures.
func (c *ClientVTXOAggSigs) ToProto() proto.Message {
	sigs := make(map[string][]byte, len(c.AggSigs))
	for txID, sig := range c.AggSigs {
		sigs[roundpb.TxIDToHex(txID)] = roundpb.SchnorrSigToBytes(
			sig,
		)
	}

	return &roundpb.ClientVTXOAggSigs{
		RoundId: c.RoundID[:],
		AggSigs: sigs,
	}
}

// ServiceMethod returns the routing key for client-side ingress
// dispatch.
func (c *ClientVTXOAggSigs) ServiceMethod() mailboxrpc.ServiceMethod {
	return mailboxrpc.ServiceMethod{
		Service: roundpb.ServiceName,
		Method:  roundpb.MethodAggSigs,
	}
}

// outboxEventSealed marks ClientVTXOAggSigs as implementing the sealed
// OutboxEvent interface.
func (c *ClientVTXOAggSigs) outboxEventSealed() {}

// RoundSealedReq is emitted when a round has been sealed (registration closed).
// The actor should create a new round to accept new registrations.
type RoundSealedReq struct {
	// SealedRoundID is the ID of the round that was just sealed.
	SealedRoundID RoundID
}

// outboxEventSealed marks RoundSealedReq as implementing the sealed OutboxEvent
// interface.
func (r *RoundSealedReq) outboxEventSealed() {}

// StartTimeoutReq is emitted when the FSM wants to start a timeout. The
// duration is specified by the FSM based on the current state's requirements.
// The Phase field identifies which state scheduled this timeout, allowing the
// actor to send the appropriate phase-specific timeout event when it expires.
type StartTimeoutReq struct {
	// RoundID is the identifier of the round to schedule a timeout for.
	RoundID RoundID

	// Phase identifies which FSM phase is scheduling this timeout. This is
	// used to create a composite timeout ID and to determine which timeout
	// event type to send when the timeout expires.
	Phase TimeoutPhase

	// Duration is how long to wait before the timeout fires.
	Duration time.Duration
}

// outboxEventSealed marks StartTimeoutReq as implementing the sealed
// OutboxEvent interface.
func (s *StartTimeoutReq) outboxEventSealed() {}

// newStartTimeoutReq creates a StartTimeoutReq for the given phase. The
// duration is determined by the phase and the environment's terms.
func newStartTimeoutReq(env *Environment, phase TimeoutPhase) *StartTimeoutReq {
	var duration time.Duration

	switch phase {
	case TimeoutPhaseRegistration:
		duration = env.Terms.RegistrationTimeout

	case TimeoutPhaseInputSigs, TimeoutPhaseVTXONonces,
		TimeoutPhaseVTXOSignatures:

		duration = env.Terms.SignatureCollectionTimeout
	}

	return &StartTimeoutReq{
		RoundID:  env.RoundID,
		Phase:    phase,
		Duration: duration,
	}
}

// CancelTimeoutReq is emitted when the FSM wants to cancel a pending timeout.
type CancelTimeoutReq struct {
	// RoundID is the identifier of the round to cancel the timeout for.
	RoundID RoundID

	// Phase identifies which FSM phase timeout to cancel. This is combined
	// with RoundID to form the composite timeout ID.
	Phase TimeoutPhase
}

// outboxEventSealed marks CancelTimeoutReq as implementing the sealed
// OutboxEvent interface.
func (c *CancelTimeoutReq) outboxEventSealed() {}

// ClientBatchInfo is an outbox message containing batch transaction data
// to send to a client. The client needs this information to create signatures
// for their boarding inputs and VTXO tree paths.
type ClientBatchInfo struct {
	// Client is the identifier of the client to send data to.
	Client clientconn.ClientID

	// RoundID is the identifier of the round this batch belongs to.
	RoundID RoundID

	// BatchPSBT is the partially signed batch transaction. The client needs
	// the full PSBT (including witness UTXOs and other metadata) to create
	// their signatures for boarding inputs.
	BatchPSBT *psbt.Packet

	// VTXOTreePaths maps tree output indices to the extracted tree paths
	// for this client. Each path contains only the transactions where the
	// client is a cosigner. This is nil if the client has no VTXO requests.
	VTXOTreePaths map[int]*tree.Tree

	// ConnectorLeafMap maps forfeited VTXO outpoints to connector leaf
	// information. This is nil if the client has no forfeit requests.
	ConnectorLeafMap map[wire.OutPoint]*types.ConnectorLeafInfo
}

// ClientID returns the identifier of the client to send the message to.
func (c *ClientBatchInfo) ClientID() clientconn.ClientID {
	return c.Client
}

// ToProto converts ClientBatchInfo to the roundpb wire format. The
// PSBT is serialized to bytes, VTXO trees are flattened to pre-order
// node slices, and connector leaves are keyed by outpoint string.
func (c *ClientBatchInfo) ToProto() proto.Message {
	psbtBytes, err := roundpb.PSBTToBytes(c.BatchPSBT)
	if err != nil {
		// Best effort: return what we can without the PSBT.
		psbtBytes = nil
	}

	// Convert VTXO tree paths keyed by output index.
	var treePaths map[int32]*roundpb.VTXOTree
	if len(c.VTXOTreePaths) > 0 {
		treePaths = make(
			map[int32]*roundpb.VTXOTree, len(c.VTXOTreePaths),
		)
		for idx, t := range c.VTXOTreePaths {
			pt, treeErr := roundpb.TreeToProto(t)
			if treeErr != nil {
				continue
			}
			treePaths[int32(idx)] = pt
		}
	}

	// Convert connector leaf map keyed by outpoint string.
	var connLeaves map[string]*roundpb.ConnectorLeafInfo
	if len(c.ConnectorLeafMap) > 0 {
		connLeaves = make(
			map[string]*roundpb.ConnectorLeafInfo,
			len(c.ConnectorLeafMap),
		)
		for op, leaf := range c.ConnectorLeafMap {
			key := roundpb.OutpointToMapKey(op)
			connLeaves[key] = connectorLeafInfoToProto(leaf)
		}
	}

	return &roundpb.ClientBatchInfo{
		RoundId:          c.RoundID[:],
		BatchPsbt:        psbtBytes,
		VtxoTreePaths:    treePaths,
		ConnectorLeafMap: connLeaves,
	}
}

// connectorLeafInfoToProto converts a types.ConnectorLeafInfo to the
// roundpb wire format.
func connectorLeafInfoToProto(
	leaf *types.ConnectorLeafInfo) *roundpb.ConnectorLeafInfo {

	if leaf == nil {
		return nil
	}

	return &roundpb.ConnectorLeafInfo{
		LeafOutpoint: roundpb.OutpointToProto(leaf.LeafOutpoint),
		LeafOutput:   roundpb.TxOutToProto(leaf.LeafOutput),
	}
}

// ServiceMethod returns the routing key for client-side ingress
// dispatch.
func (c *ClientBatchInfo) ServiceMethod() mailboxrpc.ServiceMethod {
	return mailboxrpc.ServiceMethod{
		Service: roundpb.ServiceName,
		Method:  roundpb.MethodBatchInfo,
	}
}

// outboxEventSealed marks ClientBatchInfo as implementing the sealed
// OutboxEvent interface.
func (c *ClientBatchInfo) outboxEventSealed() {}

// ClientRoundFailedResp is an outbox message emitted to notify a client that
// the round they joined has failed. The client should discard any state
// associated with this round.
type ClientRoundFailedResp struct {
	// Client is the identifier of the client to notify.
	Client clientconn.ClientID

	// RoundID is the identifier of the failed round.
	RoundID RoundID

	// Reason describes why the round failed.
	Reason string
}

// ClientID returns the identifier of the client to send the message to.
func (c *ClientRoundFailedResp) ClientID() clientconn.ClientID {
	return c.Client
}

// ToProto converts ClientRoundFailedResp to the roundpb wire format.
func (c *ClientRoundFailedResp) ToProto() proto.Message {
	return &roundpb.ClientRoundFailedResp{
		RoundId: c.RoundID[:],
		Reason:  c.Reason,
	}
}

// ServiceMethod returns the routing key for client-side ingress
// dispatch.
func (c *ClientRoundFailedResp) ServiceMethod() mailboxrpc.ServiceMethod {
	return mailboxrpc.ServiceMethod{
		Service: roundpb.ServiceName,
		Method:  roundpb.MethodRoundFailed,
	}
}

// outboxEventSealed marks ClientRoundFailedResp as implementing the sealed
// OutboxEvent interface.
func (c *ClientRoundFailedResp) outboxEventSealed() {}

// RoundFailedReq is emitted when a round has failed. The actor should clean up
// any resources associated with the round and potentially create a new round.
type RoundFailedReq struct {
	// FailedRoundID is the ID of the round that failed.
	FailedRoundID RoundID

	// Reason describes why the round failed.
	Reason string
}

// outboxEventSealed marks RoundFailedReq as implementing the sealed OutboxEvent
// interface.
func (r *RoundFailedReq) outboxEventSealed() {}

// BroadcastRoundReq requests the actor to broadcast the signed commitment
// transaction to the network and subscribe to its confirmation.
type BroadcastRoundReq struct {
	// RoundID is the identifier of the round to broadcast.
	RoundID RoundID

	// SignedTx is the fully signed commitment transaction to broadcast.
	SignedTx *wire.MsgTx

	// StartHeight is the block height when the round was created. Used as
	// the height hint for confirmation scanning to ensure we don't miss
	// confirmations that occur between round creation and broadcast.
	StartHeight uint32
}

// outboxEventSealed marks BroadcastRoundReq as implementing the sealed
// OutboxEvent interface.
func (b *BroadcastRoundReq) outboxEventSealed() {}
