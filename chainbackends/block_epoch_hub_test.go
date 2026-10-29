package chainbackends

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

const (
	// epochTestTimeout bounds every wait in the block epoch hub tests.
	epochTestTimeout = 5 * time.Second

	// testBlockQueueSize shrinks registration queues so overflow tests
	// need only a handful of blocks.
	testBlockQueueSize = 10
)

// blockStreamResult controls a raw gRPC receive, including stream failures.
type blockStreamResult struct {
	epoch *chainrpc.BlockEpoch
	err   error
}

// testBlockStream models the cancellation contract of a gRPC receive.
type testBlockStream struct {
	grpc.ClientStream

	//nolint:containedctx // the fake gRPC stream owns its receive context
	ctx     context.Context
	request *chainrpc.BlockEpoch
	results chan blockStreamResult
	owner   *testBlockClient
	once    sync.Once
}

// release marks the stream as no longer active, once.
func (s *testBlockStream) release() {
	s.once.Do(func() { s.owner.active.Add(-1) })
}

// Recv blocks until a notification, stream failure, or cancellation arrives.
func (s *testBlockStream) Recv() (*chainrpc.BlockEpoch, error) {
	select {
	case result := <-s.results:
		if result.err != nil {
			s.release()
		}

		return result.epoch, result.err

	case <-s.ctx.Done():
		s.release()

		return nil, s.ctx.Err()
	}
}

// testBlockClient counts upstream streams and captures resume cursors.
type testBlockClient struct {
	chainrpc.ChainNotifierClient

	opened    chan *testBlockStream
	calls     atomic.Int32
	active    atomic.Int32
	maxActive atomic.Int32

	// fail fails this many registrations before succeeding.
	fail atomic.Int32

	// hang makes registrations block until their context is cancelled.
	hang atomic.Bool

	// inFlight counts registrations that have not returned yet.
	inFlight atomic.Int32
}

// RegisterBlockEpochNtfn captures authentication and an independent request.
func (c *testBlockClient) RegisterBlockEpochNtfn(ctx context.Context,
	request *chainrpc.BlockEpoch, _ ...grpc.CallOption) (
	chainrpc.ChainNotifier_RegisterBlockEpochNtfnClient, error) {

	c.inFlight.Add(1)
	defer c.inFlight.Add(-1)

	c.calls.Add(1)
	if c.hang.Load() {
		c.opened <- nil
		<-ctx.Done()

		return nil, ctx.Err()
	}
	if c.fail.Add(-1) >= 0 {
		return nil, errors.New("registration failed")
	}

	md, _ := metadata.FromOutgoingContext(ctx)
	if len(md.Get("macaroon")) != 1 {
		return nil, errors.New("missing authentication")
	}

	active := c.active.Add(1)
	for old := c.maxActive.Load(); active > old; old = c.maxActive.Load() {
		if c.maxActive.CompareAndSwap(old, active) {
			break
		}
	}

	s := &testBlockStream{
		ctx:   ctx,
		owner: c,
		request: &chainrpc.BlockEpoch{
			Height: request.Height,
			Hash:   append([]byte(nil), request.Hash...),
		},
		results: make(chan blockStreamResult, 1000),
	}
	c.opened <- s

	return s, nil
}

// testBlockNotifier exposes the authenticated raw client used in production.
type testBlockNotifier struct {
	lndclient.ChainNotifierClient

	client *testBlockClient
}

// RawClientWithMacAuth adds synthetic authentication without real credentials.
func (n *testBlockNotifier) RawClientWithMacAuth(ctx context.Context) (
	context.Context, time.Duration, chainrpc.ChainNotifierClient) {

	return metadata.AppendToOutgoingContext(ctx, "macaroon", "test"),
		time.Second, n.client
}

// testBlockChainKit counts hash lookups to guard against per-block RPCs.
type testBlockChainKit struct {
	lndclient.ChainKitClient

	hashCalls atomic.Int32
}

// GetBlockHash records any attempt to refetch a notification's hash.
func (k *testBlockChainKit) GetBlockHash(context.Context, int64) (
	chainhash.Hash, error) {

	k.hashCalls.Add(1)

	return chainhash.Hash{}, errors.New("hash must come from notification")
}

