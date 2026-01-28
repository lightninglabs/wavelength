//go:build systest

package systest

import (
	"context"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	clienttree "github.com/lightninglabs/darepo-client/lib/tree"
	clienttypes "github.com/lightninglabs/darepo-client/lib/types"
	clientround "github.com/lightninglabs/darepo-client/round"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightninglabs/darepo/rounds"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/subscribe"
)

// pendingC2SMessage holds a buffered client-to-server message.
type pendingC2SMessage struct {
	ctx context.Context
	msg rounds.ActorMsg
}

// BridgeServerConn implements serverconn.ServerConnMsg interface to route
// client outbox messages directly to the server rounds actor. This replaces
// gRPC with in-process message routing for tests. All messages are recorded to
// a shared transcript for test assertions.
type BridgeServerConn struct {
	mu sync.Mutex

	// clientID identifies this client connection.
	clientID clientconn.ClientID

	// roundsActor is a reference to the server's rounds actor for message
	// routing.
	roundsActor actor.TellOnlyRef[rounds.ActorMsg]

	// transcript records all messages for test assertions.
	transcript *MessageTranscript

	// buffered when true, messages are queued instead of immediately
	// delivered.
	buffered bool

	// pendingC2S holds buffered client-to-server messages.
	pendingC2S []pendingC2SMessage
}

// NewBridgeServerConn creates a new bridge server connection for a client.
func NewBridgeServerConn(clientID clientconn.ClientID,
	roundsActor actor.TellOnlyRef[rounds.ActorMsg],
	transcript *MessageTranscript) *BridgeServerConn {

	return &BridgeServerConn{
		clientID:    clientID,
		roundsActor: roundsActor,
		transcript:  transcript,
	}
}

// Receive processes outgoing client messages and routes them to the server.
func (b *BridgeServerConn) Receive(ctx context.Context,
	msg serverconn.ServerConnMsg) fn.Result[serverconn.ServerConnResp] {

	switch m := msg.(type) {
	case *serverconn.SendClientEventRequest:
		return b.handleSendClientEvent(ctx, m)

	default:
		return fn.Err[serverconn.ServerConnResp](fmt.Errorf(
			"unknown message type: %T", msg,
		))
	}
}

// SetBuffered enables or disables message buffering. When enabled, messages
// are queued instead of immediately delivered to the server.
func (b *BridgeServerConn) SetBuffered(buffered bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.buffered = buffered
}

// PendingCount returns the number of buffered client-to-server messages.
func (b *BridgeServerConn) PendingCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return len(b.pendingC2S)
}

// FlushNext delivers the next buffered message to the server. Returns an error
// if no messages are pending.
func (b *BridgeServerConn) FlushNext() error {
	b.mu.Lock()
	if len(b.pendingC2S) == 0 {
		b.mu.Unlock()

		return fmt.Errorf("no pending messages")
	}

	msg := b.pendingC2S[0]
	b.pendingC2S = b.pendingC2S[1:]
	b.mu.Unlock()

	b.roundsActor.Tell(msg.ctx, msg.msg)

	return nil
}

// FlushAll delivers all buffered messages to the server in order.
func (b *BridgeServerConn) FlushAll() {
	b.mu.Lock()
	pending := b.pendingC2S
	b.pendingC2S = nil
	b.mu.Unlock()

	for _, msg := range pending {
		b.roundsActor.Tell(msg.ctx, msg.msg)
	}
}

// handleSendClientEvent converts a client outbox message to a server actor
// message and routes it directly to the rounds actor (or buffers it if
// buffering is enabled).
func (b *BridgeServerConn) handleSendClientEvent(ctx context.Context,
	req *serverconn.SendClientEventRequest) fn.Result[serverconn.ServerConnResp] {

	// Record the message to the transcript first.
	b.transcript.Record(ClientToServer, b.clientID, req.Message)

	// Convert the client outbox message to a server actor message.
	actorMsg, err := b.convertToActorMsg(req.Message)
	if err != nil {
		return fn.Err[serverconn.ServerConnResp](fmt.Errorf(
			"convert client message: %w", err,
		))
	}

	// Check if buffering is enabled.
	b.mu.Lock()
	if b.buffered {
		b.pendingC2S = append(b.pendingC2S, pendingC2SMessage{
			ctx: ctx,
			msg: actorMsg,
		})
		b.mu.Unlock()

		return fn.Ok[serverconn.ServerConnResp](
			&serverconn.SendClientEventResponse{
				Success: true,
			},
		)
	}
	b.mu.Unlock()

	// Immediate delivery.
	b.roundsActor.Tell(ctx, actorMsg)

	return fn.Ok[serverconn.ServerConnResp](&serverconn.SendClientEventResponse{
		Success: true,
	})
}

