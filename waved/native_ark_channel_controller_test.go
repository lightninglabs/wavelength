package waved

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/arkchannel/oorbridge"
	"github.com/lightninglabs/wavelength/lnruntime"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
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

type recordingPromotionPeer struct {
	lnruntime.ProcessFundingPeer

	boundSources []arkchannel.VTXOBinding
}

type noOpPromotionActionExecutor struct{}

// Execute accepts action-free requested-state replay in this test.
func (noOpPromotionActionExecutor) Execute(context.Context, arkchannel.ID,
	arkchannel.Action) error {

	return nil
}

type recordingProcessFundingPeer struct {
	lnruntime.ProcessFundingPeer

	state   lnruntime.FundingChannelState
	applied []arkchannel.Event
}

// GetFundingChannel returns the configured remote receive-intent state.
func (p *recordingProcessFundingPeer) GetFundingChannel(context.Context,
	arkchannel.ID) (lnruntime.FundingChannelState, error) {

	return p.state, nil
}

// ApplyChannelEvent records the remote cancellation and advances its fixture.
func (p *recordingProcessFundingPeer) ApplyChannelEvent(_ context.Context,
	_ arkchannel.ID, event arkchannel.Event) (arkchannel.Record, error) {

	p.applied = append(p.applied, event)
	p.state.Phase = arkchannel.PhaseFailed

	return arkchannel.Record{}, nil
}

type noOpArkChannelActionExecutor struct{}

// Execute accepts action-free requested-intent transitions in tests.
func (noOpArkChannelActionExecutor) Execute(context.Context, arkchannel.ID,
	arkchannel.Action) error {

	return nil
}

// BindPreparedOOR records one idempotent source replay.
func (p *recordingPromotionPeer) BindPreparedOOR(_ context.Context,
	_ arkchannel.ID, source arkchannel.VTXOBinding) (arkchannel.Record,
	error) {

	p.boundSources = append(p.boundSources, source)

	return arkchannel.Record{}, nil
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
// in-Ark refresh restores its lnd link in the quiesced state.
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

// TestWaitIncomingPaymentReadyJoinsBothBarriers verifies an accepted invoice
// cannot win before the matching channel has also become active.
func TestWaitIncomingPaymentReadyJoinsBothBarriers(t *testing.T) {
	t.Parallel()

	invoiceReady := make(chan struct{})
	channelReady := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- waitIncomingPaymentReady(
			t.Context(),
			func(context.Context) error {
				<-invoiceReady

				return nil
			},
			func(context.Context) error {
				<-channelReady

				return nil
			},
		)
	}()

	close(invoiceReady)
	select {
	case err := <-result:
		t.Fatalf("returned before channel activation: %v", err)

	case <-time.After(25 * time.Millisecond):
	}

	close(channelReady)
	require.NoError(t, <-result)
}

// TestWaitIncomingPaymentReadyPropagatesSyncFailure verifies a definitive
// channel failure reaches the SDK instead of silently disabling
// synchronization.
func TestWaitIncomingPaymentReadyPropagatesSyncFailure(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("channel failed")
	err := waitIncomingPaymentReady(
		t.Context(),
		func(ctx context.Context) error {
			<-ctx.Done()

			return ctx.Err()
		},
		func(context.Context) error {
			return wantErr
		},
	)
	require.ErrorIs(t, err, wantErr)
}

// TestReceiveIntentEndpointAgreement verifies only fully pre-PONR or fully
// retained endpoint pairs can be canceled or kept without an unsafe split.
func TestReceiveIntentEndpointAgreement(t *testing.T) {
	t.Parallel()

	require.False(
		t, receiveIntentEndpointsDisagree(
			false, arkchannel.PhaseRequested, false,
			arkchannel.PhaseNegotiating,
		),
	)
	require.False(
		t, receiveIntentEndpointsDisagree(
			false, arkchannel.PhaseActive, false,
			arkchannel.PhaseActive,
		),
	)
	require.True(
		t, receiveIntentEndpointsDisagree(
			false, arkchannel.PhaseActive, false,
			arkchannel.PhaseRequested,
		),
	)
}

