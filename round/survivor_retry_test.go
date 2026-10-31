package round

import (
	"errors"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/timeout"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestRoundReplacementRetainsIntentAndFencesOldEvents checks fresh IDs,
// sequential bounds, replay and reservation lineage without another join.
func TestRoundReplacementRetainsIntentAndFencesOldEvents(t *testing.T) {
	t.Parallel()
	h := newActorTestHarness(t)
	oldID, newID := testRoundID("old"), testRoundID("new")
	original := Intents{VTXOs: []types.VTXORequest{{Amount: 1234}}}
	h.injectRoundInState(
		oldID, &NoncesSentState{
			RoundID: oldID,
			Intents: original,
		},
	)
	t.Cleanup(func() { _ = h.actor.OnStop(t.Context()) })
	retry := &BoardingFailed{RoundID: fn.Some(oldID),
		ReplacementRoundID: fn.Some(newID), RetryAttempt: 1}
	_, err := h.actor.handleRoundReplacement(t.Context(), retry).Unpack()
	require.NoError(t, err)
	fresh := h.actor.rounds[RoundKeyStr(newID.KeyString())]
	require.NotNil(t, fresh)
	state, err := fsmState(t.Context(), fresh.FSM)
	require.NoError(t, err)
	joined, ok := state.(*RoundJoinedState)
	require.True(t, ok)
	require.Equal(t, original, joined.Intents)
	require.Nil(t, joined.Quote)
	require.Equal(
		t, []string{oldID.String()},
		h.actor.replacedRoundStrings(
			newID.String(),
		),
	)
	require.True(t, h.actor.isReplacedRound(oldID))
	// A replay is acknowledged without consuming another budget slot.
	_, err = h.actor.handleRoundReplacement(t.Context(), retry).Unpack()
	require.NoError(t, err)
	require.Equal(t, uint32(1), fresh.RetryAttempt)
	// Reusing a round ID, skipping an ordinal or exceeding the cap is
	// refused.
	for _, ordinal := range []uint32{1, 3, 11} {
		invalid := &BoardingFailed{
			RoundID:            fn.Some(newID),
			ReplacementRoundID: fn.Some(testRoundID("next")),
			RetryAttempt:       ordinal,
		}
		_, err := h.actor.
			handleRoundReplacement(t.Context(), invalid).
			Unpack()
		require.Error(t, err)
	}
	late := &ServerMessageNotification{
		Message: &NoncesAggregated{
			RoundID: oldID,
		},
	}
	_, err = h.actor.handleServerMessage(t.Context(), late).Unpack()
	require.NoError(
		t, err, "abandoned attempts cannot reach a fresh signer",
	)
}

// TestRoundReplacementRetirementFailure refuses to sign a replacement until
// the abandoned input-signature checkpoint is retired durably.
func TestRoundReplacementRetirementFailure(t *testing.T) {
	t.Parallel()
	h := newActorTestHarness(t)
	oldID, newID := testRoundID("checkpoint"), testRoundID("replacement")
	h.setupRoundInInputSigSentState(oldID)
	t.Cleanup(func() { _ = h.actor.OnStop(t.Context()) })
	storageErr := errors.New("db unavailable")
	h.roundStore.On("FailRound", mock.Anything, oldID).Return(storageErr)
	retry := &BoardingFailed{
		RoundID:            fn.Some(oldID),
		ReplacementRoundID: fn.Some(newID),
		RetryAttempt:       1,
	}
	_, err := h.actor.handleRoundReplacement(t.Context(), retry).Unpack()

	require.ErrorContains(t, err, "retire abandoned checkpoint")
	require.Contains(t, h.actor.rounds, RoundKeyStr(oldID.KeyString()))
	require.NotContains(t, h.actor.rounds, RoundKeyStr(newID.KeyString()))
}

// TestRoundReplacementPreservesDeadline prevents a succession of replacement
// round IDs from renewing an accepted operation's absolute or monotonic budget.
func TestRoundReplacementPreservesDeadline(t *testing.T) {
	t.Parallel()
	h := newActorTestHarness(t)
	clk := clock.NewTestClock(time.Unix(1_800_000_000, 0))
	h.actor.env.Now = clk.Now
	h.actor.env.AdmissionTimeout = time.Minute
	oldID, newID := testRoundID("budget-old"), testRoundID("budget-new")
	old, err := h.actor.createRoundWithState(
		t.Context(), &RoundJoinedState{
			RoundID: oldID,
		},
		fn.Some(oldID),
		nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.actor.OnStop(t.Context()) })
	initial := *old.env.admission
	clk.SetTime(clk.Now().Add(10 * time.Second))
	offer := &BoardingFailed{RoundID: fn.Some(oldID),
		ReplacementRoundID: fn.Some(newID), RetryAttempt: 1}
	_, err = h.actor.handleRoundReplacement(t.Context(), offer).Unpack()
	require.NoError(t, err)
	fresh := h.actor.rounds[RoundKeyStr(newID.KeyString())]
	require.NotNil(t, fresh)
	require.Equal(
		t, initial.deadline.ExpiresAt,
		fresh.env.admission.deadline.ExpiresAt,
	)
	require.Equal(t, initial.cutoff, fresh.env.admission.cutoff)
	h.timeoutActor.assertTimeoutScheduled(
		t,
		makeTimeoutID(
			RoundKeyStr(
				newID.KeyString(),
			),
			TimeoutPhaseAdmission,
		),
		admissionDeadlinePoll,
	)
}

// TestRoundReplacementCleanupReminder re-drives a transient checkpoint failure
// through the actual timeout mapper and the actor receive dispatch.
func TestRoundReplacementCleanupReminder(t *testing.T) {
	t.Parallel()
	h := newActorTestHarness(t)
	oldID, newID := testRoundID("retry-old"), testRoundID("retry-new")
	h.setupRoundInInputSigSentState(oldID)
	t.Cleanup(func() { _ = h.actor.OnStop(t.Context()) })
	h.roundStore.On("FailRound", mock.Anything, oldID).
		Return(errors.New("temporary database failure")).Once()
	h.roundStore.On("FailRound", mock.Anything, oldID).Return(nil).Once()
	offer := &BoardingFailed{RoundID: fn.Some(oldID),
		ReplacementRoundID: fn.Some(newID), RetryAttempt: 1}
	_, err := h.actor.handleRoundReplacement(t.Context(), offer).Unpack()
	require.Error(t, err)
	require.True(t, h.actor.isAbandonedAttempt(oldID))
	timerID := timeout.ID("round-replacement:" + oldID.String())
	h.timeoutActor.mu.Lock()
	callback := h.timeoutActor.callbacks[timerID]
	h.timeoutActor.mu.Unlock()
	require.NotNil(t, callback)
	require.NoError(
		t,
		callback.Tell(
			t.Context(), &timeout.ExpiredMsg{
				ID: timerID,
			},
		),
	)
	msg, ok := h.selfRef.waitForMessage(time.Second)
	require.True(t, ok)
	_, err = h.receive(msg).Unpack()
	require.NoError(t, err)
	require.Contains(t, h.actor.rounds, RoundKeyStr(newID.KeyString()))
	require.NotContains(t, h.actor.rounds, RoundKeyStr(oldID.KeyString()))
}