// convertToActorMsg converts a client outbox message to a server actor message.
func (b *BridgeServerConn) convertToActorMsg(
	msg serverconn.ServerMessage) (rounds.ActorMsg, error) {

	switch m := msg.(type) {
	case *clientround.JoinRoundRequest:
		// Convert client's JoinRoundRequest to server's
		// JoinRoundRequest. The client message contains
		// BoardingRequests (by value) and VTXORequests (by value)
		// which need to be converted to pointers for
		// clienttypes.JoinRoundRequest.
		boardingReqs := make([]*clienttypes.BoardingRequest, len(m.BoardingRequests))
		for i := range m.BoardingRequests {
			boardingReqs[i] = &m.BoardingRequests[i]
		}

		vtxoReqs := make([]*clienttypes.VTXORequest, len(m.VTXORequests))
		for i := range m.VTXORequests {
			vtxoReqs[i] = &m.VTXORequests[i]
		}

		// Convert ForfeitRequests to ForfeitReqs. Each forfeit
		// specifies a VTXO to forfeit.
		forfeitReqs := make(
			[]*clienttypes.ForfeitRequest, 0,
			len(m.ForfeitRequests)+len(m.RefreshRequests),
		)
		for _, forfeitReq := range m.ForfeitRequests {
			forfeitReqs = append(forfeitReqs, &clienttypes.ForfeitRequest{
				VTXOOutpoint: &forfeitReq.VTXOOutpoint,
			})
		}

		// Convert RefreshRequests to ForfeitReqs. Each refresh
		// specifies a VTXO to forfeit and a new VTXO to receive.
		// The forfeit request only needs the outpoint; the new VTXO
		// is already included in VTXORequests.
		for _, refreshReq := range m.RefreshRequests {
			forfeitReqs = append(forfeitReqs, &clienttypes.ForfeitRequest{
				VTXOOutpoint: &refreshReq.VTXOOutpoint,
			})
		}

		// Convert LeaveRequests directly. Each leave specifies an
		// on-chain destination output.
		leaveReqs := make(
			[]*clienttypes.LeaveRequest, len(m.LeaveRequests),
		)
		for i, leaveReq := range m.LeaveRequests {
			leaveReqs[i] = &clienttypes.LeaveRequest{
				Output: leaveReq.Output,
			}
		}

		return &rounds.JoinRoundRequest{
			ClientID: b.clientID,
			Request: &clienttypes.JoinRoundRequest{
				BoardingReqs: boardingReqs,
				VTXOReqs:     vtxoReqs,
				ForfeitReqs:  forfeitReqs,
				LeaveReqs:    leaveReqs,
			},
		}, nil

	case *clientround.SubmitNoncesRequest:
		// Convert client's SubmitNoncesRequest to server's RoundMsg with
		// ClientVTXONoncesEvent.
		//
		// Convert nonces from SignerKey to SigningKeyHex (both are
		// [33]byte).
		serverNonces := make(
			map[rounds.SigningKeyHex]map[clienttree.TxID]clienttree.Musig2PubNonce,
		)
		for signerKey, txNonces := range m.Nonces {
			var vertex route.Vertex
			copy(vertex[:], signerKey[:])
			serverNonces[vertex] = txNonces
		}

		return &rounds.RoundMsg{
			RoundID: rounds.RoundID(m.RoundID),
			Event: &rounds.ClientVTXONoncesEvent{
				ClientID: b.clientID,
				Nonces:   serverNonces,
			},
		}, nil

	case *clientround.SubmitPartialSigRequest:
		// Convert client's SubmitPartialSigRequest to server's
		// RoundMsg with ClientVTXOPartialSigsEvent.
		serverSigs := make(
			map[rounds.SigningKeyHex]map[clienttree.TxID]*musig2.PartialSignature,
		)
		for signerKey, txSigs := range m.Signatures {
			var vertex route.Vertex
			copy(vertex[:], signerKey[:])
			serverSigs[vertex] = txSigs
		}

		return &rounds.RoundMsg{
			RoundID: rounds.RoundID(m.RoundID),
			Event: &rounds.ClientVTXOPartialSigsEvent{
				ClientID:   b.clientID,
				Signatures: serverSigs,
			},
		}, nil

	case *clientround.SubmitForfeitSigRequest:
		// Convert client's SubmitForfeitSigRequest to server's
		// RoundMsg with ClientBoardingSignaturesEvent.
		return &rounds.RoundMsg{
			RoundID: rounds.RoundID(m.RoundID),
			Event: &rounds.ClientInputSignaturesEvent{
				ClientID:   b.clientID,
				Signatures: m.Signatures,
			},
		}, nil

	case *clientround.SubmitVTXOForfeitSigsToServer:
		// Convert client's VTXO forfeit signatures to server's
		// RoundMsg with ClientInputSignaturesEvent.ForfeitTxs.
		forfeitTxs := make(
			[]*clienttypes.ForfeitTxSig, 0, len(m.ForfeitSigs),
		)
		for outpoint, sig := range m.ForfeitSigs {
			unsignedTx, ok := m.ForfeitTxs[outpoint]
			if !ok {
				return nil, fmt.Errorf(
					"missing unsigned forfeit tx for "+
						"outpoint %s", outpoint,
				)
			}
			forfeitTxs = append(forfeitTxs, &clienttypes.ForfeitTxSig{
				UnsignedTx:    unsignedTx,
				ClientVTXOSig: sig,
			})
		}

		return &rounds.RoundMsg{
			RoundID: rounds.RoundID(m.RoundID),
			Event: &rounds.ClientInputSignaturesEvent{
				ClientID:   b.clientID,
				ForfeitTxs: forfeitTxs,
			},
		}, nil

	default:
		return nil, fmt.Errorf("unsupported client message type: %T", msg)
	}
}

