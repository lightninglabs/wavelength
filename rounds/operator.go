// Server-Side Operator Dispatch Pipeline
//
// The RoundOperator bridges the mailbox transport layer (clientconn) and the
// rounds actor FSM. It participates in a multi-layer dispatch pipeline:
//
//	Mailbox Envelope (from client)
//	   │
//	   ▼
//	clientconn Ingress Loop
//	   │  Routes by {Service, Method} key from the envelope's RpcMeta
//	   ▼
//	EnvelopeDispatcher (from makeDispatcher)
//	   │  1. Validates envelope structure
//	   │  2. Injects env.Sender as ClientID into context
//	   │  3. Calls ServeMux.ServeRPC(service, method, body)
//	   ▼
//	ServeMux (mailboxrpc.ServeMux)
//	   │  Deserializes raw bytes into typed proto request via
//	   │  the handler registered by RegisterRoundServiceMailboxServer
//	   ▼
//	Typed Handler Method (e.g. JoinRound, SubmitNonces)
//	   │  1. Extracts client ID from context
//	   │  2. Converts proto request → domain types
//	   │  3. Forwards to rounds actor via Tell/Ask
//	   ▼
//	Rounds Actor FSM
//
// Wiring: During server startup, setupRoundsSubsystem creates the
// RoundOperator and registers it on a ServeMux via the generated
// RegisterRoundServiceMailboxServer(mux, op). The operator's Dispatchers()
// method returns a DispatcherMap keyed by {Service, Method}. These
// dispatchers are merged with other operators' dispatchers (indexer, OOR)
// in RegisterClientWithAllDispatchers and installed on each per-client
// ingress loop via PerClientConfig.Dispatchers.
//
// Response path: After the handler completes, makeDispatcher builds a
// KIND_RESPONSE envelope with the handler's result (or error headers),
// and sends it back to the client via Edge.Send. The response carries
// the original CorrelationId so the client can match it to its request.
//
// See docs/dispatch_pipeline.md for the full pipeline reference and
// docs/clientconn_architecture.md for the underlying transport layer.

package rounds

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/taproot-assets/proof"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	"github.com/lightninglabs/darepo-client/rpc/roundpb"
	"github.com/lightninglabs/darepo/clientconn"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/routing/route"
	"google.golang.org/protobuf/types/known/anypb"
)

const (
	// roundServiceName is the protobuf service name used in mailbox
	// envelope routing for the round RPC service.
	roundServiceName = roundpb.ServiceName

	// operatorSenderMailboxID is the server identity stamped on
	// response and event envelopes sent by the round operator.
	operatorSenderMailboxID = "svc:rounds"

	// responseMsgPrefix prefixes mailbox response envelope IDs.
	responseMsgPrefix = "resp-"
)

// clientIDContextKey is the context key used to inject the client's
// mailbox ID into the handler context. The dispatcher extracts the
// sender from the envelope and stores it so handler methods can
// retrieve it via clientIDFromContext.
type clientIDContextKey struct{}

// RoundOperatorConfig holds dependencies for the rounds operator
// dispatcher factory.
type RoundOperatorConfig struct {
	// Edge is the mailbox client for sending response envelopes
	// back to clients.
	Edge mailboxpb.MailboxServiceClient

	// SenderMailboxID is the identity stamped on response
	// envelopes. Defaults to operatorSenderMailboxID.
	SenderMailboxID string

	// RoundsRef is the actor reference for the rounds actor. The
	// operator sends JoinRoundRequest and other actor messages via
	// Tell (fire-and-forget) because the round FSM delivers
	// responses asynchronously through outbox events.
	RoundsRef actor.ActorRef[ActorMsg, ActorResp]
}

