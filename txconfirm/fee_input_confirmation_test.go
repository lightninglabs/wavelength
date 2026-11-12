package txconfirm

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/walletcore"
	"github.com/stretchr/testify/require"
)

// confirmingWallet models an LND listing that still offers an orphaned coin.
type confirmingWallet struct {
	*rewritingWallet
	confirmed map[wire.OutPoint]bool
	checked   []wire.OutPoint
	err       error
	errAt     int
}

func (w *confirmingWallet) FeeInputConfirmed(_ context.Context,
	op wire.OutPoint) (bool, error) {

	w.checked = append(w.checked, op)
	if w.err != nil && (w.errAt == 0 || len(w.checked) == w.errAt) {
		return false, w.err
	}

	return w.confirmed[op], nil
}

// TestCPFPRejectsOrphanedFeeInput exercises selection through signing and
// package submission: the stale smallest coin must never reach either step.
func TestCPFPRejectsOrphanedFeeInput(t *testing.T) {
	t.Parallel()
	stale := makeWalletUTXOWithAmount(32_000, 1)
	live := makeWalletUTXOWithAmount(60_000, 2)
	unused := makeWalletUTXOWithAmount(90_000, 3)
	script := p2trTestPkScript(t)
	for _, coin := range []*walletcore.Utxo{stale, live, unused} {
		coin.PkScript = script
	}
	w := &confirmingWallet{
		rewritingWallet: &rewritingWallet{
			utxos: []*walletcore.Utxo{
				stale,
				live,
				unused,
			},
			changeScript: script,
			rewrite: func(tx *wire.MsgTx) *wire.MsgTx {
				op := tx.TxIn[1].PreviousOutPoint
				require.Equal(t, live.Outpoint, op)

				return tx
			},
		},
		confirmed: map[wire.OutPoint]bool{
			live.Outpoint: true, unused.Outpoint: true,
		},
	}
	chain := newFakeChainSourceRef(100)
	b := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource: chain, Wallet: w,
	})
	parent := makeTestTx(true)
	result, err := b.Submit(t.Context(), 100, &BroadcastRequest{Tx: parent})
	require.NoError(t, err)
	require.NotNil(t, result.ChildTxid)
	require.Equal(t, 1, chain.packageCallCount())
	require.Equal(t, []wire.OutPoint{
		stale.Outpoint, live.Outpoint,
	}, w.checked)
	require.NotContains(
		t, b.parentStates[parent.TxHash()].UsedFeeInputs,
		stale.Outpoint,
	)
	require.Contains(
		t, b.parentStates[parent.TxHash()].UsedFeeInputs, live.Outpoint,
	)

	// A re-confirmed coin is eligible on the next selection; stale evidence
	// is scoped to one attempt rather than becoming a permanent blacklist.
	w.confirmed[stale.Outpoint] = true
	selected, err := b.selectFeeInput(t.Context(), chainhash.Hash{9}, 1_000)
	require.NoError(t, err)
	require.Equal(t, stale.Outpoint, selected.Outpoint)
}

// TestCPFPConfirmationUnknownDoesNotSign prevents a failed lookup from being
// treated as either successful validation or an authoritative stale coin.
func TestCPFPConfirmationUnknownDoesNotSign(t *testing.T) {
	t.Parallel()
	coin := makeWalletUTXOWithAmount(50_000, 1)
	w := &confirmingWallet{
		rewritingWallet: &rewritingWallet{
			utxos: []*walletcore.Utxo{
				coin,
			},
			changeScript: p2trTestPkScript(t),
			rewrite: func(tx *wire.MsgTx) *wire.MsgTx {
				t.Error("must not sign after failed check")

				return tx
			},
		},
		err: context.DeadlineExceeded,
	}
	chain := newFakeChainSourceRef(100)
	b := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource: chain, Wallet: w,
	})
	_, err := b.Submit(
		t.Context(), 100, &BroadcastRequest{
			Tx: makeTestTx(true),
		},
	)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotErrorIs(t, err, ErrCPFPFeeInputUnavailable)
	require.Zero(t, chain.packageCallCount())
	require.Empty(t, b.parentStates)
}

