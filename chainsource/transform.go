package chainsource

import "github.com/lightninglabs/wavelength/baselib/actor"

// MapConfirmationEvent creates a transformed TellOnlyRef that accepts
// ConfirmationEvent and transforms it to the caller's desired output message
// type. This is a convenience wrapper for the common pattern of adapting
// chainsource confirmation notifications to caller-specific event types.
//
// Example usage:
//
//	// roundActorRef accepts round.ConfirmationEvent
//	adaptedRef := chainsource.MapConfirmationEvent(
//	    roundActorRef,
//	    func(cs chainsource.ConfirmationEvent) round.ConfirmationEvent {
//	        return round.ConfirmationEvent{
//	            TxID:          cs.Txid,
//	            BlockHeight:   cs.BlockHeight,
//	            Confirmations: int32(cs.NumConfs),
//	        }
//	    },
//	)
//
//	// Register with chainsource using the adapted ref.
//	chainSourceRef.Ask(ctx, &chainsource.RegisterConfRequest{
//	    NotifyActor: fn.Some(adaptedRef),
//	    // ... other fields
//	})
func MapConfirmationEvent[Out actor.Message](
	targetRef actor.TellOnlyRef[Out], mapFn func(ConfirmationEvent) Out,
) actor.TellOnlyRef[ConfirmationEvent] {

	return actor.NewMapInputRef(targetRef, mapFn)
}

// MapSpendEvent creates a transformed TellOnlyRef that accepts SpendEvent and
// transforms it to the caller's desired output message type. This is a
// convenience wrapper for the common pattern of adapting chainsource spend
// notifications to caller-specific event types.
//
// Example usage:
//
//	// roundActorRef accepts round.SpendEvent
//	adaptedRef := chainsource.MapSpendEvent(
//	    roundActorRef,
//	    func(cs chainsource.SpendEvent) round.SpendEvent {
//	        return round.SpendEvent{
//	            Outpoint:      cs.Outpoint,
//	            SpendingTxid:  cs.SpendingTxid,
//	            SpendingTx:    cs.SpendingTx,
//	        }
//	    },
//	)
//
//	// Register with chainsource using the adapted ref.
//	chainSourceRef.Ask(ctx, &chainsource.RegisterSpendRequest{
//	    NotifyActor: fn.Some(adaptedRef),
//	    // ... other fields
//	})
func MapSpendEvent[Out actor.Message](
	targetRef actor.TellOnlyRef[Out], mapFn func(SpendEvent) Out,
) actor.TellOnlyRef[SpendEvent] {

	return actor.NewMapInputRef(targetRef, mapFn)
}

// MapBlockEpoch creates a transformed TellOnlyRef that accepts BlockEpoch
// and transforms it to the caller's desired output message type. This is a
// convenience wrapper for the common pattern of adapting chainsource block
// notifications to caller-specific event types.
//
// Example usage:
//
//	// roundActorRef accepts round.BlockEvent
//	adaptedRef := chainsource.MapBlockEpoch(
//	    roundActorRef,
//	    func(cs chainsource.BlockEpoch) round.BlockEvent {
//	        return round.BlockEvent{
//	            Height:    cs.Height,
//	            Hash:      cs.Hash,
//	            Timestamp: cs.Timestamp,
//	        }
//	    },
//	)
//
//	// Subscribe to blocks using the adapted ref.
//	chainSourceRef.Ask(ctx, &chainsource.SubscribeBlocksRequest{
//	    NotifyActor: fn.Some(adaptedRef),
//	    // ... other fields
//	})
func MapBlockEpoch[Out actor.Message](
	targetRef actor.TellOnlyRef[Out], mapFn func(BlockEpoch) Out,
) actor.TellOnlyRef[BlockEpoch] {

	return actor.NewMapInputRef(targetRef, mapFn)
}
