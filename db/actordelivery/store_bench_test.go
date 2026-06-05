package actordelivery

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/db"
	adsqlc "github.com/lightninglabs/darepo-client/db/actordelivery/sqlc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// newBenchStore builds an actor delivery store backed by a fresh test database
// for benchmarking the durable mailbox and checkpoint write paths -- the "drip
// box" the OOR refactor reshapes.
func newBenchStore(b *testing.B) *Store {
	b.Helper()

	testDB := db.NewTestDB(b)
	actorQueries := adsqlc.New(testDB.DB)

	actorDB := db.NewTransactionExecutor(
		testDB.BaseDB,
		func(tx *sql.Tx) ActorDeliveryQueries {
			return actorQueries.WithTx(tx)
		},
		btclog.Disabled,
	)

	return NewStore(actorDB, clock.NewDefaultClock())
}

// BenchmarkDeliveryCheckpointWrite measures the cost of one FSM checkpoint
// write as the persisted state blob grows. It isolates exactly the write the
// old global OOR actor paid on every mutation: it serialized ALL sessions into
// one blob, so the per-mutation cost grew with the number of in-flight
// sessions. The per-session refactor replaces this with one small fixed-size
// row per session, so this benchmark is the "before" curve the refactor
// flattens.
func BenchmarkDeliveryCheckpointWrite(b *testing.B) {
	sizes := []int{
		1 << 10, // 1 KiB  -- a handful of sessions.
		1 << 14, // 16 KiB -- dozens of sessions.
		1 << 18, // 256 KiB -- a saturated whole-map blob.
	}

	for _, size := range sizes {
		b.Run(fmt.Sprintf("blob=%dB", size), func(b *testing.B) {
			ctx := context.Background()
			store := newBenchStore(b)
			blob := make([]byte, size)

			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				err := store.SaveCheckpoint(
					ctx, actor.CheckpointParams{
						ActorID:   "bench-actor",
						StateType: "oor.sessions",
						StateData: blob,
						Version:   int64(i),
					},
				)
				require.NoError(b, err)
			}
		})
	}
}

// BenchmarkDeliveryMailboxRoundTrip measures one message's full durable trip
// through a single mailbox: enqueue, lease, ack. A single global actor funnels
// every session's messages through one such serial trip; the per-session
// refactor spreads them across independent mailboxes, so this is the
// per-message floor each shard pays in parallel rather than in series.
func BenchmarkDeliveryMailboxRoundTrip(b *testing.B) {
	ctx := context.Background()
	store := newBenchStore(b)
	payload := make([]byte, 256)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		id := fmt.Sprintf("bench-msg-%d", i)
		err := store.EnqueueMessage(ctx, actor.EnqueueParams{
			ID:          id,
			MailboxID:   "bench-mailbox",
			MessageType: "BenchMsg",
			Payload:     payload,
			AvailableAt: time.Now().Add(-time.Minute),
			MaxAttempts: 3,
		})
		require.NoError(b, err)

		leaseToken := fmt.Sprintf("lease-%d", i)
		leased, err := store.LeaseNextMessage(
			ctx, "bench-mailbox", leaseToken, 30*time.Second,
		)
		require.NoError(b, err)
		require.NotNil(b, leased)

		rows, err := store.AckMessage(ctx, leased.ID, leaseToken)
		require.NoError(b, err)
		require.Equal(b, int64(1), rows)
	}
}
