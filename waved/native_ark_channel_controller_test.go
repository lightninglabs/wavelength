package waved

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/lnruntime"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

type recordingArkChannelBackingRestorer struct {
	restored []arkchannel.ID
	failID   arkchannel.ID
}

type recordingArkChannelForceCloseResumer struct {
	failPoint wire.OutPoint
	resumed   []wire.OutPoint
}

type recordingProcessPaymentPeer struct {
	lnruntime.ProcessPaymentPeer

	cancelContextErr error
	cancelHash       lntypes.Hash
	cancelReason     string
	cancelErr        error
}

// CancelOutgoingPayment records the cleanup context and requested failure.
func (p *recordingProcessPaymentPeer) CancelOutgoingPayment(ctx context.Context,
	hash lntypes.Hash, reason string) error {

	p.cancelContextErr = ctx.Err()
	p.cancelHash = hash
	p.cancelReason = reason

	return p.cancelErr
}

// ResumeForceCloseChannel records each independent on-chain reconciliation.
func (r *recordingArkChannelForceCloseResumer) ResumeForceCloseChannel(
	channelPoint wire.OutPoint) error {

	r.resumed = append(r.resumed, channelPoint)
	if channelPoint == r.failPoint {
		return fmt.Errorf("injected force-close resume failure")
	}

	return nil
}

// RestoreBacking records each durable backing passed to native lnd startup.
func (r *recordingArkChannelBackingRestorer) RestoreBacking(
	terms arkchannel.Terms, _ arkchannel.Backing) error {

	r.restored = append(r.restored, terms.ID)
	if terms.ID == r.failID {
		return fmt.Errorf("injected backing restore failure")
	}

	return nil
}

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

// TestShouldRestoreArkChannelAddsDisabled verifies only an in-progress
// cooperative close restores its lnd link in the quiesced state.
func TestShouldRestoreArkChannelAddsDisabled(t *testing.T) {
	t.Parallel()

	quiesced := []arkchannel.Phase{
		arkchannel.PhaseCoopClosing,
		arkchannel.PhaseCoopCloseSigned,
		arkchannel.PhaseCoopClosePublished,
	}
	for phase := arkchannel.PhaseRequested; phase <=
		arkchannel.PhaseCoopClosePublished; phase++ {

		expected := false
		for _, closePhase := range quiesced {
			if phase == closePhase {
				expected = true
				break
			}
		}
		require.Equal(
			t, expected, shouldRestoreArkChannelAddsDisabled(phase),
			phase.String(),
		)
	}
}

// TestRestoreNativeArkChannelBackingRecords verifies every live signed backing
// is registered before lnd startup while unsigned and archived channels are
// skipped.
func TestRestoreNativeArkChannelBackingRecords(t *testing.T) {
	t.Parallel()

	firstID := arkchannel.ID{1}
	secondID := arkchannel.ID{2}
	thirdID := arkchannel.ID{3}
	closedID := arkchannel.ID{4}
	restorer := &recordingArkChannelBackingRestorer{}
	records := []arkchannel.Record{
		{
			Snapshot: arkchannel.Snapshot{
				Terms: arkchannel.Terms{
					ID: firstID,
				},
				Backing: &arkchannel.Backing{
					Transaction: []byte{
						1,
					},
				},
			},
		},
		{
			Snapshot: arkchannel.Snapshot{
				Terms: arkchannel.Terms{
					ID: arkchannel.ID{
						9,
					},
				},
			},
		},
		{
			Snapshot: arkchannel.Snapshot{
				Terms: arkchannel.Terms{
					ID: secondID,
				},
				Backing: &arkchannel.Backing{
					Transaction: []byte{
						2,
					},
				},
			},
		},
		{
			Snapshot: arkchannel.Snapshot{
				Terms: arkchannel.Terms{
					ID: thirdID,
				},
				Backing: &arkchannel.Backing{
					Transaction: []byte{
						3,
					},
				},
			},
		},
		{
			Snapshot: arkchannel.Snapshot{
				Terms: arkchannel.Terms{
					ID: closedID,
				},
				Phase: arkchannel.PhaseClosed,
				Backing: &arkchannel.Backing{
					Transaction: []byte{
						4,
					},
				},
			},
		},
	}
	require.NoError(
		t, restoreNativeArkChannelBackingRecords(
			restorer, records,
		),
	)
	require.Equal(
		t, []arkchannel.ID{firstID, secondID, thirdID},
		restorer.restored,
	)

	restorer = &recordingArkChannelBackingRestorer{failID: secondID}
	err := restoreNativeArkChannelBackingRecords(restorer, records)
	var failures *arkchannel.ResumeFailures
	require.ErrorAs(t, err, &failures)
	require.Len(t, failures.Failures, 1)
	require.Equal(t, secondID, failures.Failures[0].ChannelID)
	require.ErrorContains(
		t, failures.Failures[0].Err, "injected backing restore failure",
	)
	require.Equal(
		t, []arkchannel.ID{firstID, secondID, thirdID},
		restorer.restored,
	)
}

