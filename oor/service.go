package oor

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/db"
)

// oorService is the default OORService implementation.
type oorService struct {
	outgoingRef   actor.ActorRef[ActorMsg, ActorResp]
	outgoingActor *OORClientActor

	incomingSource IncomingEventSource
	cursorStore    IncomingCursorStore
	unrollResolver UnrollPackageResolver

	incomingOutboxHandler OutboxHandler

	incomingPageSize     int32
	incomingPollInterval time.Duration
	incomingPollJitter   time.Duration
	jitterRand           *rand.Rand
	jitterRandMu         sync.Mutex

	workerMu     sync.Mutex
	workerCancel context.CancelFunc
	workerDone   chan struct{}

	incomingRunMu sync.Mutex

	statusMu sync.RWMutex
	status   IncomingSyncStatus
}

// NewOORService constructs a high-level OOR service for outgoing and incoming
// flow orchestration.
//
// The constructor wires local persistence handling in front of the provided
// transport/signing handler so both outgoing and incoming paths share one
// consistent outbox boundary.
func NewOORService(cfg ServiceConfig) (OORService, error) {
	if cfg.VTXOStore == nil {
		return nil, fmt.Errorf("vtxo store must be provided")
	}

	if cfg.OperatorKey == nil {
		return nil, fmt.Errorf("operator key must be provided")
	}

	if cfg.ResolveIncomingClientKey == nil {
		return nil, fmt.Errorf("incoming client key resolver must be " +
			"provided")
	}

	if cfg.ResolveIncomingMetadata == nil {
		return nil, fmt.Errorf("incoming metadata resolver must be " +
			"provided")
	}

	actorID := cfg.ActorID
	if actorID == "" {
		actorID = DefaultActorServiceKeyName
	}

	pageSize := cfg.IncomingPageSize
	if pageSize <= 0 {
		pageSize = DefaultIncomingPageSize
	}

	pollInterval := cfg.IncomingPollInterval
	if pollInterval <= 0 {
		pollInterval = DefaultIncomingPollInterval
	}

	localHandler := &LocalPersistenceOutboxHandler{
		Next:                     cfg.TransportOutboxHandler,
		Store:                    cfg.VTXOStore,
		PackageStore:             cfg.PackageStore,
		OperatorKey:              cfg.OperatorKey,
		ExitDelay:                cfg.ExitDelay,
		ResolveIncomingClientKey: cfg.ResolveIncomingClientKey,
		ResolveIncomingMetadata:  cfg.ResolveIncomingMetadata,
	}

	outgoingRef, outgoingActor, err := buildOutgoingActorClient(
		cfg, actorID, localHandler,
	)
	if err != nil {
		return nil, err
	}

	return &oorService{
		outgoingRef:           outgoingRef,
		outgoingActor:         outgoingActor,
		incomingSource:        cfg.IncomingSource,
		cursorStore:           cfg.IncomingCursorStore,
		unrollResolver:        cfg.UnrollResolver,
		incomingOutboxHandler: localHandler,
		incomingPageSize:      pageSize,
		incomingPollInterval:  pollInterval,
		incomingPollJitter:    cfg.IncomingPollJitter,
		// #nosec G404 -- non-crypto jitter for polling.
		jitterRand: rand.New(rand.NewSource(time.Now().UnixNano())),
	}, nil
}

// StartOutgoing starts one outgoing transfer via the configured outgoing actor.
func (s *oorService) StartOutgoing(ctx context.Context,
	req StartOutgoingRequest) (SessionID, error) {

	if s == nil {
		return SessionID{}, fmt.Errorf("service must be provided")
	}

	result, err := s.askOutgoing(ctx, &StartTransferRequest{
		Policy:     req.Policy,
		Inputs:     req.Inputs,
		Recipients: req.Recipients,
	})
	if err != nil {
		return SessionID{}, err
	}

	resp, ok := result.(*StartTransferResponse)
	if !ok || resp == nil {
		return SessionID{}, fmt.Errorf("unexpected response type: %T",
			result)
	}

	return resp.SessionID, nil
}

// GetOutgoingState returns a caller-facing state summary for one session.
func (s *oorService) GetOutgoingState(ctx context.Context,
	sessionID SessionID) (OutgoingStateView, error) {

	if s == nil {
		return OutgoingStateView{}, fmt.Errorf(
			"service must be provided",
		)
	}

	result, err := s.askOutgoing(
		ctx, &GetStateRequest{SessionID: sessionID},
	)
	if err != nil {
		return OutgoingStateView{}, err
	}

	resp, ok := result.(*GetStateResponse)
	if !ok || resp == nil || resp.State == nil {
		return OutgoingStateView{}, fmt.Errorf(
			"unexpected response type: %T",
			result,
		)
	}

	return outgoingStateViewFromState(sessionID, resp.State), nil
}

