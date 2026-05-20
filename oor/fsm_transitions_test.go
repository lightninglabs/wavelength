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
		ctx, &SignFailedEvent{
			Reason: "fail",
		},
		nil,
	)
	require.NoError(t, err)
	require.NotNil(t, tr)

	outbox := collectOutbox(t, tr)
	require.Len(t, outbox, 1)
	require.IsType(t, &UnlockInputsReq{}, outbox[0])
}

// TestSignFailedAfterPointOfNoReturnDoesNotUnlock asserts that a stale or
// racing SignFailedEvent arriving after CoSigned does not emit an unlock
// request and does not terminate the session: CoSignedState is the
// point-of-no-return and must remain recoverable for finalize. Regression
// test for issue #372 — terminating here would orphan the input locks
// forever because FailedState ignores subsequent finalize retries.
func TestSignFailedAfterPointOfNoReturnDoesNotUnlock(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	arkTx := wire.NewMsgTx(2)
	arkPsbt, err := psbt.NewFromUnsignedTx(arkTx)
	require.NoError(t, err)

	checkpoints := makeCheckpointPSBTs(t, wire.OutPoint{Index: 7})
	state := &CoSignedState{
		Inputs: []wire.OutPoint{
			{
				Index: 7,
			},
		},
		ArkPSBT:                 arkPsbt,
		CoSignedCheckpointPSBTs: checkpoints,
	}

	tr, err := state.ProcessEvent(
		ctx, &SignFailedEvent{
			Reason: "fail",
		},
		nil,
	)
	require.NoError(t, err)
	require.NotNil(t, tr)

	outbox := collectOutbox(t, tr)
	require.Empty(
		t, outbox,
		"no unlock may be emitted after the point-of-no-return",
	)

	// The FSM must stay in CoSignedState so the session remains
	// recoverable; transitioning to FailedState would orphan the locks
	// because FailedState ignores all subsequent events.
	next, ok := tr.NextState.(*CoSignedState)
	require.True(
		t, ok, "expected CoSignedState, got %T", tr.NextState,
	)
	require.False(
		t, next.IsTerminal(),
		"CoSignedState must remain non-terminal so finalize can "+
			"still be retried",
	)
}

// TestInputsLockSucceededEventEmitsCoSign asserts locking success advances to
// the validated state and emits CoSignReq.
func TestInputsLockSucceededEventEmitsCoSign(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	arkTx := wire.NewMsgTx(2)
	arkPsbt, err := psbt.NewFromUnsignedTx(arkTx)
	require.NoError(t, err)

	state := &AwaitingInputsLockState{
		Inputs: []wire.OutPoint{
			{
				Index: 42,
			},
		},
		ArkPSBT: arkPsbt,
		CheckpointPSBTs: makeCheckpointPSBTs(
			t, wire.OutPoint{
				Index: 42,
			},
		),
	}

	tr, err := state.ProcessEvent(ctx, &InputsLockSucceededEvent{},
		&Environment{CheckpointPolicy: arkscript.CheckpointPolicy{
			CSVDelay: 7,
		}})
	require.NoError(t, err)
	require.NotNil(t, tr)

	next, ok := tr.NextState.(*ValidatedState)
	require.True(t, ok)
	require.Same(t, arkPsbt, next.ArkPSBT)
	require.Equal(t, uint32(42), next.Inputs[0].Index)

	outbox := collectOutbox(t, tr)
	require.Len(t, outbox, 1)
	coSignReq, ok := outbox[0].(*CoSignReq)
	require.True(t, ok)
	require.Same(t, arkPsbt, coSignReq.ArkPSBT)
	require.Equal(t, uint32(42), coSignReq.Inputs[0].Index)
}

