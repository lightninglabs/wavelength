package lnruntime

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	clientdb "github.com/lightninglabs/wavelength/db"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// auditReadinessObservation captures the durable state visible when the
// production service dispatches negotiation.
type auditReadinessObservation struct {
	phase arkchannel.Phase
	err   error
}

// auditReadinessExecutor blocks negotiation so the RPC response and duplicate
// delivery behavior can be observed independently from action completion.
type auditReadinessExecutor struct {
	service *arkchannel.Service
	started chan auditReadinessObservation
	release chan struct{}
	calls   atomic.Int32
}

// ValidatePreparedOOR accepts the canonical test source.
func (*auditReadinessExecutor) ValidatePreparedOOR(context.Context,
	arkchannel.Terms, arkchannel.VTXOBinding) error {

	return nil
}

// Execute records the persisted phase before waiting for test release.
func (e *auditReadinessExecutor) Execute(ctx context.Context, id arkchannel.ID,
	action arkchannel.Action) error {

	if _, ok := action.(*arkchannel.NegotiateFunding); !ok {
		return fmt.Errorf("unexpected readiness action %T", action)
	}
	e.calls.Add(1)
	record, err := e.service.GetChannel(ctx, id)
	observation := auditReadinessObservation{err: err}
	if err == nil {
		observation.phase = record.Snapshot.Phase
	}
	e.started <- observation
	<-e.release

	return nil
}

// TestFundingPeerRPCReadinessHasOneActionOwner proves peer readiness only
// records a durable fact. The manifest path owns live execution, while a fresh
// service can recover the pending action after restart.
func TestFundingPeerRPCReadinessHasOneActionOwner(t *testing.T) {
	t.Parallel()

	record, _ := testReceiveIntentRecord(t)
	terms := record.Snapshot.Terms
	source := testIntentBinding(
		t, terms, terms.Capacity+1_000, 0,
	)
	rawStore := clientdb.NewTestDB(t)
	store := clientdb.NewStore(
		rawStore.DB, rawStore.Queries, rawStore.Backend(),
		btclog.Disabled,
	).NewArkChannelStore(clock.NewDefaultClock())
	coordinator, err := arkchannel.NewCoordinator(store)
	require.NoError(t, err)
	executor := &auditReadinessExecutor{
		started: make(chan auditReadinessObservation, 1),
		release: make(chan struct{}),
	}
	var releaseOnce sync.Once
	t.Cleanup(func() {
		releaseOnce.Do(func() {
			close(executor.release)
		})
	})
	service, err := arkchannel.NewService(
		arkchannel.PartyHub, coordinator, executor,
	)
	require.NoError(t, err)
	executor.service = service
	_, err = service.RegisterReceiveIntent(t.Context(), terms)
	require.NoError(t, err)
	_, err = service.RecordPreparedOOR(
		t.Context(), terms.ID, *source,
	)
	require.NoError(t, err)

	stored, err := service.GetChannel(t.Context(), terms.ID)
	require.NoError(t, err)
	server, _, _, _ := newAuditFundingRPCServerWithService(
		t, stored, service,
	)
	request, _, err := channelEventToRPC(
		terms.ID, &arkchannel.FundingPeerReady{},
	)
	require.NoError(t, err)

	response, err := server.ApplyChannelEvent(t.Context(), request)
	require.NoError(t, err)
	require.Equal(
		t, arkchannel.PhaseNegotiating.String(),
		response.GetChannel().GetPhase(),
	)
	select {
	case observation := <-executor.started:
		t.Fatalf("readiness started channel negotiation: %+v",
			observation)

	case <-time.After(100 * time.Millisecond):
	}
	require.Equal(t, int32(0), executor.calls.Load())

	resumeResult := make(chan error, 1)
	go func() {
		_, resumeErr := service.ResumeChannelAction(
			t.Context(), terms.ID,
		)
		resumeResult <- resumeErr
	}()
	select {
	case observation := <-executor.started:
		require.NoError(t, observation.err)
		require.Equal(
			t, arkchannel.PhaseNegotiating, observation.phase,
		)

	case <-time.After(time.Second):
		t.Fatal("manifest action did not start channel negotiation")
	}

	_, err = server.ApplyChannelEvent(t.Context(), request)
	require.NoError(t, err)
	select {
	case observation := <-executor.started:
		t.Fatalf("duplicate readiness started another action: %+v",
			observation)

	case <-time.After(100 * time.Millisecond):
	}
	require.Equal(t, int32(1), executor.calls.Load())

	releaseOnce.Do(func() {
		close(executor.release)
	})
	select {
	case err := <-resumeResult:
		require.NoError(t, err)

	case <-time.After(time.Second):
		t.Fatal("manifest action did not finish")
	}

	recoveryCoordinator, err := arkchannel.NewCoordinator(store)
	require.NoError(t, err)
	recoveryExecutor := &auditReadinessExecutor{
		started: make(chan auditReadinessObservation, 1),
		release: make(chan struct{}),
	}
	var recoveryReleaseOnce sync.Once
	t.Cleanup(func() {
		recoveryReleaseOnce.Do(func() {
			close(recoveryExecutor.release)
		})
	})
	recoveryService, err := arkchannel.NewService(
		arkchannel.PartyHub, recoveryCoordinator, recoveryExecutor,
	)
	require.NoError(t, err)
	recoveryExecutor.service = recoveryService

	recoveryResult := make(chan error, 1)
	go func() {
		recoveryResult <- recoveryService.Resume(t.Context())
	}()
	select {
	case observation := <-recoveryExecutor.started:
		require.NoError(t, observation.err)
		require.Equal(
			t, arkchannel.PhaseNegotiating, observation.phase,
		)

	case <-time.After(time.Second):
		t.Fatal("fresh service did not resume channel negotiation")
	}
	require.Equal(t, int32(1), recoveryExecutor.calls.Load())

	recoveryReleaseOnce.Do(func() {
		close(recoveryExecutor.release)
	})
	select {
	case err := <-recoveryResult:
		require.NoError(t, err)

	case <-time.After(time.Second):
		t.Fatal("recovered channel negotiation did not finish")
	}
}
