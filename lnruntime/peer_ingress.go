package lnruntime

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/serverconn"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	peerMessageMaxRetryDelay = time.Minute
	peerMessageMaxAttempts   = 1<<31 - 1
)

const (
	// PeerMessageIngressTLVType identifies one durably staged inbound BOLT
	// message. The 0x72xx range is reserved for the modular lnd runtime.
	PeerMessageIngressTLVType tlv.Type = 0x7200

	peerMessagePayloadRecord tlv.Type = 1
	peerMessageLaneRecord    tlv.Type = 3
)

// PeerMessageIngressConfig contains the durable boundary between mailbox
// delivery and native lnd message processing.
type PeerMessageIngressConfig struct {
	ActorID string
	Store   actor.DeliveryStore
	Handler PeerEventHandler
	Log     btclog.Logger
}

// PeerMessageIngress persists inbound BOLT messages before invoking native
// lnd outside the mailbox ingress transaction.
type PeerMessageIngress struct {
	actorID string
	durable *actor.DurableActor[*peerMessageIngressMsg, struct{}]
}

// peerMessageIngressMsg is one ordered BOLT message in the durable ingress
// mailbox.
type peerMessageIngressMsg struct {
	actor.BaseMessage

	Payload []byte
	Lane    string
}

// MessageType returns the stable actor message name.
func (*peerMessageIngressMsg) MessageType() string {
	return "lnruntime.PeerMessageIngress"
}

// CorrelationKey keeps every message for the logical peer in FIFO order.
func (m *peerMessageIngressMsg) CorrelationKey() string {
	return m.Lane
}

// TLVType returns the durable message type identifier.
func (*peerMessageIngressMsg) TLVType() tlv.Type {
	return PeerMessageIngressTLVType
}

// Encode serializes the BOLT payload and its FIFO lane as a TLV stream.
func (m *peerMessageIngressMsg) Encode(w io.Writer) error {
	lane := []byte(m.Lane)
	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(peerMessagePayloadRecord, &m.Payload),
		tlv.MakePrimitiveRecord(peerMessageLaneRecord, &lane),
	)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode restores the BOLT payload and FIFO lane from a TLV stream.
func (m *peerMessageIngressMsg) Decode(r io.Reader) error {
	var lane []byte
	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(peerMessagePayloadRecord, &m.Payload),
		tlv.MakePrimitiveRecord(peerMessageLaneRecord, &lane),
	)
	if err != nil {
		return err
	}
	if _, err := stream.DecodeWithParsedTypes(r); err != nil {
		return err
	}

	m.Lane = string(lane)

	return nil
}

// peerMessageIngressBehavior invokes lnd between the durable actor's short
// database transactions. LND may synchronously emit a reply, so running the
// handler while a SQLite writer is held would deadlock its durable sender.
type peerMessageIngressBehavior struct {
	handler PeerEventHandler
}

// Receive decodes and dispatches one BOLT message, then atomically consumes
// its durable mailbox row.
func (b *peerMessageIngressBehavior) Receive(ctx context.Context,
	msg *peerMessageIngressMsg,
	ax actor.Exec[struct{}]) fn.Result[struct{}] {

	message, err := UnmarshalPeerMessage(msg.Payload)
	if err != nil {
		return fn.Err[struct{}](
			fmt.Errorf("decode durable lnd peer message: %w", err),
		)
	}
	if err := b.handler(ctx, message); err != nil {
		return fn.Err[struct{}](
			fmt.Errorf("handle durable lnd peer message: %w", err),
		)
	}
	if err := ax.Commit(ctx, func(context.Context, struct{}) error {
		return nil
	}); err != nil {
		return fn.Err[struct{}](err)
	}

	return fn.Ok(struct{}{})
}

// NewPeerMessageIngress creates and starts one durable inbound BOLT queue.
func NewPeerMessageIngress(cfg PeerMessageIngressConfig) (*PeerMessageIngress,
	error) {

	return newPeerMessageIngress(cfg, parkPeerMessageRetryPolicy)
}

