package coordinator

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/unroll"
	"github.com/lightninglabs/darepo-client/vhtlcrecovery"
	"github.com/stretchr/testify/require"
)

// TestServiceEscalatePersistsBeforeUnroll verifies the crash-safe handoff:
// escalation writes the durable recovery state before unroll admission is
// requested with the recovery id as the policy reference.
func TestServiceEscalatePersistsBeforeUnroll(t *testing.T) {
	t.Parallel()

	job := testRecoveryJob("recovery-escalate", vhtlcrecovery.StateArmed)
	store := newFakeStore(job)
	registry := &fakeUnrollRegistry{
		status: &unroll.GetStatusResp{
			Found: true,
			Phase: unroll.PhaseMaterializing,
			ExitPolicyKind: unroll.ExitPolicyKind(
				job.ExitPolicyKind,
			),
			ExitPolicyRef: job.ID,
		},
	}
	service := newTestService(t, store, registry)

	status, err := service.EscalateRecovery(
		t.Context(), job.ID, "cooperative path unsafe", nil,
	)
	require.NoError(t, err)
	require.Equal(t, vhtlcrecovery.StateUnrollStarted, status.Job.State)
	require.Len(t, registry.ensureRequests, 1)

	req := registry.ensureRequests[0]
	require.Equal(t, job.VTXOOutpoint, req.Outpoint)
	require.Equal(
		t, unroll.ExitPolicyKind(job.ExitPolicyKind),
		req.ExitPolicyKind,
	)
	require.Equal(t, job.ID, req.ExitPolicyRef)
	require.Equal(t, []string{"escalate"}, store.events)
}

// TestServiceEscalateFailsClosedOnPolicyMismatch verifies an existing unroll
// job for the same target cannot silently run a different exit policy.
func TestServiceEscalateFailsClosedOnPolicyMismatch(t *testing.T) {
	t.Parallel()

	job := testRecoveryJob("recovery-mismatch", vhtlcrecovery.StateArmed)
	store := newFakeStore(job)
	registry := &fakeUnrollRegistry{
		status: &unroll.GetStatusResp{
			Found:          true,
			Phase:          unroll.PhaseMaterializing,
			ExitPolicyKind: "standard_vtxo_timeout",
		},
	}
	service := newTestService(t, store, registry)

	_, err := service.EscalateRecovery(
		t.Context(), job.ID, "cooperative path unsafe", nil,
	)
	require.ErrorContains(t, err, "unroll policy kind")

	failed, err := store.GetRecovery(t.Context(), job.ID)
	require.NoError(t, err)
	require.Equal(t, vhtlcrecovery.StateFailed, failed.State)
	require.Contains(t, failed.LastError, "unroll policy kind")
}

// TestServiceEscalateKeepsRecoveryActiveAfterStatusProbeError verifies a
// transient status probe failure after successful unroll admission does not
// mark the durable recovery row failed while the unroll job may be running.
func TestServiceEscalateKeepsRecoveryActiveAfterStatusProbeError(t *testing.T) {
	t.Parallel()

	job := testRecoveryJob("recovery-probe", vhtlcrecovery.StateArmed)
	store := newFakeStore(job)
	registry := &fakeUnrollRegistry{
		statusErr: fmt.Errorf("status probe timed out"),
	}
	service := newTestService(t, store, registry)

	_, err := service.EscalateRecovery(
		t.Context(), job.ID, "cooperative path unsafe", nil,
	)
	require.ErrorContains(t, err, "status probe timed out")
	require.Len(t, registry.ensureRequests, 1)

	stored, err := store.GetRecovery(t.Context(), job.ID)
	require.NoError(t, err)
	require.Equal(t, vhtlcrecovery.StateUnrollStarted, stored.State)
	require.Empty(t, stored.LastError)
}

// TestServiceRestoreOnlyReissuesEscalatedJobs verifies restart restore leaves
// armed jobs dormant while reissuing jobs that had already escalated before
// shutdown.
func TestServiceRestoreOnlyReissuesEscalatedJobs(t *testing.T) {
	t.Parallel()

	armed := testRecoveryJob("recovery-armed", vhtlcrecovery.StateArmed)
	active := testRecoveryJob(
		"recovery-active", vhtlcrecovery.StateUnrollStarted,
	)
	store := newFakeStore(armed, active)
	registry := &fakeUnrollRegistry{
		status: &unroll.GetStatusResp{
			Found: true,
			Phase: unroll.PhaseMaterializing,
			ExitPolicyKind: unroll.ExitPolicyKind(
				active.ExitPolicyKind,
			),
			ExitPolicyRef: active.ID,
		},
	}
	service := newTestService(t, store, registry)

	require.NoError(t, service.RestoreNonTerminal(t.Context()))
	require.Len(t, registry.ensureRequests, 1)
	require.Equal(
		t, active.VTXOOutpoint, registry.ensureRequests[0].Outpoint,
	)
	require.Equal(t, active.ID, registry.ensureRequests[0].ExitPolicyRef)
}

