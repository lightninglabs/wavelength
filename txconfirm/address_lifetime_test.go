package txconfirm

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/walletcore"
	"github.com/stretchr/testify/require"
)

// allocatingWallet records address issuance while preserving the existing
// wallet double's funding, signing, and lease behavior. Every successful call
// returns a different P2TR script, exposing accidental per-attempt allocation.
type allocatingWallet struct {
	*fakeWallet
	scriptCalls     int
	scriptErr       error
	finalizeErr     error
	fundedTemplates []*wire.MsgTx
}

// NewWalletPkScript counts even failed allocation requests. Failed requests
// provide no script to cache; successful requests produce distinct scripts.
func (w *allocatingWallet) NewWalletPkScript(_ context.Context) ([]byte,
	error) {

	w.scriptCalls++
	if w.scriptErr != nil {
		return nil, w.scriptErr
	}

	script := append([]byte{0x51, 0x20}, make([]byte, 32)...)
	script[len(script)-1] = byte(w.scriptCalls)

	return script, nil
}

// FundPsbt records the unfunded output plan before the embedded wallet can
// reject funding, exposing allocation churn even when no transaction exists.
func (w *allocatingWallet) FundPsbt(ctx context.Context, packetBytes []byte,
	feeRate int64, lockID walletcore.LockID, expiry time.Duration) (
	*wire.MsgTx, error) {

	packet, err := psbt.NewFromRawBytes(bytes.NewReader(packetBytes), false)
	if err != nil {
		return nil, err
	}
	w.fundedTemplates = append(w.fundedTemplates, packet.UnsignedTx.Copy())

	return w.fakeWallet.FundPsbt(ctx, packetBytes, feeRate, lockID, expiry)
}

// FinalizePsbt can fail after a reserved-input fanout has allocated its
// destinations, so retries must retain the plan even without a signed tx.
func (w *allocatingWallet) FinalizePsbt(ctx context.Context,
	packetBytes []byte) (*wire.MsgTx, error) {

	if w.finalizeErr != nil {
		return nil, w.finalizeErr
	}

	return w.fakeWallet.FinalizePsbt(ctx, packetBytes)
}

// TestCPFPAddressLifetime drives shortages, accepted replacements and fee-cap
// refusals through Submit. The destination belongs to the parent, not a fee
// input reservation or successful package, and is discarded only on eviction.
func TestCPFPAddressLifetime(t *testing.T) {
	t.Parallel()

	chain := newFakeChainSourceRef(100)
	wallet := &allocatingWallet{fakeWallet: &fakeWallet{}}
	b := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource:           chain,
		Wallet:                wallet,
		MaxFeeRateSatPerVByte: 6,
	})
	parent := makeTestTx(true)
	req := &BroadcastRequest{Tx: parent}
	for range 20 {
		_, err := b.Submit(t.Context(), 100, req)
		require.ErrorIs(t, err, ErrCPFPFeeInputUnavailable)
	}
	require.Equal(t, 1, wallet.scriptCalls)
	require.Empty(t, b.parentStates, "allocation is not fee history")

	wallet.utxos = []*walletcore.Utxo{makeWalletUTXO(t)}
	first, err := b.Submit(t.Context(), 102, req)
	require.NoError(t, err)
	require.EqualValues(t, 5, first.FeeRate)
	second, err := b.Submit(t.Context(), 104, req)
	require.NoError(t, err)
	require.EqualValues(t, 6, second.FeeRate)
	for range 20 {
		_, err := b.Submit(t.Context(), 106, req)
		require.ErrorIs(t, err, ErrFeeRateCapReached)
	}
	require.Equal(t, 1, wallet.scriptCalls)
	require.Equal(t, 2, chain.packageCallCount())
	require.Equal(
		t, chain.packageCalls[0].Child.TxOut[0].PkScript,
		chain.packageCalls[1].Child.TxOut[0].PkScript,
	)

	b.Evict(t.Context(), parent.TxHash())
	require.Empty(t, b.changeScripts)
	_, err = b.Submit(t.Context(), 108, req)
	require.NoError(t, err)
	require.Equal(t, 2, wallet.scriptCalls)
}

