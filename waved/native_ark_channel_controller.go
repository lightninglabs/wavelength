package waved

import (
	"fmt"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
)

// arkChannelBackingRestorer registers virtual funding before lnd restores its
// pending funding manager state.
type arkChannelBackingRestorer interface {
	RestoreBacking(arkchannel.Terms, arkchannel.Backing) error
}

// restoreNativeArkChannelBackingRecords registers every signed backing in one
// pre-start snapshot of the durable channel store.
func restoreNativeArkChannelBackingRecords(restorer arkChannelBackingRestorer,
	records []arkchannel.Record) error {

	failures := make([]arkchannel.ResumeFailure, 0)
	for _, record := range records {
		backing := record.Snapshot.Backing
		if backing == nil ||
			record.Snapshot.Phase == arkchannel.PhaseClosed {

			continue
		}
		if err := restorer.RestoreBacking(
			record.Snapshot.Terms, *backing,
		); err != nil {

			failures = append(failures, arkchannel.ResumeFailure{
				ChannelID: record.Snapshot.Terms.ID,
				Err: fmt.Errorf(
					"restore backing: %w", err,
				),
			})
		}
	}
	if len(failures) > 0 {
		return &arkchannel.ResumeFailures{Failures: failures}
	}

	return nil
}

// arkChannelForceCloseResumer reconciles one materialized channel with lnd's
// durable commitment-broadcast state.
type arkChannelForceCloseResumer interface {
	ResumeForceCloseChannel(wire.OutPoint) error
}

// resumeOnchainArkChannelRecords attempts every materialized channel and
// returns only isolated failures after the complete recovery pass.
func resumeOnchainArkChannelRecords(node arkChannelForceCloseResumer,
	records []arkchannel.Record) error {

	failures := make([]arkchannel.ResumeFailure, 0)
	for _, record := range records {
		if !shouldResumeOnchainArkChannel(record.Snapshot) {
			continue
		}
		if record.Snapshot.Backing == nil {
			failures = append(failures, arkchannel.ResumeFailure{
				ChannelID: record.Snapshot.Terms.ID,
				Err: fmt.Errorf(
					"materialized channel has no " +
						"backing",
				),
			})

			continue
		}
		if err := node.ResumeForceCloseChannel(
			record.Snapshot.Backing.ChannelPoint,
		); err != nil {

			failures = append(failures, arkchannel.ResumeFailure{
				ChannelID: record.Snapshot.Terms.ID,
				Err: fmt.Errorf("resume materialized channel: "+
					"%w",
					err),
			})
		}
	}
	if len(failures) > 0 {
		return &arkchannel.ResumeFailures{Failures: failures}
	}

	return nil
}

// shouldResumeOnchainArkChannel reports whether backing publication crossed
// the durable handoff but lnd may still need its commitment broadcast marker.
func shouldResumeOnchainArkChannel(snapshot arkchannel.Snapshot) bool {
	return snapshot.Phase == arkchannel.PhaseOnChain
}

// shouldWatchArkChannel reports whether lnd must own the channel's chain and
// contract lifecycle. An unpublished outpoint remains dormant, while an
// observed publication can be handled without first contacting the peer.
func shouldWatchArkChannel(phase arkchannel.Phase) bool {
	switch phase {
	case arkchannel.PhaseActivating, arkchannel.PhaseActive,
		arkchannel.PhaseMaterializing, arkchannel.PhaseOnChain,
		arkchannel.PhaseCoopClosing,
		arkchannel.PhaseCoopCloseSigned,
		arkchannel.PhaseCoopClosePublished:
		return true

	default:
		return false
	}
}

// shouldRestoreArkChannelAddsDisabled reports whether a durable cooperative
// close artifact requires a restored lnd link to remain quiesced.
func shouldRestoreArkChannelAddsDisabled(phase arkchannel.Phase) bool {
	switch phase {
	case arkchannel.PhaseCoopClosing,
		arkchannel.PhaseCoopCloseSigned,
		arkchannel.PhaseCoopClosePublished:
		return true

	default:
		return false
	}
}
