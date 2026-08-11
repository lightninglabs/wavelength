package vtxo

import "fmt"

// CheckForfeitAdmission reports whether the VTXO described by desc may be
// committed to a NEW cooperative forfeit — a leave, a refresh, or an
// in-round send. A nil return means the commitment is admissible.
//
// This is an advisory pre-check, not the authority. A reservation is only
// settled once the VTXO actor's FSM has processed PendingForfeitEvent, and a
// concurrent claim (an automatic refresh cohort, say) can take the coin
// between this check and that event. The FSM therefore keeps refusing what it
// must; this check exists so that the ordinary, entirely predictable
// rejections carry an error naming the operation that holds the coin, rather
// than surfacing a raw FSM event-type mismatch to the user.
//
// Expired VTXOs are deliberately admissible. Their value is recoverable only
// by forfeiting them into an ordinary round, so refusing them here would
// strand the coin permanently — ExpiredState accepts PendingForfeitEvent for
// exactly that reason.
func CheckForfeitAdmission(desc *Descriptor) error {
	if desc == nil {
		return fmt.Errorf("%w: no descriptor", ErrVTXOTerminal)
	}

	switch desc.Status {
	// The ordinary case, plus the expired-reclaim path that rides the
	// same cooperative forfeit.
	case VTXOStatusLive, VTXOStatusExpired:
		return nil

	// Already committed to a round. PendingForfeit is refused even though
	// the FSM tolerates the duplicate as a self-loop: a second request
	// carrying a different destination would otherwise report success
	// while the first destination is what actually gets paid, and which
	// of the two behaviours a caller meets is decided purely by how far
	// the round happens to have progressed in the preceding seconds.
	case VTXOStatusPendingForfeit, VTXOStatusForfeiting:
		return fmt.Errorf("%w %s", ErrForfeitInFlight,
			forfeitRoundHint(desc))

	// Claimed by an in-flight OOR spend. The spend owns the coin until it
	// completes or is released.
	case VTXOStatusSpending:
		return fmt.Errorf("%w: outpoint %s is spend-reserved",
			ErrVTXOLiquidityLocked, desc.Outpoint)

	// Handed to the chain resolver. The exit is observed rather than
	// fire-and-forget, so the coin may yet return to Live if the job
	// fails without an on-chain footprint — but it is not forfeitable in
	// the meantime.
	case VTXOStatusUnilateralExit:
		return fmt.Errorf("%w: outpoint %s", ErrExitInFlight,
			desc.Outpoint)

	// Terminal. These are normally unreachable through the manager (their
	// actors have been reaped, so the reservation would fail with "no
	// actor for outpoint"), but the FSM self-loops on every event in
	// these states, so a caller that does reach one would otherwise read
	// the self-loop as an accepted reservation.
	default:
		return fmt.Errorf("%w: outpoint %s is %s", ErrVTXOTerminal,
			desc.Outpoint, desc.Status)
	}
}

// forfeitRoundHint renders the round that holds a committed VTXO, so the
// rejection points at something the user can actually look up. The round ID
// is only persisted once the round has supplied forfeit details, so a VTXO
// still in PendingForfeit reports its state alone.
func forfeitRoundHint(desc *Descriptor) string {
	if desc.ForfeitRoundID == "" {
		return fmt.Sprintf("(outpoint %s, state %s)", desc.Outpoint,
			desc.Status)
	}

	return fmt.Sprintf("(outpoint %s, state %s, round %s)", desc.Outpoint,
		desc.Status, desc.ForfeitRoundID)
}
