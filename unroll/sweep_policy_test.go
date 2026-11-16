package unroll

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/tx/arktx"
	"github.com/lightninglabs/wavelength/txconfirm"
	"github.com/stretchr/testify/require"
)

// uncheckedSweepPolicy models a policy with a different fee budget that has
// omitted its own dust validation. The common executor must still reject dust.
type uncheckedSweepPolicy struct {
	*StandardVTXOExitSpendPolicy
	vbytes int64
}

// Kind gives the fixture a nonstandard identity so the actor uses its resolver.
func (p *uncheckedSweepPolicy) Kind() ExitPolicyKind {
	return "test_unchecked_payout"
}

// BuildSpendTx signs the fixture's actual fee-derived output without checking
// dust, exercising the executor's policy-independent persistence boundary.
func (p *uncheckedSweepPolicy) BuildSpendTx(_ context.Context,
	req ExitSpendRequest) (*wire.MsgTx, error) {

	tx := wire.NewMsgTx(arktx.TxVersion)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: req.TargetOutpoint,
		Sequence:         p.CSVDelay(),
	})
	tx.AddTxOut(
		wire.NewTxOut(
			req.TargetOutput.Value-req.FeeRateSatPerVByte*p.vbytes,
			append(
				[]byte(nil), req.DestinationPkScript...,
			),
		),
	)

	return p.signSpendTx(req, tx)
}

// TestSharedSweepDustBackstop keeps a policy that forgot its producer guard
// from persisting or submitting dust, while preserving deferred recovery.
func TestSharedSweepDustBackstop(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	inst, b, confirmer, store := newActorHarness(t, proof, desc)
	policy := &uncheckedSweepPolicy{
		StandardVTXOExitSpendPolicy: NewStandardVTXOExitSpendPolicy(
			desc,
		),
		vbytes: 200,
	}
	b.cfg.ExitSpendPolicyResolver = &fixedExitSpendPolicyResolver{
		policy: policy,
	}
	b.cfg.MaxSweepFeeRateSatPerVByte = 300
	chain, ok := b.cfg.ChainSource.(*fakeChainSourceRef)
	require.True(t, ok)
	chain.setFeeEstimate(249, nil)
	mustAsk(t, inst.Ref(), &StartUnrollRequest{
		Height: 100, Trigger: TriggerManual,
		ExitPolicyKind: policy.Kind(), ExitPolicyRef: "fixture",
	})
	mustAsk(
		t, inst.Ref(), &TxConfirmedMsg{
			Txid:   proof.RootTxids()[0],
			Height: 101,
		},
	)
	mustAsk(
		t, inst.Ref(), &TxConfirmedMsg{
			Txid:   proof.TargetOutpoint().Hash,
			Height: 102,
		},
	)
	mustAsk(t, inst.Ref(), &HeightObservedMsg{Height: 104})
	require.Equal(t, 2, confirmer.requestCount())
	cp := mustDecodeCheckpoint(t, store, "unroll-test")
	require.Nil(t, cp.SweepTx)
	require.Zero(t, cp.SweepAttempts)
	require.Empty(t, cp.Fail)
	chain.setFeeEstimate(2, nil)
	mustAsk(t, inst.Ref(), &HeightObservedMsg{Height: 105})
	require.Equal(t, 3, confirmer.requestCount())
	require.False(t, sweepHasDustOutput(confirmer.lastRequest(t).Tx))
}

// TestRepriceUsesPolicyFeeBudget proves a policy's own output fee determines
// whether replacement is useful, rather than the standard sweep's size guess.
func TestRepriceUsesPolicyFeeBudget(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	inst, b, confirmer, _ := newActorHarness(t, proof, desc)
	policy := &uncheckedSweepPolicy{
		StandardVTXOExitSpendPolicy: NewStandardVTXOExitSpendPolicy(
			desc,
		),
		vbytes: 260,
	}
	b.cfg.ExitSpendPolicyResolver = &fixedExitSpendPolicyResolver{
		policy: policy,
	}
	chain, ok := b.cfg.ChainSource.(*fakeChainSourceRef)
	require.True(t, ok)
	chain.setFeeEstimate(10, nil)
	mustAsk(t, inst.Ref(), &StartUnrollRequest{
		Height: 100, Trigger: TriggerManual,
		ExitPolicyKind: policy.Kind(), ExitPolicyRef: "fixture",
	})
	mustAsk(
		t, inst.Ref(), &TxConfirmedMsg{
			Txid:   proof.RootTxids()[0],
			Height: 101,
		},
	)
	mustAsk(
		t, inst.Ref(), &TxConfirmedMsg{
			Txid:   proof.TargetOutpoint().Hash,
			Height: 102,
		},
	)
	mustAsk(t, inst.Ref(), &HeightObservedMsg{Height: 104})
	old := confirmer.lastRequest(t).Tx.Copy()
	require.Equal(t, int64(2600), int64(desc.Amount)-old.TxOut[0].Value)
	mustAsk(t, inst.Ref(), &TxFailedMsg{
		Txid: old.TxHash(), Reason: "min relay fee not met",
		Class: txconfirm.BroadcastFailureFee,
	})
	chain.setFeeEstimate(11, nil)
	mustAsk(t, inst.Ref(), &HeightObservedMsg{Height: 105})
	require.Equal(t, 4, confirmer.requestCount())
	newTx := confirmer.lastRequest(t).Tx
	require.Equal(t, int64(2860), int64(desc.Amount)-newTx.TxOut[0].Value)
	require.Equal(t, old.TxOut[0].PkScript, newTx.TxOut[0].PkScript)
}
