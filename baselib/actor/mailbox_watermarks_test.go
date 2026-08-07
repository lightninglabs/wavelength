package actor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// depthReportingStore wraps the mock delivery store with a controllable
// MailboxDepthStore surface, so watermark tests can steer the probed depth
// independently of the rows actually enqueued.
type depthReportingStore struct {
	*mockDeliveryStore

	// depth is the backlog depth every probe reports.
	depth int64

	// probeErr, when set, makes every probe fail.
	probeErr error

	// probes counts MailboxDepth calls, so tests can assert the TTL cache
	// is actually suppressing probes on the send path.
	probes int

	// sawAmbientTx records whether any probe arrived with a database
	// transaction still in its context. The watermark probe must strip
	// the sender's ambient transaction, so tests assert this stays false.
	sawAmbientTx bool
}

// MailboxDepth reports the configured depth and counts the probe.
func (d *depthReportingStore) MailboxDepth(ctx context.Context, _ string) (
	int64, error) {

	d.probes++
	if HasTx(ctx) {
		d.sawAmbientTx = true
	}
	if d.probeErr != nil {
		return 0, d.probeErr
	}

	return d.depth, nil
}

// MailboxDepths reports a single-entry listing for the configured depth.
func (d *depthReportingStore) MailboxDepths(_ context.Context) (
	[]MailboxDepthCount, error) {

	return []MailboxDepthCount{{MailboxID: "test-mailbox", Depth: d.depth}},
		nil
}

// newWatermarkMailbox builds a durable mailbox over a depth-reporting store
// with the given watermarks and a test clock the caller controls.
func newWatermarkMailbox(t *testing.T, soft, hard int) (*DurableMailbox[
	*durablePriorityTestMsg, int], *depthReportingStore, *clock.TestClock) {

	t.Helper()

	store := &depthReportingStore{
		mockDeliveryStore: newMockDeliveryStore(),
	}
	testClock := clock.NewTestClock(time.Unix(1_000_000, 0))

	cfg := DefaultDurableMailboxConfig(
		"test-mailbox", store, newDurableTestCodec(),
	)
	cfg.SoftHighWatermark = soft
	cfg.HardHighWatermark = hard
	cfg.Clock = fn.Some[clock.Clock](testClock)

	mailbox := NewDurableMailbox[*durablePriorityTestMsg, int](
		context.Background(), cfg,
	)

	return mailbox, store, testClock
}

// watermarkTestEnv builds a sendable envelope with the given priority.
func watermarkTestEnv(priority int) envelope[*durablePriorityTestMsg, int] {
	msg := &durablePriorityTestMsg{
		durableTestMsg: durableTestMsg{
			Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(1)),
			Payload: tlv.NewPrimitiveRecord[tlv.TlvType2](
				[]byte("wm"),
			),
		},
		priority: priority,
	}

	return envelope[*durablePriorityTestMsg, int]{
		message:   msg,
		callerCtx: context.Background(),
	}
}

// TestWatermarksDisabledByDefault asserts that a mailbox with no watermarks
// configured admits sends regardless of backlog depth, and never probes the
// depth surface at all.
func TestWatermarksDisabledByDefault(t *testing.T) {
	t.Parallel()

	mailbox, store, _ := newWatermarkMailbox(t, 0, 0)
	store.depth = 1_000_000

	require.NoError(
		t,
		mailbox.Send(
			context.Background(), watermarkTestEnv(0),
		),
	)
	require.Equal(t, 0, store.probes)
}

// TestHardWatermarkRefusesSend asserts that a backlog at the hard watermark
// turns a send away with ErrMailboxSaturated and enqueues nothing.
func TestHardWatermarkRefusesSend(t *testing.T) {
	t.Parallel()

	mailbox, store, _ := newWatermarkMailbox(t, 0, 5)
	store.depth = 5

	err := mailbox.Send(context.Background(), watermarkTestEnv(0))
	require.ErrorIs(t, err, ErrMailboxSaturated)

	store.mu.Lock()
	require.Empty(t, store.messages)
	store.mu.Unlock()
}

