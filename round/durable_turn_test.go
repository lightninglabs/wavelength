package round

import (
	"context"
	"errors"
	"testing"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// turnTestTransactionKey exposes the transaction-local domain store to the
// test.
type turnTestTransactionKey struct{}

// turnTestDatabase models commit visibility for the runtime execution contract.
type turnTestDatabase struct {
	actor.DeliveryStore
	checkpoint     actor.CheckpointParams
	effects        []actor.OutboxParams
	domain         []RoundID
	failCheckpoint bool
}

// SaveCheckpoint records into the transaction-local snapshot.
func (s *turnTestDatabase) SaveCheckpoint(_ context.Context,
	checkpoint actor.CheckpointParams) error {

	if s.failCheckpoint {
		return errors.New("checkpoint failed")
	}
	s.checkpoint = checkpoint

	return nil
}

// EnqueueOutbox records effects without exposing them before commit.
func (s *turnTestDatabase) EnqueueOutbox(_ context.Context,
	effect actor.OutboxParams) error {

	s.effects = append(s.effects, effect)

	return nil
}

// turnTestExec supplies isolated transaction copies and models the fenced ack.
type turnTestExec struct {
	database   turnTestDatabase
	acked      bool
	lostLease  bool
	failCommit bool
}

// transaction applies a closure only when it succeeds under the lease fence.
func (e *turnTestExec) transaction(ctx context.Context,
	fn func(context.Context, clientDurableTx) error) error {

	if e.lostLease {
		return actor.ErrLeaseLost
	}
	next := e.database
	next.effects = append([]actor.OutboxParams(nil), next.effects...)
	next.domain = append([]RoundID(nil), next.domain...)
	ctx = context.WithValue(ctx, turnTestTransactionKey{}, &next)
	if err := fn(ctx, clientDurableTx{delivery: &next}); err != nil {
		return err
	}
	e.database = next

	return nil
}

// Read invokes the same store contract without marking delivery acknowledged.
func (e *turnTestExec) Read(ctx context.Context,
	fn func(context.Context, clientDurableTx) error) error {

	return fn(ctx, clientDurableTx{delivery: &e.database})
}

// Stage persists an in-flight checkpoint without acknowledging the command.
func (e *turnTestExec) Stage(ctx context.Context,
	fn func(context.Context, clientDurableTx) error) error {

	return e.transaction(ctx, fn)
}

// Commit records the acknowledgement only after the entire closure succeeds.
func (e *turnTestExec) Commit(ctx context.Context,
	fn func(context.Context, clientDurableTx) error) error {

	if err := e.transaction(ctx, func(txCtx context.Context,
		tx clientDurableTx) error {

		store, ok := tx.delivery.(*turnTestDatabase)
		if !ok {
			return errors.New("unexpected transaction store")
		}
		store.failCheckpoint = e.failCommit

		return fn(txCtx, tx)
	}); err != nil {
		return err
	}
	e.acked = true

	return nil
}

// TestDurableTurnCommit groups domain writes, effects, checkpoint and ack.
func TestDurableTurnCommit(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "commit"
		if fail {
			name = "rollback"
		}
		t.Run(name, func(t *testing.T) {
			h := newActorTestHarness(t)
			turn := &durableClientTurn{
				owner: h.actor, actorID: "round", version: 1,
				codec: newDurableClientCodec(),
			}
			exec := &turnTestExec{}
			command, err := durableClientIngress(
				&GetClientStateRequest{},
			)
			require.NoError(t, err)
			require.NoError(
				t,
				turn.stage(
					t.Context(), command, exec,
				),
			)
			require.False(t, exec.acked)
			staged := exec.database.checkpoint
			envelope, err := decodeDurableClientCheckpoint(
				staged.StateData, turn.codec,
			)
			require.NoError(t, err)
			require.NotEmpty(t, envelope.InFlight)
			unbind, err := turn.bind()
			require.NoError(t, err)
			defer unbind()
			id := RoundID{1}
			expectation := h.roundStore.On(
				"FailRound", mock.Anything, id,
			)
			expectation.Run(func(args mock.Arguments) {
				ctx, ok := args.Get(0).(context.Context)
				require.True(t, ok)
				transaction, ok := ctx.Value(
					turnTestTransactionKey{},
				).(*turnTestDatabase)
				require.True(t, ok)
				transaction.domain = append(
					transaction.domain, id,
				)
			}).Return(nil).Once()
			require.NoError(
				t,
				h.actor.cfg.RoundStore.FailRound(
					t.Context(), id,
				),
			)
			cancellation := &CancelTimeoutReq{
				RoundKey: "temp:key",
				Phase:    TimeoutPhaseRegistration,
			}
			err = h.actor.processOutbox(
				t.Context(), []ClientOutMsg{cancellation},
			)
			require.NoError(t, err)
			require.Empty(t, exec.database.domain)
			require.Empty(t, exec.database.effects)
			h.roundStore.AssertNotCalled(
				t, "FailRound", mock.Anything, id,
			)
			exec.failCommit = fail
			err = turn.commit(t.Context(), exec)
			if fail {
				require.ErrorContains(
					t, err, "checkpoint failed",
				)
				require.False(t, exec.acked)
				require.Empty(t, exec.database.domain)
				require.Empty(t, exec.database.effects)
				require.Equal(
					t, staged, exec.database.checkpoint,
				)
			} else {
				require.NoError(t, err)
				require.True(t, exec.acked)
				require.Equal(
					t, []RoundID{id}, exec.database.domain,
				)
				require.Len(t, exec.database.effects, 1)
				envelope, err = decodeDurableClientCheckpoint(
					exec.database.checkpoint.StateData,
					turn.codec,
				)
				require.NoError(t, err)
				require.Empty(t, envelope.InFlight)
			}
			h.roundStore.AssertExpectations(t)
		})
	}
}
