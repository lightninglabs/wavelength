package actordelivery

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// keyedTestMsg is a TLVMessage carrying an explicit correlation key. The
// reorder test enqueues two of these with the same key to exercise the
// per-key FIFO claim invariant through the full DurableActor +
// SQLite-backed store stack.
type keyedTestMsg struct {
	actor.BaseMessage

	ID  tlv.RecordT[tlv.TlvType1, []byte]
	Key tlv.RecordT[tlv.TlvType2, []byte]
}

func newKeyedTestMsg(id, key string) *keyedTestMsg {
	return &keyedTestMsg{
		ID:  tlv.NewPrimitiveRecord[tlv.TlvType1, []byte]([]byte(id)),
		Key: tlv.NewPrimitiveRecord[tlv.TlvType2, []byte]([]byte(key)),
	}
}

// MessageType returns the type name used by the durable actor framework
// for routing/dispatch logging.
func (m *keyedTestMsg) MessageType() string {
	return "actordelivery.KeyedTestMsg"
}

// TLVType returns the codec dispatch id for keyedTestMsg.
func (m *keyedTestMsg) TLVType() tlv.Type {
	return 0x4000
}

// Encode writes the message's two TLV records to w.
func (m *keyedTestMsg) Encode(w io.Writer) error {
	stream, err := tlv.NewStream(m.ID.Record(), m.Key.Record())
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode reads the message back from r.
func (m *keyedTestMsg) Decode(r io.Reader) error {
	stream, err := tlv.NewStream(m.ID.Record(), m.Key.Record())
	if err != nil {
		return err
	}

	_, err = stream.DecodeWithParsedTypes(r)

	return err
}

// CorrelationKey returns the message's per-mailbox FIFO key. Two
// keyedTestMsgs with the same Key value participate in the same FIFO
// lane, regardless of the durable mailbox's retry backoff timing.
func (m *keyedTestMsg) CorrelationKey() string {
	return string(m.Key.Val)
}

// msgID is a small helper for asserting the order of observed messages.
func (m *keyedTestMsg) msgID() string {
	return string(m.ID.Val)
}

// failOnceBehavior is an ActorBehavior that returns an error on its
// first invocation for a given message ID, then succeeds on every
// subsequent invocation. It also records the order in which it observed
// successful processing so the test can assert reorder behaviour. The
// firstFailure channel fires once per unique message ID at the moment
// that message's first-attempt failure is decided, which the test uses
// to synchronously serialise Tells against retry backoff windows.
type failOnceBehavior struct {
	mu             sync.Mutex
	failureSeen    map[string]bool
	observed       []string
	failOnlyFor    map[string]bool
	firstFailureCh chan string
}

// newFailOnceBehavior returns a fresh behavior that fails the first
// time it sees each unique message ID and succeeds thereafter. If
// failOnly is non-empty, only those IDs are subject to the
// first-attempt failure rule; everything else succeeds outright.
func newFailOnceBehavior(failOnly ...string) *failOnceBehavior {
	failOnlyFor := map[string]bool{}
	for _, id := range failOnly {
		failOnlyFor[id] = true
	}

	return &failOnceBehavior{
		failureSeen:    make(map[string]bool),
		failOnlyFor:    failOnlyFor,
		firstFailureCh: make(chan string, 16),
	}
}

// Receive returns an error on the first invocation per message ID and
// records the message ID on every subsequent successful invocation.
func (b *failOnceBehavior) Receive(ctx context.Context,
	msg *keyedTestMsg) fn.Result[int] {

	b.mu.Lock()
	id := msg.msgID()

	shouldFail := len(b.failOnlyFor) == 0 || b.failOnlyFor[id]
	if shouldFail && !b.failureSeen[id] {
		b.failureSeen[id] = true
		b.mu.Unlock()

		select {
		case b.firstFailureCh <- id:
		default:
		}

		return fn.Err[int](
			errors.New("simulated transient failure on first " +
				"attempt"),
		)
	}

	b.observed = append(b.observed, id)
	b.mu.Unlock()

	return fn.Ok(1)
}

// observedOrder returns a snapshot of the order in which the behavior
// has successfully processed messages.
func (b *failOnceBehavior) observedOrder() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	cp := make([]string, len(b.observed))
	copy(cp, b.observed)

	return cp
}

