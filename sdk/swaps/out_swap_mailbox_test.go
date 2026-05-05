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
	body, err := anypb.New(protoEvent)
	require.NoError(t, err)

	edge := &testOutSwapMailboxEdge{
		pullResp: &mailboxpb.PullResponse{
			Status:     &mailboxpb.Status{Ok: true},
			NextCursor: 8,
			Envelopes: []*mailboxpb.Envelope{
				{
					Type:     outSwapHtlcEventType,
					Body:     body,
					EventSeq: 7,
					Rpc: &mailboxpb.RpcMeta{
						Kind: mailboxpb.
							RpcMeta_KIND_EVENT,
						Service: outSwapHtlcEventService,
						Method:  outSwapHtlcEventMethod,
					},
				},
			},
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
	require.Nil(t, edge.ackReq)

	require.NoError(t, notification.Ack(t.Context()))
	require.NotNil(t, edge.ackReq)
	require.Equal(
		t, OutSwapMailboxID(authKey.PubKey(), hash),
		edge.ackReq.GetMailboxId(),
	)
	require.Equal(t, uint64(8), edge.ackReq.GetCursor())
}