// TestHardWatermarkRestartPriorityExempt asserts that a restart-priority
// message is admitted even when the backlog is far past the hard watermark:
// the RestartMessage that would un-wedge a stuck actor must not be refused by
// the backlog it exists to drain.
func TestHardWatermarkRestartPriorityExempt(t *testing.T) {
	t.Parallel()

	mailbox, store, _ := newWatermarkMailbox(t, 0, 5)
	store.depth = 500

	err := mailbox.Send(
		context.Background(), watermarkTestEnv(RestartPriority),
	)
	require.NoError(t, err)

	store.mu.Lock()
	require.Len(t, store.messages, 1)
	store.mu.Unlock()

	// The exemption short-circuits before the probe, so the depth surface
	// is never consulted for a restart message.
	require.Equal(t, 0, store.probes)
}

// TestControlPriorityExempt asserts that a control-priority message (the
// tier boot-time restore/resume messages sit at) is admitted at saturation,
// without ever consulting the depth surface.
func TestControlPriorityExempt(t *testing.T) {
	t.Parallel()

	mailbox, store, _ := newWatermarkMailbox(t, 0, 5)
	store.depth = 500

	err := mailbox.Send(
		context.Background(), watermarkTestEnv(ControlPriority),
	)
	require.NoError(t, err)
	require.Equal(t, 0, store.probes)
}

// TestOutboxDeliveryExemptFromWatermarks asserts that an outbox-propagated
// send (the CDC hand-off, marked by the outbox ID the publisher stamps into
// the context) is admitted at saturation: the message was already accepted
// at its true producer, so refusing the hand-off would dead-letter a
// committed outbox row instead of shedding load.
func TestOutboxDeliveryExemptFromWatermarks(t *testing.T) {
	t.Parallel()

	mailbox, store, _ := newWatermarkMailbox(t, 0, 5)
	store.depth = 500

	ctx := WithOutboxID(context.Background(), generateID())
	require.NoError(t, mailbox.Send(ctx, watermarkTestEnv(0)))

	store.mu.Lock()
	require.Len(t, store.messages, 1)
	store.mu.Unlock()

	require.Equal(t, 0, store.probes)
}

// TestProbeStripsAmbientTx asserts that the depth probe never runs inside
// the sender's ambient database transaction: joining a writer's transaction
// would take predicate locks over the whole mailbox partition and
// manufacture serialization conflicts with the consumer's acks.
func TestProbeStripsAmbientTx(t *testing.T) {
	t.Parallel()

	mailbox, store, _ := newWatermarkMailbox(t, 0, 5)
	store.depth = 1

	ctx := WithTx(context.Background(), nil)
	require.True(t, HasTx(ctx))

	require.NoError(t, mailbox.Send(ctx, watermarkTestEnv(0)))
	require.Equal(t, 1, store.probes)
	require.False(t, store.sawAmbientTx)
}

// TestSoftEpisodeOpensAtSaturation asserts that a backlog that enters
// saturation within one probe window still opens the soft-watermark episode:
// the operator's designed first signal must fire at or before the first
// refused send.
func TestSoftEpisodeOpensAtSaturation(t *testing.T) {
	t.Parallel()

	mailbox, store, _ := newWatermarkMailbox(t, 3, 5)
	store.depth = 10

	err := mailbox.Send(context.Background(), watermarkTestEnv(0))
	require.ErrorIs(t, err, ErrMailboxSaturated)
	require.True(t, mailbox.depth.softBreached)
}

// TestWatermarkLocalDeltaCrossesHard asserts that sends accepted inside one
// probe window count against the hard watermark: with a probed depth of 3 and
// a hard watermark of 5, the two sends that lift the estimate to 5 are
// admitted and the third is refused, all on a single probe.
func TestWatermarkLocalDeltaCrossesHard(t *testing.T) {
	t.Parallel()

	mailbox, store, _ := newWatermarkMailbox(t, 0, 5)
	store.depth = 3
	ctx := context.Background()

	require.NoError(t, mailbox.Send(ctx, watermarkTestEnv(0)))
	require.NoError(t, mailbox.Send(ctx, watermarkTestEnv(0)))

	err := mailbox.Send(ctx, watermarkTestEnv(0))
	require.ErrorIs(t, err, ErrMailboxSaturated)

	require.Equal(t, 1, store.probes)
}