// newTestBlockNotifier builds the real notifier over a controllable RPC. Its
// reconnect backoff is shortened so reconnect tests run quickly, and its queues
// are shrunk so overflow tests stay small.
func newTestBlockNotifier(t *testing.T) (*LndClientChainNotifier,
	*testBlockClient, *testBlockChainKit) {

	t.Helper()

	client := &testBlockClient{opened: make(chan *testBlockStream, 1000)}
	kit := &testBlockChainKit{}
	n := NewLndClientChainNotifier(LndClientChainNotifierConfig{
		LND: &lndclient.LndServices{
			ChainNotifier: &testBlockNotifier{client: client},
			ChainKit:      kit,
		},
	})
	n.epochs.queueSize = testBlockQueueSize
	n.epochs.minBackoff = time.Millisecond
	n.epochs.maxBackoff = 10 * time.Millisecond
	t.Cleanup(func() { require.NoError(t, n.Stop()) })

	return n, client, kit
}

// newTestBlockBackend wraps the test notifier in a real LNDBackend.
func newTestBlockBackend(t *testing.T) (*LNDBackend, *testBlockClient,
	*testBlockChainKit) {

	t.Helper()

	n, client, kit := newTestBlockNotifier(t)
	b := NewLNDBackend(n, &stubFeeEstimator{}, &stubBroadcaster{})
	t.Cleanup(func() { require.NoError(t, b.Stop()) })

	return b, client, kit
}

// nextTestBlockStream waits for an upstream registration without polling.
func nextTestBlockStream(t *testing.T,
	client *testBlockClient) *testBlockStream {

	t.Helper()

	select {
	case stream := <-client.opened:
		return stream

	case <-time.After(epochTestTimeout):
		t.Fatal("upstream did not register")

		return nil
	}
}

// requireNoNewStream asserts the hub did not open another upstream stream.
func requireNoNewStream(t *testing.T, client *testBlockClient) {
	t.Helper()

	select {
	case <-client.opened:
		t.Fatal("unexpected upstream registration")

	case <-time.After(50 * time.Millisecond):
	}
}

// sendTestBlock emits a raw event with independently chosen height and hash.
func sendTestBlock(stream *testBlockStream, height uint32, hash byte) {
	blockHash := chainhash.Hash{hash}
	stream.results <- blockStreamResult{epoch: &chainrpc.BlockEpoch{
		Height: height, Hash: blockHash[:],
	}}
}

// recvTestEpoch waits for one notifier event and verifies its identity.
func recvTestEpoch(t *testing.T, event *chainntnfs.BlockEpochEvent,
	height int32, hash byte) *chainntnfs.BlockEpoch {

	t.Helper()

	select {
	case epoch, ok := <-event.Epochs:
		require.True(t, ok, "registration unexpectedly closed")
		require.Equal(t, height, epoch.Height)
		require.Equal(t, chainhash.Hash{hash}, *epoch.Hash)

		return epoch

	case <-time.After(epochTestTimeout):
		t.Fatalf("no epoch for height %d", height)

		return nil
	}
}

// receiveTestBlock waits for one backend event and verifies its identity.
func receiveTestBlock(t *testing.T, sub *chainsource.BlockRegistration,
	height int32, hash byte) *chainsource.BlockEpoch {

	t.Helper()

	select {
	case epoch, ok := <-sub.Epochs:
		require.True(t, ok, "subscription unexpectedly closed")
		require.Equal(t, height, epoch.Height)
		require.Equal(t, chainhash.Hash{hash}, epoch.Hash)

		return epoch

	case <-time.After(epochTestTimeout):
		t.Fatalf("no block for height %d", height)

		return nil
	}
}

// requireEpochClosed waits for a registration to close, discarding any epochs
// still buffered.
func requireEpochClosed(t *testing.T, event *chainntnfs.BlockEpochEvent) {
	t.Helper()

	timeout := time.After(epochTestTimeout)
	for {
		select {
		case _, ok := <-event.Epochs:
			if !ok {
				return
			}

		case <-timeout:
			t.Fatal("epoch channel was not closed")
		}
	}
}

