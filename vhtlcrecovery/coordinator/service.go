package coordinator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/unroll"
	"github.com/lightninglabs/wavelength/vhtlcrecovery"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lntypes"
)

var errUnrollPolicyMismatch = errors.New("unroll policy mismatch")

// Store is the durable persistence surface required by the recovery service.
// Implementations must use SQL transactions for every state mutation because
// the service intentionally keeps no durable in-memory state of its own.
type Store interface {
	// ArmRecovery stores one dormant recovery job or returns an existing
	// idempotent row when the request was already processed.
	ArmRecovery(ctx context.Context, job vhtlcrecovery.RecoveryJob) (
		*vhtlcrecovery.RecoveryJob, bool, error)

	// GetRecovery loads one recovery row by recovery id.
	GetRecovery(ctx context.Context,
		id string) (*vhtlcrecovery.RecoveryJob, error)

	// ListNonTerminalRecoveries returns all recovery rows that may need
	// restore or reconciliation after daemon startup.
	ListNonTerminalRecoveries(ctx context.Context) (
		[]vhtlcrecovery.RecoveryJob, error)

	// ListRecoveries returns every recovery row for operator inspection.
	ListRecoveries(ctx context.Context) ([]vhtlcrecovery.RecoveryJob, error)

	// EscalateRecovery moves an armed recovery row into active unroll.
	EscalateRecovery(ctx context.Context, id string,
		claimPreimage []byte) error

	// CancelRecovery records that cooperative settlement or operator action
	// made an unspent recovery job unnecessary.
	CancelRecovery(ctx context.Context, id, reason string,
		cooperativeTxid []byte) error

	// CompleteRecovery records successful on-chain completion.
	CompleteRecovery(ctx context.Context, id string) error

	// FailRecovery records a terminal recovery failure that needs
	// attention.
	FailRecovery(ctx context.Context, id string, failure error) error
}

// UnrollRegistry is the small unroll status surface used by recovery. It is
// narrower than the actor ref so tests can model status without spinning up
// the full unroll subsystem. Admission no longer lives here: recovery forces
// the exit through the VTXO manager (see ExitAdmitter) so the target is
// visible to the manager and to restart recovery, and only reads status back
// from the registry.
type UnrollRegistry interface {
	// GetStatus returns the current registry view for one target.
	GetStatus(ctx context.Context,
		target wire.OutPoint) (*unroll.GetStatusResp, error)
}

// ExitAdmitter forces a recovery target into unilateral exit through the VTXO
// manager's single admission gate. The manager owns the state transition
// (persisting the target into VTXOStatusUnilateralExit, out of the live set)
// and, via its chain-resolver seam, starts the durable unroll job under the
// request's exit policy. Recovery hands off to the manager rather than
// admitting the registry job directly so a vHTLC exit converges on the same
// path as manual, critical-expiry, and fraud exits: the manager knows the
// coin is exiting, and the #400 restart orphan scan covers it.
type ExitAdmitter interface {
	// ForceExit drives one target into unilateral exit and returns once
	// the manager has accepted (or declined) the transition. The registry
	// job is started asynchronously through the manager's outbox.
	ForceExit(ctx context.Context, req actormsg.ForceUnrollRequest) error
}

// TargetMaterializer ensures the vHTLC target has the local descriptor and
// package bindings that generic unroll needs before the recovery service admits
// the target. Implementations are domain adapters: the coordinator owns the
// recovery state machine, while packages such as waved know how to stitch
// vHTLC recovery rows back to local OOR artifacts and VTXO descriptors.
type TargetMaterializer interface {
	// EnsureRecoveryTarget materializes the recovery target described by
	// job into local unroll-readable state. The method must be idempotent
	// because it runs on every escalation retry and restore.
	EnsureRecoveryTarget(ctx context.Context,
		job vhtlcrecovery.RecoveryJob) error
}

// ActorUnrollRegistry adapts the live unroll registry actor to the
// vHTLC-recovery service interface.
type ActorUnrollRegistry struct {
	ref actor.ActorRef[unroll.RegistryMsg, unroll.RegistryResp]
}

// NewActorUnrollRegistry wraps one live unroll registry actor reference.
func NewActorUnrollRegistry(ref actor.ActorRef[
	unroll.RegistryMsg, unroll.RegistryResp]) ActorUnrollRegistry {

	return ActorUnrollRegistry{ref: ref}
}

