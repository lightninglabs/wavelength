package lnruntime

import (
	"context"
	"fmt"
	"sync"
	"testing"

	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// recordingPeerEventSender captures durably admitted peer events.
type recordingPeerEventSender struct {
	mu     sync.Mutex
	events []PeerEvent
}

// SendPeerEvent records one event.
func (s *recordingPeerEventSender) SendPeerEvent(_ context.Context,
	event PeerEvent) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	event.Payload = append([]byte(nil), event.Payload...)
	s.events = append(s.events, event)

	return nil
}

// recordingServerConnRef captures messages told to serverconn.
type recordingServerConnRef struct {
	messages []serverconn.ServerConnMsg
}

// ID returns the test actor identifier.
func (r *recordingServerConnRef) ID() string {
	return "recording-serverconn"
}

// Tell records one serverconn message.
func (r *recordingServerConnRef) Tell(_ context.Context,
	message serverconn.ServerConnMsg) error {

	r.messages = append(r.messages, message)

	return nil
}

// TryTell records one serverconn message without blocking.
func (r *recordingServerConnRef) TryTell(ctx context.Context,
	message serverconn.ServerConnMsg) error {

	return r.Tell(ctx, message)
}

// TestPeerMessageCodec verifies each mailbox payload contains exactly one
// ordinary lnd wire message.
func TestPeerMessageCodec(t *testing.T) {
	t.Parallel()

	original := lnwire.NewPing(17)
	payload, err := MarshalPeerMessage(original)
	require.NoError(t, err)

	decoded, err := UnmarshalPeerMessage(payload)
	require.NoError(t, err)
	decodedPing, ok := decoded.(*lnwire.Ping)
	require.True(t, ok)
	require.Equal(t, original.NumPongBytes, decodedPing.NumPongBytes)

	_, err = UnmarshalPeerMessage(append(payload, 1))
	require.ErrorContains(t, err, "trailing bytes")
}

// TestDurablePeerTransportPreservesDistinctEvents verifies identical BOLT
// messages retain distinct mailbox identities while sharing one FIFO lane.
func TestDurablePeerTransportPreservesDistinctEvents(t *testing.T) {
	t.Parallel()

	sender := &recordingPeerEventSender{}
	nextIdentity := 0
	transport, err := NewDurablePeerTransport(
		DurablePeerTransportConfig{
			Sender:         sender,
			CorrelationKey: "lnpeer/operator",
			NewIdentity: func() (string, error) {
				nextIdentity++

				identity := fmt.Sprintf("event-%d",
					nextIdentity)

				return identity, nil
			},
		},
	)
	require.NoError(t, err)

	message := lnwire.NewPing(9)
	require.NoError(t, transport.SendMessages(false, message, message))
	require.Len(t, sender.events, 2)
	require.Equal(t, "event-1", sender.events[0].Identity)
	require.Equal(t, "event-2", sender.events[1].Identity)
	require.Equal(t, "lnpeer/operator",
		sender.events[0].CorrelationKey)
	require.Equal(t, sender.events[0].Payload, sender.events[1].Payload)
}

// TestServerConnPeerSender verifies peer identity, ordering, and payload are
// retained at Wavelength's durable client-to-operator boundary.
func TestServerConnPeerSender(t *testing.T) {
	t.Parallel()

	serverRef := &recordingServerConnRef{}
	sender, err := NewServerConnPeerSender(serverRef)
	require.NoError(t, err)

	payload, err := MarshalPeerMessage(lnwire.NewPing(5))
	require.NoError(t, err)
	event := PeerEvent{
		Identity:       "peer-event-id",
		CorrelationKey: "lnpeer/operator",
		Payload:        payload,
	}
	require.NoError(t, sender.SendPeerEvent(t.Context(), event))
	require.Len(t, serverRef.messages, 1)

	serverMessage := serverRef.messages[0]
	request, ok := serverMessage.(*serverconn.SendClientEventRequest)
	require.True(t, ok)
	require.Equal(t, event.Identity, request.MsgID)
	require.Equal(t, event.Identity, request.IdempotencyKey)
	require.Equal(t, event.CorrelationKey, request.CorrelationKey())
	require.Equal(t, PeerMessageRoute(), request.Message.ServiceMethod())

	message, err := request.Message.ToProto().Unpack()
	require.NoError(t, err)
	body, ok := message.(*wrapperspb.BytesValue)
	require.True(t, ok)
	require.Equal(t, payload, body.Value)
}

// TestPeerMessageDispatcher verifies mailbox ingress decodes and handles the
// lnd message before returning.
func TestPeerMessageDispatcher(t *testing.T) {
	t.Parallel()

	payload, err := MarshalPeerMessage(lnwire.NewPing(3))
	require.NoError(t, err)
	body, err := anypb.New(&wrapperspb.BytesValue{Value: payload})
	require.NoError(t, err)

	var handled lnwire.Message
	dispatch := NewPeerMessageDispatcher(
		func(_ context.Context, message lnwire.Message) error {
			handled = message

			return nil
		},
	)
	require.NoError(
		t,
		dispatch(
			t.Context(), &mailboxpb.Envelope{
				Body: body,
			},
		),
	)
	handledPing, ok := handled.(*lnwire.Ping)
	require.True(t, ok)
	require.EqualValues(t, 3, handledPing.NumPongBytes)
}

var _ PeerEventSender = (*recordingPeerEventSender)(nil)
