package rounds

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo/clientconn"
)

// SealPredicate inspects the accumulated client registrations and returns
// true if the round should be sealed immediately (before the registration
// timeout fires). Predicates are pure functions — they must not perform
// I/O or modify state.
type SealPredicate func(
	regs map[clientconn.ClientID]*ClientRegistration) bool

// MaxClients returns a predicate that seals when the number of registered
// clients reaches the given limit. A limit of zero disables the check.
func MaxClients(limit int) SealPredicate {
	return func(
		regs map[clientconn.ClientID]*ClientRegistration) bool {

		if limit <= 0 {
			return false
		}

		return len(regs) >= limit
	}
}

// MaxOutputAmount returns a predicate that seals when the total output
// value across all registrations reaches the given threshold. Output
// value is the sum of all VTXO amounts and leave output values. A
// threshold of zero disables the check.
func MaxOutputAmount(threshold btcutil.Amount) SealPredicate {
	return func(
		regs map[clientconn.ClientID]*ClientRegistration) bool {

		if threshold <= 0 {
			return false
		}

		var total btcutil.Amount
		for _, reg := range regs {
			for _, vtxo := range reg.VTXODescriptors {
				total += vtxo.Amount
			}

			for _, leave := range reg.LeaveOutputs {
				total += btcutil.Amount(leave.Value)
			}
		}

		return total >= threshold
	}
}

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
