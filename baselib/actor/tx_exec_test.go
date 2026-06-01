package actor

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// execTestBehavior is a TxBehavior whose per-message logic is supplied by a
// closure. Its store type is the framework DeliveryStore, so the factory is the
// identity on the tx-scoped store.
type execTestBehavior struct {
	onReceive func(ctx context.Context, msg *actorTestMsg,
		ax Exec[DeliveryStore]) fn.Result[int]
}

// Receive implements TxBehavior.
func (b *execTestBehavior) Receive(ctx context.Context, msg *actorTestMsg,
	ax Exec[DeliveryStore]) fn.Result[int] {

	return b.onReceive(ctx, msg, ax)
}

// identityStoreFactory hands the tx-scoped DeliveryStore straight through as
// the behavior's typed store.
func identityStoreFactory(_ context.Context, ds DeliveryStore) DeliveryStore {
	return ds
}

// enqueueEffects returns a Commit closure that writes one outbox effect per id.
// It is the canonical "several writes in one Commit" workload used to exercise
// the atomic-grouping invariant.
func enqueueEffects(ids ...string) func(context.Context, DeliveryStore) error {
	return func(ctx context.Context, ds DeliveryStore) error {
		for _, id := range ids {
			err := ds.EnqueueOutbox(ctx, OutboxParams{
				ID:            id,
				SourceActorID: "actor-a",
				TargetActorID: "actor-b",
				MessageType:   "actor.TestMsg",
			})
			if err != nil {
				return err
			}
		}

		return nil
	}
}

// execHarness bundles a tx-aware mock store and codec with helpers and
// assertions for exercising the Read/Commit execution path. It keeps the tests
// focused on behavior (what was consumed / committed) rather than mock
// plumbing. All query helpers take the store lock internally.
type execHarness struct {
	store *mockTxAwareStore
	codec *MessageCodec
}

// newExecHarness builds a harness over a fresh tx-aware store.
func newExecHarness() *execHarness {
	return &execHarness{
		store: newMockTxAwareStore(),
		codec: newActorTestCodec(),
	}
}

// seedLeased enqueues msgID into mailboxID and leases it under leaseToken,
// returning the harness as the consumer that now holds the lease.
func (h *execHarness) seedLeased(t require.TestingT, mailboxID, msgID,
	leaseToken string) {

	ctx := context.Background()

	require.NoError(
		t,
		h.store.EnqueueMessage(
			ctx, EnqueueParams{
				ID:          msgID,
				MailboxID:   mailboxID,
				MessageType: "actor.TestMsg",
				MaxAttempts: 10,
				AvailableAt: time.Now().Add(-time.Second),
			},
		),
	)

	leased, err := h.store.LeaseNextMessage(
		ctx, mailboxID, leaseToken, time.Minute,
	)
	require.NoError(t, err)
	require.NotNil(t, leased)
	require.Equal(t, msgID, leased.ID)
	require.Equal(t, leaseToken, leased.LeaseToken)
}

// coreFor builds an execCore bound to msgID and holding leaseToken.
func (h *execHarness) coreFor(msgID, leaseToken string) *execCore {
	return &execCore{
		store:      h.store,
		msgID:      msgID,
		leaseToken: leaseToken,
		actorID:    "actor-a",
		dedupTTL:   time.Hour,
	}
}

// messageExists reports whether the mailbox row is still present.
func (h *execHarness) messageExists(id string) bool {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()

	_, ok := h.store.messages[id]

	return ok
}

// isProcessed reports whether the dedup mark is set.
func (h *execHarness) isProcessed(id string) bool {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()

	return h.store.processed[id]
}

// outboxCount returns the number of committed outbox effects.
func (h *execHarness) outboxCount() int {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()

	return len(h.store.outbox)
}

// outboxHas reports whether an outbox effect with the given id committed.
func (h *execHarness) outboxHas(id string) bool {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()

	_, ok := h.store.outbox[id]

	return ok
}

// requireConsumed asserts the message was fence-acked (row gone) and the dedup
// mark was set -- the two halves of "the message was durably consumed".
func (h *execHarness) requireConsumed(t require.TestingT, id string) {
	require.False(t, h.messageExists(id), "message should be consumed")
	require.True(t, h.isProcessed(id), "message should be marked processed")
}

// requireRetained asserts the message is still claimable and unconsumed: the
// row is present and no dedup mark was written.
func (h *execHarness) requireRetained(t require.TestingT, id string) {
	require.True(t, h.messageExists(id), "message should be retained")
	require.False(t, h.isProcessed(id), "message must not be processed")
}