// pendingS2CMessage holds a buffered server-to-client message.
type pendingS2CMessage struct {
	ctx context.Context
	msg clientround.ClientMsg
}

// BridgeClientConn implements clientconn.ClientConnMsg interface to route
// server outbox messages directly to client actors. This replaces gRPC with
// in-process message routing for tests. All messages are recorded to a shared
// transcript for test assertions.
type BridgeClientConn struct {
	mu sync.RWMutex

	// clients maps client IDs to their actor references.
	clients map[clientconn.ClientID]actor.TellOnlyRef[actormsg.RoundReceivable]

	// eventServers maps client IDs to subscribe.Server instances that
	// broadcast events to subscribers. This enables TestClient to observe
	// events in an event-driven manner without polling.
	eventServers map[clientconn.ClientID]*subscribe.Server

	// transcript records all messages for test assertions.
	transcript *MessageTranscript

	// buffered when true, messages are queued instead of immediately
	// delivered.
	buffered bool

	// pendingS2C holds buffered server-to-client messages per client.
	pendingS2C map[clientconn.ClientID][]pendingS2CMessage
}

// NewBridgeClientConn creates a new bridge client connection.
func NewBridgeClientConn(transcript *MessageTranscript) *BridgeClientConn {
	return &BridgeClientConn{
		clients:      make(map[clientconn.ClientID]actor.TellOnlyRef[actormsg.RoundReceivable]),
		eventServers: make(map[clientconn.ClientID]*subscribe.Server),
		transcript:   transcript,
		pendingS2C:   make(map[clientconn.ClientID][]pendingS2CMessage),
	}
}

// Subscribe returns a subscribe.Client that receives all events delivered to
// the specified client. If no server exists for this client, one is created.
// The caller should call Cancel() when done to unsubscribe.
func (b *BridgeClientConn) Subscribe(
	clientID clientconn.ClientID) (*subscribe.Client, error) {

	b.mu.Lock()
	defer b.mu.Unlock()

	server, ok := b.eventServers[clientID]
	if !ok {
		server = subscribe.NewServer()
		if err := server.Start(); err != nil {
			return nil, fmt.Errorf("start event server: %w", err)
		}
		b.eventServers[clientID] = server
	}

	return server.Subscribe()
}

