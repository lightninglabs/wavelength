package unroll

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/lib/recovery"
	"github.com/lightninglabs/wavelength/txconfirm"
	"github.com/stretchr/testify/require"
)

// TestWalletExitQuoteSharedAncestry checks that two outputs of the same
// target transaction share all proof packages but each still needs a sweep.
func TestWalletExitQuoteSharedAncestry(t *testing.T) {
	t.Parallel()
	first := buildSiblingOutputProof(t)
	op := first.TargetOutpoint()
	op.Index = 0
	var nodes []*recovery.Node
	for _, layer := range first.Layers() {
		for _, id := range layer {
			node, _ := first.Node(id)
			nodes = append(nodes, node)
		}
	}
	second, err := recovery.NewProof(op, first.CSVDelay(), nodes...)
	require.NoError(t, err)
	targets := []WalletExitTarget{
		{
			Proof:  first,
			Expiry: 500,
		},
		{
			Proof:  second,
			Expiry: 400,
		},
	}
	one := []btcutil.Amount{100_000}
	quote, err := QuoteWalletExit(targets, one, 5)
	require.NoError(t, err)
	require.EqualValues(t, 6, quote.Transactions)
	require.True(t, quote.ReuseChange)
	require.Zero(t, quote.Deposit)
	var packages btcutil.Amount
	for _, node := range nodes {
		vbytes := (txconfirm.EstimateWeight(node.Tx) + 3) / 4
		packages += btcutil.Amount(vbytes+defaultCPFPChildVBytes) * 5
	}
	require.Equal(t, packages, quote.WalletFee)
	require.EqualValues(
		t, packages+2*5*estimatedSweepVBytes, quote.NetworkFee,
	)
	require.Equal(t, []btcutil.Amount{100_000}, one)
}

// TestWalletExitCoinProjection covers a single large output, distinct
// confirmed inputs, ineligible small coins, and the dust boundary.
func TestWalletExitCoinProjection(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		coins   []btcutil.Amount
		fees    []btcutil.Amount
		deposit btcutil.Amount
		reuse   bool
	}{
		{
			"parallel",
			[]btcutil.Amount{
				10000,
				10000,
			},
			[]btcutil.Amount{
				3000,
				3000,
			},
			0,
			false,
		},
		{
			"serial",
			[]btcutil.Amount{
				10000,
			},
			[]btcutil.Amount{
				3000,
				3000,
			},
			0,
			true,
		},
		{
			"exact change",
			[]btcutil.Amount{
				10000,
			},
			[]btcutil.Amount{
				5000,
				5000 - txconfirm.DustLimit,
			},
			0,
			true,
		},
		{
			"fee coins below admission floor",
			[]btcutil.Amount{
				8000,
				8000,
			},
			[]btcutil.Amount{
				6000,
				6000,
			},
			12000 + txconfirm.DustLimit,
			false,
		},
		{
			"insufficient single input",
			[]btcutil.Amount{
				10000,
			},
			[]btcutil.Amount{
				13000,
			},
			13000 + txconfirm.DustLimit,
			false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			deposit, reuse := projectExitFeeCoins(
				test.coins, test.fees,
			)
			require.Equal(t, test.deposit, deposit)
			require.Equal(t, test.reuse, reuse)
		})
	}
}
