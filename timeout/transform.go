package timeout

import (
	"github.com/lightninglabs/wavelength/baselib/actor"
)

// MapTimeoutExpired creates a transformed TellOnlyRef that converts timeout
// expiry messages into a target actor's message type. This helper simplifies
// integrating the timeout actor with other actors by automatically
// transforming ExpiredMsg into the appropriate message type.
//
// Example usage:
//
//	// Transform timeout expiry into a rounds actor message.
//	callbackRef := timeout.MapTimeoutExpired(
//	    roundsActorRef,
//	    func(expired timeout.ExpiredMsg) rounds.ActorMsg {
//	        return &rounds.TimeoutExpired{
//	  	      ID: expired.ID,
//	  	}
//	    },
//	)
//
//	// Send schedule request to timeout actor.
//	timeoutActorRef.Tell(ctx, &timeout.ScheduleTimeoutRequest{
//		ID:       timeout.ID(m.RoundID),
//		Duration: m.Duration,
//		Callback: callbackRef,
//	})
func MapTimeoutExpired[Out actor.Message](targetRef actor.TellOnlyRef[Out],
	mapFn func(ExpiredMsg) Out) actor.TellOnlyRef[*ExpiredMsg] {

	// Use the MapInputRef utility from the actor library to handle the
	// transformation from *ExpiredMsg to the caller's output type.
	return actor.NewMapInputRef(
		targetRef,
		func(expired *ExpiredMsg) Out {
			return mapFn(*expired)
		},
	)
}

// MapTickFired creates a transformed TellOnlyRef that converts recurring-
// tick fire messages into a target actor's message type. It mirrors
// MapTimeoutExpired for the recurring-tick scheduler:
//
//	tickRef := timeout.MapTickFired(
//	    roundsActorRef,
//	    func(fired timeout.TickFiredMsg) rounds.ActorMsg {
//	        return &rounds.TickFired{ID: fired.ID}
//	    },
//	)
//
//	timeoutActorRef.Tell(ctx, &timeout.ScheduleRecurringTickRequest{
//	    ID:       compositeID,
//	    Interval: m.Interval,
//	    Callback: tickRef,
//	})
func MapTickFired[Out actor.Message](targetRef actor.TellOnlyRef[Out],
	mapFn func(TickFiredMsg) Out) actor.TellOnlyRef[*TickFiredMsg] {

	return actor.NewMapInputRef(
		targetRef,
		func(fired *TickFiredMsg) Out {
			return mapFn(*fired)
		},
	)
}