// WaitForEvent waits for an event matching the predicate to be delivered to
// the specified client. Returns the matching event or an error if timeout
// occurs. The predicate should return true for the event to match.
func (b *BridgeClientConn) WaitForEvent(clientID clientconn.ClientID,
	predicate func(clientround.ClientEvent) bool, timeout time.Duration,
) (clientround.ClientEvent, error) {

	sub, err := b.Subscribe(clientID)
	if err != nil {
		return nil, fmt.Errorf("subscribe: %w", err)
	}
	defer sub.Cancel()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case update := <-sub.Updates():
			event, ok := update.(clientround.ClientEvent)
			if !ok {
				continue
			}

			if predicate(event) {
				return event, nil
			}

		case <-sub.Quit():
			return nil, fmt.Errorf("subscription closed")

		case <-timer.C:
			return nil, fmt.Errorf("timeout waiting for event")
		}
	}
}

// Stop stops all event servers for this bridge.
func (b *BridgeClientConn) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, server := range b.eventServers {
		_ = server.Stop()
	}
	b.eventServers = make(map[clientconn.ClientID]*subscribe.Server)
}

// SetBuffered enables or disables message buffering. When enabled, messages
// are queued instead of immediately delivered to clients.
func (b *BridgeClientConn) SetBuffered(buffered bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.buffered = buffered
}

// PendingCountFor returns the number of buffered server-to-client messages for
// a specific client.
func (b *BridgeClientConn) PendingCountFor(clientID clientconn.ClientID) int {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return len(b.pendingS2C[clientID])
}

// FlushNextFor delivers the next buffered message to a specific client.
// Returns an error if no messages are pending for that client.
func (b *BridgeClientConn) FlushNextFor(clientID clientconn.ClientID) error {
	b.mu.Lock()
	pending := b.pendingS2C[clientID]
	if len(pending) == 0 {
		b.mu.Unlock()

		return fmt.Errorf("no pending messages for %s", clientID)
	}

	msg := pending[0]
	b.pendingS2C[clientID] = pending[1:]
	clientRef := b.clients[clientID]
	b.mu.Unlock()

	if clientRef == nil {
		return fmt.Errorf("client %s not registered", clientID)
	}

	clientRef.Tell(msg.ctx, msg.msg)

	return nil
}

// FlushAllFor delivers all buffered messages to a specific client in order.
func (b *BridgeClientConn) FlushAllFor(clientID clientconn.ClientID) {
	b.mu.Lock()
	pending := b.pendingS2C[clientID]
	b.pendingS2C[clientID] = nil
	clientRef := b.clients[clientID]
	b.mu.Unlock()

	if clientRef == nil {
		return
	}

	for _, msg := range pending {
		clientRef.Tell(msg.ctx, msg.msg)
	}
}

// RegisterClient adds a client actor reference for message routing.
func (b *BridgeClientConn) RegisterClient(id clientconn.ClientID,
	ref actor.TellOnlyRef[actormsg.RoundReceivable]) {

	b.mu.Lock()
	defer b.mu.Unlock()

	b.clients[id] = ref
}

// UnregisterClient removes a client from the bridge, cleaning up all
// associated state including the client reference, event server, and any
// pending messages. This is called when a client is stopped for restart
// testing.
func (b *BridgeClientConn) UnregisterClient(id clientconn.ClientID) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Remove client reference.
	delete(b.clients, id)

	// Stop and remove event server for this client.
	if server, ok := b.eventServers[id]; ok {
		server.Stop()
		delete(b.eventServers, id)
	}

	// Clear any pending messages for this client.
	delete(b.pendingS2C, id)
}

// Receive processes outgoing server messages and routes them to clients.
func (b *BridgeClientConn) Receive(ctx context.Context,
	msg clientconn.ClientConnMsg) fn.Result[clientconn.ClientConnResp] {

	switch m := msg.(type) {
	case *clientconn.SendServerEventRequest:
		return b.handleSendServerEvent(ctx, m)

	default:
		return fn.Err[clientconn.ClientConnResp](fmt.Errorf(
			"unknown message type: %T", msg,
		))
	}
}