// TestCPFPRejectedPackageKeepsAddress proves fee-input cleanup on a rejected
// package does not remove its parent's cached destination or fabricate prior
// fee history. A later successful retry uses the original output script.
func TestCPFPRejectedPackageKeepsAddress(t *testing.T) {
	t.Parallel()

	chain := newFakeChainSourceRef(100)
	chain.packageErr = fmt.Errorf("mempool temporarily full")
	wallet := &allocatingWallet{fakeWallet: &fakeWallet{
		utxos: []*walletcore.Utxo{
			makeWalletUTXO(t),
		},
	}}
	b := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource: chain,
		Wallet:      wallet,
	})
	req := &BroadcastRequest{Tx: makeTestTx(true)}
	for range 3 {
		_, err := b.Submit(t.Context(), 100, req)
		require.Error(t, err)
		require.Empty(t, b.parentStates)
	}
	chain.packageErr = nil
	_, err := b.Submit(t.Context(), 102, req)
	require.NoError(t, err)
	require.Equal(t, 1, wallet.scriptCalls)
	for _, call := range chain.packageCalls {
		require.Equal(
			t, chain.packageCalls[0].Child.TxOut[0].PkScript,
			call.Child.TxOut[0].PkScript,
		)
	}
}

// TestCPFPAddressCacheBoundaries pins independent parents, failed derivation,
// defensive copies, eviction without fee state, and the deliberate restart
// boundary: a new broadcaster may allocate again for the same parent.
func TestCPFPAddressCacheBoundaries(t *testing.T) {
	t.Parallel()

	wallet := &allocatingWallet{fakeWallet: &fakeWallet{},
		scriptErr: fmt.Errorf("wallet unavailable")}
	cfg := BroadcasterConfig{Wallet: wallet}
	b := NewCPFPBroadcaster(cfg)
	parentA := makeTestTxWithPrevoutSeed(1, true).TxHash()
	parentB := makeTestTxWithPrevoutSeed(2, true).TxHash()
	_, err := b.parentChangePkScript(t.Context(), parentA)
	require.Error(t, err)
	require.Empty(t, b.changeScripts)
	wallet.scriptErr = nil
	a, err := b.parentChangePkScript(t.Context(), parentA)
	require.NoError(t, err)
	original := append([]byte(nil), a...)
	a[0]++
	a, err = b.parentChangePkScript(t.Context(), parentA)
	require.NoError(t, err)
	require.Equal(t, original, a)
	other, err := b.parentChangePkScript(t.Context(), parentB)
	require.NoError(t, err)
	require.NotEqual(t, a, other)
	require.Equal(t, 3, wallet.scriptCalls)
	b.Evict(t.Context(), parentA)
	require.NotContains(t, b.changeScripts, parentA)
	require.Contains(t, b.changeScripts, parentB)
	restarted := NewCPFPBroadcaster(cfg)
	afterRestart, err := restarted.parentChangePkScript(
		t.Context(), parentA,
	)
	require.NoError(t, err)
	require.NotEqual(t, original, afterRestart)
	require.Equal(t, 4, wallet.scriptCalls)
}

// TestDirectBroadcastDoesNotAllocateChange keeps plain transactions and the
// initial submission of a funded-anchor parent outside the CPFP cache path.
func TestDirectBroadcastDoesNotAllocateChange(t *testing.T) {
	t.Parallel()

	wallet := &allocatingWallet{fakeWallet: &fakeWallet{}}
	b := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource: newFakeChainSourceRef(100), Wallet: wallet,
	})
	plain := makeTestTx(false)
	funded := makeTestTx(true)
	funded.TxOut[1].Value = 330
	for _, parent := range []*wire.MsgTx{plain, funded} {
		result, err := b.Submit(
			t.Context(), 100, &BroadcastRequest{
				Tx: parent,
			},
		)
		require.NoError(t, err)
		require.Nil(t, result.ChildTxid)
	}
	require.Zero(t, wallet.scriptCalls)
	require.Empty(t, b.changeScripts)
}

