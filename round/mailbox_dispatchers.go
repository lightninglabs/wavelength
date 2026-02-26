package round

import (
	"context"
	"fmt"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	"github.com/lightninglabs/darepo-client/roundwire"
	"github.com/lightninglabs/darepo-client/serverconn"
)

// NewMailboxDispatchers returns serverconn dispatchers for server->client round
// EVENT envelopes.
func NewMailboxDispatchers(
	roundRef actor.ActorRef[
		actormsg.RoundReceivable,
		actormsg.RoundActorResp,
	],
) serverconn.DispatcherMap {

	dispatch := func(ctx context.Context, env *mailboxpb.Envelope) error {
		if env == nil || env.Rpc == nil || env.Body == nil {
			return fmt.Errorf("invalid round event envelope")
		}

		event, err := DecodeServerMailboxPayload(
			env.Rpc.Method, env.Body.Value,
		)
		if err != nil {
			return fmt.Errorf("decode round event payload: %w", err)
		}

		return roundRef.Tell(
			ctx, &ServerMessageNotification{
				Message: event,
			},
		)
	}

	methods := []string{
		roundwire.MethodClientErrorResp,
		roundwire.MethodClientSuccessResp,
		roundwire.MethodClientAwaitingInputSigsResp,
		roundwire.MethodClientVTXOAggNonces,
		roundwire.MethodClientVTXOAggSigs,
		roundwire.MethodClientBatchInfo,
		roundwire.MethodClientRoundFailedResp,
	}

	dispatchers := make(serverconn.DispatcherMap, len(methods))
	for _, method := range methods {
		dispatchers[mailboxrpc.ServiceMethod{
			Service: roundwire.ServiceName,
			Method:  method,
		}] = dispatch
	}

	return dispatchers
}