// TestBlockEpochHubManySubscribers registers hundreds of concurrent
// subscribers through the backend, as a client with one block subscription
// per live VTXO actor does, and proves fanout neither multiplies upstream
// streams nor makes a single hash RPC. Events include same-height replacements
// and lower heights, which must all be delivered in upstream order. A late
// subscriber receives an independent tip snapshot.
func TestBlockEpochHubManySubscribers(t *testing.T) {
	t.Parallel()

	b, client, kit := newTestBlockBackend(t)

	const subscribers = 420
	subs := make([]*chainsource.BlockRegistration, subscribers)
	errs := make(chan error, subscribers)
	var wg sync.WaitGroup
	for i := range subscribers {
		wg.Go(func() {
			var err error
			subs[i], err = b.RegisterBlocks(t.Context())
			errs <- err
		})
	}
	wg.Wait()
	for range subscribers {
		require.NoError(t, <-errs)
	}

	stream := nextTestBlockStream(t, client)
	require.Empty(t, stream.request.Hash, "first open must follow the tip")
	for i, height := range []uint32{100, 101, 101, 99, 100} {
		hash := byte(i + 1)
		sendTestBlock(stream, height, hash)
		for _, sub := range subs {
			epoch := receiveTestBlock(t, sub, int32(height), hash)

			// Mutation must not corrupt another queue or the
			// cached tip.
			epoch.Hash[0] = 255
		}
	}

	late, err := b.RegisterBlocks(t.Context())
	require.NoError(t, err)
	receiveTestBlock(t, late, 100, 5)

	require.EqualValues(t, 1, client.calls.Load())
	require.Zero(t, kit.hashCalls.Load())

	for _, sub := range subs {
		sub.Cancel()
		sub.Cancel()
	}
	require.EqualValues(t, 1, client.active.Load())

	// The final cancellation joins the reader, so the upstream stream is
	// released by the time it returns.
	late.Cancel()
	require.Zero(t, client.active.Load())
	require.EqualValues(t, 1, client.maxActive.Load())
}

// TestBlockEpochHubSlowSubscriber closes a stalled registration's bounded
// queue while healthy registrations keep receiving every epoch, in order, on
// the original stream.
func TestBlockEpochHubSlowSubscriber(t *testing.T) {
	t.Parallel()

	n, client, _ := newTestBlockNotifier(t)
	slow, err := n.RegisterBlockEpochNtfn(nil)
	require.NoError(t, err)
	fast, err := n.RegisterBlockEpochNtfn(nil)
	require.NoError(t, err)
	defer fast.Cancel()

	stream := nextTestBlockStream(t, client)
	for i := range testBlockQueueSize + 2 {
		sendTestBlock(stream, uint32(i+1), byte(i+1))
		recvTestEpoch(t, fast, int32(i+1), byte(i+1))
	}

	// The stalled registration keeps what it buffered, then closes.
	for i := range testBlockQueueSize {
		recvTestEpoch(t, slow, int32(i+1), byte(i+1))
	}
	_, ok := <-slow.Epochs
	require.False(t, ok)
	slow.Cancel()

	require.EqualValues(t, 1, client.calls.Load())
	require.EqualValues(t, 1, client.active.Load())
}

// TestBlockEpochHubReconnect verifies that an upstream failure is absorbed by
// one shared reconnect: every registration stays open, a failed retry backs
// off, and the new stream resumes from the last delivered hash/height cursor
// rather than asking for the tip only.
func TestBlockEpochHubReconnect(t *testing.T) {
	t.Parallel()

	b, client, kit := newTestBlockBackend(t)

	var subs []*chainsource.BlockRegistration
	for range 50 {
		sub, err := b.RegisterBlocks(t.Context())
		require.NoError(t, err)
		subs = append(subs, sub)
	}

	stream := nextTestBlockStream(t, client)
	sendTestBlock(stream, 100, 1)
	for _, sub := range subs {
		receiveTestBlock(t, sub, 100, 1)
	}

	// Exercise a failed reconnect before recovery, without losing any
	// registration.
	client.fail.Store(1)
	stream.results <- blockStreamResult{err: errors.New("connection lost")}

	reconnected := nextTestBlockStream(t, client)
	require.EqualValues(t, 100, reconnected.request.Height)
	wantHash := chainhash.Hash{1}
	require.Equal(t, wantHash[:], reconnected.request.Hash)

	// lnd's catch-up after a reorg can replay the cursor height with a
	// new hash. Neither it nor the next block may be deduplicated away.
	sendTestBlock(reconnected, 100, 2)
	sendTestBlock(reconnected, 101, 3)
	for _, sub := range subs {
		receiveTestBlock(t, sub, 100, 2)
		receiveTestBlock(t, sub, 101, 3)
	}

	require.EqualValues(t, 3, client.calls.Load())
	require.EqualValues(t, 1, client.maxActive.Load())
	require.Zero(t, kit.hashCalls.Load())
}

