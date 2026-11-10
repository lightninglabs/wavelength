package unroll

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	chain "github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestAdmissionDoesNotWaitForChildIO proves that one started child cannot
// hold status and terminal bookkeeping behind its broadcast work.
func TestAdmissionDoesNotWaitForChildIO(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	registry, store, _, _ := newRegistryHarness(t, proof, desc)
	blocked := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	registry.behavior.spawnFunc = func(_ context.Context,
		target wire.OutPoint) (*VTXOUnrollActor, error) {

		behavior := actor.NewFunctionBehavior(
			func(ctx context.Context, msg Msg) fn.Result[Resp] {
				if _, ok := msg.(*StartUnrollRequest); ok {
					close(blocked)
					select {
					case <-release:
					case <-ctx.Done():
					}

					return fn.Ok[Resp](&AckResp{})
				}

				return fn.Ok[Resp](&GetStateResp{
					Started: true,
					Phase:   PhaseMaterializing,
				})
			},
		)

		//nolint:contextcheck // The test cleanup owns this child actor.
		return newTestUnrollChild(t, target, behavior), nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	future := registry.Ref().Ask(ctx, &EnsureUnrollRequest{
		Outpoint: proof.TargetOutpoint(),
	})
	select {
	case <-blocked:
	case <-ctx.Done():
		t.Fatal("child never started")
	}
	response, err := future.Await(ctx).Unpack()
	require.NoError(t, err, "admission must not wait for broadcast IO")
	admitted, ok := response.(*EnsureUnrollResp)
	require.True(t, ok)
	require.True(t, admitted.Created)
	cancel()
	queryCtx, queryCancel := context.WithTimeout(t.Context(), time.Second)
	defer queryCancel()
	_, err = registry.Ref().Ask(queryCtx, &GetStatusRequest{
		Outpoint: proof.TargetOutpoint(),
	}).Await(queryCtx).Unpack()
	require.NoError(t, err)
	_, err = registry.Ref().Ask(queryCtx, &UnrollTerminatedMsg{
		Outpoint: proof.TargetOutpoint(),
		ActorID:  actorIDForTarget(proof.TargetOutpoint()),
		Phase:    PhaseFailed, FailReason: "confirmed conflict",
		Conflicted: true,
	}).Await(queryCtx).Unpack()
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		record, err := store.GetRecord(
			t.Context(), proof.TargetOutpoint(),
		)

		return err == nil && record != nil && record.ConflictedFailure
	}, time.Second, time.Millisecond)
}

// heldRegistryHeight keeps backend IO open until the test releases it or the
// registry shuts down. The query's own context must own that lifetime.
type heldRegistryHeight struct {
	*fakeRegistryChainSourceRef
	entered  chan struct{}
	release  chan struct{}
	canceled chan struct{}
}

// Ask holds only best-height queries; all other chain requests use the fake.
func (h *heldRegistryHeight) Ask(ctx context.Context,
	msg chain.ChainSourceMsg) actor.Future[chain.ChainSourceResp] {

	if _, ok := msg.(*chain.BestHeightRequest); !ok {
		return h.fakeRegistryChainSourceRef.Ask(ctx, msg)
	}
	promise := actor.NewPromise[chain.ChainSourceResp]()
	close(h.entered)
	go func() {
		select {
		case <-h.release:
			promise.Complete(
				fn.Ok[chain.ChainSourceResp](
					&chain.BestHeightResponse{
						Height: 200,
					},
				),
			)

		case <-ctx.Done():
			close(h.canceled)
			promise.Complete(
				fn.Err[chain.ChainSourceResp](
					ctx.Err(),
				),
			)
		}
	}()

	return promise.Future()
}

// TestAdmissionHeightLookupHasRegistryLifetime proves that backend lookup
// neither blocks control-plane turns nor survives registry shutdown.
func TestAdmissionHeightLookupHasRegistryLifetime(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		name := "caller cancellation"
		if shutdown {
			name = "registry shutdown"
		}
		t.Run(name, func(t *testing.T) {
			proof := buildLinearProof(t)
			desc := testDescriptor(
				t, proof.TargetOutpoint(), proof.CSVDelay(),
			)
			registry, _, _, tx := newRegistryHarness(t, proof, desc)
			height := &heldRegistryHeight{
				fakeRegistryChainSourceRef: new(
					fakeRegistryChainSourceRef,
				),
				entered:  make(chan struct{}),
				release:  make(chan struct{}),
				canceled: make(chan struct{}),
			}
			registry.behavior.cfg.ChainSource = height
			ctx, cancel := context.WithTimeout(
				t.Context(), time.Second,
			)
			defer cancel()
			_, err := registry.Ref().Ask(ctx, &EnsureUnrollRequest{
				Outpoint: proof.TargetOutpoint(),
			}).Await(ctx).Unpack()
			require.NoError(t, err)
			<-height.entered
			cancel()
			if shutdown {
				registry.Stop()
				select {
				case <-height.canceled:
				case <-time.After(time.Second):
					t.Fatal(
						"registry shutdown left " +
							"admission IO running",
					)
				}

				return
			}
			statusCtx, statusCancel := context.WithTimeout(
				t.Context(), time.Second,
			)
			defer statusCancel()
			_, err = registry.Ref().Ask(
				statusCtx, &GetStatusRequest{
					Outpoint: proof.TargetOutpoint(),
				},
			).Await(statusCtx).Unpack()
			require.NoError(t, err)
			close(height.release)
			require.Eventually(t, func() bool {
				return tx.requestCount() > 0
			}, time.Second, time.Millisecond)
		})
	}
}