// RoundOperator provides RoundService RPC dispatchers for the
// per-client clientconn ingress loops. Unlike the indexer operator
// which uses synchronous ServeMux dispatch, the round operator
// translates inbound mailbox envelopes into actor messages and fires
// them at the rounds actor via Tell. Client responses flow back
// asynchronously through the outbox event path (bridge → per-client
// DurableActor → client mailbox).
//
// For request methods (JoinRound, SubmitNonces, etc.), the dispatcher
// sends an immediate KIND_RESPONSE acknowledgment so the client's
// mailbox cursor can advance, while the actual result arrives later as
// a push event.
type RoundOperator struct {
	cfg RoundOperatorConfig
	mux *mailboxrpc.ServeMux
}

// NewRoundOperator creates a new round operator and registers the
// round RPC handlers on a ServeMux for request deserialization.
func NewRoundOperator(
	cfg RoundOperatorConfig) (*RoundOperator, error) {

	if cfg.Edge == nil {
		return nil, fmt.Errorf("edge is required")
	}
	if cfg.SenderMailboxID == "" {
		cfg.SenderMailboxID = operatorSenderMailboxID
	}

	// Register the round service handlers for proto
	// deserialization. The handler implementations are thin shims
	// that convert proto requests to actor messages.
	mux := mailboxrpc.NewServeMux()
	op := &RoundOperator{cfg: cfg, mux: mux}

	roundpb.RegisterRoundServiceMailboxServer(mux, op)

	return op, nil
}

// Dispatchers returns the EnvelopeDispatcher map for all
// RoundService RPC methods. Each dispatcher follows the same
// pattern as the indexer operator: extract principal, process via
// ServeMux, build response envelope, send via edge.
func (o *RoundOperator) Dispatchers() clientconn.DispatcherMap {
	methods := []string{
		"JoinRound",
		"SubmitNonces",
		"SubmitPartialSigs",
		"SubmitForfeitSigs",
		"SubmitVTXOForfeitSigs",
	}

	dm := make(clientconn.DispatcherMap, len(methods))
	for _, method := range methods {
		key := mailboxrpc.ServiceMethod{
			Service: roundServiceName,
			Method:  method,
		}

		dm[key] = o.makeDispatcher(method)
	}

	return dm
}

// makeDispatcher creates an EnvelopeDispatcher closure for a single
// RPC method. The closure captures the operator's mux and edge for
// processing and responding. It injects the envelope sender as the
// client ID into the context so handler methods can retrieve it.
func (o *RoundOperator) makeDispatcher(
	method string) clientconn.EnvelopeDispatcher {

	return func(ctx context.Context,
		env *mailboxpb.Envelope) error {

		if env == nil || env.Rpc == nil {
			return nil
		}
		if env.Body == nil {
			return fmt.Errorf("missing request body")
		}

		// Inject the envelope sender as the client ID into
		// the context. Handler methods retrieve this via
		// clientIDFromContext.
		ctx = context.WithValue(
			ctx, clientIDContextKey{},
			clientconn.ClientID(env.Sender),
		)

		// Process the request through the ServeMux. The mux
		// deserializes the proto and calls our handler impl
		// (JoinRound, SubmitNonces, etc.).
		resp, handlerErr := o.mux.ServeRPC(
			ctx, env.Rpc.Service, method,
			env.Body.Value,
		)

		// Determine where to send the response.
		replyTo := env.Rpc.ReplyTo
		if replyTo == "" {
			replyTo = env.Sender
		}

		responseEnv := &mailboxpb.Envelope{
			ProtocolVersion: env.ProtocolVersion,
			MsgId:           responseMsgPrefix + env.MsgId,
			Sender:          o.cfg.SenderMailboxID,
			Recipient:       replyTo,
			CreatedAtUnixMs: time.Now().UnixMilli(),
			Headers: mailboxrpc.EncodeErrorHeaders(
				handlerErr,
			),
			Rpc: &mailboxpb.RpcMeta{
				Kind:          mailboxpb.RpcMeta_KIND_RESPONSE,
				Service:       env.Rpc.Service,
				Method:        method,
				CorrelationId: env.Rpc.CorrelationId,
			},
		}

		if handlerErr == nil && resp != nil {
			respAny, err := anypb.New(resp)
			if err != nil {
				return fmt.Errorf(
					"marshal response: %w", err,
				)
			}

			responseEnv.Body = respAny
		}

		sendResp, err := o.cfg.Edge.Send(
			ctx, &mailboxpb.SendRequest{
				Envelope: responseEnv,
			},
		)
		if err != nil {
			return fmt.Errorf("send response: %w", err)
		}
		if sendResp.Status != nil && !sendResp.Status.Ok {
			return fmt.Errorf(
				"send response status: %s (%s)",
				sendResp.Status.Message,
				sendResp.Status.Code,
			)
		}

		return nil
	}
}

