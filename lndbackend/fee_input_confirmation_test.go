package lndbackend

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/stretchr/testify/require"
)

// confirmationWallet keeps the wallet's original block even after a reorg.
type confirmationWallet struct {
	lndclient.WalletKitClient
	transaction lndclient.Transaction
	err         error
}

func (w *confirmationWallet) GetTransaction(context.Context, chainhash.Hash) (
	lndclient.Transaction, error) {

	return w.transaction, w.err
}

type confirmationChain struct {
	lndclient.ChainKitClient
	hash   chainhash.Hash
	err    error
	height int64
}

func (c *confirmationChain) GetBlockHash(_ context.Context, height int64) (
	chainhash.Hash, error) {

	c.height = height

	return c.hash, c.err
}

// TestFeeInputConfirmationReorg proves stale wallet confirmations do not
// authorize a fee input and that a later real confirmation restores it.
func TestFeeInputConfirmationReorg(t *testing.T) {
	t.Parallel()
	oldBlock := chainhash.Hash{1}
	activeBlock := chainhash.Hash{2}
	w := &confirmationWallet{transaction: lndclient.Transaction{
		Tx: wire.NewMsgTx(3), BlockHeight: 100,
		BlockHash: oldBlock.String(), Confirmations: 1_000,
	}}
	c := &confirmationChain{hash: activeBlock}
	b := NewBoardingBackend(w, c)
	op := wire.OutPoint{Hash: chainhash.Hash{3}}

	confirmed, err := b.FeeInputConfirmed(t.Context(), op)
	require.NoError(t, err)
	require.False(t, confirmed, "wallet confirmation count is stale")
	require.Equal(t, int64(100), c.height)

	// Re-mining the same transaction is accepted without restarting or
	// clearing a blacklist. Only the active confirmation binding changes.
	w.transaction.BlockHash = activeBlock.String()
	confirmed, err = b.FeeInputConfirmed(t.Context(), op)
	require.NoError(t, err)
	require.True(t, confirmed)

	c.err = context.DeadlineExceeded
	confirmed, err = b.FeeInputConfirmed(t.Context(), op)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.False(t, confirmed)
	c.err = nil

	w.transaction.BlockHeight = 0
	w.transaction.BlockHash = ""
	confirmed, err = b.FeeInputConfirmed(t.Context(), op)
	require.NoError(t, err)
	require.False(t, confirmed)

	w.err = context.Canceled
	confirmed, err = b.FeeInputConfirmed(t.Context(), op)
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, confirmed)
}