// longRetryPolicy returns true for up to 5 attempts with a deliberately
// long delay so the reorder test can observe the in-backoff message
// without racing against retry completion. The test gates msg-2's
// enqueue on msg-1's first-attempt failure, then claims at a point
// when msg-1's backoff is still active — the per-key FIFO must hold
// even though msg-2 has a smaller available_at.
func longRetryPolicy(err error, attempts int) (bool, time.Duration) {
	if attempts >= 5 {
		return false, 0
	}

	return true, 2 * time.Second
}

// newKeyedTestCodec returns a MessageCodec that knows how to decode
// keyedTestMsg payloads back from the durable mailbox.
func newKeyedTestCodec() *actor.MessageCodec {
	codec := actor.NewMessageCodec()
	codec.MustRegister(0x4000, func() actor.TLVMessage {
		return &keyedTestMsg{}
	})

	return codec
}

// preEnqueueKeyed inserts a message directly into the store before the
// actor starts. This avoids the SQLite single-connection limitation that
// would otherwise let Tell from a test goroutine race against the actor's
// own write transaction.
func preEnqueueKeyed(t *testing.T, store actor.TxAwareDeliveryStore, mailbox,
	msgID, payloadID, key string, createdAt time.Time) {

	t.Helper()

	msg := newKeyedTestMsg(payloadID, key)

	// The actor's mailbox decodes by registered TLV type, so use the
	// codec to produce bytes the actor's runtime can decode.
	codec := newKeyedTestCodec()
	payload, err := codec.Encode(msg)
	require.NoError(t, err)

	require.NoError(
		t,
		store.EnqueueMessage(
			t.Context(), actor.EnqueueParams{
				ID:             msgID,
				MailboxID:      mailbox,
				MessageType:    msg.MessageType(),
				Payload:        payload,
				AvailableAt:    createdAt,
				MaxAttempts:    5,
				CorrelationKey: key,
			},
		),
	)
}

// TestDurableActorPerKeyFIFOSurvivesTransientFailure is the actor-level
// equivalent of TestPerKeyFIFOBlocksOvertakeOnNack. It pre-enqueues two
// messages with the same correlation key, then starts a real
// DurableActor with a behavior that fails the first attempt of msg-1.
// With the fix, msg-2 must not be processed until msg-1's retry succeeds,
// even though msg-1 spends time in retry backoff with a larger
// available_at than msg-2.
func TestDurableActorPerKeyFIFOSurvivesTransientFailure(t *testing.T) {
	t.Parallel()

	rawDB := newSQLiteDB(t)
	require.NoError(t, RunMigrations(rawDB, sqlc.BackendTypeSqlite))

	store, err := NewTxAwareDeliveryStoreFromDB(
		rawDB, sqlc.BackendTypeSqlite, clock.NewDefaultClock(),
		btclog.Disabled,
	)
	require.NoError(t, err)

	const mailbox = "reorder-test-actor"
	const key = "alice/round-1"

	// Pre-enqueue both messages before the actor starts so the
	// reorder scenario is set up without concurrent writes from the
	// test goroutine. msg-1 has the smaller created_at and is the
	// FIFO head of the key; msg-2 follows.
	now := time.Now()
	preEnqueueKeyed(t, store, mailbox, "msg-1", "msg-1", key, now)
	preEnqueueKeyed(
		t, store, mailbox, "msg-2", "msg-2", key,
		now.Add(time.Millisecond),
	)

	codec := newKeyedTestCodec()

	// Only msg-1 hits the first-attempt failure; msg-2 succeeds on
	// its first claim once the actor reaches it.
	behavior := newFailOnceBehavior("msg-1")

	cfg := actor.DefaultDurableActorConfig[*keyedTestMsg, int](
		mailbox, behavior, store, codec,
	)
	cfg.TellRetryPolicy = longRetryPolicy
	cfg.PollInterval = 25 * time.Millisecond
	cfg.LeaseDuration = 5 * time.Second
	cfg.HeartbeatInterval = 2 * time.Second

	a := actor.NewDurableActor(cfg)

	a.Start()
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(
			context.Background(), 5*time.Second,
		)
		defer stopCancel()
		require.NoError(t, a.StopAndWait(stopCtx))
	}()

	// Wait until both messages have been successfully processed. The
	// retry policy delay is 2 seconds, so the whole sequence runs in
	// ~2-3 seconds: msg-1 fails immediately, backs off 2s, retries
	// and succeeds, then msg-2 becomes claim-eligible and runs.
	require.Eventually(t, func() bool {
		return len(behavior.observedOrder()) == 2
	}, 10*time.Second, 25*time.Millisecond,
		"both messages should eventually be processed")

	// The critical assertion: msg-1 was processed before msg-2 even
	// though msg-2 had a strictly smaller available_at during msg-1's
	// retry backoff window.
	require.Equal(
		t, []string{"msg-1", "msg-2"}, behavior.observedOrder(),
		"per-key FIFO must hold same-key messages in emission "+
			"order even when one transiently fails and is in "+
			"backoff",
	)
}

