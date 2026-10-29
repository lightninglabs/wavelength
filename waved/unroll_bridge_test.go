package waved

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/unroll"
	"github.com/lightninglabs/wavelength/vhtlcrecovery"
	"github.com/lightninglabs/wavelength/vhtlcrecovery/coordinator"
	"github.com/lightninglabs/wavelength/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestExitPolicyKindMirrorsRecoveryConstants pins the string-typed actormsg
// exit-policy enum to the canonical vhtlcrecovery constants. The two are kept
// in sync by hand (actormsg cannot import vhtlcrecovery without a cycle), so a
// drift would silently break the round-trip through the ForceUnroll path.
func TestExitPolicyKindMirrorsRecoveryConstants(t *testing.T) {
	t.Parallel()

	require.Equal(
		t, vhtlcrecovery.ExitPolicyKindClaim,
		string(actormsg.ExitPolicyVHTLCClaim),
	)
	require.Equal(
		t, vhtlcrecovery.ExitPolicyKindRefundWithoutReceiver,
		string(actormsg.ExitPolicyVHTLCRefundWithoutReceiver),
	)
}

// TestUnrollStartTrigger verifies the string-typed UnrollTrigger that rides the
// ForceUnroll path maps back onto the right unroll.StartTrigger, and that an
// empty or unknown trigger falls back to critical expiry (preserving the
// auto-expiry default and the historical manual-exit behavior).
func TestUnrollStartTrigger(t *testing.T) {
	t.Parallel()

	require.Equal(
		t, unroll.TriggerCriticalExpiry,
		unrollStartTrigger(actormsg.UnrollTriggerCriticalExpiry),
	)
	require.Equal(
		t, unroll.TriggerManual,
		unrollStartTrigger(actormsg.UnrollTriggerManual),
	)
	require.Equal(
		t, unroll.TriggerFraudSpend,
		unrollStartTrigger(actormsg.UnrollTriggerFraudSpend),
	)
	require.Equal(
		t, unroll.TriggerCriticalExpiry,
		unrollStartTrigger(
			actormsg.UnrollTrigger("not-a-real-trigger"),
		),
	)
}

// TestEnsureUnrollFromExpiring verifies the chain-resolver bridge threads the
// trigger and optional exit policy from a VTXO ExpiringNotification into the
// registry EnsureUnrollRequest, and that a None policy keeps the registry on
// its standard timeout policy.
func TestEnsureUnrollFromExpiring(t *testing.T) {
	t.Parallel()

	outpoint := wire.OutPoint{Hash: chainhash.Hash{0xaa}, Index: 3}

	// A vHTLC refund carries an explicit trigger and exit policy: both must
	// survive into the registry request.
	withPolicy := ensureUnrollFromExpiring(vtxo.ExpiringNotification{
		VTXO:    &vtxo.Descriptor{Outpoint: outpoint},
		Trigger: actormsg.UnrollTriggerManual,
		ExitPolicy: fn.Some(actormsg.ExitPolicy{
			Kind: actormsg.ExitPolicyVHTLCRefundWithoutReceiver,
			Ref:  actormsg.ExitPolicyRef("recovery-42"),
		}),
	})
	require.Equal(t, outpoint, withPolicy.Outpoint)
	require.Equal(t, unroll.TriggerManual, withPolicy.Trigger)
	require.Equal(
		t, unroll.ExitPolicyKind(
			actormsg.ExitPolicyVHTLCRefundWithoutReceiver,
		),
		withPolicy.ExitPolicyKind,
	)
	require.Equal(t, "recovery-42", withPolicy.ExitPolicyRef)

	// A critical-expiry notification carries no policy: the registry
	// request must leave the policy identity empty (standard timeout).
	autoExpiry := ensureUnrollFromExpiring(vtxo.ExpiringNotification{
		VTXO: &vtxo.Descriptor{Outpoint: outpoint},
	})
	require.Equal(t, unroll.TriggerCriticalExpiry, autoExpiry.Trigger)
	require.Empty(t, autoExpiry.ExitPolicyKind)
	require.Empty(t, autoExpiry.ExitPolicyRef)
}

// armedRecoveryStore stubs the coordinator store surface down to the one query
// the unroll admission lookup uses.
type armedRecoveryStore struct {
	coordinator.Store

	jobs []vhtlcrecovery.RecoveryJob
	err  error
}

