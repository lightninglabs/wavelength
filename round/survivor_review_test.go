package round

import (
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/timeout"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestReplacementRefusalAppliesAbort keeps the operator's dead verdict even
// when a successor is unsupported or its ordinal is invalid.
func TestReplacementRefusalAppliesAbort(t *testing.T) {
	for _, quoted := range []bool{false, true} {
		h := newActorTestHarness(t)
		h.actor.env.AdmissionTimeout = 0
		id := testRoundID("refused")
		var state ClientState = &NoncesSentState{RoundID: id}
		if quoted {
			state = &QuoteReceivedState{RoundID: id}
		}
		h.injectRoundInState(id, state)
		t.Cleanup(func() { _ = h.actor.OnStop(t.Context()) })
		h.roundStore.
			On("FailRound", mock.Anything, id).
			Return(nil).
			Maybe()
		offer := &BoardingFailed{
			RoundID:            fn.Some(id),
			ReplacementRoundID: fn.Some(testRoundID("unused")),
			RetryAttempt:       2,
			Reason:             "operator abandoned attempt",
		}
		res := h.actor.handleServerMessage(
			t.Context(), &ServerMessageNotification{
				Message: offer,
			},
		)
		require.NoError(t, res.Err())
		current, err := fsmState(
			t.Context(),
			h.actor.rounds[RoundKeyStr(id.KeyString())].FSM,
		)
		require.NoError(t, err)
		require.IsType(t, &ClientFailedState{}, current)
	}
}

// TestReplacementRequiresFreshQuote refuses to advance on a commitment before
// the successor's quote, and evaluates its own operator fee cap.
func TestReplacementRequiresFreshQuote(t *testing.T) {
	h := newActorTestHarness(t)
	h.actor.env.AdmissionTimeout = 0
	old, next := testRoundID("quote-old"), testRoundID("quote-next")
	h.injectRoundInState(old, &NoncesSentState{RoundID: old})
	t.Cleanup(func() { _ = h.actor.OnStop(t.Context()) })
	require.NoError(
		t, h.actor.handleRoundReplacement(t.Context(),
			&BoardingFailed{
				RoundID:            fn.Some(old),
				ReplacementRoundID: fn.Some(next),
				RetryAttempt:       1,
			}).Err(),
	)
	fresh := h.actor.rounds[RoundKeyStr(next.KeyString())]
	_, err := fresh.FSM.AskEvent(t.Context(),
		&CommitmentTxBuilt{RoundID: next}).Await(t.Context()).Unpack()
	require.NoError(t, err)
	state, err := fsmState(t.Context(), fresh.FSM)
	require.NoError(t, err)
	require.IsType(t, &IntentSentState{}, state)
	quote := newQuoteReceivedTestState(
		int64(h.actor.cfg.MaxOperatorFee)+1, 0,
	).
		Quote
	quote.SealPass = 1
	quote.QuoteExpiresAt = time.Now().Add(time.Minute).Unix()
	events, err := fresh.FSM.AskEvent(t.Context(),
		&JoinRoundQuoteReceived{RoundID: next, Quote: quote}).
		Await(t.Context()).Unpack()
	require.NoError(t, err)
	state, err = fsmState(t.Context(), fresh.FSM)
	require.NoError(t, err)
	require.IsType(t, &ClientFailedState{}, state)
	var rejected bool
	failed, ok := state.(*ClientFailedState)
	require.True(t, ok)
	require.Contains(t, failed.Reason, "exceeds cap")
	for _, event := range events {
		if _, ok := event.(*JoinRoundRejectOutbox); ok {
			rejected = true
		}
	}
	require.True(t, rejected)
}

// TestReplacementCannotRenewMissingBudget refuses a restored attempt without
// its original monotonic deadline instead of granting a new window.
func TestReplacementCannotRenewMissingBudget(t *testing.T) {
	h := newActorTestHarness(t)
	h.actor.env.AdmissionTimeout = time.Minute
	old, next := testRoundID("restored"), testRoundID("renewed")
	h.injectRoundInState(old, &NoncesSentState{RoundID: old})
	t.Cleanup(func() { _ = h.actor.OnStop(t.Context()) })
	result := h.actor.handleRoundReplacement(t.Context(), &BoardingFailed{
		RoundID:            fn.Some(old),
		ReplacementRoundID: fn.Some(next),
		RetryAttempt:       1,
	})
	require.ErrorContains(t, result.Err(), "original admission budget")
	require.NotContains(t, h.actor.rounds, RoundKeyStr(next.KeyString()))
}

// TestReplacementCleanupExhaustionAppliesAbort delivers the original failure
// when the bounded local checkpoint cleanup can no longer be retried.
func TestReplacementCleanupExhaustionAppliesAbort(t *testing.T) {
	h := newActorTestHarness(t)
	h.actor.env.AdmissionTimeout = 0
	old, next := testRoundID("exhausted"), testRoundID("not-started")
	h.injectRoundInState(old, &InputSigSentState{RoundID: old})
	t.Cleanup(func() { _ = h.actor.OnStop(t.Context()) })
	tracked := h.actor.rounds[RoundKeyStr(old.KeyString())]
	tracked.ReplacementFailures = 5
	h.roundStore.On("FailRound", mock.Anything, old).
		Return(errors.New("store unavailable")).Once()
	result := h.actor.handleServerMessage(t.Context(),
		&ServerMessageNotification{Message: &BoardingFailed{
			RoundID:            fn.Some(old),
			ReplacementRoundID: fn.Some(next),
			RetryAttempt:       1,
			Reason:             "operator abandoned attempt",
		}})
	require.NoError(t, result.Err())
	state, err := fsmState(t.Context(), tracked.FSM)
	require.NoError(t, err)
	require.IsType(t, &ClientFailedState{}, state)
	require.Nil(t, tracked.PendingReplacement)
	require.NotContains(t, h.actor.rounds, RoundKeyStr(next.KeyString()))
}

// TestReplacementRetiresConfirmationRoute removes a dead checkpoint's txid
// route while keeping its boarding inputs owned by the successor.
func TestReplacementRetiresConfirmationRoute(t *testing.T) {
	h := newActorTestHarness(t)
	h.actor.env.AdmissionTimeout = 0
	old, next := testRoundID("checkpoint-old"), testRoundID(
		"checkpoint-next",
	)
	op := wire.OutPoint{Index: 123}
	intent := BoardingIntent{
		BoardingIntent: wallet.BoardingIntent{
			Outpoint: op,
		},
	}
	h.injectRoundInState(old, &InputSigSentState{RoundID: old,
		Intents: Intents{Boarding: []BoardingIntent{intent}}})
	t.Cleanup(func() { _ = h.actor.OnStop(t.Context()) })
	tracked := h.actor.rounds[RoundKeyStr(old.KeyString())]
	tracked.TxID = chainhash.HashH([]byte("abandoned commitment"))
	h.actor.commitmentTxIndex[tracked.TxID] = RoundKeyStr(old.KeyString())
	h.roundStore.On("FailRound", mock.Anything, old).Return(nil).Once()
	require.NoError(
		t, h.actor.handleRoundReplacement(t.Context(),
			&BoardingFailed{
				RoundID:            fn.Some(old),
				ReplacementRoundID: fn.Some(next),
				RetryAttempt:       1,
			}).Err(),
	)
	require.NotContains(t, h.actor.commitmentTxIndex, tracked.TxID)
	require.ErrorIs(
		t,
		h.actor.checkBoardingInputsAvailable(
			t.Context(), []BoardingIntent{intent},
		),
		ErrBoardingInFlight,
	)
	other := BoardingIntent{BoardingIntent: wallet.BoardingIntent{
		Outpoint: wire.OutPoint{
			Index: 124,
		}}}
	require.NoError(
		t,
		h.actor.checkBoardingInputsAvailable(
			t.Context(), []BoardingIntent{other},
		),
	)
}

// TestReplacementQuoteEffectsRetry keeps a failed accept delivery after the
// quote has already advanced the FSM, then resumes without replaying it.
func TestReplacementQuoteEffectsRetry(t *testing.T) {
	h := newActorTestHarness(t)
	id := testRoundID("accept-retry")
	h.injectRoundInState(id, &RoundJoinedState{RoundID: id})
	t.Cleanup(func() { _ = h.actor.OnStop(t.Context()) })
	fresh := h.actor.rounds[RoundKeyStr(id.KeyString())]
	fresh.ReplacementQuoteOutbox = []ClientOutMsg{
		&JoinRoundAcceptOutbox{
			RoundID: id,
		},
	}
	original := h.actor.cfg.ServerConn
	h.actor.cfg.ServerConn = &failedQuoteRejectRef{
		mockServerConnRef: h.serverConn, err: errors.New(
			"mailbox unavailable",
		)}
	require.Error(t, h.actor.deliverReplacementQuote(t.Context(), id).Err())
	require.Len(t, fresh.ReplacementQuoteOutbox, 1)
	h.actor.cfg.ServerConn = original
	timerID := timeout.ID("replacement-quote:" + id.String())
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
	message, ok := h.selfRef.waitForMessage(time.Second)
	require.True(t, ok)
	require.NoError(t, h.receive(message).Err())
	require.Empty(t, fresh.ReplacementQuoteOutbox)
}

// TestReplacementBeforeAdmissionAckAppliesAbort handles an offer delivered
// before the acknowledgement that assigns its round ID.
func TestReplacementBeforeAdmissionAckAppliesAbort(t *testing.T) {
	h := newActorTestHarness(t)
	h.actor.env.AdmissionTimeout = 0
	pending, err := h.actor.createRoundWithState(
		t.Context(), &IntentSentState{}, fn.None[RoundID](), nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.actor.OnStop(t.Context()) })
	old := testRoundID("ack-not-received")
	h.roundStore.On("FailRound", mock.Anything, old).Return(nil).Maybe()
	result := h.actor.handleServerMessage(t.Context(),
		&ServerMessageNotification{Message: &BoardingFailed{
			RoundID: fn.Some(old),
			ReplacementRoundID: fn.Some(
				testRoundID("unused-successor"),
			),
			RetryAttempt: 1,
		}})
	require.NoError(t, result.Err())
	state, err := fsmState(t.Context(), pending.FSM)
	require.NoError(t, err)
	require.IsType(t, &IntentSentState{}, state)
	require.NoError(
		t, h.actor.handleRoundJoined(t.Context(),
			&RoundJoined{RoundID: old}).Err(),
	)
	state, err = fsmState(t.Context(), pending.FSM)
	require.NoError(t, err)
	require.IsType(t, &ClientFailedState{}, state)
}

