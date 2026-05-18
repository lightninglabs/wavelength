package unroll

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// fakeEffectRegistryRef records unroll effect replay requests.
type fakeEffectRegistryRef struct {
	mu       sync.Mutex
	requests []*replayUnrollEffectMsg
	err      error
}

// ID returns the fake actor ID.
func (f *fakeEffectRegistryRef) ID() string {
	return "fake-effect-registry"
}

// Tell is unused by these tests.
func (f *fakeEffectRegistryRef) Tell(context.Context, RegistryMsg) error {
	return nil
}

// Ask records replay requests.
func (f *fakeEffectRegistryRef) Ask(_ context.Context,
	msg RegistryMsg) actor.Future[RegistryResp] {

	promise := actor.NewPromise[RegistryResp]()
	req, ok := msg.(*replayUnrollEffectMsg)
	if !ok {
		promise.Complete(
			fn.Err[RegistryResp](
				fmt.Errorf("unexpected registry msg %T", msg),
			),
		)

		return promise.Future()
	}

	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()

	if f.err != nil {
		promise.Complete(fn.Err[RegistryResp](f.err))

		return promise.Future()
	}

	promise.Complete(fn.Ok[RegistryResp](&RegistryAckResp{}))

	return promise.Future()
}

// requestCount returns the number of recorded replay requests.
func (f *fakeEffectRegistryRef) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.requests)
}

// lastRequest returns the latest recorded replay request.
func (f *fakeEffectRegistryRef) lastRequest(
	t *testing.T) *replayUnrollEffectMsg {

	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()

	require.NotEmpty(t, f.requests)

	return f.requests[len(f.requests)-1]
}

// TestEffectWorkerRejectsUnknownEffectType verifies that the SQL effect
// enum is enforced before the worker asks the registry to replay a target.
func TestEffectWorkerRejectsUnknownEffectType(t *testing.T) {
	t.Parallel()

	registry := &fakeEffectRegistryRef{}
	worker := &EffectWorker{registry: registry}

	err := worker.handleEffect(t.Context(), db.UnrollEffectRecord{
		EffectType: "new_effect_without_runtime_contract",
	})
	require.ErrorContains(t, err, "unknown unroll effect type")
	require.Zero(t, registry.requestCount())
}

// TestEffectWorkerReplaysKnownEffectType verifies that known effect rows all
// use the single target replay path. The actor derives the concrete pending
// work from SQL instead of the worker duplicating FSM dispatch logic.
func TestEffectWorkerReplaysKnownEffectType(t *testing.T) {
	t.Parallel()

	target := wire.OutPoint{
		Hash: chainhash.Hash{
			0x01,
			0x02,
			0x03,
		},
		Index: 7,
	}
	for effectType := range validUnrollEffectTypes {
		effectType := effectType
		t.Run(effectType, func(t *testing.T) {
			t.Parallel()

			registry := &fakeEffectRegistryRef{}
			worker := &EffectWorker{registry: registry}

			err := worker.handleEffect(
				t.Context(), db.UnrollEffectRecord{
					TargetOutpoint: target,
					EffectType:     effectType,
				},
			)
			require.NoError(t, err)
			require.Equal(
				t, target, registry.lastRequest(t).Outpoint,
			)
		})
	}
}