// TestBlockEpochHubCatchUpBurst verifies that the production queue absorbs
// lnd's catch-up replay after an outage. On reconnect lnd sends every missed
// block at once; registrations that are not draining in lockstep must receive
// all of them in order rather than overflow, re-register, and see only the
// tip.
func TestBlockEpochHubCatchUpBurst(t *testing.T) {
	t.Parallel()

	n, client, _ := newTestBlockNotifier(t)
	n.epochs.queueSize = blockEpochQueueSize

	// Many more blocks than an outage-free queue needs, but within the
	// production bound.
	const missed = 200

	var subs []*chainntnfs.BlockEpochEvent
	for range 20 {
		event, err := n.RegisterBlockEpochNtfn(nil)
		require.NoError(t, err)
		defer event.Cancel()
		subs = append(subs, event)
	}

	stream := nextTestBlockStream(t, client)
	sendTestBlock(stream, 100, 100)
	stream.results <- blockStreamResult{err: errors.New("lnd restarted")}

	// No registration reads while lnd replays the missed blocks.
	reconnected := nextTestBlockStream(t, client)
	require.EqualValues(t, 100, reconnected.request.Height)
	for i := range missed {
		height := uint32(101 + i)
		sendTestBlock(reconnected, height, byte(height))
	}

	for _, sub := range subs {
		recvTestEpoch(t, sub, 100, 100)
		for i := range missed {
			height := int32(101 + i)
			recvTestEpoch(t, sub, height, byte(height))
		}
	}
	require.EqualValues(t, 2, client.calls.Load())
}

// TestBlockEpochHubMalformedEvent verifies that a malformed event on the
// shared stream is never forwarded and never closes a registration. The
// reader abandons that stream and resumes from the last valid cursor, so no
// block is skipped.
func TestBlockEpochHubMalformedEvent(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		epoch *chainrpc.BlockEpoch
	}{
		{
			name: "nil epoch",
		},
		{
			name: "short hash",
			epoch: &chainrpc.BlockEpoch{
				Hash: []byte{
					1,
				},
			},
		},
		{
			name: "long hash",
			epoch: &chainrpc.BlockEpoch{
				Hash: make([]byte, 33),
			},
		},
		{
			name: "overflow height",
			epoch: &chainrpc.BlockEpoch{
				Height: 1 << 31,
				Hash:   make([]byte, chainhash.HashSize),
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			n, client, kit := newTestBlockNotifier(t)
			event, err := n.RegisterBlockEpochNtfn(nil)
			require.NoError(t, err)
			defer event.Cancel()

			stream := nextTestBlockStream(t, client)
			sendTestBlock(stream, 100, 42)
			recvTestEpoch(t, event, 100, 42)

			stream.results <- blockStreamResult{epoch: test.epoch}
			reconnected := nextTestBlockStream(t, client)
			require.ErrorIs(t, stream.ctx.Err(), context.Canceled)
			require.EqualValues(t, 100, reconnected.request.Height)
			cursorHash := chainhash.Hash{42}
			require.Equal(
				t, cursorHash[:], reconnected.request.Hash,
			)

			sendTestBlock(reconnected, 101, 43)
			recvTestEpoch(t, event, 101, 43)
			require.Zero(t, kit.hashCalls.Load())
		})
	}
}