// startTxActor wires beh into a durable actor on the Read/Commit path and
// starts it. The caller is responsible for stopping it.
func (h *execHarness) startTxActor(t *testing.T,
	beh TxBehavior[*actorTestMsg, int, DeliveryStore],
) *DurableActor[*actorTestMsg, int] {

	cfg := DefaultDurableTxActorConfig[*actorTestMsg, int, DeliveryStore](
		"test-actor", beh, identityStoreFactory, h.store, h.codec,
	)
	cfg.PollInterval = 10 * time.Millisecond

	a := NewDurableActor(cfg).UnwrapOrFail(t)
	a.Start()

	return a
}

// tellTestMsg sends a value-carrying test message to the actor.
func tellTestMsg(t *testing.T, a *DurableActor[*actorTestMsg, int],
	value uint64) {

	t.Helper()

	msg := &actorTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](value),
	}
	require.NoError(t, a.Ref().Tell(context.Background(), msg))
}

// TestExecCommitFenceConsumesMessage verifies the happy path: a Commit with the
// held lease token runs the user writes, marks the message processed, and
// fence-acks it (deletes the mailbox row), all reported as committed.
func TestExecCommitFenceConsumesMessage(t *testing.T) {
	t.Parallel()

	h := newExecHarness()
	h.seedLeased(t, "mb", "m1", "tok-1")

	core := h.coreFor("m1", "tok-1")
	err := core.commit(context.Background(), enqueueEffects("out-1"))
	require.NoError(t, err)

	require.True(t, core.committed)
	h.requireConsumed(t, "m1")
	require.True(t, h.outboxHas("out-1"))
}

// TestExecCommitStaleLeaseNoOps verifies the fence: a Commit attempted with a
// lease token that no longer matches (message reclaimed elsewhere) aborts with
// ErrLeaseLost and applies nothing -- no user write, no dedup mark, message
// retained.
func TestExecCommitStaleLeaseNoOps(t *testing.T) {
	t.Parallel()

	h := newExecHarness()
	h.seedLeased(t, "mb", "m1", "tok-1")

	core := h.coreFor("m1", "stale-token")

	var userWriteRan bool
	err := core.commit(context.Background(), func(ctx context.Context,
		ds DeliveryStore) error {

		userWriteRan = true

		return enqueueEffects("out-stale")(ctx, ds)
	})
	require.ErrorIs(t, err, ErrLeaseLost)

	require.False(t, core.committed)
	require.False(
		t, userWriteRan,
		"user closure must not run when the fence rejects the commit",
	)
	h.requireRetained(t, "m1")
	require.Zero(t, h.outboxCount())
}

// TestExecCommitTwiceSecondFails verifies a successful Commit cannot be
// double-applied: a second Commit on the same core finds the message already
// consumed and fails the fence without adding further effects.
func TestExecCommitTwiceSecondFails(t *testing.T) {
	t.Parallel()

	h := newExecHarness()
	h.seedLeased(t, "mb", "m1", "tok-1")

	core := h.coreFor("m1", "tok-1")
	require.NoError(
		t,
		core.commit(
			context.Background(), enqueueEffects("out-1"),
		),
	)

	// A second commit hits the fence (the row is gone) and applies nothing.
	err := core.commit(context.Background(), enqueueEffects("out-2"))
	require.ErrorIs(t, err, ErrLeaseLost)

	require.True(t, h.outboxHas("out-1"))
	require.False(t, h.outboxHas("out-2"))
	require.Equal(t, 1, h.outboxCount())
}

// TestDurableActorExecPathCommits drives a TxBehavior end-to-end through the
// actor: a Tell is delivered, the behavior commits an outbox effect, and the
// message is consumed.
func TestDurableActorExecPathCommits(t *testing.T) {
	t.Parallel()

	h := newExecHarness()

	committed := make(chan struct{}, 1)
	behavior := &execTestBehavior{
		onReceive: func(ctx context.Context, msg *actorTestMsg,
			ax Exec[DeliveryStore]) fn.Result[int] {

			err := ax.Commit(ctx, enqueueEffects("effect-1"))
			if err != nil {
				return fn.Err[int](err)
			}

			select {
			case committed <- struct{}{}:
			default:
			}

			return fn.Ok(7)
		},
	}

	a := h.startTxActor(t, behavior)
	defer a.Stop()

	tellTestMsg(t, a, 1)

	select {
	case <-committed:
	case <-time.After(2 * time.Second):
		t.Fatal("behavior never committed")
	}

	// The committed effect is durable and the mailbox drains to empty (the
	// fence-ack consumed the message).
	require.Eventually(t, func() bool {
		return h.outboxHas("effect-1") && !h.anyMessages()
	}, 2*time.Second, 10*time.Millisecond)

	// The Commit path was used (a tx ran).
	require.True(t, h.store.txExecuted.Load())
}

