//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"context"
	"fmt"
	"time"

	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/protobuf/proto"
)

// roundStatusProvider is optional for embeddings that predate round progress.
// A bounded actor snapshot supplies each activity derivation.
type roundStatusProvider interface {
	ListRounds(context.Context,
		*waverpc.ListRoundsRequest) (*waverpc.ListRoundsResponse, error)
}

// applyBatchWaits annotates existing pending entries, never creating a new
// payment or deposit. Reconciliation persists changes through the canonical
// projector, which suppresses duplicate events for unchanged timing metadata.
func (h *history) applyBatchWaits(ctx context.Context,
	entries []*wavewalletrpc.WalletEntry) error {

	provider, ok := h.deps.RPCServer.(roundStatusProvider)
	if !ok {
		return nil
	}

	// Terminal entries cannot be waiting for registration. Avoid an actor
	// query when the page contains nothing that could match an active
	// round.
	relevant := false
	for _, entry := range entries {
		if batchWaitEligible(entry) {
			relevant = true
			break
		}
	}
	if !relevant {
		return nil
	}

	// Progress reads must not stall the daemon's reconciliation loop if
	// the round actor is busy. This child context owns only the snapshot.
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rounds, err := provider.ListRounds(queryCtx, &waverpc.ListRoundsRequest{
		StateFilter: waverpc.RoundState_ROUND_STATE_PENDING_ASSEMBLY,
	})
	if err != nil {
		return fmt.Errorf("query pending batch windows: %w", err)
	}

	deposits := make(map[string]*waverpc.RoundInfo)
	inputs := make(map[string]*waverpc.RoundInfo)
	for _, info := range rounds.GetRounds() {
		if info.GetBatchWindow() == nil {
			continue
		}
		for _, address := range info.GetBoardingAddresses() {
			rememberEarliestWait(deposits, address, info)
		}
		for _, outpoint := range info.GetInputOutpoints() {
			rememberEarliestWait(inputs, outpoint, info)
		}
	}

	for _, entry := range entries {
		if !batchWaitEligible(entry) {
			continue
		}

		if entry.Progress == nil {
			entry.Progress = &wavewalletrpc.WalletEntryProgress{}
		}
		entry.Progress.WaitingReason = ""
		entry.Progress.BatchWindow = nil

		var info *waverpc.RoundInfo
		switch entry.GetKind() {
		case wavewalletrpc.EntryKind_ENTRY_KIND_DEPOSIT:
			address := entry.
				GetRequest().
				GetOnchainAddress().
				GetAddress()
			info = deposits[address]

		case wavewalletrpc.EntryKind_ENTRY_KIND_EXIT:
			info = inputs[entry.GetProgress().GetVtxoOutpoint()]

		default:
			continue
		}
		if info == nil {
			continue
		}

		window := info.GetBatchWindow()
		entry.Progress.WaitingReason = info.GetWaitingReason()
		batchWindow := &wavewalletrpc.ScheduledBatchWindow{
			RegistrationOpensUnix: window.RegistrationOpensUnix,
			CutoffUnix:            window.GetCutoffUnix(),
			PlannedJoinUnixMs:     window.GetPlannedJoinUnixMs(),
		}
		entry.Progress.BatchWindow = batchWindow
	}

	return nil
}

// batchWaitEligible limits correlation to pending deposits and cooperative
// sends. A unilateral exit must not inherit the schedule of a round input.
func batchWaitEligible(entry *wavewalletrpc.WalletEntry) bool {
	if entry.GetStatus() != wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING {
		return false
	}

	switch entry.GetKind() {
	case wavewalletrpc.EntryKind_ENTRY_KIND_DEPOSIT:
		return true

	case wavewalletrpc.EntryKind_ENTRY_KIND_EXIT:
		return entry.GetRequest().GetOnchainAddress() != nil

	default:
		return false
	}
}

// rememberEarliestWait makes overlapping address matches deterministic even
// though actor snapshots originate from a map. A grouped deposit displays its
// earliest outstanding registration opportunity.
func rememberEarliestWait(waits map[string]*waverpc.RoundInfo, key string,
	info *waverpc.RoundInfo) {

	previous := waits[key]
	if previous == nil {
		waits[key] = info

		return
	}

	window, prevWindow := info.BatchWindow, previous.BatchWindow
	earlierJoin := window.PlannedJoinUnixMs < prevWindow.PlannedJoinUnixMs
	if window.CutoffUnix < prevWindow.CutoffUnix ||
		(window.CutoffUnix == prevWindow.CutoffUnix &&
			earlierJoin) {

		waits[key] = info
	}
}

// preserveActivityBatchWait retains only optional wait metadata when its source
// is unavailable. This runs under projectMu with the existing-row read so a
// concurrent projection cannot be overwritten with an older wait. Terminal
// lifecycle changes remain authoritative and always clear the stored window.
func preserveActivityBatchWait(existing, next *wavewalletrpc.WalletEntry) {
	if !batchWaitEligible(next) ||
		existing.GetProgress().GetBatchWindow() == nil {
		return
	}

	if next.Progress == nil {
		next.Progress = &wavewalletrpc.WalletEntryProgress{}
	}
	next.Progress.WaitingReason = existing.GetProgress().GetWaitingReason()
	next.Progress.BatchWindow = &wavewalletrpc.ScheduledBatchWindow{}
	proto.Merge(
		next.Progress.BatchWindow,
		existing.GetProgress().GetBatchWindow(),
	)
}
