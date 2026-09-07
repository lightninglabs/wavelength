package unroll

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/stretchr/testify/require"
)

// TestAssetUnrollWithholdsBitcoinSweep still materializes the presigned path
// while refusing both fresh and checkpoint-restored Bitcoin sweeps.
func TestAssetUnrollWithholdsBitcoinSweep(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	root := chainhash.Hash{1}
	desc.TaprootAssetRoot = &root
	unrollActor, behavior, txconfirmRef, _, exec := newActorHarnessExec(
		t, proof, desc,
	)
	mustAsk(t, unrollActor.Ref(), &StartUnrollRequest{
		Height: 100, Trigger: TriggerManual,
	})
	require.Equal(t, 1, txconfirmRef.requestCount())
	require.Equal(
		t, proof.RootTxids()[0],
		txconfirmRef.lastRequest(t).Tx.TxHash(),
	)
	_, err := behavior.resolveExitSpendPolicy(t.Context())
	require.ErrorIs(t, err, vtxo.ErrAssetVTXORequiresTransition)
	require.ErrorIs(
		t,
		behavior.startSweep(
			t.Context(), exec,
		),
		vtxo.ErrAssetVTXORequiresTransition,
	)
	behavior.sweepTx = wire.NewMsgTx(2)
	require.ErrorIs(
		t,
		behavior.startSweep(
			t.Context(), exec,
		),
		vtxo.ErrAssetVTXORequiresTransition,
	)
	require.Equal(t, 1, txconfirmRef.requestCount())
}
