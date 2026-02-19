package serverconn

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// bytesServerMessage is a minimal ServerMessage wrapper for TLV tests.
type bytesServerMessage struct {
	payload []byte
}

// ToProto converts the payload to a protobuf wrapper.
func (m *bytesServerMessage) ToProto() proto.Message {
	return wrapperspb.Bytes(m.payload)
}

// encodeTLVMessage serializes a TLV message to bytes for round-trip tests.
func encodeTLVMessage(t *testing.T, msg actor.TLVMessage) []byte {
	t.Helper()

	var buf bytes.Buffer
	require.NoError(t, msg.Encode(&buf))

	return buf.Bytes()
}

// TestSendClientEventRequest_TLVRoundTrip_DeterministicIDs verifies that equal
// payloads produce stable derived MsgID and IdempotencyKey across round trips.
func TestSendClientEventRequest_TLVRoundTrip_DeterministicIDs(t *testing.T) {
	t.Parallel()

	reqA := &SendClientEventRequest{
		Message: &bytesServerMessage{payload: []byte("same-event")},
	}
	reqB := &SendClientEventRequest{
		Message: &bytesServerMessage{payload: []byte("same-event")},
	}

	var decodedA SendClientEventRequest
	require.NoError(
		t, decodedA.Decode(bytes.NewReader(encodeTLVMessage(t, reqA))),
	)

	var decodedB SendClientEventRequest
	require.NoError(
		t, decodedB.Decode(bytes.NewReader(encodeTLVMessage(t, reqB))),
	)

	require.NotEmpty(t, decodedA.MsgID)
	require.NotEmpty(t, decodedA.IdempotencyKey)
	require.Equal(t, decodedA.MsgID, decodedB.MsgID)
	require.Equal(
		t, decodedA.IdempotencyKey, decodedB.IdempotencyKey,
	)

	msg, ok := decodedA.Message.ToProto().(*wrapperspb.BytesValue)
	require.True(t, ok)
	require.Equal(t, []byte("same-event"), msg.Value)
}

// TestSendClientEventRequest_TLVRoundTrip_ExplicitIDs verifies explicit
// identifiers survive Encode/Decode unchanged.
func TestSendClientEventRequest_TLVRoundTrip_ExplicitIDs(t *testing.T) {
	t.Parallel()

	req := &SendClientEventRequest{
		Message:        &bytesServerMessage{payload: []byte("payload")},
		MsgID:          "msg-explicit",
		IdempotencyKey: "idem-explicit",
	}

	var decoded SendClientEventRequest
	require.NoError(
		t, decoded.Decode(bytes.NewReader(encodeTLVMessage(t, req))),
	)

	require.Equal(t, "msg-explicit", decoded.MsgID)
	require.Equal(t, "idem-explicit", decoded.IdempotencyKey)
}

// TestSendRPCRequest_TLVRoundTrip verifies envelope serialization round trips.
func TestSendRPCRequest_TLVRoundTrip(t *testing.T) {
	t.Parallel()

	body, err := anypb.New(wrapperspb.String("request"))
	require.NoError(t, err)

	original := &SendRPCRequest{
		Envelope: &mailboxpb.Envelope{
			ProtocolVersion: 1,
			MsgId:           "msg-1",
			Sender:          "client-1",
			Recipient:       "server-1",
			CreatedAtUnixMs: time.Now().UnixMilli(),
			Body:            body,
			Rpc: &mailboxpb.RpcMeta{
				Kind:          mailboxpb.RpcMeta_KIND_REQUEST,
				Service:       "test.Svc",
				Method:        "DoThing",
				CorrelationId: "corr-1",
			},
		},
	}

	var decoded SendRPCRequest
	encoded := encodeTLVMessage(t, original)

	require.NoError(
		t, decoded.Decode(bytes.NewReader(encoded)),
	)

	require.True(t, proto.Equal(original.Envelope, decoded.Envelope))
}

// TestServerConnMessageMetadata verifies static message metadata methods.
func TestServerConnMessageMetadata(t *testing.T) {
	t.Parallel()

	eventReq := &SendClientEventRequest{}
	require.Equal(t, "SendClientEventRequest", eventReq.MessageType())
	require.Equal(t, SendClientEventRequestMsgType, eventReq.TLVType())
	eventReq.serverConnMsgSealed()

	eventResp := &SendClientEventResponse{}
	require.Equal(t, "SendClientEventResponse", eventResp.MessageType())
	eventResp.serverConnRespSealed()

	rpcReq := &SendRPCRequest{}
	require.Equal(t, "SendRPCRequest", rpcReq.MessageType())
	require.Equal(t, SendRPCRequestMsgType, rpcReq.TLVType())
	rpcReq.serverConnMsgSealed()
}

// TestRawServerMessage_ToProtoDecodeFailure verifies ToProto returns nil when
// the Any payload cannot be resolved through the protobuf type registry.
func TestRawServerMessage_ToProtoDecodeFailure(t *testing.T) {
	t.Parallel()

	raw := &rawServerMessage{
		anyMsg: &anypb.Any{
			TypeUrl: "type.googleapis.com/test.unknown.Message",
			Value:   []byte{0x01},
		},
	}

	require.Nil(t, raw.ToProto())
}

