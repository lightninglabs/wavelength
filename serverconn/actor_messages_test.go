package serverconn

import (
	"testing"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/internal/indexerlimits"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// bytesServerMessage is a minimal ServerMessage wrapper for actor message
// tests.
type bytesServerMessage struct {
	payload []byte
	key     string
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

// CorrelationKey returns the per-key FIFO lane for tests that need it.
func (m *bytesServerMessage) CorrelationKey() string {
	return m.key
}

// TestSendClientEventRequestCorrelationKey verifies the transport wrapper
// keeps actor-level FIFO routing as an in-memory concern.
func TestSendClientEventRequestCorrelationKey(t *testing.T) {
	t.Parallel()

	req := &SendClientEventRequest{
		Message: &bytesServerMessage{
			payload: []byte("payload"),
			key:     "oor/session-1",
		},
	}

	require.Equal(t, "oor/session-1", req.CorrelationKey())
	require.Empty(t, (&SendClientEventRequest{}).CorrelationKey())
}

// TestSendListOORRecipientEventsByScriptRequestBuildBody verifies recipient
// query messages construct a proof-gated proto body and deterministic identity
// material for SQL-owned egress.
func TestSendListOORRecipientEventsByScriptRequestBuildBody(t *testing.T) {
	t.Parallel()

	req := &SendListOORRecipientEventsByScriptRequest{
		PkScript: []byte{
			0x51,
			0x20,
			0x01,
		},
		AfterEventID:  7,
		Limit:         1,
		CorrelationID: "corr-recipient-query",
	}

	body, stable, err := req.BuildBody(
		t.Context(), &testDurableUnaryBuilder{},
	)
	require.NoError(t, err)
	require.NotNil(t, body)
	require.NotEmpty(t, stable)

	bodyAgain, stableAgain, err := req.BuildBody(
		t.Context(), &testDurableUnaryBuilder{},
	)
	require.NoError(t, err)
	require.True(t, proto.Equal(body, bodyAgain))
	require.Equal(t, stable, stableAgain)

	require.Equal(t, "corr-recipient-query", req.QueryCorrelationID())
	require.Empty(t, req.QueryMsgID())
	require.Empty(t, req.QueryIdempotencyKey())
}

// TestSendListVTXOsByScriptsRequestBuildBody verifies VTXO query messages
// construct a proof-gated proto body and deterministic identity material for
// SQL-owned egress.
func TestSendListVTXOsByScriptsRequestBuildBody(t *testing.T) {
	t.Parallel()

	req := &SendListVTXOsByScriptsRequest{
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

	body, stable, err := req.BuildBody(
		t.Context(), &testDurableUnaryBuilder{},
	)
	require.NoError(t, err)
	require.NotNil(t, body)
	require.NotEmpty(t, stable)

	_, stableAgain, err := req.BuildBody(
		t.Context(), &testDurableUnaryBuilder{},
	)
	require.NoError(t, err)
	require.Equal(t, stable, stableAgain)

	require.Equal(t, "corr-vtxo-query", req.QueryCorrelationID())
	require.Empty(t, req.QueryMsgID())
	require.Empty(t, req.QueryIdempotencyKey())
}

// TestSendListVTXOsByScriptsRequestBuildBodyRejectsOversizedCursor verifies
// the query builder refuses attacker-sized opaque cursors before send.
func TestSendListVTXOsByScriptsRequestBuildBodyRejectsOversizedCursor(
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

	_, _, err := req.BuildBody(t.Context(), &testDurableUnaryBuilder{})
	require.ErrorContains(t, err, "after cursor: vtxo cursor length")
}

// TestServerConnMessageMetadata verifies static message metadata methods.
func TestServerConnMessageMetadata(t *testing.T) {
	t.Parallel()

	eventReq := &SendClientEventRequest{}
	require.Equal(t, "SendClientEventRequest", eventReq.MessageType())
	eventReq.serverConnMsgSealed()

	eventResp := &SendClientEventResponse{}
	require.Equal(t, "SendClientEventResponse", eventResp.MessageType())
	eventResp.serverConnRespSealed()

	rpcReq := &SendRPCRequest{}
	require.Equal(t, "SendRPCRequest", rpcReq.MessageType())
	rpcReq.serverConnMsgSealed()

	unaryReq := &SendUnaryRequest{}
	require.Equal(t, "SendUnaryRequest", unaryReq.MessageType())
	unaryReq.serverConnMsgSealed()

	recipientReq := &SendListOORRecipientEventsByScriptRequest{}
	require.Equal(
		t, "SendListOORRecipientEventsByScriptRequest",
		recipientReq.MessageType(),
	)
	recipientReq.serverConnMsgSealed()

	vtxoReq := &SendListVTXOsByScriptsRequest{}
	require.Equal(
		t, "SendListVTXOsByScriptsRequest", vtxoReq.MessageType(),
	)
	vtxoReq.serverConnMsgSealed()
}

// unknownServerConnMsg is a test-only unsupported message for Receive.
type unknownServerConnMsg struct {
	actor.BaseMessage
}

// MessageType returns a test message type name.
func (m *unknownServerConnMsg) MessageType() string {
	return "unknownServerConnMsg"
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
