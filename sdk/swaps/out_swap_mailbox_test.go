package swaps

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/serverconn/mailboxpull"
	"github.com/lightninglabs/wavelength/swaprpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"
)

// pullStep is one step in a scripted pull sequence: either an error is
// returned (simulating a transport failure) or a response is delivered. Used
// by testOutSwapMailboxEdge to drive disconnect/reconnect scenarios.
type pullStep struct {
	err  error
	resp *mailboxpb.PullResponse
}

type testOutSwapMailboxEdge struct {
	// pullResp, when non-nil, is returned for every Pull call. Legacy
	// single-shot behavior used by tests that don't care about reconnect.
	pullResp *mailboxpb.PullResponse

	// pullScript drives a sequence of Pull outcomes when non-empty. Steps
	// are consumed in order; once exhausted, subsequent calls behave as
	// if the script never ended (so leaked test goroutines time out
	// against ctx rather than panicking).
	pullScript []pullStep

	// pullCursors records the cursor sent on every Pull request so tests
	// can assert that retries reuse the prior cursor instead of resetting
	// to zero.
	pullCursors []uint64

	// pullCalls counts how many times Pull was invoked.
	pullCalls int

	ackReq *mailboxpb.AckUpToRequest
}

func (t *testOutSwapMailboxEdge) Send(context.Context, *mailboxpb.SendRequest,
	...grpc.CallOption) (*mailboxpb.SendResponse, error) {

	return nil, nil
}

func (t *testOutSwapMailboxEdge) Pull(_ context.Context,
	req *mailboxpb.PullRequest, _ ...grpc.CallOption) (
	*mailboxpb.PullResponse, error) {

	t.pullCursors = append(t.pullCursors, req.GetCursor())
	idx := t.pullCalls
	t.pullCalls++

	if len(t.pullScript) > 0 {
		if idx >= len(t.pullScript) {
			// Exhausted script: keep returning the last step so a
			// stuck outer loop times out on ctx rather than
			// dereferencing nil.
			step := t.pullScript[len(t.pullScript)-1]

			return step.resp, step.err
		}
		step := t.pullScript[idx]

		return step.resp, step.err
	}

	return t.pullResp, nil
}

func (t *testOutSwapMailboxEdge) AckUpTo(_ context.Context,
	req *mailboxpb.AckUpToRequest, _ ...grpc.CallOption) (
	*mailboxpb.AckUpToResponse, error) {

	t.ackReq = req

	return &mailboxpb.AckUpToResponse{
		Status: &mailboxpb.Status{
			Ok: true,
		},
	}, nil
}

// fastReceiverBackoff is the zero-wait backoff used by reconnect tests so we
// don't add wall time to the suite.
func fastReceiverBackoff() mailboxpull.BackoffConfig {
	return mailboxpull.BackoffConfig{
		BaseDelay: time.Microsecond,
		MaxDelay:  time.Microsecond,
	}
}

// TestMailboxOutSwapEventReceiverPullsAndAcks verifies that the SDK pulls a
// matching mailbox event and acks it only after the caller accepts it.
func TestMailboxOutSwapEventReceiverPullsAndAcks(t *testing.T) {
	t.Parallel()

	authKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	hash := lntypes.Hash{9, 8, 7}
	protoEvent := &swaprpc.OutSwapHtlcEvent{
		PaymentHash: hash[:],
		AmountSat:   42_000,
		VhtlcConfig: &swaprpc.VHTLCConfig{
			RefundLocktime:                       144,
			UnilateralClaimDelay:                 12,
			UnilateralRefundDelay:                24,
			UnilateralRefundWithoutReceiverDelay: 36,
			SwapserverPubkey: authKey.PubKey().
				SerializeCompressed(),
		},
	}
	body, err := anypb.New(&swaprpc.SwapMailboxEvent{
		Event: &swaprpc.SwapMailboxEvent_OutSwapHtlc{
			OutSwapHtlc: protoEvent,
		},
	})
	require.NoError(t, err)

	edge := &testOutSwapMailboxEdge{
		pullResp: &mailboxpb.PullResponse{
			Status: &mailboxpb.Status{
				Ok: true,
			},
			NextCursor: 8,
			Envelopes: []*mailboxpb.Envelope{{
				Type:     outSwapMailboxEventType,
				Body:     body,
				EventSeq: 7,
				Rpc: &mailboxpb.RpcMeta{
					Kind: mailboxpb.
						RpcMeta_KIND_EVENT,
					Service: outSwapMailboxEventService,
					Method:  outSwapMailboxEventMethod,
				},
			}},
		},
	}
	receiver := NewMailboxOutSwapEventReceiver(edge, "")
	receiver.pullWaitTimeout = time.Millisecond

	notification, err := receiver.WaitOutSwapHtlc(
		t.Context(), hash, authKey.PubKey(),
	)
	require.NoError(t, err)
	require.NotNil(t, notification)
	require.Equal(t, hash, notification.Event.PaymentHash)
	require.EqualValues(t, 42_000, notification.Event.AmountSat)
	require.Equal(t, uint64(8), notification.AckCursor)
	require.Nil(t, edge.ackReq)

	require.NoError(t, notification.Ack(t.Context()))
	require.NotNil(t, edge.ackReq)
	require.Equal(
		t,
		OutSwapMailboxID(
			authKey.PubKey(), hash,
		),
		edge.ackReq.GetMailboxId(),
	)
	require.Equal(t, uint64(8), edge.ackReq.GetCursor())
}