// JoinRound implements roundpb.RoundServiceMailboxServer. It converts
// the proto request to a domain JoinRoundRequest and fires it at the
// rounds actor. The actual client response arrives asynchronously via
// the outbox event path.
func (o *RoundOperator) JoinRound(ctx context.Context,
	req *roundpb.JoinRoundRequest) (
	*roundpb.ClientSuccessResp, error) {

	domainReq, err := joinRoundRequestFromProto(req)
	if err != nil {
		return nil, fmt.Errorf("parse join request: %w", err)
	}

	clientID := clientIDFromContext(ctx)

	actorMsg := &JoinRoundRequest{
		ClientID: clientID,
		Request:  domainReq,
	}

	tellErr := o.cfg.RoundsRef.Tell(ctx, actorMsg)
	if tellErr != nil {
		return nil, fmt.Errorf(
			"tell rounds actor: %w", tellErr,
		)
	}

	// Return an empty success response as acknowledgment. The
	// real response (with round ID and accepted outpoints) arrives
	// via the ClientSuccessResp outbox event through the bridge.
	return &roundpb.ClientSuccessResp{}, nil
}

// SubmitNonces implements roundpb.RoundServiceMailboxServer. It
// converts the proto nonce submission into a ClientVTXONoncesEvent
// and forwards it to the rounds actor for the specified round.
func (o *RoundOperator) SubmitNonces(ctx context.Context,
	req *roundpb.SubmitNoncesRequest) (
	*roundpb.ClientVTXOAggNonces, error) {

	roundID, err := parseRoundID(req.GetRoundId())
	if err != nil {
		return nil, fmt.Errorf("parse round_id: %w", err)
	}

	// Convert the proto nonce map into the domain type:
	// map[SigningKeyHex]map[tree.TxID]tree.Musig2PubNonce.
	nonces, err := noncesFromProto(req.GetNonces())
	if err != nil {
		return nil, fmt.Errorf("parse nonces: %w", err)
	}

	clientID := clientIDFromContext(ctx)

	tellErr := o.cfg.RoundsRef.Tell(ctx, &RoundMsg{
		RoundID: roundID,
		Event: &ClientVTXONoncesEvent{
			ClientID: clientID,
			Nonces:   nonces,
		},
	})
	if tellErr != nil {
		return nil, fmt.Errorf(
			"tell rounds actor: %w", tellErr,
		)
	}

	// Return an empty ack. The aggregated nonces arrive via the
	// outbox event path.
	return &roundpb.ClientVTXOAggNonces{}, nil
}

// SubmitPartialSigs implements roundpb.RoundServiceMailboxServer. It
// converts the proto partial signature submission into a
// ClientVTXOPartialSigsEvent and forwards it to the rounds actor.
func (o *RoundOperator) SubmitPartialSigs(ctx context.Context,
	req *roundpb.SubmitPartialSigRequest) (
	*roundpb.ClientVTXOAggSigs, error) {

	roundID, err := parseRoundID(req.GetRoundId())
	if err != nil {
		return nil, fmt.Errorf("parse round_id: %w", err)
	}

	// Convert the proto signature map into the domain type:
	// map[SigningKeyHex]map[tree.TxID]*musig2.PartialSignature.
	sigs, err := partialSigsFromProto(req.GetSignatures())
	if err != nil {
		return nil, fmt.Errorf("parse signatures: %w", err)
	}

	clientID := clientIDFromContext(ctx)

	tellErr := o.cfg.RoundsRef.Tell(ctx, &RoundMsg{
		RoundID: roundID,
		Event: &ClientVTXOPartialSigsEvent{
			ClientID:   clientID,
			Signatures: sigs,
		},
	})
	if tellErr != nil {
		return nil, fmt.Errorf(
			"tell rounds actor: %w", tellErr,
		)
	}

	// Return an empty ack. The aggregated signatures arrive via
	// the outbox event path.
	return &roundpb.ClientVTXOAggSigs{}, nil
}

