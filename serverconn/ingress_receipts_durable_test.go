package serverconn_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// TestIngressEventReceiptsSurviveRestart drives the actual router, durable
// mailbox and ingress loop. A consumed occurrence survives remote ACK and local
// inbox deletion, while a new identical response and legacy events still
// arrive.
func TestIngressEventReceiptsSurviveRestart(t *testing.T) {
	ctx := context.Background()
	dsn := ingressTestDSN(t)
	raw, store := openIngressStore(t, dsn)
	lane := t.Name() + uuid.NewString()
	occurrence := "delivery-v1:" + uuid.NewString()
	body, err := anypb.New(wrapperspb.Int64(7))
	require.NoError(t, err)
	run := func(id string, seq uint64) {
		routes := quarantineTestRouter(t, store, lane, true)
		env := &mailboxpb.Envelope{
			ProtocolVersion:    1,
			ArkProtocolVersion: 1,
			EventSeq:           seq,
			MsgId:              id,
			IdempotencyKey:     "same-semantic-key",
			Sender:             "server",
			Recipient:          lane,
			Body:               body,
			Rpc: &mailboxpb.RpcMeta{
				Kind:    mailboxpb.RpcMeta_KIND_EVENT,
				Service: "test",
				Method:  "Event",
			},
		}
		done := make(chan struct{})
		edge := &durableIngressEdge{}
		edge.pull = func(ctx context.Context,
			req *mailboxpb.PullRequest) (*mailboxpb.PullResponse,
			error) {

			if req.Cursor > seq {
				<-ctx.Done()

				return nil, ctx.Err()
			}

			return &mailboxpb.PullResponse{
				Status: &mailboxpb.Status{
					Ok: true,
				},
				Envelopes: []*mailboxpb.Envelope{
					env,
				},
				NextCursor: seq + 1,
			}, nil
		}
		edge.ack = func(_ context.Context,
			req *mailboxpb.AckUpToRequest) (
			*mailboxpb.AckUpToResponse, error) {

			if req.Cursor == seq+1 {
				close(done)
			}

			return &mailboxpb.AckUpToResponse{
				Status: &mailboxpb.Status{
					Ok: true,
				},
			}, nil
		}
		cfg := serverconn.DefaultConnectorConfig()
		cfg.ArkProtocolVersion = 1
		cfg.LocalMailboxID = lane
		cfg.RemoteMailboxID = "server"
		cfg.Store = store
		cfg.Edge = edge
		cfg.Dispatchers = routes
		runDurableIngress(t, cfg, done)
	}
	run(occurrence, 1)
	row, err := store.PeekNextMessage(ctx, lane)
	require.NoError(t, err)
	require.NotNil(t, row)
	originalID := row.ID
	_, err = store.AckMessageByID(ctx, row.ID)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	_, store = openIngressStore(t, dsn)
	run(occurrence, 7)
	row, err = store.PeekNextMessage(ctx, lane)
	require.NoError(t, err)
	require.Nil(t, row)
	// An independently enqueued identical response keeps its own
	// occurrence.
	run("delivery-v1:"+uuid.NewString(), 9)
	row, err = store.PeekNextMessage(ctx, lane)
	require.NoError(t, err)
	require.NotNil(t, row)
	require.NotEqual(t, originalID, row.ID)
	_, err = store.AckMessageByID(ctx, row.ID)
	require.NoError(t, err)
	for _, seq := range []uint64{11, 13} {
		run("evt-legacy-body-hash", seq)
		row, err = store.PeekNextMessage(ctx, lane)
		require.NoError(t, err)
		require.NotNil(t, row)
		_, err = store.AckMessageByID(ctx, row.ID)
		require.NoError(t, err)
	}
}
