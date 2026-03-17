//go:build systest

package systest

import (
	"fmt"

	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	clientoor "github.com/lightninglabs/darepo-client/oor"
	clientround "github.com/lightninglabs/darepo-client/round"
	"github.com/lightninglabs/darepo-client/rpc/oorpb"
	"github.com/lightninglabs/darepo-client/rpc/roundpb"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/lightninglabs/darepo/rounds"
	"google.golang.org/protobuf/proto"
)

const (
	// indexerServiceName is the ArkService name used for
	// indexer push notifications.
	indexerServiceName = "arkrpc.ArkService"

	// indexerMethodIncomingOOR is the method name for incoming
	// OOR transfer notifications.
	indexerMethodIncomingOOR = "IncomingOOR"
)

// registerClientRoundRoutes registers server→client event routes on
// the client-side EventRouter. Each route deserializes the proto
// event body, converts it to the client-side domain type via
// FromProto, wraps it in a ServerMessageNotification, and tells the
// client round actor.
//
// These routes correspond to the 7 server outbox event types that
// the bridge's convertToClientEvent() type switch previously handled.
func registerClientRoundRoutes(router *serverconn.EventRouter,
	roundKey actor.ServiceKey[
		actormsg.RoundReceivable, actormsg.RoundActorResp,
	]) {

	svc := roundpb.ServiceName

	// ClientSuccessResp → RoundJoined.
	serverconn.AddRoute(
		router,
		serverconn.EventRouteConfig[
			actormsg.RoundReceivable,
			actormsg.RoundActorResp,
		]{
			Service: svc,
			Method:  rounds.MethodClientSuccessResp,
			NewEvent: func() proto.Message {
				return &roundpb.ClientSuccessResp{}
			},
			Key: roundKey,
			Adapt: func(p proto.Message) (
				actormsg.RoundReceivable, error) {

				event := &clientround.RoundJoined{}
				if err := event.FromProto(p); err != nil {
					return nil, err
				}

				return &clientround.ServerMessageNotification{
					Message: event,
				}, nil
			},
		},
	)

	// ClientBatchInfo → CommitmentTxBuilt.
	serverconn.AddRoute(
		router,
		serverconn.EventRouteConfig[
			actormsg.RoundReceivable,
			actormsg.RoundActorResp,
		]{
			Service: svc,
			Method:  rounds.MethodClientBatchInfo,
			NewEvent: func() proto.Message {
				return &roundpb.ClientBatchInfo{}
			},
			Key: roundKey,
			Adapt: func(p proto.Message) (
				actormsg.RoundReceivable, error) {

				event := &clientround.CommitmentTxBuilt{}
				if err := event.FromProto(p); err != nil {
					return nil, err
				}

				return &clientround.ServerMessageNotification{
					Message: event,
				}, nil
			},
		},
	)

	// ClientAwaitingInputSigsResp → AwaitingBoardingSigs.
	serverconn.AddRoute(
		router,
		serverconn.EventRouteConfig[
			actormsg.RoundReceivable,
			actormsg.RoundActorResp,
		]{
			Service: svc,
			Method:  rounds.MethodClientAwaitingInputSigsResp,
			NewEvent: func() proto.Message {
				return &roundpb.ClientAwaitingInputSigsResp{}
			},
			Key: roundKey,
			Adapt: func(p proto.Message) (
				actormsg.RoundReceivable, error) {

				event := &clientround.AwaitingBoardingSigs{}
				if err := event.FromProto(p); err != nil {
					return nil, err
				}

				return &clientround.ServerMessageNotification{
					Message: event,
				}, nil
			},
		},
	)

	// ClientVTXOAggNonces → NoncesAggregated.
	serverconn.AddRoute(
		router,
		serverconn.EventRouteConfig[
			actormsg.RoundReceivable,
			actormsg.RoundActorResp,
		]{
			Service: svc,
			Method:  rounds.MethodClientVTXOAggNonces,
			NewEvent: func() proto.Message {
				return &roundpb.ClientVTXOAggNonces{}
			},
			Key: roundKey,
			Adapt: func(p proto.Message) (
				actormsg.RoundReceivable, error) {

				event := &clientround.NoncesAggregated{}
				if err := event.FromProto(p); err != nil {
					return nil, err
				}

				return &clientround.ServerMessageNotification{
					Message: event,
				}, nil
			},
		},
	)

	// ClientVTXOAggSigs → OperatorSigned.
	serverconn.AddRoute(
		router,
		serverconn.EventRouteConfig[
			actormsg.RoundReceivable,
			actormsg.RoundActorResp,
		]{
			Service: svc,
			Method:  rounds.MethodClientVTXOAggSigs,
			NewEvent: func() proto.Message {
				return &roundpb.ClientVTXOAggSigs{}
			},
			Key: roundKey,
			Adapt: func(p proto.Message) (
				actormsg.RoundReceivable, error) {

				event := &clientround.OperatorSigned{}
				if err := event.FromProto(p); err != nil {
					return nil, err
				}

				return &clientround.ServerMessageNotification{
					Message: event,
				}, nil
			},
		},
	)

	// ClientRoundFailedResp → BoardingFailed.
	serverconn.AddRoute(
		router,
		serverconn.EventRouteConfig[
			actormsg.RoundReceivable,
			actormsg.RoundActorResp,
		]{
			Service: svc,
			Method:  rounds.MethodClientRoundFailedResp,
			NewEvent: func() proto.Message {
				return &roundpb.ClientRoundFailedResp{}
			},
			Key: roundKey,
			Adapt: func(p proto.Message) (
				actormsg.RoundReceivable, error) {

				event := &clientround.BoardingFailed{}
				if err := event.FromProto(p); err != nil {
					return nil, err
				}

				return &clientround.ServerMessageNotification{
					Message: event,
				}, nil
			},
		},
	)

	// ClientErrorResp → BoardingFailed.
	serverconn.AddRoute(
		router,
		serverconn.EventRouteConfig[
			actormsg.RoundReceivable,
			actormsg.RoundActorResp,
		]{
			Service: svc,
			Method:  rounds.MethodClientErrorResp,
			NewEvent: func() proto.Message {
				return &roundpb.ClientErrorResp{}
			},
			Key: roundKey,
			Adapt: func(p proto.Message) (
				actormsg.RoundReceivable, error) {

				event := &clientround.BoardingFailed{}
				if err := event.FromProto(p); err != nil {
					return nil, err
				}

				return &clientround.ServerMessageNotification{
					Message: event,
				}, nil
			},
		},
	)
}

