package wallet

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/wire/v2"
)

// boardIntentReplayer replays persisted Board intents after a daemon
// restart. A persisted Board intent is live when at least one of its anchor
// outpoints still resolves to a BoardingStatusConfirmed boarding intent;
// otherwise the round it was admitted into has already adopted/swept/failed
// the deposit and the row is stale.
type boardIntentReplayer struct {
	ark *Ark
}

// Kind returns the intent kind this replayer owns.
func (b *boardIntentReplayer) Kind() PendingIntentKind {
	return PendingIntentKindBoard
}

// Replay reconciles the persisted Board intents against the current set of
// confirmed boarding intents and self-Tells a single BoardRequest when at
// least one anchor is still live. The self-Tell re-runs handleBoard against
// a real BoardingStore read, which re-persists with a fresh timestamp and
// re-issues the downstream TriggerBoardMsg; FIFO ordering of the wallet
// mailbox guarantees any user-issued Board RPC arriving over gRPC after the
// daemon's replay Ask returns is processed AFTER the replay, eliminating
// the gRPC-admission-vs-replay startup race.
func (b *boardIntentReplayer) Replay(ctx context.Context,
	intents []PendingIntent) (bool, error) {

	a := b.ark

	// Reconcile the persisted intents against the current set of
	// confirmed boarding intents. An anchor is "live" only if its
	// outpoint still has a BoardingStatusConfirmed intent.
	confirmed, err := a.store.FetchBoardingIntentsByStatus(
		ctx, BoardingStatusConfirmed,
	)
	if err != nil {
		return false, fmt.Errorf("fetch confirmed boarding intents: %w",
			err)
	}

	confirmedSet := make(map[wire.OutPoint]struct{}, len(confirmed))
	for _, intent := range confirmed {
		confirmedSet[intent.Outpoint] = struct{}{}
	}

	var (
		liveTarget          uint32
		livePolicyTemplate  []byte
		livePkScript        []byte
		earliestRequestedAt int64
		liveAnchors         int
		liveIntents         int
		staleIntentIDs      []PendingIntentID
	)
	for _, intent := range intents {
		intentLive := false
		for _, anchor := range intent.Anchors {
			if _, ok := confirmedSet[anchor]; !ok {
				continue
			}

			intentLive = true
			liveAnchors++
		}

		if !intentLive {
			// This intent anchors only outpoints that are no longer
			// Confirmed: the round it was admitted into has already
			// adopted/swept/failed the deposit. Record it for
			// cleanup so it does not linger as an orphaned row when
			// other intents are still live. (When nothing is live,
			// the bulk-clear branch below handles it in one shot.)
			staleIntentIDs = append(staleIntentIDs, intent.ID)

			continue
		}

		liveIntents++

		// Intents are listed in requested_at_unix ascending order,
		// so the most recent live intent's target wins — matching
		// the legacy per-row "last live row wins" semantics. The
		// store reconstructs the concrete payload from the board
		// detail table, so the type assertion always holds.
		payload, ok := intent.Payload.(*BoardIntentPayload)
		if !ok {
			return false, fmt.Errorf("board replayer got "+
				"non-board payload %T", intent.Payload)
		}
		liveTarget = payload.TargetVTXOCount
		livePolicyTemplate = payload.PolicyTemplate
		livePkScript = payload.PkScript

		if earliestRequestedAt == 0 ||
			intent.RequestedAt < earliestRequestedAt {

			earliestRequestedAt = intent.RequestedAt
		}
	}

	if liveIntents == 0 {
		// Every persisted intent anchors only outpoints that are no
		// longer Confirmed. The Board calls these rows belonged to
		// have already completed; sweep them so the next start is a
		// no-op.
		err := a.store.ClearPendingIntentsByKind(
			ctx, PendingIntentKindBoard,
		)
		if err != nil {
			return false, fmt.Errorf("clear stale pending board "+
				"intents: %w", err)
		}

		a.logger(ctx).InfoS(
			ctx,
			"Cleared stale pending Board intents on startup",
			slog.Int("stale_intent_count", len(intents)),
		)

		return false, nil
	}

	// At least one intent is live, so the bulk-clear branch did not run.
	// Delete any stale intents individually so they do not linger as
	// orphaned rows until a later all-stale start.
	for _, id := range staleIntentIDs {
		if err := a.store.DeletePendingIntent(ctx, id); err != nil {
			return false, fmt.Errorf("clear stale board intent: %w",
				err)
		}
	}

	a.logger(ctx).InfoS(
		ctx,
		"Replaying persisted Board request after restart",
		slog.Int("target_vtxo_count", int(liveTarget)),
		slog.Int("live_anchor_count", liveAnchors),
		slog.Int("live_intent_count", liveIntents),
		slog.Int("stale_intent_count", len(intents)-liveIntents),
		slog.Int64("earliest_requested_at_unix", earliestRequestedAt),
	)

	// Self-Tell the BoardRequest. handleBoard will re-walk the confirmed
	// set, re-persist with a fresh timestamp, and Tell the round actor.
	// The daemon Asks the wallet to replay only after the round-client
	// actor has registered with the receptionist, so the downstream
	// TriggerBoardMsg dispatch from handleBoard sees a live round-actor
	// ref rather than a "not found" lookup that would silently drop the
	// replay.
	err = a.selfRef.Tell(ctx, &BoardRequest{
		TargetVTXOCount: liveTarget,
		PolicyTemplate:  livePolicyTemplate,
		PkScript:        livePkScript,
	})
	if err != nil {
		return false, fmt.Errorf("self-tell pending board request: %w",
			err)
	}

	return true, nil
}