// TestMailboxOutSwapEventReceiverPullsInArkEvent verifies that same-Ark vHTLC
// events use the shared mailbox envelope and carry ack metadata.
func TestMailboxOutSwapEventReceiverPullsInArkEvent(t *testing.T) {
	t.Parallel()

	senderKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	receiverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	hash := lntypes.Hash{4, 5, 6}
	protoEvent := &swaprpc.InArkHtlcEvent{
		PaymentHash:  hash[:],
		AmountSat:    21_000,
		SenderPubkey: senderKey.PubKey().SerializeCompressed(),
		VhtlcConfig: &swaprpc.VHTLCConfig{
			RefundLocktime:                       155,
			UnilateralClaimDelay:                 10,
			UnilateralRefundDelay:                20,
			UnilateralRefundWithoutReceiverDelay: 30,
			SwapserverPubkey: senderKey.PubKey().
				SerializeCompressed(),
		},
		VhtlcOutpoint:  "txid:0",
		VhtlcAmountSat: 21_500,
	}
	body, err := anypb.New(&swaprpc.SwapMailboxEvent{
		Event: &swaprpc.SwapMailboxEvent_InArkHtlc{
			InArkHtlc: protoEvent,
		},
	})
	require.NoError(t, err)

	edge := &testOutSwapMailboxEdge{
		pullResp: &mailboxpb.PullResponse{
			Status: &mailboxpb.Status{
				Ok: true,
			},
			NextCursor: 13,
			Envelopes: []*mailboxpb.Envelope{{
				Type:     outSwapMailboxEventType,
				Body:     body,
				EventSeq: 12,
				Rpc: &mailboxpb.RpcMeta{
					Kind: mailboxpb.
						RpcMeta_KIND_EVENT,
					Service: outSwapMailboxEventService,
					Method:  outSwapMailboxEventMethod,
				},
			}},
		},
	}
	receiver := NewMailboxOutSwapEventReceiver(edge, "")
	receiver.pullWaitTimeout = time.Millisecond

	notification, err := receiver.WaitIncomingVHTLC(
		t.Context(), hash, receiverKey.PubKey(),
	)
	require.NoError(t, err)
	require.NotNil(t, notification)
	require.Nil(t, notification.OutSwap)
	require.NotNil(t, notification.InArk)
	require.Equal(t, hash, notification.InArk.PaymentHash)
	require.EqualValues(t, 21_000, notification.InArk.AmountSat)
	require.True(
		t,
		notification.InArk.SenderPubkey.IsEqual(
			senderKey.PubKey(),
		),
	)
	require.EqualValues(
		t, 155, notification.InArk.VHTLCConfig.RefundLocktime,
	)
	require.EqualValues(
		t, 30, notification.InArk.VHTLCConfig.
			UnilateralRefundWithoutReceiverDelay,
	)
	require.Equal(t, "txid:0", notification.InArk.VHTLCOutpoint)
	require.EqualValues(t, 21_500, notification.InArk.VHTLCAmountSat)
	require.Equal(t, uint64(13), notification.AckCursor)
	require.NotNil(t, notification.Ack)
	require.Nil(t, edge.ackReq)

	require.NoError(t, notification.Ack(t.Context()))
	require.NotNil(t, edge.ackReq)
	require.Equal(
		t,
		OutSwapMailboxID(
			receiverKey.PubKey(), hash,
		),
		edge.ackReq.GetMailboxId(),
	)
	require.Equal(t, uint64(13), edge.ackReq.GetCursor())
}