// TestDurableActorExecPathRetriesOnError verifies that a behavior which fails
// before committing (e.g. side-effect IO error) leaves the message for retry
// rather than consuming it.
func TestDurableActorExecPathRetriesOnError(t *testing.T) {
	t.Parallel()

	h := newExecHarness()

	behavior := &execTestBehavior{
		onReceive: func(ctx context.Context, msg *actorTestMsg,
			_ Exec[DeliveryStore]) fn.Result[int] {

			// Simulate IO failing before any Commit.
			return fn.Err[int](context.DeadlineExceeded)
		},
	}

	a := h.startTxActor(t, behavior)
	defer a.Stop()

	tellTestMsg(t, a, 1)

	// The failure nacks for retry; no outbox effect is produced.
	require.Eventually(t, func() bool {
		return h.store.nackCalled.Load()
	}, 2*time.Second, 10*time.Millisecond)

	require.Zero(t, h.outboxCount())
}

// TestDurableActorExecPathConsumesWithoutCommit verifies that a behavior which
// returns success without calling Commit (e.g. a RestartMessage handler) still
// has its message consumed via the non-transactional ack path, with no Commit
// transaction opened.
func TestDurableActorExecPathConsumesWithoutCommit(t *testing.T) {
	t.Parallel()

	h := newExecHarness()

	behavior := &execTestBehavior{
		onReceive: func(ctx context.Context, msg *actorTestMsg,
			_ Exec[DeliveryStore]) fn.Result[int] {

			// No Commit: nothing to persist, just consume the
			// message.
			return fn.Ok(0)
		},
	}

	a := h.startTxActor(t, behavior)
	defer a.Stop()

	tellTestMsg(t, a, 1)

	require.Eventually(t, func() bool {
		return !h.anyMessages()
	}, 2*time.Second, 10*time.Millisecond)

	require.False(
		t, h.store.txExecuted.Load(),
		"no Commit means no transaction should run",
	)
}

// anyMessages reports whether any mailbox rows remain.
func (h *execHarness) anyMessages() bool {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()

	return len(h.store.messages) > 0
}

// TestNewDurableActorTxBehaviorRequiresTxStore verifies that pairing a
// TxBehavior with a store that is not transaction-aware is rejected at
// construction.
func TestNewDurableActorTxBehaviorRequiresTxStore(t *testing.T) {
	t.Parallel()

	// A plain (non-tx-aware) delivery store.
	store := newMockDeliveryStore()
	codec := newActorTestCodec()

	behavior := &execTestBehavior{
		onReceive: func(ctx context.Context, msg *actorTestMsg,
			_ Exec[DeliveryStore]) fn.Result[int] {

			return fn.Ok(0)
		},
	}

	cfg := DefaultDurableTxActorConfig[*actorTestMsg, int, DeliveryStore](
		"test-actor", behavior, identityStoreFactory, store, codec,
	)

	_, err := NewDurableActor(cfg).Unpack()
	require.ErrorIs(t, err, ErrTxBehaviorNeedsTxStore)
}

// TestDurableActorExecPathRejectsDurableAsk verifies that a DurableAsk
// delivered to a Read/Commit (TxBehavior) actor is not silently consumed: the
// behavior never runs, an error response is written to the outbox so the caller
// does not hang, and the message is consumed (acked + dedup-marked).
func TestDurableActorExecPathRejectsDurableAsk(t *testing.T) {
	t.Parallel()

	h := newExecHarness()

	var behaviorRan atomic.Bool
	behavior := &execTestBehavior{
		onReceive: func(ctx context.Context, msg *actorTestMsg,
			ax Exec[DeliveryStore]) fn.Result[int] {

			behaviorRan.Store(true)

			return fn.Ok(0)
		},
	}

	a := h.startTxActor(t, behavior)
	defer a.Stop()

	durableRef, ok := a.Ref().(DurableActorRef[*actorTestMsg, int])
	require.True(t, ok, "durable actor ref must support DurableAsk")

	msg := &actorTestMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(1)),
	}
	require.NoError(
		t,
		durableRef.DurableAsk(
			context.Background(), msg, DurableAskParams{
				CallbackActorID: "callback-actor",
				CorrelationID:   "corr-1",
			},
		),
	)

	// The request is rejected: an error response lands in the outbox and
	// the mailbox row is consumed, without the behavior ever running.
	require.Eventually(t, func() bool {
		h.store.mu.Lock()
		defer h.store.mu.Unlock()

		return len(h.store.outbox) == 1 && len(h.store.messages) == 0
	}, 2*time.Second, 10*time.Millisecond)

	require.False(
		t, behaviorRan.Load(),
		"behavior must not run for a rejected DurableAsk",
	)
}

