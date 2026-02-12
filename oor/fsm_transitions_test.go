package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

// TestSignFailedBeforePointOfNoReturnUnlocks asserts that signing failures
// before CoSigned emit an unlock request.
func TestSignFailedBeforePointOfNoReturnUnlocks(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	state := &ValidatedState{}

	tr, err := state.ProcessEvent(
		ctx, &SignFailedEvent{Reason: "fail"}, nil,
	)
	require.NoError(t, err)
	require.NotNil(t, tr)

	outbox := collectOutbox(t, tr)
	require.Len(t, outbox, 1)
	require.IsType(t, &UnlockInputsReq{}, outbox[0])
}

// TestSignFailedAfterPointOfNoReturnDoesNotUnlock asserts that failures after
// CoSigned do not emit any unlock request.
func TestSignFailedAfterPointOfNoReturnDoesNotUnlock(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	state := &CoSignedState{}

	tr, err := state.ProcessEvent(
		ctx, &SignFailedEvent{Reason: "fail"}, nil,
	)
	require.NoError(t, err)
	require.NotNil(t, tr)

	outbox := collectOutbox(t, tr)
	require.Empty(t, outbox)
}

// TestInputsLockSucceededEventEmitsCoSign asserts locking success advances
// to the co-signing gate and emits CoSignReq.
func TestInputsLockSucceededEventEmitsCoSign(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	arkTx := wire.NewMsgTx(2)
	arkPsbt, err := psbt.NewFromUnsignedTx(arkTx)
	require.NoError(t, err)

	state := &RequestedState{
		ArkPSBT: arkPsbt,
	}

	tr, err := state.ProcessEvent(ctx, &InputsLockSucceededEvent{}, nil)
	require.NoError(t, err)
	require.NotNil(t, tr)

	next, ok := tr.NextState.(*ValidatedState)
	require.True(t, ok)
	require.Same(t, arkPsbt, next.ArkPSBT)

	outbox := collectOutbox(t, tr)
	require.Len(t, outbox, 1)
	require.IsType(t, &CoSignReq{}, outbox[0])
}

// TestInputsLockFailedEventMovesToFailedState asserts lock failures transition
// to terminal failure without additional side effects.
func TestInputsLockFailedEventMovesToFailedState(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	state := &RequestedState{}
	tr, err := state.ProcessEvent(
		ctx, &InputsLockFailedEvent{Reason: "lock busy"}, nil,
	)
	require.NoError(t, err)
	require.NotNil(t, tr)

	failed, ok := tr.NextState.(*FailedState)
	require.True(t, ok)
	require.Equal(t, "lock busy", failed.Reason)

	outbox := collectOutbox(t, tr)
	require.Empty(t, outbox)
}

// TestFinalizeRequestCarriesArkToValidation asserts finalize validation
// receives the canonical Ark PSBT from session state.
func TestFinalizeRequestCarriesArkToValidation(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	arkTx := wire.NewMsgTx(2)
	arkPsbt, err := psbt.NewFromUnsignedTx(arkTx)
	require.NoError(t, err)

	state := &CoSignedState{
		ArkPSBT: arkPsbt,
	}

	tr, err := state.ProcessEvent(ctx, &FinalizeRequestedEvent{
		FinalCheckpointPSBTs: []*psbt.Packet{{}},
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, tr)

	outbox := collectOutbox(t, tr)
	require.Len(t, outbox, 1)

	validateReq, ok := outbox[0].(*ValidateFinalizeReq)
	require.True(t, ok)
	require.Same(t, arkPsbt, validateReq.ArkPSBT)
}

// TestSubmitRequestedPopulatesLockInputs asserts SubmitRequestedEvent derives
// and emits the checkpoint input outpoints in LockInputsReq.
func TestSubmitRequestedPopulatesLockInputs(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	checkpoints := makeCheckpointPSBTs(t,
		wire.OutPoint{Index: 1},
		wire.OutPoint{Index: 2},
	)

	state := &IdleState{}
	tr, err := state.ProcessEvent(ctx, &SubmitRequestedEvent{
		CheckpointPSBTs: checkpoints,
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, tr)

	outbox := collectOutbox(t, tr)
	require.Len(t, outbox, 1)

	lockReq, ok := outbox[0].(*LockInputsReq)
	require.True(t, ok)
	require.Len(t, lockReq.Inputs, 2)
	require.Equal(t, uint32(1), lockReq.Inputs[0].Index)
	require.Equal(t, uint32(2), lockReq.Inputs[1].Index)
}

// collectOutbox is a test helper that extracts the outbox list from a
// transition.
func collectOutbox(t *testing.T, tr *StateTransition) []OutboxEvent {
	t.Helper()

	if tr.NewEvents.IsNone() {
		return nil
	}

	emitted := tr.NewEvents.UnwrapOr(EmittedEvent{})

	return emitted.Outbox
}

// makeCheckpointPSBTs returns v0-like checkpoint PSBTs with one input each for
// lock-set extraction tests.
func makeCheckpointPSBTs(t *testing.T,
	inputs ...wire.OutPoint) []*psbt.Packet {

	t.Helper()

	checkpoints := make([]*psbt.Packet, 0, len(inputs))
	for i := range inputs {
		tx := wire.NewMsgTx(2)
		tx.AddTxIn(&wire.TxIn{PreviousOutPoint: inputs[i]})
		tx.AddTxOut(&wire.TxOut{Value: 1000, PkScript: []byte{0x51}})

		pkt, err := psbt.NewFromUnsignedTx(tx)
		require.NoError(t, err)

		checkpoints = append(checkpoints, pkt)
	}

	return checkpoints
}