// GetStatus asks the live unroll registry for one target's current status.
func (r ActorUnrollRegistry) GetStatus(ctx context.Context,
	target wire.OutPoint) (*unroll.GetStatusResp, error) {

	resp, err := r.ref.Ask(ctx, &unroll.GetStatusRequest{
		Outpoint: target,
	}).Await(ctx).Unpack()
	if err != nil {
		return nil, err
	}

	statusResp, ok := resp.(*unroll.GetStatusResp)
	if !ok {
		return nil, fmt.Errorf("unexpected unroll status response %T",
			resp)
	}

	return statusResp, nil
}

// ServiceConfig configures the vHTLC recovery coordinator.
type ServiceConfig struct {
	// Store persists recovery jobs and terminal reconciliation.
	Store Store

	// Unroll queries the generic unroll subsystem for per-target status.
	Unroll UnrollRegistry

	// Exiter forces a recovery target into unilateral exit through the
	// VTXO manager's admission gate.
	Exiter ExitAdmitter

	// Log is an optional structured subsystem logger.
	Log fn.Option[btclog.Logger]

	// TargetMaterializer prepares local VTXO/package state for non-standard
	// vHTLC targets before generic unroll admission. Nil preserves the
	// historical behavior where the target descriptor must already exist.
	TargetMaterializer TargetMaterializer
}

// RecoveryStatus is the joined recovery/unroll status returned to callers.
// The recovery job is always loaded from durable SQL. Unroll fields are
// best-effort runtime observations and are absent while a job remains armed.
type RecoveryStatus struct {
	Job vhtlcrecovery.RecoveryJob

	UnrollFound   bool
	UnrollActive  bool
	UnrollPhase   unroll.Phase
	UnrollSweep   *chainhash.Hash
	UnrollFailure string
}

// Service coordinates durable vHTLC recovery jobs with the generic unroll
// subsystem. It is intentionally a thin service, not a durable actor: SQL owns
// recovery state, and the unroll registry owns per-target execution workers.
type Service struct {
	store              Store
	unroll             UnrollRegistry
	exiter             ExitAdmitter
	targetMaterializer TargetMaterializer
	log                btclog.Logger
}

// NewService creates a vHTLC recovery service from durable storage, the unroll
// status surface, and the VTXO manager exit-admission seam.
func NewService(cfg ServiceConfig) (*Service, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("vhtlc recovery store is required")
	}
	if cfg.Unroll == nil {
		return nil, fmt.Errorf("unroll registry is required")
	}
	if cfg.Exiter == nil {
		return nil, fmt.Errorf("exit admitter is required")
	}

	return &Service{
		store:              cfg.Store,
		unroll:             cfg.Unroll,
		exiter:             cfg.Exiter,
		targetMaterializer: cfg.TargetMaterializer,
		log:                cfg.Log.UnwrapOr(btclog.Disabled),
	}, nil
}

// ArmRecovery persists a dormant recovery job. Repeated calls with the same
// request id or same swap/action return the existing row only when all durable
// recovery parameters match.
func (s *Service) ArmRecovery(ctx context.Context,
	job vhtlcrecovery.RecoveryJob) (*vhtlcrecovery.RecoveryJob, bool,
	error) {

	stored, created, err := s.store.ArmRecovery(ctx, job)
	if err != nil {
		return nil, false, err
	}

	if created {
		attrs := recoveryLogAttrs(*stored)
		s.log.InfoS(ctx, "vhtlc recovery job created",
			attrs...,
		)
	} else {
		attrs := recoveryLogAttrs(*stored)
		s.log.DebugS(ctx, "vhtlc recovery already armed",
			attrs...,
		)
	}

	return stored, created, nil
}