// registerClientOORRoutes registers server→client OOR response routes
// on the client-side EventRouter. These routes handle async responses
// from the server OOR actor, converting them to DriveEventRequest
// messages that advance the client OOR FSM.
//
// The indexer client is used for the IncomingOOR route to query the
// server for full Ark PSBT + checkpoint data when a lightweight
// incoming transfer notification is received.
//
// This mirrors the production wiring in darepod/server.go's
// registerOOREventRoutes.
func registerClientOORRoutes(router *serverconn.EventRouter,
	oorKey actor.ServiceKey[
		clientoor.OORDurableMsg, clientoor.ActorResp,
	]) {

	// SubmitPackage response: server accepted the submit and
	// returned co-signed checkpoint PSBTs.
	serverconn.AddRoute(
		router,
		serverconn.EventRouteConfig[
			clientoor.OORDurableMsg, clientoor.ActorResp,
		]{
			Service: oorpb.ServiceName,
			Method:  oorpb.MethodSubmitPackage,
			NewEvent: func() proto.Message {
				return &oorpb.SubmitPackageResponse{}
			},
			Key: oorKey,
			Adapt: func(p proto.Message) (
				clientoor.OORDurableMsg, error) {

				resp, ok := p.(*oorpb.SubmitPackageResponse)
				if !ok {
					return nil, fmt.Errorf(
						"expected SubmitPackageResponse"+
							", got %T", p,
					)
				}

				sessionID, checkpoints, err :=
					oorpb.ParseSubmitPackageResponse(
						resp,
					)
				if err != nil {
					return nil, fmt.Errorf(
						"parse submit response: %w",
						err,
					)
				}

				return &clientoor.DriveEventRequest{
					SessionID: clientoor.SessionID(
						sessionID,
					),
					Event: &clientoor.SubmitAcceptedEvent{
						SessionID: clientoor.SessionID(
							sessionID,
						),
						CoSignedCheckpointPSBTs: checkpoints,
					},
				}, nil
			},
		},
	)

	// FinalizePackage response: server accepted the finalize.
	serverconn.AddRoute(
		router,
		serverconn.EventRouteConfig[
			clientoor.OORDurableMsg, clientoor.ActorResp,
		]{
			Service: oorpb.ServiceName,
			Method:  oorpb.MethodFinalizePackage,
			NewEvent: func() proto.Message {
				return &oorpb.FinalizePackageResponse{}
			},
			Key: oorKey,
			Adapt: func(p proto.Message) (
				clientoor.OORDurableMsg, error) {

				resp, ok := p.(*oorpb.FinalizePackageResponse)
				if !ok {
					return nil, fmt.Errorf(
						"expected FinalizePackageResponse"+
							", got %T", p,
					)
				}

				sessionID, err :=
					oorpb.ParseFinalizePackageResponse(
						resp,
					)
				if err != nil {
					return nil, fmt.Errorf(
						"parse finalize response: %w",
						err,
					)
				}

				return &clientoor.DriveEventRequest{
					SessionID: clientoor.SessionID(
						sessionID,
					),
					Event: &clientoor.FinalizeAcceptedEvent{},
				}, nil
			},
		},
	)

	// ListVTXOsByScripts response: server returned authoritative incoming
	// metadata for a durable OOR receive session query.
	serverconn.AddEnvelopeRoute(
		router,
		serverconn.EnvelopeRouteConfig[
			clientoor.OORDurableMsg, clientoor.ActorResp,
		]{
			Service: "arkrpc.IndexerService",
			Method:  "ListVTXOsByScripts",
			NewEvent: func() proto.Message {
				return &arkrpc.ListVTXOsByScriptsResponse{}
			},
			Key: oorKey,
			Adapt: func(env *mailboxpb.Envelope,
				p proto.Message) (
				clientoor.OORDurableMsg, error) {

				if env == nil || env.Rpc == nil {
					return nil, fmt.Errorf("incoming metadata " +
						"response envelope must be provided")
				}

				sessionID, err :=
					clientoor.ParseIncomingMetadataCorrelationID(
						env.Rpc.CorrelationId,
					)
				if err != nil {
					return nil, err
				}

				if rpcErr := mailboxrpc.DecodeErrorHeaders(
					env.Headers,
				); rpcErr != nil {

					return &clientoor.DriveEventRequest{
						SessionID: sessionID,
						Event: &clientoor.FailEvent{
							Reason: fmt.Sprintf(
								"query incoming "+
									"metadata: %v",
								rpcErr,
							),
						},
					}, nil
				}

				resp, ok := p.(*arkrpc.ListVTXOsByScriptsResponse)
				if !ok {
					return nil, fmt.Errorf("expected "+
						"ListVTXOsByScriptsResponse, got %T",
						p)
				}

				matches, err :=
					clientoor.IncomingMetadataMatchesFromResponse(
						sessionID, resp,
					)
				if err != nil {
					return nil, err
				}

				return &clientoor.DriveEventRequest{
					SessionID: sessionID,
					Event: &clientoor.
						IncomingMetadataResolvedEvent{
						Matches: matches,
					},
				}, nil
			},
		},
	)

	// ListOORRecipientEventsByScript response: server resolved the
	// lightweight incoming transfer hint into the full Ark package for a
	// durable OOR receive session query.
	serverconn.AddEnvelopeRoute(
		router,
		serverconn.EnvelopeRouteConfig[
			clientoor.OORDurableMsg, clientoor.ActorResp,
		]{
			Service: "arkrpc.IndexerService",
			Method:  "ListOORRecipientEventsByScript",
			NewEvent: func() proto.Message {
				return &arkrpc.ListOORRecipientEventsByScriptResponse{}
			},
			Key: oorKey,
			Adapt: func(env *mailboxpb.Envelope,
				p proto.Message) (
				clientoor.OORDurableMsg, error) {

				if env == nil || env.Rpc == nil {
					return nil, fmt.Errorf("incoming resolve " +
						"response envelope must be provided")
				}

				sessionID, recipientEventID, err :=
					clientoor.ParseIncomingResolveCorrelationID(
						env.Rpc.CorrelationId,
					)
				if err != nil {
					return nil, err
				}

				if rpcErr := mailboxrpc.DecodeErrorHeaders(
					env.Headers,
				); rpcErr != nil {

					return &clientoor.DriveEventRequest{
						SessionID: sessionID,
						Event: &clientoor.FailEvent{
							Reason: fmt.Sprintf(
								"resolve incoming "+
									"transfer: %v",
								rpcErr,
							),
						},
					}, nil
				}

				resp, ok := p.(*arkrpc.ListOORRecipientEventsByScriptResponse)
				if !ok {
					return nil, fmt.Errorf("expected "+
						"ListOORRecipientEventsByScriptResponse"+
						", got %T", p)
				}

				incomingEvent, err :=
					clientoor.IncomingTransferEventFromResponse(
						sessionID, recipientEventID, resp,
					)
				if err != nil {
					return nil, err
				}

				return &clientoor.DriveEventRequest{
					SessionID: sessionID,
					Event:     incomingEvent,
				}, nil
			},
		},
	)

	// IncomingOOR: persist only the lightweight notification hint here; the
	// durable OOR actor performs the follow-up indexer query from its own
	// worker context, matching production and avoiding ingress deadlock.
	serverconn.AddRoute(
		router,
		serverconn.EventRouteConfig[
			clientoor.OORDurableMsg, clientoor.ActorResp,
		]{
			Service: indexerServiceName,
			Method:  indexerMethodIncomingOOR,
			NewEvent: func() proto.Message {
				return &arkrpc.IncomingOOREvent{}
			},
			Key: oorKey,
			Adapt: func(p proto.Message) (
				clientoor.OORDurableMsg, error) {

				evt, ok := p.(*arkrpc.IncomingOOREvent)
				if !ok {
					return nil, fmt.Errorf(
						"expected IncomingOOREvent"+
							", got %T", p,
					)
				}

				return clientoor.NewResolveIncomingTransferRequest(
					evt,
				)
			},
		},
	)
}