// TestBlockEpochHubSubscriberContext verifies one caller's context cannot end
// another subscriber's upstream, and that a fresh tip-following stream opens
// after the last cancellation.
func TestBlockEpochHubSubscriberContext(t *testing.T) {
	t.Parallel()

	b, client, _ := newTestBlockBackend(t)

	firstCtx, cancel := context.WithCancel(t.Context())
	first, err := b.RegisterBlocks(firstCtx)
	require.NoError(t, err)
	second, err := b.RegisterBlocks(t.Context())
	require.NoError(t, err)

	stream := nextTestBlockStream(t, client)
	cancel()
	first.Cancel()

	sendTestBlock(stream, 100, 1)
	receiveTestBlock(t, second, 100, 1)
	require.EqualValues(t, 1, client.active.Load())

	second.Cancel()
	require.Zero(t, client.active.Load())

	third, err := b.RegisterBlocks(t.Context())
	require.NoError(t, err)
	defer third.Cancel()

	stream = nextTestBlockStream(t, client)
	require.Empty(t, stream.request.Hash)
	sendTestBlock(stream, 101, 2)
	receiveTestBlock(t, third, 101, 2)
	require.EqualValues(t, 1, client.maxActive.Load())
}

// TestBlockEpochHubStop verifies that Stop closes every registration, joins
// the raw receive, and rejects later registrations. Cancellation stays safe
// after shutdown.
func TestBlockEpochHubStop(t *testing.T) {
	t.Parallel()

	n, client, _ := newTestBlockNotifier(t)
	first, err := n.RegisterBlockEpochNtfn(nil)
	require.NoError(t, err)
	second, err := n.RegisterBlockEpochNtfn(nil)
	require.NoError(t, err)
	stream := nextTestBlockStream(t, client)

	require.NoError(t, n.Stop())
	requireEpochClosed(t, first)
	requireEpochClosed(t, second)
	require.ErrorIs(t, stream.ctx.Err(), context.Canceled)
	require.Zero(t, client.active.Load())

	first.Cancel()
	second.Cancel()

	_, err = n.RegisterBlockEpochNtfn(nil)
	require.ErrorIs(t, err, errNotifierStopped)
	_, err = n.RegisterBlockEpochNtfn(&chainntnfs.BlockEpoch{
		Hash: &chainhash.Hash{},
	})
	require.ErrorIs(t, err, errNotifierStopped)
}

// TestBlockEpochHubStopDuringOpen verifies that Stop cancels a hanging first
// open, fails the opener and every registration waiting on it, and returns
// only after the lnd call returned.
func TestBlockEpochHubStopDuringOpen(t *testing.T) {
	t.Parallel()

	n, client, _ := newTestBlockNotifier(t)
	client.hang.Store(true)

	const numRegs = 3
	errs := make(chan error, numRegs)
	for range numRegs {
		go func() {
			_, err := n.RegisterBlockEpochNtfn(nil)
			errs <- err
		}()
	}

	// Wait for the open to start so Stop has to cancel it.
	nextTestBlockStream(t, client)

	require.NoError(t, n.Stop())
	require.Zero(t, client.inFlight.Load())

	for range numRegs {
		select {
		case err := <-errs:
			require.ErrorIs(t, err, errNotifierStopped)

		case <-time.After(epochTestTimeout):
			t.Fatal("registration did not return after Stop")
		}
	}
	require.EqualValues(t, 1, client.calls.Load())
}

// TestBlockEpochHubStopDuringReconnect ensures shutdown interrupts the
// reconnect backoff and leaves neither a reconnecting reader nor an open
// registration behind.
func TestBlockEpochHubStopDuringReconnect(t *testing.T) {
	t.Parallel()

	n, client, _ := newTestBlockNotifier(t)
	n.epochs.minBackoff = time.Hour
	n.epochs.maxBackoff = time.Hour

	event, err := n.RegisterBlockEpochNtfn(nil)
	require.NoError(t, err)
	stream := nextTestBlockStream(t, client)
	stream.results <- blockStreamResult{err: errors.New("connection lost")}

	select {
	case <-stream.ctx.Done():
	case <-time.After(epochTestTimeout):
		t.Fatal("failed stream was not cancelled")
	}

	require.NoError(t, n.Stop())
	requireEpochClosed(t, event)
	require.EqualValues(t, 1, client.calls.Load())
}

// TestBlockEpochHubInitialFailure verifies a failed first open is returned,
// does not poison the hub, and that the next registration retries the open.
func TestBlockEpochHubInitialFailure(t *testing.T) {
	t.Parallel()

	b, client, _ := newTestBlockBackend(t)

	client.fail.Store(1)
	_, err := b.RegisterBlocks(t.Context())
	require.ErrorContains(t, err, "registration failed")

	sub, err := b.RegisterBlocks(t.Context())
	require.NoError(t, err)
	defer sub.Cancel()

	stream := nextTestBlockStream(t, client)
	sendTestBlock(stream, 100, 1)
	receiveTestBlock(t, sub, 100, 1)
}

