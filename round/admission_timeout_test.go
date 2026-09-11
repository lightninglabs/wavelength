package round

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/stretchr/testify/require"
)

// acceptedStates covers every accepted pre-checkpoint state, including local
// intermediate states whose internal events can race deadline delivery.
func acceptedStates(id RoundID, intents Intents) []ClientState {
	return []ClientState{
		&IntentSentState{
			AdmittedRoundID: id,
			Intents:         intents,
		},
		&QuoteReceivedState{
			RoundID: id,
			Intents: intents,
		},
		&RoundJoinedState{
			RoundID: id,
			Intents: intents,
		},
		&CommitmentTxReceivedState{
			RoundID: id,
			Intents: intents,
		},
		&CommitmentTxValidatedState{
			RoundID: id,
			Intents: intents,
		},
		&NoncesSentState{
			RoundID: id,
			Intents: intents,
		},
		&NoncesAggregatedState{
			RoundID: id,
			Intents: intents,
		},
		&PartialSigsSentState{
			RoundID: id,
			Intents: intents,
		},
		&ForfeitSignaturesCollectingState{
			RoundID: id,
			Intents: intents,
		},
	}
}

// deadlineTestEnv starts a real deadline contract against the shared store
// double. The caller owns clock advancement between synchronous FSM events.
func deadlineTestEnv(t *testing.T, id RoundID,
	now *time.Time) *ClientEnvironment {

	t.Helper()

	env := &ClientEnvironment{
		RoundStore:       &MockRoundStore{},
		Log:              btclog.Disabled,
		AdmissionTimeout: time.Minute,
		Now: func() time.Time {
			return *now
		},
	}
	require.NoError(
		t,
		env.constrainAdmission(
			t.Context(), id, now.Add(time.Minute),
		),
	)

	return env
}

// TestAdmissionSilenceAtEveryState proves operator silence expires without
// another operator event and uses the existing safe reservation release path.
// Early, duplicate and foreign wakeups cannot release an active reservation.
func TestAdmissionSilenceAtEveryState(t *testing.T) {
	t.Parallel()

	id := testRoundIDTr("silent-admission")
	op := regTimeoutOutpoint(0x61, 0)
	intents := Intents{
		Forfeits: []types.ForfeitRequest{
			mkForfeit(op, 10_000),
		},
	}
	for _, state := range acceptedStates(id, intents) {
		t.Run(state.String(), func(t *testing.T) {
			now := time.Unix(1_800_000_000, 0)
			env := deadlineTestEnv(t, id, &now)
			wake := &AdmissionTimedOut{RoundID: id}
			early, err := state.ProcessEvent(t.Context(), wake, env)
			require.NoError(t, err)
			require.Same(t, state, early.NextState)
			earlyOut := early.NewEvents.UnwrapOr(
				ClientEmittedEvent{},
			)
			arm, ok := findOutbox[*StartTimeoutReq](earlyOut.Outbox)
			require.True(t, ok)
			require.Equal(t, TimeoutPhaseAdmission, arm.Phase)

			now = now.Add(time.Minute)
			foreign, err := state.ProcessEvent(
				t.Context(), &AdmissionTimedOut{
					RoundID: testRoundIDTr("foreign"),
				},
				env,
			)
			require.NoError(t, err)
			require.Same(t, state, foreign.NextState)

			tr, err := state.ProcessEvent(t.Context(), wake, env)
			require.NoError(t, err)
			require.IsType(t, &ClientFailedState{}, tr.NextState)
			emitted := tr.NewEvents.UnwrapOr(ClientEmittedEvent{})
			release, ok := findOutbox[*ReleaseForfeitReservation](
				emitted.Outbox,
			)
			require.True(t, ok)
			require.Equal(t, []wire.OutPoint{op}, release.Outpoints)

			// Both timeout replay and a late operator message stay
			// terminal.
			for _, late := range []ClientEvent{
				wake,
				&OperatorSigned{
					RoundID: id,
				},
			} {
				replay, err := tr.NextState.ProcessEvent(
					t.Context(), late, env,
				)
				require.NoError(t, err)
				require.Same(t, tr.NextState, replay.NextState)
				out := replay.NewEvents.UnwrapOr(
					ClientEmittedEvent{},
				)
				require.Empty(t, out.Outbox)
			}
		})
	}
}