// TestFanoutFailedFundingReusesOutputPlan proves idle funding failures retain
// destinations, while amounts remain fresh and growing demand allocates only
// the added output. Resolved demand and eviction end the unsubmitted plan.
func TestFanoutFailedFundingReusesOutputPlan(t *testing.T) {
	t.Parallel()

	wallet := &allocatingWallet{fakeWallet: &fakeWallet{}}
	b := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource: newFakeChainSourceRef(100), Wallet: wallet,
	})
	c := newTestFeeBumpController(t, b)
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
			minAmount: 20_000,
		},
	}
	for range 20 {
		_, err := c.EnsureSupply(t.Context(), demands, 5, 100, 1)
		require.ErrorContains(t, err, "insufficient funds")
	}
	require.Equal(t, 2, wallet.scriptCalls)
	require.Len(t, wallet.fundedTemplates, 20)
	for _, template := range wallet.fundedTemplates {
		require.Equal(
			t, wallet.fundedTemplates[0].TxOut, template.TxOut,
		)
	}

	demands[0].minAmount++
	demands = append(demands, feeInputDemand{
		parentTxid: chainhash.Hash{3}, minAmount: 30_000,
	})
	_, err := c.EnsureSupply(t.Context(), demands, 6, 102, 1)
	require.Error(t, err)
	require.Equal(t, 3, wallet.scriptCalls)
	last := wallet.fundedTemplates[len(wallet.fundedTemplates)-1]
	require.EqualValues(t, 10_001, last.TxOut[0].Value)
	for i := range 2 {
		require.Equal(
			t, wallet.fundedTemplates[0].TxOut[i].PkScript,
			last.TxOut[i].PkScript,
		)
	}

	_, err = c.EnsureSupply(t.Context(), nil, 6, 103, 1)
	require.NoError(t, err)
	require.Empty(t, c.env.fanoutScripts.outputs)
	_, err = c.EnsureSupply(t.Context(), demands[:2], 6, 104, 1)
	require.Error(t, err)
	require.Equal(t, 5, wallet.scriptCalls)
	c.PruneParent(t.Context(), chainhash.Hash{99})
	require.Len(t, c.env.fanoutScripts.outputs, 2)
	c.PruneParent(t.Context(), demands[0].parentTxid)
	require.Len(t, c.env.fanoutScripts.outputs, 2)
	c.PruneParent(t.Context(), demands[1].parentTxid)
	require.Empty(t, c.env.fanoutScripts.outputs)
}

// TestFanoutFallbackKeepsDestinations drives both funding strategies through
// repeated signing failures. Output and change slots are distinct and reused
// until submission; a later fanout must allocate fresh destinations.
func TestFanoutFallbackKeepsDestinations(t *testing.T) {
	t.Parallel()

	wallet := &allocatingWallet{fakeWallet: &fakeWallet{},
		finalizeErr: fmt.Errorf("signer unavailable")}
	b := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource: newFakeChainSourceRef(100), Wallet: wallet,
	})
	c := newTestFeeBumpController(t, b)
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
	reserved := makeWalletUTXOWithAmount(100_000, 7)
	b.reserveFeeInput(t.Context(), demands[0].parentTxid, &FeeInput{
		Outpoint: reserved.Outpoint, Confirmed: true,
		Output: &wire.TxOut{Value: int64(reserved.Amount),
			PkScript: reserved.PkScript},
	})
	for range 10 {
		_, err := c.EnsureSupply(t.Context(), demands, 5, 100, 1)
		require.ErrorContains(t, err, "signer unavailable")
	}
	require.Equal(
		t, 4, wallet.scriptCalls,
		"three output slots and separate fallback change",
	)
	wallet.finalizeErr = nil
	pending, err := c.EnsureSupply(t.Context(), demands, 5, 102, 1)
	require.NoError(t, err)
	require.NotNil(t, pending)
	require.Equal(t, 4, wallet.scriptCalls)
	require.Len(t, pending.tx.TxOut, 4)
	for i, output := range pending.tx.TxOut {
		for _, other := range pending.tx.TxOut[i+1:] {
			require.NotEqual(t, output.PkScript, other.PkScript)
		}
	}
	require.Empty(t, c.env.fanoutScripts.outputs)
	require.Empty(t, c.env.fanoutScripts.change)
	c.OnFanoutConfirmed(t.Context(), pending.txid)
	demands[0].parentTxid = chainhash.Hash{3}
	demands[1].parentTxid = chainhash.Hash{4}
	_, err = c.EnsureSupply(t.Context(), demands, 5, 104, 1)
	require.ErrorContains(t, err, "insufficient funds")
	require.Equal(t, 6, wallet.scriptCalls)
	require.NotEqual(
		t, pending.tx.TxOut[0].PkScript, c.env.fanoutScripts.outputs[0],
	)
}

