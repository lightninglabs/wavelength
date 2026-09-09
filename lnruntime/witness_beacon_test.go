package lnruntime

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestWitnessBeaconPrioritizesSubscribedPreimage verifies unrelated witnesses
// cannot fill a resolver's bounded notification channel and hide its preimage.
func TestWitnessBeaconPrioritizesSubscribedPreimage(t *testing.T) {
	t.Parallel()

	db := channeldb.OpenForTesting(t, t.TempDir())
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	beacon, err := NewWitnessBeacon(db)
	require.NoError(t, err)
	target := lntypes.Preimage{255, 1}
	subscription, err := beacon.SubscribeUpdates(
		lnwire.ShortChannelID{}, &channeldb.HTLC{
			RHash: target.Hash(),
		}, nil, nil,
	)
	require.NoError(t, err)
	t.Cleanup(subscription.CancelSubscription)

	const unrelatedCount = 64
	preimages := make([]lntypes.Preimage, 0, unrelatedCount+1)
	for i := 0; i < unrelatedCount; i++ {
		preimages = append(preimages, lntypes.Preimage{byte(i + 1)})
	}
	preimages = append(preimages, target)

	require.NoError(t, beacon.AddPreimages(preimages...))
	select {
	case actual := <-subscription.WitnessUpdates:
		require.Equal(t, target, actual)

	case <-time.After(5 * time.Second):
		t.Fatal("subscriber missed its persisted preimage")
	}
}

// TestWitnessBeaconCancellationClosesSubscriber verifies cancellation races
// neither notification nor subsequent durable cache writes.
func TestWitnessBeaconCancellationClosesSubscriber(t *testing.T) {
	t.Parallel()

	db := channeldb.OpenForTesting(t, t.TempDir())
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	beacon, err := NewWitnessBeacon(db)
	require.NoError(t, err)
	preimage := lntypes.Preimage{9, 8, 7}
	subscription, err := beacon.SubscribeUpdates(
		lnwire.ShortChannelID{}, &channeldb.HTLC{
			RHash: preimage.Hash(),
		}, nil, nil,
	)
	require.NoError(t, err)

	start := make(chan struct{})
	added := make(chan error, 1)
	go func() {
		<-start
		added <- beacon.AddPreimages(preimage)
	}()
	close(start)
	subscription.CancelSubscription()
	for range subscription.WitnessUpdates {
	}
	require.NoError(t, <-added)

	stored, ok := beacon.LookupPreimage(preimage.Hash())
	require.True(t, ok)
	require.Equal(t, preimage, stored)
}