// SubmitForfeitSigs implements roundpb.RoundServiceMailboxServer. It
// converts the proto boarding input signatures into a
// ClientInputSignaturesEvent and forwards it to the rounds actor.
func (o *RoundOperator) SubmitForfeitSigs(ctx context.Context,
	req *roundpb.SubmitForfeitSigRequest) (
	*roundpb.ClientAwaitingInputSigsResp, error) {

	roundID, err := parseRoundID(req.GetRoundId())
	if err != nil {
		return nil, fmt.Errorf("parse round_id: %w", err)
	}

	// Convert proto boarding input signatures to domain type.
	boardingSigs, err := boardingInputSigsFromProto(
		req.GetSignatures(),
	)
	if err != nil {
		return nil, fmt.Errorf(
			"parse boarding signatures: %w", err,
		)
	}

	clientID := clientIDFromContext(ctx)

	tellErr := o.cfg.RoundsRef.Tell(ctx, &RoundMsg{
		RoundID: roundID,
		Event: &ClientInputSignaturesEvent{
			ClientID:   clientID,
			Signatures: boardingSigs,
		},
	})
	if tellErr != nil {
		return nil, fmt.Errorf(
			"tell rounds actor: %w", tellErr,
		)
	}

	return &roundpb.ClientAwaitingInputSigsResp{}, nil
}

// SubmitVTXOForfeitSigs implements
// roundpb.RoundServiceMailboxServer. It converts the proto VTXO
// forfeit signatures into a ClientInputSignaturesEvent (with the
// ForfeitTxs field populated) and forwards it to the rounds actor.
func (o *RoundOperator) SubmitVTXOForfeitSigs(ctx context.Context,
	req *roundpb.SubmitVTXOForfeitSigsRequest) (
	*roundpb.ClientSuccessResp, error) {

	roundID, err := parseRoundID(req.GetRoundId())
	if err != nil {
		return nil, fmt.Errorf("parse round_id: %w", err)
	}

	// Convert proto forfeit tx signatures to domain type.
	forfeitTxs, err := forfeitTxSigsFromProto(
		req.GetForfeitTxs(),
	)
	if err != nil {
		return nil, fmt.Errorf(
			"parse forfeit signatures: %w", err,
		)
	}

	clientID := clientIDFromContext(ctx)

	tellErr := o.cfg.RoundsRef.Tell(ctx, &RoundMsg{
		RoundID: roundID,
		Event: &ClientInputSignaturesEvent{
			ClientID:   clientID,
			ForfeitTxs: forfeitTxs,
		},
	})
	if tellErr != nil {
		return nil, fmt.Errorf(
			"tell rounds actor: %w", tellErr,
		)
	}

	return &roundpb.ClientSuccessResp{}, nil
}

