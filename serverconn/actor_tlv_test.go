package serverconn

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/internal/indexerlimits"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	fn "github.com/lightningnetwork/lnd/fn/v2"
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

const (
	testEventService = "test.v1.EventService"
	testEventMethod  = "PushEvent"
)

// ServiceMethod returns deterministic routing metadata for tests.
func (m *bytesServerMessage) ServiceMethod() mailboxrpc.ServiceMethod {
	return mailboxrpc.ServiceMethod{
		Service: testEventService,
		Method:  testEventMethod,
	}
}

// ToProto converts the payload to a protobuf wrapper.
func (m *bytesServerMessage) ToProto() fn.Result[proto.Message] {
	return fn.Ok[proto.Message](wrapperspb.Bytes(m.payload))
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
		Message: &bytesServerMessage{
			payload: []byte("same-event"),
		},
	}
	reqB := &SendClientEventRequest{
		Message: &bytesServerMessage{
			payload: []byte("same-event"),
		},
	}

	var decodedA SendClientEventRequest
	require.NoError(
		t,
		decodedA.Decode(
			bytes.NewReader(
				encodeTLVMessage(t, reqA),
			),
		),
	)

	var decodedB SendClientEventRequest
	require.NoError(
		t,
		decodedB.Decode(
			bytes.NewReader(
				encodeTLVMessage(t, reqB),
			),
		),
	)

	require.NotEmpty(t, decodedA.MsgID)
	require.NotEmpty(t, decodedA.IdempotencyKey)
	require.Equal(t, decodedA.MsgID, decodedB.MsgID)
	require.Equal(
		t, decodedA.IdempotencyKey, decodedB.IdempotencyKey,
	)
	require.Equal(t, testEventService, decodedA.Service)
	require.Equal(t, testEventMethod, decodedA.Method)
	require.Equal(t, testEventService, decodedB.Service)
	require.Equal(t, testEventMethod, decodedB.Method)

	protoMsg := decodedA.Message.ToProto().UnwrapOrFail(t)
	msg, ok := protoMsg.(*wrapperspb.BytesValue)
	require.True(t, ok)
	require.Equal(t, []byte("same-event"), msg.Value)
}

