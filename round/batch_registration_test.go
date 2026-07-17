package round

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/batchcanon"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type testRoundBatchRegistrar struct {
	register func(context.Context, *batchcanon.RegisterBatchRequest) error
}

func (t *testRoundBatchRegistrar) RegisterBatch(ctx context.Context,
	req *batchcanon.RegisterBatchRequest) error {

	return t.register(ctx, req)
}

// TestInputSigSentRegistersBatchBeforeExposure verifies the confirmation path
// durably registers complete commitment evidence before it saves any VTXO.
func TestInputSigSentRegistersBatchBeforeExposure(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	intent := h.newTestBoardingIntent()
	state := h.newInputSigSentState(
		testRoundIDTr("round-register-batch"), []BoardingIntent{intent},
	)
	state.SweepDelay = 1008
	h.withState(state)

	var (
		registered   bool
		saved        bool
		registration *batchcanon.RegisterBatchRequest
	)
	h.vtxoStore.On(
		"SaveVTXOs", mock.Anything, mock.Anything,
	).Run(func(mock.Arguments) {
		require.True(t, registered, "VTXOs saved before registration")
		saved = true
	}).Return(nil)
	h.env.BatchRegistrar = &testRoundBatchRegistrar{
		register: func(_ context.Context,
			req *batchcanon.RegisterBatchRequest) error {

			require.False(
				t, saved, "registration ran after VTXO save",
			)
			registered = true
			registration = req

			return nil
		},
	}

	commitmentTx := state.CommitmentTx.UnsignedTx
	_, err := h.sendEvent(&BoardingConfirmed{
		TxID:        commitmentTx.TxHash(),
		BlockHeight: 101,
	})
	require.NoError(t, err)
	require.True(t, registered)
	require.True(t, saved)
	require.NotNil(t, registration)
	require.Equal(t, commitmentTx.TxHash(), registration.BatchTxID)
	require.Equal(t, uint32(0), registration.BatchOutputIndex)
	require.Equal(t, int32(1008), registration.CSVExpiryDelta)
	require.Equal(
		t, commitmentTx.TxOut[0].PkScript,
		registration.ConfirmationPkScript,
	)
	require.Len(t, registration.ConsumedInputs, 1)
	require.Equal(
		t, commitmentTx.TxIn[0].PreviousOutPoint,
		registration.ConsumedInputs[0].Outpoint,
	)
	require.Equal(
		t, state.CommitmentTx.Inputs[0].WitnessUtxo.Value,
		registration.ConsumedInputs[0].Value,
	)
	require.Equal(
		t, state.CommitmentTx.Inputs[0].WitnessUtxo.PkScript,
		registration.ConsumedInputs[0].PkScript,
	)
	require.Len(t, registration.DependentVTXOs, 1)
	require.Empty(t, registration.ConsumedVTXOs)

	var decoded wire.MsgTx
	require.NoError(
		t,
		decoded.Deserialize(
			bytes.NewReader(registration.BatchTx),
		),
	)
	require.Equal(t, commitmentTx.TxHash(), decoded.TxHash())
}

// TestRoundBatchRegistrationBindsRefreshConsumer verifies a refresh edge uses
// the exact next lifecycle revision and the full distinct creator lineage.
func TestRoundBatchRegistrationBindsRefreshConsumer(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	intent := h.newTestBoardingIntent()
	state := h.newInputSigSentState(
		testRoundIDTr("round-register-refresh"),
		[]BoardingIntent{intent},
	)
	forfeitedOutpoint := h.newTestOutpoint()
	creatorA := chainhash.HashH([]byte("creator-a"))
	creatorB := chainhash.HashH([]byte("creator-b"))
	state.ForfeitedVTXOs = []wire.OutPoint{forfeitedOutpoint}
	h.withState(state)

	h.setupMockVTXOStoreForSave()
	h.vtxoStore.On(
		"GetVTXO", mock.Anything, forfeitedOutpoint,
	).Return(&ClientVTXO{
		Outpoint:         forfeitedOutpoint,
		CommitmentTxID:   creatorA,
		BusinessRevision: 7,
		Ancestry: []types.Ancestry{
			{CommitmentTxID: creatorA},
			{CommitmentTxID: creatorB},
		},
	}, nil)

	var registration *batchcanon.RegisterBatchRequest
	h.env.BatchRegistrar = &testRoundBatchRegistrar{
		register: func(_ context.Context,
			req *batchcanon.RegisterBatchRequest) error {

			registration = req

			return nil
		},
	}

	batchTxID := state.CommitmentTx.UnsignedTx.TxHash()
	_, err := h.sendEvent(&BoardingConfirmed{
		TxID:        batchTxID,
		BlockHeight: 101,
	})
	require.NoError(t, err)
	require.NotNil(t, registration)
	require.Len(t, registration.ConsumedVTXOs, 1)

	edge := registration.ConsumedVTXOs[0]
	require.Equal(t, forfeitedOutpoint, edge.ConsumedVTXO)
	require.Equal(t, batchTxID, edge.ConsumerBatch)
	require.Equal(t, uint64(8), edge.ExpectedRevision)
	require.Equal(
		t, []chainhash.Hash{creatorA, creatorB}, edge.CreatorLineage,
	)
}

// TestInputSigSentRegistrationFailureDoesNotExposeVTXOs verifies a failed
// registration leaves the fail-closed boundary intact.
func TestInputSigSentRegistrationFailureDoesNotExposeVTXOs(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	intent := h.newTestBoardingIntent()
	state := h.newInputSigSentState(
		testRoundIDTr("round-register-failure"),
		[]BoardingIntent{intent},
	)
	h.withState(state)

	h.env.BatchRegistrar = &testRoundBatchRegistrar{
		register: func(context.Context,
			*batchcanon.RegisterBatchRequest) error {

			return errors.New("registration unavailable")
		},
	}

	_, err := h.sendEvent(&BoardingConfirmed{
		TxID:        state.CommitmentTx.UnsignedTx.TxHash(),
		BlockHeight: 101,
	})
	require.ErrorContains(t, err, "registration unavailable")
	h.vtxoStore.AssertNotCalled(t, "SaveVTXOs")
}
