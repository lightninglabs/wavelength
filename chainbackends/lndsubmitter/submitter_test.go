package lndsubmitter

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// fakeWalletKit is a walletKitSubmitter that records its inputs and returns a
// canned result.
type fakeWalletKit struct {
	gotTxns    []*wire.MsgTx
	gotMaxRate *chainfee.SatPerVByte
	result     *lndclient.SubmitPackageResult
	err        error
}

// SubmitPackage records the call and returns the canned result/err.
func (f *fakeWalletKit) SubmitPackage(_ context.Context, txns []*wire.MsgTx,
	maxFeeRate *chainfee.SatPerVByte) (*lndclient.SubmitPackageResult,
	error) {

	f.gotTxns = txns
	f.gotMaxRate = maxFeeRate

	return f.result, f.err
}

// TestSubmitPackageAssemblesAndMaps verifies that the submitter assembles the
// package parents-first/child-last, forwards a nil fee rate untouched, and maps
// the lndclient-native result into the btcjson result (including the per-tx
// error mapping).
func TestSubmitPackageAssemblesAndMaps(t *testing.T) {
	t.Parallel()

	parent := wire.NewMsgTx(3)
	child := wire.NewMsgTx(3)
	childHash := child.TxHash()

	fake := &fakeWalletKit{
		result: &lndclient.SubmitPackageResult{
			PackageMsg: "success",
			ReplacedTransactions: []chainhash.Hash{
				{
					0x01,
				},
			},
			TxResults: map[string]lndclient.SubmitPackageTxResult{
				"wtxid-ok": {
					Txid: childHash,
					Err:  "",
				},
				"wtxid-bad": {
					Txid: chainhash.Hash{
						0x02,
					},
					Err: "too-low-fee",
				},
			},
		},
	}

	s := New(fake)
	res, err := s.SubmitPackage(
		context.Background(), []*wire.MsgTx{parent}, child, nil,
	)
	require.NoError(t, err)

	// Package assembled parents-first, child last.
	require.Len(t, fake.gotTxns, 2)
	require.Equal(t, parent, fake.gotTxns[0])
	require.Equal(t, child, fake.gotTxns[1])

	// Nil fee rate forwarded as nil (node default).
	require.Nil(t, fake.gotMaxRate)

	// Package-level fields mapped through.
	require.Equal(t, "success", res.PackageMsg)
	require.Equal(
		t, fake.result.ReplacedTransactions, res.ReplacedTransactions,
	)

	// Accepted tx: no error surfaced.
	require.Nil(t, res.TxResults["wtxid-ok"].Error)
	require.Equal(t, childHash, res.TxResults["wtxid-ok"].TxID)

	// Rejected tx: error surfaced as a non-nil pointer to the reason.
	require.NotNil(t, res.TxResults["wtxid-bad"].Error)
	require.Equal(t, "too-low-fee", *res.TxResults["wtxid-bad"].Error)
}

// TestSubmitPackageForwardsMaxFeeRate verifies a non-nil ceiling is converted
// from BTC/kvB to sat/vByte and forwarded to lnd, rounding to the nearest
// integer rather than truncating (which would lower the ceiling).
func TestSubmitPackageForwardsMaxFeeRate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		btcKvB float64
		want   chainfee.SatPerVByte
	}{
		{
			// 0.00012 BTC/kvB == 12 sat/vByte exactly.
			name:   "exact",
			btcKvB: 0.00012,
			want:   12,
		},
		{
			// 0.000125 BTC/kvB == 12.5 sat/vByte, which must round
			// up to 13 rather than truncate down to 12.
			name:   "rounds up",
			btcKvB: 0.000125,
			want:   13,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeWalletKit{
				result: &lndclient.SubmitPackageResult{
					PackageMsg: "success",
				},
			}

			rate := tc.btcKvB

			s := New(fake)
			_, err := s.SubmitPackage(
				context.Background(), nil, wire.NewMsgTx(3),
				&rate,
			)
			require.NoError(t, err)
			require.NotNil(t, fake.gotMaxRate)
			require.Equal(t, tc.want, *fake.gotMaxRate)
		})
	}
}

// TestSubmitPackageRejectsNilTxns verifies that a nil child or a nil parent is
// rejected with an error rather than causing a nil pointer panic.
func TestSubmitPackageRejectsNilTxns(t *testing.T) {
	t.Parallel()

	s := New(&fakeWalletKit{})

	// Nil child is rejected.
	_, err := s.SubmitPackage(context.Background(), nil, nil, nil)
	require.ErrorContains(t, err, "nil child transaction")

	// A nil parent is rejected.
	_, err = s.SubmitPackage(
		context.Background(), []*wire.MsgTx{nil}, wire.NewMsgTx(3), nil,
	)
	require.ErrorContains(t, err, "nil parent transaction")
}
