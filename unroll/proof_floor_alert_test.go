package unroll

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// TestProofNodeFloorAlertDeduper verifies that one registry produces one
// fallback warning even when many child actors reach it concurrently.
func TestProofNodeFloorAlertDeduper(t *testing.T) {
	t.Parallel()

	deduper := newProofNodeFloorAlertDeduper()
	require.True(t, deduper.first())
	require.False(t, deduper.first())

	// Concurrent children must not both claim the first warning.
	concurrent := newProofNodeFloorAlertDeduper()
	var firstCount atomic.Int32
	var workers sync.WaitGroup
	for range 32 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if concurrent.first() {
				firstCount.Add(1)
			}
		}()
	}
	workers.Wait()
	require.Equal(t, int32(1), firstCount.Load())

	// Exercise the production registry constructor and child configuration.
	// Two children must receive the same registry-lifetime deduper.
	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	registry := NewUnrollRegistryActor(RegistryConfig{
		DeliveryStore:              newMemCheckpointStore(),
		LegacyProofScanFloor:       123,
		LegacyProofScanOperatorKey: operatorPriv.PubKey(),
	})
	t.Cleanup(registry.Stop)
	require.NotNil(t, registry.behavior.proofNodeFloorAlerts)

	firstChild := registry.behavior.childConfig(wire.OutPoint{Index: 1})
	secondChild := registry.behavior.childConfig(wire.OutPoint{Index: 2})

	require.Same(
		t, registry.behavior.proofNodeFloorAlerts,
		firstChild.proofNodeFloorAlerts,
	)
	require.Same(
		t, registry.behavior.proofNodeFloorAlerts,
		secondChild.proofNodeFloorAlerts,
	)
	require.Equal(
		t, uint32(123), firstChild.LegacyProofScanFloor,
	)
	require.Equal(
		t, uint32(123), secondChild.LegacyProofScanFloor,
	)
	require.True(
		t, operatorPriv.PubKey().IsEqual(
			firstChild.LegacyProofScanOperatorKey,
		),
	)
	require.True(
		t, operatorPriv.PubKey().IsEqual(
			secondChild.LegacyProofScanOperatorKey,
		),
	)
}