// joinRoundRequestFromProto converts a roundpb.JoinRoundRequest to
// the domain types.JoinRoundRequest. Each sub-request type (boarding,
// VTXO, forfeit, leave) is converted using the roundpb helper
// functions for outpoints, public keys, and transaction outputs.
func joinRoundRequestFromProto(
	req *roundpb.JoinRoundRequest) (*types.JoinRoundRequest, error) {

	// Parse the participant identifier (33-byte compressed
	// public key).
	identifier, err := btcec.ParsePubKey(req.GetIdentifier())
	if err != nil {
		return nil, fmt.Errorf(
			"parse identifier pubkey: %w", err,
		)
	}

	// Convert boarding requests.
	boardingReqs := make(
		[]*types.BoardingRequest,
		0, len(req.GetBoardingRequests()),
	)
	for i, br := range req.GetBoardingRequests() {
		domainBR, err := boardingRequestFromProto(br)
		if err != nil {
			return nil, fmt.Errorf(
				"boarding_request[%d]: %w", i, err,
			)
		}

		boardingReqs = append(boardingReqs, domainBR)
	}

	// Convert VTXO requests.
	vtxoReqs := make(
		[]*types.VTXORequest,
		0, len(req.GetVtxoRequests()),
	)
	for i, vr := range req.GetVtxoRequests() {
		domainVR, err := vtxoRequestFromProto(vr)
		if err != nil {
			return nil, fmt.Errorf(
				"vtxo_request[%d]: %w", i, err,
			)
		}

		vtxoReqs = append(vtxoReqs, domainVR)
	}

	// Convert forfeit requests.
	forfeitReqs := make(
		[]*types.ForfeitRequest,
		0, len(req.GetForfeitRequests()),
	)
	for i, fr := range req.GetForfeitRequests() {
		op, err := roundpb.OutpointFromProto(
			fr.GetVtxoOutpoint(),
		)
		if err != nil {
			return nil, fmt.Errorf(
				"forfeit_request[%d]: %w", i, err,
			)
		}

		forfeitReqs = append(forfeitReqs, &types.ForfeitRequest{
			VTXOOutpoint: &op,
		})
	}

	// Convert leave requests.
	leaveReqs := make(
		[]*types.LeaveRequest,
		0, len(req.GetLeaveRequests()),
	)
	for i, lr := range req.GetLeaveRequests() {
		txOut, err := roundpb.TxOutFromProto(lr.GetOutput())
		if err != nil {
			return nil, fmt.Errorf(
				"leave_request[%d]: %w", i, err,
			)
		}

		leaveReqs = append(leaveReqs, &types.LeaveRequest{
			Output: txOut,
		})
	}

	// Convert auth payload if present.
	var auth *types.JoinRoundAuth
	if req.GetAuth() != nil {
		auth = &types.JoinRoundAuth{
			Message:    req.Auth.GetMessage(),
			ValidFrom:  req.Auth.GetValidFrom(),
			ValidUntil: req.Auth.GetValidUntil(),
			Signature:  req.Auth.GetSignature(),
		}
	}

	return &types.JoinRoundRequest{
		Identifier:   identifier,
		VTXOReqs:     vtxoReqs,
		BoardingReqs: boardingReqs,
		LeaveReqs:    leaveReqs,
		ForfeitReqs:  forfeitReqs,
		Auth:         auth,
	}, nil
}

// boardingRequestFromProto converts a single proto BoardingRequest
// to the domain types.BoardingRequest. The TxProof field is left
// as None since the server verifies boarding UTXOs via its own
// chain source.
func boardingRequestFromProto(
	br *roundpb.BoardingRequest) (*types.BoardingRequest, error) {

	op, err := roundpb.OutpointFromProto(br.GetOutpoint())
	if err != nil {
		return nil, fmt.Errorf("outpoint: %w", err)
	}

	clientKey, err := btcec.ParsePubKey(br.GetClientKey())
	if err != nil {
		return nil, fmt.Errorf("client_key: %w", err)
	}

	operatorKey, err := btcec.ParsePubKey(br.GetOperatorKey())
	if err != nil {
		return nil, fmt.Errorf("operator_key: %w", err)
	}

	return &types.BoardingRequest{
		Outpoint:    &op,
		ClientKey:   clientKey,
		OperatorKey: operatorKey,
		ExitDelay:   br.GetExitDelay(),
		TxProof:     fn.None[proof.TxProof](),
	}, nil
}

