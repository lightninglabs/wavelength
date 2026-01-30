package chainsource

import (
	"errors"
	"testing"

	"github.com/btcsuite/btcwallet/chain"
	"github.com/stretchr/testify/require"
)

// TestIsIgnorableBroadcastError tests error classification for expected
// re-broadcast failures.
func TestIsIgnorableBroadcastError(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil",
			err:  nil,
			want: false,
		},
		{
			name: "insufficient fee sentinel",
			err:  chain.ErrInsufficientFee,
			want: true,
		},
		{
			name: "same non-witness data sentinel",
			err:  chain.ErrSameNonWitnessData,
			want: true,
		},
		{
			name: "tx already confirmed sentinel",
			err:  chain.ErrTxAlreadyConfirmed,
			want: true,
		},
		{
			name: "tx already known sentinel",
			err:  chain.ErrTxAlreadyKnown,
			want: true,
		},
		{
			name: "bitcoind already in mempool string",
			err: errors.New(
				"sendrawtransaction: txn-already-in-mempool",
			),
			want: true,
		},
		{
			name: "unknown error",
			err:  errors.New("some other error"),
			want: false,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want,
				IsIgnorableBroadcastError(tc.err),
			)
		})
	}
}

// TestIsIgnorableMempoolRejectReason tests reject reason classification for
// testmempoolaccept responses.
func TestIsIgnorableMempoolRejectReason(t *testing.T) {
	t.Parallel()

	require.False(t, IsIgnorableMempoolRejectReason(""))
	require.True(t, IsIgnorableMempoolRejectReason(
		chain.ErrTxAlreadyKnown.Error(),
	))
	require.True(t, IsIgnorableMempoolRejectReason(
		chain.ErrTxAlreadyConfirmed.Error(),
	))
	require.True(t, IsIgnorableMempoolRejectReason("txn already known"))
	require.True(t, IsIgnorableMempoolRejectReason("already in mempool"))
	require.False(t, IsIgnorableMempoolRejectReason("missing inputs"))
}