// TestWatermarkProbeTTLRefresh asserts that a saturated mailbox recovers once
// the probe TTL elapses and the store reports a drained backlog.
func TestWatermarkProbeTTLRefresh(t *testing.T) {
	t.Parallel()

	mailbox, store, testClock := newWatermarkMailbox(t, 0, 5)
	store.depth = 5
	ctx := context.Background()

	err := mailbox.Send(ctx, watermarkTestEnv(0))
	require.ErrorIs(t, err, ErrMailboxSaturated)

	// Within the TTL the cached probe still refuses, without re-probing.
	err = mailbox.Send(ctx, watermarkTestEnv(0))
	require.ErrorIs(t, err, ErrMailboxSaturated)
	require.Equal(t, 1, store.probes)

	// The consumer drains the backlog; once the TTL elapses the next send
	// re-probes and is admitted.
	store.depth = 0
	testClock.SetTime(testClock.Now().Add(depthProbeTTL))

	require.NoError(t, mailbox.Send(ctx, watermarkTestEnv(0)))
	require.Equal(t, 2, store.probes)
}

// TestWatermarkProbeFailureFailsOpen asserts that a failed depth probe admits
// the send: a broken monitoring read must not become message loss.
func TestWatermarkProbeFailureFailsOpen(t *testing.T) {
	t.Parallel()

	mailbox, store, _ := newWatermarkMailbox(t, 0, 1)
	store.depth = 100
	store.probeErr = errors.New("depth read broke")

	require.NoError(
		t,
		mailbox.Send(
			context.Background(), watermarkTestEnv(0),
		),
	)
}

// TestSoftWatermarkWarnsWithoutRefusing asserts that a backlog past the soft
// watermark (with no hard watermark) never refuses a send, and that the
// breach episode opens and closes with the estimate.
func TestSoftWatermarkWarnsWithoutRefusing(t *testing.T) {
	t.Parallel()

	mailbox, store, testClock := newWatermarkMailbox(t, 3, 0)
	store.depth = 10
	ctx := context.Background()

	require.NoError(t, mailbox.Send(ctx, watermarkTestEnv(0)))
	require.True(t, mailbox.depth.softBreached)

	// Drain the backlog: after the TTL elapses the next send closes the
	// episode.
	store.depth = 0
	testClock.SetTime(testClock.Now().Add(depthProbeTTL))

	require.NoError(t, mailbox.Send(ctx, watermarkTestEnv(0)))
	require.False(t, mailbox.depth.softBreached)
}

// TestTrySendInheritsSaturation asserts that the non-blocking send path
// surfaces the same saturation refusal as Send.
func TestTrySendInheritsSaturation(t *testing.T) {
	t.Parallel()

	mailbox, store, _ := newWatermarkMailbox(t, 0, 5)
	store.depth = 5

	err := mailbox.TrySend(watermarkTestEnv(0))
	require.ErrorIs(t, err, ErrMailboxSaturated)
}

// TestWatermarksWithoutDepthStore asserts that configuring watermarks over a
// store without the depth surface is a harmless no-op.
func TestWatermarksWithoutDepthStore(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	cfg := DefaultDurableMailboxConfig(
		"test-mailbox", store, newDurableTestCodec(),
	)
	cfg.HardHighWatermark = 1

	mailbox := NewDurableMailbox[*durablePriorityTestMsg, int](
		context.Background(), cfg,
	)

	require.NoError(
		t,
		mailbox.Send(
			context.Background(), watermarkTestEnv(0),
		),
	)
}

// TestNormalizeWatermarks pins the construction-time normalization: negatives
// disable, and a soft value above the hard value is lowered to it.
func TestNormalizeWatermarks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		soft     int
		hard     int
		wantSoft int
		wantHard int
	}{
		{
			"both disabled",
			0,
			0,
			0,
			0,
		},
		{
			"negatives disable",
			-5,
			-1,
			0,
			0,
		},
		{
			"soft only",
			100,
			0,
			100,
			0,
		},
		{
			"hard only",
			0,
			100,
			0,
			100,
		},
		{
			"ordered pair kept",
			100,
			1000,
			100,
			1000,
		},
		{
			"soft above hard lowered",
			5000,
			1000,
			1000,
			1000,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			soft, hard := normalizeWatermarks(tc.soft, tc.hard)
			require.Equal(t, tc.wantSoft, soft)
			require.Equal(t, tc.wantHard, hard)
		})
	}
}
