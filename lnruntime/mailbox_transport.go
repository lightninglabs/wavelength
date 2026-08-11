package lnruntime

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/lightninglabs/wavelength/baselib/actor"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/lightninglabs/wavelength/serverconn"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	// PeerMessageService is the mailbox route for native lnd peer traffic.
	PeerMessageService = "lnruntime.v1.PeerService"

	// PeerMessageMethod is the mailbox method for one ordered wire message.
	PeerMessageMethod = "Message"
)

// PeerEvent is one native lnd wire message prepared for durable delivery.
type PeerEvent struct {
	Identity       string
	CorrelationKey string
	Payload        []byte
}

// PeerEventSender persists one peer event in the application's authenticated
// transport before returning.
type PeerEventSender interface {
	SendPeerEvent(context.Context, PeerEvent) error
}

// PeerEventHandler processes one decoded lnd wire message received from the
// authenticated peer.
type PeerEventHandler func(context.Context, lnwire.Message) error

// DurablePeerTransportConfig configures ordered lnd traffic over a durable
// application transport.
type DurablePeerTransportConfig struct {
	Sender         PeerEventSender
	CorrelationKey string
	NewIdentity    func() (string, error)
}

// DurablePeerTransport serializes all messages for one logical lnd peer and
// persists each as a distinct mailbox event.
type DurablePeerTransport struct {
	cfg DurablePeerTransportConfig

	mu sync.Mutex
}

// NewDurablePeerTransport constructs an ordered transport for one logical
// peer.
func NewDurablePeerTransport(cfg DurablePeerTransportConfig) (
	*DurablePeerTransport, error) {

	if cfg.Sender == nil {
		return nil, fmt.Errorf("peer event sender is required")
	}
	if cfg.CorrelationKey == "" {
		return nil, fmt.Errorf("peer correlation key is required")
	}
	if cfg.NewIdentity == nil {
		cfg.NewIdentity = newPeerEventIdentity
	}

	return &DurablePeerTransport{cfg: cfg}, nil
}

// SendMessages durably enqueues each lnd message in per-peer FIFO order. The
// transport always waits for durable admission, so the sync hint does not
// change its behavior.
func (t *DurablePeerTransport) SendMessages(_ bool,
	messages ...lnwire.Message) error {

	t.mu.Lock()
	defer t.mu.Unlock()

	for i, message := range messages {
		if message == nil {
			return fmt.Errorf("lnd peer message %d is nil", i)
		}

		payload, err := MarshalPeerMessage(message)
		if err != nil {
			return fmt.Errorf("marshal lnd peer message %d: %w", i,
				err)
		}
		identity, err := t.cfg.NewIdentity()
		if err != nil {
			return fmt.Errorf("allocate lnd peer message "+
				"identity: %w", err)
		}
		if identity == "" {
			return fmt.Errorf("lnd peer message identity is empty")
		}

		event := PeerEvent{
			Identity:       identity,
			CorrelationKey: t.cfg.CorrelationKey,
			Payload:        payload,
		}
		if err := t.cfg.Sender.SendPeerEvent(
			context.Background(), event,
		); err != nil {
			return fmt.Errorf("persist lnd peer message %d: %w", i,
				err)
		}
	}

	return nil
}

// MarshalPeerMessage encodes one lnd wire message including its BOLT message
// type.
func MarshalPeerMessage(message lnwire.Message) ([]byte, error) {
	if message == nil {
		return nil, fmt.Errorf("lnd peer message is nil")
	}

	var payload bytes.Buffer
	if _, err := lnwire.WriteMessage(&payload, message, 0); err != nil {
		return nil, err
	}

	return payload.Bytes(), nil
}

// UnmarshalPeerMessage decodes exactly one lnd wire message and rejects
// trailing bytes so one mailbox event cannot smuggle an unordered batch.
func UnmarshalPeerMessage(payload []byte) (lnwire.Message, error) {
	reader := bytes.NewReader(payload)
	message, err := lnwire.ReadMessage(reader, 0)
	if err != nil {
		return nil, err
	}
	if reader.Len() != 0 {
		return nil, fmt.Errorf("lnd peer message has %d trailing bytes",
			reader.Len())
	}

	return message, nil
}

// PeerMessageRoute returns the mailbox service and method used for native lnd
// peer traffic.
func PeerMessageRoute() mailboxrpc.ServiceMethod {
	return mailboxrpc.ServiceMethod{
		Service: PeerMessageService,
		Method:  PeerMessageMethod,
	}
}

// NewPeerMessageDispatcher constructs a mailbox ingress route that decodes
// and processes one native lnd wire message before the envelope is acked.
func NewPeerMessageDispatcher(
	handler PeerEventHandler) serverconn.EnvelopeDispatcher {

	return func(ctx context.Context, env *mailboxpb.Envelope) error {
		if handler == nil {
			return fmt.Errorf("lnd peer event handler is required")
		}
		if env == nil || env.Body == nil {
			return fmt.Errorf("lnd peer event body is required")
		}

		body := &wrapperspb.BytesValue{}
		if err := env.Body.UnmarshalTo(body); err != nil {
			return fmt.Errorf("decode lnd peer event body: %w", err)
		}
		message, err := UnmarshalPeerMessage(body.Value)
		if err != nil {
			return fmt.Errorf("decode lnd peer message: %w", err)
		}

		return handler(ctx, message)
	}
}

// ServerConnPeerSender persists client-to-operator peer messages through the
// existing Wavelength server connection actor.
type ServerConnPeerSender struct {
	server actor.TellOnlyRef[serverconn.ServerConnMsg]
}

// NewServerConnPeerSender constructs a peer sender backed by serverconn.
func NewServerConnPeerSender(
	server actor.TellOnlyRef[serverconn.ServerConnMsg]) (
	*ServerConnPeerSender, error) {

	if server == nil {
		return nil, fmt.Errorf("server connection is required")
	}

	return &ServerConnPeerSender{server: server}, nil
}

// SendPeerEvent commits one native lnd event to serverconn's durable egress
// mailbox.
func (s *ServerConnPeerSender) SendPeerEvent(ctx context.Context,
	event PeerEvent) error {

	message := &peerServerMessage{event: event}

	return s.server.Tell(ctx, &serverconn.SendClientEventRequest{
		Message:        message,
		MsgID:          event.Identity,
		IdempotencyKey: event.Identity,
	})
}

// peerServerMessage adapts a peer event to serverconn's protobuf boundary.
type peerServerMessage struct {
	event PeerEvent
}

// ToProto wraps the ordinary BOLT bytes in a registered protobuf scalar.
func (m *peerServerMessage) ToProto() fn.Result[proto.Message] {
	return fn.Ok[proto.Message](&wrapperspb.BytesValue{
		Value: append([]byte(nil), m.event.Payload...),
	})
}

// ServiceMethod returns the native lnd peer mailbox route.
func (m *peerServerMessage) ServiceMethod() mailboxrpc.ServiceMethod {
	return PeerMessageRoute()
}

// CorrelationKey keeps every message for one logical peer in FIFO order.
func (m *peerServerMessage) CorrelationKey() string {
	return m.event.CorrelationKey
}

// newPeerEventIdentity returns a unique mailbox identity. It is deliberately
// independent of the body because identical BOLT messages can be distinct
// protocol events.
func newPeerEventIdentity() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}

	return id.String(), nil
}

var _ MessageTransport = (*DurablePeerTransport)(nil)

var _ PeerEventSender = (*ServerConnPeerSender)(nil)

var _ serverconn.ServerMessage = (*peerServerMessage)(nil)
