package conn

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"pgregory.net/rapid"
)

// awaitNow performs a non-blocking await on a Future by using a
// context with a very short deadline. This is the Future-based
// equivalent of a non-blocking channel receive.
func awaitNow(future actor.Future[*mailboxpb.Envelope]) (*mailboxpb.Envelope,
	bool) {

	ctx, cancel := context.WithTimeout(
		context.Background(), time.Millisecond,
	)
	defer cancel()

	result := future.Await(ctx)
	if result.IsErr() {
		return nil, false
	}

	var env *mailboxpb.Envelope
	result.WhenOk(func(e *mailboxpb.Envelope) {
		env = e
	})

	return env, true
}

// TestResponseRegistry_Interleavings_Property validates response-registry
// invariants under randomized register/remove/deliver interleavings.
// The model tracks three pieces of state: whether a waiter entry exists
// in the registry map, whether its promise has been settled (completed),
// and whether a buffered response is pending for the correlation ID.
func TestResponseRegistry_Interleavings_Property(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		registry := NewResponseRegistry(time.Hour)
		id := CorrelationID("property-correlation")

		var (
			future        actor.Future[*mailboxpb.Envelope]
			waiterExists  bool
			futureSettled bool

			pendingMsg string
			hasPending bool
		)

		steps := rapid.IntRange(20, 200).Draw(rt, "steps")

		for i := range steps {
			opLabel := fmt.Sprintf("op_%d", i)
			op := rapid.IntRange(0, 2).Draw(rt, opLabel)

			switch op {
			case 0:
				future = registry.RegisterWaiter(id)

				if !waiterExists {
					// Fresh waiter created with a new
					// unsettled promise.
					waiterExists = true
					futureSettled = false
				}

				// If a buffered response exists and the
				// promise hasn't been settled yet, it should
				// be completed immediately.
				if hasPending && !futureSettled {
					env, ok := awaitNow(future)
					if !ok {
						rt.Fatalf("missing pending")
					}

					if env == nil ||
						env.MsgId != pendingMsg {

						rt.Fatalf(
							"pending mismatch: %v",
							env)
					}

					hasPending = false
					futureSettled = true
				}

			case 1:
				registry.RemoveWaiter(id)
				waiterExists = false
				futureSettled = false

			case 2:
				msgID := fmt.Sprintf("msg-%d-%d", i,
					rapid.Int().Draw(rt, "msg_rand"))

				result := registry.DeliverResponse(
					id, &mailboxpb.Envelope{
						MsgId: msgID,
					},
				)
				if result == DeliveryDropped {
					rt.Fatalf("delivery failed")
				}

				if waiterExists && !futureSettled {
					env, got := awaitNow(future)
					if !got {
						rt.Fatalf("waiter missing")
					}

					if env == nil || env.MsgId != msgID {
						rt.Fatalf("waiter mismatch: %v",
							env)
					}

					futureSettled = true
				} else if !waiterExists && !hasPending {
					pendingMsg = msgID
					hasPending = true
				}
			}
		}
	})
}
