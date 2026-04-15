package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
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

// TestInputsLockSucceededEventEmitsValidateSubmit asserts locking success
// advances to submit validation and emits ValidateSubmitReq.
func TestInputsLockSucceededEventEmitsValidateSubmit(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	arkTx := wire.NewMsgTx(2)
	arkPsbt, err := psbt.NewFromUnsignedTx(arkTx)
	require.NoError(t, err)

	state := &AwaitingInputsLockState{
		Inputs:  []wire.OutPoint{{Index: 42}},
		ArkPSBT: arkPsbt,
	}

	policy := arkscript.CheckpointPolicy{CSVDelay: 7}
	tr, err := state.ProcessEvent(ctx, &InputsLockSucceededEvent{},
		&Environment{CheckpointPolicy: policy})
	require.NoError(t, err)
	require.NotNil(t, tr)

	next, ok := tr.NextState.(*AwaitingSubmitValidationState)
	require.True(t, ok)
	require.Same(t, arkPsbt, next.ArkPSBT)
	require.Equal(t, uint32(42), next.Inputs[0].Index)

	outbox := collectOutbox(t, tr)
	require.Len(t, outbox, 1)
	validateReq, ok := outbox[0].(*ValidateSubmitReq)
	require.True(t, ok)
	require.Same(t, arkPsbt, validateReq.ArkPSBT)
	require.Equal(t, policy, validateReq.CheckpointPolicy)
}

