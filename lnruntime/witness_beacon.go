package lnruntime

import (
	"fmt"
	"sync"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
)

// WitnessBeacon persists invoice preimages in lnd's channel database and
// provides the subscription surface expected by native channel links.
type WitnessBeacon struct {
	cache *channeldb.WitnessCache

	mu          sync.Mutex
	nextID      uint64
	subscribers map[uint64]*witnessSubscriber
}

// witnessSubscriber relays an unbounded in-memory queue to one resolver so a
// temporarily slow consumer cannot make AddPreimages block or drop a witness.
type witnessSubscriber struct {
	updates chan lntypes.Preimage
	wake    chan struct{}
	quit    chan struct{}
	done    chan struct{}
	stop    sync.Once

	mu      sync.Mutex
	pending []lntypes.Preimage
	head    int
	closed  bool
}

// NewWitnessBeacon constructs a persistent witness beacon.
func NewWitnessBeacon(db *channeldb.DB) (*WitnessBeacon, error) {
	if db == nil {
		return nil, fmt.Errorf("channel database is required")
	}

	return &WitnessBeacon{
		cache: db.NewWitnessCache(),
		subscribers: make(
			map[uint64]*witnessSubscriber,
		),
	}, nil
}

// SubscribeUpdates registers a resolver for future preimages. A subscriber
// can always recover a missed update through LookupPreimage.
func (b *WitnessBeacon) SubscribeUpdates(lnwire.ShortChannelID, *chanstate.HTLC,
	*hop.Payload, []byte) (*contractcourt.WitnessSubscription, error) {

	b.mu.Lock()
	b.nextID++
	id := b.nextID
	subscriber := newWitnessSubscriber()
	b.subscribers[id] = subscriber
	b.mu.Unlock()

	var once sync.Once

	return &contractcourt.WitnessSubscription{
		WitnessUpdates: subscriber.updates,
		CancelSubscription: func() {
			once.Do(func() {
				b.mu.Lock()
				delete(b.subscribers, id)
				b.mu.Unlock()
				subscriber.cancel()
			})
		},
	}, nil
}

// newWitnessSubscriber starts one lossless subscriber relay.
func newWitnessSubscriber() *witnessSubscriber {
	subscriber := &witnessSubscriber{
		updates: make(chan lntypes.Preimage, 16),
		wake:    make(chan struct{}, 1),
		quit:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go subscriber.run()

	return subscriber
}

// enqueue appends one persisted preimage without waiting for the resolver.
func (s *witnessSubscriber) enqueue(preimage lntypes.Preimage) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()

		return
	}
	s.pending = append(s.pending, preimage)
	s.mu.Unlock()

	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// run delivers queued preimages in insertion order.
func (s *witnessSubscriber) run() {
	defer close(s.done)
	defer close(s.updates)

	for {
		preimage, ok := s.next()
		if !ok {
			select {
			case <-s.wake:
				continue

			case <-s.quit:
				return
			}
		}

		select {
		case s.updates <- preimage:
			s.advance()

		case <-s.quit:
			return
		}
	}
}

// next returns the oldest undelivered preimage.
func (s *witnessSubscriber) next() (lntypes.Preimage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || s.head >= len(s.pending) {
		return lntypes.Preimage{}, false
	}

	return s.pending[s.head], true
}

// advance consumes the preimage most recently returned by next.
func (s *witnessSubscriber) advance() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || s.head >= len(s.pending) {
		return
	}
	s.head++
	if s.head == len(s.pending) {
		s.pending = s.pending[:0]
		s.head = 0
	}
}

// cancel stops the relay and waits until it closes the public update channel.
func (s *witnessSubscriber) cancel() {
	s.stop.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.pending = nil
		s.head = 0
		s.mu.Unlock()
		close(s.quit)
	})
	<-s.done
}

// LookupPreimage reads lnd's persistent witness cache.
func (b *WitnessBeacon) LookupPreimage(hash lntypes.Hash) (lntypes.Preimage,
	bool) {

	preimage, err := b.cache.LookupSha256Witness(hash)

	return preimage, err == nil
}

// AddPreimages persists preimages before notifying live subscribers.
func (b *WitnessBeacon) AddPreimages(preimages ...lntypes.Preimage) error {
	if err := b.cache.AddSha256Witnesses(preimages...); err != nil {
		return err
	}

	b.mu.Lock()
	subscribers := make([]*witnessSubscriber, 0, len(b.subscribers))
	for _, subscriber := range b.subscribers {
		subscribers = append(subscribers, subscriber)
	}
	b.mu.Unlock()

	for _, preimage := range preimages {
		for _, subscriber := range subscribers {
			subscriber.enqueue(preimage)
		}
	}

	return nil
}

var _ contractcourt.WitnessBeacon = (*WitnessBeacon)(nil)