// ListNonTerminalRecoveries returns the configured recovery rows.
func (s armedRecoveryStore) ListNonTerminalRecoveries(context.Context) (
	[]vhtlcrecovery.RecoveryJob, error) {

	return s.jobs, s.err
}

// TestArmedRecoveryExitPolicy verifies the unroll admission lookup resolves the
// exit policy identity of the non-terminal recovery obligation that owns a
// target outpoint, and reports no obligation for any other target. This is the
// seam that keeps a policy-less unroll writer from binding a recovery target to
// the standard VTXO timeout policy.
func TestArmedRecoveryExitPolicy(t *testing.T) {
	t.Parallel()

	target := wire.OutPoint{Index: 1}
	store := armedRecoveryStore{jobs: []vhtlcrecovery.RecoveryJob{
		{
			ID: "recovery-other",
			VTXOOutpoint: wire.OutPoint{
				Index: 2,
			},
			ExitPolicyKind: vhtlcrecovery.
				ExitPolicyKindRefundWithoutReceiver,
		},
		{
			ID:             "recovery-1",
			VTXOOutpoint:   target,
			ExitPolicyKind: vhtlcrecovery.ExitPolicyKindClaim,
		},
	}}

	kind, ref, ok, err := armedRecoveryExitPolicy(
		t.Context(), store, target,
	)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(
		t, unroll.ExitPolicyKind(vhtlcrecovery.ExitPolicyKindClaim),
		kind,
	)
	require.Equal(t, "recovery-1", ref)

	_, _, ok, err = armedRecoveryExitPolicy(
		t.Context(), store, wire.OutPoint{
			Index: 3,
		},
	)
	require.NoError(t, err)
	require.False(t, ok)

	_, _, _, err = armedRecoveryExitPolicy(
		t.Context(), armedRecoveryStore{
			err: errors.New("store unavailable"),
		}, target,
	)
	require.Error(t, err)
}

// scanRecoveryStore stubs the coordinator store surface the orphan scan uses:
// the armed-policy lookup and the terminal failure of a conflicting row.
type scanRecoveryStore struct {
	coordinator.Store

	jobs   []vhtlcrecovery.RecoveryJob
	failed map[string]error
}

// ListNonTerminalRecoveries returns the configured recovery rows.
func (s *scanRecoveryStore) ListNonTerminalRecoveries(context.Context) (
	[]vhtlcrecovery.RecoveryJob, error) {

	return s.jobs, nil
}

// FailRecovery records the terminal failure instead of persisting it.
func (s *scanRecoveryStore) FailRecovery(_ context.Context, id string,
	failure error) error {

	if s.failed == nil {
		s.failed = make(map[string]error)
	}
	s.failed[id] = failure

	return nil
}

// scanRegistryStore is the minimal unroll control-plane store for the orphan
// scan test. It counts upserts so a skipped target can be told apart from one
// that was admitted afresh.
type scanRegistryStore struct {
	records map[wire.OutPoint]unroll.RegistryRecord
	upserts int
}

// UpsertRecord stores one record.
func (s *scanRegistryStore) UpsertRecord(_ context.Context,
	record unroll.RegistryRecord) error {

	s.upserts++
	s.records[record.TargetOutpoint] = record

	return nil
}

// GetRecord returns the pre-bound record for a target.
func (s *scanRegistryStore) GetRecord(_ context.Context, target wire.OutPoint) (
	*unroll.RegistryRecord, error) {

	record, ok := s.records[target]
	if !ok {
		return nil, nil
	}

	return &record, nil
}

// ListNonTerminalRecords returns nothing: the scan under test never restores.
func (s *scanRegistryStore) ListNonTerminalRecords(context.Context) (
	[]unroll.RegistryRecord, error) {

	return nil, nil
}

// MarkTerminal stores the terminal record.
func (s *scanRegistryStore) MarkTerminal(_ context.Context,
	record unroll.RegistryRecord) error {

	s.records[record.TargetOutpoint] = record

	return nil
}

// scanVTXOStore stubs the VTXO store surface read by the orphan scan.
type scanVTXOStore struct {
	vtxo.VTXOStore

	descs []*vtxo.Descriptor
}

// ListVTXOsByStatus returns the configured descriptors.
func (s *scanVTXOStore) ListVTXOsByStatus(context.Context, vtxo.VTXOStatus) (
	[]*vtxo.Descriptor, error) {

	return s.descs, nil
}

