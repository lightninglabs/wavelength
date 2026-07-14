package serverconn

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// keyedServerMessage is a ServerMessage that also satisfies the
// actor.Message contract and stamps a custom correlation key. It models
// the OOR/round outbox messages (e.g. SendSubmitPackageRequest) which
// opt in to per-key FIFO claim ordering on the durable mailbox.
type keyedServerMessage struct {
	actor.BaseMessage

	key     string
	payload []byte
}

// MessageType returns the type name for routing and logging.
func (m *keyedServerMessage) MessageType() string {
	return "keyedServerMessage"
}

// CorrelationKey returns the per-key FIFO key stamped on the message.
func (m *keyedServerMessage) CorrelationKey() string { return m.key }

// ServiceMethod returns deterministic routing metadata.
func (m *keyedServerMessage) ServiceMethod() mailboxrpc.ServiceMethod {
	return mailboxrpc.ServiceMethod{Service: "test.svc", Method: "Push"}
}

// ToProto wraps the payload bytes in a wrapperspb so the request can be
// serialized through the standard TLV path.
func (m *keyedServerMessage) ToProto() fn.Result[proto.Message] {
	return fn.Ok[proto.Message](wrapperspb.Bytes(m.payload))
}

// TestSendClientEventRequestForwardsInnerCorrelationKey verifies that the
// wrapper exposes the inner ServerMessage's correlation key so the durable
// mailbox stamps the right per-key FIFO lane at enqueue. Without the
// forward the embedded BaseMessage default ("") would land every wrapped
// outbox row in the unkeyed lane and the per-key claim invariant would
// never trigger for production traffic.
func TestSendClientEventRequestForwardsInnerCorrelationKey(t *testing.T) {
	t.Parallel()

	const innerKey = "oor/abc123"

	req := &SendClientEventRequest{
		Message: &keyedServerMessage{
			key:     innerKey,
			payload: []byte("submit-package"),
		},
	}

	require.Equal(t, innerKey, req.CorrelationKey())
}

// TestSendClientEventRequestUnkeyedInnerReturnsEmpty covers the
// opt-out path: when the inner ServerMessage's CorrelationKey is
// empty (the BaseMessage default), the wrapper must also return ""
// so the message keeps the legacy global available_at claim order
// and does not synthesize a phantom lane.
func TestSendClientEventRequestUnkeyedInnerReturnsEmpty(t *testing.T) {
	t.Parallel()

	req := &SendClientEventRequest{
		Message: &keyedServerMessage{
			key:     "",
			payload: []byte("unkeyed"),
		},
	}

	require.Equal(t, "", req.CorrelationKey())
}

// TestSendClientEventRequestNonImplementerReturnsEmpty covers the
// post-decode path: rawServerMessage (and any other ServerMessage that
// does not implement CorrelationKey) must yield "" without panicking.
// CorrelationKey is only consumed at enqueue, so this branch only
// matters defensively — a future wrapper that calls CorrelationKey
// after Decode must still see a stable empty value.
func TestSendClientEventRequestNonImplementerReturnsEmpty(t *testing.T) {
	t.Parallel()

	req := &SendClientEventRequest{
		Message: &bytesServerMessage{
			payload: []byte("legacy"),
		},
	}

	require.Equal(t, "", req.CorrelationKey())
}

// TestSendClientEventRequestNilMessageReturnsEmpty guards the
// degenerate case where the wrapper is constructed without a Message.
// The forward must not panic on the nil interface assertion.
func TestSendClientEventRequestNilMessageReturnsEmpty(t *testing.T) {
	t.Parallel()

	req := &SendClientEventRequest{}

	require.Equal(t, "", req.CorrelationKey())
}

// capturingEnqueueStore wraps memCheckpointStore and records the
// EnqueueParams seen by EnqueueMessage. Used to assert that the inner
// CorrelationKey survives the wrapper hop and reaches the persistence
// layer with the expected value.
type capturingEnqueueStore struct {
	*memCheckpointStore

	lastParams actor.EnqueueParams
	called     bool
}

// EnqueueMessage records the incoming params before delegating to the
// underlying in-memory store.
func (s *capturingEnqueueStore) EnqueueMessage(ctx context.Context,
	params actor.EnqueueParams) error {

	s.lastParams = params
	s.called = true

	return s.memCheckpointStore.EnqueueMessage(ctx, params)
}