// TestCPFPReservedConfirmationReorg retires stale eligibility, keeps parent fee
// history, and preserves reservations when confirmation evidence is unknown.
func TestCPFPReservedConfirmationReorg(t *testing.T) {
	t.Parallel()
	coin := makeWalletUTXOWithAmount(50_000, 1)
	w := &confirmingWallet{
		rewritingWallet: &rewritingWallet{},
		confirmed:       make(map[wire.OutPoint]bool),
	}
	b := NewCPFPBroadcaster(BroadcasterConfig{Wallet: w})
	parent := chainhash.Hash{7}
	b.reserveFeeInput(t.Context(), parent, feeInputFromWalletUTXO(coin))
	b.parentStates[parent].LastPackageFee = 1_000
	b.parentStates[parent].LastFeeRate = 2
	_, err := b.selectFeeInput(t.Context(), parent, 1_000)
	require.ErrorIs(t, err, errCPFPFeeInputShortfall)
	require.NotContains(
		t, b.parentStates[parent].UsedFeeInputs, coin.Outpoint,
	)
	require.EqualValues(t, 1_000, b.parentStates[parent].LastPackageFee)

	b.reserveFeeInput(t.Context(), parent, feeInputFromWalletUTXO(coin))
	w.err = context.DeadlineExceeded
	_, err = b.selectFeeInput(t.Context(), parent, 1_000)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Contains(t, b.parentStates[parent].UsedFeeInputs, coin.Outpoint)

	w.err = nil
	w.confirmed[coin.Outpoint] = true
	selected, err := b.selectFeeInput(t.Context(), parent, 1_000)
	require.NoError(t, err)
	require.Equal(t, coin.Outpoint, selected.Outpoint)
}

// TestCPFPStaleReservedInputReselects covers an accepted child's fee coin
// remaining orphaned indefinitely while another confirmed coin can pay.
func TestCPFPStaleReservedInputReselects(t *testing.T) {
	t.Parallel()
	old := makeWalletUTXOWithAmount(20_000, 1)
	live := makeWalletUTXOWithAmount(60_000, 2)
	script := p2trTestPkScript(t)
	old.PkScript, live.PkScript = script, script
	expected := old.Outpoint
	w := &confirmingWallet{
		rewritingWallet: &rewritingWallet{
			utxos: []*walletcore.Utxo{
				old,
			}, changeScript: script,
			rewrite: func(tx *wire.MsgTx) *wire.MsgTx {
				require.Equal(
					t, expected,
					tx.TxIn[1].PreviousOutPoint,
				)

				return tx
			},
		},
		confirmed: map[wire.OutPoint]bool{
			old.Outpoint:  true,
			live.Outpoint: true,
		},
	}
	chain := newFakeChainSourceRef(100)
	b := NewCPFPBroadcaster(
		BroadcasterConfig{
			ChainSource: chain,
			Wallet:      w,
		},
	)
	parent := makeTestTx(true)
	req := &BroadcastRequest{Tx: parent}
	_, err := b.Submit(t.Context(), 100, req)
	require.NoError(t, err)

	// The prior child cannot remain on this branch after its confirmed
	// funding is orphaned. Its old wallet record never updates in this
	// test.
	w.confirmed[old.Outpoint] = false
	w.utxos = []*walletcore.Utxo{live}
	expected = live.Outpoint
	for height := int32(101); height <= 102; height++ {
		_, err = b.Submit(t.Context(), height, req)
		require.NoError(t, err)
	}
	require.Equal(t, 3, chain.packageCallCount())
	require.False(t, w.confirmed[old.Outpoint])
	// Retire stale eligibility without discarding the parent's accepted
	// fee history or the healthy replacement reservation.
	require.NotContains(
		t, b.parentStates[parent.TxHash()].UsedFeeInputs, old.Outpoint,
	)
	require.Positive(t, b.parentStates[parent.TxHash()].LastPackageFee)
	require.Contains(
		t, b.parentStates[parent.TxHash()].UsedFeeInputs, live.Outpoint,
	)
}

// TestCPFPConfirmationUnknownDuringReselect keeps a mixed-script fee top-up
// from broadcasting a direct parent or starting fanout after a failed check.
func TestCPFPConfirmationUnknownDuringReselect(t *testing.T) {
	t.Parallel()
	coin := makeWalletUTXOWithAmount(50_000, 1)
	w := &confirmingWallet{
		rewritingWallet: &rewritingWallet{
			utxos: []*walletcore.Utxo{
				coin,
			}, changeScript: p2trTestPkScript(t),
			rewrite: func(tx *wire.MsgTx) *wire.MsgTx {
				t.Error("must not sign after failed check")

				return tx
			},
		},
		confirmed: map[wire.OutPoint]bool{
			coin.Outpoint: true,
		},
		err: context.DeadlineExceeded, errAt: 2,
	}
	chain := newFakeChainSourceRef(100)
	b := NewCPFPBroadcaster(
		BroadcasterConfig{
			ChainSource: chain,
			Wallet:      w,
		},
	)
	_, err := b.Submit(
		t.Context(), 100, &BroadcastRequest{
			Tx: makeTestTx(true),
		},
	)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotErrorIs(t, err, ErrCPFPFeeInputUnavailable)
	require.Zero(t, chain.packageCallCount())
	require.Zero(t, chain.broadcastCallCount())
	require.Empty(t, b.parentStates)
}