// TestInputsLockFailedEventMovesToFailedState asserts lock failures transition
// to terminal failure without additional side effects.
func TestInputsLockFailedEventMovesToFailedState(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	state := &AwaitingInputsLockState{}
	tr, err := state.ProcessEvent(
		ctx, &InputsLockFailedEvent{Reason: "lock busy"},
		&Environment{},
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

// TestAwaitingFinalizeValidationRetryKeepsCanonicalPackage asserts finalize
// retries in AwaitingFinalizeValidationState re-emit validation with the
// already accepted canonical checkpoint package when payload matches.
func TestAwaitingFinalizeValidationRetryKeepsCanonicalPackage(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	arkTx := wire.NewMsgTx(2)
	arkPsbt, err := psbt.NewFromUnsignedTx(arkTx)
	require.NoError(t, err)

	storedCheckpoints := makeCheckpointPSBTs(t, wire.OutPoint{Index: 11})
	retryCheckpoints := makeCheckpointPSBTs(t, wire.OutPoint{Index: 11})

	state := &AwaitingFinalizeValidationState{
		ArkPSBT:              arkPsbt,
		FinalCheckpointPSBTs: storedCheckpoints,
	}

	tr, err := state.ProcessEvent(ctx, &FinalizeRequestedEvent{
		FinalCheckpointPSBTs: retryCheckpoints,
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, tr)
	require.Same(t, state, tr.NextState)

	outbox := collectOutbox(t, tr)
	require.Len(t, outbox, 1)

	validateReq, ok := outbox[0].(*ValidateFinalizeReq)
	require.True(t, ok)
	require.Same(t, arkPsbt, validateReq.ArkPSBT)
	require.Equal(t, storedCheckpoints, validateReq.FinalCheckpointPSBTs)
}

// TestAwaitingFinalizeValidationRetryRejectsMismatchedPackage asserts finalize
// retries that change checkpoint payload are rejected explicitly.
func TestAwaitingFinalizeValidationRetryRejectsMismatchedPackage(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	state := &AwaitingFinalizeValidationState{
		FinalCheckpointPSBTs: makeCheckpointPSBTs(
			t, wire.OutPoint{Index: 21},
		),
	}

	tr, err := state.ProcessEvent(ctx, &FinalizeRequestedEvent{
		FinalCheckpointPSBTs: makeCheckpointPSBTs(
			t, wire.OutPoint{Index: 22},
		),
	}, nil)
	require.ErrorContains(t, err, "final checkpoint package mismatch")
	require.Nil(t, tr)
}

// TestFinalizeSucceededTransitionsToRecipientNotifyState asserts finalize
// success emits notify work before entering the terminal finalized state.
func TestFinalizeSucceededTransitionsToRecipientNotifyState(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	arkTx := wire.NewMsgTx(2)
	arkPsbt, err := psbt.NewFromUnsignedTx(arkTx)
	require.NoError(t, err)

	checkpointTx := wire.NewMsgTx(2)
	checkpointPsbt, err := psbt.NewFromUnsignedTx(checkpointTx)
	require.NoError(t, err)
	finalCheckpoints := []*psbt.Packet{checkpointPsbt}

	state := &CoSignedState{
		ArkPSBT: arkPsbt,
	}

	tr, err := state.ProcessEvent(ctx, &FinalizeSucceededEvent{
		FinalCheckpointPSBTs: finalCheckpoints,
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, tr)

	next, ok := tr.NextState.(*AwaitingRecipientsNotifyState)
	require.True(t, ok)
	require.Same(t, arkPsbt, next.ArkPSBT)
	require.Equal(t, finalCheckpoints, next.FinalCheckpointPSBTs)

	outbox := collectOutbox(t, tr)
	require.Len(t, outbox, 1)
	notifyReq, ok := outbox[0].(*NotifyRecipientsReq)
	require.True(t, ok)
	require.Same(t, arkPsbt, notifyReq.ArkPSBT)
	require.Equal(t, finalCheckpoints, notifyReq.FinalCheckpointPSBTs)
}

// TestAwaitingRecipientsNotifyFinalizeRetryReemitsNotify asserts finalize
// retries re-emit recipient notifications when notification is still pending.
func TestAwaitingRecipientsNotifyFinalizeRetryReemitsNotify(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	arkTx := wire.NewMsgTx(2)
	arkPsbt, err := psbt.NewFromUnsignedTx(arkTx)
	require.NoError(t, err)

	checkpointTx := wire.NewMsgTx(2)
	checkpointPsbt, err := psbt.NewFromUnsignedTx(checkpointTx)
	require.NoError(t, err)
	finalCheckpoints := []*psbt.Packet{checkpointPsbt}

	state := &AwaitingRecipientsNotifyState{
		ArkPSBT:              arkPsbt,
		FinalCheckpointPSBTs: finalCheckpoints,
	}

	tr, err := state.ProcessEvent(ctx, &FinalizeRequestedEvent{}, nil)
	require.NoError(t, err)
	require.NotNil(t, tr)
	require.Same(t, state, tr.NextState)

	outbox := collectOutbox(t, tr)
	require.Len(t, outbox, 1)
	notifyReq, ok := outbox[0].(*NotifyRecipientsReq)
	require.True(t, ok)
	require.Same(t, arkPsbt, notifyReq.ArkPSBT)
	require.Equal(t, finalCheckpoints, notifyReq.FinalCheckpointPSBTs)
}

// TestAwaitingRecipientsNotifyEvents asserts recipient notification success
// reaches finalized and notification failure remains retryable with an
// observable failure reason.
func TestAwaitingRecipientsNotifyEvents(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	arkTx := wire.NewMsgTx(2)
	arkPsbt, err := psbt.NewFromUnsignedTx(arkTx)
	require.NoError(t, err)

	checkpointTx := wire.NewMsgTx(2)
	checkpointPsbt, err := psbt.NewFromUnsignedTx(checkpointTx)
	require.NoError(t, err)
	finalCheckpoints := []*psbt.Packet{checkpointPsbt}

	state := &AwaitingRecipientsNotifyState{
		ArkPSBT:              arkPsbt,
		FinalCheckpointPSBTs: finalCheckpoints,
	}

	successTr, err := state.ProcessEvent(
		ctx, &NotifyRecipientsSucceededEvent{}, nil,
	)
	require.NoError(t, err)
	require.NotNil(t, successTr)
	require.IsType(t, &FinalizedState{}, successTr.NextState)
	require.Empty(t, collectOutbox(t, successTr))

	failTr, err := state.ProcessEvent(
		ctx, &NotifyRecipientsFailedEvent{Reason: "notify failed"}, nil,
	)
	require.NoError(t, err)
	require.NotNil(t, failTr)

	next, ok := failTr.NextState.(*AwaitingRecipientsNotifyState)
	require.True(t, ok)
	require.Same(t, state.ArkPSBT, next.ArkPSBT)
	require.Equal(t, finalCheckpoints, next.FinalCheckpointPSBTs)
	require.Equal(t, "notify failed", next.LastNotifyFailureReason)
	require.Empty(t, collectOutbox(t, failTr))
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