// TestServerConnCodec_RoundTrip verifies both serverconn message types can be
// encoded and decoded via NewServerConnCodec.
func TestServerConnCodec_RoundTrip(t *testing.T) {
	t.Parallel()

	codec := NewServerConnCodec()

	eventPayload := []byte("codec-event")
	eventReq := &SendClientEventRequest{
		Message: &bytesServerMessage{
			payload: eventPayload,
		},
		MsgID:          "msg-codec-event",
		IdempotencyKey: "idem-codec-event",
	}

	eventBytes, err := codec.Encode(eventReq)
	require.NoError(t, err)

	decodedEvent, err := codec.Decode(eventBytes)
	require.NoError(t, err)

	typedEvent, ok := decodedEvent.(*SendClientEventRequest)
	require.True(t, ok)
	require.Equal(t, eventReq.MsgID, typedEvent.MsgID)
	require.Equal(
		t, eventReq.IdempotencyKey, typedEvent.IdempotencyKey,
	)

	body, err := anypb.New(wrapperspb.String("codec-rpc"))
	require.NoError(t, err)

	rpcReq := &SendRPCRequest{
		Envelope: &mailboxpb.Envelope{
			ProtocolVersion: 1,
			MsgId:           "msg-codec-rpc",
			Sender:          "client-1",
			Recipient:       "server-1",
			Body:            body,
		},
	}

	rpcBytes, err := codec.Encode(rpcReq)
	require.NoError(t, err)

	decodedRPC, err := codec.Decode(rpcBytes)
	require.NoError(t, err)

	typedRPC, ok := decodedRPC.(*SendRPCRequest)
	require.True(t, ok)
	require.Equal(t, rpcReq.Envelope.MsgId, typedRPC.Envelope.MsgId)
	require.Equal(t, rpcReq.Envelope.Sender, typedRPC.Envelope.Sender)
}

// unknownServerConnMsg is a test-only unsupported message for Receive.
type unknownServerConnMsg struct {
	actor.BaseMessage
}

// MessageType returns a test message type name.
func (m *unknownServerConnMsg) MessageType() string {
	return "unknownServerConnMsg"
}

// TLVType returns a test-only TLV type.
func (m *unknownServerConnMsg) TLVType() tlv.Type {
	return 999_999
}

// Encode serializes a no-op payload for unknown message tests.
func (m *unknownServerConnMsg) Encode(w io.Writer) error {
	_, err := w.Write(nil)

	return err
}

// Decode deserializes a no-op payload for unknown message tests.
func (m *unknownServerConnMsg) Decode(r io.Reader) error {
	_, _ = io.Copy(io.Discard, r)

	return nil
}

// serverConnMsgSealed marks the type as part of ServerConnMsg in tests.
func (m *unknownServerConnMsg) serverConnMsgSealed() {}

// TestServerConnectionActor_ReceiveUnknownMessage verifies Receive rejects
// unsupported message types.
func TestServerConnectionActor_ReceiveUnknownMessage(t *testing.T) {
	t.Parallel()

	connector, _, _ := newTestConnector(t, nil)
	result := connector.Receive(t.Context(), &unknownServerConnMsg{})
	require.Error(t, result.Err())
}

// TestServerConnectionActor_ReceiveSendRPCRequest verifies the SendRPCRequest
// path sends the provided envelope to the mailbox edge.
func TestServerConnectionActor_ReceiveSendRPCRequest(t *testing.T) {
	t.Parallel()

	connector, mb, _ := newTestConnector(t, nil)

	body, err := anypb.New(wrapperspb.String("rpc"))
	require.NoError(t, err)

	envelope := &mailboxpb.Envelope{
		ProtocolVersion: 1,
		MsgId:           "msg-rpc",
		Sender:          "client-1",
		Recipient:       "server-1",
		Body:            body,
		Rpc: &mailboxpb.RpcMeta{
			Kind:          mailboxpb.RpcMeta_KIND_REQUEST,
			Service:       "test.Svc",
			Method:        "Rpc",
			CorrelationId: "corr-rpc",
		},
	}

	result := connector.Receive(t.Context(), &SendRPCRequest{
		Envelope: envelope,
	})
	require.NoError(t, result.Err())

	mb.mu.Lock()
	envs := append(
		[]*mailboxpb.Envelope(nil), mb.mailboxes["server-1"]...,
	)
	mb.mu.Unlock()

	require.Len(t, envs, 1)
	require.Equal(t, envelope.MsgId, envs[0].MsgId)
	require.Equal(t, envelope.Sender, envs[0].Sender)
	require.Equal(t, envelope.Recipient, envs[0].Recipient)
	require.Equal(
		t, envelope.Rpc.CorrelationId, envs[0].Rpc.CorrelationId,
	)
}
