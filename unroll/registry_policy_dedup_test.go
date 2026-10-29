package unroll

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/recovery"
	"github.com/lightninglabs/wavelength/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

const (
	// vhtlcClaimTestKind is the durable identity a recovery admission
	// uses for the vHTLC claim leaf.
	vhtlcClaimTestKind = ExitPolicyKind("vhtlc_claim")

	// vhtlcClaimTestRef identifies one armed recovery obligation.
	vhtlcClaimTestRef = "recovery-1"
)

// vhtlcClaimTestPolicy is a descriptor-backed policy that reports the vHTLC
// claim kind, so admission tests can exercise a non-standard binding without
// importing the recovery policy implementation.
type vhtlcClaimTestPolicy struct {
	*StandardVTXOExitSpendPolicy
}

// Kind returns the non-standard durable policy identity.
func (p *vhtlcClaimTestPolicy) Kind() ExitPolicyKind {
	return vhtlcClaimTestKind
}

// TestRegistryRejectsMismatchedPolicyOnDedup verifies that a duplicate
// admission naming a different exit policy than the record already bound to
// the target is rejected with the typed mismatch error on every dedup path:
// the in-memory active child, the in-memory pending record, and the durable
// record.
func TestRegistryRejectsMismatchedPolicyOnDedup(t *testing.T) {
	target := wire.OutPoint{Index: 0}

	t.Run("active", func(t *testing.T) {
		proof := buildLinearProof(t)
		desc := testDescriptor(
			t, proof.TargetOutpoint(), proof.CSVDelay(),
		)
		registry, store, _, _ := newRegistryHarness(t, proof, desc)

		// Bind the target to the standard VTXO timeout policy.
		_, err := registry.Ref().Ask(
			t.Context(), &EnsureUnrollRequest{
				Outpoint: proof.TargetOutpoint(),
				Trigger:  TriggerManual,
			},
		).Await(t.Context()).Unpack()
		require.NoError(t, err)

		// A second admission for the same outpoint under a recovery
		// policy must not silently dedup as Created=false.
		_, err = registry.Ref().Ask(
			t.Context(), &EnsureUnrollRequest{
				Outpoint:       proof.TargetOutpoint(),
				Trigger:        TriggerManual,
				ExitPolicyKind: vhtlcClaimTestKind,
				ExitPolicyRef:  vhtlcClaimTestRef,
			},
		).Await(t.Context()).Unpack()
		require.Error(t, err)
		require.ErrorIs(t, err, ErrExitPolicyMismatch)

		var mismatch *ExitPolicyMismatchError
		require.True(t, errors.As(err, &mismatch))
		require.Equal(
			t, StandardVTXOTimeoutExitPolicyKind,
			mismatch.BoundKind,
		)
		require.Equal(t, vhtlcClaimTestKind, mismatch.RequestedKind)
		require.Equal(t, vhtlcClaimTestRef, mismatch.RequestedRef)

		// The rejected admission left the recorded binding untouched.
		record, err := store.GetRecord(
			t.Context(), proof.TargetOutpoint(),
		)
		require.NoError(t, err)
		require.NotNil(t, record)
		require.Equal(
			t, StandardVTXOTimeoutExitPolicyKind,
			record.ExitPolicyKind,
		)
		require.Empty(t, record.ExitPolicyRef)
	})

	t.Run("pending", func(t *testing.T) {
		behavior := newPolicyTestBehavior(t)
		behavior.pending[target] = RegistryRecord{
			TargetOutpoint: target,
			Trigger:        TriggerManual,
			Phase:          PhaseMaterializing,
		}

		_, err := behavior.handleEnsure(
			t.Context(), &EnsureUnrollRequest{
				Outpoint:       target,
				Trigger:        TriggerManual,
				ExitPolicyKind: vhtlcClaimTestKind,
				ExitPolicyRef:  vhtlcClaimTestRef,
			},
		).Unpack()
		require.ErrorIs(t, err, ErrExitPolicyMismatch)
	})

	t.Run("store", func(t *testing.T) {
		behavior := newPolicyTestBehavior(t)
		require.NoError(
			t,
			behavior.cfg.Store.UpsertRecord(
				t.Context(), RegistryRecord{
					TargetOutpoint: target,
					Trigger:        TriggerManual,
					Phase:          PhaseMaterializing,
				},
			),
		)

		_, err := behavior.handleEnsure(
			t.Context(), &EnsureUnrollRequest{
				Outpoint:       target,
				Trigger:        TriggerManual,
				ExitPolicyKind: vhtlcClaimTestKind,
				ExitPolicyRef:  vhtlcClaimTestRef,
			},
		).Unpack()
		require.ErrorIs(t, err, ErrExitPolicyMismatch)
	})
}

