package chainbackends

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
)

const (
	// blockEpochQueueSize bounds each registration's pending epochs. It
	// must absorb lnd's catch-up burst after a reconnect, which replays
	// every block missed during the outage at once: a small queue would
	// overflow every registration that is not draining in lockstep, and
	// each would then re-register and see only the tip. 1024 covers about
	// a week of missed blocks for 8 KiB of channel buffer per
	// registration. A registration that still falls this far behind is
	// wedged, and is closed rather than stalling the shared reader or
	// growing memory without bound.
	blockEpochQueueSize = 1024

	// blockEpochDedicatedQueueSize buffers a dedicated cursor stream. Its
	// forwarder blocks instead of overflowing, so it needs no headroom.
	blockEpochDedicatedQueueSize = 10

	// blockEpochReconnectBackoff is the first delay before the shared
	// reader reopens a failed upstream stream.
	blockEpochReconnectBackoff = time.Second

	// blockEpochMaxReconnectBackoff caps the reconnect delay during an
	// outage.
	blockEpochMaxReconnectBackoff = 30 * time.Second
)

var (
	// errNotifierStopped is returned by block epoch registrations made
	// after the notifier was stopped, including registrations that were
	// waiting on a stream open that Stop cancelled.
	errNotifierStopped = errors.New("chain notifier stopped")

	// errMalformedBlockEpoch reports a wire event that cannot be turned
	// into a block identity. The stream is abandoned so the reader can
	// resume from its last valid cursor instead of skipping a block.
	errMalformedBlockEpoch = errors.New("malformed block epoch")
)

// blockEpochStream is one authenticated chainrpc block epoch stream. Unlike
// lndclient's height-only wrapper, every event carries the block hash, so no
// ChainKit lookup is needed and the identity stays tied to the notification
// even if the chain reorgs between the event and its delivery.
type blockEpochStream struct {
	recv   chainrpc.ChainNotifier_RegisterBlockEpochNtfnClient
	cancel context.CancelFunc
}

// openBlockEpochStream opens a raw chainrpc block stream under parent. A nil
// cursor asks lnd for the current tip; a non-nil cursor asks lnd to deliver
// every block connected after it, rewinding to the common ancestor if the
// cursor was reorged out. Establishment is bounded by lndRegistrationTimeout
// without imposing a deadline on the healthy stream.
func (n *LndClientChainNotifier) openBlockEpochStream(parent context.Context,
	cursor *chainntnfs.BlockEpoch) (*blockEpochStream, error) {

	request := &chainrpc.BlockEpoch{}
	if cursor != nil {
		if cursor.Hash == nil || cursor.Height < 0 {
			return nil, fmt.Errorf("invalid block epoch cursor")
		}
		request.Hash = cursor.Hash[:]
		request.Height = uint32(cursor.Height)
	}

	ctx, cancel := context.WithCancel(parent)
	authCtx, _, client := n.cfg.LND.ChainNotifier.RawClientWithMacAuth(ctx)

	// Cancellation interrupts a blocked gRPC registration, so the timer
	// both bounds establishment and lets the parent abort it early.
	timer := time.AfterFunc(lndRegistrationTimeout, cancel)
	recv, err := client.RegisterBlockEpochNtfn(authCtx, request)
	if !timer.Stop() && err == nil {
		err = context.DeadlineExceeded
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		cancel()

		return nil, fmt.Errorf("open block epoch stream: %w", err)
	}

	return &blockEpochStream{recv: recv, cancel: cancel}, nil
}

// next blocks for the next event and validates it into a block identity.
func (s *blockEpochStream) next() (*chainntnfs.BlockEpoch, error) {
	epoch, err := s.recv.Recv()
	if err != nil {
		return nil, err
	}

	if epoch == nil || len(epoch.Hash) != chainhash.HashSize ||
		epoch.Height > math.MaxInt32 {
		return nil, errMalformedBlockEpoch
	}

	var hash chainhash.Hash
	copy(hash[:], epoch.Hash)

	return &chainntnfs.BlockEpoch{
		Height: int32(epoch.Height),
		Hash:   &hash,
	}, nil
}

// copyBlockEpoch returns an independent copy, so no registration can observe
// another's mutation of the shared hash.
func copyBlockEpoch(epoch *chainntnfs.BlockEpoch) *chainntnfs.BlockEpoch {
	hash := *epoch.Hash

	return &chainntnfs.BlockEpoch{
		Height:      epoch.Height,
		Hash:        &hash,
		BlockHeader: epoch.BlockHeader,
	}
}

