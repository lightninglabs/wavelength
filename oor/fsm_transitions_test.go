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

// TestSubmitValidatedBeforeInputsLockedIsIgnored asserts that RequestedState
// rejects early validation success before inputs are locked.
func TestSubmitValidatedBeforeInputsLockedIsIgnored(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	state := &RequestedState{
		InputsLocked: false,
	}

	tr, err := state.ProcessEvent(ctx, &SubmitValidatedEvent{}, nil)
	require.NoError(t, err)
	require.NotNil(t, tr)
	require.Same(t, state, tr.NextState)

	outbox := collectOutbox(t, tr)
	require.Empty(t, outbox)
}

// TestInputsLockedEventEnablesValidation asserts that locking success marks the
// requested state as lock-confirmed and emits submit validation.
func TestInputsLockedEventEnablesValidation(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	state := &RequestedState{
		InputsLocked: false,
	}

	tr, err := state.ProcessEvent(ctx, &InputsLockedEvent{}, nil)
	require.NoError(t, err)
	require.NotNil(t, tr)

	next, ok := tr.NextState.(*RequestedState)
	require.True(t, ok)
	require.True(t, next.InputsLocked)

	outbox := collectOutbox(t, tr)
	require.Len(t, outbox, 1)
	require.IsType(t, &ValidateSubmitReq{}, outbox[0])
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
