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
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
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

// Recv blocks until a notification, stream failure, or cancellation arrives.
func (s *testBlockStream) Recv() (*chainrpc.BlockEpoch, error) {
	select {
	case result := <-s.results:
		if result.err != nil {
			s.once.Do(func() { s.owner.active.Add(-1) })
		}

		return result.epoch, result.err

	case <-s.ctx.Done():
		s.once.Do(func() { s.owner.active.Add(-1) })

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
	fail      atomic.Bool
}

// RegisterBlockEpochNtfn captures authentication and an independent request.
func (c *testBlockClient) RegisterBlockEpochNtfn(ctx context.Context,
	request *chainrpc.BlockEpoch, _ ...grpc.CallOption) (
	chainrpc.ChainNotifier_RegisterBlockEpochNtfnClient, error) {

	c.calls.Add(1)
	if c.fail.Swap(false) {
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
		ctx: ctx, owner: c,
		request: &chainrpc.BlockEpoch{
			Height: request.Height,
			Hash:   append([]byte(nil), request.Hash...),
		},
		results: make(chan blockStreamResult, 100),
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

// testBlockChainKit counts hash lookups to guard against per-subscriber RPCs.
type testBlockChainKit struct {
	lndclient.ChainKitClient
	hashCalls atomic.Int32
}

// GetBlockHash records any unexpected attempt to refetch a notification hash.
func (k *testBlockChainKit) GetBlockHash(context.Context, int64) (
	chainhash.Hash, error) {

	k.hashCalls.Add(1)

	return chainhash.Hash{}, errors.New("hash must come from notification")
}

// newTestBlockBackend builds the real adapter/backend over a controllable RPC.
func newTestBlockBackend(t *testing.T) (*LNDBackend, *testBlockClient,
	*testBlockChainKit) {

	t.Helper()
	client := &testBlockClient{opened: make(chan *testBlockStream, 1000)}
	kit := &testBlockChainKit{}
	notifier := NewLndClientChainNotifier(LndClientChainNotifierConfig{
		LND: &lndclient.LndServices{
			ChainNotifier: &testBlockNotifier{client: client},
			ChainKit:      kit,
		},
	})
	b := NewLNDBackend(notifier, &stubFeeEstimator{}, &stubBroadcaster{})
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

	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not register")

		return nil
	}
}

// sendTestBlock emits a raw event with independently chosen height and hash.
func sendTestBlock(stream *testBlockStream, height uint32, hash byte) {
	blockHash := chainhash.Hash{hash}
	stream.results <- blockStreamResult{epoch: &chainrpc.BlockEpoch{
		Height: height, Hash: blockHash[:],
	}}
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

	case <-time.After(5 * time.Second):
		t.Fatal("block was not delivered")

		return nil
	}
}

// TestLNDBlocksManySubscribers proves fanout does not multiply streams or hash
// RPCs, even with simultaneous registrations, same-height replacements, and
// decreasing heights. A late subscriber receives an independent tip snapshot.
func TestLNDBlocksManySubscribers(t *testing.T) {
	t.Parallel()
	b, client, kit := newTestBlockBackend(t)
	const subscribers = 413
	subs := make([]*chainsource.BlockRegistration, subscribers)
	var wg sync.WaitGroup
	errs := make(chan error, subscribers)
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
	for i, height := range []uint32{100, 101, 101, 99, 100} {
		hash := byte(i + 1)
		sendTestBlock(stream, height, hash)
		for _, sub := range subs {
			epoch := receiveTestBlock(t, sub, int32(height), hash)
			// Mutation must not corrupt another queue or cached
			// tip.
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
	late.Cancel()
	require.Zero(t, client.active.Load())
	require.EqualValues(t, 1, client.maxActive.Load())
}

// TestLNDBlocksSlowSubscriber closes a bounded stalled queue while healthy
// subscribers continue to receive every epoch in order on the original stream.
func TestLNDBlocksSlowSubscriber(t *testing.T) {
	t.Parallel()
	b, client, _ := newTestBlockBackend(t)
	slow, err := b.RegisterBlocks(t.Context())
	require.NoError(t, err)
	fast, err := b.RegisterBlocks(t.Context())
	require.NoError(t, err)
	stream := nextTestBlockStream(t, client)
	for i := range lndBlockBufferSize + 2 {
		sendTestBlock(stream, uint32(i+1), byte(i+1))
		receiveTestBlock(t, fast, int32(i+1), byte(i+1))
	}
	for i := range lndBlockBufferSize {
		receiveTestBlock(t, slow, int32(i+1), byte(i+1))
	}
	_, ok := <-slow.Epochs
	require.False(t, ok)
	slow.Cancel()
	require.EqualValues(t, 1, client.calls.Load())
	require.EqualValues(t, 1, client.active.Load())
}

// TestLNDBlocksReconnect verifies one reconnect serves all subscribers and
// passes the last delivered hash/height cursor rather than asking for tip only.
func TestLNDBlocksReconnect(t *testing.T) {
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
	// Exercise a failed reconnect before recovery, without losing owners.
	client.fail.Store(true)
	stream.results <- blockStreamResult{err: errors.New("connection lost")}
	reconnected := nextTestBlockStream(t, client)
	require.EqualValues(t, 100, reconnected.request.Height)
	wantHash := chainhash.Hash{1}
	require.Equal(t, wantHash[:], reconnected.request.Hash)
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

// TestLNDBlocksSubscriberContext verifies one caller cannot cancel another
// subscriber's upstream, and that a new run starts after the last cancellation.
func TestLNDBlocksSubscriberContext(t *testing.T) {
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
	stream = nextTestBlockStream(t, client)
	require.Empty(t, stream.request.Hash)
	sendTestBlock(stream, 101, 2)
	receiveTestBlock(t, third, 101, 2)
	require.EqualValues(t, 1, client.maxActive.Load())
}

// TestLNDBlocksStop closes all subscriptions, joins the raw receive, and
// rejects new registrations. Cancellation remains safe after backend shutdown.
func TestLNDBlocksStop(t *testing.T) {
	t.Parallel()
	b, client, _ := newTestBlockBackend(t)
	sub, err := b.RegisterBlocks(t.Context())
	require.NoError(t, err)
	stream := nextTestBlockStream(t, client)
	require.NoError(t, b.Stop())
	_, ok := <-sub.Epochs
	require.False(t, ok)
	require.ErrorIs(t, stream.ctx.Err(), context.Canceled)
	require.Zero(t, client.active.Load())
	sub.Cancel()
	_, err = b.RegisterBlocks(t.Context())
	require.ErrorContains(t, err, "stopped")
}

// TestLNDBlocksInitialFailure verifies failed stream establishment does not
// poison the backend and an already-cancelled caller never opens a stream.
func TestLNDBlocksInitialFailure(t *testing.T) {
	t.Parallel()
	b, client, _ := newTestBlockBackend(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := b.RegisterBlocks(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, client.calls.Load())
	client.fail.Store(true)
	_, err = b.RegisterBlocks(t.Context())
	require.ErrorContains(t, err, "registration failed")
	sub, err := b.RegisterBlocks(t.Context())
	require.NoError(t, err)
	stream := nextTestBlockStream(t, client)
	sendTestBlock(stream, 100, 1)
	receiveTestBlock(t, sub, 100, 1)
}

// TestLNDBlocksConcurrentCancellation covers last-owner/new-owner races without
// overlapping raw streams or closing a channel while fanout is using it.
func TestLNDBlocksConcurrentCancellation(t *testing.T) {
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

// TestLndClientBlockEpochMalformed ensures malformed wire events close the
// registration and cancel its receive context, allowing cursor-based recovery.
func TestLndClientBlockEpochMalformed(t *testing.T) {
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
				Height: 1 << 31, Hash: make(
					[]byte, chainhash.HashSize,
				),
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			b, client, kit := newTestBlockBackend(t)
			cursorHash := chainhash.Hash{42}
			event, err := b.notifier.RegisterBlockEpochNtfn(
				&chainntnfs.BlockEpoch{
					Height: 100, Hash: &cursorHash,
				},
			)
			require.NoError(t, err)
			defer event.Cancel()
			stream := nextTestBlockStream(t, client)
			require.EqualValues(t, 100, stream.request.Height)
			require.Equal(t, cursorHash[:], stream.request.Hash)
			stream.results <- blockStreamResult{epoch: test.epoch}
			select {
			case _, ok := <-event.Epochs:
				require.False(
					t, ok, "malformed epoch was forwarded",
				)

			case <-time.After(time.Second):
				t.Fatal(
					"malformed event did not close " +
						"registration",
				)
			}
			require.ErrorIs(t, stream.ctx.Err(), context.Canceled)
			require.Zero(t, kit.hashCalls.Load())
		})
	}
}

// TestLNDBlocksStopDuringReconnect ensures shutdown interrupts the reconnect
// backoff and cannot leave a reconnecting worker or open subscriber behind.
func TestLNDBlocksStopDuringReconnect(t *testing.T) {
	t.Parallel()
	b, client, _ := newTestBlockBackend(t)
	sub, err := b.RegisterBlocks(t.Context())
	require.NoError(t, err)
	stream := nextTestBlockStream(t, client)
	stream.results <- blockStreamResult{err: errors.New("connection lost")}
	select {
	case <-stream.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("failed stream was not cancelled")
	}
	require.NoError(t, b.Stop())
	_, ok := <-sub.Epochs
	require.False(t, ok)
	require.EqualValues(t, 1, client.calls.Load())
}
