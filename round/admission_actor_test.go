package round

import (
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/timeout"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestAdmissionActorTimeoutDelivery exercises the real timeout callback mapper,
// actor routing and FSM dispatch. A timer fires without an operator message,
// expires the accepted attempt, and cannot revive it on duplicate delivery.
func TestAdmissionActorTimeoutDelivery(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	h.setupMockRoundStoreForStart()
	require.NoError(t, h.start())
	clk := clock.NewTestClock(time.Unix(1_800_000_000, 0))
	h.actor.env.Now = clk.Now
	id := testRoundIDTr("actor-admission-timeout")
	require.NoError(
		t,
		h.actor.env.constrainAdmission(
			h.ctx, id, clk.Now().Add(time.Minute),
		),
	)
	h.injectRoundInState(id, &RoundJoinedState{RoundID: id})
	key := RoundKeyStr(id.KeyString())
	timerID := makeTimeoutID(key, TimeoutPhaseAdmission)
	require.NoError(
		t,
		h.actor.askEventAndProcessOutbox(
			h.ctx, h.actor.rounds[key], &AdmissionTimedOut{
				RoundID: id,
			},
		),
	)
	h.timeoutActor.assertTimeoutScheduled(t, timerID, admissionDeadlinePoll)

	clk.SetTime(clk.Now().Add(time.Minute))
	h.roundStore.On("FailRound", mock.Anything, id).Return(nil).Once()
	h.timeoutActor.mu.Lock()
	callback := h.timeoutActor.callbacks[timerID]
	h.timeoutActor.mu.Unlock()
	require.NotNil(t, callback)
	require.NoError(
		t,
		callback.Tell(
			h.ctx, &timeout.ExpiredMsg{
				ID: timerID,
			},
		),
	)
	msg, ok := h.selfRef.waitForMessage(time.Second)
	require.True(t, ok)
	_, err := h.receive(msg).Unpack()
	require.NoError(t, err)
	h.assertFSMState("ClientFailedState")

	_, err = h.receive(msg).Unpack()
	require.NoError(t, err)
	h.roundStore.AssertExpectations(t)
}