// TestBlockEpochHubConcurrentCancellation races last-owner cancellations
// against new registrations. Streams must never overlap, and no channel may be
// closed while fanout uses it.
func TestBlockEpochHubConcurrentCancellation(t *testing.T) {
	t.Parallel()

	b, client, _ := newTestBlockBackend(t)

	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for range 50 {
		wg.Go(func() {
			sub, err := b.RegisterBlocks(t.Context())
			errs <- err
			if err == nil {
				sub.Cancel()
			}
		})
	}
	wg.Wait()
	for range 50 {
		require.NoError(t, <-errs)
	}

	require.Zero(t, client.active.Load())
	require.EqualValues(t, 1, client.maxActive.Load())
}

// TestBlockEpochHubConcurrentChurn races registrations and cancellations
// against live epochs, upstream failures, and reconnects. Run under -race it
// checks the ownership model for data races, panics, deadlocks, overlapping
// streams, and registrations that are never closed.
func TestBlockEpochHubConcurrentChurn(t *testing.T) {
	t.Parallel()

	n, client, _ := newTestBlockNotifier(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// Feed epochs to whichever stream is current, and periodically fail
	// it so registrations live through reconnects.
	var feeder sync.WaitGroup
	feeder.Go(func() {
		var (
			stream *testBlockStream
			height uint32
		)
		for ctx.Err() == nil {
			select {
			case next := <-client.opened:
				stream = next

			default:
			}

			if stream != nil && stream.ctx.Err() == nil {
				height++
				sendTestBlock(stream, height, byte(height))
				if height%25 == 0 {
					stream.results <- blockStreamResult{
						err: errors.New("churn"),
					}
				}
			}

			time.Sleep(time.Millisecond)
		}
	})

	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for range 50 {
				event, err := n.RegisterBlockEpochNtfn(nil)
				if !assertNoError(t, err) {
					return
				}

				// Read a little, then cancel and drain to
				// prove the channel is always closed.
				select {
				case <-event.Epochs:
				case <-time.After(2 * time.Millisecond):
				}

				event.Cancel()
				requireEpochClosed(t, event)
			}
		})
	}

	workers.Wait()
	cancel()
	feeder.Wait()

	require.Zero(t, client.active.Load())
	require.EqualValues(t, 1, client.maxActive.Load())
}

// assertNoError reports a registration error from a worker goroutine, where
// require's FailNow is not allowed.
func assertNoError(t *testing.T, err error) bool {
	t.Helper()

	if err != nil {
		t.Errorf("register: %v", err)

		return false
	}

	return true
}

// TestBlockEpochHubDedicatedCursor verifies a registration with a cursor gets
// its own stream carrying that cursor, independent of the shared stream, and
// that a malformed event closes it rather than skipping a block.
func TestBlockEpochHubDedicatedCursor(t *testing.T) {
	t.Parallel()

	n, client, kit := newTestBlockNotifier(t)

	shared, err := n.RegisterBlockEpochNtfn(nil)
	require.NoError(t, err)
	defer shared.Cancel()
	sharedStream := nextTestBlockStream(t, client)

	cursorHash := chainhash.Hash{42}
	event, err := n.RegisterBlockEpochNtfn(&chainntnfs.BlockEpoch{
		Height: 100, Hash: &cursorHash,
	})
	require.NoError(t, err)
	defer event.Cancel()

	stream := nextTestBlockStream(t, client)
	require.EqualValues(t, 100, stream.request.Height)
	require.Equal(t, cursorHash[:], stream.request.Hash)

	sendTestBlock(stream, 101, 1)
	recvTestEpoch(t, event, 101, 1)

	stream.results <- blockStreamResult{epoch: &chainrpc.BlockEpoch{
		Hash: []byte{
			1,
		},
	}}
	requireEpochClosed(t, event)
	require.ErrorIs(t, stream.ctx.Err(), context.Canceled)
	requireNoNewStream(t, client)

	// The shared stream is untouched by the dedicated one's failure.
	sendTestBlock(sharedStream, 101, 1)
	recvTestEpoch(t, shared, 101, 1)
	require.Zero(t, kit.hashCalls.Load())

	_, err = n.RegisterBlockEpochNtfn(&chainntnfs.BlockEpoch{Height: 1})
	require.ErrorContains(t, err, "invalid block epoch cursor")
}