// vtxoRequestFromProto converts a single proto VTXORequest to the
// domain types.VTXORequest. The SigningKey field is left zero since
// it is a client-side concern used for MuSig2 key locators.
func vtxoRequestFromProto(
	vr *roundpb.VTXORequest) (*types.VTXORequest, error) {

	clientKey, err := btcec.ParsePubKey(vr.GetClientKey())
	if err != nil {
		return nil, fmt.Errorf("client_key: %w", err)
	}

	operatorKey, err := btcec.ParsePubKey(vr.GetOperatorKey())
	if err != nil {
		return nil, fmt.Errorf("operator_key: %w", err)
	}

	return &types.VTXORequest{
		Amount:      btcutil.Amount(vr.GetAmount()),
		PkScript:    vr.GetPkScript(),
		Expiry:      vr.GetExpiry(),
		ClientKey:   clientKey,
		OperatorKey: operatorKey,
	}, nil
}

// parseRoundID parses a 16-byte UUID from the proto round_id field
// into a RoundID.
func parseRoundID(raw []byte) (RoundID, error) {
	if len(raw) != 16 {
		return RoundID{}, fmt.Errorf(
			"invalid round_id length: %d, want 16",
			len(raw),
		)
	}

	var id RoundID
	copy(id[:], raw)

	return id, nil
}

// noncesFromProto converts the proto nonce map into the domain
// representation used by ClientVTXONoncesEvent. The outer map key
// is a signing key hex string (33-byte compressed pubkey), and the
// inner map key is a transaction ID.
func noncesFromProto(
	protoNonces map[string]*roundpb.SignerNonces) (
	map[SigningKeyHex]map[tree.TxID]tree.Musig2PubNonce, error) {

	result := make(
		map[SigningKeyHex]map[tree.TxID]tree.Musig2PubNonce,
		len(protoNonces),
	)

	for keyHex, sn := range protoNonces {
		signingKey, err := route.NewVertexFromStr(keyHex)
		if err != nil {
			return nil, fmt.Errorf(
				"signing key %q: %w", keyHex, err,
			)
		}

		txNonces := make(
			map[tree.TxID]tree.Musig2PubNonce,
			len(sn.GetTxNonces()),
		)
		for txIDHex, nonceBytes := range sn.GetTxNonces() {
			txID, err := roundpb.TxIDFromHex(txIDHex)
			if err != nil {
				return nil, fmt.Errorf(
					"tx_id %q: %w",
					txIDHex, err,
				)
			}

			if len(nonceBytes) != musig2.PubNonceSize {
				return nil, fmt.Errorf(
					"nonce for tx %s: want %d "+
						"bytes, got %d",
					txIDHex,
					musig2.PubNonceSize,
					len(nonceBytes),
				)
			}

			var nonce tree.Musig2PubNonce
			copy(nonce[:], nonceBytes)

			txNonces[txID] = nonce
		}

		result[signingKey] = txNonces
	}

	return result, nil
}

// partialSigsFromProto converts the proto partial signature map into
// the domain representation used by ClientVTXOPartialSigsEvent.
func partialSigsFromProto(
	protoSigs map[string]*roundpb.SignerPartialSigs) (
	map[SigningKeyHex]map[tree.TxID]*musig2.PartialSignature,
	error) {

	result := make(
		map[SigningKeyHex]map[tree.TxID]*musig2.PartialSignature,
		len(protoSigs),
	)

	for keyHex, sp := range protoSigs {
		signingKey, err := route.NewVertexFromStr(keyHex)
		if err != nil {
			return nil, fmt.Errorf(
				"signing key %q: %w", keyHex, err,
			)
		}

		txSigs := make(
			map[tree.TxID]*musig2.PartialSignature,
			len(sp.GetTxSigs()),
		)
		for txIDHex, sigBytes := range sp.GetTxSigs() {
			txID, err := roundpb.TxIDFromHex(txIDHex)
			if err != nil {
				return nil, fmt.Errorf(
					"tx_id %q: %w",
					txIDHex, err,
				)
			}

			var pSig musig2.PartialSignature
			err = pSig.Decode(
				bytes.NewReader(sigBytes),
			)
			if err != nil {
				return nil, fmt.Errorf(
					"partial sig for tx %s: %w",
					txIDHex, err,
				)
			}

			txSigs[txID] = &pSig
		}

		result[signingKey] = txSigs
	}

	return result, nil
}

