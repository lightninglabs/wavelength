package actordelivery

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// benchSendLatency is the simulated wire-send (Edge.Send) latency each handler
// incurs. It models a network round-trip to the operator's mailbox, which
// dominates the local DB write cost, so throughput is governed by how many of
// these sends can be in flight at once. Keeping it well above the per-commit
// fsync cost is what makes the writer-contention difference visible: with a
// tiny send the durable store's fsync would be the bottleneck and pooling could
// not help regardless of execution path.
const benchSendLatency = 20 * time.Millisecond

// slowClassicBehavior is a classic ActorBehavior whose handler sleeps to
// simulate a slow outbound send. Against a tx-aware store the runtime wraps the
// whole handler in one writer transaction (processInTransaction), so the SQLite
// writer is held for the entire send -- the pre-migration serverconn egress
// shape. Even with many workers the writer lock serializes every send.
type slowClassicBehavior struct {
	done *atomic.Int64
}

// Receive sleeps to model the send, then records completion.
func (b slowClassicBehavior) Receive(_ context.Context,
	_ *keyedTestMsg) fn.Result[int] {

	time.Sleep(benchSendLatency)
	b.done.Add(1)

	return fn.Ok(1)
}

// slowTxBehavior is the Read/Commit counterpart: it sleeps with no writer held,
// then commits to consume the message. This is the migrated serverconn egress
// shape, where a worker pool can run many sends concurrently.
type slowTxBehavior struct {
	done *atomic.Int64
}

// bindStores returns the empty per-message store.
func (b slowTxBehavior) bindStores(context.Context,
	actor.DeliveryStore) fifoTx {

	return fifoTx{}
}

// Receive sleeps outside any transaction, then commits.
func (b slowTxBehavior) Receive(ctx context.Context, _ *keyedTestMsg,
	ax actor.Exec[fifoTx]) fn.Result[int] {

	time.Sleep(benchSendLatency)

	commit := func(context.Context, fifoTx) error { return nil }
	if err := ax.Commit(ctx, commit); err != nil {
		return fn.Err[int](err)
	}

	b.done.Add(1)

	return fn.Ok(1)
}

// benchEnqueue inserts a single unkeyed message directly into the store.
// Unkeyed messages share no FIFO lane, so the pool may drain them fully in
// parallel -- isolating the writer-lock contention that the benchmark measures.
func benchEnqueue(b *testing.B, store actor.TxAwareDeliveryStore, mailbox,
	id string, createdAt time.Time, codec *actor.MessageCodec) {

	msg := newKeyedTestMsg(id, "")
	payload, err := codec.Encode(msg)
	require.NoError(b, err)

	require.NoError(
		b,
		store.EnqueueMessage(
			context.Background(), actor.EnqueueParams{
				ID:          id,
				MailboxID:   mailbox,
				MessageType: msg.MessageType(),
				Payload:     payload,
				AvailableAt: createdAt,
				MaxAttempts: 5,
			},
		),
	)
}

// benchmarkDrain pre-enqueues b.N unkeyed messages, then times how long the
// configured durable actor takes to drain them, reporting messages per second.
// The classic path holds the writer across each send, so its throughput is
// ~1/benchSendLatency regardless of worker count; the Read/Commit pool runs
// sends concurrently, so its throughput scales with the worker count.
func benchmarkDrain(b *testing.B, numWorkers int, readCommit bool) {
	rawDB := newConcurrentSQLiteDB(b)
	require.NoError(b, RunMigrations(rawDB, sqlc.BackendTypeSqlite))

	store, err := NewTxAwareDeliveryStoreFromDB(
		rawDB, sqlc.BackendTypeSqlite, clock.NewDefaultClock(),
		btclog.Disabled,
	)
	require.NoError(b, err)

	const mailbox = "bench-egress"
	codec := newKeyedTestCodec()

	now := time.Now()
	for i := 0; i < b.N; i++ {
		benchEnqueue(
			b, store, mailbox, fmt.Sprintf("m-%d", i),
			now.Add(time.Duration(i)*time.Microsecond), codec,
		)
	}

	var done atomic.Int64

	var cfg actor.DurableActorConfig[*keyedTestMsg, int]
	if readCommit {
		beh := slowTxBehavior{done: &done}
		cfg = actor.DefaultDurableTxActorConfig[
			*keyedTestMsg, int, fifoTx,
		](
			mailbox, beh, beh.bindStores, store, codec,
		)
	} else {
		cfg = actor.DefaultDurableActorConfig[*keyedTestMsg, int](
			mailbox, slowClassicBehavior{
				done: &done,
			},
			store,
			codec,
		)

		// Pooling the classic path is forbidden in production (the
		// guard rejects NumWorkers > 1 on a classic behavior), but the
		// benchmark deliberately measures it to show it gains nothing,
		// so flip the test-only escape hatch.
		cfg = cfg.AllowConcurrentClassicBehavior()
	}
	cfg.NumWorkers = numWorkers
	cfg.PollInterval = 2 * time.Millisecond
	cfg.LeaseDuration = 30 * time.Second
	cfg.HeartbeatInterval = 10 * time.Second

	a, err := actor.NewDurableActor(cfg).Unpack()
	require.NoError(b, err)

	b.ResetTimer()
	start := time.Now()
	a.Start()

	for done.Load() < int64(b.N) {
		time.Sleep(time.Millisecond)
	}

	elapsed := time.Since(start)
	b.StopTimer()

	stopCtx, cancel := context.WithTimeout(
		context.Background(), 10*time.Second,
	)
	require.NoError(b, a.StopAndWait(stopCtx))
	cancel()

	b.ReportMetric(float64(b.N)/elapsed.Seconds(), "msgs/sec")
}

// BenchmarkEgressClassicW1 measures the legacy single-sender classic path.
func BenchmarkEgressClassicW1(b *testing.B) { benchmarkDrain(b, 1, false) }

// BenchmarkEgressClassicW4 shows that adding workers does NOT help the classic
// path: the writer lock held across each send serializes them anyway.
func BenchmarkEgressClassicW4(b *testing.B) { benchmarkDrain(b, 4, false) }

// BenchmarkEgressReadCommitW1 measures the migrated single-worker path: the
// writer is freed during the send, but one loop still sends serially.
func BenchmarkEgressReadCommitW1(b *testing.B) { benchmarkDrain(b, 1, true) }

// BenchmarkEgressReadCommitW4 shows the payoff: four workers send concurrently
// because no writer is held across the send, multiplying throughput.
func BenchmarkEgressReadCommitW4(b *testing.B) { benchmarkDrain(b, 4, true) }