// TestAdmissionDeadlinePreemptsLateMessage proves a message arriving ahead of
// the timer cannot advance an already expired attempt into signing.
func TestAdmissionDeadlinePreemptsLateMessage(t *testing.T) {
	t.Parallel()

	id := testRoundIDTr("late-admission-message")
	now := time.Unix(1_800_000_000, 0)
	env := deadlineTestEnv(t, id, &now)
	now = now.Add(2 * time.Minute)
	for _, state := range acceptedStates(id, Intents{}) {
		tr, err := state.ProcessEvent(
			t.Context(), &OperatorSigned{
				RoundID: id,
			},
			env,
		)
		require.NoError(t, err)
		require.IsType(t, &ClientFailedState{}, tr.NextState)
	}
}

// TestAdmissionTimeoutCannotReleaseCheckpoint pins the point of no return:
// even an overdue or closed admission does not release checkpointed inputs.
func TestAdmissionTimeoutCannotReleaseCheckpoint(t *testing.T) {
	t.Parallel()

	id := testRoundIDTr("checkpointed-admission")
	now := time.Unix(1_800_000_000, 0)
	env := deadlineTestEnv(t, id, &now)
	now = now.Add(time.Hour)
	state := &InputSigSentState{RoundID: id}
	tr, err := state.ProcessEvent(
		t.Context(), &AdmissionTimedOut{
			RoundID: id,
		},
		env,
	)
	require.NoError(t, err)
	require.Same(t, state, tr.NextState)
	require.Empty(t, tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox)
}

// failedAdmissionStore injects a failure at the persistence boundary, before
// any accepted state or operator response can depend on an unrecorded budget.
type failedAdmissionStore struct {
	RoundStore
}

// ConstrainAdmissionDeadline refuses the admission write for failure testing.
func (s *failedAdmissionStore) ConstrainAdmissionDeadline(context.Context,
	RoundID, time.Time) (AdmissionDeadline, error) {

	return AdmissionDeadline{}, errors.New("deadline write failed")
}

// TestAdmissionPersistenceBeforeAcceptance proves save failure safely fails
// the attempt and does not emit a quote acceptance or advance its state.
func TestAdmissionPersistenceBeforeAcceptance(t *testing.T) {
	t.Parallel()

	id := testRoundIDTr("failed-admission-write")
	op := regTimeoutOutpoint(0x62, 0)
	state := &IntentSentState{
		Intents: Intents{
			Forfeits: []types.ForfeitRequest{
				mkForfeit(op, 10_000),
			},
		},
	}
	env := &ClientEnvironment{
		RoundStore:       &failedAdmissionStore{},
		Log:              btclog.Disabled,
		AdmissionTimeout: time.Minute,
	}
	tr, err := state.ProcessEvent(
		t.Context(), &RoundJoined{
			RoundID: id,
		},
		env,
	)
	require.NoError(t, err)
	require.IsType(t, &ClientFailedState{}, tr.NextState)
	out := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox
	_, released := findOutbox[*ReleaseForfeitReservation](out)
	require.True(t, released)
	_, accepted := findOutbox[*JoinRoundAcceptOutbox](out)
	require.False(t, accepted)
}

// TestAdmissionAcceptedQuoteKeepsBudget proves accepting a short-lived quote
// preserves the full participation budget through a slow signing ceremony.
func TestAdmissionAcceptedQuoteKeepsBudget(t *testing.T) {
	t.Parallel()

	s := newQuoteReceivedTestState(5000, 0)
	now := time.Unix(1_800_000_000, 0)
	started := now
	env := &ClientEnvironment{
		RoundStore:       &MockRoundStore{},
		Log:              btclog.Disabled,
		AdmissionTimeout: defaultAdmissionTimeout,
		Now:              func() time.Time { return now },
	}
	_, err := (&IntentSentState{}).ProcessEvent(
		t.Context(), &RoundJoined{
			RoundID: s.RoundID,
		},
		env,
	)
	require.NoError(t, err)
	original := env.admission.deadline.ExpiresAt
	require.Equal(t, started.Add(30*time.Minute).Unix(), original.Unix())

	s.Quote.QuoteExpiresAt = now.Add(10 * time.Second).Unix()
	tr, err := s.ProcessEvent(t.Context(), &QuoteAccepted{
		RoundID: s.RoundID,
		QuoteID: s.Quote.QuoteID,
	}, env)
	require.NoError(t, err)
	require.IsType(t, &RoundJoinedState{}, tr.NextState)
	require.Equal(t, original, env.admission.deadline.ExpiresAt)

	// Crossing both quote expiry and the old five-minute default must
	// leave the accepted signing attempt active.
	now = started.Add(6 * time.Minute)
	wake := &AdmissionTimedOut{RoundID: s.RoundID}
	active, err := tr.NextState.ProcessEvent(t.Context(), wake, env)
	require.NoError(t, err)
	require.Same(t, tr.NextState, active.NextState)
	out := active.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox
	_, released := findOutbox[*ReleaseForfeitReservation](out)
	require.False(t, released)

	// The longer default remains a hard limit on operator silence.
	now = started.Add(30 * time.Minute)
	expired, err := active.NextState.ProcessEvent(t.Context(), wake, env)
	require.NoError(t, err)
	require.IsType(t, &ClientFailedState{}, expired.NextState)
}