// blockEpochOpener opens a raw block stream under ctx from an optional cursor.
type blockEpochOpener func(ctx context.Context,
	cursor *chainntnfs.BlockEpoch) (*blockEpochStream, error)

// blockEpochHub multiplexes one raw lnd block epoch stream across every
// tip-following block registration of a notifier.
//
// Without it, each registration opened its own gRPC stream to lnd and
// resolved every height with its own ChainKit.GetBlockHash call, so a client
// with hundreds of subscribers (one per live VTXO actor, plus finality
// watches) held hundreds of streams and made hundreds of identical RPCs per
// block. The hub opens one stream on demand, reads the hash from the event
// itself, and fans the epoch out in-process.
//
// Ownership rules that keep teardown race-free:
//   - The upstream belongs to the hub, never to the registration that opened
//     it. It ends when its last registration cancels or the hub stops, and
//     stays installed until its reader exits, so a new registration racing
//     the last cancellation waits instead of opening an overlapping stream.
//   - Upstream failures are handled once, by the reader: it reconnects with
//     backoff from the last delivered block while registrations stay open.
//     Closing every registration instead would make each owner reconnect at
//     once, the thundering herd the hub exists to prevent.
//   - Each registration channel is sent to and closed only under mu, so a
//     cancellation can never race a send.
//   - mu is never held across a blocking call. The first open runs unlocked
//     behind an explicit opening state; waiters join its result.
type blockEpochHub struct {
	// open opens a raw block stream from the given cursor.
	open blockEpochOpener

	// log resolves the logger for a context.
	log func(context.Context) btclog.Logger

	// queueSize is each registration's queue capacity.
	queueSize int

	// minBackoff and maxBackoff bound the reader's reconnect delay.
	minBackoff time.Duration
	maxBackoff time.Duration

	// ctx is the root of every stream the hub opens. Stop cancels it.
	//
	//nolint:containedctx // the hub, not any caller, owns its streams
	ctx    context.Context
	cancel context.CancelFunc

	mu sync.Mutex

	// stopped is set by stop. No registration succeeds afterwards.
	stopped bool

	// up is the current upstream generation, or nil. It remains set while
	// a cancelled generation's reader is still exiting.
	up *epochUpstream

	// opening is the in-flight first open, or nil. Registrations that
	// find it set wait for it instead of opening a second stream.
	opening *epochOpen

	// wg tracks upstream readers, dedicated streams, and in-flight opens.
	wg sync.WaitGroup
}

// epochOpen is one in-flight attempt to open a new upstream generation.
type epochOpen struct {
	// done is closed once the attempt resolved. err is set before.
	done chan struct{}
	err  error
}

// epochUpstream is one generation of the shared upstream stream.
type epochUpstream struct {
	//nolint:containedctx // the generation owns its reader's lifetime
	ctx    context.Context
	cancel context.CancelFunc

	// done is closed after the reader exited and closed every
	// registration of this generation.
	done chan struct{}

	// The fields below are guarded by blockEpochHub.mu.

	// subs holds this generation's registrations.
	subs map[*epochSub]struct{}

	// tip is the latest delivered epoch. New registrations receive it
	// first, mirroring lnd sending the current tip on a fresh stream.
	tip *chainntnfs.BlockEpoch
}

// epochSub is a single registration's bounded delivery queue.
type epochSub struct {
	epochs chan *chainntnfs.BlockEpoch
}

// newBlockEpochHub returns a hub that opens its streams through open.
func newBlockEpochHub(open blockEpochOpener,
	log func(context.Context) btclog.Logger) *blockEpochHub {

	ctx, cancel := context.WithCancel(context.Background())

	return &blockEpochHub{
		open:       open,
		log:        log,
		queueSize:  blockEpochQueueSize,
		minBackoff: blockEpochReconnectBackoff,
		maxBackoff: blockEpochMaxReconnectBackoff,
		ctx:        ctx,
		cancel:     cancel,
	}
}

// register adds a tip-following registration, opening the shared stream if
// none is open.
func (h *blockEpochHub) register() (*chainntnfs.BlockEpochEvent, error) {
	for {
		h.mu.Lock()
		if h.stopped {
			h.mu.Unlock()

			return nil, errNotifierStopped
		}

		switch {
		// The previous generation is still exiting. Wait for its
		// reader so two upstream streams never overlap.
		case h.up != nil && h.up.ctx.Err() != nil:
			done := h.up.done
			h.mu.Unlock()
			<-done

			continue

		// Join the open stream.
		case h.up != nil:
			event := h.addSubLocked(h.up)
			h.mu.Unlock()

			return event, nil

		// Another registration is opening the stream: wait for it and
		// join the result.
		case h.opening != nil:
			attempt := h.opening
			h.mu.Unlock()
			<-attempt.done
			if attempt.err != nil {
				return nil, attempt.err
			}

			continue
		}

		// Open the stream ourselves without holding mu, so Stop can
		// cancel a slow open instead of waiting behind it.
		attempt := &epochOpen{done: make(chan struct{})}
		h.opening = attempt
		h.wg.Add(1)
		h.mu.Unlock()

		return h.openUpstream(attempt)
	}
}

