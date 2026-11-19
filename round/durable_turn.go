package round

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightningnetwork/lnd/tlv"
)

// clientDurableTx carries the runtime's transaction-bound delivery store.
type clientDurableTx struct {
	delivery actor.DeliveryStore
}

// durableClientTurn groups one command's domain mutations and external effects.
type durableClientTurn struct {
	owner   *RoundClientActor
	actorID string
	version int64
	codec   *actor.MessageCodec
	writes  clientTurnWrites
	effects []actor.OutboxParams
}

// stage records in-flight input before any signing or cross-actor work begins.
func (t *durableClientTurn) stage(ctx context.Context, message actor.TLVMessage,
	ax actor.Exec[clientDurableTx]) error {

	// Registration can still reject a queued intent. Publish its receipt
	// only in commit, after the FSM has accepted it. If recovery closed
	// the ID first, the failed write rolls back the state and all effects.
	if cmd, ok := message.(*durableClientCommand); ok &&
		cmd.Kind == clientCommandKeyedExit {

		record := t.owner.cfg.CommitExitReceipt
		if record == nil {
			return fmt.Errorf("keyed exit receipt store " +
				"unavailable")
		}
		var id, receipt []byte
		err := decodeDurableFields(
			cmd.Key, tlv.MakePrimitiveRecord(1, &id),
			tlv.MakePrimitiveRecord(3, &receipt),
		)
		if err != nil {
			return err
		}
		t.writes.writes = append(
			t.writes.writes,
			func(txCtx context.Context) error {
				return record(txCtx, string(id), receipt)
			},
		)
	}
	snapshot, err := t.owner.encodeActorSnapshot(ctx)
	if err != nil {
		return err
	}
	input, err := t.codec.Encode(message)
	if err != nil {
		return err
	}
	signingRound, err := t.owner.interruptedSigningRound(ctx, message)
	if err != nil {
		return err
	}
	checkpoint := &durableClientCheckpoint{
		Snapshot: snapshot, InFlight: input, SigningRound: signingRound,
	}
	data, err := checkpoint.encode()
	if err != nil {
		return err
	}

	return ax.Stage(
		ctx,
		func(txCtx context.Context, tx clientDurableTx) error {
			return tx.delivery.SaveCheckpoint(
				txCtx, actor.CheckpointParams{
					ActorID:   t.actorID,
					StateType: "client-round-in-flight",
					StateData: data,
					Version:   t.version,
				},
			)
		},
	)
}

// capture freezes each effect and assigns its outbox identity before commit.
func (t *durableClientTurn) capture(_ context.Context, msg ClientOutMsg) error {
	effect, err := t.owner.captureDurableEffect(msg)
	if err != nil {
		return err
	}

	return t.captureFrozen(effect)
}

// captureFrozen preserves committed timer deadlines when rebuilding runtime
// work.
func (t *durableClientTurn) captureFrozen(effect *durableClientEffect) error {
	payload, err := t.codec.Encode(effect)
	if err != nil {
		return err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	t.effects = append(t.effects, actor.OutboxParams{
		ID:            id.String(),
		SourceActorID: t.actorID, TargetActorID: t.actorID,
		MessageType: effect.MessageType(), Payload: payload,
	})

	return nil
}

// bind defers domain writes for this turn while leaving signer IO outside a
// writer.
func (t *durableClientTurn) bind() (func(), error) {
	if t.owner.queueEffect != nil {
		return nil, fmt.Errorf("client turn already bound")
	}
	originalConfig := t.owner.cfg
	config := *originalConfig
	config.RoundStore = &bufferedRoundStore{
		RoundStore: originalConfig.RoundStore, turn: &t.writes,
	}
	config.VTXOStore = &bufferedVTXOStore{
		VTXOStore: originalConfig.VTXOStore, turn: &t.writes,
	}
	t.owner.cfg = &config
	t.owner.queueEffect = t.capture
	t.owner.bindRoundStores(&config)

	return func() {
		t.owner.cfg = originalConfig
		t.owner.queueEffect = nil
		t.owner.bindRoundStores(originalConfig)
	}, nil
}

// bindRoundStores covers existing rounds and the template for new rounds.
func (a *RoundClientActor) bindRoundStores(config *RoundClientConfig) {
	bind := func(env *ClientEnvironment) {
		if env == nil {
			return
		}
		env.RoundStore = config.RoundStore
		env.VTXOStore = config.VTXOStore
	}
	bind(a.env)
	for _, round := range a.rounds {
		bind(round.env)
	}
}

// commit persists domain state, checkpoint, effects, and the runtime ack
// together.
func (t *durableClientTurn) commit(ctx context.Context,
	ax actor.Exec[clientDurableTx]) error {

	if err := t.owner.pruneDurableTimers(); err != nil {
		return err
	}
	snapshot, err := t.owner.encodeActorSnapshot(ctx)
	if err != nil {
		return err
	}
	checkpoint := &durableClientCheckpoint{Snapshot: snapshot}
	data, err := checkpoint.encode()
	if err != nil {
		return err
	}

	return ax.Commit(
		ctx,
		func(txCtx context.Context, tx clientDurableTx) error {
			if err := t.writes.apply(txCtx); err != nil {
				return err
			}
			for _, effect := range t.effects {
				if err := tx.delivery.EnqueueOutbox(
					txCtx, effect,
				); err != nil {
					return err
				}
			}

			return tx.delivery.SaveCheckpoint(
				txCtx, actor.CheckpointParams{
					ActorID:   t.actorID,
					StateType: "client-round",
					StateData: data,
					Version:   t.version + 1,
				},
			)
		},
	)
}