// TestAdmissionPendingQuoteExpires rejects an acceptance decision delayed
// past quote expiry without changing the attempt's durable signing deadline.
func TestAdmissionPendingQuoteExpires(t *testing.T) {
	t.Parallel()

	s := newQuoteReceivedTestState(5000, 0)
	now := time.Unix(1_800_000_000, 0)
	env := deadlineTestEnv(t, s.RoundID, &now)
	original := env.admission.deadline.ExpiresAt
	s.Quote.QuoteExpiresAt = now.Add(10 * time.Second).Unix()
	now = now.Add(10 * time.Second)
	tr, err := s.ProcessEvent(t.Context(), &QuoteAccepted{
		RoundID: s.RoundID,
		QuoteID: s.Quote.QuoteID,
	}, env)
	require.NoError(t, err)
	require.IsType(t, &ClientFailedState{}, tr.NextState)
	out := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox
	_, accepted := findOutbox[*JoinRoundAcceptOutbox](out)
	require.False(t, accepted)
	require.Equal(t, original, env.admission.deadline.ExpiresAt)
}

// TestAdmissionFarFutureQuote proves a remote quote expiry never changes
// the locally derived deadline, even beyond the storage timestamp range.
func TestAdmissionFarFutureQuote(t *testing.T) {
	t.Parallel()

	s := newQuoteReceivedTestState(5000, 0)
	now := time.Unix(1_800_000_000, 0)
	env := deadlineTestEnv(t, s.RoundID, &now)
	original := env.admission.deadline.ExpiresAt
	s.Quote.QuoteExpiresAt = 9_300_000_000
	tr, err := s.ProcessEvent(t.Context(), &QuoteAccepted{
		RoundID: s.RoundID,
		QuoteID: s.Quote.QuoteID,
	}, env)
	require.NoError(t, err)
	require.IsType(t, &RoundJoinedState{}, tr.NextState)
	require.Equal(t, original, env.admission.deadline.ExpiresAt)
}

// TestAdmissionClockBudgets proves either clock can expire an attempt, while
// repeated persistence cannot renew the process-local cutoff after a rewind.
func TestAdmissionClockBudgets(t *testing.T) {
	t.Parallel()

	id := testRoundIDTr("admission-clock")
	now := time.Now()
	env := deadlineTestEnv(t, id, &now)
	originalCutoff := env.admission.cutoff

	// Simulate wall time before admission; the saved and process-local
	// cutoffs remain the original ones when a replay proposes more time.
	now = now.Add(-time.Hour)
	require.NoError(
		t,
		env.constrainAdmission(
			t.Context(), id, originalCutoff.Add(time.Hour),
		),
	)
	require.Equal(t, originalCutoff, env.admission.cutoff)

	// A monotonic cutoff can expire even while absolute expiry is future.
	budget := &admissionBudget{
		deadline: AdmissionDeadline{
			ExpiresAt: now.Add(time.Hour),
		},
		cutoff: now.Add(-time.Second),
	}
	require.True(t, budget.expired(now))

	// A forward wall correction can expire before monotonic time does.
	budget.deadline.ExpiresAt = now.Add(-time.Second)
	budget.cutoff = now.Add(time.Hour)
	require.True(t, budget.expired(now))
}

// TestAdmissionArmsBeforeCancel proves acceptance persists and schedules the
// replacement timer before the fallible cancellation of registration.
func TestAdmissionArmsBeforeCancel(t *testing.T) {
	t.Parallel()

	id := testRoundIDTr("admission-arm-order")
	store := &MockRoundStore{}
	env := &ClientEnvironment{
		RoundStore:       store,
		Log:              btclog.Disabled,
		AdmissionTimeout: time.Minute,
	}
	state := &IntentSentState{}
	tr, err := state.ProcessEvent(
		t.Context(), &RoundJoined{
			RoundID: id,
		},
		env,
	)
	require.NoError(t, err)
	require.IsType(t, &IntentSentState{}, tr.NextState)
	out := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox
	require.Len(t, out, 2)
	require.IsType(t, &StartTimeoutReq{}, out[0])
	require.IsType(t, &CancelTimeoutReq{}, out[1])
	deadline, ok := store.deadlines[id]
	require.True(t, ok)
	require.False(t, deadline.Closed)
}