// TestFanoutPublishEndsScriptPlan models a broadcast timeout after the first
// candidate may have reached the network. A later funding attempt using a
// different input must not pay destinations from that published candidate.
func TestFanoutPublishEndsScriptPlan(t *testing.T) {
	t.Parallel()

	chain := newFakeChainSourceRef(100)
	chain.broadcastErr = context.DeadlineExceeded
	wallet := &allocatingWallet{fakeWallet: &fakeWallet{
		utxos: []*walletcore.Utxo{
			makeWalletUTXOWithAmount(100_000, 7),
		},
	}}
	b := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource: chain, Wallet: wallet,
	})
	c := newTestFeeBumpController(t, b)
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
	pending, err := c.EnsureSupply(t.Context(), demands, 5, 100, 1)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, pending)
	require.Len(t, chain.broadcastCalls, 1)
	first := chain.broadcastCalls[0].Tx.Copy()
	require.Equal(t, 1, wallet.scriptCalls)

	// The old input remains unavailable after the ambiguous attempt. The
	// existing funding retry can select another coin and publish both txs.
	wallet.utxos = []*walletcore.Utxo{makeWalletUTXOWithAmount(100_000, 8)}
	chain.broadcastErr = nil
	pending, err = c.EnsureSupply(t.Context(), demands, 5, 102, 1)
	require.NoError(t, err)
	require.NotNil(t, pending)
	require.Len(t, chain.broadcastCalls, 2)
	require.NotEqual(
		t, first.TxIn[0].PreviousOutPoint,
		pending.tx.TxIn[0].PreviousOutPoint,
	)
	require.NotEqual(
		t, first.TxOut[0].PkScript, pending.tx.TxOut[0].PkScript,
	)
	require.Equal(t, 2, wallet.scriptCalls)
}

// TestFanoutPartialAllocationSurvivesFailure proves a failed later derivation
// does not consume the successful earlier slot again, and callers cannot
// mutate a cached script through the returned output plan.
func TestFanoutPartialAllocationSurvivesFailure(t *testing.T) {
	t.Parallel()

	wallet := &allocatingWallet{fakeWallet: &fakeWallet{}}
	env := &feeBumpEnvironment{broadcaster: NewCPFPBroadcaster(
		BroadcasterConfig{
			Wallet: wallet,
		},
	)}
	demands := []feeInputDemand{{minAmount: 10_000}}
	first, err := env.fanoutOutputs(t.Context(), demands, 1)
	require.NoError(t, err)
	original := append([]byte(nil), first[0].PkScript...)
	first[0].PkScript[0]++
	wallet.scriptErr = fmt.Errorf("wallet unavailable")
	_, err = env.fanoutOutputs(t.Context(), demands, 2)
	require.Error(t, err)
	wallet.scriptErr = nil
	outputs, err := env.fanoutOutputs(t.Context(), demands, 2)
	require.NoError(t, err)
	require.Equal(
		t, 3, wallet.scriptCalls,
		"two successful destinations and one failed allocation",
	)
	require.Equal(t, original, outputs[0].PkScript)
	require.NotEqual(t, outputs[0].PkScript, outputs[1].PkScript)
}
