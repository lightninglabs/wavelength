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

	// Exercise the production registry constructor and spawn seam. Two real
	// children must receive the same registry-lifetime deduper.
	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	registry := NewUnrollRegistryActor(RegistryConfig{
		DeliveryStore:              newMemCheckpointStore(),
		LegacyProofScanFloor:       123,
		LegacyProofScanOperatorKey: operatorPriv.PubKey(),
	})
	t.Cleanup(registry.Stop)
	require.NotNil(t, registry.behavior.proofNodeFloorAlerts)

	firstChild, err := registry.behavior.spawn(
		t.Context(), wire.OutPoint{
			Index: 1,
		},
	)
	require.NoError(t, err)
	t.Cleanup(firstChild.Stop)
	secondChild, err := registry.behavior.spawn(
		t.Context(), wire.OutPoint{
			Index: 2,
		},
	)
	require.NoError(t, err)
	t.Cleanup(secondChild.Stop)

	require.Same(
		t, registry.behavior.proofNodeFloorAlerts,
		firstChild.behavior.cfg.proofNodeFloorAlerts,
	)
	require.Same(
		t, registry.behavior.proofNodeFloorAlerts,
		secondChild.behavior.cfg.proofNodeFloorAlerts,
	)
	require.Equal(
		t, uint32(123), firstChild.behavior.cfg.LegacyProofScanFloor,
	)
	require.Equal(
		t, uint32(123), secondChild.behavior.cfg.LegacyProofScanFloor,
	)
	require.True(
		t, operatorPriv.PubKey().IsEqual(
			firstChild.behavior.cfg.LegacyProofScanOperatorKey,
		),
	)
	require.True(
		t, operatorPriv.PubKey().IsEqual(
			secondChild.behavior.cfg.LegacyProofScanOperatorKey,
		),
	)
}