// openUpstream performs the open published as h.opening and resolves it,
// either installing a new generation or failing every waiter.
func (h *blockEpochHub) openUpstream(attempt *epochOpen) (
	*chainntnfs.BlockEpochEvent, error) {

	defer h.wg.Done()

	ctx, cancel := context.WithCancel(h.ctx)
	stream, err := h.open(ctx, nil)

	h.mu.Lock()
	h.opening = nil

	// A stop wins over whatever the open returned: cancelling the open
	// usually surfaces as a gRPC Canceled error, and a stream that opened
	// anyway must not outlive Stop.
	if h.stopped {
		err = errNotifierStopped
	}
	if err != nil {
		attempt.err = err
		close(attempt.done)
		h.mu.Unlock()

		if stream != nil {
			stream.cancel()
		}
		cancel()

		return nil, err
	}

	up := &epochUpstream{
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
		subs:   make(map[*epochSub]struct{}),
	}
	h.up = up

	h.wg.Add(1)
	go h.runUpstream(up, stream)

	event := h.addSubLocked(up)
	close(attempt.done)
	h.mu.Unlock()

	h.log(ctx).InfoS(ctx, "Opened shared block epoch stream")

	return event, nil
}

// addSubLocked creates a registration in the given generation, seeded with
// the cached tip. The caller must hold mu, the same lock publish holds while
// caching a tip and fanning it out, so a registration sees each epoch once.
func (h *blockEpochHub) addSubLocked(
	up *epochUpstream) *chainntnfs.BlockEpochEvent {

	sub := &epochSub{
		epochs: make(chan *chainntnfs.BlockEpoch, h.queueSize),
	}
	if up.tip != nil {
		sub.epochs <- copyBlockEpoch(up.tip)
	}
	up.subs[sub] = struct{}{}

	var cancelOnce sync.Once

	return &chainntnfs.BlockEpochEvent{
		Epochs: sub.epochs,
		Cancel: func() {
			cancelOnce.Do(func() {
				h.removeSub(up, sub)
			})
		},
	}
}

// removeSub closes one registration. Removing the last one cancels the
// generation and joins its reader, so the upstream stream is fully released
// when the final Cancel returns.
func (h *blockEpochHub) removeSub(up *epochUpstream, sub *epochSub) {
	h.mu.Lock()
	if _, ok := up.subs[sub]; ok {
		delete(up.subs, sub)
		close(sub.epochs)
	}
	last := len(up.subs) == 0
	if last {
		up.cancel()
	}
	h.mu.Unlock()

	if last {
		<-up.done
	}
}

// runUpstream reads one generation's stream and fans each epoch out. A stream
// failure or malformed event is handled here, once for every registration:
// the reader reconnects with backoff, resuming from the last delivered block.
// Live epochs keep upstream order with no height-based deduplication, which
// would hide same-height or lower-height reorg replacements.
func (h *blockEpochHub) runUpstream(up *epochUpstream,
	stream *blockEpochStream) {

	defer h.wg.Done()
	defer h.endUpstream(up)

	ctx := up.ctx
	var cursor *chainntnfs.BlockEpoch
	backoff := h.minBackoff
	for {
		epoch, err := stream.next()
		if err != nil {
			stream.cancel()
			if ctx.Err() != nil {
				return
			}

			h.log(ctx).WarnS(ctx, "Shared block epoch stream ended",
				err,
				slog.Duration("reconnect_in", backoff),
			)

			stream = h.reconnect(up, cursor, &backoff)
			if stream == nil {
				return
			}

			continue
		}

		h.publish(up, epoch)

		cursor = epoch
		backoff = h.minBackoff
	}
}

