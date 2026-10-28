package timeout

import (
	"errors"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
)

// mailboxPressureGrace lets retained callbacks absorb brief queue bursts before
// asking an operator to investigate a recipient that is not draining its queue.
const mailboxPressureGrace = 30 * time.Second

// mailboxPressure tracks one consecutive full-mailbox episode. It only controls
// diagnostics; the timer entry still owns delivery and its unchanged backoff.
// All access belongs to the timeout actor's receive loop.
type mailboxPressure struct {
	since  time.Time
	warned bool
}

// shouldWarn reports the first actionable full-mailbox failure in an episode.
// Other errors remain immediately visible and end the full-mailbox episode.
// The owner must clear this state after successful delivery or replacement.
func (p *mailboxPressure) shouldWarn(now time.Time, err error) bool {
	if !errors.Is(err, actor.ErrMailboxFull) {
		*p = mailboxPressure{}

		return true
	}

	if p.since.IsZero() {
		p.since = now
	}
	if p.warned || now.Sub(p.since) < mailboxPressureGrace {
		return false
	}

	p.warned = true

	return true
}
