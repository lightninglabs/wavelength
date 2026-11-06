package round

import (
	"context"

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
	txid chainhash.Hash, info ConfInfo,
	then func(context.Context) error) error {

	s.turn.writes = append(s.turn.writes, func(ctx context.Context) error {
		return s.RoundStore.FinalizeRound(ctx, id, txid, info, then)
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

var _ RoundStore = (*bufferedRoundStore)(nil)
var _ VTXOStore = (*bufferedVTXOStore)(nil)
