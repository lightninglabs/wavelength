package unroll

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/vtxo"
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
	registry := NewUnrollRegistryActor(RegistryConfig{
		DeliveryStore:        newMemCheckpointStore(),
		LegacyProofScanFloor: 1,
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
		t, uint32(1), firstChild.behavior.cfg.LegacyProofScanFloor,
	)
	require.Equal(
		t, uint32(1), secondChild.behavior.cfg.LegacyProofScanFloor,
	)
	// Drive both spawned behaviors through the real warning site. The
	// registry-wide deduper must allow only the first child to warn.
	var buf bytes.Buffer
	logger := btclog.NewSLogger(btclog.NewDefaultHandler(&buf))
	logger.SetLevel(btclog.LevelInfo)
	for _, child := range []*VTXOUnrollActor{firstChild, secondChild} {
		child.behavior.desc = &vtxo.Descriptor{
			CreatedHeight: 850_000,
			Ancestry: []vtxo.Ancestry{{
				CommitmentHeight: 0,
			}},
		}
		child.behavior.pending = &actorCheckpoint{Height: 850_100}
		child.behavior.log = logger
		proofTxid := chainhash.Hash{
			byte(child.behavior.cfg.TargetOutpoint.Index),
		}
		child.behavior.proofNodeConfHeightHint(
			t.Context(), proofTxid,
		)
	}

	require.Equal(
		t, 1,
		bytes.Count(
			buf.Bytes(), []byte(
				"Legacy proof commitment height "+
					"unavailable; using safe fallback "+
					"floor",
			),
		),
	)
	require.Contains(t, buf.String(), "[WRN]")
}
