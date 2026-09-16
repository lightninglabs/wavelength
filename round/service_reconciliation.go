package round

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// These aliases keep ownership checks readable beside the lifecycle checks.
const (
	scheduledServiceMode = roundpb.ServiceMode_SERVICE_SCHEDULED
	operationFailed      = roundpb.OperationPhase_OPERATION_FAILED
	operationUnknown     = roundpb.OperationPhase_OPERATION_UNKNOWN
	operationUncertain   = roundpb.OperationPhase_OPERATION_UNCERTAIN
	operationNotAdmitted = roundpb.OperationPhase_OPERATION_NOT_ADMITTED
)

// serviceReconcileLimit bounds foreground recovery before explicit deferral.
const serviceReconcileLimit = 3

// ServiceReconcileState stops pre-checkpoint signing while resolving ownership.
// It retains the original intent and all reservations. Exhausting the probe
// budget makes the state dormant; it never releases inputs or implies failure.
// InputSigSentState continues to own all post-checkpoint reconciliation.
type ServiceReconcileState struct {
	// Cold keeps a restored record from reconstructing a signing request.
	Cold bool

	Intents Intents
	RoundID RoundID
	Probes  uint32
}

// String exposes whether foreground recovery has become dormant.
func (s *ServiceReconcileState) String() string {
	if s.Probes >= serviceReconcileLimit {
		return "ServiceDeferred"
	}

	return "ServiceReconcile"
}

// IsTerminal keeps retained ownership available for a later authoritative
// reply.
func (s *ServiceReconcileState) IsTerminal() bool { return false }

// clientStateSealed implements the state marker.
func (s *ServiceReconcileState) clientStateSealed() {}

// beginServiceReconciliation intercepts service failures before any rollback.
// Typed non-admission keeps its existing direct fallback path.
func beginServiceReconciliation(event ClientEvent, intents Intents,
	roundID RoundID, env *ClientEnvironment) *ClientStateTransition {

	failure, ok := event.(*BoardingFailed)
	if !ok || intents.Service == nil {
		return nil
	}
	if roundID == (RoundID{}) && failure.Admission != nil {
		pending := &IntentSentState{Intents: intents}
		if pending.fallbackAfterNonAdmission(
			failure, env.now(),
		) != nil {
			return nil
		}
	}

	return startServiceReconciliation(intents, roundID, env)
}

// startServiceReconciliation arms the first bounded ownership probe.
func startServiceReconciliation(intents Intents, roundID RoundID,
	env *ClientEnvironment) *ClientStateTransition {

	s := &ServiceReconcileState{Intents: intents.Clone(), RoundID: roundID}

	transition := s.probe(env)
	if transition.NewEvents.IsSome() {
		events := transition.NewEvents.UnwrapOr(ClientEmittedEvent{})
		events.Outbox = append(events.Outbox, &CancelTimeoutReq{
			RoundKey: env.RoundKey,
			Phase:    TimeoutPhaseParticipation,
		})
		transition.NewEvents = fn.Some(events)
	}

	return transition
}

// probe bounds traffic by both a fixed count and the original expiry. No reply
// means deferral with reservations retained, never permission to execute again.
func (s *ServiceReconcileState) probe(
	env *ClientEnvironment) *ClientStateTransition {

	next := *s
	remaining := s.Intents.Service.Expiry().Sub(env.now())
	if s.Probes >= serviceReconcileLimit || (!s.Cold && remaining <= 0) {
		next.Probes = serviceReconcileLimit

		return selfLoop(&next)
	}
	delay := env.StatusReconcileTimeout
	if delay <= 0 {
		delay = 5 * time.Second
	}
	if remaining > 0 {
		delay = min(delay, remaining)
	}
	next.Probes++

	return &ClientStateTransition{
		NextState: &next,
		NewEvents: fn.Some(ClientEmittedEvent{Outbox: []ClientOutMsg{
			&QueryRoundStatusOutbox{
				RoundID:     s.RoundID,
				OperationID: s.Intents.Service.OperationID,
			},
			&StartTimeoutReq{
				RoundKey: env.RoundKey,
				Phase:    TimeoutPhaseRegistration,
				Duration: delay,
			},
		}}),
	}
}

