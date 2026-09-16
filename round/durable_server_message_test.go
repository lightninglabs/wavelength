package round

import (
	"bytes"
	"testing"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestDurableServerMessageReplay reconstructs a server push from the runtime
// codec without retaining the original protobuf or domain event in memory.
func TestDurableServerMessageReplay(t *testing.T) {
	id := testRoundID("durable-wire-event")
	quoteID := bytes.Repeat([]byte{1}, 32)
	payload := &roundpb.JoinRoundQuote{
		RoundId: id.String(), QuoteId: quoteID,
		OperatorFeeSat: 11, QuoteExpiresAt: 1700000000,
	}
	notification, err := NewServerMessageNotification(
		roundpb.MethodJoinRoundQuote, payload,
	)
	require.NoError(t, err)
	payload.OperatorFeeSat = 99
	payload.QuoteId[0] = 2
	codec := actor.NewMessageCodec()
	require.NoError(
		t, codec.Register(
			durableServerMessageType, func() actor.TLVMessage {
				return &ServerMessageNotification{}
			},
		),
	)
	raw, err := codec.Encode(notification)
	require.NoError(t, err)
	decoded, err := codec.Decode(raw)
	require.NoError(t, err)
	replayed, ok := decoded.(*ServerMessageNotification)
	require.True(t, ok)
	require.Nil(t, replayed.Message)
	require.NoError(t, replayed.restoreWireEvent(0))
	require.Equal(t, notification.Message, replayed.Message)
	quote, ok := replayed.Message.(*JoinRoundQuoteReceived)
	require.True(t, ok)
	require.EqualValues(t, 11, quote.Quote.OperatorFeeSat)
	require.EqualValues(t, 1, quote.Quote.QuoteID[0])
	again, err := codec.Encode(replayed)
	require.NoError(t, err)
	require.Equal(t, raw, again)
}

// TestDurableServerMessageMethodBinding requires the same method and protobuf
// pairing used by the transport router and rejects typed nil payloads.
func TestDurableServerMessageMethodBinding(t *testing.T) {
	_, err := NewServerMessageNotification(
		roundpb.MethodJoinAck, &roundpb.JoinRoundQuote{},
	)
	require.Error(t, err)
	var missing *roundpb.ClientSuccessResp
	_, err = NewServerMessageNotification(roundpb.MethodJoinAck, missing)
	require.Error(t, err)
}

// TestDurableServerMessageReplayTreeLimit applies the receiving actor's tree
// limit before deserializing the stored batch tree.
func TestDurableServerMessageReplayTreeLimit(t *testing.T) {
	id := testRoundID("durable-wire-limit")
	payload := &roundpb.ClientBatchInfo{
		RoundId: id[:], BatchPsbt: validPSBTBytes(t),
		VtxoTreePaths: map[int32]*roundpb.VTXOTree{
			0: {
				Nodes: []*roundpb.TreeNode{
					{},
					{},
				},
			},
		},
	}
	raw, err := proto.Marshal(payload)
	require.NoError(t, err)
	replayed := &ServerMessageNotification{
		wireMethod: []byte(roundpb.MethodBatchInfo), wirePayload: raw,
	}
	err = replayed.restoreWireEvent(1)
	require.ErrorContains(t, err, "tree has 2 nodes, exceeds maximum 1")
	require.Nil(t, replayed.Message)
}