// TestRecoverOrphanedUnrollJobsRejectsDormantPolicyMismatch verifies a
// conflicting durable record without a live child cannot pass the boot gate
// or retire the recovery obligation that must retry on the next restart.
func TestRecoverOrphanedUnrollJobsRejectsDormantPolicyMismatch(t *testing.T) {
	t.Parallel()

	target := wire.OutPoint{Hash: chainhash.Hash{0xbb}, Index: 7}

	// The durable record binds the target to the standard timeout policy.
	registryStore := &scanRegistryStore{
		records: map[wire.OutPoint]unroll.RegistryRecord{
			target: {
				TargetOutpoint: target,
				ActorID:        "vtxo-unroll-standard",
				Trigger:        unroll.TriggerManual,
				ExitPolicyKind: unroll.
					StandardVTXOTimeoutExitPolicyKind,
				Phase: unroll.PhasePending,
			},
		},
	}

	// A recovery obligation for the same outpoint was armed later, so the
	// scan's policy-less admission inherits its identity and conflicts
	// with the bound record.
	recoveryStore := &scanRecoveryStore{
		jobs: []vhtlcrecovery.RecoveryJob{{
			ID:             "recovery-1",
			VTXOOutpoint:   target,
			ExitPolicyKind: vhtlcrecovery.ExitPolicyKindClaim,
		}},
	}

	registry := unroll.NewUnrollRegistryActor(unroll.RegistryConfig{
		Store: registryStore,
		ArmedExitPolicy: func(ctx context.Context, op wire.OutPoint) (
			unroll.ExitPolicyKind, string, bool, error) {

			return armedRecoveryExitPolicy(ctx, recoveryStore, op)
		},
		Log: fn.Some(btclog.Disabled),
	})
	t.Cleanup(registry.Stop)

	server := &Server{log: btclog.Disabled}
	err := server.recoverOrphanedUnrollJobs(
		t.Context(), &scanVTXOStore{
			descs: []*vtxo.Descriptor{{Outpoint: target}},
		}, registry, recoveryStore,
	)

	// The dormant record is still stranded, so the boot gate must fail.
	require.ErrorIs(t, err, unroll.ErrExitPolicyMismatch)

	// The target was skipped: the bound record is untouched and no second
	// admission was written for the outpoint.
	record := registryStore.records[target]
	require.Equal(
		t, unroll.StandardVTXOTimeoutExitPolicyKind,
		record.ExitPolicyKind,
	)
	require.Empty(t, record.ExitPolicyRef)
	require.Equal(t, "vtxo-unroll-standard", record.ActorID)
	require.Zero(t, registryStore.upserts)

	// Preserve the obligation so a failed restore does not erase its retry.
	require.Empty(t, recoveryStore.failed)
}

// conflictStatusRegistry controls the worker evidence used by conflict cleanup.
type conflictStatusRegistry struct {
	status *unroll.GetStatusResp
	err    error
}

// GetStatus returns the configured worker status or lookup error.
func (r conflictStatusRegistry) GetStatus(context.Context, wire.OutPoint) (
	*unroll.GetStatusResp, error) {

	return r.status, r.err
}

// TestFailConflictingRecoveryRequiresLiveChild verifies cleanup only retires a
// conflicting obligation when a live worker is confirmed to own its target.
func TestFailConflictingRecoveryRequiresLiveChild(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name        string
		registry    conflictStatusRegistry
		wantFailure bool
	}{
		{
			name: "active",
			registry: conflictStatusRegistry{
				status: &unroll.GetStatusResp{
					Found: true, Active: true,
				},
			},
			wantFailure: true,
		},
		{
			name: "dormant",
			registry: conflictStatusRegistry{
				status: &unroll.GetStatusResp{
					Found: true,
				},
			},
		},
		{
			name: "missing",
		},
		{
			name: "lookup failure",
			registry: conflictStatusRegistry{
				err: errors.New("store unavailable"),
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &scanRecoveryStore{}
			mismatch := &unroll.ExitPolicyMismatchError{
				Target: wire.OutPoint{
					Index: 7,
				},
				RequestedRef: "recovery-1",
			}
			err := failConflictingRecovery(
				t.Context(), store, test.registry, mismatch,
			)
			if !test.wantFailure {
				require.Error(t, err)
				require.Empty(t, store.failed)

				return
			}

			require.NoError(t, err)
			require.ErrorIs(t, store.failed["recovery-1"], mismatch)
		})
	}
}