// boardingInputSigsFromProto converts proto BoardingInputSignature
// entries to the domain types.BoardingInputSignature slice.
func boardingInputSigsFromProto(
	pbSigs []*roundpb.BoardingInputSignature) (
	[]*types.BoardingInputSignature, error) {

	sigs := make(
		[]*types.BoardingInputSignature, 0, len(pbSigs),
	)

	for i, pb := range pbSigs {
		op, err := roundpb.OutpointFromProto(
			pb.GetOutpoint(),
		)
		if err != nil {
			return nil, fmt.Errorf(
				"signature[%d] outpoint: %w", i, err,
			)
		}

		clientSig, err := roundpb.SchnorrSigFromBytes(
			pb.GetClientSignature(),
		)
		if err != nil {
			return nil, fmt.Errorf(
				"signature[%d] client_sig: %w", i, err,
			)
		}

		sigs = append(sigs, &types.BoardingInputSignature{
			InputIndex:      int(pb.GetInputIndex()),
			Outpoint:        op,
			ClientSignature: clientSig,
		})
	}

	return sigs, nil
}

// forfeitTxSigsFromProto converts proto ForfeitTxSig entries to
// the domain types.ForfeitTxSig slice.
func forfeitTxSigsFromProto(
	pbSigs []*roundpb.ForfeitTxSig) (
	[]*types.ForfeitTxSig, error) {

	sigs := make([]*types.ForfeitTxSig, 0, len(pbSigs))

	for i, pb := range pbSigs {
		unsignedTx, err := roundpb.MsgTxFromBytes(
			pb.GetUnsignedTx(),
		)
		if err != nil {
			return nil, fmt.Errorf(
				"forfeit_tx[%d] unsigned_tx: %w",
				i, err,
			)
		}

		vtxoSig, err := roundpb.SchnorrSigFromBytes(
			pb.GetClientVtxoSig(),
		)
		if err != nil {
			return nil, fmt.Errorf(
				"forfeit_tx[%d] client_vtxo_sig: %w",
				i, err,
			)
		}

		sigs = append(sigs, &types.ForfeitTxSig{
			UnsignedTx:    unsignedTx,
			ClientVTXOSig: vtxoSig,
		})
	}

	return sigs, nil
}

// clientIDFromContext extracts the client ID from the context. The
// dispatcher injects the envelope sender as a context value before
// calling the handler method.
func clientIDFromContext(ctx context.Context) clientconn.ClientID {
	id, _ := ctx.Value(
		clientIDContextKey{},
	).(clientconn.ClientID)

	return id
}

// Compile-time check that RoundOperator implements the mailbox server
// interface.
var _ roundpb.RoundServiceMailboxServer = (*RoundOperator)(nil)

// Compile-time check that all client-facing outbox events satisfy
// ClientMessage for bridge delivery.
var _ clientconn.ClientMessage = (*ClientErrorResp)(nil)
var _ clientconn.ClientMessage = (*ClientSuccessResp)(nil)
var _ clientconn.ClientMessage = (*ClientAwaitingInputSigsResp)(nil)
var _ clientconn.ClientMessage = (*ClientVTXOAggNonces)(nil)
var _ clientconn.ClientMessage = (*ClientVTXOAggSigs)(nil)
var _ clientconn.ClientMessage = (*ClientBatchInfo)(nil)
var _ clientconn.ClientMessage = (*ClientRoundFailedResp)(nil)

// Ensure unused imports compile. The wire package is used in
// forfeitTxSigsFromProto via roundpb.MsgTxFromBytes which returns
// *wire.MsgTx.
var _ = (*wire.MsgTx)(nil)