// TestAdmissionTimeoutKeepsUncertainChild proves that a handler timeout is
// not interpreted as proof that the admitted exit performed no side effect.
func TestAdmissionTimeoutKeepsUncertainChild(t *testing.T) {
	for _, outcome := range []error{
		context.Canceled,
		context.DeadlineExceeded,
	} {
		t.Run(outcome.Error(), func(t *testing.T) {
			proof := buildLinearProof(t)
			desc := testDescriptor(
				t, proof.TargetOutpoint(), proof.CSVDelay(),
			)
			registry, store, _, _ := newRegistryHarness(
				t, proof, desc,
			)
			started := make(chan struct{})
			registry.behavior.spawnFunc = func(_ context.Context,
				target wire.OutPoint) (*VTXOUnrollActor,
				error) {

				behavior := actor.NewFunctionBehavior(
					func(context.Context,
						Msg) fn.Result[Resp] {

						close(started)

						return fn.Err[Resp](outcome)
					},
				)

				//nolint:contextcheck // Test-owned.
				child := newTestUnrollChild(t, target, behavior)

				return child, nil
			}
			_, err := registry.Ref().Ask(
				t.Context(), &EnsureUnrollRequest{
					Outpoint: proof.TargetOutpoint(),
				},
			).Await(t.Context()).Unpack()
			require.NoError(t, err)
			<-started
			registry.behavior.admissionWorkers.Wait()
			record, err := store.GetRecord(
				t.Context(), proof.TargetOutpoint(),
			)
			require.NoError(t, err)
			require.Equal(t, PhasePending, record.Phase)
			require.False(t, record.RecoverableFailure)
			resp, err := registry.Ref().Ask(
				t.Context(), &GetStatusRequest{
					Outpoint: proof.TargetOutpoint(),
				},
			).Await(t.Context()).Unpack()
			require.NoError(t, err)
			status, ok := resp.(*GetStatusResp)
			require.True(t, ok)
			require.True(t, status.Active)
		})
	}
}

// TestPendingAdmissionRestoreKeepsPolicy covers restart after the registry
// persisted acceptance but before background startup created a checkpoint.
func TestPendingAdmissionRestoreKeepsPolicy(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	registry, store, checkpoints, _ := newRegistryHarness(t, proof, desc)
	registry.behavior.cfg.ExitSpendPolicyResolver =
		&recordingExitSpendPolicyResolver{}
	record := RegistryRecord{
		TargetOutpoint: proof.TargetOutpoint(),
		ActorID:        actorIDForTarget(proof.TargetOutpoint()),
		Phase:          PhasePending,
		Trigger:        TriggerManual,
		ExitPolicyKind: "vhtlc_claim",
		ExitPolicyRef:  "durable-recovery-reference",
	}
	require.NoError(t, store.UpsertRecord(t.Context(), record))
	require.NoError(t, registry.RestoreNonTerminal(t.Context()))
	checkpoint, err := checkpoints.LoadCheckpoint(
		t.Context(), record.ActorID,
	)
	require.NoError(t, err)
	require.NotNil(t, checkpoint)
	decoded, err := decodeCheckpoint(checkpoint.StateData)
	require.NoError(t, err)
	require.Equal(t, record.ExitPolicyKind, decoded.ExitPolicyKind)
	require.Equal(t, record.ExitPolicyRef, decoded.ExitPolicyRef)
}

// retryRegistryHeight fails the first tip lookup, then holds the retry so the
// test can inspect the accepted job before allowing startup to proceed.
type retryRegistryHeight struct {
	*heldRegistryHeight
	failed atomic.Bool
}

// Ask injects one transient backend failure without starting the child.
func (h *retryRegistryHeight) Ask(ctx context.Context,
	msg chain.ChainSourceMsg) actor.Future[chain.ChainSourceResp] {

	if _, height := msg.(*chain.BestHeightRequest); height &&
		!h.failed.Swap(true) {

		promise := actor.NewPromise[chain.ChainSourceResp]()
		promise.Complete(
			fn.Err[chain.ChainSourceResp](
				errors.New("transient backend unavailable"),
			),
		)

		return promise.Future()
	}

	return h.heldRegistryHeight.Ask(ctx, msg)
}

// TestAdmissionRetriesHeightWithoutRecoveringVTXO proves transient tip IO
// cannot terminate an accepted exit or authorize recovery to the live set.
func TestAdmissionRetriesHeightWithoutRecoveringVTXO(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	registry, store, _, tx := newRegistryHarness(t, proof, desc)
	observer := &captureExitObserver{}
	var ref actor.TellOnlyRef[vtxo.ManagerMsg] = observer
	registry.behavior.cfg.VTXOExitObserver = fn.Some(ref)
	height := &retryRegistryHeight{heldRegistryHeight: &heldRegistryHeight{
		fakeRegistryChainSourceRef: new(fakeRegistryChainSourceRef),
		entered:                    make(chan struct{}),
		release:                    make(chan struct{}),
		canceled:                   make(chan struct{}),
	}}
	registry.behavior.cfg.ChainSource = height
	_, err := registry.Ref().Ask(t.Context(), &EnsureUnrollRequest{
		Outpoint: proof.TargetOutpoint(),
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)
	select {
	case <-height.entered:
	case <-time.After(time.Second):
		t.Fatal("admission did not retry the failed height lookup")
	}
	record, err := store.GetRecord(t.Context(), proof.TargetOutpoint())
	require.NoError(t, err)
	require.Equal(t, PhasePending, record.Phase)
	require.False(t, record.RecoverableFailure)
	require.Empty(t, observer.notifications())
	require.Zero(t, tx.requestCount())
	close(height.release)
	require.Eventually(t, func() bool {
		return tx.requestCount() > 0
	}, time.Second, time.Millisecond)
	require.Empty(t, observer.notifications())
}
