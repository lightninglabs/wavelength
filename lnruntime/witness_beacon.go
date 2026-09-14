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

// witnessSubscriber holds the one preimage an HTLC resolver is waiting for.
type witnessSubscriber struct {
	updates chan lntypes.Preimage
	hash    lntypes.Hash
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

// SubscribeUpdates registers a resolver for its HTLC's future preimage.
func (b *WitnessBeacon) SubscribeUpdates(_ lnwire.ShortChannelID,
	htlc *chanstate.HTLC, _ *hop.Payload, _ []byte) (
	*contractcourt.WitnessSubscription, error) {

	if htlc == nil {
		return nil, fmt.Errorf("HTLC is required")
	}

	b.mu.Lock()
	b.nextID++
	id := b.nextID
	subscriber := &witnessSubscriber{
		updates: make(chan lntypes.Preimage, 1),
		hash:    htlc.RHash,
	}
	b.subscribers[id] = subscriber
	b.mu.Unlock()

	var once sync.Once

	return &contractcourt.WitnessSubscription{
		WitnessUpdates: subscriber.updates,
		CancelSubscription: func() {
			once.Do(func() {
				b.mu.Lock()
				delete(b.subscribers, id)
				close(subscriber.updates)
				b.mu.Unlock()
			})
		},
	}, nil
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
	for _, preimage := range preimages {
		hash := preimage.Hash()
		for _, subscriber := range b.subscribers {
			if subscriber.hash != hash {
				continue
			}

			// A full channel already contains the only witness this
			// HTLC resolver needs.
			select {
			case subscriber.updates <- preimage:
			default:
			}
		}
	}
	b.mu.Unlock()

	return nil
}

var _ contractcourt.WitnessBeacon = (*WitnessBeacon)(nil)
