package round

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"google.golang.org/protobuf/proto"
)

// decodeServiceAdmission checks response shape and takes ownership of its data.
func decodeServiceAdmission(reply *roundpb.ServiceAdmission,
	accepted bool) (*roundpb.ServiceAdmission, error) {

	if reply == nil {
		return nil, nil
	}
	if len(reply.OperationId) != 32 ||
		bytes.Equal(reply.OperationId, make([]byte, 32)) {
		return nil, fmt.Errorf("invalid admission operation ID")
	}
	if _, ok := roundpb.AdmissionCode_name[int32(reply.Code)]; !ok ||
		reply.Code == roundpb.AdmissionCode_ADMISSION_UNSPECIFIED {
		return nil, fmt.Errorf("invalid admission code")
	}
	isAccepted := reply.Code == roundpb.AdmissionCode_ADMISSION_ACCEPTED
	if accepted != isAccepted {
		return nil, fmt.Errorf("admission code disagrees with response")
	}
	if reply.Mode != roundpb.ServiceMode_SERVICE_SCHEDULED &&
		reply.Mode != roundpb.ServiceMode_SERVICE_IMMEDIATE {
		return nil, fmt.Errorf("invalid admission mode")
	}
	if accepted && (reply.Attempt == 0 ||
		reply.ParticipationDeadlineUnix <= 0 ||
		reply.ParticipationDeadlineUnix > 253402300799) {
		return nil, fmt.Errorf("invalid admission attempt or deadline")
	}
	cloned := &roundpb.ServiceAdmission{}
	proto.Merge(cloned, reply)

	return cloned, nil
}

// validateRoundAdmission binds a response before it can change actor routing.
func validateRoundAdmission(ctx context.Context, roundFSM *RoundFSM,
	event *RoundJoined) error {

	state, err := fsmState(ctx, roundFSM.FSM)
	if err != nil {
		return err
	}
	pending, ok := state.(*IntentSentState)
	if !ok {
		return fmt.Errorf("round is not awaiting admission")
	}

	if pending.AdmittedRoundID != (RoundID{}) &&
		pending.AdmittedRoundID != event.RoundID {
		return fmt.Errorf("admission changed round identity")
	}

	return validateServiceAssignment(
		pending.Intents.Service, event.Admission,
	)
}

// validateServiceAssignment checks the accepted assignment against the signed
// request, including the slot identity and the client's absolute expiry.
func validateServiceAssignment(request *types.ServiceRequest,
	admission *roundpb.ServiceAdmission) error {

	if request == nil && admission == nil {
		return nil
	}
	if request == nil || admission == nil {
		return fmt.Errorf("service admission missing or unsolicited")
	}
	reply, err := decodeServiceAdmission(admission, true)
	if err != nil {
		return err
	}
	deadline := reply.ParticipationDeadlineUnix
	if !bytes.Equal(reply.OperationId, request.OperationID[:]) ||
		types.ServiceMode(reply.Mode) != request.Mode ||
		uint64(deadline) > request.ExpiresAtUnix {
		return fmt.Errorf("service admission differs from " +
			"authorization")
	}
	if request.Mode == types.ServiceScheduled {
		slot := reply.AssignedSlot
		if slot == nil ||
			slot.ScheduleVersion != request.ScheduleVersion ||
			slot.SlotIndex != request.SlotIndex ||
			slot.RegistrationCloseUnix >= deadline ||
			deadline > slot.ParticipationDeadlineUnix {
			return fmt.Errorf("service admission differs from " +
				"requested slot")
		}
	} else if reply.AssignedSlot != nil {
		return fmt.Errorf("immediate admission cannot assign a slot")
	}

	return nil
}

// findServiceOperation correlates pre-assignment failures without guessing
// which of several pending operations the server rejected.
func (a *RoundClientActor) findServiceOperation(ctx context.Context,
	reply *roundpb.ServiceAdmission) *RoundFSM {

	scanCtx, cancel := fsmScanContext(ctx)
	defer cancel()
	var match *RoundFSM
	for _, candidate := range a.rounds {
		state, err := fsmState(scanCtx, candidate.FSM)
		if err != nil {
			continue
		}
		pending, ok := state.(*IntentSentState)
		if !ok || pending.Intents.Service == nil ||
			!bytes.Equal(
				reply.OperationId,
				pending.Intents.Service.OperationID[:],
			) {

			continue
		}
		mode := roundpb.ServiceMode(pending.Intents.Service.Mode)
		if mode != reply.Mode {
			continue
		}
		if match != nil {
			return nil
		}
		match = candidate
	}

	return match
}

// fallbackAfterNonAdmission switches once, only on an explicit rejection before
// admission. Missing replies, local timeouts, conflicts, and uncertain signing
// never authorize switching. Existing local input reservations remain held.
func (s *IntentSentState) fallbackAfterNonAdmission(failure *BoardingFailed,
	now time.Time) *ClientStateTransition {

	request := s.Intents.Service
	reply := failure.Admission
	if s.AdmittedRoundID != (RoundID{}) || request == nil || reply == nil ||
		request.Mode != types.ServiceScheduled ||
		!request.AllowFallback ||
		reply.Mode != roundpb.ServiceMode_SERVICE_SCHEDULED ||
		!bytes.Equal(reply.OperationId, request.OperationID[:]) ||
		!now.Before(request.Expiry()) {
		return nil
	}
	switch reply.Code {
	case roundpb.AdmissionCode_ADMISSION_NOT_OPEN,
		roundpb.AdmissionCode_ADMISSION_CLOSED,
		roundpb.AdmissionCode_ADMISSION_FULL,
		roundpb.AdmissionCode_ADMISSION_BUSY,
		roundpb.AdmissionCode_ADMISSION_UNAVAILABLE,
		roundpb.AdmissionCode_ADMISSION_FAILED:
	default:
		return nil
	}
	intents := s.Intents.Clone()
	intents.Service.Mode = types.ServiceImmediate
	intents.Service.ScheduleVersion = 0
	intents.Service.SlotIndex = 0

	return &ClientStateTransition{
		NextState: &PendingRoundAssembly{
			Service: intents.Service, Boarding: intents.Boarding,
			VTXOs: intents.VTXOs, Forfeits: intents.Forfeits,
			Leaves: intents.Leaves,
		},
		NewEvents: fn.Some(ClientEmittedEvent{
			InternalEvent: []ClientEvent{&IntentRequested{}},
		}),
	}
}

// participationFailure stops client progress without claiming that the remote
// operation has relinquished ownership. It carries no fallback authorization.
func participationFailure() *BoardingFailed {
	return &BoardingFailed{
		Reason: "participation deadline reached; " +
			"reconcile operation status",
		Error:       fmt.Errorf("participation deadline reached"),
		Recoverable: true,
	}
}

// boundParticipationEvent fences pre-checkpoint progress even when an expired
// timer is still queued behind a protocol message. Post-checkpoint states use
// status reconciliation and retain their durable transaction ownership.
func (e *ClientEnvironment) boundParticipationEvent(
	event ClientEvent) ClientEvent {

	if e.ParticipationDeadline.IsZero() ||
		e.now().Before(e.ParticipationDeadline) {
		return event
	}
	if _, failed := event.(*BoardingFailed); failed {
		return event
	}

	return participationFailure()
}