// reconnect reopens the generation's stream from cursor, waiting backoff
// before each attempt. It returns nil only once the generation is cancelled.
// The backoff doubles after every attempt and is reset by the caller only
// after a valid event, so a stream that opens and then fails at once still
// backs off.
func (h *blockEpochHub) reconnect(up *epochUpstream,
	cursor *chainntnfs.BlockEpoch,
	backoff *time.Duration) *blockEpochStream {

	ctx := up.ctx
	for {
		timer := time.NewTimer(*backoff)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()

			return nil
		}

		stream, err := h.open(ctx, cursor)
		*backoff = min(*backoff*2, h.maxBackoff)
		if err == nil {
			h.log(ctx).InfoS(ctx, "Reconnected block epoch stream",
				slog.Bool("resumed", cursor != nil),
			)

			return stream
		}
		if ctx.Err() != nil {
			return nil
		}

		h.log(ctx).InfoS(ctx, "Block epoch stream reconnect failed",
			slog.Any("err", err),
			slog.Duration("retry_in", *backoff),
		)
	}
}

// publish caches an epoch as the generation's tip and gives every
// registration its own copy. A full queue is closed rather than dropping one
// event or stalling the reader; its owner re-registers and reconciles against
// the live tip.
func (h *blockEpochHub) publish(up *epochUpstream,
	epoch *chainntnfs.BlockEpoch) {

	h.mu.Lock()
	defer h.mu.Unlock()

	up.tip = epoch
	for sub := range up.subs {
		select {
		case sub.epochs <- copyBlockEpoch(epoch):
		default:
			delete(up.subs, sub)
			close(sub.epochs)
			h.log(up.ctx).InfoS(up.ctx, "Closing slow block "+
				"epoch subscriber",
				slog.Int("height", int(epoch.Height)),
				slog.Int("queue_size", h.queueSize),
			)
		}
	}

	h.log(up.ctx).DebugS(up.ctx, "Fanned out block epoch",
		slog.Int("height", int(epoch.Height)),
		slog.Int("subscribers", len(up.subs)),
	)

	if len(up.subs) == 0 {
		up.cancel()
	}
}

// endUpstream closes a finished generation's registrations and detaches it.
// It runs only after the generation was cancelled, by its last registration
// or by stop, never on an upstream failure.
func (h *blockEpochHub) endUpstream(up *epochUpstream) {
	up.cancel()

	h.mu.Lock()
	n := len(up.subs)
	for sub := range up.subs {
		delete(up.subs, sub)
		close(sub.epochs)
	}
	if h.up == up {
		h.up = nil
	}
	close(up.done)
	h.mu.Unlock()

	h.log(up.ctx).InfoS(up.ctx, "Shared block epoch stream closed",
		slog.Int("subscribers", n),
	)
}

// registerDedicated opens a stream of its own for a caller that supplied a
// cursor, since lnd's historical catch-up from that cursor cannot be shared.
// It is not used by tip-following registrations.
func (h *blockEpochHub) registerDedicated(cursor *chainntnfs.BlockEpoch) (
	*chainntnfs.BlockEpochEvent, error) {

	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()

		return nil, errNotifierStopped
	}
	h.wg.Add(1)
	h.mu.Unlock()

	ctx, cancel := context.WithCancel(h.ctx)
	stream, err := h.open(ctx, cursor)
	if err != nil {
		cancel()
		h.wg.Done()

		return nil, err
	}

	epochs := make(
		chan *chainntnfs.BlockEpoch, blockEpochDedicatedQueueSize,
	)
	done := make(chan struct{})
	go func() {
		defer h.wg.Done()
		defer close(done)
		defer close(epochs)
		defer cancel()

		h.forwardDedicated(ctx, stream, epochs)
	}()

	return &chainntnfs.BlockEpochEvent{
		Epochs: epochs,
		Cancel: func() {
			cancel()
			<-done
		},
	}, nil
}

// forwardDedicated copies a dedicated stream's epochs until it fails or ctx
// ends. Unlike the shared reader it never reconnects: its owner asked for
// catch-up from a specific block and must decide how to resume.
func (h *blockEpochHub) forwardDedicated(ctx context.Context,
	stream *blockEpochStream, epochs chan<- *chainntnfs.BlockEpoch) {

	defer stream.cancel()

	for {
		epoch, err := stream.next()
		if err != nil {
			if !isBlockEpochShutdownError(ctx, err) {
				h.log(ctx).WarnS(ctx, "Dedicated block epoch "+
					"stream ended", err)
			}

			return
		}

		select {
		case epochs <- epoch:
		case <-ctx.Done():
			return
		}
	}
}

// stop cancels any in-flight open, ends every stream, closes every
// registration, and waits for every hub goroutine to exit. Registrations
// made afterwards fail with errNotifierStopped.
func (h *blockEpochHub) stop() {
	h.mu.Lock()
	h.stopped = true
	h.mu.Unlock()

	// Every stream and open derives from the root context, so this one
	// cancellation reaches all of them, including a blocked open.
	h.cancel()
	h.wg.Wait()
}