// TestSendClientEventRequestTLVRoundTripPreservesKey pins down the
// outbox-CDC delivery path: when OOR/round wraps an outbox event in
// SendClientEventRequest and writes it to the actor outbox, the wrapper
// is decoded later by OutboxPublisher and Tell'd into the serverconn
// mailbox for the first time. That first enqueue is where the durable
// mailbox reads CorrelationKey to stamp the claim lane, but Decode
// replaces the concrete inner Message with a *rawServerMessage that no
// longer implements CorrelationKey. Without persisting the key in the
// wrapper's TLV stream, the structural assertion in CorrelationKey
// would miss and the row would land in the unkeyed lane (defeating
// per-session FIFO for transport-outbox-enabled deployments). The fix
// writes the key into its own TLV record at Encode and caches it on
// Decode. This test exercises that roundtrip.
func TestSendClientEventRequestTLVRoundTripPreservesKey(t *testing.T) {
	t.Parallel()

	const innerKey = "oor/abc123"

	original := &SendClientEventRequest{
		Message: &keyedServerMessage{
			key:     innerKey,
			payload: []byte("outbox-cdc-event"),
		},
	}

	// Sanity: the pre-encode wrapper exposes the key via the
	// structural assertion on the concrete inner message.
	require.Equal(t, innerKey, original.CorrelationKey())

	var buf bytes.Buffer
	require.NoError(t, original.Encode(&buf))

	var decoded SendClientEventRequest
	require.NoError(t, decoded.Decode(bytes.NewReader(buf.Bytes())))

	// After Decode the inner is a *rawServerMessage and the
	// structural assertion misses; the wrapper must surface the
	// cached key persisted in the TLV stream.
	_, isRaw := decoded.Message.(*rawServerMessage)
	require.True(
		t, isRaw,
		"decode replaces concrete inner with rawServerMessage",
	)
	require.Equal(t, innerKey, decoded.CorrelationKey())

	// Re-encoding the decoded wrapper must persist the cached key so
	// any further hop through outbox CDC keeps the lane stable.
	var buf2 bytes.Buffer
	require.NoError(t, decoded.Encode(&buf2))

	var roundTripped SendClientEventRequest
	require.NoError(
		t,
		roundTripped.Decode(
			bytes.NewReader(
				buf2.Bytes(),
			),
		),
	)
	require.Equal(t, innerKey, roundTripped.CorrelationKey())
}

// TestSendClientEventRequestTLVRoundTripPreservesEmptyKey covers the
// opt-out path through the TLV stream: an inner message with no
// correlation key (or one that resolves to "" via the unkeyed fallback,
// e.g. a malformed PSBT in SendSubmitPackageRequest) must round-trip
// to an empty cached key, keeping the row in the legacy unkeyed lane.
func TestSendClientEventRequestTLVRoundTripPreservesEmptyKey(t *testing.T) {
	t.Parallel()

	original := &SendClientEventRequest{
		Message: &keyedServerMessage{
			key:     "",
			payload: []byte("unkeyed-event"),
		},
	}

	var buf bytes.Buffer
	require.NoError(t, original.Encode(&buf))

	var decoded SendClientEventRequest
	require.NoError(t, decoded.Decode(bytes.NewReader(buf.Bytes())))

	require.Equal(t, "", decoded.CorrelationKey())
}

// TestSendClientEventRequestPlumbsCorrelationKeyToStore is the
// integration-level regression test for H-1: a stamped outbox message
// wrapped in SendClientEventRequest and Tell'd to the durable actor must
// reach the DeliveryStore.EnqueueMessage call site with a non-empty
// CorrelationKey on EnqueueParams. Before the wrapper forward this
// assertion failed — every wrapped row landed with CorrelationKey == ""
// even when the inner message carried a per-session key. With the
// forward in place the captured params carry the inner key, which is
// exactly what the per-correlation-key FIFO claim path consumes.
func TestSendClientEventRequestPlumbsCorrelationKeyToStore(t *testing.T) {
	t.Parallel()

	const innerKey = "round/feedface"

	mb := newInMemoryMailbox()
	base := newMemCheckpointStore()
	store := &capturingEnqueueStore{memCheckpointStore: base}

	// Build the config with the base store so the helper's concrete
	// type fits, then swap to the capturing decorator on the interface
	// field. Production wires the store the same way: through the
	// actor.DeliveryStore interface on ConnectorConfig.Store.
	cfg := newTestConnectorConfig(mb, base)
	cfg.Store = store
	cfg.Codec = NewServerConnCodec()

	durable := newDurableConnectorForTest(t, cfg, 50*time.Millisecond)
	durable.Start()
	defer durable.Stop()

	err := durable.TellRef().Tell(t.Context(), &SendClientEventRequest{
		Message: &keyedServerMessage{
			key:     innerKey,
			payload: []byte("stamped-event"),
		},
	})
	require.NoError(t, err)

	require.True(t, store.called, "EnqueueMessage must be invoked")
	require.Equal(
		t, innerKey, store.lastParams.CorrelationKey,
		"wrapper must forward inner CorrelationKey into EnqueueParams",
	)
}