// TestServiceRestoreContinuesAfterPolicyMismatch verifies unrecoverable
// unroll ownership conflicts are marked failed without preventing later rows
// from being reissued.
func TestServiceRestoreContinuesAfterPolicyMismatch(t *testing.T) {
	t.Parallel()

	first := testRecoveryJob(
		"recovery-first", vhtlcrecovery.StateUnrollStarted,
	)
	second := testRecoveryJob(
		"recovery-second", vhtlcrecovery.StateUnrollStarted,
	)
	store := newFakeStore(first, second)
	registry := &fakeUnrollRegistry{
		status: &unroll.GetStatusResp{
			Found:          true,
			Phase:          unroll.PhaseMaterializing,
			ExitPolicyKind: "standard_vtxo_timeout",
		},
	}
	service := newTestService(t, store, registry)

	require.NoError(t, service.RestoreNonTerminal(t.Context()))
	require.Len(t, registry.ensureRequests, 2)

	storedFirst, err := store.GetRecovery(t.Context(), first.ID)
	require.NoError(t, err)
	require.Equal(t, vhtlcrecovery.StateFailed, storedFirst.State)

	storedSecond, err := store.GetRecovery(t.Context(), second.ID)
	require.NoError(t, err)
	require.Equal(t, vhtlcrecovery.StateFailed, storedSecond.State)
}

// TestServiceRestoreKeepsRecoveryActiveAfterTransientError verifies transient
// restore/admission errors leave the durable recovery row non-terminal so the
// next restore pass can retry.
func TestServiceRestoreKeepsRecoveryActiveAfterTransientError(t *testing.T) {
	t.Parallel()

	job := testRecoveryJob(
		"recovery-transient", vhtlcrecovery.StateUnrollStarted,
	)
	store := newFakeStore(job)
	registry := &fakeUnrollRegistry{
		ensureErr: fmt.Errorf("actor transport unavailable"),
	}
	service := newTestService(t, store, registry)

	require.NoError(t, service.RestoreNonTerminal(t.Context()))
	require.Len(t, registry.ensureRequests, 1)

	stored, err := store.GetRecovery(t.Context(), job.ID)
	require.NoError(t, err)
	require.Equal(t, vhtlcrecovery.StateUnrollStarted, stored.State)
	require.Empty(t, stored.LastError)
}

// TestServiceStatusReconcilesTerminalUnroll verifies status polling folds a
// terminal unroll result back into the durable recovery row.
func TestServiceStatusReconcilesTerminalUnroll(t *testing.T) {
	t.Parallel()

	job := testRecoveryJob(
		"recovery-complete", vhtlcrecovery.StateUnrollStarted,
	)
	store := newFakeStore(job)
	sweepTxid := chainhash.Hash{1, 2, 3}
	registry := &fakeUnrollRegistry{
		status: &unroll.GetStatusResp{
			Found:     true,
			Phase:     unroll.PhaseCompleted,
			SweepTxid: &sweepTxid,
			ExitPolicyKind: unroll.ExitPolicyKind(
				job.ExitPolicyKind,
			),
			ExitPolicyRef: job.ID,
		},
	}
	service := newTestService(t, store, registry)

	status, err := service.GetRecoveryStatus(t.Context(), job.ID)
	require.NoError(t, err)
	require.Equal(t, vhtlcrecovery.StateCompleted, status.Job.State)
	require.True(t, status.UnrollFound)
	require.Equal(t, unroll.PhaseCompleted, status.UnrollPhase)
	require.Equal(t, &sweepTxid, status.UnrollSweep)
}