// TestCancelReceiveIntentTerminatesBothRequestedEndpoints verifies another
// winning rail cannot leave a requested channel record live at either endpoint.
func TestCancelReceiveIntentTerminatesBothRequestedEndpoints(t *testing.T) {
	t.Parallel()

	now := time.Unix(35_000, 0).UTC()
	controller, coordinator, terms, closeStore := testPrePONRController(
		t, now, oorbridge.PreparationLookup{},
	)
	t.Cleanup(closeStore)
	hash := lntypes.Hash{4, 5, 6}
	terms.ID = arkchannel.ReceiveIntentID(hash)
	terms.PaymentHash = hash
	_, err := coordinator.Request(t.Context(), terms)
	require.NoError(t, err)
	service, err := arkchannel.NewService(
		controller.party, coordinator, noOpArkChannelActionExecutor{},
	)
	require.NoError(t, err)
	remote := &recordingProcessFundingPeer{
		state: lnruntime.FundingChannelState{
			Phase: arkchannel.PhaseRequested,
		},
	}
	controller.service = service
	controller.remote = remote

	err = controller.cancelReceiveIntent(
		t.Context(), hash, "vHTLC rail won",
	)
	require.NoError(t, err)
	require.Len(t, remote.applied, 1)
	_, ok := remote.applied[0].(*arkchannel.Fail)
	require.True(t, ok)
	record, err := service.GetChannel(t.Context(), terms.ID)
	require.NoError(t, err)
	require.Equal(t, arkchannel.PhaseFailed, record.Snapshot.Phase)
}

// TestEnsureClientStartedSharesProcessAttempt proves one canceled request does
// not cancel startup that another caller is waiting on.
func TestEnsureClientStartedSharesProcessAttempt(t *testing.T) {
	t.Parallel()

	startupErr := errors.New("startup failed")
	started := make(chan struct{})
	release := make(chan struct{})
	var starts atomic.Int32
	controller := &NativeArkChannelController{
		clientStarter: func(ctx context.Context) error {
			if starts.Add(1) == 1 {
				close(started)
			}

			select {
			case <-release:
				return startupErr

			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
	controller.initLifecycle(t.Context())
	t.Cleanup(func() {
		require.NoError(t, controller.Stop())
	})

	requestCtx, cancel := context.WithCancel(t.Context())
	firstResult := make(chan error, 1)
	go func() {
		firstResult <- controller.ensureClientStarted(requestCtx)
	}()
	<-started
	cancel()
	require.ErrorIs(t, <-firstResult, context.Canceled)

	secondResult := make(chan error, 1)
	go func() {
		secondResult <- controller.ensureClientStarted(t.Context())
	}()
	require.Never(t, func() bool {
		return starts.Load() != 1
	}, 25*time.Millisecond, time.Millisecond)
	close(release)
	require.ErrorIs(t, <-secondResult, startupErr)
	require.Equal(t, int32(1), starts.Load())
}

// TestArkChannelControllerStopCancelsStartup verifies concurrent shutdown is
// idempotent and waits for a process-owned startup attempt to exit.
func TestArkChannelControllerStopCancelsStartup(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	controller := &NativeArkChannelController{
		clientStarter: func(ctx context.Context) error {
			close(started)
			<-ctx.Done()

			return ctx.Err()
		},
	}
	controller.initLifecycle(t.Context())
	startResult := make(chan error, 1)
	go func() {
		startResult <- controller.ensureClientStarted(t.Context())
	}()
	<-started

	var shutdown sync.WaitGroup
	shutdown.Add(2)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			defer shutdown.Done()

			errs <- controller.Stop()
		}()
	}
	shutdown.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.ErrorIs(t, <-startResult, context.Canceled)
}

// TestArkChannelControllerStartsForExistingState proves startup recovery does
// not wait for a new RPC when the durable store already owns a channel.
func TestArkChannelControllerStartsForExistingState(t *testing.T) {
	t.Parallel()

	now := time.Unix(30_000, 0).UTC()
	controller, coordinator, terms, closeStore := testPrePONRController(
		t, now, oorbridge.PreparationLookup{
			Status: oorbridge.PreparationAbsent,
		},
	)
	t.Cleanup(closeStore)
	_, err := coordinator.Request(t.Context(), terms)
	require.NoError(t, err)
	controller.party = arkchannel.PartyClient
	controller.initLifecycle(t.Context())
	started := make(chan struct{})
	controller.clientStarter = func(ctx context.Context) error {
		close(started)
		<-ctx.Done()

		return ctx.Err()
	}
	controller.Start()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("existing Ark channel did not trigger client startup")
	}
	require.NoError(t, controller.Stop())
}