// TestResumeOnchainArkChannelRecordsIsolatesFailures verifies one broken lnd
// close record does not suppress reconciliation for another channel.
func TestResumeOnchainArkChannelRecordsIsolatesFailures(t *testing.T) {
	t.Parallel()

	firstPoint := wire.OutPoint{Index: 1}
	secondPoint := wire.OutPoint{Index: 2}
	resumer := &recordingArkChannelForceCloseResumer{
		failPoint: firstPoint,
	}
	records := []arkchannel.Record{
		{
			Snapshot: arkchannel.Snapshot{
				Terms: arkchannel.Terms{
					ID: arkchannel.ID{
						1,
					},
				},
				Phase: arkchannel.PhaseOnChain,
				Backing: &arkchannel.Backing{
					ChannelPoint: firstPoint,
				},
			},
		},
		{
			Snapshot: arkchannel.Snapshot{
				Terms: arkchannel.Terms{
					ID: arkchannel.ID{
						9,
					},
				},
				Phase: arkchannel.PhaseActive,
			},
		},
		{
			Snapshot: arkchannel.Snapshot{
				Terms: arkchannel.Terms{
					ID: arkchannel.ID{
						2,
					},
				},
				Phase: arkchannel.PhaseOnChain,
				Backing: &arkchannel.Backing{
					ChannelPoint: secondPoint,
				},
			},
		},
	}
	err := resumeOnchainArkChannelRecords(resumer, records)
	var failures *arkchannel.ResumeFailures
	require.ErrorAs(t, err, &failures)
	require.Len(t, failures.Failures, 1)
	require.Equal(t, arkchannel.ID{1}, failures.Failures[0].ChannelID)
	require.Equal(
		t, []wire.OutPoint{firstPoint, secondPoint}, resumer.resumed,
	)
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

// TestCancelOutgoingPaymentDetachesFromRequest proves every successful
// preparation gets a cleanup attempt even after its request is canceled.
func TestCancelOutgoingPaymentDetachesFromRequest(t *testing.T) {
	t.Parallel()

	requestCtx, cancel := context.WithCancel(t.Context())
	cancel()
	cause := errors.New("private payment failed")
	hash := lntypes.Hash{1, 2, 3}
	peer := &recordingProcessPaymentPeer{}
	controller := &NativeArkChannelController{paymentPeer: peer}

	err := controller.cancelOutgoingPayment(requestCtx, hash, cause)
	require.ErrorIs(t, err, cause)
	require.NoError(t, peer.cancelContextErr)
	require.Equal(t, hash, peer.cancelHash)
	require.Equal(t, cause.Error(), peer.cancelReason)

	peer.cancelErr = errors.New("cleanup failed")
	err = controller.cancelOutgoingPayment(requestCtx, hash, cause)
	require.ErrorIs(t, err, cause)
	require.ErrorIs(t, err, peer.cancelErr)
}