// TestRegistryMatchingPolicyDedupIsIdempotent verifies a duplicate admission
// that carries the same (kind, ref) binding as the existing record stays
// idempotent: no error, Created=false, same actor.
func TestRegistryMatchingPolicyDedupIsIdempotent(t *testing.T) {
	target := wire.OutPoint{Index: 0}
	behavior := newPolicyTestBehavior(t)
	require.NoError(
		t,
		behavior.cfg.Store.UpsertRecord(
			t.Context(), RegistryRecord{
				TargetOutpoint: target,
				ActorID:        actorIDForTarget(target),
				Trigger:        TriggerManual,
				ExitPolicyKind: vhtlcClaimTestKind,
				ExitPolicyRef:  vhtlcClaimTestRef,
				Phase:          PhaseCompleted,
			},
		),
	)

	resp, err := behavior.handleEnsure(
		t.Context(), &EnsureUnrollRequest{
			Outpoint:       target,
			Trigger:        TriggerManual,
			ExitPolicyKind: vhtlcClaimTestKind,
			ExitPolicyRef:  vhtlcClaimTestRef,
		},
	).Unpack()
	require.NoError(t, err)

	ensureResp, ok := resp.(*EnsureUnrollResp)
	require.True(t, ok)
	require.False(t, ensureResp.Created)
	require.Equal(t, actorIDForTarget(target), ensureResp.ActorID)
}

// TestRegistryPolicyLessDedupKeepsBoundPolicy verifies the historical dedup
// semantics survive for a caller that names no policy and inherits none: the
// record already bound to the target keeps driving it, so a differing binding
// is a no-op rather than an error. The manual Unroll RPC, critical expiry, and
// the fraud watcher all depend on that idempotency.
func TestRegistryPolicyLessDedupKeepsBoundPolicy(t *testing.T) {
	target := wire.OutPoint{Index: 0}
	behavior := newPolicyTestBehavior(t)
	require.NoError(
		t,
		behavior.cfg.Store.UpsertRecord(
			t.Context(), RegistryRecord{
				TargetOutpoint: target,
				ActorID:        actorIDForTarget(target),
				Trigger:        TriggerRestart,
				ExitPolicyKind: vhtlcClaimTestKind,
				ExitPolicyRef:  vhtlcClaimTestRef,
				Phase:          PhaseCompleted,
			},
		),
	)

	resp, err := behavior.handleEnsure(
		t.Context(), &EnsureUnrollRequest{
			Outpoint: target,
			Trigger:  TriggerCriticalExpiry,
		},
	).Unpack()
	require.NoError(t, err)

	ensureResp, ok := resp.(*EnsureUnrollResp)
	require.True(t, ok)
	require.False(t, ensureResp.Created)
	require.Equal(t, actorIDForTarget(target), ensureResp.ActorID)

	// The dedup left the recorded binding untouched.
	record, err := behavior.cfg.Store.GetRecord(t.Context(), target)
	require.NoError(t, err)
	require.NotNil(t, record)
	require.Equal(t, vhtlcClaimTestKind, record.ExitPolicyKind)
	require.Equal(t, vhtlcClaimTestRef, record.ExitPolicyRef)
}

