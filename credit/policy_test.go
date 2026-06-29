package credit

import (
	"testing"

	"github.com/lightninglabs/darepo-client/db"
	"github.com/stretchr/testify/require"
)

// TestRedeemDecision asserts the auto-redeem interlock: redeem only when the
// available balance clears the threshold and no credit-consuming pay or
// in-flight redemption is pending; a pending receive never blocks.
func TestRedeemDecision(t *testing.T) {
	t.Parallel()

	pay := db.CreditOperationRecord{Kind: db.CreditOpKindPay}
	recv := db.CreditOperationRecord{Kind: db.CreditOpKindReceive}
	redeem := db.CreditOperationRecord{Kind: db.CreditOpKindRedeem}

	cases := []struct {
		name      string
		available uint64
		threshold uint64
		ops       []db.CreditOperationRecord
		wantAmt   uint64
		wantOK    bool
	}{
		{
			name:      "above threshold no ops",
			available: 1000,
			threshold: 354,
			wantAmt:   1000,
			wantOK:    true,
		},
		{
			name:      "at threshold",
			available: 354,
			threshold: 354,
			wantOK:    false,
		},
		{
			name:      "below threshold",
			available: 100,
			threshold: 354,
			wantOK:    false,
		},
		{
			name:      "pending pay blocks",
			available: 1000,
			threshold: 354,
			ops: []db.CreditOperationRecord{
				pay,
			},
			wantOK: false,
		},
		{
			name:      "pending redeem blocks",
			available: 1000,
			threshold: 354,
			ops: []db.CreditOperationRecord{
				redeem,
			},
			wantOK: false,
		},
		{
			name:      "pending receive does not block",
			available: 1000,
			threshold: 354,
			ops: []db.CreditOperationRecord{
				recv,
			},
			wantAmt: 1000,
			wantOK:  true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			amt, ok := redeemDecision(
				tc.available, tc.threshold, tc.ops,
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
