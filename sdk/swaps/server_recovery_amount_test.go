package swaps

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/wavelength/swaprpc"
	"github.com/stretchr/testify/require"
)

// TestRestoredReceiveRequiresAdvertisedAmount keeps a rejected underfunded
// claim from becoming acceptable after seed restore, including padded credit.
func TestRestoredReceiveRequiresAdvertisedAmount(t *testing.T) {
	t.Parallel()
	direction := swaprpc.
		RecoverableSwapDirection_RECOVERABLE_SWAP_DIRECTION_OUT
	for _, test := range []struct {
		name       string
		advertised uint64
		funded     int64
		want       uint32
	}{
		{
			"exact",
			42_000,
			42_000,
			1,
		},
		{
			"underfunded",
			42_000,
			41_999,
			0,
		},
		{
			"overfunded",
			42_000,
			42_001,
			0,
		},
		{
			"missing amount",
			0,
			42_000,
			0,
		},
		{
			"overflow amount",
			^uint64(0),
			42_000,
			0,
		},
		{
			"attached credit exact",
			43_000,
			43_000,
			1,
		},
		{
			"attached credit missing",
			43_000,
			42_000,
			0,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, daemon, server, inRow := newRefundRecoveryFixture(
				t,
			)
			preimage, params, err := newOutSwapPreimage(
				t.Context(), daemon,
			)
			require.NoError(t, err)
			remote, err := btcec.ParsePubKey(
				inRow.GetReceiverPubkey(),
			)
			require.NoError(t, err)
			row := recoverableSwapForTest(
				t, direction, preimage.Hash(), remote,
				daemon.identityKey, remote, params,
			)
			row.AmountSat = test.advertised
			server.recoverableRows = []*swaprpc.RecoverableSwap{row}
			daemon.liveLookupHook = func(int) (*VTXOInfo, error) {
				return &VTXOInfo{
					Outpoint:  "amount:0",
					AmountSat: test.funded,
				}, nil
			}
			result, err := c.RecoverSwapserverVHTLCs(
				t.Context(),
			)
			require.NoError(t, err)
			require.Equal(t, test.want, result.RecoveredVHTLCClaims)
			require.EqualValues(
				t, test.want, daemon.armRecoveryCalls,
			)
			require.Zero(t, daemon.sendCustomCalls)
			if test.want == 0 {
				_, err := c.GetSwapSummary(
					t.Context(), preimage.Hash(),
				)
				require.ErrorIs(t, err, ErrSwapSummaryNotFound)
			}
		})
	}
}