// TestRegistryPolicyLessDedupRejectsInheritedMismatch verifies the policy-less
// dedup exemption does not swallow the conflict this check exists to catch: a
// request that inherits an armed recovery obligation must still be rejected
// when the record bound to the target names a different policy.
func TestRegistryPolicyLessDedupRejectsInheritedMismatch(t *testing.T) {
	target := wire.OutPoint{Index: 0}
	behavior := newPolicyTestBehavior(t)
	behavior.cfg.ArmedExitPolicy = func(context.Context, wire.OutPoint) (
		ExitPolicyKind, string, bool, error) {

		return vhtlcClaimTestKind, vhtlcClaimTestRef, true, nil
	}
	require.NoError(
		t,
		behavior.cfg.Store.UpsertRecord(
			t.Context(), RegistryRecord{
				TargetOutpoint: target,
				ActorID:        actorIDForTarget(target),
				Trigger:        TriggerFraudSpend,
				Phase:          PhaseCompleted,
			},
		),
	)

	_, err := behavior.handleEnsure(
		t.Context(), &EnsureUnrollRequest{
			Outpoint: target,
			Trigger:  TriggerFraudSpend,
		},
	).Unpack()
	require.ErrorIs(t, err, ErrExitPolicyMismatch)

	var mismatch *ExitPolicyMismatchError
	require.True(t, errors.As(err, &mismatch))
	require.Equal(t, vhtlcClaimTestKind, mismatch.RequestedKind)
	require.Equal(t, vhtlcClaimTestRef, mismatch.RequestedRef)
}

// TestRegistryMatchingPolicyDedupSkipsArmedLookup verifies an explicit
// duplicate that names the identity already bound to the target is answered
// without consulting the armed recovery lookup. The binding cannot change
// under such a request, so a failing recovery read must not turn an
// idempotent repeat into an error.
func TestRegistryMatchingPolicyDedupSkipsArmedLookup(t *testing.T) {
	target := wire.OutPoint{Index: 0}
	lookupErr := errors.New("recovery store unavailable")

	var lookups atomic.Int32
	behavior := newPolicyTestBehavior(t)
	behavior.cfg.ArmedExitPolicy = func(context.Context, wire.OutPoint) (
		ExitPolicyKind, string, bool, error) {

		lookups.Add(1)

		return "", "", false, lookupErr
	}
	require.NoError(
		t,
		behavior.cfg.Store.UpsertRecord(
			t.Context(), RegistryRecord{
				TargetOutpoint: target,
				ActorID:        actorIDForTarget(target),
				Trigger:        TriggerManual,
				ExitPolicyKind: vhtlcClaimTestKind,
				ExitPolicyRef:  vhtlcClaimTestRef,
				Phase:          PhaseCompleted,
			},
		),
	)

	resp, err := behavior.handleEnsure(
		t.Context(), &EnsureUnrollRequest{
			Outpoint:       target,
			Trigger:        TriggerManual,
			ExitPolicyKind: vhtlcClaimTestKind,
			ExitPolicyRef:  vhtlcClaimTestRef,
		},
	).Unpack()
	require.NoError(t, err)

	ensureResp, ok := resp.(*EnsureUnrollResp)
	require.True(t, ok)
	require.False(t, ensureResp.Created)
	require.EqualValues(t, 0, lookups.Load())
}

