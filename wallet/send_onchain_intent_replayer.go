package wallet

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/types"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// replayRoundRegisterTimeout bounds the round-actor Ask issued when replaying
// a persisted onchain send after restart. Replay runs on the single-worker
// wallet mailbox via a self-Tell, so an unresponsive round actor must not be
// able to block it indefinitely; on timeout the intent stays persisted and
// the next start retries.
const replayRoundRegisterTimeout = 30 * time.Second

// buildSendOnChainIntentPackage assembles the round intent for an onchain
// send from the reserved forfeit set and the replay parameters. Shared
// between the live handleSendOnChain path and the restart replay path so
// both produce the identical intent shape: forfeits over the selected
// outpoints, one leave output at the destination, and (bounded mode only) a
// change VTXO with a freshly derived owner key.
//
// The change key is derived per call rather than persisted: a replayed
// intent's original derivation never reached a round (the outbox row would
// have been cleared otherwise), so deriving a fresh key only costs a small
// amount of keychain gap, never funds.
func (a *Ark) buildSendOnChainIntentPackage(ctx context.Context,
	p SendOnChainIntentPayload, selectedOutpoints []wire.OutPoint,
	selectedAmounts []btcutil.Amount, change btcutil.Amount) (
	[]types.ForfeitRequest, []*types.LeaveRequest, []types.VTXORequest,
	error) {

	forfeits := make(
		[]types.ForfeitRequest, 0, len(selectedOutpoints),
	)
	for i, op := range selectedOutpoints {
		forfeits = append(forfeits, types.ForfeitRequest{
			VTXOOutpoint: &op,
			Amount:       selectedAmounts[i],
		})
	}

	var (
		leaves []*types.LeaveRequest
		vtxos  []types.VTXORequest
	)

	if p.SweepAll {
		// One leave output, IsChange=true: the server stamps the
		// residual (Σinputs − fee) onto it at seal time. We still ship
		// the pre-fee Σ(inputs) as the placeholder value rather than a
		// bare zero. The round FSM's IntentRequested validation sums
		// the leave output values and rejects a zero total ("no VTXO
		// output amount"), and the operator's join-request validation
		// rejects a sub-dust output before the seal-time builder runs —
		// the same admission-time floor the bounded-mode change VTXO
		// below relies on. Σ(inputs) is above dust by construction (the
		// forfeited VTXOs each cleared it), so both checks pass and the
		// server rewrites the slot at seal time regardless of the
		// placeholder.
		var totalInput btcutil.Amount
		for _, amt := range selectedAmounts {
			totalInput += amt
		}

		leaves = []*types.LeaveRequest{
			{
				Output: &wire.TxOut{
					PkScript: p.DestinationPkScript,
					Value:    int64(totalInput),
				},
				IsChange: true,
			},
		}

		return forfeits, leaves, vtxos, nil
	}

	// One fixed leave + one change VTXO. The change VTXO uses the same
	// arkscript pattern as the directed-send self-change at
	// buildSendVTXORequests so confirmation-time ownership persistence
	// works through the standard OwnedScriptRegistrar path.
	leaves = []*types.LeaveRequest{
		{
			Output: &wire.TxOut{
				PkScript: p.DestinationPkScript,
				Value:    int64(p.TargetAmountSat),
			},
			IsChange: false,
		},
	}

	changeClientKey, err := a.backend.DeriveNextKey(
		ctx, types.VTXOOwnerKeyFamily,
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("derive change client key: %w",
			err)
	}

	policyTemplate, pkScript, err := arkscript.EncodeStandardVTXOArtifacts(
		changeClientKey.PubKey, p.OperatorKey, p.VTXOExitDelay,
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("build change descriptor: %w",
			err)
	}

	vtxos = []types.VTXORequest{
		{
			// Amount is the caller's projected change
			// (totalSelected − TargetAmountSat). The seal-time
			// quote rewrites this slot with the residual
			// (Σin − Σfixed − fee) because IsChange=true, but
			// the value must already be above the operator's
			// dust floor at admission time — the join-request
			// validation runs before the seal-time builder, and
			// a zero or sub-dust placeholder is rejected with
			// ErrVTXOAmountBelowMinimum.
			Amount:         change,
			PolicyTemplate: policyTemplate,
			PkScript:       pkScript,
			Expiry:         p.VTXOExitDelay,
			ClientKey:      changeClientKey.PubKey,
			OwnerKey:       *changeClientKey,
			OperatorKey:    p.OperatorKey,
			Origin:         types.VTXOOriginRoundRefresh,
			IsChange:       true,
		},
	}

	return forfeits, leaves, vtxos, nil
}