// TestPromotionIdentifiersAreStableAndScoped verifies a retried creation gets
// the same protocol identity without colliding across daemon identities.
func TestPromotionIdentifiersAreStableAndScoped(t *testing.T) {
	t.Parallel()

	first := &NativeArkChannelController{cfg: ArkChannelControllerConfig{
		IdentityKey: testKeyDescriptor(t, 70),
	}}
	second := &NativeArkChannelController{cfg: ArkChannelControllerConfig{
		IdentityKey: testKeyDescriptor(t, 71),
	}}

	firstID, firstPending, firstSCID :=
		first.promotionIdentifiers("invoice-42")
	retryID, retryPending, retrySCID :=
		first.promotionIdentifiers("invoice-42")
	require.Equal(t, firstID, retryID)
	require.Equal(t, firstPending, retryPending)
	require.Equal(t, firstSCID, retrySCID)
	reservedSCID := lnwire.NewShortChanIDFromInt(firstSCID)
	require.NotZero(t, reservedSCID.BlockHeight)
	require.Zero(t, reservedSCID.TxPosition)

	otherKeyID, _, _ := first.promotionIdentifiers("invoice-43")
	otherIdentityID, _, _ := second.promotionIdentifiers("invoice-42")
	require.NotEqual(t, firstID, otherKeyID)
	require.NotEqual(t, firstID, otherIdentityID)
}

// TestNewPromotionTermsRequiresIdempotencyKey verifies the controller cannot
// reintroduce an unrecoverable random channel identity below the RPC layer.
func TestNewPromotionTermsRequiresIdempotencyKey(t *testing.T) {
	t.Parallel()

	controller := &NativeArkChannelController{}
	_, err := controller.newPromotionTerms(100_000, "")
	require.ErrorContains(t, err, "idempotency key is required")
}

// TestResumeBoundPromotionReturnsActiveRecord verifies an RPC replay does not
// re-enter the prepared-OOR admission path after channel activation.
func TestResumeBoundPromotionReturnsActiveRecord(t *testing.T) {
	t.Parallel()

	record := arkchannel.Record{Snapshot: arkchannel.Snapshot{
		Terms: arkchannel.Terms{
			ID: arkchannel.ID{
				1,
			},
		},
		Phase: arkchannel.PhaseActive,
	}}
	controller := &NativeArkChannelController{}

	actual, err := controller.resumeBoundPromotion(t.Context(), record)
	require.NoError(t, err)
	require.Equal(t, record, actual)
}

// TestResumeBoundPromotionReplaysRequestedPeerBind verifies a crash after the
// local source commit can still prompt the peer readiness handshake.
func TestResumeBoundPromotionReplaysRequestedPeerBind(t *testing.T) {
	t.Parallel()

	now := time.Unix(40_000, 0).UTC()
	controller, coordinator, terms, closeStore := testPrePONRController(
		t, now, oorbridge.PreparationLookup{},
	)
	t.Cleanup(closeStore)
	_, err := coordinator.Request(t.Context(), terms)
	require.NoError(t, err)
	binding := testPrePONRBinding(t, terms)
	record, _, err := coordinator.Apply(
		t.Context(), terms.ID, &arkchannel.BindVTXO{
			Binding: binding,
		},
	)
	require.NoError(t, err)
	require.Equal(t, arkchannel.PhaseRequested, record.Snapshot.Phase)

	service, err := arkchannel.NewService(
		controller.party, coordinator, noOpPromotionActionExecutor{},
	)
	require.NoError(t, err)
	remote := &recordingPromotionPeer{}
	controller.service = service
	controller.remote = remote

	actual, err := controller.resumeBoundPromotion(t.Context(), record)
	require.NoError(t, err)
	require.Equal(t, arkchannel.PhaseRequested, actual.Snapshot.Phase)
	require.Equal(t, []arkchannel.VTXOBinding{binding}, remote.boundSources)
}