// blockedBlockTarget models actor mailbox backpressure while respecting the
// subscription's cancellation context.
type blockedBlockTarget struct {
	entered    chan struct{}
	release    chan struct{}
	cancelled  chan struct{}
	delivered  chan chainsource.BlockEpoch
	once       sync.Once
	cancelOnce sync.Once
}

// ID identifies the notification target used by the test actor.
func (r *blockedBlockTarget) ID() string {
	return "blocked-block-target"
}

// Tell blocks delivery until the target recovers or its subscription stops.
func (r *blockedBlockTarget) Tell(ctx context.Context,
	epoch chainsource.BlockEpoch) error {

	r.once.Do(func() { close(r.entered) })
	select {
	case <-r.release:
	case <-ctx.Done():
		r.cancelOnce.Do(func() { close(r.cancelled) })

		return ctx.Err()
	}

	select {
	case r.delivered <- epoch:
		return nil

	case <-ctx.Done():
		return ctx.Err()
	}
}

// TryTell is unused: BlockEpochActor uses context-aware Tell for backpressure.
func (r *blockedBlockTarget) TryTell(context.Context,
	chainsource.BlockEpoch) error {

	return errors.New("test target does not support TryTell")
}

// TestBlockEpochHubActorOverflow exercises the real chainsource actor over the
// shared backend. A blocked target whose registration overflows must either
// recover with the latest tip or stop promptly, without disturbing another
// subscriber or the shared stream.
func TestBlockEpochHubActorOverflow(t *testing.T) {
	t.Parallel()

	// The backend forwarder buffers its own queue in front of the hub's,
	// so overflow the pair with margin.
	const backlog = 3 * testBlockQueueSize

	for _, stop := range []bool{false, true} {
		name := "recover"
		if stop {
			name = "cancel"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			backend, client, _ := newTestBlockBackend(t)
			fast, err := backend.RegisterBlocks(t.Context())
			require.NoError(t, err)
			stream := nextTestBlockStream(t, client)

			target := &blockedBlockTarget{
				entered:   make(chan struct{}),
				release:   make(chan struct{}),
				cancelled: make(chan struct{}),
				delivered: make(
					chan chainsource.BlockEpoch, 2*backlog,
				),
			}
			watch := chainsource.NewBlockEpochActor(
				chainsource.BlockEpochConfig{
					Backend: backend,
				},
			)
			defer watch.Stop()

			var notifyRef actor.TellOnlyRef[chainsource.BlockEpoch]
			notifyRef = target
			result := watch.Receive(t.Context(),
				&chainsource.SubscribeBlocksRequest{
					CallerID:    "overflow-test",
					NotifyActor: fn.Some(notifyRef),
				},
			)
			require.True(t, result.IsOk())

			sendTestBlock(stream, 100, 1)
			receiveTestBlock(t, fast, 100, 1)
			select {
			case <-target.entered:
			case <-time.After(epochTestTimeout):
				t.Fatal("actor did not reach blocked target")
			}

			for i := range backlog {
				sendTestBlock(stream, uint32(101+i), byte(2+i))
				receiveTestBlock(
					t, fast, int32(101+i), byte(2+i),
				)
			}
			tip := int32(100 + backlog)

			if stop {
				watch.Stop()
				select {
				case <-target.cancelled:
				default:
					t.Fatal(
						"stop did not cancel " +
							"blocked Tell",
					)
				}
			} else {
				close(target.release)
				deadline := time.After(epochTestTimeout)
				for recovered := false; !recovered; {
					select {
					case epoch := <-target.delivered:
						recovered = epoch.Height == tip

					case <-deadline:
						t.Fatal("tip timeout")
					}
				}
				watch.Stop()
			}

			require.EqualValues(t, 1, client.calls.Load())
			require.EqualValues(t, 1, client.active.Load())
			fast.Cancel()
			require.Zero(t, client.active.Load())
		})
	}
}
