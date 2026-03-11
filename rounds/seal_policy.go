package rounds

import (
	"github.com/lightninglabs/darepo/clientconn"
)

// SealPredicate inspects the accumulated client registrations and returns
// true if the round should be sealed immediately (before the registration
// timeout fires). Predicates are pure functions — they must not perform
// I/O or modify state.
type SealPredicate func(
	regs map[clientconn.ClientID]*ClientRegistration) bool

// AnySealPredicate returns a composite predicate that seals when ANY of the
// given sub-predicates returns true (logical OR). When preds is empty, the
// returned predicate always returns false (no early seal).
func AnySealPredicate(preds ...SealPredicate) SealPredicate {
	return func(
		regs map[clientconn.ClientID]*ClientRegistration) bool {

		for _, p := range preds {
			if p(regs) {
				return true
			}
		}

		return false
	}
}