// sendOnChainIntentReplayer replays persisted SendOnChain intents after a
// daemon restart. handleSendOnChain reserves the forfeit anchors before it
// persists the intent, so a crashed send's anchors recover from their
// persisted status directly into PendingForfeit (not Live) — the VTXO
// manager's startup orphan sweep only releases Spending VTXOs and never
// touches PendingForfeit anchors. Replay re-reserves exactly those
// outpoints, which is safe because re-issuing a forfeit reservation against
// an already-PendingForfeit VTXO self-loops idempotently in the VTXO FSM.
// Selection is never re-run, since that could silently bind the intent to a
// different forfeit set and change its identity. The rebuilt intent is then
// re-registered with the round actor.
type sendOnChainIntentReplayer struct {
	ark *Ark
}

// Kind returns the intent kind this replayer owns.
func (s *sendOnChainIntentReplayer) Kind() PendingIntentKind {
	return PendingIntentKindSendOnChain
}

// Replay self-Tells one ReplaySendOnChainIntent per persisted intent. The
// per-intent work (reserve, rebuild, register) runs from the wallet's own
// mailbox so FIFO ordering against user RPCs admitted after the daemon's
// replay Ask is preserved, mirroring the Board replay shape.
func (s *sendOnChainIntentReplayer) Replay(ctx context.Context,
	intents []PendingIntent) (bool, error) {

	a := s.ark

	for _, intent := range intents {
		err := a.selfRef.Tell(ctx, &ReplaySendOnChainIntent{
			Intent: intent,
		})
		if err != nil {
			return false, fmt.Errorf("self-tell pending onchain "+
				"send intent: %w", err)
		}
	}

	a.logger(ctx).InfoS(
		ctx,
		"Replaying persisted onchain send intents after restart",
		slog.Int("intent_count", len(intents)),
	)

	return len(intents) > 0, nil
}