// SyncIncomingOnce executes one incoming-sync cycle.
func (s *oorService) SyncIncomingOnce(ctx context.Context) error {
	if s == nil {
		return fmt.Errorf("service must be provided")
	}

	_, _, err := s.runIncomingCycle(ctx)

	return err
}

// StartIncomingSync starts the background incoming-sync loop.
func (s *oorService) StartIncomingSync(ctx context.Context) error {
	if s == nil {
		return fmt.Errorf("service must be provided")
	}

	if ctx == nil {
		ctx = context.Background()
	}

	s.workerMu.Lock()
	defer s.workerMu.Unlock()

	if s.workerCancel != nil {
		return nil
	}

	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	s.workerCancel = cancel
	s.workerDone = done

	s.setWorkerRunning(true)

	go s.runIncomingWorker(loopCtx, done)

	return nil
}

// StopIncomingSync stops the background incoming-sync loop.
func (s *oorService) StopIncomingSync(ctx context.Context) error {
	if s == nil {
		return nil
	}

	s.workerMu.Lock()
	cancel := s.workerCancel
	done := s.workerDone

	s.workerCancel = nil
	s.workerDone = nil
	if cancel != nil {
		cancel()
	}

	s.workerMu.Unlock()

	if done == nil {
		s.setWorkerRunning(false)
		return nil
	}

	select {
	case <-done:
		return nil

	case <-ctx.Done():
		return ctx.Err()
	}
}

// GetIncomingSyncStatus returns the latest incoming-sync status snapshot.
func (s *oorService) GetIncomingSyncStatus() IncomingSyncStatus {
	if s == nil {
		return IncomingSyncStatus{}
	}

	s.statusMu.RLock()
	defer s.statusMu.RUnlock()

	return s.status
}

// ResolveUnrollPackages resolves locally persisted package artifacts for one
// outpoint.
func (s *oorService) ResolveUnrollPackages(ctx context.Context,
	outpoint wire.OutPoint) (*db.OORUnrollPackages, error) {

	if s == nil || s.unrollResolver == nil {
		return nil, fmt.Errorf("unroll resolver must be provided")
	}

	return s.unrollResolver.ResolveUnrollPackages(ctx, outpoint)
}

// Stop stops the service and all managed workers.
func (s *oorService) Stop(ctx context.Context) error {
	if s == nil {
		return nil
	}

	if err := s.StopIncomingSync(ctx); err != nil {
		return err
	}

	if s.outgoingActor != nil {
		s.outgoingActor.Stop()
	}

	return nil
}

// runIncomingWorker runs the background incoming-sync loop until canceled.
func (s *oorService) runIncomingWorker(
	ctx context.Context, done chan struct{},
) {

	defer close(done)
	defer s.setWorkerRunning(false)

	for {
		if ctx.Err() != nil {
			return
		}

		_, _, _ = s.runIncomingCycle(ctx)

		delay := s.nextIncomingPollDelay()
		timer := time.NewTimer(delay)

		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}

			return

		case <-timer.C:
		}
	}
}

// runIncomingCycle executes one tracked incoming-sync cycle and updates status.
func (s *oorService) runIncomingCycle(ctx context.Context) (int, int, error) {
	s.incomingRunMu.Lock()
	defer s.incomingRunMu.Unlock()

	start := time.Now()
	s.setCycleStart(start)

	scripts, events, err := s.syncIncomingOnce(ctx)
	s.setCycleFinish(time.Now(), scripts, events, err)

	return scripts, events, err
}

// syncIncomingOnce performs one full poll/process pass across tracked scripts.
func (s *oorService) syncIncomingOnce(ctx context.Context) (int, int, error) {
	if s.incomingSource == nil {
		return 0, 0, fmt.Errorf("incoming source must be provided")
	}

	if s.cursorStore == nil {
		return 0, 0, fmt.Errorf(
			"incoming cursor store must be provided",
		)
	}

	if s.incomingOutboxHandler == nil {
		return 0, 0, fmt.Errorf(
			"incoming outbox handler must be provided",
		)
	}

	scripts, err := s.cursorStore.ListOwnedReceiveScripts(ctx)
	if err != nil {
		return 0, 0, err
	}

	totalEvents := 0
	for i := range scripts {
		processed, err := s.syncIncomingScript(ctx, scripts[i])
		if err != nil {
			return i, totalEvents, err
		}

		totalEvents += processed
	}

	return len(scripts), totalEvents, nil
}

