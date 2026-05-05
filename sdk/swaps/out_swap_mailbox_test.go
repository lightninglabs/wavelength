package swaps

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/lightninglabs/darepo-client/swaprpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/anypb"
)

type testOutSwapMailboxEdge struct {
	pullResp *mailboxpb.PullResponse
	ackReq   *mailboxpb.AckUpToRequest
}

func (t *testOutSwapMailboxEdge) Send(context.Context,
	*mailboxpb.SendRequest, ...grpc.CallOption) (
	*mailboxpb.SendResponse, error) {

	return nil, nil
}

func (t *testOutSwapMailboxEdge) Pull(context.Context,
	*mailboxpb.PullRequest, ...grpc.CallOption) (
	*mailboxpb.PullResponse, error) {

	return t.pullResp, nil
}

func (t *testOutSwapMailboxEdge) AckUpTo(_ context.Context,
	req *mailboxpb.AckUpToRequest, _ ...grpc.CallOption) (
	*mailboxpb.AckUpToResponse, error) {

	t.ackReq = req

	return &mailboxpb.AckUpToResponse{
		Status: &mailboxpb.Status{Ok: true},
	}, nil
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
			Status:     &mailboxpb.Status{Ok: true},
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
		t, OutSwapMailboxID(authKey.PubKey(), hash),
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
			Status:     &mailboxpb.Status{Ok: true},
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
		t, notification.InArk.SenderPubkey.IsEqual(senderKey.PubKey()),
	)
	require.EqualValues(
		t, 155, notification.InArk.VHTLCConfig.RefundLocktime,
	)
	require.EqualValues(
		t, 30,
		notification.InArk.VHTLCConfig.
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
		t, OutSwapMailboxID(receiverKey.PubKey(), hash),
		edge.ackReq.GetMailboxId(),
	)
	require.Equal(t, uint64(13), edge.ackReq.GetCursor())
}
