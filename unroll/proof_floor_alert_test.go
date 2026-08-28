package unroll

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// TestProofNodeFloorAlertDeduper verifies that targets sharing one proof
// transaction produce one alert while an independent proof still alerts.
func TestProofNodeFloorAlertDeduper(t *testing.T) {
	t.Parallel()

	deduper := newProofNodeFloorAlertDeduper()
	firstProof := chainhash.Hash{1}
	secondProof := chainhash.Hash{2}

	require.True(t, deduper.first(firstProof))
	require.False(t, deduper.first(firstProof))
	require.True(t, deduper.first(secondProof))

	// Concurrent children must not both claim the first warning.
	concurrent := newProofNodeFloorAlertDeduper()
	var firstCount atomic.Int32
	var workers sync.WaitGroup
	for range 32 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if concurrent.first(firstProof) {
				firstCount.Add(1)
			}
		}()
	}
	workers.Wait()
	require.Equal(t, int32(1), firstCount.Load())

	// Exercise the production registry constructor and spawn seam. Two real
	// children must receive the same registry-lifetime deduper.
	registry := NewUnrollRegistryActor(RegistryConfig{
		DeliveryStore: newMemCheckpointStore(),
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
}
