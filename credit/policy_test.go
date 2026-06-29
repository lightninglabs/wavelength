package credit

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/stretchr/testify/require"
)

// TestRedeemWatermarkCleared asserts the receive-driven auto-redeem watermark
// check: a settled receive triggers a redeem only when auto-redeem is enabled
// and the earmark-adjusted available balance strictly clears the threshold. The
// no-pending-pay/redeem interlock is applied separately by the registry.
func TestRedeemWatermarkCleared(t *testing.T) {
	t.Parallel()

	const threshold = 354

	earmarkOf := func(v uint64) *atomic.Pointer[EarmarkFunc] {
		var p atomic.Pointer[EarmarkFunc]
		var fn EarmarkFunc = func(context.Context) (uint64, error) {
			return v, nil
		}
		p.Store(&fn)

		return &p
	}
	earmarkErr := func() *atomic.Pointer[EarmarkFunc] {
		var p atomic.Pointer[EarmarkFunc]
		var fn EarmarkFunc = func(context.Context) (uint64, error) {
			return 0, context.DeadlineExceeded
		}
		p.Store(&fn)

		return &p
	}

	cases := []struct {
		name      string
		enabled   bool
		available uint64
		earmark   *atomic.Pointer[EarmarkFunc]
		wantAmt   uint64
		wantOK    bool
	}{
		{
			name:      "above threshold no earmark",
			enabled:   true,
			available: 1000,
			wantAmt:   1000,
			wantOK:    true,
		},
		{
			name:      "at threshold",
			enabled:   true,
			available: threshold,
			wantOK:    false,
		},
		{
			name:      "below threshold",
			enabled:   true,
			available: 100,
			wantOK:    false,
		},
		{
			name:      "disabled never redeems",
			enabled:   false,
			available: 1000,
			wantOK:    false,
		},
		{
			name:      "earmark drops below threshold",
			enabled:   true,
			available: 1000,
			earmark:   earmarkOf(800),
			wantOK:    false,
		},
		{
			name:      "earmark leaves headroom",
			enabled:   true,
			available: 1000,
			earmark:   earmarkOf(200),
			wantAmt:   800,
			wantOK:    true,
		},
		{
			name:      "earmark error fails safe",
			enabled:   true,
			available: 1000,
			earmark:   earmarkErr(),
			wantOK:    false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b := &opBehavior{
				cfg: OpActorConfig{
					OpID:              "op",
					AutoRedeemEnabled: tc.enabled,
					MinRedeemSat:      threshold,
					Daemon:            newFakeDaemon(),
					Earmark:           tc.earmark,
				},
				log: btclog.Disabled,
			}

			amt, ok := b.redeemWatermarkCleared(
				context.Background(), &CreditSnapshot{
					AvailableSat: tc.available,
				},
			)
			require.Equal(t, tc.wantOK, ok)
			require.Equal(t, tc.wantAmt, amt)
		})
	}
}

// TestRedeemOpKeyUnique asserts redeem op keys are prefixed and random.
func TestRedeemOpKeyUnique(t *testing.T) {
	t.Parallel()

	a, err := redeemOpKey()
	require.NoError(t, err)
	b, err := redeemOpKey()
	require.NoError(t, err)

	require.Contains(t, a, "redeem:")
	require.NotEqual(t, a, b)
}
