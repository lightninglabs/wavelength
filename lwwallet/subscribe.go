package lwwallet

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/subscribe"
)

// EventServer is a thin generic wrapper around lnd's subscribe.Server
// that delivers typed events to its subscribers. The underlying
// subscribe.Server already handles every concurrency concern we care
// about — single-goroutine subscriber handler (no send-on-closed-
// channel race possible by construction), per-client unbounded
// queue (a slow consumer cannot wedge the broadcaster), idempotent
// Cancel — so EventServer's only job is to keep callers from having
// to type-assert away the interface{} that subscribe.Server speaks.
type EventServer[T any] struct {
	inner *subscribe.Server
	log   btclog.Logger
}

// NewEventServer constructs a typed event server. Start must be
// called before SendUpdate or Subscribe.
func NewEventServer[T any](log btclog.Logger) *EventServer[T] {
	return &EventServer[T]{
		inner: subscribe.NewServer(),
		log:   log,
	}
}

// Start makes the server ready to accept subscriptions and updates.
// Start is idempotent.
func (s *EventServer[T]) Start() error {
	return s.inner.Start()
}

// Stop tears down the subscriber handler and closes every active
// subscription. Stop is idempotent and safe to call concurrently.
func (s *EventServer[T]) Stop() error {
	return s.inner.Stop()
}

// SendUpdate broadcasts a typed event to every active subscriber.
// The call returns ErrServerShuttingDown if the server has been
// stopped.
func (s *EventServer[T]) SendUpdate(event T) error {
	return s.inner.SendUpdate(event)
}

// Subscribe returns a typed subscription. The returned subscription
// owns a translator goroutine that converts subscribe.Server's
// untyped updates into the typed channel exposed via Updates().
// Cancel must be called to release the subscription.
func (s *EventServer[T]) Subscribe() (*Subscription[T], error) {
	client, err := s.inner.Subscribe()
	if err != nil {
		return nil, fmt.Errorf("subscribe: %w", err)
	}

	sub := &Subscription[T]{
		inner: client,
		out:   make(chan T, 1),
		quit:  make(chan struct{}),
		log:   s.log,
	}

	sub.wg.Add(1)
	go sub.translate()

	return sub, nil
}

// Subscription is a typed handle on an active subscribe.Client. It
// converts the inner channel of interface{} updates into a typed
// channel of T events on a dedicated translator goroutine.
type Subscription[T any] struct {
	inner *subscribe.Client

	out  chan T
	quit chan struct{}

	cancelOnce sync.Once
	wg         sync.WaitGroup

	log btclog.Logger
}

// Updates returns the typed event channel. The channel is closed
// when the subscription is cancelled or the upstream server is
// stopped.
func (s *Subscription[T]) Updates() <-chan T {
	return s.out
}

// Quit returns a channel closed when the upstream server is
// shutting down. Consumers should select on Updates and Quit to
// react to either a new event or the server going away.
func (s *Subscription[T]) Quit() <-chan struct{} {
	return s.inner.Quit()
}

// Cancel deregisters the subscription and waits for the translator
// goroutine to exit. Cancel is idempotent and safe to call from
// any goroutine; calling it from inside an Updates handler is fine
// because the translator unblocks via the local quit channel.
func (s *Subscription[T]) Cancel() {
	s.cancelOnce.Do(func() {
		// Close the local quit first so the translator can
		// escape a parked send into the typed out channel
		// before we block on inner.Cancel(). inner.Cancel
		// blocks until the server handler removes us, which in
		// turn closes inner.Quit(); the translator picks that
		// up next.
		close(s.quit)
		s.inner.Cancel()
	})

	s.wg.Wait()
}

// translate is the per-subscription goroutine that pulls untyped
// updates off the inner subscribe.Client, asserts them to T, and
// forwards them to the typed out channel. The goroutine exits when
// either the inner server signals shutdown or the local Cancel
// fires.
func (s *Subscription[T]) translate() {
	defer s.wg.Done()
	defer close(s.out)

	for {
		select {
		case upd, ok := <-s.inner.Updates():
			if !ok {
				return
			}

			typed, ok := upd.(T)
			if !ok {
				// The inner server should only ever
				// deliver T values because SendUpdate
				// only accepts T; a type mismatch here
				// is a programming bug.
				s.log.ErrorS(context.Background(),
					"Event server type assertion failed",
					fmt.Errorf("got %T", upd),
					slog.String(
						"want", fmt.Sprintf("%T",
							*new(T)),
					))

				continue
			}

			select {
			case s.out <- typed:

			case <-s.quit:
				return

			case <-s.inner.Quit():
				return
			}

		case <-s.inner.Quit():
			return

		case <-s.quit:
			return
		}
	}
}