// TestMailboxOutSwapEventReceiverPullsForfeitSignatureRequest verifies that
// out-swap refresh signing requests use the shared swap mailbox envelope and
// surface a durable ack hook.
func TestMailboxOutSwapEventReceiverPullsForfeitSignatureRequest(t *testing.T) {
	t.Parallel()

	receiverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	hash := lntypes.Hash{7, 7, 7}
	protoPayload := &swaprpc.ForfeitSignaturePayload{
		RequestId:             []byte("request-id"),
		PaymentHash:           hash[:],
		VhtlcOutpoint:         "vhtlc:0",
		VhtlcAmountSat:        42_000,
		VhtlcPkScript:         []byte("vhtlc-pk-script"),
		VhtlcPolicyTemplate:   []byte("policy"),
		ForfeitSpendPath:      []byte("forfeit-path"),
		UnsignedForfeitTx:     []byte("unsigned-tx"),
		ConnectorOutpoint:     "connector:0",
		ConnectorAmountSat:    330,
		ConnectorPkScript:     []byte("connector-pk-script"),
		ServerForfeitPkScript: []byte("server-forfeit-pk-script"),
	}
	body, err := anypb.New(&swaprpc.SwapMailboxEvent{
		Event: &swaprpc.SwapMailboxEvent_OutSwapForfeitSignatureRequest{
			OutSwapForfeitSignatureRequest: &swaprpc.
				OutSwapForfeitSignatureRequest{
				Payload: protoPayload,
			},
		},
	})
	require.NoError(t, err)

	edge := &testOutSwapMailboxEdge{
		pullResp: &mailboxpb.PullResponse{
			Status: &mailboxpb.Status{
				Ok: true,
			},
			NextCursor: 22,
			Envelopes: []*mailboxpb.Envelope{{
				Type:     outSwapMailboxEventType,
				Body:     body,
				EventSeq: 21,
				Rpc: &mailboxpb.RpcMeta{
					Kind: mailboxpb.
						RpcMeta_KIND_EVENT,
					Service: outSwapMailboxEventService,
					Method:  outSwapMailboxEventMethod,
				},
			}},
		},
	}
	receiver := NewMailboxOutSwapEventReceiver(edge, "")
	receiver.pullWaitTimeout = time.Millisecond

	notification, err := receiver.WaitOutSwapForfeitSignature(
		t.Context(), hash, receiverKey.PubKey(),
	)
	require.NoError(t, err)
	require.NotNil(t, notification)
	require.Equal(t, uint64(22), notification.AckCursor)
	require.Equal(t, []byte("request-id"), notification.Payload.RequestID)
	require.Equal(t, hash, notification.Payload.PaymentHash)
	require.Equal(t, "vhtlc:0", notification.Payload.VHTLCOutpoint)
	require.EqualValues(t, 42_000, notification.Payload.VHTLCAmountSat)
	require.Equal(
		t, []byte("server-forfeit-pk-script"),
		notification.Payload.ServerForfeitPkScript,
	)
	require.Nil(t, edge.ackReq)

	require.NoError(t, notification.Ack(t.Context()))
	require.NotNil(t, edge.ackReq)
	require.Equal(
		t,
		OutSwapMailboxID(
			receiverKey.PubKey(), hash,
		),
		edge.ackReq.GetMailboxId(),
	)
	require.Equal(t, uint64(22), edge.ackReq.GetCursor())
}

// successfulOutSwapPullResp builds a one-envelope PullResponse carrying the
// out-swap HTLC event matching paymentHash. Used by reconnect tests so each
// case can wire its own scripted sequence of failures + this final success.
func successfulOutSwapPullResp(t *testing.T, paymentHash lntypes.Hash,
	swapserverKey *btcec.PublicKey, eventSeq,
	nextCursor uint64) (*mailboxpb.PullResponse, error) {

	t.Helper()

	// Hoist the inner event so the VHTLCConfig literal sits at a shallower
	// indent level. gofmt aligns struct field colons to the longest field
	// name (UnilateralRefundWithoutReceiverDelay = 38 chars), and at the
	// deeper anypb.New(...) nesting that alignment overflows the 80-char
	// limit on every adjacent field.
	protoEvent := &swaprpc.OutSwapHtlcEvent{
		PaymentHash: paymentHash[:],
		AmountSat:   1_000,
		VhtlcConfig: &swaprpc.VHTLCConfig{
			RefundLocktime:                       144,
			UnilateralClaimDelay:                 12,
			UnilateralRefundDelay:                24,
			UnilateralRefundWithoutReceiverDelay: 36,
			SwapserverPubkey: swapserverKey.
				SerializeCompressed(),
		},
	}
	body, err := anypb.New(&swaprpc.SwapMailboxEvent{
		Event: &swaprpc.SwapMailboxEvent_OutSwapHtlc{
			OutSwapHtlc: protoEvent,
		},
	})
	if err != nil {
		return nil, err
	}

	return &mailboxpb.PullResponse{
		Status: &mailboxpb.Status{
			Ok: true,
		},
		NextCursor: nextCursor,
		Envelopes: []*mailboxpb.Envelope{{
			Type:     outSwapMailboxEventType,
			Body:     body,
			EventSeq: eventSeq,
			Rpc: &mailboxpb.RpcMeta{
				Kind:    mailboxpb.RpcMeta_KIND_EVENT,
				Service: outSwapMailboxEventService,
				Method:  outSwapMailboxEventMethod,
			},
		}},
	}, nil
}