// TestDurableActorCrossKeyIndependence confirms that two messages on
// different correlation keys can interleave freely even when one of
// them is in retry backoff. Both messages are pre-enqueued; the first
// key is configured to fail on its first attempt, the second key is
// expected to succeed promptly while the first key is in backoff.
func TestDurableActorCrossKeyIndependence(t *testing.T) {
	t.Parallel()

	rawDB := newSQLiteDB(t)
	require.NoError(t, RunMigrations(rawDB, sqlc.BackendTypeSqlite))

	store, err := NewTxAwareDeliveryStoreFromDB(
		rawDB, sqlc.BackendTypeSqlite, clock.NewDefaultClock(),
		btclog.Disabled,
	)
	require.NoError(t, err)

	const mailbox = "reorder-test-cross"

	// Pre-enqueue one message per key. K1 enqueues first, K2 right
	// after. Without per-key FIFO, K1's retry backoff still wouldn't
	// block K2 (different key); with the fix, behavior is unchanged
	// across keys — we assert that here.
	now := time.Now()
	preEnqueueKeyed(
		t, store, mailbox, "k1-msg-1", "k1-msg-1", "alice/round-1", now,
	)
	preEnqueueKeyed(
		t, store, mailbox, "k2-msg-1", "k2-msg-1", "bob/round-2",
		now.Add(time.Millisecond),
	)

	codec := newKeyedTestCodec()

	// Only k1's first message hits the first-attempt failure path.
	behavior := newFailOnceBehavior("k1-msg-1")

	cfg := actor.DefaultDurableActorConfig[*keyedTestMsg, int](
		mailbox, behavior, store, codec,
	)
	cfg.TellRetryPolicy = longRetryPolicy
	cfg.PollInterval = 25 * time.Millisecond
	cfg.LeaseDuration = 5 * time.Second
	cfg.HeartbeatInterval = 2 * time.Second

	a := actor.NewDurableActor(cfg)

	a.Start()
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(
			context.Background(), 5*time.Second,
		)
		defer stopCancel()
		require.NoError(t, a.StopAndWait(stopCtx))
	}()

	// K2 should be processed well within K1's backoff window since
	// they're on different keys. The retry delay is 2s; if K2 has to
	// wait for K1's retry it would fail this assertion.
	require.Eventually(t, func() bool {
		for _, id := range behavior.observedOrder() {
			if id == "k2-msg-1" {
				return true
			}
		}

		return false
	}, 1*time.Second, 25*time.Millisecond,
		"K2 must process well within K1's backoff window")
}
