package round

import (
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// durableClientBehavior owns the native mailbox transaction boundary. A failed
// or panicked turn forces checkpoint reload before another message can observe
// tentative in-memory progress.
type durableClientBehavior struct {
	owner       *RoundClientActor
	actorID     string
	codec       *actor.MessageCodec
	version     int64
	initialized bool
	reload      bool
	ready       actor.Promise[struct{}]
}

// Receive reports native restart completion to the startup owner.
func (b *durableClientBehavior) Receive(ctx context.Context,
	message actor.TLVMessage,
	ax actor.Exec[clientDurableTx]) fn.Result[actormsg.RoundActorResp] {

	result := b.receive(ctx, message, ax)
	if _, restart := message.(*actor.RestartMessage); restart &&
		b.ready != nil {

		b.ready.Complete(fn.NewResult(struct{}{}, result.Err()))
	}

	return result
}

// receive commits the full state, domain writes, effects, and consumption as
// one unit, with signing work outside the database writer transaction.
func (b *durableClientBehavior) receive(ctx context.Context,
	message actor.TLVMessage,
	ax actor.Exec[clientDurableTx]) fn.Result[actormsg.RoundActorResp] {

	_, restart := message.(*actor.RestartMessage)
	if !b.initialized && !restart {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("client restart has not completed"),
		)
	}
	recovering := restart || b.reload
	if recovering {
		if err := b.restore(ctx, ax); err != nil {
			return fn.Err[actormsg.RoundActorResp](err)
		}
	}
	t := &durableClientTurn{
		owner: b.owner, actorID: b.actorID,
		version: b.version, codec: b.codec,
	}
	if !restart {
		if err := t.stage(ctx, message, ax); err != nil {
			return fn.Err[actormsg.RoundActorResp](err)
		}
	}

	// This stays set if Receive panics, including a panic during encoding
	// or commit. The runtime catches the panic and redelivers the message.
	b.reload = true
	unbind, err := t.bind()
	if err != nil {
		return fn.Err[actormsg.RoundActorResp](err)
	}
	defer unbind()
	if recovering {
		if err := b.owner.restoreRuntimeEffects(ctx, t); err != nil {
			return fn.Err[actormsg.RoundActorResp](err)
		}
	}
	response := fn.Ok[actormsg.RoundActorResp](nil)
	if !restart {
		input, ok := message.(actormsg.RoundReceivable)
		if !ok {
			return fn.Err[actormsg.RoundActorResp](
				fmt.Errorf("unsupported client ingress %T",
					message),
			)
		}
		response = b.owner.Receive(ctx, input)
		if response.IsErr() {
			return response
		}
	}
	if err := t.commit(ctx, ax); err != nil {
		return fn.Err[actormsg.RoundActorResp](err)
	}
	b.version++
	b.initialized = true
	b.reload = false

	return response
}

// restore reads the latest checkpoint rather than trusting a previously queued
// restart envelope, which may have survived a second process crash.
func (b *durableClientBehavior) restore(ctx context.Context,
	ax actor.Exec[clientDurableTx]) error {

	var checkpoint *actor.Checkpoint
	err := ax.Read(
		ctx,
		func(txCtx context.Context, tx clientDurableTx) error {
			var err error
			checkpoint, err = tx.delivery.LoadCheckpoint(
				txCtx, b.actorID,
			)

			return err
		},
	)
	if err != nil {
		return err
	}
	if checkpoint == nil {
		if b.initialized {
			return fmt.Errorf("initialized client checkpoint " +
				"disappeared")
		}

		return ax.Read(
			ctx,
			func(txCtx context.Context, _ clientDurableTx) error {
				return b.owner.importLegacyRounds(txCtx)
			},
		)
	}
	if checkpoint.ActorID != b.actorID {
		return fmt.Errorf("client checkpoint owner differs")
	}
	envelope, err := decodeDurableClientCheckpoint(
		checkpoint.StateData, b.codec,
	)
	if err != nil {
		return err
	}
	snapshot, err := decodeActorSnapshot(envelope.Snapshot, b.owner.env)
	if err != nil {
		return err
	}
	if len(envelope.InFlight) != 0 {
		for _, round := range snapshot.rounds {
			// A crash while entering the nonce ceremony may leave
			// external signer state even though the before-image
			// does not contain sessions yet. Reconcile instead of
			// creating a second ceremony for the same commitment.
			switch round.state.(type) {
			case *CommitmentTxReceivedState,
				*CommitmentTxValidatedState:

				round.lostSignerSessions = true
			}
		}
	}
	// Release process-local sessions before replacing their owning runners.
	if err := b.owner.OnStop(ctx); err != nil {
		return err
	}
	b.owner.installActorSnapshot(ctx, snapshot)
	b.version = checkpoint.Version

	return nil
}