// ProcessEvent permits a mode switch only after an authoritative safe result.
func (s *ServiceReconcileState) ProcessEvent(ctx context.Context,
	event ClientEvent, env *ClientEnvironment) (*ClientStateTransition,
	error) {

	switch evt := event.(type) {
	case *RegistrationTimedOut:
		return s.probe(env), nil

	case *RoundStatusReported:
		op := evt.Operation
		req := s.Intents.Service
		if op == nil ||
			!bytes.Equal(op.OperationId, req.OperationID[:]) {
			return selfLoop(s), nil
		}
		if s.RoundID != (RoundID{}) && evt.RoundID != s.RoundID {
			return selfLoop(s), nil
		}
		safe := op.Phase == operationFailed ||
			(op.Phase == operationNotAdmitted &&
				s.RoundID == (RoundID{}))
		if !safe {
			return selfLoop(s), nil
		}
		if s.Cold || req.Mode != types.ServiceScheduled ||
			!req.AllowFallback ||
			!env.now().Before(req.Expiry()) ||
			(op.Phase == operationFailed &&
				op.Mode != scheduledServiceMode) {
			return s.finishSafeOperation(ctx, env)
		}
		intents := s.Intents.Clone()
		intents.Service.Mode = types.ServiceImmediate
		intents.Service.ScheduleVersion = 0
		intents.Service.SlotIndex = 0
		env.ParticipationDeadline = time.Time{}

		return &ClientStateTransition{
			NextState: &PendingRoundAssembly{
				Service:  intents.Service,
				Boarding: intents.Boarding,
				VTXOs:    intents.VTXOs,
				Forfeits: intents.Forfeits,
				Leaves:   intents.Leaves,
			},
			NewEvents: fn.Some(
				ClientEmittedEvent{
					InternalEvent: []ClientEvent{
						&IntentRequested{},
					},
				},
			),
		}, nil

	default:
		return selfLoop(s), nil
	}
}

// validateOperationReport rejects contradictory lifecycle and ownership claims.
func validateOperationReport(report *roundpb.ClientRoundStatusReport) error {
	op := report.Operation
	if op == nil {
		return nil
	}
	if len(op.OperationId) != 32 ||
		bytes.Equal(op.OperationId, make([]byte, 32)) {
		return fmt.Errorf("invalid status operation ID")
	}
	if _, ok := roundpb.OperationPhase_name[int32(op.Phase)]; !ok {
		return fmt.Errorf("unknown operation phase")
	}
	if op.Phase == operationUnknown ||
		op.Phase == operationNotAdmitted {

		if report.Status != 0 {
			return fmt.Errorf("unknown operation has a round " +
				"verdict")
		}

		return nil
	}
	if op.Attempt == 0 || len(op.RoundId) != 16 ||
		bytes.Equal(op.RoundId, make([]byte, 16)) ||
		!bytes.Equal(op.RoundId, report.RoundId) ||
		(op.Mode != roundpb.ServiceMode_SERVICE_SCHEDULED &&
			op.Mode != roundpb.ServiceMode_SERVICE_IMMEDIATE) ||
		op.ParticipationDeadlineUnix == 0 ||
		op.ParticipationDeadlineUnix > 253402300799 {
		return fmt.Errorf("invalid operation assignment")
	}
	if report.Status == roundStatusDead &&
		op.Phase != operationFailed {
		return fmt.Errorf("owned operation cannot report a dead round")
	}

	return nil
}

