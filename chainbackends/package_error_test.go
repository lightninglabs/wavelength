package chainbackends

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/stretchr/testify/require"
)

// TestPackageTxErrorReplacementFeeConstraints verifies that Core replacement
// diagnostics become structured fee floors while unrelated errors remain
// unclassified.
func TestPackageTxErrorReplacementFeeConstraints(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		reason  string
		fee     *btcutil.Amount
		deficit *btcutil.Amount
		feeRate *int64
	}{
		{
			name: "conflicting total fee",
			reason: "insufficient fee, rejecting replacement tx, " +
				"less fees than conflicting txs; " +
				"0.00001 < 0.0001",
			fee: amountPtr(10_000),
		},
		{
			name: "incremental relay deficit",
			reason: "insufficient fee, rejecting replacement tx, " +
				"not enough additional fees to relay; " +
				"0.00000001 < 0.00000016",
			deficit: amountPtr(15),
		},
		{
			name: "conflicting feerate",
			reason: "insufficient fee, rejecting replacement tx; " +
				"new feerate 0.00004 BTC/kvB <= old feerate " +
				"0.00207 BTC/kvB",
			feeRate: int64Ptr(207),
		},
		{
			name:   "unrelated rejection",
			reason: "bad-txns-inputs-missingorspent",
		},
		{
			name: "malformed replacement amount",
			reason: "less fees than conflicting txs; broken < " +
				"also-broken",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := NewPackageTxError(
				"wtxid", chainhash.Hash{1}, test.reason,
			)
			got := err.ReplacementConstraints()
			if test.fee == nil && test.deficit == nil &&
				test.feeRate == nil {

				require.Nil(t, got)

				return
			}

			require.NotNil(t, got)
			require.Equal(t, test.fee, got.ConflictingFee)
			require.Equal(
				t, test.deficit, got.AdditionalFeeDeficit,
			)
			require.Equal(
				t, test.feeRate,
				got.ConflictingFeeRateSatPerVByte,
			)
		})
	}
}

// amountPtr returns a pointer to a test amount.
func amountPtr(value btcutil.Amount) *btcutil.Amount {
	return &value
}

// int64Ptr returns a pointer to a test integer.
func int64Ptr(value int64) *int64 {
	return &value
}