// TestInputsLockFailedEventMovesToFailedState asserts lock failures transition
// to terminal failure without additional side effects.
func TestInputsLockFailedEventMovesToFailedState(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	state := &AwaitingInputsLockState{}
	tr, err := state.ProcessEvent(
		ctx, &InputsLockFailedEvent{
			Reason: "lock busy",
		},
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

// TestFinalizeFailedAfterPointOfNoReturnStaysRecoverable asserts that a
// finalize-validation failure after the point-of-no-return falls back to
// CoSignedState (carrying the failure reason) rather than transitioning to
// the terminal FailedState. The recoverable state preserves the input locks
// — which must not be released after co-sign — while still permitting the
// client to resubmit a corrected finalize package. Regression test for
// issue #372: terminating here would orphan the input locks forever because
// FailedState ignores all subsequent FinalizeRequestedEvents.
func TestFinalizeFailedAfterPointOfNoReturnStaysRecoverable(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	arkTx := wire.NewMsgTx(2)
	arkPsbt, err := psbt.NewFromUnsignedTx(arkTx)
	require.NoError(t, err)

	inputs := []wire.OutPoint{{Index: 13}}
	coSigned := makeCheckpointPSBTs(t, wire.OutPoint{Index: 13})
	finalCheckpoints := makeCheckpointPSBTs(t, wire.OutPoint{Index: 13})

	state := &AwaitingFinalizeValidationState{
		Inputs:                  inputs,
		ArkPSBT:                 arkPsbt,
		CoSignedCheckpointPSBTs: coSigned,
		FinalCheckpointPSBTs:    finalCheckpoints,
	}

	tr, err := state.ProcessEvent(ctx, &FinalizeFailedEvent{
		Reason: "bad signature",
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, tr)

	// We must NOT enter the terminal FailedState; locks would orphan.
	_, isFailed := tr.NextState.(*FailedState)
	require.False(
		t, isFailed, "finalize failure after point-of-no-return "+
			"must not reach the terminal FailedState",
	)

	next, ok := tr.NextState.(*CoSignedState)
	require.True(
		t, ok, "expected CoSignedState fallback, got %T", tr.NextState,
	)
	require.False(
		t, next.IsTerminal(),
		"fallback state must be non-terminal so finalize can be "+
			"retried",
	)

	// State must carry the data needed for a retry plus the failure
	// reason for the caller-visible error.
	require.Same(t, arkPsbt, next.ArkPSBT)
	require.Equal(t, inputs, next.Inputs)
	require.Equal(t, coSigned, next.CoSignedCheckpointPSBTs)
	require.Equal(t, "bad signature", next.LastFinalizeFailureReason)

	// No outbox: in particular no UnlockInputsReq may be emitted past
	// the point-of-no-return.
	require.Empty(t, collectOutbox(t, tr))
}

// TestCoSignedStateAcceptsFinalizeRetryAfterFailure end-to-end asserts the
// recovery path is wired: a failed finalize lands back in CoSignedState, and
// a subsequent FinalizeRequestedEvent from the same state successfully
// re-enters AwaitingFinalizeValidationState with a fresh ValidateFinalizeReq.
// Without the fix, the session would be terminal and the retry would be
// silently dropped by FailedState. Regression test for issue #372.
func TestCoSignedStateAcceptsFinalizeRetryAfterFailure(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	arkTx := wire.NewMsgTx(2)
	arkPsbt, err := psbt.NewFromUnsignedTx(arkTx)
	require.NoError(t, err)

	inputs := []wire.OutPoint{{Index: 99}}
	coSigned := makeCheckpointPSBTs(t, wire.OutPoint{Index: 99})

	// Step 1: a finalize attempt fails validation.
	awaitingValidation := &AwaitingFinalizeValidationState{
		Inputs:                  inputs,
		ArkPSBT:                 arkPsbt,
		CoSignedCheckpointPSBTs: coSigned,
		FinalCheckpointPSBTs: makeCheckpointPSBTs(
			t, wire.OutPoint{
				Index: 99,
			},
		),
	}
	failTr, err := awaitingValidation.ProcessEvent(
		ctx, &FinalizeFailedEvent{
			Reason: "malformed package",
		},
		nil,
	)
	require.NoError(t, err)
	recovered, ok := failTr.NextState.(*CoSignedState)
	require.True(t, ok)

	// Step 2: the client retries finalize with a corrected package; the
	// FSM must accept it and emit a fresh validation request.
	retryCheckpoints := makeCheckpointPSBTs(t, wire.OutPoint{Index: 99})
	retryTr, err := recovered.ProcessEvent(ctx, &FinalizeRequestedEvent{
		FinalCheckpointPSBTs: retryCheckpoints,
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, retryTr)

	nextState, ok := retryTr.NextState.(*AwaitingFinalizeValidationState)
	require.True(
		t, ok, "expected AwaitingFinalizeValidationState on retry, "+
			"got %T", retryTr.NextState,
	)
	require.Same(t, arkPsbt, nextState.ArkPSBT)
	require.Equal(t, retryCheckpoints, nextState.FinalCheckpointPSBTs)

	outbox := collectOutbox(t, retryTr)
	require.Len(t, outbox, 1)
	validateReq, ok := outbox[0].(*ValidateFinalizeReq)
	require.True(t, ok)
	require.Same(t, arkPsbt, validateReq.ArkPSBT)
	require.Equal(t, retryCheckpoints, validateReq.FinalCheckpointPSBTs)
}

// TestAwaitingFinalizeValidationRetryRejectsMismatchedPackage asserts finalize
// retries that change checkpoint payload are rejected explicitly.
func TestAwaitingFinalizeValidationRetryRejectsMismatchedPackage(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	state := &AwaitingFinalizeValidationState{
		FinalCheckpointPSBTs: makeCheckpointPSBTs(
			t, wire.OutPoint{
				Index: 21,
			},
		),
	}

	tr, err := state.ProcessEvent(ctx, &FinalizeRequestedEvent{
		FinalCheckpointPSBTs: makeCheckpointPSBTs(
			t, wire.OutPoint{
				Index: 22,
			},
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
		ctx, &NotifyRecipientsFailedEvent{
			Reason: "notify failed",
		},
		nil,
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

// TestSubmitRequestedEmitsValidateSubmit asserts SubmitRequestedEvent derives
// checkpoint inputs, moves to submit validation, and emits ValidateSubmitReq.
func TestSubmitRequestedEmitsValidateSubmit(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	checkpoints := makeCheckpointPSBTs(
		t, wire.OutPoint{
			Index: 1,
		}, wire.OutPoint{
			Index: 2,
		},
	)

	state := &IdleState{}
	policy := arkscript.CheckpointPolicy{CSVDelay: 11}
	tr, err := state.ProcessEvent(ctx, &SubmitRequestedEvent{
		CheckpointPSBTs: checkpoints,
	}, &Environment{CheckpointPolicy: policy})
	require.NoError(t, err)
	require.NotNil(t, tr)

	next, ok := tr.NextState.(*AwaitingSubmitValidationState)
	require.True(t, ok)
	require.Len(t, next.Inputs, 2)
	require.Equal(t, uint32(1), next.Inputs[0].Index)
	require.Equal(t, uint32(2), next.Inputs[1].Index)

	outbox := collectOutbox(t, tr)
	require.Len(t, outbox, 1)

	validateReq, ok := outbox[0].(*ValidateSubmitReq)
	require.True(t, ok)
	require.Equal(t, checkpoints, validateReq.CheckpointPSBTs)
	require.Equal(t, policy, validateReq.CheckpointPolicy)
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
func makeCheckpointPSBTs(t *testing.T, inputs ...wire.OutPoint) []*psbt.Packet {
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

// TestAwaitingSubmitValidationRejectCodePropagation pins the FSM-side
// invariant that AwaitingSubmitValidationState.ProcessEvent carries
// the typed RejectCode from a SubmitFailedEvent into the resulting
// FailedState verbatim. Without this assertion, a regression that
// constructed FailedState{Reason: evt.Reason} (omitting evt.Code)
// would silently downgrade every typed reject to
// RejectCodeUnspecified — exactly the failure mode the typed code was
// added to prevent. Table-driven over every defined RejectCode so
// future additions to the code space pick up the assertion
// automatically.
func TestAwaitingSubmitValidationRejectCodePropagation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		code RejectCode
	}{
		{
			name: "unspecified default",
			code: RejectCodeUnspecified,
		},
		{
			name: "lineage too large",
			code: RejectCodeLineageTooLarge,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()

			state := &AwaitingSubmitValidationState{}
			tr, err := state.ProcessEvent(
				ctx,
				&SubmitFailedEvent{
					Reason: "synthetic failure",
					Code:   tc.code,
				},
				&Environment{},
			)
			require.NoError(t, err)
			require.NotNil(t, tr)

			failed, ok := tr.NextState.(*FailedState)
			require.True(
				t, ok, "SubmitFailedEvent must transition "+
					"to FailedState, got %T", tr.NextState,
			)
			require.Equal(
				t, "synthetic failure", failed.Reason,
				"reason must round-trip verbatim",
			)
			require.Equal(
				t, tc.code, failed.Code, "typed reject "+
					"code must round-trip verbatim so "+
					"the actor's FailedState branch "+
					"can surface the same code on wire",
			)

			outbox := collectOutbox(t, tr)
			require.Empty(
				t, outbox, "a submit-validation failure "+
					"must not emit any outbox events: "+
					"the cap check runs before "+
					"LockInputsReq so there is no "+
					"phantom unlock",
			)
		})
	}
}
