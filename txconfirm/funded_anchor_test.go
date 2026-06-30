package txconfirm

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/walletcore"
	"github.com/stretchr/testify/require"
)

// makeFundedAnchorTx builds a v2 transaction carrying a funded (non-zero) P2A
// anchor, modelling an independently-valid commitment-style parent that pays
// its own fee and reserves the anchor as a CPFP handle.
func makeFundedAnchorTx(seed byte, anchorValue int64) *wire.MsgTx {
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{seed},
			Index: 0,
		},
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    100_000,
		PkScript: []byte{txscript.OP_TRUE},
	})
	tx.AddTxOut(arkscript.AnchorOutput(arkscript.WithAnchorValue(anchorValue)))

	return tx
}

// TestSubmitFundedAnchorInitialBroadcastsDirect asserts that the initial
// submission of a funded-anchor parent goes out as a plain direct broadcast:
// the parent is independently valid, so no CPFP child is built and no fee
// input is reserved until a bump is actually requested.
func TestSubmitFundedAnchorInitialBroadcastsDirect(t *testing.T) {
	t.Parallel()

	chain := newFakeChainSourceRef(100)
	b := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource: chain,
		Wallet: &fakeWallet{
			utxos: []*walletcore.Utxo{makeWalletUTXO(t)},
		},
	})

	result, err := b.Submit(t.Context(), 100, &BroadcastRequest{
		Tx:        makeFundedAnchorTx(7, 330),
		Label:     "funded-initial",
		IsFeeBump: false,
	})
	require.NoError(t, err)

	// A direct broadcast leaves no CPFP child and submits no package.
	require.Nil(t, result.ChildTxid)
	require.Equal(t, 1, chain.broadcastCallCount())
	require.Equal(t, 0, chain.packageCallCount())
}

// TestSubmitFundedAnchorFeeBumpBuildsChild asserts that a fee-bump submission
// of a funded-anchor parent (the path a stuck-tx bump drives) builds and
// submits a CPFP child spending the funded anchor.
func TestSubmitFundedAnchorFeeBumpBuildsChild(t *testing.T) {
	t.Parallel()

	chain := newFakeChainSourceRef(100)
	b := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource: chain,
		Wallet: &fakeWallet{
			utxos: []*walletcore.Utxo{makeWalletUTXO(t)},
		},
	})

	result, err := b.Submit(t.Context(), 100, &BroadcastRequest{
		Tx:        makeFundedAnchorTx(8, 330),
		Label:     "funded-bump",
		IsFeeBump: true,
		ParentFee: 500,
	})
	require.NoError(t, err)

	// A fee bump submits a parent+child package and surfaces the child.
	require.NotNil(t, result.ChildTxid)
	require.Equal(t, 1, chain.packageCallCount())
}

// TestSubmitFundedAnchorV2NotRejected asserts that a v2 funded-anchor parent is
// accepted rather than rejected by the TRUC version gate, which only applies to
// zero-value ephemeral anchors.
func TestSubmitFundedAnchorV2NotRejected(t *testing.T) {
	t.Parallel()

	chain := newFakeChainSourceRef(100)
	b := NewCPFPBroadcaster(BroadcasterConfig{ChainSource: chain})

	tx := makeFundedAnchorTx(9, 330)
	require.EqualValues(t, 2, tx.Version)

	_, err := b.Submit(t.Context(), 100, &BroadcastRequest{
		Tx:        tx,
		Label:     "funded-v2",
		IsFeeBump: false,
	})
	require.NoError(t, err)
	require.NotErrorIs(t, err, ErrNonTRUCParent)
}

// TestChildFeeFromPackageFee asserts the package fee is split so the child pays
// only the shortfall the parent does not already cover, with a positive floor.
func TestChildFeeFromPackageFee(t *testing.T) {
	t.Parallel()

	// Parent pays nothing (ephemeral): the child pays the whole package.
	require.EqualValues(
		t, 1_000, childFeeFromPackageFee(1_000, 0),
	)

	// Funded parent: the child pays the package fee minus the parent's fee.
	require.EqualValues(
		t, 700, childFeeFromPackageFee(1_000, 300),
	)

	// Parent fee already covers (or exceeds) the package target: the child
	// still pays a positive floor of one satoshi.
	require.EqualValues(
		t, 1, childFeeFromPackageFee(1_000, 1_000),
	)
	require.EqualValues(
		t, 1, childFeeFromPackageFee(1_000, 5_000),
	)
}

// TestTargetOrEstimatedFeeRate asserts an operator-supplied target overrides
// the estimator and is clamped to the configured maximum, while a non-positive
// target defers to the estimator.
func TestTargetOrEstimatedFeeRate(t *testing.T) {
	t.Parallel()

	chain := newFakeChainSourceRef(100)
	chain.feeRate = 5
	b := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource:           chain,
		MaxFeeRateSatPerVByte: 50,
	})

	// A non-positive target defers to the estimator (5 sat/vB).
	rate, err := b.targetOrEstimatedFeeRate(t.Context(), 0)
	require.NoError(t, err)
	require.EqualValues(t, 5, rate)

	// A target within the cap is used verbatim.
	rate, err = b.targetOrEstimatedFeeRate(t.Context(), 25)
	require.NoError(t, err)
	require.EqualValues(t, 25, rate)

	// A target above the cap is clamped to the maximum.
	rate, err = b.targetOrEstimatedFeeRate(t.Context(), 999)
	require.NoError(t, err)
	require.EqualValues(t, 50, rate)
}