// TestServiceCompletedStatusKeepsUnrollSweep verifies a recovery row that was
// already marked completed by an earlier status poll still joins the terminal
// unroll snapshot so RPC callers can observe the sweep txid deterministically.
func TestServiceCompletedStatusKeepsUnrollSweep(t *testing.T) {
	t.Parallel()

	job := testRecoveryJob(
		"recovery-already-complete", vhtlcrecovery.StateCompleted,
	)
	store := newFakeStore(job)
	sweepTxid := chainhash.Hash{4, 5, 6}
	registry := &fakeUnrollRegistry{
		status: &unroll.GetStatusResp{
			Found:     true,
			Phase:     unroll.PhaseCompleted,
			SweepTxid: &sweepTxid,
			ExitPolicyKind: unroll.ExitPolicyKind(
				job.ExitPolicyKind,
			),
			ExitPolicyRef: job.ID,
		},
	}
	service := newTestService(t, store, registry)

	status, err := service.GetRecoveryStatus(t.Context(), job.ID)
	require.NoError(t, err)
	require.Equal(t, vhtlcrecovery.StateCompleted, status.Job.State)
	require.True(t, status.UnrollFound)
	require.Equal(t, unroll.PhaseCompleted, status.UnrollPhase)
	require.Equal(t, &sweepTxid, status.UnrollSweep)
}

// newTestService builds a service from fake dependencies and fails the test if
// the constructor rejects the dependency set.
func newTestService(t *testing.T, store Store,
	registry UnrollRegistry) *Service {

	t.Helper()

	service, err := NewService(ServiceConfig{
		Store:  store,
		Unroll: registry,
	})
	require.NoError(t, err)

	return service
}

// testRecoveryJob returns a minimally complete recovery row for coordinator
// tests. Script policy validation is covered in the unrollpolicy package, so
// this helper focuses on the durable coordinator fields.
func testRecoveryJob(id, state string) vhtlcrecovery.RecoveryJob {
	txid := chainhash.Hash{byte(len(id)), 1, 2, 3}
	now := time.Unix(100, 0).UTC()

	return vhtlcrecovery.RecoveryJob{
		ID:        id,
		RequestID: id + "-request",
		SwapID:    []byte(id + "-swap"),
		Direction: vhtlcrecovery.DirectionReceive,
		Action:    vhtlcrecovery.ActionClaim,
		State:     state,
		VTXOOutpoint: wire.OutPoint{
			Hash:  txid,
			Index: 7,
		},
		VTXOAmountSat:  50_000,
		ExitPolicyKind: vhtlcrecovery.ExitPolicyKindClaim,
		CreatedAt:      now,
		UpdatedAt:      now,
		ArmedAt:        &now,
		PreimageHash:   make([]byte, chainhash.HashSize),
		DestinationScript: []byte{
			0x51,
		},
		SenderPubkey: []byte{
			0x02,
			0x01,
		},
		ReceiverPubkey: []byte{
			0x02,
			0x02,
		},
		ServerPubkey: []byte{
			0x02,
			0x03,
		},
		SignerKeyFamily:         1,
		SignerKeyIndex:          2,
		MaxFeeRateSatPerKWeight: 2_500,
	}
}

// fakeStore is an in-memory Store implementation for service tests.
type fakeStore struct {
	jobs   map[string]vhtlcrecovery.RecoveryJob
	events []string
}

// newFakeStore indexes the supplied jobs by recovery id.
func newFakeStore(jobs ...vhtlcrecovery.RecoveryJob) *fakeStore {
	store := &fakeStore{
		jobs: make(map[string]vhtlcrecovery.RecoveryJob),
	}
	for i := range jobs {
		store.jobs[jobs[i].ID] = jobs[i]
	}

	return store
}

// ArmRecovery implements Store by inserting or returning the in-memory row.
func (s *fakeStore) ArmRecovery(_ context.Context,
	job vhtlcrecovery.RecoveryJob) (*vhtlcrecovery.RecoveryJob, bool,
	error) {

	if existing, ok := s.jobs[job.ID]; ok {
		return cloneRecoveryJob(existing), false, nil
	}

	s.jobs[job.ID] = job

	return cloneRecoveryJob(job), true, nil
}

// GetRecovery implements Store by loading one recovery row.
func (s *fakeStore) GetRecovery(_ context.Context, id string) (
	*vhtlcrecovery.RecoveryJob, error) {

	job, ok := s.jobs[id]
	if !ok {
		return nil, fmt.Errorf("missing recovery %s", id)
	}

	return cloneRecoveryJob(job), nil
}

