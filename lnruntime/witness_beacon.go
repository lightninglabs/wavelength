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
	subscribers map[uint64]chan lntypes.Preimage
}

// NewWitnessBeacon constructs a persistent witness beacon.
func NewWitnessBeacon(db *channeldb.DB) (*WitnessBeacon, error) {
	if db == nil {
		return nil, fmt.Errorf("channel database is required")
	}

	return &WitnessBeacon{
		cache: db.NewWitnessCache(),
		subscribers: make(
			map[uint64]chan lntypes.Preimage,
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
	updates := make(chan lntypes.Preimage, 16)
	b.subscribers[id] = updates
	b.mu.Unlock()

	var once sync.Once

	return &contractcourt.WitnessSubscription{
		WitnessUpdates: updates,
		CancelSubscription: func() {
			once.Do(func() {
				b.mu.Lock()
				delete(b.subscribers, id)
				close(updates)
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
	defer b.mu.Unlock()
	for _, preimage := range preimages {
		for _, updates := range b.subscribers {
			select {
			case updates <- preimage:
			default:
			}
		}
	}

	return nil
}

var _ contractcourt.WitnessBeacon = (*WitnessBeacon)(nil)