// findReconcilingOperation routes a lost-admission answer by operation ID.
func (a *RoundClientActor) findReconcilingOperation(ctx context.Context,
	op *roundpb.OperationStatus) *RoundFSM {

	scanCtx, cancel := fsmScanContext(ctx)
	defer cancel()
	for _, candidate := range a.rounds {
		state, err := fsmState(scanCtx, candidate.FSM)
		if err != nil {
			continue
		}
		reconciling, ok := state.(*ServiceReconcileState)
		if ok && bytes.Equal(
			op.OperationId,
			reconciling.Intents.Service.OperationID[:],
		) {
			return candidate
		}
	}

	return nil
}

// participationExpired checks the fixed assignment deadline after slow work.
func (e *ClientEnvironment) participationExpired() bool {
	return !e.ParticipationDeadline.IsZero() &&
		!e.now().Before(e.ParticipationDeadline)
}

// participationContext bounds signing APIs that accept cancellation.
func (e *ClientEnvironment) participationContext(ctx context.Context) (
	context.Context, context.CancelFunc) {

	if e.ParticipationDeadline.IsZero() {
		return context.WithCancel(ctx)
	}

	return context.WithTimeout(ctx, e.ParticipationDeadline.Sub(e.now()))
}

// fenceServiceTransition rechecks the deadline before handing off an outbox.
// If checkpointing already committed, retain that state and its notifications;
// only suppress signature handoff. A pre-checkpoint timeout retains ownership
// in reconciliation. Neither path rolls back a committed signing checkpoint.
func fenceServiceTransition(ctx context.Context, source ClientState,
	transition *ClientStateTransition, err error, intents Intents,
	roundID RoundID,
	env *ClientEnvironment) (*ClientStateTransition, error) {

	if intents.Service == nil || !env.participationExpired() {
		return transition, err
	}
	if transition != nil {
		switch transition.NextState.(type) {
		case *ServiceReconcileState:
			return transition, err

		case *InputSigSentState:
			events := transition.NewEvents.UnwrapOr(
				ClientEmittedEvent{},
			)
			outbox := make([]ClientOutMsg, 0, len(events.Outbox))
			for _, message := range events.Outbox {
				switch message.(type) {
				case *SubmitForfeitSigRequest,
					*SubmitVTXOForfeitSigsToServer:

					continue
				}
				outbox = append(outbox, message)
			}
			events.Outbox = outbox
			transition.NewEvents = fn.Some(events)

			return transition, err
		}
	}
	if len(signingSessionsFromState(source)) != 0 {
		cleanupServiceSessions(ctx, source, env)
	} else if transition != nil {
		if next, ok := transition.NextState.(ClientState); ok {
			cleanupServiceSessions(ctx, next, env)
		}
	}

	return startServiceReconciliation(intents, roundID, env), nil
}

// cleanupServiceSessions releases external nonce state before discarding a
// signing state. It does not touch input or operation reservations.
func cleanupServiceSessions(ctx context.Context, state ClientState,
	env *ClientEnvironment) {

	if err := cleanupSignerSessions(
		signingSessionsFromState(state),
	); err != nil {

		env.Log.WarnS(ctx, "Unable to clean up MuSig2 sessions", err)
	}
}

// finishSafeOperation releases a live pre-checkpoint operation only after the
// authenticated status path proved failure or non-admission. Cold records and
// unresolved probes never reach this method.
func (s *ServiceReconcileState) finishSafeOperation(ctx context.Context,
	env *ClientEnvironment) (*ClientStateTransition, error) {

	if env.ServiceStore != nil {
		if err := env.ServiceStore.CompleteServiceOperation(
			ctx, s.Intents.Service.OperationID,
		); err != nil {
			return selfLoop(s), err
		}
	}
	reason := "service operation ended before signing"
	transition := failWithNotification(
		reason, fmt.Errorf("%s", reason), true, fn.Some(s.RoundID),
	)

	return releaseForfeitsOnFailure(
		transition, nil, fn.Some(s.RoundID), s.Intents.Forfeits,
	)
}
