package txconfirm

import (
	"bytes"
	"context"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/walletcore"
	"github.com/stretchr/testify/require"
)

// faultSplitJournal injects a lost write receipt and a transient failure
// clearing the record after confirmation.
type faultSplitJournal struct {
	record          []byte
	loseSaveReceipt bool
	failClear       bool
}

// SaveWalletFeeSplit can commit the record but lose its response.
func (j *faultSplitJournal) SaveWalletFeeSplit(ctx context.Context,
	candidate []byte) (int64, error) {

	if j.record != nil && !bytes.Equal(candidate, j.record) {
		return 0, nil
	}
	j.record = bytes.Clone(candidate)
	if j.loseSaveReceipt {
		j.loseSaveReceipt = false

		return 0, context.DeadlineExceeded
	}

	return 1, nil
}

// ClearWalletFeeSplit can fail before deleting the record.
func (j *faultSplitJournal) ClearWalletFeeSplit(ctx context.Context,
	candidate []byte) error {

	if j.failClear {
		j.failClear = false

		return context.DeadlineExceeded
	}
	if bytes.Equal(candidate, j.record) {
		j.record = nil
	}

	return nil
}

// TestFeeSplitJournalRestart proves that a committed candidate survives lost
// receipts, new wallet supply, and a restart before confirmation.
func TestFeeSplitJournalRestart(t *testing.T) {
	t.Parallel()

	t.Run("split confirmed", func(t *testing.T) {
		testFeeSplitJournalResolution(t, false)
	})
	t.Run("competing spender confirmed", func(t *testing.T) {
		testFeeSplitJournalResolution(t, true)
	})
}

// testFeeSplitJournalResolution restores an ambiguous split and fails its
// first journal clear. Only the winning transaction may supply fee outputs.
func testFeeSplitJournalResolution(t *testing.T, competitor bool) {
	t.Helper()

	ctx := t.Context()
	journal := &faultSplitJournal{loseSaveReceipt: true}
	chain := newFakeChainSourceRef(100)
	wallet := &allocatingWallet{fakeWallet: &fakeWallet{
		utxos: []*walletcore.Utxo{
			makeWalletUTXOWithAmount(100_000, 7),
		},
	}}
	demands := []feeInputDemand{
		{
			parentTxid: chainhash.Hash{
				1,
			},
			minAmount: 10_000,
		},
		{
			parentTxid: chainhash.Hash{
				2,
			},
			minAmount: 10_000,
		},
	}
	first := newTestFeeBumpController(
		t,
		NewCPFPBroadcaster(
			BroadcasterConfig{
				ChainSource: chain,
				Wallet:      wallet,
			},
		),
	)
	first.env.journal = journal
	_, err := first.EnsureSupply(ctx, demands, 5, 100, 1)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Empty(t, chain.broadcastCalls)

	raw := bytes.Clone(journal.record)
	require.NotEmpty(t, raw)

	chain.broadcastErr = context.DeadlineExceeded
	_, err = first.EnsureSupply(ctx, demands, 5, 101, 1)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Len(t, chain.broadcastCalls, 1)
	signed := chain.broadcastCalls[0].Tx.Copy()

	// Start another actor with only the database and fresh wallet supply.
	wallet.utxos = []*walletcore.Utxo{makeWalletUTXOWithAmount(100_000, 8)}
	actor := NewTxBroadcasterActor(
		Config{
			ChainSource:     chain,
			Wallet:          wallet,
			FeeSplitJournal: journal,
		},
	)
	require.NoError(t, actor.RestoreFeeSplit(raw))
	actor.ensureFeeBumpStarted(ctx)
	t.Cleanup(actor.feeBumpFSM.Stop)
	restored := &testFeeBump{
		t:   t,
		fsm: actor.feeBumpFSM,
		env: actor.feeBumpEnv,
	}
	pending := restored.PendingFanout()
	require.Equal(t, first.PendingFanout().assignments, pending.assignments)
	require.Equal(t, int32(100), pending.heightHint)
	require.Equal(t, signed.TxHash(), pending.txid)
	chain.broadcastErr = nil
	_, err = restored.EnsureSupply(ctx, demands, 5, 102, 1)
	require.NoError(t, err)
	require.Len(t, chain.broadcastCalls, 2)
	var before, after bytes.Buffer
	require.NoError(t, signed.Serialize(&before))
	require.NoError(t, chain.broadcastCalls[1].Tx.Serialize(&after))
	require.Equal(t, before.Bytes(), after.Bytes())
	require.Len(t, wallet.fundedTemplates, 1)

	// A delayed callback from an older split cannot release this one.
	_, err = restored.fsm.AskEvent(ctx, &feeBumpFanoutResolved{
		txid: chainhash.Hash{8}, spender: chainhash.Hash{9},
	}).Await(ctx).Unpack()
	require.NoError(t, err)
	require.Equal(t, raw, journal.record)

	journal.failClear = true
	spender := signed.TxHash()
	if competitor {
		spender = chainhash.Hash{22}
	}
	_, err = restored.fsm.AskEvent(ctx, &feeBumpFanoutResolved{
		txid: signed.TxHash(), spender: spender,
	}).Await(ctx).Unpack()
	require.NoError(t, err)
	require.ErrorIs(t, restored.env.takeLastErr(), context.DeadlineExceeded)
	require.NotNil(t, restored.PendingFanout())
	_, err = restored.EnsureSupply(ctx, nil, 5, 103, 1)
	require.NoError(t, err)
	require.Nil(t, restored.PendingFanout())
	require.Len(t, chain.broadcastCalls, 2)
	require.Empty(t, journal.record)

	if competitor {
		// A real conflict invalidates every predicted output. The
		// next demand must be able to use the fresh wallet coin.
		for parent := range pending.assignments {
			state := restored.env.broadcaster.parentState(parent)
			require.Empty(t, state.UsedFeeInputs)
			require.Empty(t, state.PredictedFeeInputs)
		}
		replacement, err := restored.EnsureSupply(
			ctx, demands, 5, 104, 1,
		)
		require.NoError(t, err)
		require.NotNil(t, replacement)
		require.NotEqual(t, signed.TxHash(), replacement.txid)
	}
}