// TestStaleReplacementCannotAbortPendingJoin preserves an unrelated join when
// a completed predecessor's offer is redelivered after its lineage is gone.
func TestStaleReplacementCannotAbortPendingJoin(t *testing.T) {
	h := newActorTestHarness(t)
	h.actor.env.AdmissionTimeout = 0
	pending, err := h.actor.createRoundWithState(
		t.Context(), &IntentSentState{}, fn.None[RoundID](), nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.actor.OnStop(t.Context()) })
	stale, own := testRoundID("retired-A"), testRoundID("new-B")
	result := h.actor.handleServerMessage(t.Context(),
		&ServerMessageNotification{Message: &BoardingFailed{
			RoundID: fn.Some(stale),
			ReplacementRoundID: fn.Some(
				testRoundID("retired-A-successor"),
			),
			RetryAttempt: 1,
		}})
	require.NoError(t, result.Err())
	require.NoError(
		t, h.actor.handleRoundJoined(t.Context(),
			&RoundJoined{RoundID: own}).Err(),
	)
	state, err := fsmState(t.Context(), pending.FSM)
	require.NoError(t, err)
	sent, ok := state.(*IntentSentState)
	require.True(t, ok)
	require.Equal(t, own, sent.AdmittedRoundID)
	require.Empty(t, h.actor.pendingReplacementOffers)
}

// TestPendingReplacementOffersBounded limits storage for unmatched offers.
func TestPendingReplacementOffersBounded(t *testing.T) {
	h := newActorTestHarness(t)
	h.actor.env.AdmissionTimeout = 0
	_, err := h.actor.createRoundWithState(
		t.Context(), &IntentSentState{}, fn.None[RoundID](), nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.actor.OnStop(t.Context()) })
	for i := 0; i <= maxPendingQuotes; i++ {
		id, err := NewRoundID()
		require.NoError(t, err)
		result := h.actor.bufferReplacementOffer(&BoardingFailed{
			RoundID:            fn.Some(id),
			ReplacementRoundID: fn.Some(testRoundID("successor")),
			RetryAttempt:       1,
		})
		if i == maxPendingQuotes {
			require.ErrorContains(t, result.Err(), "limit reached")
		} else {
			require.NoError(t, result.Err())
		}
	}
	require.Len(t, h.actor.pendingReplacementOffers, maxPendingQuotes)
}
