package serverconn

import (
	"fmt"
	"math"

	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
)

// validatePullCursor checks the entire response before any dispatch can cause
// an irreversible side effect. Sequences may have gaps: allocation need not be
// recipient-local or rollback-free. The wire contract only proves ordering and
// the successor of the returned rows, not that the server omitted no rows.
func validatePullCursor(cursor uint64, resp *mailboxpb.PullResponse) error {
	if len(resp.Envelopes) == 0 {
		// Older edges return zero on an empty long-poll. Neither zero
		// nor an echoed cursor advances the client's persisted state.
		if resp.NextCursor != 0 && resp.NextCursor != cursor {
			return fmt.Errorf("empty pull changed cursor from "+
				"%d to %d", cursor, resp.NextCursor)
		}

		return nil
	}

	var previous uint64
	for _, env := range resp.Envelopes {
		if env == nil || env.EventSeq == 0 || env.EventSeq < cursor ||
			env.EventSeq <= previous {
			return fmt.Errorf("invalid pull sequence %d after %d "+
				"at cursor %d", env.GetEventSeq(), previous,
				cursor)
		}

		previous = env.EventSeq
	}
	if previous == math.MaxUint64 || resp.NextCursor != previous+1 {
		return fmt.Errorf("invalid next cursor %d after sequence %d",
			resp.NextCursor, previous)
	}

	return nil
}
