package vtxo

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestLegacyHeightRepairRetriesBeforeExpiry proves a failed repair preserves
// the actor metadata, the next epoch can retry, and a successful repair stops
// further lookups.
func TestLegacyHeightRepairRetriesBeforeExpiry(t *testing.T) {
	t.Parallel()

	desc := &Descriptor{CreatedHeight: 1001, BatchExpiry: 1000}
	failure := errors.New("write unavailable")
	calls := 0
	a := NewVTXOActor(t.Context(), &VTXOActorConfig{
		VTXO: desc,
		RepairCreatedHeight: func(ctx context.Context,
			got *Descriptor) (int32, error) {

			calls++
			require.NotSame(t, desc, got)
			require.Equal(t, desc.Outpoint, got.Outpoint)
			deadline, ok := ctx.Deadline()
			require.True(t, ok)
			require.LessOrEqual(
				t, time.Until(deadline),
				createdHeightRepairTimeout,
			)

			return 700, failure
		},
	})
	a.repairLegacyCreatedHeight(t.Context())
	require.Equal(t, int32(1001), a.cfg.VTXO.CreatedHeight)
	failure = nil
	a.repairLegacyCreatedHeight(t.Context())
	require.Equal(t, int32(700), a.cfg.VTXO.CreatedHeight)
	a.repairLegacyCreatedHeight(t.Context())
	require.Equal(t, 2, calls)
	require.Equal(t, int32(1001), desc.CreatedHeight)
}