// syncIncomingScript processes all available events for one recipient script.
func (s *oorService) syncIncomingScript(ctx context.Context,
	script OwnedReceiveScript) (int, error) {

	if len(script.PkScript) == 0 {
		return 0, fmt.Errorf("owned receive script must be provided")
	}

	cursor, err := s.cursorStore.GetRecipientCursor(ctx, script.PkScript)
	if err != nil {
		return 0, err
	}

	lastEventID := int64(0)
	if cursor != nil {
		lastEventID = cursor.LastEventID
	}

	processed := 0

	for {
		events, err := s.incomingSource.ListRecipientEvents(
			ctx, script.PkScript, lastEventID, s.incomingPageSize,
		)
		if err != nil {
			return processed, err
		}

		if len(events) == 0 {
			return processed, nil
		}

		for i := range events {
			event := events[i]
			if event == nil {
				return processed, fmt.Errorf(
					"incoming event must be provided",
				)
			}

			if err := validateIncomingEvent(script.PkScript,
				lastEventID, event); err != nil {
				return processed, err
			}

			if err := s.processIncomingEvent(
				ctx, event,
			); err != nil {
				return processed, err
			}

			sessionID := event.SessionID
			err := s.cursorStore.UpsertRecipientCursor(
				ctx, script.PkScript, event.EventID, &sessionID,
			)
			if err != nil {
				return processed, err
			}

			lastEventID = event.EventID
			processed++
		}

		if int32(len(events)) < s.incomingPageSize {
			return processed, nil
		}
	}
}

// processIncomingEvent drives one incoming event through the receive FSM.
func (s *oorService) processIncomingEvent(ctx context.Context,
	event *IncomingRecipientEvent) error {

	// Incoming receive processing intentionally uses a short-lived FSM per
	// event. Restart safety is provided by the persisted recipient cursor,
	// plus idempotent local materialization in the outbox boundary.
	session, err := NewReceiveSession(ctx, event.ArkPSBT, event.SessionID)
	if err != nil {
		return err
	}
	defer session.FSM.Stop()

	outbox, err := askFSMEvent(ctx, session.FSM, &IncomingTransferEvent{
		SessionID:            event.SessionID,
		ArkPSBT:              event.ArkPSBT,
		FinalCheckpointPSBTs: event.FinalCheckpointPSBTs,
	})
	if err != nil {
		return err
	}

	if err := s.driveReceiveOutbox(ctx, event.SessionID,
		session.FSM, outbox); err != nil {
		return err
	}

	state, err := currentReceiveState(session.FSM)
	if err != nil {
		return err
	}

	if _, ok := state.(*ReceiveCompleted); !ok {
		return fmt.Errorf(
			"incoming session did not reach completion: %s",
			state.String(),
		)
	}

	return nil
}

// driveReceiveOutbox executes receive-side outbox requests and feeds follow-up
// events back into the receive FSM until no outbox remains.
func (s *oorService) driveReceiveOutbox(ctx context.Context,
	sessionID SessionID, fsm *StateMachine, outbox []OutboxEvent) error {

	for _, msg := range outbox {
		followUps, err := s.incomingOutboxHandler.Handle(
			ctx, sessionID, msg,
		)
		if err != nil {
			return fmt.Errorf("handle incoming outbox %s: %w",
				msg.outboxType(), err)
		}

		for _, followUp := range followUps {
			nextOutbox, err := askFSMEvent(ctx, fsm, followUp)
			if err != nil {
				return err
			}

			if err := s.driveReceiveOutbox(ctx,
				sessionID, fsm, nextOutbox); err != nil {
				return err
			}
		}
	}

	return nil
}

// currentReceiveState returns the concrete receive FSM state type.
func currentReceiveState(fsm *StateMachine) (ReceiveState, error) {
	if fsm == nil {
		return nil, fmt.Errorf("fsm must be provided")
	}

	state, err := fsm.CurrentState()
	if err != nil {
		return nil, err
	}

	receiveState, ok := state.(ReceiveState)
	if !ok {
		return nil, fmt.Errorf(
			"unexpected receive state type: %T", state,
		)
	}

	return receiveState, nil
}

// askFSMEvent sends one event into an FSM and returns emitted outbox requests.
func askFSMEvent(ctx context.Context, fsm *StateMachine,
	event Event) ([]OutboxEvent, error) {

	if fsm == nil {
		return nil, fmt.Errorf("fsm must be provided")
	}

	future := fsm.AskEvent(ctx, event)
	result := future.Await(ctx)
	if result.IsErr() {
		return nil, result.Err()
	}

	return result.UnwrapOr(nil), nil
}