// handleReplaySendOnChainIntent re-issues one persisted onchain send after
// a restart: re-reserve the exact anchor outpoints, rebuild the identical
// intent package from the TLV payload, and register it with the round
// actor.
//
// Staleness: a reservation failure means at least one anchor is no longer
// a live VTXO — it was consumed by another spend path (OOR, unroll, a round
// whose checkpoint cleared a different intent's anchors) since the row was
// written. The intent can never be adopted again, so the row is deleted
// rather than left to fail on every future start.
func (a *Ark) handleReplaySendOnChainIntent(ctx context.Context,
	msg *ReplaySendOnChainIntent) fn.Result[WalletResp] {

	intent := msg.Intent

	payload, ok := intent.Payload.(*SendOnChainIntentPayload)
	if !ok {
		return fn.Err[WalletResp](
			fmt.Errorf("send replayer got non-send payload %T",
				intent.Payload),
		)
	}

	// Verify the row's content still hashes to its own key before
	// trusting it. The intent ID commits to (kind, anchors, payload
	// fields), so a detail row that was corrupted or tampered with
	// since the original handleSendOnChain write cannot pass: e.g. a
	// flipped sweep_all column would turn a bounded exact-amount send
	// into a fee-absorbing IsChange=true leave that drains the full
	// forfeit set to the destination, exactly the #634 overpay this
	// send path was built to prevent. Dropping the row is the
	// funds-safe failure mode: no send happens and the user re-issues.
	// Send anchors are all-or-nothing (the adopting round consumes the
	// entire reserved forfeit set and CommitState sweeps the whole
	// row), so unlike Board there is no legitimate partial-anchor
	// state that could trip this check.
	wantID := NewPendingIntentID(payload, intent.Anchors)
	if wantID != intent.ID {
		a.logger(ctx).WarnS(ctx, "Dropping onchain send intent "+
			"with mismatched integrity hash", nil,
			slog.Int("anchor_count", len(intent.Anchors)),
			slog.Int64("requested_at_unix", intent.RequestedAt))

		if delErr := a.store.DeletePendingIntent(
			ctx, intent.ID,
		); delErr != nil {
			return fn.Err[WalletResp](
				fmt.Errorf("delete tampered onchain send "+
					"intent: %w", delErr),
			)
		}

		return fn.Ok[WalletResp](&SendOnChainResponse{
			Status: SendOnChainStatusSubmitted,
		})
	}

	// Re-reserve exactly the persisted anchors. Selection is NOT
	// re-run: the anchors are the intent's identity (and its
	// idempotency key), so a replay that picked different inputs would
	// be a different intent with a stale twin left behind.
	_, err := a.askManager(ctx, &actormsg.ReserveForfeitRequest{
		Outpoints: intent.Anchors,
	})
	if err != nil {
		a.logger(ctx).WarnS(ctx, "Dropping stale onchain send "+
			"intent: anchors no longer reservable", err,
			slog.Int("anchor_count", len(intent.Anchors)),
			slog.Int64("requested_at_unix", intent.RequestedAt))

		if delErr := a.store.DeletePendingIntent(
			ctx, intent.ID,
		); delErr != nil {
			return fn.Err[WalletResp](
				fmt.Errorf("delete stale onchain send "+
					"intent: %w", delErr),
			)
		}

		return fn.Ok[WalletResp](&SendOnChainResponse{
			Status: SendOnChainStatusSubmitted,
		})
	}

	// Release the reservation if anything below fails; the row stays
	// in place so the next start retries. context.WithoutCancel keeps
	// the cleanup alive across shutdown-time cancellation.
	committed := false
	defer func() {
		if committed {
			return
		}

		releaseCtx := context.WithoutCancel(ctx)
		releaseErr := a.releaseManagerForfeitStrict(
			releaseCtx, intent.Anchors,
		)
		if releaseErr != nil {
			a.logger(releaseCtx).WarnS(
				releaseCtx,
				"Failed to release replayed send "+
					"reservation",
				releaseErr,
			)
		}
	}()

	// Materialize the canonical amounts from the VTXO store. The
	// reservation gates concurrent consumers, so the read is race-free.
	var (
		selectedAmounts []btcutil.Amount
		totalSelected   btcutil.Amount
	)
	for _, op := range intent.Anchors {
		vtxo, err := a.vtxoReader.GetVTXO(ctx, op)
		if err != nil {
			return fn.Err[WalletResp](
				fmt.Errorf("load replayed send vtxo %s: %w",
					op, err),
			)
		}

		selectedAmounts = append(selectedAmounts, vtxo.Amount)
		totalSelected += vtxo.Amount
	}

	// Defensive re-validation of the bounded-mode change projection.
	// The anchors are the same VTXOs the original call validated, so a
	// failure here indicates store corruption rather than a user error;
	// the row is kept so the next start can retry after investigation.
	var change btcutil.Amount
	if !payload.SweepAll {
		if totalSelected <= payload.TargetAmountSat {
			return fn.Err[WalletResp](
				fmt.Errorf("replayed send shortfall: "+
					"selected %d, need >%d", totalSelected,
					payload.TargetAmountSat),
			)
		}
		change = totalSelected - payload.TargetAmountSat
		if change < payload.DustLimit {
			return fn.Err[WalletResp](
				fmt.Errorf("replayed send change %d below "+
					"dust limit %d", change,
					payload.DustLimit),
			)
		}
	}

	forfeits, leaves, vtxos, err := a.buildSendOnChainIntentPackage(
		ctx, *payload, intent.Anchors, selectedAmounts, change,
	)
	if err != nil {
		return fn.Err[WalletResp](
			fmt.Errorf("rebuild onchain send intent: %w", err),
		)
	}

	// Register with the round actor. Unlike the live RPC path — which
	// leaves TriggerRegistration false so darepod.TriggerRoundRegistration
	// can fire the IntentRequested step separately — replay has no RPC
	// caller to drive the second step, so the registration and trigger
	// collapse into one Ask. The latent-ctx interleaving documented at
	// handleSendOnChain's registration site cannot bite here because the
	// Ask context is fully detached (WithoutCancel of the startup replay
	// Ask, which itself outlives the FSM transition).
	serviceKey := actormsg.RoundActorServiceKey()
	roundRef := serviceKey.Ref(a.actorSystem)

	// Bound the Ask with a deadline. This handler runs via a self-Tell
	// into the single-worker wallet mailbox, so an unresponsive round
	// actor at startup would not just stall replay but head-of-line-block
	// every later wallet message. Unlike the live path, there is no RPC
	// caller deadline to fall back on, so we impose one. WithoutCancel
	// detaches from the startup ctx; the timeout caps the wait. On
	// timeout the error path below keeps the row and releases the
	// reservation, so the next start simply retries.
	askCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), replayRoundRegisterTimeout,
	)
	defer cancel()

	future := roundRef.Ask(askCtx, &actormsg.RegisterIntentMsg{
		Forfeits:            forfeits,
		VTXOs:               vtxos,
		Leaves:              leaves,
		TriggerRegistration: true,
	})
	result := future.Await(askCtx)
	if result.IsErr() {

		// Keep the row: the round actor may simply not be able to
		// admit the intent yet (e.g. operator unreachable during
		// startup). The deferred release returns the VTXOs to the
		// live set and the next start retries the replay.
		return fn.Err[WalletResp](
			fmt.Errorf(
				"round rejected replayed onchain send "+
					"intent: %w", result.Err(),
			),
		)
	}

	committed = true

	a.logger(ctx).InfoS(ctx, "Replayed onchain send intent registered",
		slog.Int("forfeits", len(forfeits)),
		slog.Int("leaves", len(leaves)),
		slog.Int("change_vtxos", len(vtxos)),
		slog.Int64("total_selected", int64(totalSelected)),
		slog.Bool("sweep_all", payload.SweepAll),
	)

	return fn.Ok[WalletResp](&SendOnChainResponse{
		Status:            SendOnChainStatusSubmitted,
		ActualAmountSat:   payload.TargetAmountSat,
		SelectedOutpoints: intent.Anchors,
		TotalSelected:     totalSelected,
		ChangeAmount:      change,
	})
}
