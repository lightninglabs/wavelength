package actordelivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/db"
	adsqlc "github.com/lightninglabs/wavelength/db/actordelivery/sqlc"
	mailboxconn "github.com/lightninglabs/wavelength/mailbox/conn"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// TestIngressReceiptsCommitExpiryAndGC proves durable enqueue consumption is
// atomic, survives inbox deletion/reopen, and expires without replay renewal.
// Cleanup has an exact batch bound and does not remove unexpired evidence.
func TestIngressReceiptsCommitExpiryAndGC(t *testing.T) {
	ctx := context.Background()
	testDB := db.NewTestDB(t)
	raw := testDB.DB
	now := time.Unix(1800000000, 0)
	clk := clock.NewTestClock(now)
	store, err := NewTxAwareDeliveryStoreFromDB(
		raw, testDB.Backend(), clk, nil,
	)
	require.NoError(t, err)
	q := adsqlc.New(raw)
	receiptCtx := mailboxconn.WithIngressReceipt(ctx, "lane-a/event-1")
	params := actor.EnqueueParams{
		ID:          "attempt-1",
		MailboxID:   "consumer",
		MessageType: "test",
		Payload:     []byte("money event"),
		AvailableAt: now,
		MaxAttempts: 5,
	}
	err = store.ExecTx(
		receiptCtx, false,
		func(ctx context.Context, tx actor.DeliveryStore) error {
			require.NoError(t, tx.EnqueueMessage(ctx, params))

			return errors.New("crash before commit")
		},
	)
	require.Error(t, err)
	_, err = q.GetIngressReceipt(ctx, "lane-a/event-1")
	require.ErrorIs(t, err, sql.ErrNoRows)
	row, err := store.PeekNextMessage(ctx, "consumer")
	require.NoError(t, err)
	require.Nil(t, row)
	require.NoError(t, store.EnqueueMessage(receiptCtx, params))
	receipt, err := q.GetIngressReceipt(ctx, "lane-a/event-1")
	require.NoError(t, err)
	require.Equal(t, now.Unix(), receipt.ConsumedAt)
	require.Equal(
		t, now.Add(mailboxconn.IngressReceiptRetention).Unix(),
		receipt.ExpiresAt,
	)
	_, err = store.AckMessageByID(ctx, params.ID)
	require.NoError(t, err)
	// A new store object has no in-memory receipt state to preserve.
	store, err = NewTxAwareDeliveryStoreFromDB(
		raw, testDB.Backend(), clk, nil,
	)
	require.NoError(t, err)
	clk.SetTime(now.Add(mailboxconn.IngressReceiptRetention - time.Second))
	params.ID = "fresh-envelope-uuid"
	require.NoError(t, store.EnqueueMessage(receiptCtx, params))
	row, err = store.PeekNextMessage(ctx, "consumer")
	require.NoError(t, err)
	require.Nil(t, row)
	repeated, err := q.GetIngressReceipt(ctx, "lane-a/event-1")
	require.NoError(t, err)
	require.Equal(t, receipt, repeated)
	// Contradictory payload reuse fails closed during retention.
	params.Payload = []byte("different money event")
	require.ErrorContains(
		t, store.EnqueueMessage(receiptCtx, params),
		"different payload",
	)
	params.Payload = []byte("money event")
	// Independent lanes retain their own occurrence even with the same
	// event ID.
	require.NoError(
		t,
		store.EnqueueMessage(
			mailboxconn.WithIngressReceipt(ctx, "lane-b/event-1"),
			params,
		),
	)
	_, err = store.AckMessageByID(ctx, params.ID)
	require.NoError(t, err)
	// At the exact cutoff admission resumes even without physical GC.
	clk.SetTime(now.Add(mailboxconn.IngressReceiptRetention))
	require.NoError(t, store.EnqueueMessage(receiptCtx, params))
	row, err = store.PeekNextMessage(ctx, "consumer")
	require.NoError(t, err)
	require.NotNil(t, row)
	renewed, err := q.GetIngressReceipt(ctx, "lane-a/event-1")
	require.NoError(t, err)
	require.Equal(t, clk.Now().Unix(), renewed.ConsumedAt)
	for i := 0; i < 5; i++ {
		_, err := q.AdmitIngressReceipt(
			ctx, adsqlc.AdmitIngressReceiptParams{
				ID:          fmt.Sprintf("expired-%d", i),
				PayloadHash: []byte{1},
				MailboxID:   "other",
				ConsumedAt:  now.Unix() - 1,
				ExpiresAt:   clk.Now().Unix(),
			},
		)
		require.NoError(t, err)
	}
	pruner, ok := store.(interface {
		PruneIngressReceipts(context.Context, int) (int64, error)
	})
	require.True(t, ok)
	count, err := pruner.PruneIngressReceipts(ctx, 2)
	require.NoError(t, err)
	require.Equal(t, int64(2), count)
	count, err = pruner.PruneIngressReceipts(ctx, 2)
	require.NoError(t, err)
	require.Equal(t, int64(2), count)
	count, err = pruner.PruneIngressReceipts(ctx, 2)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
	_, err = q.GetIngressReceipt(ctx, "lane-a/event-1")
	require.NoError(t, err)
	_, err = q.GetIngressReceipt(ctx, "lane-b/event-1")
	require.NoError(t, err)
}
