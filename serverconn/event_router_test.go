package serverconn

import (
	"testing"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// TestAddEnvelopeRouteRejectsNilBodyWithoutEncodedError verifies that
// AddEnvelopeRoute still fails closed on malformed routed responses that
// omit both the proto body and the encoded gRPC status headers.
func TestAddEnvelopeRouteRejectsNilBodyWithoutEncodedError(t *testing.T) {
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

// TestAddEnvelopeRouteCanMarkEnvelopeHandled verifies that routes can consume
// an envelope without forwarding a message to an actor mailbox.
func TestAddEnvelopeRouteCanMarkEnvelopeHandled(t *testing.T) {
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

				return nil, ErrEnvelopeHandled
			},
		},
	)

	dispatcher := router.AsDispatcherMap()[mailboxrpc.ServiceMethod{
		Service: "test.Svc",
		Method:  "Unary",
	}]

	body, err := anypb.New(wrapperspb.String("stale"))
	require.NoError(t, err)

	err = dispatcher(t.Context(), &mailboxpb.Envelope{
		Body: body,
		Rpc: &mailboxpb.RpcMeta{
			Kind:          mailboxpb.RpcMeta_KIND_RESPONSE,
			CorrelationId: "corr-1",
			Service:       "test.Svc",
			Method:        "Unary",
		},
	})
	require.NoError(t, err)
	require.True(t, adaptCalled)
}
