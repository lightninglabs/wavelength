package chainsource

import (
	"context"
	"fmt"
	"sync"

	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// BlockEpochActor is a single-subscription actor that monitors new blocks and
// delivers block epoch events. Each instance serves exactly one subscription.
//
// The actor supports dual-mode operation: Iterator mode for range-based
// iteration, and Actor mode for asynchronous event delivery to a registered
// actor. Each actor creates its own backend registration (no sharing).
type BlockEpochActor struct {
	// backend is the blockchain backend used to monitor blocks.
	backend ChainBackend

	// notifyActor is used in Actor mode to send events. None in Iterator
	// mode.
	notifyActor fn.Option[actor.TellOnlyRef[BlockEpoch]]

	// epochChan is used in Iterator mode to deliver blocks. Nil in Actor
	// mode.
	epochChan chan BlockEpoch

	// registration is the backend registration for this actor.
	registration *BlockRegistration

	// ctx is the actor's context, cancelled when the actor stops.
	ctx context.Context

	// cancel cancels the actor's context.
	cancel context.CancelFunc

	// cancelFunc is the custom cancel function returned in the response.
	cancelFunc func()

	// closeOnce ensures the epochChan is only closed once.
	closeOnce sync.Once

	// wg tracks background goroutines for graceful shutdown.
	wg sync.WaitGroup
}

// NewBlockEpochActor creates a new BlockEpochActor instance with the given
// backend and parent context. The actor waits for a SubscribeBlocksRequest
// message to begin monitoring.
func NewBlockEpochActor(backend ChainBackend,
	parentCtx context.Context) *BlockEpochActor {

	ctx, cancel := context.WithCancel(parentCtx)

	return &BlockEpochActor{
		backend: backend,
		ctx:     ctx,
		cancel:  cancel,
	}
}

// Receive processes incoming messages for the BlockEpochActor.
func (a *BlockEpochActor) Receive(actorCtx context.Context,
	msg EpochMsg) fn.Result[EpochResp] {

	switch m := msg.(type) {
	case *SubscribeBlocksRequest:
		return a.handleSubscribeBlocks(actorCtx, m)

	default:
		return fn.Err[EpochResp](fmt.Errorf("unknown message type: %T",
			msg))
	}
}

// handleSubscribeBlocks processes a block subscription request. It configures
// the actor and starts monitoring.
func (a *BlockEpochActor) handleSubscribeBlocks(actorCtx context.Context,
	req *SubscribeBlocksRequest) fn.Result[EpochResp] {

	// Configure the actor with request parameters.
	a.notifyActor = req.NotifyActor
	resp := &SubscribeBlocksResponse{}

	// No we'll determine the notification mode: notify actor or iterator.
	if req.NotifyActor.IsSome() {
		// Actor mode: no channel needed.
		resp.Cancel = a.cancel
	} else {
		// In iterator mode we use a channel to to funnel block epochs
		// from a listening goroutine into the main iterator. This lets
		// us consume block epochs as the iterator may block.
		a.epochChan = make(chan BlockEpoch, epochChannelSize)

		// Create an iter.Seq that reads from the channel.
		iterator := func(yield func(BlockEpoch) bool) {
			defer func() {
				// Close the channel when iterator finishes.
				a.closeOnce.Do(func() {
					if a.epochChan != nil {
						close(a.epochChan)
					}
				})
				a.cancel()
			}()

			for {
				select {
				case epoch, ok := <-a.epochChan:
					if !ok {
						return
					}

					if !yield(epoch) {
						return
					}

				case <-a.ctx.Done():
					return
				}
			}
		}

		// Wrap Cancel to also clean up properly.
		cancelFunc := func() {
			a.closeOnce.Do(func() {
				if a.epochChan != nil {
					close(a.epochChan)
				}
			})
			a.cancel()
		}

		resp.Iterator = iterator
		resp.Cancel = cancelFunc
		a.cancelFunc = cancelFunc
	}

	// Register with the backend to receive block notifications. We do this
	// before starting the goroutine so we can return an error to the
	// caller if registration fails.
	registration, err := a.backend.RegisterBlocks(a.ctx)
	if err != nil {
		return fn.Err[EpochResp](fmt.Errorf(
			"failed to register for blocks: %w", err))
	}
	a.registration = registration

	// Now we'll make a goroutine to monitor blocks and forward events.
	a.wg.Add(1)
	go a.monitorBlocks()

	return fn.Ok[EpochResp](resp)
}

// monitorBlocks runs in a background goroutine and forwards block events to
// the subscriber (either channel or actor reference).
func (a *BlockEpochActor) monitorBlocks() {
	defer a.wg.Done()

	// Make sure we clean up the registration on exit.
	defer func() {
		if a.registration != nil {
			a.registration.Cancel()
		}
	}()

	for {
		select {
		case epoch, ok := <-a.registration.Epochs:
			if !ok {
				return
			}

			var timestamp int64
			if epoch.BlockHeader != nil {
				timestamp = epoch.BlockHeader.Timestamp.Unix()
			}

			blockEpoch := BlockEpoch{
				Height:    epoch.Height,
				Hash:      *epoch.Hash,
				Timestamp: timestamp,
			}

			// If there's an epoch channel, then we're in iterator
			// mode.
			if a.epochChan != nil {
				select {
				case a.epochChan <- blockEpoch:
				case <-a.ctx.Done():
					return
				}
			} else {
				// Otherwise, this is actor mode, so we'll
				// deliver in the block epoch via a Tell.
				a.notifyActor.WhenSome(func(ref actor.TellOnlyRef[BlockEpoch]) {
					ref.Tell(a.ctx, blockEpoch)
				})
			}

		case <-a.ctx.Done():
			return
		}
	}
}

// Stop gracefully shuts down the BlockEpochActor. It cancels the context and
// waits for the monitoring goroutine to complete.
func (a *BlockEpochActor) Stop() {
	a.cancel()

	a.wg.Wait()
}
