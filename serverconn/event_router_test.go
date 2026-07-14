package serverconn

import (
	"context"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/lightninglabs/wavelength/serverconn/hellotestpb"
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

// stoppedRef is a fake ActorRef whose Tell always fails, modeling a per-session
// child that was reaped/stopped but whose receptionist registration has not yet
// been cleared. Its Tell returns ErrActorTerminated like a durable mailbox that
// has been closed.
type stoppedRef struct {
	id      string
	tellErr error

	tells int
}

func (r *stoppedRef) ID() string {
	return r.id
}

func (r *stoppedRef) Tell(context.Context, *helloStartedMsg) error {
	r.tells++

	return r.tellErr
}

func (r *stoppedRef) Ask(context.Context,
	*helloStartedMsg) actor.Future[struct{}] {

	panic("stoppedRef.Ask must not be called")
}

// TestEventRouteResolveKeyReapedChildReturnsError pins that a resolved per-
// session key whose registered ref errors on Tell (a reaped/stopped child)
// surfaces that error from the dispatcher instead of swallowing it and falling
// back to the static route key. Returning the error stalls the ingress cursor
// so the server redelivers the envelope; falling back would mis-route the event
// to the coordinator and silently lose the per-session ordering guarantee.
func TestEventRouteResolveKeyReapedChildReturnsError(t *testing.T) {
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

	// The static fallback key has a live actor that must stay quiet: the
	// reaped-child error must NOT fall back to it.
	fallback := &greetingBehavior{
		received: make(chan *helloStartedMsg, 1),
	}
	fallbackKey.Spawn(system, "fallback-actor", fallback)

	// Register a stopped child under the resolved per-session key whose
	// Tell fails like a closed durable mailbox.
	reaped := &stoppedRef{
		id:      "reaped-session-actor",
		tellErr: actor.ErrActorTerminated,
	}
	require.NoError(
		t,
		actor.RegisterWithReceptionist(
			system.Receptionist(), sessionKey, reaped,
		),
	)

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

	body, err := anypb.New(&hellotestpb.HelloStartedEvent{
		SessionId: "session-1",
	})
	require.NoError(t, err)

	// The dispatch resolves to the reaped child, whose Tell fails. The
	// dispatcher must surface that error so the ingress cursor stalls and
	// the server redelivers.
	err = dispatcher(t.Context(), &mailboxpb.Envelope{
		Body: body,
		Rpc: &mailboxpb.RpcMeta{
			Kind:    mailboxpb.RpcMeta_KIND_EVENT,
			Service: "test.Svc",
			Method:  "Started",
		},
	})
	require.Error(t, err)
	require.ErrorIs(t, err, actor.ErrActorTerminated)

	// The reaped child was told exactly once, and the error was NOT
	// swallowed into a fallback Tell on the static key.
	require.Equal(t, 1, reaped.tells)
	require.Empty(t, fallback.received)
}
