package oor

import (
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/timeout"
)

// NewRetryCallbackRef creates a TellOnlyRef that transforms timeout
// expiry notifications into OOR actor ResumeSessionRequest messages.
// The timeout ID is expected to be the hex-encoded session ID (set by
// SigningOutboxHandler.handleScheduleRetry), which is parsed back into
// a SessionID for the resume request.
//
// This bridges the timeout actor's fire-and-forget callback with the OOR
// actor's durable mailbox, enabling event-driven retry scheduling without
// blocking goroutines on time.Sleep.
func NewRetryCallbackRef(
	oorRef actor.TellOnlyRef[OORDurableMsg],
) actor.TellOnlyRef[*timeout.ExpiredMsg] {

	return actor.NewMapInputRef(
		oorRef,
		func(expired *timeout.ExpiredMsg) OORDurableMsg {
			// Parse the timeout ID back into a session ID.
			// The ID was set to sessionID.String() (hex) when
			// the timeout was scheduled.
			sessionHash, err := chainhash.NewHashFromStr(
				string(expired.ID),
			)
			if err != nil {

				// This should never happen since we control
				// the timeout ID format. Return a
				// zero-session resume request that the
				// actor will reject gracefully.
				return &ResumeSessionRequest{}
			}

			return &ResumeSessionRequest{
				SessionID:      SessionID(*sessionHash),
				FromRetryTimer: true,
			}
		},
	)
}