// TestSendClientEventRequest_TLVRoundTrip_ExplicitIDs verifies explicit
// identifiers survive Encode/Decode unchanged.
func TestSendClientEventRequest_TLVRoundTrip_ExplicitIDs(t *testing.T) {
	t.Parallel()

	req := &SendClientEventRequest{
		Message: &bytesServerMessage{
			payload: []byte("payload"),
		},
		MsgID:          "msg-explicit",
		IdempotencyKey: "idem-explicit",
	}

	var decoded SendClientEventRequest
	require.NoError(
		t,
		decoded.Decode(
			bytes.NewReader(
				encodeTLVMessage(t, req),
			),
		),
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

// TestSendUnaryRequest_TLVRoundTrip verifies durable unary request payload
// serialization round trips with stable derived identifiers.
func TestSendUnaryRequest_TLVRoundTrip(t *testing.T) {
	t.Parallel()

	original, err := NewSendUnaryRequest(
		mailboxrpc.ServiceMethod{
			Service: "test.Svc",
			Method:  "DoThing",
		},
		wrapperspb.String("request"),
		"corr-unary",
	)
	require.NoError(t, err)

	var decoded SendUnaryRequest
	encoded := encodeTLVMessage(t, original)

	require.NoError(
		t, decoded.Decode(bytes.NewReader(encoded)),
	)

	require.NotNil(t, decoded.Body)
	require.NotEmpty(t, decoded.MsgID)
	require.NotEmpty(t, decoded.IdempotencyKey)
	require.Equal(t, original.Service, decoded.Service)
	require.Equal(t, original.Method, decoded.Method)
	require.Equal(t, original.CorrelationID, decoded.CorrelationID)

	payload := &wrapperspb.StringValue{}
	require.NoError(t, decoded.Body.UnmarshalTo(payload))
	require.Equal(t, "request", payload.Value)
}

// TestSendListOORRecipientEventsByScriptRequest_TLVRoundTrip verifies the
// durable recipient-events query message round-trips through TLV encoding.
func TestSendListOORRecipientEventsByScriptRequest_TLVRoundTrip(t *testing.T) {
	t.Parallel()

	original := &SendListOORRecipientEventsByScriptRequest{
		PkScript: []byte{
			0x51,
			0x20,
			0x01,
		},
		AfterEventID:  7,
		Limit:         1,
		CorrelationID: "corr-recipient-query",
	}

	var decoded SendListOORRecipientEventsByScriptRequest
	encoded := encodeTLVMessage(t, original)

	require.NoError(
		t, decoded.Decode(bytes.NewReader(encoded)),
	)

	require.Equal(t, original.PkScript, decoded.PkScript)
	require.Equal(t, original.AfterEventID, decoded.AfterEventID)
	require.Equal(t, original.Limit, decoded.Limit)
	require.Equal(t, original.CorrelationID, decoded.CorrelationID)
	require.NotEmpty(t, decoded.MsgID)
	require.NotEmpty(t, decoded.IdempotencyKey)
}

// TestSendListVTXOsByScriptsRequest_TLVRoundTrip verifies the durable
// VTXO-by-scripts query message round-trips through TLV encoding.
func TestSendListVTXOsByScriptsRequest_TLVRoundTrip(t *testing.T) {
	t.Parallel()

	original := &SendListVTXOsByScriptsRequest{
		PkScripts: [][]byte{
			{
				0x51,
				0x20,
				0x01,
			},
			{
				0x51,
				0x20,
				0x02,
			},
		},
		AfterCursor:   []byte("cursor-11"),
		Limit:         128,
		CorrelationID: "corr-vtxo-query",
	}

	var decoded SendListVTXOsByScriptsRequest
	encoded := encodeTLVMessage(t, original)

	require.NoError(
		t, decoded.Decode(bytes.NewReader(encoded)),
	)

	require.Equal(t, original.PkScripts, decoded.PkScripts)
	require.Equal(t, original.AfterCursor, decoded.AfterCursor)
	require.Equal(t, original.Limit, decoded.Limit)
	require.Equal(t, original.CorrelationID, decoded.CorrelationID)
	require.NotEmpty(t, decoded.MsgID)
	require.NotEmpty(t, decoded.IdempotencyKey)
}

// TestSendListVTXOsByScriptsRequest_EncodeRejectsOversizedCursor verifies the
// durable query encoder refuses attacker-sized opaque cursors.
func TestSendListVTXOsByScriptsRequest_EncodeRejectsOversizedCursor(
	t *testing.T) {

	t.Parallel()

	req := &SendListVTXOsByScriptsRequest{
		PkScripts: [][]byte{
			{
				0x51,
				0x20,
				0x01,
			},
		},
		AfterCursor: make(
			[]byte, indexerlimits.MaxVTXOsByScriptsCursorBytes+1,
		),
		Limit:         128,
		CorrelationID: "corr-vtxo-query",
	}

	var encoded bytes.Buffer
	err := req.Encode(&encoded)
	require.ErrorContains(t, err, "after cursor: vtxo cursor length")
}

// TestSendListVTXOsByScriptsRequest_DecodesLegacyZeroCursor verifies an old
// durable uint64(0) cursor can still replay as the empty keyset cursor.
func TestSendListVTXOsByScriptsRequest_DecodesLegacyZeroCursor(t *testing.T) {
	t.Parallel()

	type (
		legacyCursorTLV = listVTXOsLegacyCursorRecordTLV
		correlationTLV  = listVTXOsCorrelationRecordTLV
		idempotencyTLV  = listVTXOsIdempotencyRecordTLV
	)

	pkScriptsRaw, err := encodeLengthPrefixedBlobList(
		[][]byte{{0x51, 0x20, 0x01}},
	)
	require.NoError(t, err)

	pkScriptsRec := tlv.NewPrimitiveRecord[listVTXOsPkScriptsRecordTLV](
		pkScriptsRaw,
	)
	legacyCursorRec := tlv.NewPrimitiveRecord[legacyCursorTLV](uint64(0))
	limitRec := tlv.NewPrimitiveRecord[listVTXOsLimitRecordTLV](
		uint64(128),
	)
	correlationRec := tlv.NewPrimitiveRecord[correlationTLV](
		[]byte("corr-legacy"),
	)
	msgIDRec := tlv.NewPrimitiveRecord[listVTXOsMsgIDRecordTLV](
		[]byte("msg-legacy"),
	)
	idempotencyRec := tlv.NewPrimitiveRecord[idempotencyTLV](
		[]byte("idem-legacy"),
	)

	stream, err := tlv.NewStream(
		pkScriptsRec.Record(), legacyCursorRec.Record(),
		limitRec.Record(), correlationRec.Record(), msgIDRec.Record(),
		idempotencyRec.Record(),
	)
	require.NoError(t, err)

	var encoded bytes.Buffer
	require.NoError(t, stream.Encode(&encoded))

	var decoded SendListVTXOsByScriptsRequest
	require.NoError(t, decoded.Decode(bytes.NewReader(encoded.Bytes())))
	require.Empty(t, decoded.AfterCursor)
	require.Equal(t, uint32(128), decoded.Limit)
}

// TestNormalizeVTXOAfterCursorRejectsLegacyNonZero verifies old durable
// non-zero cursors are rejected because they cannot be translated to keyset
// pagination safely.
func TestNormalizeVTXOAfterCursorRejectsLegacyNonZero(t *testing.T) {
	t.Parallel()

	_, err := normalizeVTXOAfterCursor(1, true, nil, false)
	require.ErrorContains(t, err, "unsupported legacy vtxo cursor 1")

	cursor, err := normalizeVTXOAfterCursor(
		0, true, []byte("cursor-11"), true,
	)
	require.NoError(t, err)
	require.Equal(t, []byte("cursor-11"), cursor)
}

// TestNormalizeVTXOAfterCursorRejectsOversizedCursor verifies durable replay
// rejects an oversized opaque cursor before copying it into actor state.
func TestNormalizeVTXOAfterCursorRejectsOversizedCursor(t *testing.T) {
	t.Parallel()

	_, err := normalizeVTXOAfterCursor(
		0, false,
		make([]byte, indexerlimits.MaxVTXOsByScriptsCursorBytes+1),
		true,
	)
	require.ErrorContains(t, err, "after cursor: vtxo cursor length")
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

	unaryReq := &SendUnaryRequest{}
	require.Equal(t, "SendUnaryRequest", unaryReq.MessageType())
	require.Equal(t, SendUnaryRequestMsgType, unaryReq.TLVType())
	unaryReq.serverConnMsgSealed()

	recipientReq := &SendListOORRecipientEventsByScriptRequest{}
	require.Equal(
		t, "SendListOORRecipientEventsByScriptRequest",
		recipientReq.MessageType(),
	)
	require.Equal(
		t, SendListOORRecipientEventsByScriptRequestMsgType,
		recipientReq.TLVType(),
	)
	recipientReq.serverConnMsgSealed()

	vtxoReq := &SendListVTXOsByScriptsRequest{}
	require.Equal(
		t, "SendListVTXOsByScriptsRequest", vtxoReq.MessageType(),
	)
	require.Equal(
		t, SendListVTXOsByScriptsRequestMsgType, vtxoReq.TLVType(),
	)
	vtxoReq.serverConnMsgSealed()
}

// TestRawServerMessage_ToProtoDecodeFailure verifies ToProto returns an error
// when the Any payload cannot be resolved through the protobuf type registry.
func TestRawServerMessage_ToProtoDecodeFailure(t *testing.T) {
	t.Parallel()

	raw := &rawServerMessage{
		anyMsg: &anypb.Any{
			TypeUrl: "type.googleapis.com/test.unknown.Message",
			Value: []byte{
				0x01,
			},
		},
	}

	_, toProtoErr := raw.ToProto().Unpack()
	require.Error(t, toProtoErr)
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

	unaryReq, err := NewSendUnaryRequest(
		mailboxrpc.ServiceMethod{
			Service: "test.Svc",
			Method:  "DoThing",
		},
		wrapperspb.String("codec-unary"),
		"corr-codec-unary",
	)
	require.NoError(t, err)

	unaryBytes, err := codec.Encode(unaryReq)
	require.NoError(t, err)

	decodedUnary, err := codec.Decode(unaryBytes)
	require.NoError(t, err)

	typedUnary, ok := decodedUnary.(*SendUnaryRequest)
	require.True(t, ok)
	require.Equal(t, unaryReq.Service, typedUnary.Service)
	require.Equal(t, unaryReq.Method, typedUnary.Method)
	require.Equal(
		t, unaryReq.CorrelationID, typedUnary.CorrelationID,
	)

	recipientReq := &SendListOORRecipientEventsByScriptRequest{
		PkScript: []byte{
			0x51,
			0x20,
			0x04,
		},
		AfterEventID:  9,
		Limit:         1,
		CorrelationID: "corr-codec-recipient",
	}

	recipientBytes, err := codec.Encode(recipientReq)
	require.NoError(t, err)

	decodedRecipient, err := codec.Decode(recipientBytes)
	require.NoError(t, err)

	typedRecipient, ok :=
		decodedRecipient.(*SendListOORRecipientEventsByScriptRequest) //nolint:ll
	require.True(t, ok)
	require.Equal(t, recipientReq.PkScript, typedRecipient.PkScript)
	require.Equal(
		t, recipientReq.CorrelationID, typedRecipient.CorrelationID,
	)

	vtxoReq := &SendListVTXOsByScriptsRequest{
		PkScripts: [][]byte{
			{
				0x51,
				0x20,
				0x05,
			},
			{
				0x51,
				0x20,
				0x06,
			},
		},
		AfterCursor:   []byte("cursor-13"),
		Limit:         128,
		CorrelationID: "corr-codec-vtxo",
	}

	vtxoBytes, err := codec.Encode(vtxoReq)
	require.NoError(t, err)

	decodedVTXO, err := codec.Decode(vtxoBytes)
	require.NoError(t, err)

	typedVTXO, ok := decodedVTXO.(*SendListVTXOsByScriptsRequest)
	require.True(t, ok)
	require.Equal(t, vtxoReq.PkScripts, typedVTXO.PkScripts)
	require.Equal(t, vtxoReq.CorrelationID, typedVTXO.CorrelationID)
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
	result := connector.Receive(
		t.Context(), &unknownServerConnMsg{}, &fakeEgressExec{},
	)
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
	}, &fakeEgressExec{})
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
