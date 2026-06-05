package serverconn

import (
	"testing"
	"time"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	"github.com/lightninglabs/darepo-client/serverconn/hellotestpb"
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

// TestEventRouteResolveKeyFastPath verifies the per-message fast path: when
// ResolveKey names a service key with a live registration, the message is
// told straight to that actor; a miss falls back to the static route key.
func TestEventRouteResolveKeyFastPath(t *testing.T) {
	t.Parallel()

	system := actor.NewActorSystem()
	defer func() {
		require.NoError(t, system.Shutdown(t.Context()))
	}()

	fallbackKey := actor.NewServiceKey[*helloStartedMsg, struct{}](
		"fallback-route",
	)
	sessionKey := actor.NewServiceKey[*helloStartedMsg, struct{}](
		"session-route",
	)

	fallback := &greetingBehavior{
		received: make(chan *helloStartedMsg, 1),
	}
	session := &greetingBehavior{
		received: make(chan *helloStartedMsg, 1),
	}

	fallbackKey.Spawn(system, "fallback-actor", fallback)

	router := NewEventRouter(system)
	AddRoute(router, EventRouteConfig[*helloStartedMsg, struct{}]{
		Service: "test.Svc",
		Method:  "Started",
		NewEvent: func() proto.Message {
			return &hellotestpb.HelloStartedEvent{}
		},
		Key: fallbackKey,
		Adapt: func(p proto.Message) (*helloStartedMsg, error) {
			m := &helloStartedMsg{}

			return m, m.FromProto(p)
		},
		ResolveKey: func(_ *helloStartedMsg) (
			actor.ServiceKey[*helloStartedMsg, struct{}], bool) {

			return sessionKey, true
		},
	})

	dispatcher := router.AsDispatcherMap()[mailboxrpc.ServiceMethod{
		Service: "test.Svc",
		Method:  "Started",
	}]

	dispatch := func(sessionID string) {
		body, err := anypb.New(&hellotestpb.HelloStartedEvent{
			SessionId: sessionID,
		})
		require.NoError(t, err)

		err = dispatcher(t.Context(), &mailboxpb.Envelope{
			Body: body,
			Rpc: &mailboxpb.RpcMeta{
				Kind:    mailboxpb.RpcMeta_KIND_EVENT,
				Service: "test.Svc",
				Method:  "Started",
			},
		})
		require.NoError(t, err)
	}

	// Miss: nothing is registered under the resolved key yet, so the
	// message lands on the static fallback key.
	dispatch("session-1")
	select {
	case msg := <-fallback.received:
		require.Equal(t, "session-1", msg.SessionID)

	case <-time.After(time.Second):
		t.Fatal("fallback actor never received the message")
	}

	// Hit: once an actor registers under the resolved key, dispatch goes
	// straight to it and the fallback stays quiet.
	sessionKey.Spawn(system, "session-actor", session)

	dispatch("session-2")
	select {
	case msg := <-session.received:
		require.Equal(t, "session-2", msg.SessionID)

	case <-time.After(time.Second):
		t.Fatal("session actor never received the message")
	}
	require.Empty(t, fallback.received)
}
