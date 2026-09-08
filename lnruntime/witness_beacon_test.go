package lnruntime

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestWitnessBeaconDeliversBeyondSubscriberBuffer verifies a slow resolver
// receives every persisted preimage in order without blocking the producer.
func TestWitnessBeaconDeliversBeyondSubscriberBuffer(t *testing.T) {
	t.Parallel()

	db := channeldb.OpenForTesting(t, t.TempDir())
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	beacon, err := NewWitnessBeacon(db)
	require.NoError(t, err)
	subscription, err := beacon.SubscribeUpdates(
		lnwire.ShortChannelID{}, nil, nil, nil,
	)
	require.NoError(t, err)
	t.Cleanup(subscription.CancelSubscription)

	const count = 64
	preimages := make([]lntypes.Preimage, 0, count)
	for i := 0; i < count; i++ {
		preimages = append(preimages, lntypes.Preimage{byte(i + 1)})
	}

	added := make(chan error, 1)
	go func() {
		added <- beacon.AddPreimages(preimages...)
	}()
	select {
	case err := <-added:
		require.NoError(t, err)

	case <-time.After(5 * time.Second):
		t.Fatal("preimage persistence blocked on a slow subscriber")
	}

	for _, expected := range preimages {
		select {
		case actual := <-subscription.WitnessUpdates:
			require.Equal(t, expected, actual)

		case <-time.After(5 * time.Second):
			t.Fatal("subscriber missed a persisted preimage")
		}
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
	subscription, err := beacon.SubscribeUpdates(
		lnwire.ShortChannelID{}, nil, nil, nil,
	)
	require.NoError(t, err)

	preimage := lntypes.Preimage{9, 8, 7}
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