// validateIncomingEvent validates cursor ordering and script targeting.
func validateIncomingEvent(expectedScript []byte, lastEventID int64,
	event *IncomingRecipientEvent) error {

	if event.EventID <= lastEventID {
		return fmt.Errorf(
			"incoming event id %d is not strictly after %d",
			event.EventID, lastEventID,
		)
	}

	if len(event.RecipientPkScript) > 0 &&
		!bytes.Equal(event.RecipientPkScript, expectedScript) {

		return fmt.Errorf("incoming event recipient script mismatch")
	}

	if event.ArkPSBT == nil || event.ArkPSBT.UnsignedTx == nil {
		return fmt.Errorf("incoming event ark psbt must be provided")
	}

	if len(event.FinalCheckpointPSBTs) == 0 {
		return fmt.Errorf("incoming event checkpoints must be provided")
	}

	return nil
}

// outgoingStateViewFromState maps a concrete outgoing state to a stable view.
func outgoingStateViewFromState(sessionID SessionID,
	state State) OutgoingStateView {

	view := OutgoingStateView{
		SessionID: sessionID,
		StateName: state.String(),
		Terminal:  state.IsTerminal(),
	}

	if failedState, ok := state.(*Failed); ok {
		view.FailedReason = failedState.Reason
	}

	if retryState, ok := state.(*RetryBackoff); ok {
		view.RetryAfter = retryState.RetryAfter
		view.RetryReason = retryState.Reason
	}

	return view
}

// nextIncomingPollDelay calculates one worker sleep interval including jitter.
func (s *oorService) nextIncomingPollDelay() time.Duration {
	delay := s.incomingPollInterval
	if s.incomingPollJitter <= 0 {
		return delay
	}

	jitterMax := int64(s.incomingPollJitter) + 1
	if jitterMax <= 0 {
		return delay
	}

	s.jitterRandMu.Lock()
	jitter := time.Duration(s.jitterRand.Int63n(jitterMax))
	s.jitterRandMu.Unlock()

	return delay + jitter
}

// askOutgoing sends one command to the configured outgoing actor endpoint.
func (s *oorService) askOutgoing(ctx context.Context,
	msg ActorMsg) (ActorResp, error) {

	if s.outgoingRef != nil {
		future := s.outgoingRef.Ask(ctx, msg)
		result := future.Await(ctx)
		if result.IsErr() {
			return nil, result.Err()
		}

		return result.UnwrapOr(nil), nil
	}

	if s.outgoingActor != nil {
		result := s.outgoingActor.Receive(ctx, msg)
		if result.IsErr() {
			return nil, result.Err()
		}

		return result.UnwrapOr(nil), nil
	}

	return nil, fmt.Errorf("outgoing actor must be configured")
}

// buildOutgoingActorClient resolves or constructs outgoing actor wiring.
func buildOutgoingActorClient(cfg ServiceConfig, actorID string,
	localHandler OutboxHandler) (actor.ActorRef[ActorMsg, ActorResp],
	*OORClientActor, error) {

	if cfg.OutgoingRef != nil {
		return cfg.OutgoingRef, nil, nil
	}

	if cfg.ActorSystem != nil {
		serviceKey := ActorServiceKey(actorID)
		if cfg.OutgoingServiceKey != nil {
			serviceKey = *cfg.OutgoingServiceKey
		}

		refs := actor.FindInReceptionist(
			cfg.ActorSystem.Receptionist(), serviceKey,
		)
		if len(refs) == 0 {
			return nil, nil, fmt.Errorf(
				"no outgoing actor registered for service key",
			)
		}

		return serviceKey.Ref(cfg.ActorSystem), nil, nil
	}

	if cfg.DeliveryStore == nil {
		return nil, nil, fmt.Errorf("delivery store must be provided")
	}

	localActor := NewOORClientActor(ClientActorCfg{
		ActorID:       actorID,
		DeliveryStore: cfg.DeliveryStore,
		OutboxHandler: localHandler,
		PackageStore:  cfg.PackageStore,
	})
	if localActor.startupErr != nil {
		return nil, nil, localActor.startupErr
	}

	return nil, localActor, nil
}

// setWorkerRunning updates the running flag in status.
func (s *oorService) setWorkerRunning(running bool) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()

	s.status.Running = running
}

// setCycleStart records start metadata for one sync cycle.
func (s *oorService) setCycleStart(start time.Time) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()

	s.status.LastRunStartedAt = start
}

// setCycleFinish records completion metadata for one sync cycle.
func (s *oorService) setCycleFinish(finished time.Time,
	processedScripts int, processedEvents int, err error) {

	s.statusMu.Lock()
	defer s.statusMu.Unlock()

	s.status.LastRunFinishedAt = finished
	s.status.LastRunProcessedScripts = processedScripts
	s.status.LastRunProcessedEvents = processedEvents
	s.status.TotalProcessedScripts += int64(processedScripts)
	s.status.TotalProcessedEvents += int64(processedEvents)

	if err != nil {
		s.status.LastError = err.Error()
	} else {
		s.status.LastError = ""
	}
}

var _ OORService = (*oorService)(nil)
