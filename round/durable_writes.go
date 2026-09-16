package round

import (
	"context"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

// clientTurnWrites holds domain changes until the durable actor commits its
// checkpoint, outbox, and delivery acknowledgement in the same transaction.
// Closures receive the commit context; they never retain a request context.
type clientTurnWrites struct {
	writes []func(context.Context) error
}

// apply joins every domain write to the transaction supplied by the runtime.
func (w *clientTurnWrites) apply(ctx context.Context) error {
	for _, write := range w.writes {
		if err := write(ctx); err != nil {
			return err
		}
	}

	return nil
}

// bufferedRoundStore forwards reads and defers writes for a single actor turn.
type bufferedRoundStore struct {
	RoundStore
	turn *clientTurnWrites
}

// CommitState defers the domain checkpoint until effects are durably queued.
func (s *bufferedRoundStore) CommitState(_ context.Context, round *Round,
	state ClientState) error {

	s.turn.writes = append(s.turn.writes, func(ctx context.Context) error {
		return s.RoundStore.CommitState(ctx, round, state)
	})

	return nil
}

// FinalizeRound keeps retirement atomic with the actor's confirmation routing.
func (s *bufferedRoundStore) FinalizeRound(_ context.Context, id RoundID,
	txid chainhash.Hash, info ConfInfo) error {

	s.turn.writes = append(s.turn.writes, func(ctx context.Context) error {
		return s.RoundStore.FinalizeRound(ctx, id, txid, info)
	})

	return nil
}

// FailRound defers reservation release until the failure state is committed.
func (s *bufferedRoundStore) FailRound(_ context.Context, id RoundID) error {
	s.turn.writes = append(s.turn.writes, func(ctx context.Context) error {
		return s.RoundStore.FailRound(ctx, id)
	})

	return nil
}

// FailForfeitIntents keeps originating job retirement in the same commit.
func (s *bufferedRoundStore) FailForfeitIntents(_ context.Context,
	points []wire.OutPoint, reason string, code RoundFailureCode) error {

	points = append([]wire.OutPoint(nil), points...)
	s.turn.writes = append(s.turn.writes, func(ctx context.Context) error {
		return s.RoundStore.FailForfeitIntents(
			ctx, points, reason, code,
		)
	})

	return nil
}

// bufferedVTXOStore joins owned output changes to the round's final commit.
type bufferedVTXOStore struct {
	VTXOStore
	turn *clientTurnWrites
}

// SaveVTXOs defers persistence until confirmation state and effects are ready.
func (s *bufferedVTXOStore) SaveVTXOs(_ context.Context,
	values []*ClientVTXO) error {

	s.turn.writes = append(s.turn.writes, func(ctx context.Context) error {
		return s.VTXOStore.SaveVTXOs(ctx, values)
	})

	return nil
}

// MarkVTXOSpent keeps input consumption in the same transaction as the round.
func (s *bufferedVTXOStore) MarkVTXOSpent(_ context.Context,
	point wire.OutPoint) error {

	s.turn.writes = append(s.turn.writes, func(ctx context.Context) error {
		return s.VTXOStore.MarkVTXOSpent(ctx, point)
	})

	return nil
}

// bufferedServiceStore retains operation ownership in the actor transaction.
type bufferedServiceStore struct {
	ServiceOperationStore
	turn *clientTurnWrites
}

// SaveServiceOperation defers reservation insertion to the fenced commit.
func (s *bufferedServiceStore) SaveServiceOperation(_ context.Context,
	operation DeferredServiceOperation) error {

	operation.Inputs = append([]wire.OutPoint(nil), operation.Inputs...)
	operation.Forfeits = append([]wire.OutPoint(nil), operation.Forfeits...)
	s.turn.writes = append(s.turn.writes, func(ctx context.Context) error {
		return s.ServiceOperationStore.SaveServiceOperation(
			ctx, operation,
		)
	})

	return nil
}

// BindServiceOperation preserves the server identity and fixed deadline.
func (s *bufferedServiceStore) BindServiceOperation(_ context.Context,
	id [32]byte, roundID RoundID, deadline time.Time) error {

	s.turn.writes = append(s.turn.writes, func(ctx context.Context) error {
		return s.ServiceOperationStore.BindServiceOperation(
			ctx, id, roundID, deadline,
		)
	})

	return nil
}

// CompleteServiceOperation defers ownership release until completion commits.
func (s *bufferedServiceStore) CompleteServiceOperation(_ context.Context,
	id [32]byte) error {

	s.turn.writes = append(s.turn.writes, func(ctx context.Context) error {
		return s.ServiceOperationStore.CompleteServiceOperation(ctx, id)
	})

	return nil
}

var _ RoundStore = (*bufferedRoundStore)(nil)
var _ VTXOStore = (*bufferedVTXOStore)(nil)
var _ ServiceOperationStore = (*bufferedServiceStore)(nil)
