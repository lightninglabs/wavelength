//nolint:ll
package serverconn

import (
	"context"
	"database/sql"
	"testing"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// TestAddEnvelopeRoute_RejectsNilBodyWithoutEncodedError verifies that
// AddEnvelopeRoute still fails closed on malformed routed responses that
// omit both the proto body and the encoded gRPC status headers.
func TestAddEnvelopeRoute_RejectsNilBodyWithoutEncodedError(t *testing.T) {
	t.Parallel()

	router := NewEventRouter(actor.NewActorSystem())
	routeKey := actor.NewServiceKey[*helloStartedMsg, struct{}](
		"test-route",
	)

	adaptCalled := false

	AddEnvelopeRoute(
		router, EnvelopeRouteConfig[*helloStartedMsg, struct{}]{
			Service: "test.Svc",
			Method:  "Unary",
			NewEvent: func() proto.Message {
				return &wrapperspb.StringValue{}
			},
			Key: routeKey,
			Adapt: func(_ *mailboxpb.Envelope, _ proto.Message) (
				*helloStartedMsg, error) {

				adaptCalled = true

				return &helloStartedMsg{}, nil
			},
		},
	)

	dispatcher := router.AsDispatcherMap()[mailboxrpc.ServiceMethod{
		Service: "test.Svc",
		Method:  "Unary",
	}]

	err := dispatcher(t.Context(), &mailboxpb.Envelope{
		Rpc: &mailboxpb.RpcMeta{
			Kind:          mailboxpb.RpcMeta_KIND_RESPONSE,
			CorrelationId: "corr-1",
			Service:       "test.Svc",
			Method:        "Unary",
		},
	})
	require.ErrorContains(
		t, err, "nil envelope body without encoded error",
	)
	require.False(t, adaptCalled)
}

func TestAddEnvelopeRoute_AsksActorWithTransactionContext(t *testing.T) {
	t.Parallel()

	system := actor.NewActorSystem()
	routeKey := actor.NewServiceKey[*helloStartedMsg, struct{}](
		"test-route-tx",
	)

	processed := make(chan bool, 1)
	behavior := actor.NewFunctionBehavior(
		func(ctx context.Context, _ *helloStartedMsg) fn.Result[struct{}] {
			processed <- actor.HasTx(ctx)

			return fn.Ok(struct{}{})
		},
	)
	actor.RegisterWithSystem(system, "route-tx-actor", routeKey, behavior)

	router := NewEventRouter(system)
	AddEnvelopeRoute(
		router, EnvelopeRouteConfig[*helloStartedMsg, struct{}]{
			Service: "test.Svc",
			Method:  "Unary",
			NewEvent: func() proto.Message {
				return &wrapperspb.StringValue{}
			},
			Key: routeKey,
			Adapt: func(_ *mailboxpb.Envelope, _ proto.Message) (
				*helloStartedMsg, error) {

				return &helloStartedMsg{}, nil
			},
		},
	)

	body, err := anypb.New(wrapperspb.String("ok"))
	require.NoError(t, err)

	dispatcher := router.AsDispatcherMap()[mailboxrpc.ServiceMethod{
		Service: "test.Svc",
		Method:  "Unary",
	}]

	txCtx := actor.WithTx(t.Context(), (*sql.Tx)(nil))
	err = dispatcher(txCtx, &mailboxpb.Envelope{
		Body: body,
		Rpc: &mailboxpb.RpcMeta{
			Kind:          mailboxpb.RpcMeta_KIND_RESPONSE,
			CorrelationId: "corr-1",
			Service:       "test.Svc",
			Method:        "Unary",
		},
	})
	require.NoError(t, err)

	require.True(t, <-processed)
}
