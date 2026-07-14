package credit

import (
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/timeout"
)

// expiredMsgRef is the timeout-actor poll-timer callback sink shape, aliased to
// keep the NewRetryCallbackRef signature within the line limit.
type expiredMsgRef = actor.TellOnlyRef[*timeout.ExpiredMsg]

// NewRetryCallbackRef bridges timeout-actor expiries into credit resume pokes.
// When a per-operation poll timer fires, the timeout actor sends an ExpiredMsg
// whose ID is the operation id (set when the timer was scheduled). This ref
// maps that expiry into a ResumeCreditOpRequest told to the registry, which
// routes it to the owning child so an awaiting state reconciles without a hot
// loop.
func NewRetryCallbackRef(registry actor.TellOnlyRef[CreditMsg]) expiredMsgRef {
	return actor.NewMapInputRef(
		registry,
		func(expired *timeout.ExpiredMsg) CreditMsg {
			return &ResumeCreditOpRequest{
				OpID:           string(expired.ID),
				FromRetryTimer: true,
			}
		},
	)
}