// handleSendServerEvent converts a server outbox message to a client actor
// message and routes it to the appropriate client (or buffers it if buffering
// is enabled).
func (b *BridgeClientConn) handleSendServerEvent(ctx context.Context,
	req *clientconn.SendServerEventRequest) fn.Result[clientconn.ClientConnResp] {

	// Get the target client ID from the message.
	targetID := req.Message.ClientID()

	// Record the message to the transcript first.
	b.transcript.Record(ServerToClient, targetID, req.Message)

	// Convert the server outbox message to a client event.
	clientEvent, err := b.convertToClientEvent(req.Message)
	if err != nil {
		return fn.Err[clientconn.ClientConnResp](fmt.Errorf(
			"convert server message: %w", err,
		))
	}

	// Broadcast to any subscribers watching this client's events.
	b.mu.RLock()
	if server, ok := b.eventServers[targetID]; ok {
		// Ignore errors here - subscribers may have quit.
		_ = server.SendUpdate(clientEvent)
	}
	b.mu.RUnlock()

	// Wrap in ServerMessageNotification.
	notification := &clientround.ServerMessageNotification{
		Message: clientEvent,
	}

	// Check if buffering is enabled.
	b.mu.Lock()
	if b.buffered {
		b.pendingS2C[targetID] = append(
			b.pendingS2C[targetID], pendingS2CMessage{
				ctx: ctx,
				msg: notification,
			},
		)
		b.mu.Unlock()

		return fn.Ok[clientconn.ClientConnResp](
			&clientconn.SendClientEventResponse{
				Success: true,
			},
		)
	}

	// Get the client reference for immediate delivery.
	clientRef, ok := b.clients[targetID]
	b.mu.Unlock()

	if !ok {
		return fn.Err[clientconn.ClientConnResp](fmt.Errorf(
			"client %s not registered", targetID,
		))
	}

	// Immediate delivery.
	clientRef.Tell(ctx, notification)

	return fn.Ok[clientconn.ClientConnResp](&clientconn.SendClientEventResponse{
		Success: true,
	})
}

// convertToClientEvent converts a server outbox message to a client event.
func (b *BridgeClientConn) convertToClientEvent(
	msg clientconn.ClientMessage) (clientround.ClientEvent, error) {

	switch m := msg.(type) {
	case *rounds.ClientSuccessResp:
		return &clientround.RoundJoined{
			RoundID:                   clientround.RoundID(m.RoundID),
			AcceptedBoardingOutpoints: m.AcceptedBoardingOutpoints,
			AcceptedVTXOOutpoints:     m.AcceptedVTXOOutpoints,
		}, nil

	case *rounds.ClientBatchInfo:
		// Convert connector leaf map from server format to client format.
		// The client format has additional fields (VTXOAmount, LeafIndex)
		// that are looked up from local VTXO state by the client.
		var forfeitMappings map[wire.OutPoint]*clientround.ConnectorLeafInfo
		if m.ConnectorLeafMap != nil {
			forfeitMappings = make(
				map[wire.OutPoint]*clientround.ConnectorLeafInfo,
				len(m.ConnectorLeafMap),
			)
			for outpoint, info := range m.ConnectorLeafMap {
				forfeitMappings[outpoint] = &clientround.ConnectorLeafInfo{
					ConnectorOutpoint: info.LeafOutpoint,
					ConnectorPkScript: info.LeafOutput.PkScript,
					ConnectorAmount:   info.LeafOutput.Value,
					// VTXOAmount is looked up from client's
					// local VTXO state.
				}
			}
		}

		return &clientround.CommitmentTxBuilt{
			RoundID:         clientround.RoundID(m.RoundID),
			Tx:              m.BatchPSBT,
			VTXOTreePaths:   m.VTXOTreePaths,
			ForfeitMappings: forfeitMappings,
		}, nil

	case *rounds.ClientAwaitingInputSigsResp:
		return &clientround.AwaitingBoardingSigs{
			RoundID: clientround.RoundID(m.RoundID),
		}, nil

	case *rounds.ClientVTXOAggNonces:
		return &clientround.NoncesAggregated{
			RoundID:   clientround.RoundID(m.RoundID),
			AggNonces: m.AggNonces,
		}, nil

	case *rounds.ClientVTXOAggSigs:
		return &clientround.OperatorSigned{
			RoundID: clientround.RoundID(m.RoundID),
			AggSigs: m.AggSigs,
		}, nil

	case *rounds.ClientRoundFailedResp:
		return &clientround.BoardingFailed{
			Reason:      m.Reason,
			Recoverable: true,
		}, nil

	case *rounds.ClientErrorResp:
		return &clientround.BoardingFailed{
			Reason:      m.ErrorMsg,
			Recoverable: true,
		}, nil

	default:
		return nil, fmt.Errorf("unsupported server message type: %T", msg)
	}
}

// ClientIDFromPubKey creates a client ID from a public key.
func ClientIDFromPubKey(pubKey []byte) clientconn.ClientID {
	return clientconn.ClientID(hex.EncodeToString(pubKey))
}
