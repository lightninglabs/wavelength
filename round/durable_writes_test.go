package round

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// durableWriteContextKey identifies a transaction context in the test store.
type durableWriteContextKey struct{}

// TestDurableWritesDeferToCommitContext checks the boundary before persistence.
func TestDurableWritesDeferToCommitContext(t *testing.T) {
	store := &MockRoundStore{}
	turn := &clientTurnWrites{}
	buffered := &bufferedRoundStore{RoundStore: store, turn: turn}
	id := RoundID{7}
	requestCtx, cancel := context.WithCancel(t.Context())
	require.NoError(t, buffered.FailRound(requestCtx, id))
	cancel()
	store.AssertNotCalled(t, "FailRound", mock.Anything, mock.Anything)
	transactionCtx := context.WithValue(
		t.Context(), durableWriteContextKey{}, 123,
	)
	store.On("FailRound", mock.MatchedBy(func(ctx context.Context) bool {
		value := ctx.Value(durableWriteContextKey{})

		return ctx.Err() == nil && value == 123
	}), id).Return(nil).Once()
	require.NoError(t, turn.apply(transactionCtx))
	store.AssertExpectations(t)
}

// TestDurableWritesStopOnError leaves rollback ownership with the runtime.
func TestDurableWritesStopOnError(t *testing.T) {
	store := &MockRoundStore{}
	turn := &clientTurnWrites{}
	buffered := &bufferedRoundStore{RoundStore: store, turn: turn}
	first, second := RoundID{1}, RoundID{2}
	require.NoError(t, buffered.FailRound(t.Context(), first))
	require.NoError(t, buffered.FailRound(t.Context(), second))
	failed := errors.New("domain write failed")
	store.On("FailRound", mock.Anything, first).Return(failed).Once()
	require.ErrorIs(t, turn.apply(t.Context()), failed)
	store.AssertNotCalled(t, "FailRound", mock.Anything, second)
	store.AssertExpectations(t)
}

// TestDurableWritesFreezeInputSelection prevents later buffer reuse changing
// jobs.
func TestDurableWritesFreezeInputSelection(t *testing.T) {
	store := &MockRoundStore{}
	turn := &clientTurnWrites{}
	buffered := &bufferedRoundStore{RoundStore: store, turn: turn}
	points := []wire.OutPoint{{Index: 7}}
	require.NoError(
		t,
		buffered.FailForfeitIntents(
			t.Context(), points, "failed", RoundFailureCode(1),
		),
	)
	points[0].Index = 8
	store.On("FailForfeitIntents", mock.Anything,
		[]wire.OutPoint{{Index: 7}}, "failed", RoundFailureCode(1),
	).Return(nil).Once()
	require.NoError(t, turn.apply(t.Context()))
	store.AssertExpectations(t)
}