// TestRegistryInheritsArmedRecoveryPolicy verifies a policy-less admission --
// the shape the manual Unroll RPC, the fraud watcher, and critical expiry all
// submit -- binds the target to the armed recovery obligation's exit policy
// instead of the standard VTXO timeout policy.
func TestRegistryInheritsArmedRecoveryPolicy(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	target := proof.TargetOutpoint()

	var lookups atomic.Int32
	var lookedUp wire.OutPoint
	registry, store, _ := newRegistryHarnessWithPolicy(
		t, proof, desc, func(_ context.Context, got wire.OutPoint) (
			ExitPolicyKind, string, bool, error) {

			lookups.Add(1)
			lookedUp = got

			return vhtlcClaimTestKind, vhtlcClaimTestRef, true, nil
		},
	)

	resp, err := registry.Ref().Ask(t.Context(), &EnsureUnrollRequest{
		Outpoint: target,
		Trigger:  TriggerFraudSpend,
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	ensureResp, ok := resp.(*EnsureUnrollResp)
	require.True(t, ok)
	require.True(t, ensureResp.Created)
	require.EqualValues(t, 1, lookups.Load())
	require.Equal(t, target, lookedUp)

	// The durable record carries the armed identity, not the empty
	// standard one, so a restart re-admits under the same policy.
	record, err := store.GetRecord(t.Context(), target)
	require.NoError(t, err)
	require.NotNil(t, record)
	require.Equal(t, vhtlcClaimTestKind, record.ExitPolicyKind)
	require.Equal(t, vhtlcClaimTestRef, record.ExitPolicyRef)

	// A repeat policy-less admission stays idempotent under the inherited
	// identity rather than tripping the mismatch check.
	resp, err = registry.Ref().Ask(t.Context(), &EnsureUnrollRequest{
		Outpoint: target,
		Trigger:  TriggerFraudSpend,
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	ensureResp, ok = resp.(*EnsureUnrollResp)
	require.True(t, ok)
	require.False(t, ensureResp.Created)
}

// TestRegistryRejectsPolicyConflictWithArmedRecovery verifies an admission
// that names a different policy than the armed obligation for the same target
// is rejected before any record is written, instead of admitting an exit that
// the obligation does not describe.
func TestRegistryRejectsPolicyConflictWithArmedRecovery(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	target := proof.TargetOutpoint()

	registry, store, _ := newRegistryHarnessWithPolicy(
		t, proof, desc, func(_ context.Context, _ wire.OutPoint) (
			ExitPolicyKind, string, bool, error) {

			return vhtlcClaimTestKind, vhtlcClaimTestRef, true, nil
		},
	)

	_, err := registry.Ref().Ask(t.Context(), &EnsureUnrollRequest{
		Outpoint:       target,
		Trigger:        TriggerManual,
		ExitPolicyKind: vhtlcClaimTestKind,
		ExitPolicyRef:  "stale-recovery",
	}).Await(t.Context()).Unpack()
	require.ErrorIs(t, err, ErrExitPolicyMismatch)

	record, err := store.GetRecord(t.Context(), target)
	require.NoError(t, err)
	require.Nil(t, record)
}

// TestRegistryFailsClosedOnArmedPolicyLookupError verifies an unreadable
// recovery obligation aborts admission rather than silently falling back to
// the standard policy.
func TestRegistryFailsClosedOnArmedPolicyLookupError(t *testing.T) {
	target := wire.OutPoint{Index: 0}
	lookupErr := errors.New("recovery store unavailable")

	behavior := newPolicyTestBehavior(t)
	behavior.cfg.ArmedExitPolicy = func(context.Context, wire.OutPoint) (
		ExitPolicyKind, string, bool, error) {

		return "", "", false, lookupErr
	}

	_, err := behavior.handleEnsure(
		t.Context(), &EnsureUnrollRequest{
			Outpoint: target,
			Trigger:  TriggerManual,
		},
	).Unpack()
	require.ErrorIs(t, err, lookupErr)
}

// newPolicyTestBehavior builds a registry behavior with no active child for
// direct handleEnsure tests.
func newPolicyTestBehavior(t *testing.T) *registryBehavior {
	t.Helper()

	behavior := &registryBehavior{
		cfg: RegistryConfig{
			Store: newMemRegistryStore(),
		},
		log:        btclog.Disabled,
		selfRef:    noopRegistryTellRef{},
		active:     make(map[wire.OutPoint]*VTXOUnrollActor),
		pending:    make(map[wire.OutPoint]RegistryRecord),
		persisting: make(map[wire.OutPoint]RegistryRecord),
	}
	t.Cleanup(func() {
		for _, child := range behavior.active {
			child.Stop()
		}
	})

	return behavior
}

// newRegistryHarnessWithPolicy builds the standard registry harness with an
// armed-policy lookup plus a resolver for the non-standard test kind.
func newRegistryHarnessWithPolicy(t *testing.T, proof *recovery.Proof,
	desc *vtxo.Descriptor, lookup ArmedExitPolicyLookup) (
	*UnrollRegistryActor, *memRegistryStore, *memCheckpointStore) {

	t.Helper()

	store := newMemRegistryStore()
	checkpoints := newMemCheckpointStore()
	policy := &vhtlcClaimTestPolicy{
		StandardVTXOExitSpendPolicy: NewStandardVTXOExitSpendPolicy(
			desc,
		),
	}

	cfg := RegistryConfig{
		Store:         store,
		DeliveryStore: checkpoints,
		ProofAssembler: &mockProofAssembler{
			proof: proof,
		},
		VTXOStore: &mockVTXOStore{
			desc: desc,
		},
		TxConfirmRef: &fakeTxConfirmRef{},
		ChainSource: &fakeRegistryChainSourceRef{
			height: 200,
		},
		Wallet: &fakeSweepWallet{},
		ExitSpendPolicyResolver: &fixedExitSpendPolicyResolver{
			policy: policy,
		},
		ArmedExitPolicy: lookup,
		Log:             fn.Some(btclog.Disabled),
	}

	registry := newRegistryHarnessWithSpawn(t, cfg)
	t.Cleanup(registry.Stop)

	return registry, store, checkpoints
}

// TestRegistryReadmitsRecoverableFailureUnderNewPolicy verifies the
// RecoverableFailure exemption still allows a fresh admission under a new
// exit policy. Such a record is terminal but left no on-chain footprint and
// the VTXO was rolled back to live, so its old binding is dead and must not
// block the target from exiting under a different policy.
func TestRegistryReadmitsRecoverableFailureUnderNewPolicy(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	target := proof.TargetOutpoint()

	registry, store, _ := newRegistryHarnessWithPolicy(t, proof, desc, nil)

	// The previous attempt failed cleanly under the standard timeout
	// policy: terminal, recoverable, and with no sweep txid.
	require.NoError(
		t,
		store.UpsertRecord(
			t.Context(), RegistryRecord{
				TargetOutpoint: target,
				ActorID:        "vtxo-unroll-previous",
				Trigger:        TriggerManual,
				Phase:          PhaseFailed,
				FailReason: "sweep broadcast rejected; no " +
					"footprint",
				RecoverableFailure: true,
			},
		),
	)

	resp, err := registry.Ref().Ask(t.Context(), &EnsureUnrollRequest{
		Outpoint:       target,
		Trigger:        TriggerManual,
		ExitPolicyKind: vhtlcClaimTestKind,
		ExitPolicyRef:  vhtlcClaimTestRef,
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	ensureResp, ok := resp.(*EnsureUnrollResp)
	require.True(t, ok)
	require.True(t, ensureResp.Created)

	// The dead attempt was replaced by a record bound to the new policy.
	record, err := store.GetRecord(t.Context(), target)
	require.NoError(t, err)
	require.NotNil(t, record)
	require.Equal(t, vhtlcClaimTestKind, record.ExitPolicyKind)
	require.Equal(t, vhtlcClaimTestRef, record.ExitPolicyRef)
	require.False(t, record.RecoverableFailure)
}

// TestRegistryInheritedPolicyReachesStartRequest verifies the identity an
// admission resolves -- here, the armed recovery obligation's -- is what the
// spawned child is actually started with, not just what the registry record
// records. A child started under the standard policy while the record claims
// the recovery policy would exit through a leaf the output does not have.
func TestRegistryInheritedPolicyReachesStartRequest(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	target := proof.TargetOutpoint()

	registry, _, _ := newRegistryHarnessWithPolicy(
		t, proof, desc, func(_ context.Context, _ wire.OutPoint) (
			ExitPolicyKind, string, bool, error) {

			return vhtlcClaimTestKind, vhtlcClaimTestRef, true, nil
		},
	)

	// Capture the message the registry starts the child with, and echo its
	// identity back from GetStateRequest the way the real child derives it
	// from its own start request.
	var (
		mu       sync.Mutex
		startReq *StartUnrollRequest
	)
	starts := make(chan *StartUnrollRequest, 1)
	registry.behavior.spawnFunc = func(_ context.Context,
		target wire.OutPoint) (*VTXOUnrollActor, error) {

		behavior := actor.NewFunctionBehavior(
			func(_ context.Context, msg Msg) fn.Result[Resp] {
				switch m := msg.(type) {
				case *StartUnrollRequest:
					mu.Lock()
					startReq = m
					mu.Unlock()

					starts <- m

					return fn.Ok[Resp](&AckResp{})

				case *GetStateRequest:
					mu.Lock()
					defer mu.Unlock()

					state := &GetStateResp{
						Started: true,
						Trigger: TriggerFraudSpend,
						Phase:   PhasePending,
					}
					if startReq != nil {
						state.ExitPolicyKind =
							startReq.ExitPolicyKind
						state.ExitPolicyRef =
							startReq.ExitPolicyRef
					}

					return fn.Ok[Resp](state)

				default:
					return fn.Err[Resp](
						fmt.Errorf("unexpected msg %T",
							msg),
					)
				}
			},
		)

		//nolint:contextcheck // test child owns its own lifecycle
		return newTestUnrollChild(t, target, behavior), nil
	}

	_, err := registry.Ref().Ask(t.Context(), &EnsureUnrollRequest{
		Outpoint: target,
		Trigger:  TriggerFraudSpend,
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	select {
	case start := <-starts:
		require.Equal(t, vhtlcClaimTestKind, start.ExitPolicyKind)
		require.Equal(t, vhtlcClaimTestRef, start.ExitPolicyRef)

	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for StartUnrollRequest")
	}
}

// TestRegistryDeadBindingCannotSkipArmedLookup verifies a fresh admission
// cannot use a recoverable failure's obsolete identity to bypass an armed
// obligation that now owns the target.
func TestRegistryDeadBindingCannotSkipArmedLookup(t *testing.T) {
	for _, test := range []struct {
		name    string
		lookup  ArmedExitPolicyLookup
		wantErr error
	}{
		{
			name: "conflicting recovery",
			lookup: func(context.Context, wire.OutPoint) (
				ExitPolicyKind, string, bool, error) {

				return vhtlcClaimTestKind, "recovery-new",
					true, nil
			},
			wantErr: ErrExitPolicyMismatch,
		},
		{
			name: "unreadable recovery",
			lookup: func(context.Context, wire.OutPoint) (
				ExitPolicyKind, string, bool, error) {

				return "", "", false, context.DeadlineExceeded
			},
			wantErr: context.DeadlineExceeded,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			proof := buildLinearProof(t)
			desc := testDescriptor(
				t, proof.TargetOutpoint(), proof.CSVDelay(),
			)
			target := proof.TargetOutpoint()
			registry, store, _ := newRegistryHarnessWithPolicy(
				t, proof, desc, test.lookup,
			)
			existing := RegistryRecord{
				TargetOutpoint:     target,
				ActorID:            "old-attempt",
				ExitPolicyKind:     vhtlcClaimTestKind,
				ExitPolicyRef:      vhtlcClaimTestRef,
				Phase:              PhaseFailed,
				RecoverableFailure: true,
			}
			require.NoError(
				t,
				store.UpsertRecord(
					t.Context(), existing,
				),
			)

			_, err := registry.Ref().Ask(
				t.Context(), &EnsureUnrollRequest{
					Outpoint:       target,
					Trigger:        TriggerManual,
					ExitPolicyKind: vhtlcClaimTestKind,
					ExitPolicyRef:  vhtlcClaimTestRef,
				},
			).Await(t.Context()).Unpack()
			require.ErrorIs(t, err, test.wantErr)

			record, err := store.GetRecord(t.Context(), target)
			require.NoError(t, err)
			require.Equal(t, existing, *record)
		})
	}
}