// TestNewDurableActorRejectsMissingBehavior verifies construction fails closed
// when no usable behavior is configured: a zero-value config (forgotten
// Behavior field, which the fn.Either zero value reports as a Left holding nil)
// and a Right holding a zero-value BoundTxBehavior (run hook never bound) both
// error with ErrNoBehavior instead of panicking at first dispatch.
func TestNewDurableActorRejectsMissingBehavior(t *testing.T) {
	t.Parallel()

	store := newMockTxAwareStore()
	codec := newActorTestCodec()

	// Forgotten Behavior field: the either zero value is Left(nil).
	missingCfg := DurableActorConfig[*actorTestMsg, int]{
		ID:    "test-actor",
		Store: store,
		Codec: codec,
	}
	_, err := NewDurableActor(missingCfg).Unpack()
	require.ErrorIs(t, err, ErrNoBehavior)

	// Right holding a zero-value BoundTxBehavior (no run hook).
	zeroTxCfg := DefaultDurableActorConfig[*actorTestMsg, int](
		"test-actor",
		newMockBehavior(
			fn.Ok(1),
		),
		store,
		codec,
	)
	zeroTxCfg.Behavior = fn.NewRight[ActorBehavior[*actorTestMsg, int]](
		BoundTxBehavior[*actorTestMsg, int]{},
	)
	_, err = NewDurableActor(zeroTxCfg).Unpack()
	require.ErrorIs(t, err, ErrNoBehavior)
}

// TestDurableActorConfigBehaviorEither verifies the two config constructors
// populate the correct side of the Behavior either.
func TestDurableActorConfigBehaviorEither(t *testing.T) {
	t.Parallel()

	store := newMockTxAwareStore()
	codec := newActorTestCodec()

	classic := newMockBehavior(fn.Ok(1))
	classicCfg := DefaultDurableActorConfig[*actorTestMsg, int](
		"a", classic, store, codec,
	)
	require.True(t, classicCfg.Behavior.IsLeft())
	require.False(t, classicCfg.Behavior.IsRight())

	tx := &execTestBehavior{
		onReceive: func(ctx context.Context, msg *actorTestMsg,
			_ Exec[DeliveryStore]) fn.Result[int] {

			return fn.Ok(0)
		},
	}
	txCfg := DefaultDurableTxActorConfig[*actorTestMsg, int, DeliveryStore](
		"a", tx, identityStoreFactory, store, codec,
	)
	require.True(t, txCfg.Behavior.IsRight())
	require.False(t, txCfg.Behavior.IsLeft())
}

// tokenGen draws non-empty lease tokens for property tests.
var tokenGen = rapid.StringMatching(`[a-zA-Z0-9]{1,12}`)

// TestExecCommitFenceProperty is the property form of the fence invariant:
// across arbitrary held/attempted token pairs, a Commit consumes the message
// and applies its effect exactly when the attempted token matches the held
// lease, and otherwise applies nothing and returns ErrLeaseLost.
func TestExecCommitFenceProperty(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		held := tokenGen.Draw(rt, "held")
		sameLease := rapid.Bool().Draw(rt, "sameLease")

		// Build an attempted token that is equal to or definitely
		// different from the held token.
		attempted := held
		if !sameLease {
			attempted = held + "-mismatch"
		}

		h := newExecHarness()
		h.seedLeased(rt, "mb", "m1", held)

		core := h.coreFor("m1", attempted)
		err := core.commit(
			context.Background(), enqueueEffects("eff"),
		)

		if sameLease {
			// Matching lease: consumed exactly once, effect
			// present.
			require.NoError(rt, err)
			require.True(rt, core.committed)
			h.requireConsumed(rt, "m1")
			require.True(rt, h.outboxHas("eff"))
			require.Equal(rt, 1, h.outboxCount())

			return
		}

		// Mismatched lease: nothing applied, message retained.
		require.ErrorIs(rt, err, ErrLeaseLost)
		require.False(rt, core.committed)
		h.requireRetained(rt, "m1")
		require.Zero(rt, h.outboxCount())
	})
}

// TestExecCommitAtomicGroupingProperty is the property form of the
// atomic-grouping invariant: whatever number of effects a single Commit writes
// (including zero), on success every effect is present and the message is
// consumed and marked processed -- all-or-nothing in one transaction.
func TestExecCommitAtomicGroupingProperty(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(0, 8).Draw(rt, "numEffects")

		ids := make([]string, n)
		for i := range ids {
			ids[i] = fmt.Sprintf("eff-%d", i)
		}

		h := newExecHarness()
		h.seedLeased(rt, "mb", "m1", "tok")

		core := h.coreFor("m1", "tok")
		require.NoError(
			rt,
			core.commit(
				context.Background(), enqueueEffects(ids...),
			),
		)

		require.True(rt, core.committed)
		h.requireConsumed(rt, "m1")

		// Exactly the n effects committed -- no more, no fewer.
		require.Equal(rt, n, h.outboxCount())
		for _, id := range ids {
			require.True(rt, h.outboxHas(id))
		}
	})
}
