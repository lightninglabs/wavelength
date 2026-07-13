package chainsource

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/stretchr/testify/require"
)

// flakyArmBackend fails RegisterBlocks the first failCount times, then
// succeeds. It models a backend that is briefly unavailable at the exact
// moment finality arming is attempted (the failure mode that previously
// stranded a watch once the bounded retry schedule was exhausted).
type flakyArmBackend struct {
	*mockBackend

	mu       sync.Mutex
	failLeft int
	attempts int
}

// RegisterBlocks fails until failLeft is drained, then returns a live
// registration.
func (b *flakyArmBackend) RegisterBlocks(ctx context.Context) (
	*BlockRegistration, error) {

	b.mu.Lock()
	defer b.mu.Unlock()

	b.attempts++
	if b.failLeft > 0 {
		b.failLeft--

		return nil, errors.New("backend temporarily unavailable")
	}

	return &BlockRegistration{
		Epochs: make(chan *BlockEpoch, 1),
		Cancel: func() {},
	}, nil
}

// attemptCount returns how many RegisterBlocks calls have been made.
func (b *flakyArmBackend) attemptCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.attempts
}

// TestRegisterBlocksForFinalityRetriesUntilArmed asserts that finality arming
// retries past the old fixed three-attempt cap and eventually arms once a
// transiently-unavailable backend recovers. This is the reorg-safety
// robustness property: for gRPC lndclient / lwwallet, height synthesis is the
// only Done source, so abandoning the arm would strand the round/exit in
// provisional until a daemon restart. failLeft is five (past both the old cap
// and finalityArmEscalateAfter) so the escalation-warning branch is exercised.
func TestRegisterBlocksForFinalityRetriesUntilArmed(t *testing.T) {
	t.Parallel()

	backend := &flakyArmBackend{
		mockBackend: newMockBackend(),
		failLeft:    5,
	}

	reg, err := registerBlocksForFinality(
		context.Background(), backend, btclog.Disabled,
	)
	require.NoError(t, err, "arming must succeed once the backend recovers")
	require.NotNil(t, reg, "a live block registration must be returned")
	require.NotNil(t, reg.Epochs)
	require.GreaterOrEqual(
		t, backend.attemptCount(), 6,
		"arming must retry past the old three-attempt cap",
	)
}

// TestRegisterBlocksForFinalityStopsOnContextCancel asserts that the otherwise
// unbounded arming retry exits promptly when the watch's context is cancelled
// (daemon shutdown), returning the context error rather than looping forever.
func TestRegisterBlocksForFinalityStopsOnContextCancel(t *testing.T) {
	t.Parallel()

	// A backend that never recovers, so arming would loop indefinitely
	// were it not bounded by the context.
	backend := &flakyArmBackend{
		mockBackend: newMockBackend(),
		failLeft:    1 << 30,
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	reg, err := registerBlocksForFinality(ctx, backend, btclog.Disabled)
	require.Error(
		t, err, "arming must return once the context is cancelled",
	)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, reg)
}