// parkPeerMessageRetryPolicy keeps an ordered BOLT lane blocked until its
// endpoint can process the message. Advancing past a failed CommitSig or
// RevokeAndAck would silently desynchronize the channel.
func parkPeerMessageRetryPolicy(_ error, attempts int) (bool, time.Duration) {
	if attempts < 0 {
		attempts = 0
	}
	if attempts > 6 {
		attempts = 6
	}
	delay := time.Second << uint(attempts)
	if delay > peerMessageMaxRetryDelay {
		delay = peerMessageMaxRetryDelay
	}

	return true, delay
}

// newPeerMessageIngress accepts a retry policy so tests can exercise repeated
// endpoint failures without waiting for production backoff intervals.
func newPeerMessageIngress(cfg PeerMessageIngressConfig,
	retryPolicy actor.TellRetryPolicy) (*PeerMessageIngress, error) {

	if cfg.ActorID == "" {
		return nil, fmt.Errorf("peer ingress actor id is required")
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("peer ingress delivery store is " +
			"required")
	}
	if cfg.Handler == nil {
		return nil, fmt.Errorf("peer ingress handler is required")
	}
	if retryPolicy == nil {
		return nil, fmt.Errorf("peer ingress retry policy is required")
	}

	codec := actor.NewMessageCodec()
	codec.MustRegister(PeerMessageIngressTLVType, func() actor.TLVMessage {
		return &peerMessageIngressMsg{}
	})
	behavior := &peerMessageIngressBehavior{handler: cfg.Handler}
	durableCfg := actor.DefaultDurableTxActorConfig[
		*peerMessageIngressMsg, struct{}, struct{},
	](
		cfg.ActorID, behavior,
		func(context.Context, actor.DeliveryStore) struct{} {
			return struct{}{}
		},
		cfg.Store, codec,
	)
	if cfg.Log != nil {
		durableCfg.Log = fn.Some(cfg.Log)
	}
	durableCfg.TellRetryPolicy = retryPolicy
	durableCfg.MaxAttempts = peerMessageMaxAttempts
	durable, err := actor.NewDurableActor(durableCfg).Unpack()
	if err != nil {
		return nil, fmt.Errorf("create peer ingress actor: %w", err)
	}
	durable.Start()

	return &PeerMessageIngress{
		actorID: cfg.ActorID,
		durable: durable,
	}, nil
}

// Dispatcher returns the mailbox route that durably stages one valid BOLT
// message. The Tell joins the caller's ingress transaction; LND processing
// starts only after that transaction commits and wakes the durable actor.
func (i *PeerMessageIngress) Dispatcher() serverconn.EnvelopeDispatcher {
	return func(ctx context.Context, env *mailboxpb.Envelope) error {
		if i == nil || i.durable == nil {
			return fmt.Errorf("lnd peer ingress is not initialized")
		}
		if env == nil || env.Body == nil {
			return fmt.Errorf("lnd peer event body is required")
		}

		body := &wrapperspb.BytesValue{}
		if err := env.Body.UnmarshalTo(body); err != nil {
			return fmt.Errorf("decode lnd peer event body: %w", err)
		}
		if _, err := UnmarshalPeerMessage(body.Value); err != nil {
			return fmt.Errorf("decode lnd peer message: %w", err)
		}

		return i.durable.Ref().Tell(ctx, &peerMessageIngressMsg{
			Payload: append([]byte(nil), body.Value...),
			Lane:    i.actorID,
		})
	}
}

// StopAndWait stops the durable ingress actor and waits for its worker.
func (i *PeerMessageIngress) StopAndWait(ctx context.Context) error {
	if i == nil || i.durable == nil {
		return nil
	}

	return i.durable.StopAndWait(ctx)
}

// Stop stops the durable ingress actor without waiting for its worker.
func (i *PeerMessageIngress) Stop() {
	if i == nil || i.durable == nil {
		return
	}

	i.durable.Stop()
}

var _ actor.TxBehavior[
	*peerMessageIngressMsg, struct{}, struct{},
] = (*peerMessageIngressBehavior)(nil)
