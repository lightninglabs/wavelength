package unroll

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/stretchr/testify/require"
)

// TestRegistryStopJoinsDetachedPersistence proves that Stop cannot release a
// shared database while a terminal control-plane write still owns it.
func TestRegistryStopJoinsDetachedPersistence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newBlockingRegistryStore()
		defer func() {
			select {
			case <-store.release:
			default:
				close(store.release)
			}
		}()
		registry := NewUnrollRegistryActor(RegistryConfig{Store: store})
		record := RegistryRecord{
			TargetOutpoint: wire.OutPoint{
				Index: 7,
			},
			Phase: PhaseCompleted,
		}
		registry.behavior.persistRecordAsync(
			record.TargetOutpoint, 0, record,
		)
		<-store.started

		stopped := make(chan struct{})
		go func() {
			registry.Stop()
			close(stopped)
		}()
		synctest.Wait()
		select {
		case <-stopped:
			t.Fatal("registry returned before its database write")

		default:
		}

		close(store.release)
		<-stopped
		stored, err := store.GetRecord(
			t.Context(), record.TargetOutpoint,
		)
		require.NoError(t, err)
		require.NotNil(t, stored)
		require.Equal(t, PhaseCompleted, stored.Phase)
	})
}

// retainedWatchRef models the chain's stable caller-ID registration keys.
// A replacement child registers those same keys before old cleanup finishes.
type retainedWatchRef struct {
	actor.ActorRef[chainsource.ChainSourceMsg, chainsource.ChainSourceResp]
	unregistered atomic.Int32
}

// Tell counts removals that would affect a replacement using the same IDs.
func (r *retainedWatchRef) Tell(_ context.Context,
	msg chainsource.ChainSourceMsg) error {

	switch msg.(type) {
	case *chainsource.UnsubscribeBlocksRequest,
		*chainsource.UnregisterSpendRequest:

		r.unregistered.Add(1)
	}

	return nil
}

// TestNativeStopPreservesReadmittedWatchIDs proves the new native/FSM join
// does not add delayed stable-ID removals to the existing watch lifecycle.
func TestNativeStopPreservesReadmittedWatchIDs(t *testing.T) {
	var workers sync.WaitGroup
	chain := &retainedWatchRef{}
	child, err := NewVTXOUnrollActor(Config{
		TargetOutpoint: wire.OutPoint{Index: 8},
		DeliveryStore:  newMemCheckpointStore(),
		ChainSource:    chain,
		workers:        &workers,
	})
	require.NoError(t, err)
	// The native worker has no messages. These flags model the previous
	// incarnation's active subscriptions, whose stable IDs a readmission
	// reuses. Stop's joined continuation must only stop its own FSM.
	child.behavior.blockSubActive = true
	child.behavior.spendWatchActive = true
	child.Stop()
	workers.Wait()
	require.Zero(t, chain.unregistered.Load())
}

// unclaimedAdmissionRef supplies only the identity needed by the watcher.
type unclaimedAdmissionRef struct {
	actor.ActorRef[Msg, Resp]
}

// ID names the unresolved child without completing its admission promise.
func (*unclaimedAdmissionRef) ID() string { return "unclaimed-child" }

// TestRegistryStopCancelsUnsettledAdmissionWatcher proves a pending Ask whose
// child has not claimed it cannot keep the joined registry alive indefinitely.
func TestRegistryStopCancelsUnsettledAdmissionWatcher(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		registry := NewUnrollRegistryActor(RegistryConfig{
			Store: newMemRegistryStore(),
		})
		promise := actor.NewPromise[Resp]()
		registry.behavior.watchChildAdmissionResult(
			context.Background(), wire.OutPoint{
				Index: 9,
			}, &VTXOUnrollActor{
				ref: &unclaimedAdmissionRef{},
			},
			promise.Future(),
		)
		registry.Stop()
	})
}