// EscalateRecovery moves a dormant job into active unroll and ensures the
// generic unroll registry has a child job with this recovery id as its policy
// reference. The SQL transition happens before unroll admission so a crash in
// the handoff is recovered by RestoreNonTerminal.
func (s *Service) EscalateRecovery(ctx context.Context, id, reason string,
	claimPreimage []byte) (*RecoveryStatus, error) {

	job, err := s.store.GetRecovery(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := validateClaimPreimage(*job, claimPreimage); err != nil {
		return nil, err
	}

	if job.IsTerminal() {
		return s.reconcileLoaded(ctx, *job)
	}

	if job.State == vhtlcrecovery.StateArmed {
		s.log.InfoS(ctx, "escalating vhtlc through on-chain recovery",
			append(
				recoveryLogAttrs(*job),
				slog.String("reason", reason),
			)...,
		)

		if err := s.store.EscalateRecovery(
			ctx, id, claimPreimage,
		); err != nil {
			return nil, err
		}

		job, err = s.store.GetRecovery(ctx, id)
		if err != nil {
			return nil, err
		}
	}

	if err := s.ensureUnroll(ctx, *job); err != nil {
		if errors.Is(err, errUnrollPolicyMismatch) {
			failErr := s.store.FailRecovery(ctx, id, err)
			if failErr != nil {
				return nil, fmt.Errorf("ensure unroll: %w; "+
					"mark recovery failed: %v", err,
					failErr)
			}
		}

		return nil, err
	}

	return s.GetRecoveryStatus(ctx, id)
}

// validateClaimPreimage verifies optional cross-process claim preimage material
// before it is written into the recovery row. In-process swap runtimes may pass
// nil and let the registered preimage resolver provide the secret later.
func validateClaimPreimage(job vhtlcrecovery.RecoveryJob,
	claimPreimage []byte) error {

	if len(claimPreimage) == 0 {
		return nil
	}
	if job.Action != vhtlcrecovery.ActionClaim {
		return fmt.Errorf("claim preimage is only valid for claim " +
			"recovery")
	}

	preimage, err := lntypes.MakePreimage(claimPreimage)
	if err != nil {
		return fmt.Errorf("decode claim preimage: %w", err)
	}
	preimageHash, err := lntypes.MakeHash(job.PreimageHash)
	if err != nil {
		return fmt.Errorf("decode preimage hash: %w", err)
	}
	if !preimage.Matches(preimageHash) {
		return fmt.Errorf("claim preimage does not match recovery hash")
	}

	return nil
}

// CancelRecovery marks a non-terminal recovery cancelled. This method is
// idempotent for already-terminal rows: callers receive the current status and
// no state is changed when cooperative completion is reported twice. Cancelling
// a row intentionally does not stop an already-admitted unroll worker; it only
// stops the recovery service from treating later unroll results as
// authoritative for this recovery row.
func (s *Service) CancelRecovery(ctx context.Context, id, reason string,
	cooperativeTxid []byte) (*RecoveryStatus, error) {

	job, err := s.store.GetRecovery(ctx, id)
	if err != nil {
		return nil, err
	}
	if job.IsTerminal() {
		return s.reconcileLoaded(ctx, *job)
	}

	s.log.InfoS(ctx, "vhtlc recovery cancelled",
		append(
			recoveryLogAttrs(*job),
			slog.String("cancel_reason", reason),
		)...,
	)

	if err := s.store.CancelRecovery(
		ctx, id, reason, cooperativeTxid,
	); err != nil {
		return nil, err
	}

	return s.GetRecoveryStatus(ctx, id)
}

// GetRecoveryStatus loads durable recovery state and reconciles any terminal
// unroll result into the recovery row before returning the status.
func (s *Service) GetRecoveryStatus(ctx context.Context, id string) (
	*RecoveryStatus, error) {

	job, err := s.store.GetRecovery(ctx, id)
	if err != nil {
		return nil, err
	}

	return s.reconcileLoaded(ctx, *job)
}

// ListRecoveryStatuses returns all durable recovery rows joined with any
// current unroll status. Listing is read-only except that terminal unroll
// status may be reconciled into the recovery row before the status is returned.
func (s *Service) ListRecoveryStatuses(ctx context.Context) ([]RecoveryStatus,
	error) {

	jobs, err := s.store.ListRecoveries(ctx)
	if err != nil {
		return nil, err
	}

	statuses := make([]RecoveryStatus, 0, len(jobs))
	for i := range jobs {
		status, err := s.reconcileLoaded(ctx, jobs[i])
		if err != nil {
			return nil, err
		}

		statuses = append(statuses, *status)
	}

	return statuses, nil
}

// RestoreNonTerminal reissues unroll admission for every active recovery row
// after daemon startup. Armed jobs are deliberately left dormant; only jobs
// that were already escalated before the crash are restarted.
func (s *Service) RestoreNonTerminal(ctx context.Context) error {
	jobs, err := s.store.ListNonTerminalRecoveries(ctx)
	if err != nil {
		return err
	}

	for i := range jobs {
		job := jobs[i]
		if job.State == vhtlcrecovery.StateArmed {
			s.log.DebugS(ctx, "vhtlc recovery remains armed",
				slog.String("recovery_id", job.ID),
				slog.String(
					"swap_id",
					fmt.Sprintf("%x", job.SwapID),
				),
				slog.String("direction", job.Direction),
				slog.String("action", job.Action),
				slog.String("state", job.State),
				slog.String(
					"exit_policy_kind", job.ExitPolicyKind,
				),
			)

			continue
		}

		attrs := recoveryLogAttrs(job)
		s.log.InfoS(ctx, "resumed vhtlc recovery job", attrs...)

		if err := s.ensureUnroll(ctx, job); err != nil {
			if errors.Is(err, errUnrollPolicyMismatch) {
				if failErr := s.store.FailRecovery(
					ctx, job.ID, err,
				); failErr != nil {

					s.log.WarnS(ctx, "unable to mark vhtlc "+
						"recovery failed after restore "+
						"error", failErr, attrs...)

					continue
				}
			}

			s.log.WarnS(ctx, "vhtlc recovery restore will retry",
				err,
				slog.String("recovery_id", job.ID),
				slog.String("state", job.State),
				slog.String(
					"vtxo_outpoint",
					job.VTXOOutpoint.String(),
				),
			)

			continue
		}

		if _, err := s.reconcileLoaded(ctx, job); err != nil {
			s.log.WarnS(ctx, "unable to reconcile restored "+
				"vhtlc recovery", err, attrs...)
		}
	}

	return nil
}

// ensureUnroll admits the target into unroll using the recovery row's durable
// exit policy identity, then forces the target into unilateral exit through
// the VTXO manager, which owns the transition and starts the durable unroll
// job through its chain-resolver seam.
//
// Admission is asynchronous now: the manager Ask returns once the VTXO is
// transitioned to UnilateralExitState, but the registry job is started by the
// manager's outbox, so the coordinator no longer reads the registry record
// back for synchronous policy-conflict verification. The registry admission
// boundary still validates the (kind, ref) identity, and the recovery row's
// durable policy is re-driven on restart, so the exit policy survives without
// the inline check.
func (s *Service) ensureUnroll(ctx context.Context,
	job vhtlcrecovery.RecoveryJob) error {

	if s.targetMaterializer != nil {
		if err := s.targetMaterializer.EnsureRecoveryTarget(
			ctx, job,
		); err != nil {
			return fmt.Errorf("materialize recovery target: %w",
				err)
		}
	}

	err := s.exiter.ForceExit(ctx, actormsg.ForceUnrollRequest{
		Outpoint: job.VTXOOutpoint,
		Reason:   "vhtlc recovery",
		Trigger:  actormsg.UnrollTriggerManual,
		ExitPolicy: fn.Some(actormsg.ExitPolicy{
			Kind: actormsg.ExitPolicyKind(job.ExitPolicyKind),
			Ref:  actormsg.ExitPolicyRef(job.ID),
		}),
	})
	if err != nil {
		return fmt.Errorf("force vhtlc recovery exit: %w", err)
	}

	s.log.InfoS(ctx, "forced vhtlc recovery exit through vtxo manager",
		append(
			recoveryLogAttrs(job),
			slog.String("vtxo_outpoint", job.VTXOOutpoint.String()),
			slog.String("exit_policy_kind", job.ExitPolicyKind),
			slog.String("exit_policy_ref", job.ID),
		)...,
	)

	// Best-effort policy-conflict guard. The registry admission boundary is
	// first-writer-wins and does not reject a later request that names a
	// different policy, so a pre-existing unroll record (e.g. a standard
	// timeout exit that claimed this outpoint first) would silently keep
	// its policy while this recovery believes it exits under the refund
	// policy. Fail the recovery in that case rather than exit under the
	// wrong policy. A not-yet-visible record is the normal case now that
	// admission is asynchronous through the manager, so it is left to the
	// registry's own validation plus the restart re-drive, not treated as
	// an error.
	status, err := s.unroll.GetStatus(ctx, job.VTXOOutpoint)
	if err != nil {
		s.log.WarnS(ctx, "unable to verify vhtlc recovery unroll "+
			"status after force exit", err, recoveryLogAttrs(job)...)

		return nil
	}
	if status == nil || !status.Found {
		return nil
	}
	if status.ExitPolicyKind != "" &&
		string(status.ExitPolicyKind) != job.ExitPolicyKind {
		return fmt.Errorf("%w: unroll policy kind %q does not match "+
			"recovery kind %q", errUnrollPolicyMismatch,
			status.ExitPolicyKind, job.ExitPolicyKind)
	}
	if status.ExitPolicyRef != "" && status.ExitPolicyRef != job.ID {
		return fmt.Errorf("%w: unroll policy ref %q does not match "+
			"recovery id %q", errUnrollPolicyMismatch,
			status.ExitPolicyRef, job.ID)
	}

	return nil
}

// reconcileLoaded joins one durable recovery row with the current unroll
// status and folds terminal unroll outcomes back into the recovery table.
func (s *Service) reconcileLoaded(ctx context.Context,
	job vhtlcrecovery.RecoveryJob) (*RecoveryStatus, error) {

	status := &RecoveryStatus{
		Job: job,
	}
	if job.State == vhtlcrecovery.StateArmed {
		return status, nil
	}

	unrollStatus, err := s.unroll.GetStatus(ctx, job.VTXOOutpoint)
	if err != nil {
		if job.IsTerminal() {
			s.log.DebugS(ctx, "unable to join terminal vhtlc "+
				"recovery with unroll status", append(
				recoveryLogAttrs(job),
				slog.String("error", err.Error()),
			)...)

			return status, nil
		}

		return nil, err
	}
	if unrollStatus == nil || !unrollStatus.Found {
		return status, nil
	}

	applyUnrollObservation(status, unrollStatus)
	if job.IsTerminal() {
		return status, nil
	}

	switch status.UnrollPhase {
	case unroll.PhasePending, unroll.PhaseMaterializing,
		unroll.PhaseCSVPending, unroll.PhaseSweepBroadcast,
		unroll.PhaseSweepConfirmation,
		unroll.PhaseExternalSpendObserved:
		// PhaseExternalSpendObserved is a parked, non-terminal state:
		// the unroll actor observed an unfinalized external spend of
		// the target and is holding (a reorg can resurrect it), so the
		// recovery job likewise holds its current state until the
		// unroll resolves one way or the other.
		return status, nil

	case unroll.PhaseCompleted:
		attrs := recoveryLogAttrs(job)
		s.log.InfoS(ctx, "vhtlc recovery completed",
			attrs...,
		)

		if err := s.store.CompleteRecovery(ctx, job.ID); err != nil {
			return nil, err
		}

		return s.statusAfterTerminalWrite(ctx, job.ID, status)

	case unroll.PhaseFailed:
		failure := fmt.Errorf("unroll failed")
		if status.UnrollFailure != "" {
			failure = fmt.Errorf("unroll failed: %s",
				status.UnrollFailure)
		}

		attrs := recoveryLogAttrs(job)
		s.log.WarnS(ctx, "vhtlc recovery failed", failure, attrs...)

		if err := s.store.FailRecovery(
			ctx, job.ID, failure,
		); err != nil {
			return nil, err
		}

		return s.statusAfterTerminalWrite(ctx, job.ID, status)
	}

	return status, nil
}

// applyUnrollObservation folds one registry status response into the joined
// recovery status. Active child state is preferred over the registry row
// because it can contain the newest in-memory sweep txid before the registry's
// terminal snapshot has finished its async SQL retry loop.
func applyUnrollObservation(status *RecoveryStatus,
	unrollStatus *unroll.GetStatusResp) {

	status.UnrollFound = true
	status.UnrollActive = unrollStatus.Active
	status.UnrollPhase = unrollStatus.Phase
	status.UnrollFailure = unrollStatus.FailReason
	status.UnrollSweep = unrollStatus.SweepTxid
	if unrollStatus.Active && unrollStatus.State != nil {
		status.UnrollPhase = unrollStatus.State.Phase
		status.UnrollFailure = unrollStatus.State.FailReason
		status.UnrollSweep = unrollStatus.State.SweepTxid
	}
}

// statusAfterTerminalWrite reloads the durable row after a reconcile write
// while preserving the unroll observation that triggered the transition.
func (s *Service) statusAfterTerminalWrite(ctx context.Context, id string,
	status *RecoveryStatus) (*RecoveryStatus, error) {

	job, err := s.store.GetRecovery(ctx, id)
	if err != nil {
		return nil, err
	}

	status.Job = *job

	return status, nil
}

// recoveryLogAttrs returns the common structured fields emitted on every
// recovery log line. It deliberately omits ClaimPreimage because that value is
// secret witness material.
func recoveryLogAttrs(job vhtlcrecovery.RecoveryJob) []any {
	return []any{
		slog.String("recovery_id", job.ID),
		slog.String("swap_id", fmt.Sprintf("%x", job.SwapID)),
		slog.String("direction", job.Direction),
		slog.String("action", job.Action),
		slog.String("state", job.State),
		slog.String("exit_policy_kind", job.ExitPolicyKind),
	}
}