// TestMailboxOutSwapEventReceiverRetriesOnUnavailable verifies that transient
// codes.Unavailable errors (which is what a TCP RST against the mailbox
// endpoint looks like to gRPC) are retried until the receiver gets a real
// event, instead of failing the swap on the first blip.
//
// This is the regression test for issue #505.
func TestMailboxOutSwapEventReceiverRetriesOnUnavailable(t *testing.T) {
	t.Parallel()

	authKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	hash := lntypes.Hash{1, 2, 3}
	successResp, err := successfulOutSwapPullResp(
		t, hash, authKey.PubKey(), 11, 12,
	)
	require.NoError(t, err)

	edge := &testOutSwapMailboxEdge{
		pullScript: []pullStep{
			{
				err: status.Error(codes.Unavailable, "rst1"),
			},
			{
				err: status.Error(codes.Unavailable, "rst2"),
			},
			{
				err: status.Error(codes.Unavailable, "rst3"),
			},
			{
				resp: successResp,
			},
		},
	}
	receiver := NewMailboxOutSwapEventReceiver(
		edge, "",
		WithMailboxPullBackoff(
			fastReceiverBackoff(),
		),
	)
	receiver.pullWaitTimeout = time.Millisecond

	notification, err := receiver.WaitOutSwapHtlc(
		t.Context(), hash, authKey.PubKey(),
	)
	require.NoError(t, err)
	require.NotNil(t, notification)
	require.Equal(t, hash, notification.Event.PaymentHash)
	require.Equal(t, uint64(12), notification.AckCursor)

	// Three transport failures + one success.
	require.Equal(t, 4, edge.pullCalls)
}

// TestMailboxOutSwapEventReceiverRespectsCtxCancel verifies that a cancelled
// ctx is honored promptly even when the endpoint keeps flapping; the receiver
// must not loop forever after the caller (typically the swap FSM shutting
// down) bails out.
func TestMailboxOutSwapEventReceiverRespectsCtxCancel(t *testing.T) {
	t.Parallel()

	authKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	hash := lntypes.Hash{4, 5, 6}
	edge := &testOutSwapMailboxEdge{
		pullScript: []pullStep{
			{
				err: status.Error(codes.Unavailable, "rst"),
			},
		},
	}
	receiver := NewMailboxOutSwapEventReceiver(
		edge, "",
		WithMailboxPullBackoff(
			fastReceiverBackoff(),
		),
	)
	receiver.pullWaitTimeout = time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = receiver.WaitOutSwapHtlc(ctx, hash, authKey.PubKey())
	require.ErrorIs(t, err, context.Canceled)
}

// TestMailboxOutSwapEventReceiverPreservesCursorAcrossRetry verifies that
// when a pull advances the cursor (e.g. by returning an empty batch with a
// non-zero NextCursor) and a subsequent attempt fails transiently, the
// retried pull resumes from the advanced cursor rather than rewinding to
// zero. Without cursor preservation the receiver would re-scan envelopes
// the server has already delivered, potentially double-processing events.
func TestMailboxOutSwapEventReceiverPreservesCursorAcrossRetry(t *testing.T) {
	t.Parallel()

	authKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	hash := lntypes.Hash{7, 8, 9}
	successResp, err := successfulOutSwapPullResp(
		t, hash, authKey.PubKey(), 6, 7,
	)
	require.NoError(t, err)

	edge := &testOutSwapMailboxEdge{
		pullScript: []pullStep{
			// First pull: empty batch, but advance the cursor to
			// 5 so the next iteration of the outer loop carries
			// that cursor forward.
			{resp: &mailboxpb.PullResponse{
				Status: &mailboxpb.Status{
					Ok: true,
				},
				NextCursor: 5,
			}},
			// Transport blip: must not reset the cursor.
			{
				err: status.Error(codes.Unavailable, "rst"),
			},
			// Server eventually delivers the real event.
			{
				resp: successResp,
			},
		},
	}
	receiver := NewMailboxOutSwapEventReceiver(
		edge, "",
		WithMailboxPullBackoff(
			fastReceiverBackoff(),
		),
	)
	receiver.pullWaitTimeout = time.Millisecond

	notification, err := receiver.WaitOutSwapHtlc(
		t.Context(), hash, authKey.PubKey(),
	)
	require.NoError(t, err)
	require.NotNil(t, notification)
	require.Equal(t, uint64(7), notification.AckCursor)

	// Three Pull calls: empty batch (cursor 0), failed retry attempt
	// (cursor 5), success (cursor 5).
	require.Equal(
		t, []uint64{0, 5, 5}, edge.pullCursors,
		"cursor must not rewind to zero on transient error",
	)
}
