package waved

import (
	"testing"

	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/stretchr/testify/require"
)

// TestShouldWatchArkChannel verifies restart admission follows durable channel
// ownership instead of admitting every channel in lnd's database.
func TestShouldWatchArkChannel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		phase arkchannel.Phase
		watch bool
	}{
		{
			name:  "requested",
			phase: arkchannel.PhaseRequested,
		},
		{
			name:  "negotiating",
			phase: arkchannel.PhaseNegotiating,
		},
		{
			name:  "backing ready",
			phase: arkchannel.PhaseBackingReady,
		},
		{
			name:  "activating",
			phase: arkchannel.PhaseActivating,
			watch: true,
		},
		{
			name:  "active",
			phase: arkchannel.PhaseActive,
			watch: true,
		},
		{
			name:  "materializing",
			phase: arkchannel.PhaseMaterializing,
			watch: true,
		},
		{
			name:  "on chain",
			phase: arkchannel.PhaseOnChain,
			watch: true,
		},
		{
			name:  "closed",
			phase: arkchannel.PhaseClosed,
		},
		{
			name:  "cancelling",
			phase: arkchannel.PhaseCancelling,
		},
		{
			name:  "failed",
			phase: arkchannel.PhaseFailed,
		},
		{
			name:  "coop closing",
			phase: arkchannel.PhaseCoopClosing,
			watch: true,
		},
		{
			name:  "coop close signed",
			phase: arkchannel.PhaseCoopCloseSigned,
			watch: true,
		},
		{
			name:  "coop close published",
			phase: arkchannel.PhaseCoopClosePublished,
			watch: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			actual := shouldWatchArkChannel(test.phase)
			require.Equal(t, test.watch, actual)
		})
	}
}

// TestShouldResumeOnchainArkChannel verifies either recovery-ready endpoint
// re-drives lnd commitment publication after backing materialization.
func TestShouldResumeOnchainArkChannel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		phase    arkchannel.Phase
		expected bool
	}{
		{
			name:  "on-chain channel resumes commitment",
			phase: arkchannel.PhaseOnChain, expected: true,
		},
		{
			name:  "active source has nothing to resume",
			phase: arkchannel.PhaseActive,
		},
		{
			name:  "closed source has nothing to resume",
			phase: arkchannel.PhaseClosed,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			snapshot := arkchannel.Snapshot{Phase: test.phase}
			require.Equal(
				t, test.expected,
				shouldResumeOnchainArkChannel(snapshot),
			)
		})
	}
}
