package round

import (
	"context"
	"fmt"
	"log/slog"

	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// processLocalOutbox applies round bookkeeping before effects are committed.
// The durable turn supplies buffered stores so these writes join its
// checkpoint.
func (a *RoundClientActor) processLocalOutbox(ctx context.Context,
	msg ClientOutMsg) (bool, error) {

	switch m := msg.(type) {
	case *RoundCompletedNotification:
		a.log.InfoS(ctx, "Processing round completion notification",
			slog.String("round_id", m.RoundID.String()),
			slog.String("txid", m.TxID.String()),
		)

		// Round FSM reached ConfirmedState. Perform actor
		// cleanup.
		err := a.onRoundComplete(
			ctx, m.RoundID, m.TxID, m.ConfInfo,
		)
		if err != nil {
			return true, fmt.Errorf("failed to complete round "+
				"%s: %w", m.RoundID, err)
		}

		// Count the confirmed round for observability.
		a.emitRoundCompleted(
			ctx, m.RoundID.String(),
			"confirmed",
		)

		return true, nil

	case *RoundCheckpointedNotification:
		a.log.InfoS(ctx, "Processing round checkpoint notification",
			slog.String("round_id", m.RoundID.String()),
		)

		// Find the round by its RoundID (should already be
		// re-keyed at this point).
		keyStr := RoundKeyStr(m.RoundID.KeyString())
		roundFSM, exists := a.rounds[keyStr]
		if !exists {
			return true, fmt.Errorf("round not found for "+
				"checkpoint: %s", m.RoundID)
		}

		// Get the current state to extract commitment tx info.
		state, err := fsmState(ctx, roundFSM.FSM)
		if err != nil {
			return true, fmt.Errorf("failed to get state: %w", err)
		}

		inputSigState, ok := state.(*InputSigSentState)
		if !ok {
			return true, fmt.Errorf("round not in "+
				"InputSigSentState, got %T", state)
		}

		// Update round FSM with commitment tx info.
		txid := inputSigState.CommitmentTx.UnsignedTx.TxHash()
		roundFSM.TxID = txid
		roundFSM.CommitmentTx = fn.Some(
			inputSigState.CommitmentTx,
		)

		// Index the transaction before the confirmation request
		// emitted by the FSM can be delivered. The FSM outbox
		// owns the steady-state registration; registering again
		// here creates two notifier subscriptions for the same
		// tx. A restarted actor still re-registers active
		// rounds in Start.
		a.commitmentTxIndex[txid] = keyStr

		a.log.InfoS(ctx, "Round checkpoint processed",
			slog.String("round_id", m.RoundID.String()),
			slog.String("commitment_txid", txid.String()),
		)

		return true, nil

	case *RoundFailedNotification:
		// Round entered failed state. Log for observability.
		roundIDStr := "none"
		m.RoundID.WhenSome(func(id RoundID) {
			roundIDStr = id.String()
		})
		if m.Recoverable {
			a.log.InfoS(ctx, "Round failed",
				slog.Any("err", m.OriginalError),
				slog.String("round_id", roundIDStr),
				slog.String("reason", m.Reason),
				slog.Bool("recoverable", true),
			)
		} else {
			a.log.WarnS(ctx, "Round failed",
				m.OriginalError,
				slog.String("round_id", roundIDStr),
				slog.String("reason", m.Reason),
				slog.Bool("recoverable", false),
			)
		}

		// Count the failed round for observability. The
		// counter pairs with the confirmed branch above so an
		// operator can track the join-to-completion ratio.
		a.emitRoundCompleted(ctx, roundIDStr, "failed")

		// Retire the durable side of the round. Reaping only
		// drops the in-memory FSM, so without this the
		// checkpoint row stays in ListActiveRounds and is
		// re-hydrated on every start, and the deposits it
		// adopted stay adopted: out of the sweep and pinned
		// against the board limit for good.
		m.RoundID.WhenSome(func(id RoundID) {
			a.retireFailedRound(ctx, id)
		})

		return true, nil

	case *TerminalJobFailedNotification:
		// A terminal-for-job round failure (e.g. the operator
		// could not fund the commitment tx). The accompanying
		// ReleaseForfeitReservation has already returned the
		// VTXOs to the live set; here we drop the originating
		// job's persisted pending intent so restart replay does
		// not re-submit the same inputs into the same wall, and
		// surface the job's activity entry as failed.
		a.handleTerminalJobFailure(ctx, m)

		return true, nil

	default:
		return false, nil
	}
}