// ListNonTerminalRecoveries implements Store by returning every non-terminal
// in-memory row.
func (s *fakeStore) ListNonTerminalRecoveries(context.Context) (
	[]vhtlcrecovery.RecoveryJob, error) {

	var jobs []vhtlcrecovery.RecoveryJob
	for _, job := range s.jobs {
		if !job.IsTerminal() {
			jobs = append(jobs, *cloneRecoveryJob(job))
		}
	}

	return jobs, nil
}

// ListRecoveries implements Store by returning every in-memory row.
func (s *fakeStore) ListRecoveries(context.Context) (
	[]vhtlcrecovery.RecoveryJob, error) {

	jobs := make([]vhtlcrecovery.RecoveryJob, 0, len(s.jobs))
	for _, job := range s.jobs {
		jobs = append(jobs, *cloneRecoveryJob(job))
	}

	return jobs, nil
}

// EscalateRecovery implements Store by moving an armed job to unroll_started.
func (s *fakeStore) EscalateRecovery(_ context.Context, id string,
	claimPreimage []byte) error {

	job, ok := s.jobs[id]
	if !ok {
		return fmt.Errorf("missing recovery %s", id)
	}
	job.State = vhtlcrecovery.StateUnrollStarted
	job.ClaimPreimage = append([]byte(nil), claimPreimage...)
	s.jobs[id] = job
	s.events = append(s.events, "escalate")

	return nil
}

// CancelRecovery implements Store by moving a job to cancelled.
func (s *fakeStore) CancelRecovery(_ context.Context, id, reason string,
	cooperativeTxid []byte) error {

	job, ok := s.jobs[id]
	if !ok {
		return fmt.Errorf("missing recovery %s", id)
	}
	job.State = vhtlcrecovery.StateCancelled
	job.CancelReason = reason
	job.CooperativeTxid = append([]byte(nil), cooperativeTxid...)
	s.jobs[id] = job

	return nil
}

// CompleteRecovery implements Store by moving a job to completed.
func (s *fakeStore) CompleteRecovery(_ context.Context, id string) error {
	job, ok := s.jobs[id]
	if !ok {
		return fmt.Errorf("missing recovery %s", id)
	}
	job.State = vhtlcrecovery.StateCompleted
	s.jobs[id] = job

	return nil
}

// FailRecovery implements Store by moving a job to failed.
func (s *fakeStore) FailRecovery(_ context.Context, id string,
	failure error) error {

	job, ok := s.jobs[id]
	if !ok {
		return fmt.Errorf("missing recovery %s", id)
	}
	job.State = vhtlcrecovery.StateFailed
	if failure != nil {
		job.LastError = failure.Error()
	}
	s.jobs[id] = job

	return nil
}

// fakeUnrollRegistry records ensure requests and returns one configured status.
type fakeUnrollRegistry struct {
	ensureRequests []unroll.EnsureUnrollRequest
	ensureErr      error
	status         *unroll.GetStatusResp
	statusErr      error
}

// EnsureUnroll implements UnrollRegistry by recording the request.
func (r *fakeUnrollRegistry) EnsureUnroll(_ context.Context,
	req unroll.EnsureUnrollRequest) (*unroll.EnsureUnrollResp, error) {

	r.ensureRequests = append(r.ensureRequests, req)
	if r.ensureErr != nil {
		return nil, r.ensureErr
	}

	return &unroll.EnsureUnrollResp{
		ActorID: "actor-" + req.Outpoint.String(),
		Created: true,
	}, nil
}

// GetStatus implements UnrollRegistry by returning the configured status.
func (r *fakeUnrollRegistry) GetStatus(_ context.Context, _ wire.OutPoint) (
	*unroll.GetStatusResp, error) {

	if r.statusErr != nil {
		return nil, r.statusErr
	}

	if r.status == nil {
		return &unroll.GetStatusResp{}, nil
	}

	status := *r.status

	return &status, nil
}

// cloneRecoveryJob returns a shallow value copy with owned slices for fields
// mutated by tests.
func cloneRecoveryJob(
	job vhtlcrecovery.RecoveryJob) *vhtlcrecovery.RecoveryJob {

	clone := job
	clone.SwapID = append([]byte(nil), job.SwapID...)
	clone.PreimageHash = append([]byte(nil), job.PreimageHash...)
	clone.ClaimPreimage = append([]byte(nil), job.ClaimPreimage...)
	clone.CooperativeTxid = append([]byte(nil), job.CooperativeTxid...)

	return &clone
}
