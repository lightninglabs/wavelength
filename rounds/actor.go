package rounds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/timeout"
	"github.com/lightninglabs/darepo/batch"
	"github.com/lightninglabs/darepo/batchwatcher"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightninglabs/darepo/metrics"
	"github.com/lightninglabs/darepo/vtxo"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// ActorConfig contains the configuration parameters for the rounds actor.
type ActorConfig struct {
	// ChainParams are the Bitcoin network parameters.
	ChainParams *chaincfg.Params

	// Log is an optional logger. When None, logging is disabled.
	Log fn.Option[btclog.Logger]

	// Terms are the operator terms for the round.
	Terms *batch.Terms

	// ForfeitScript is the output script that clients must use for the
	// penalty output in forfeit transactions. This allows the server to
	// claim forfeited VTXO funds.
	ForfeitScript []byte

	// ClientsConn is a reference to the ClientsConnectionActor for sending
	// messages to registered clients.
	ClientsConn actor.TellOnlyRef[clientconn.ClientConnMsg]

	// BoardingInputLocker provides global locking of boarding inputs
	// across concurrent rounds.
	BoardingInputLocker BoardingInputLocker

	// ChainSource provides access to on-chain data. If not set, the FSM
	// relies on client-provided TxProofs for UTXO validation.
	ChainSource ChainSource

	// HeaderVerifier validates that a block header exists on the best
	// chain at the claimed height. Used to verify TxProofs when
	// ChainSource is nil. If nil and ChainSource is nil, TxProof
	// header verification is skipped (regtest only).
	HeaderVerifier proof.HeaderVerifier

	// TimeoutActor is a reference to the timeout scheduling actor.
	TimeoutActor actor.TellOnlyRef[timeout.Msg]

	// SelfRef is a reference to this actor for receiving asynchronous
	// notifications (e.g., timeout expirations). The actor uses this to
	// create mapped references for callback registration.
	SelfRef actor.TellOnlyRef[ActorMsg]

	// WalletController provides access to wallet operations for batch
	// building.
	WalletController WalletController

	// FeeEstimator provides fee rate estimation for transactions.
	FeeEstimator chainfee.Estimator

	// WalletAccount is the wallet account to use for funding batch
	// transactions.
	WalletAccount string

	// ConfTarget is the confirmation target to use for fee estimation.
	ConfTarget uint32

	// MinConfs is the minimum number of confirmations required for wallet
	// UTXOs to be used for funding.
	MinConfs int32

	// RoundStore provides persistent storage for rounds.
	RoundStore RoundStore

	// VTXOStore provides persistent storage for rounds VTXOs.
	//
	// Rounds persist additional metadata (tree descriptor + forfeit info)
	// that the generic OOR-focused `vtxo.Store` does not carry yet, so this
	// remains a rounds-scoped projection interface.
	VTXOStore VTXOStore

	// ChainSourceActor is a reference to the chain source actor for
	// broadcasting transactions and subscribing to confirmations.
	ChainSourceActor actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]

	// ConfirmationTarget is the number of confirmations required before
	// transitioning a round to the confirmed state.
	ConfirmationTarget uint32

	// BatchWatcher is an optional reference to the batch watcher actor for
	// registering confirmed batches for on-chain tree monitoring.
	BatchWatcher fn.Option[actor.ActorRef[
		batchwatcher.BatchWatcherMsg, batchwatcher.BatchWatcherResp,
	]]

	// VTXOLocker provides mutual exclusion for VTXO outpoints across
	// concurrent subsystems (rounds and OOR transfers).
	VTXOLocker vtxo.Locker

	// DisableJoinRequestAuth skips join-request BIP-322 validation.
	// This should only be enabled in focused unit tests.
	DisableJoinRequestAuth bool

	// ShouldSeal is an optional predicate evaluated after each
	// successful client join. When it returns true the round is
	// sealed immediately without waiting for the registration
	// timeout. A nil predicate is equivalent to "never seal early".
	ShouldSeal SealPredicate

	// MetricsActor is an optional reference to the centralized
	// metrics actor. When set, the rounds actor sends metric
	// events here instead of calling Prometheus directly.
	MetricsActor fn.Option[actor.TellOnlyRef[metrics.Msg]]
}

// Actor is the server rounds actor. It wraps the round FSM and manages its
// lifecycle. It tracks multiple concurrent rounds in a unified map, with the
// currentRoundID identifying which round accepts new registrations.
type Actor struct {
	// cfg contains all the configuration for this actor.
	cfg *ActorConfig

	// rounds holds all active rounds keyed by their IDs. This includes the
	// round currently accepting registrations and any sealed rounds still
	// being processed.
	rounds map[RoundID]*RoundFSM

	// currentRoundID identifies the round that currently accepts new
	// registrations.
	// Access the round via rounds[currentRoundID].
	currentRoundID RoundID

	// clientRounds tracks which rounds each client is participating
	// in. Updated when clients join rounds and when rounds
	// complete/fail.
	clientRounds map[clientconn.ClientID]map[RoundID]struct{}

	log btclog.Logger
}

// tellMetrics sends a metric message to the metrics actor if
// configured. Errors are silently ignored since metrics are
// best-effort.
func (a *Actor) tellMetrics(ctx context.Context, msg metrics.Msg) {
	a.cfg.MetricsActor.WhenSome(
		func(ref actor.TellOnlyRef[metrics.Msg]) {
			_ = ref.Tell(ctx, msg)
		},
	)
}

// makeTimeoutID creates a composite timeout ID from a round ID and phase.
// The format is "roundID:phase" which allows the actor to identify both which
// round the timeout belongs to and which phase scheduled it.
func makeTimeoutID(roundID RoundID, phase TimeoutPhase) timeout.ID {
	return timeout.ID(fmt.Sprintf("%s:%s", uuid.UUID(roundID), phase))
}

// parseTimeoutID extracts the round ID and phase from a composite timeout ID.
// Returns an error if the ID format is invalid.
func parseTimeoutID(id timeout.ID) (RoundID, TimeoutPhase, error) {
	parts := strings.SplitN(string(id), ":", 2)
	if len(parts) != 2 {
		return RoundID{}, "", fmt.Errorf("invalid timeout ID "+
			"format: %s", id)
	}

	roundUUID, err := uuid.Parse(parts[0])
	if err != nil {
		return RoundID{}, "", fmt.Errorf("invalid round ID in "+
			"timeout ID: %w", err)
	}

	return RoundID(roundUUID), TimeoutPhase(parts[1]), nil
}

// NewActor creates a new server rounds actor with the provided configuration.
// It will check the rounds-store for any rounds that still need to be tracked
// and resume them. It will create a new "live" round that will accept new
// registrations.
func NewActor(cfg *ActorConfig) *Actor {
	clientRounds := make(map[clientconn.ClientID]map[RoundID]struct{})

	return &Actor{
		cfg:          cfg,
		log:          cfg.Log.UnwrapOr(btclog.Disabled),
		rounds:       make(map[RoundID]*RoundFSM),
		clientRounds: clientRounds,
	}
}

// Start initializes the actor. It loads any pending rounds from storage that
// need to be tracked until confirmation, then creates a new live round FSM to
// accept registrations.
func (a *Actor) Start(ctx context.Context) error {
	// Validate that the UTXO lock duration is long enough to cover
	// the worst-case round lifetime. This prevents silent lease
	// expiry mid-round which could lead to double-spend attempts.
	if err := a.cfg.Terms.ValidateFundPsbtLockDuration(); err != nil {
		return fmt.Errorf("invalid terms: %w", err)
	}

	// Load previous rounds from storage that still need to be managed
	// (e.g., rounds awaiting confirmation).
	if err := a.loadPendingRounds(ctx); err != nil {
		return fmt.Errorf("unable to load pending rounds: %w", err)
	}

	// Create a new round to accept registrations.
	round, err := a.newRoundFSM(ctx)
	if err != nil {
		return fmt.Errorf("unable to create new round FSM: %w", err)
	}

	a.rounds[round.RoundID] = round
	a.currentRoundID = round.RoundID

	return nil
}

// getCurrentRound returns the round FSM currently accepting registrations.
func (a *Actor) getCurrentRound() *RoundFSM {
	return a.rounds[a.currentRoundID]
}

// trackClientJoin records that a client has joined a specific round.
func (a *Actor) trackClientJoin(ctx context.Context,
	clientID clientconn.ClientID, roundID RoundID) {

	if a.clientRounds[clientID] == nil {
		a.clientRounds[clientID] = make(map[RoundID]struct{})
	}
	a.clientRounds[clientID][roundID] = struct{}{}

	a.tellMetrics(ctx, &metrics.ClientJoinedRoundMsg{
		RoundID: roundID.String(),
	})
}

// untrackRound removes a round from all clients' tracking. This is called when
// a round completes or fails.
func (a *Actor) untrackRound(roundID RoundID) {
	for clientID := range a.clientRounds {
		delete(a.clientRounds[clientID], roundID)
		// Clean up empty client entries.
		if len(a.clientRounds[clientID]) == 0 {
			delete(a.clientRounds, clientID)
		}
	}
}

// getClientRounds returns the list of round IDs that a client is currently
// participating in. This is a private helper; external callers should use
// GetClientRoundsRequest message via the actor's Receive method.
func (a *Actor) getClientRounds(clientID clientconn.ClientID) []RoundID {
	roundSet := a.clientRounds[clientID]
	if len(roundSet) == 0 {
		return nil
	}

	rounds := make([]RoundID, 0, len(roundSet))
	for roundID := range roundSet {
		rounds = append(rounds, roundID)
	}

	return rounds
}

// loadPendingRounds loads all pending rounds from storage and creates FSMs for
// them. These are rounds that have been finalized but not yet confirmed
// on-chain.
func (a *Actor) loadPendingRounds(ctx context.Context) error {
	rounds, err := a.cfg.RoundStore.LoadPendingRounds(ctx)
	if err != nil {
		return fmt.Errorf("failed to load pending rounds: %w", err)
	}

	// Unlock VTXOs that were locked by rounds that are no longer active.
	// This cleans up locks from rounds that were abandoned due to crashes
	// or failures before completion.
	activeRoundIDs := make([]RoundID, len(rounds))
	for i, round := range rounds {
		activeRoundIDs[i] = round.RoundID
	}

	err = a.cfg.VTXOStore.UnlockStaleVTXOs(ctx, activeRoundIDs)
	if err != nil {
		return fmt.Errorf("failed to unlock stale vtxos: %w", err)
	}

	for _, round := range rounds {
		roundFSM, err := a.loadRoundFSM(ctx, round)
		if err != nil {
			return fmt.Errorf("failed to load round %s: %w",
				round.RoundID, err)
		}

		a.rounds[round.RoundID] = roundFSM

		a.log.InfoS(ctx, "Loaded pending round from storage",
			"round_id", round.RoundID)
	}

	return nil
}

// loadRoundFSM creates a new FSM for a persisted round, starting in
// FinalizedState. This is used to restore rounds that were finalized but not
// yet confirmed on-chain. After creating the FSM, it re-subscribes to
// confirmation notifications for the round's transaction.
func (a *Actor) loadRoundFSM(ctx context.Context, round *Round) (*RoundFSM,
	error) {

	// Create the FSM starting in FinalizedState since the round was already
	// signed and persisted.
	initialState := &FinalizedState{
		ClientRegistrations: round.ClientRegistrations,
		FinalTx:             round.FinalTx,
		VTXOTrees:           round.VTXOTrees,
		ForfeitInfos:        round.ForfeitInfos,
	}

	// Use 0 as height hint for loaded rounds. This causes LND to scan from
	// the beginning which is safe but slower. When DB support is added, the
	// start height should be persisted and loaded here.
	//
	// TODO(roasbeef): Load StartHeight from round.StartHeight when DB
	// stores it.
	fsm := a.buildAndStartRoundFSM(ctx, round.RoundID, initialState, 0)

	// Re-subscribe to confirmation notifications.
	broadcastReq := &BroadcastRoundReq{
		RoundID:     round.RoundID,
		SignedTx:    round.FinalTx,
		StartHeight: 0,
	}
	if err := a.broadcastAndSubscribe(ctx, broadcastReq); err != nil {
		return nil, fmt.Errorf("failed to re-subscribe to "+
			"confirmation: %w", err)
	}

	return fsm, nil
}

// buildAndStartRoundFSM creates a new round FSM with the given initial
// state and starts it. The FSM environment is populated from the actor
// config and the provided round-specific parameters.
func (a *Actor) buildAndStartRoundFSM(ctx context.Context, roundID RoundID,
	state State, startHeight uint32) *RoundFSM {

	fsmPrefix := roundID.LogPrefix()
	fsmLogger := a.cfg.Log.UnwrapOr(btclog.Disabled).WithPrefix(fsmPrefix)

	env := &Environment{
		RoundID:                roundID,
		Log:                    fsmLogger,
		ChainParams:            a.cfg.ChainParams,
		BoardingInputLocker:    a.cfg.BoardingInputLocker,
		ChainSource:            a.cfg.ChainSource,
		HeaderVerifier:         a.cfg.HeaderVerifier,
		Terms:                  a.cfg.Terms,
		ForfeitScript:          a.cfg.ForfeitScript,
		WalletController:       a.cfg.WalletController,
		FeeEstimator:           a.cfg.FeeEstimator,
		WalletAccount:          a.cfg.WalletAccount,
		ConfTarget:             a.cfg.ConfTarget,
		MinConfs:               a.cfg.MinConfs,
		RoundStore:             a.cfg.RoundStore,
		VTXOStore:              a.cfg.VTXOStore,
		VTXOLocker:             a.cfg.VTXOLocker,
		StartHeight:            startHeight,
		DisableJoinRequestAuth: a.cfg.DisableJoinRequestAuth,
		ShouldSeal:             a.cfg.ShouldSeal,
	}

	fsmCfg := StateMachineCfg{
		InitialState:  state,
		Env:           env,
		Logger:        a.log.WithPrefix(fsmPrefix),
		ErrorReporter: newLoggingErrorReporter(fsmLogger),
	}
	fsm := protofsm.NewStateMachine(fsmCfg)
	fsm.Start(ctx)

	return &RoundFSM{
		FSM:     &fsm,
		RoundID: roundID,
	}
}

// Receive processes an actor message and returns a response. This is the main
// entry point for the actor.
func (a *Actor) Receive(ctx context.Context,
	msg ActorMsg) fn.Result[ActorResp] {

	a.log.DebugS(ctx, "Received actor message",
		slog.String("msg_type", msg.MessageType()))

	switch m := msg.(type) {
	case *JoinRoundRequest:
		return a.handleJoinRoundRequest(ctx, m)

	case *TimeoutMsg:
		return a.handleTimeout(ctx, m)

	case *RoundMsg:
		return a.handleRoundEvent(ctx, m)

	case *ConfirmationMsg:
		return a.handleConfirmation(ctx, m)

	case *TriggerBatchMsg:
		return a.handleTriggerBatch(ctx, m)

	case *GetClientRoundsRequest:
		return a.handleGetClientRounds(ctx, m)

	default:
		a.log.WarnS(ctx, "Unknown message type", nil,
			slog.String("msg_type", msg.MessageType()))

		return fn.Err[ActorResp](fmt.Errorf(
			"unknown message type: %T", m))
	}
}

// handleRoundEvent processes RoundMsg messages by forwarding the contained
// Event to the specified round's FSM.
func (a *Actor) handleRoundEvent(ctx context.Context,
	msg *RoundMsg) fn.Result[ActorResp] {

	round := a.getRound(msg.RoundID)
	if round == nil {
		return fn.Err[ActorResp](fmt.Errorf("round %s not found",
			msg.RoundID))
	}

	err := a.askEventAndProcessOutbox(
		ctx, msg.RoundID, round.FSM, msg.Event,
	)
	if err != nil {
		return fn.Err[ActorResp](fmt.Errorf(
			"FSM error processing event: %w", err))
	}

	return fn.Ok[ActorResp](nil)
}

// handleTriggerBatch processes a TriggerBatchMsg by sending a SealEvent to
// the current live round's FSM. This allows the admin to manually trigger
// a batch without waiting for the registration timeout.
func (a *Actor) handleTriggerBatch(ctx context.Context,
	_ *TriggerBatchMsg) fn.Result[ActorResp] {

	currentRound := a.getCurrentRound()
	if currentRound == nil {
		return fn.Err[ActorResp](fmt.Errorf(
			"no active round to seal"))
	}

	roundID := currentRound.RoundID

	a.log.InfoS(ctx, "Manual batch trigger received",
		slog.String("round_id", roundID.String()))

	err := a.askEventAndProcessOutbox(
		ctx, roundID, currentRound.FSM, &SealEvent{},
	)
	if err != nil {
		return fn.Err[ActorResp](fmt.Errorf(
			"FSM error processing seal: %w", err))
	}

	return fn.Ok[ActorResp](&TriggerBatchResp{
		RoundID: roundID,
	})
}

// getRound returns the round FSM for the given round ID, or nil if not found.
func (a *Actor) getRound(roundID RoundID) *RoundFSM {
	return a.rounds[roundID]
}

// handleGetClientRounds processes a GetClientRoundsRequest and returns the list
// of rounds the client is participating in.
func (a *Actor) handleGetClientRounds(_ context.Context,
	msg *GetClientRoundsRequest) fn.Result[ActorResp] {

	rounds := a.getClientRounds(msg.ClientID)
	return fn.Ok[ActorResp](&GetClientRoundsResponse{
		RoundIDs: rounds,
	})
}

// askEventAndProcessOutbox sends an event to the FSM and processes any emitted
// outbox messages. This consolidates a common pattern throughout the actor
// where FSM events trigger outbox processing.
func (a *Actor) askEventAndProcessOutbox(ctx context.Context,
	roundID RoundID, fsm *StateMachine, event Event) error {

	outbox, err := a.askAndDrive(ctx, roundID, fsm, event)
	if err != nil {
		return err
	}

	if len(outbox) > 0 {
		if err := a.processOutbox(ctx, outbox); err != nil {
			return fmt.Errorf("failed to process outbox: %w", err)
		}
	}

	return nil
}

// askAndDrive sends a single event to the FSM and returns the resulting
// outbox messages for processOutbox routing.
func (a *Actor) askAndDrive(ctx context.Context, _ RoundID,
	fsm *StateMachine, event Event) ([]OutboxEvent, error) {

	if fsm == nil {
		return nil, fmt.Errorf("fsm must be provided")
	}

	fut := fsm.AskEvent(ctx, event)
	result := fut.Await(ctx)

	return result.Unpack()
}

// processOutbox processes messages emitted by the FSM via Outbox and routes
// them to the appropriate destination.
func (a *Actor) processOutbox(ctx context.Context, outbox []OutboxEvent) error {
	var outboxErrs []error

	appendOutboxErr := func(action string, msg OutboxEvent, err error) {
		if err != nil {
			wrapped := fmt.Errorf("%s: %w", action, err)
			logFields := []any{
				"outbox_event_type", fmt.Sprintf("%T", msg),
				"action", action,
			}
			roundID := outboxRoundID(msg)
			if roundID != "" {
				logFields = append(
					logFields, "round_id", roundID,
				)
			}
			a.log.WarnS(
				ctx, "Outbox dispatch failed", wrapped, logFields...,
			)
			outboxErrs = append(outboxErrs, wrapped)
		}
	}

	for _, msg := range outbox {
		switch m := msg.(type) {
		// Check if this message should be sent to client(s). All
		// client-bound messages implement the ClientMessage interface.
		case clientconn.ClientMessage:
			// Track client join when a ClientSuccessResp is sent.
			if successResp, ok := m.(*ClientSuccessResp); ok {
				a.trackClientJoin(
					ctx, successResp.Client,
					successResp.RoundID,
				)
			}

			sendReq := &clientconn.SendServerEventRequest{
				Message: m,
			}
			err := a.cfg.ClientsConn.Tell(ctx, sendReq)
			if err != nil {
				appendOutboxErr("send client event", msg, err)
			}

		case *StartTimeoutReq:
			// Notify metrics actor that a new phase is starting.
			a.tellMetrics(ctx, &metrics.PhaseStartedMsg{
				RoundID: m.RoundID.String(),
				Phase:   string(m.Phase),
			})

			// Create composite timeout ID that includes the phase.
			// This allows us to identify which state scheduled the
			// timeout when it expires.
			compositeID := makeTimeoutID(m.RoundID, m.Phase)

			// MapTimeoutExpired creates a callback that converts
			// timeout.ExpiredMsg to our TimeoutMsg. The phase is
			// encoded in the composite ID and will be parsed by
			// handleTimeout.
			callbackRef := timeout.MapTimeoutExpired(
				a.cfg.SelfRef,
				func(expired timeout.ExpiredMsg) ActorMsg {
					return &TimeoutMsg{
						TimeoutID: expired.ID,
					}
				},
			)

			// Send schedule request to timeout actor with
			// composite ID.
			req := &timeout.ScheduleTimeoutRequest{
				ID:       compositeID,
				Duration: m.Duration,
				Callback: callbackRef,
			}
			err := a.cfg.TimeoutActor.Tell(ctx, req)
			if err != nil {
				appendOutboxErr("schedule timeout", msg, err)
			}

		case *CancelTimeoutReq:
			// Phase completed before timeout fired.
			a.tellMetrics(ctx, &metrics.PhaseEndedMsg{
				RoundID: m.RoundID.String(),
				Phase:   string(m.Phase),
			})

			// Cancel timeout using composite ID constructed from
			// round ID and phase.
			compositeID := makeTimeoutID(m.RoundID, m.Phase)
			cancelReq := &timeout.CancelTimeoutRequest{
				ID: compositeID,
			}
			err := a.cfg.TimeoutActor.Tell(ctx, cancelReq)
			if err != nil {
				appendOutboxErr("cancel timeout", msg, err)
			}

		case *RoundSealedReq:
			// Notify metrics actor that registration closed.
			a.tellMetrics(ctx, &metrics.RoundSealedMsg{
				RoundID: m.SealedRoundID.String(),
			})

			// Round has been sealed - create a new round for new
			// registrations.
			newRound, err := a.newRoundFSM(ctx)
			if err != nil {
				return fmt.Errorf(
					"failed to create new round: %w", err)
			}

			a.rounds[newRound.RoundID] = newRound
			a.currentRoundID = newRound.RoundID

			a.log.InfoS(ctx, "Created new round after sealing",
				"sealed_round", m.SealedRoundID,
				"new_round", newRound.RoundID)

		case *RoundFailedReq:
			// Round has failed - clean up and create a new round if
			// this was the current round.
			a.log.ErrorS(ctx, "Round failed",
				fmt.Errorf("round failed: %s", m.Reason),
				"round_id", m.FailedRoundID,
				"reason", m.Reason)

			// Notify metrics actor of round failure.
			a.tellMetrics(ctx, &metrics.RoundCompletedMsg{
				RoundID: m.FailedRoundID.String(),
				Status:  "failed",
			})

			// Untrack all clients from this failed round.
			a.untrackRound(m.FailedRoundID)

			// Remove the failed round from tracking.
			delete(a.rounds, m.FailedRoundID)

			// If this was the current round, create a new one.
			if a.currentRoundID == m.FailedRoundID {
				newRound, err := a.newRoundFSM(ctx)
				if err != nil {
					return fmt.Errorf("failed to create "+
						"new round after failure: %w",
						err)
				}

				a.rounds[newRound.RoundID] = newRound
				a.currentRoundID = newRound.RoundID

				a.log.InfoS(ctx, "Created new round after "+
					"failure",
					"failed_round", m.FailedRoundID,
					"new_round", newRound.RoundID)
			}

		case *BroadcastRoundReq:
			// Broadcast the transaction and subscribe to
			// confirmation.
			//
			// TODO(elle): Handle broadcast failures - if broadcast
			// fails, we should retry, fee bump or transition to a
			// failed state.
			err := a.broadcastAndSubscribe(ctx, m)
			if err != nil {
				return fmt.Errorf("failed to broadcast "+
					"round %s: %w", m.RoundID, err)
			}

		default:
			// Unknown outbox message. This could be an internal FSM
			// event that doesn't need routing, so we ignore it.
			_ = m
		}
	}

	if len(outboxErrs) > 0 {
		// TODO(#91): Use durable outbox retries by message type
		// (critical vs best-effort). Joined enqueue errors are only a
		// short-term surface.
		return errors.Join(outboxErrs...)
	}

	return nil
}

// outboxRoundID extracts the round identifier from an outbox event for
// structured logging. Returns empty string if the event has no round context.
func outboxRoundID(msg OutboxEvent) string {
	switch m := msg.(type) {
	case *ClientSuccessResp:
		return m.RoundID.String()
	case *ClientAwaitingInputSigsResp:
		return m.RoundID.String()
	case *ClientVTXOAggNonces:
		return m.RoundID.String()
	case *ClientVTXOAggSigs:
		return m.RoundID.String()
	case *ClientBatchInfo:
		return m.RoundID.String()
	case *ClientRoundFailedResp:
		return m.RoundID.String()
	case *RoundSealedReq:
		return m.SealedRoundID.String()
	case *StartTimeoutReq:
		return m.RoundID.String()
	case *CancelTimeoutReq:
		return m.RoundID.String()
	case *RoundFailedReq:
		return m.FailedRoundID.String()
	case *BroadcastRoundReq:
		return m.RoundID.String()
	default:
		return ""
	}
}

// newRoundFSM creates and starts a new round FSM instance with a unique round
// ID and returns it. It queries the chain source for the current best height
// to use as the height hint for confirmation tracking.
func (a *Actor) newRoundFSM(ctx context.Context) (*RoundFSM, error) {
	roundID, err := NewRoundID()
	if err != nil {
		return nil, fmt.Errorf("unable to generate round ID: %w", err)
	}

	// Query the current best height to use as the start height for this
	// round. This height will be used as the height hint when subscribing
	// to confirmation notifications later.
	var startHeight uint32
	if a.cfg.ChainSourceActor != nil {
		heightFuture := a.cfg.ChainSourceActor.Ask(
			ctx, &chainsource.BestHeightRequest{},
		)
		heightResult := heightFuture.Await(ctx)
		heightResp, err := heightResult.Unpack()
		if err != nil {
			return nil, fmt.Errorf("get best height: %w", err)
		}
		bhr, ok := heightResp.(*chainsource.BestHeightResponse)
		if !ok {
			return nil, fmt.Errorf("unexpected height resp type")
		}
		bestHeightResp := bhr
		startHeight = uint32(bestHeightResp.Height)
	}

	// Notify metrics actor of new round creation.
	a.tellMetrics(ctx, &metrics.RoundCreatedMsg{
		RoundID: roundID.String(),
	})

	return a.buildAndStartRoundFSM(
		ctx, roundID, &CreatedState{}, startHeight,
	), nil
}

// handleJoinRoundRequest processes a JoinRoundRequest message by forwarding it
// to the current round FSM.
func (a *Actor) handleJoinRoundRequest(ctx context.Context,
	msg *JoinRoundRequest) fn.Result[ActorResp] {

	currentRound := a.getCurrentRound()
	if currentRound == nil {
		return fn.Err[ActorResp](fmt.Errorf(
			"no current round available"))
	}

	// Query current best height so join-auth freshness checks can evaluate
	// against the latest chain tip.
	var currentBlockHeight uint32
	if a.cfg.ChainSourceActor != nil {
		heightFuture := a.cfg.ChainSourceActor.Ask(
			ctx, &chainsource.BestHeightRequest{},
		)
		heightResult := heightFuture.Await(ctx)
		heightResp, err := heightResult.Unpack()
		if err != nil {
			a.log.WarnS(ctx, "Failed to query best height for "+
				"join auth validation", err)
		} else {
			bestHeightResp, ok := heightResp.(*chainsource.
				BestHeightResponse)
			if !ok {
				a.log.WarnS(ctx,
					"Unexpected best height "+
						"response type", nil,
					slog.String("type",
						fmt.Sprintf("%T",
							heightResp)),
				)
			} else {
				currentBlockHeight = uint32(
					bestHeightResp.Height,
				)
			}
		}
	}

	// Convert the actor message to an FSM event.
	joinEvent := &ClientJoinRequestEvent{
		ClientID:           msg.ClientID,
		Request:            msg.Request,
		CurrentBlockHeight: currentBlockHeight,
	}

	err := a.askEventAndProcessOutbox(
		ctx, currentRound.RoundID, currentRound.FSM, joinEvent,
	)
	if err != nil {
		return fn.Err[ActorResp](fmt.Errorf(
			"FSM error processing join request: %w", err))
	}

	return fn.Ok[ActorResp](nil)
}

// handleTimeout processes a timeout message by parsing the composite timeout
// ID to extract the round ID and phase, then sending the appropriate
// phase-specific timeout event to the round's FSM.
func (a *Actor) handleTimeout(ctx context.Context,
	msg *TimeoutMsg) fn.Result[ActorResp] {

	// Parse the composite timeout ID to get round ID and phase.
	roundID, phase, err := parseTimeoutID(msg.TimeoutID)
	if err != nil {
		a.log.WarnS(ctx, "Failed to parse timeout ID", err,
			"timeout_id", string(msg.TimeoutID))

		return fn.Ok[ActorResp](nil)
	}

	// Find the round for this timeout.
	round := a.getRound(roundID)
	if round == nil {
		// Stale timeout for unknown round, ignore.
		a.log.DebugS(ctx, "Ignoring timeout for unknown round",
			"round_id", roundID,
			"phase", phase)

		return fn.Ok[ActorResp](nil)
	}

	// Notify metrics actor that a phase ended by timeout.
	if phase == TimeoutPhaseRegistration ||
		phase == TimeoutPhaseInputSigs ||
		phase == TimeoutPhaseVTXONonces {

		a.tellMetrics(ctx, &metrics.PhaseEndedMsg{
			RoundID:  roundID.String(),
			Phase:    string(phase),
			TimedOut: true,
		})
	}

	// Also notify for registration timeout specifically so the
	// metrics actor can observe it with "timeout" status.
	if phase == TimeoutPhaseRegistration {
		a.tellMetrics(ctx, &metrics.RoundSealedMsg{
			RoundID:  roundID.String(),
			TimedOut: true,
		})
	}

	// Create the appropriate phase-specific timeout event.
	var timeoutEvent Event
	switch phase {
	case TimeoutPhaseRegistration:
		timeoutEvent = &RegistrationTimeoutEvent{}

	case TimeoutPhaseInputSigs:
		timeoutEvent = &InputSignaturesTimeoutEvent{}

	case TimeoutPhaseVTXONonces:
		timeoutEvent = &VTXONoncesTimeoutEvent{}

	case TimeoutPhaseVTXOSignatures:
		timeoutEvent = &VTXOSignaturesTimeoutEvent{}

	default:
		// Unknown phase - log warning and ignore.
		a.log.WarnS(ctx, "Ignoring timeout with unknown phase", nil,
			"round_id", roundID,
			"phase", phase)

		return fn.Ok[ActorResp](nil)
	}

	// Send the phase-specific timeout event to the FSM.
	err = a.askEventAndProcessOutbox(
		ctx, roundID, round.FSM, timeoutEvent,
	)
	if err != nil {
		return fn.Err[ActorResp](fmt.Errorf(
			"FSM error processing %s timeout: %w", phase, err))
	}

	return fn.Ok[ActorResp](nil)
}

// handleConfirmation processes a ConfirmationMsg by forwarding a
// TransactionConfirmedEvent to the appropriate round's FSM.
func (a *Actor) handleConfirmation(ctx context.Context,
	msg *ConfirmationMsg) fn.Result[ActorResp] {

	// Find the round for this confirmation.
	round := a.getRound(msg.RoundID)
	if round == nil {
		// Round no longer tracked - this can happen if the round was
		// cleaned up before the confirmation arrived.
		a.log.WarnS(ctx, "Ignoring confirmation for unknown round", nil,
			"round_id", msg.RoundID)

		return fn.Ok[ActorResp](nil)
	}

	// Forward the confirmation event to the FSM.
	confirmedEvent := &TransactionConfirmedEvent{
		BlockHeight: msg.BlockHeight,
		BlockHash:   msg.BlockHash,
		NumConfs:    msg.NumConfs,
	}

	err := a.askEventAndProcessOutbox(
		ctx, msg.RoundID, round.FSM, confirmedEvent,
	)
	if err != nil {
		return fn.Err[ActorResp](fmt.Errorf(
			"FSM error processing confirmation: %w", err))
	}

	// Get the confirmed state to access VTXOTrees for batch watcher
	// registration.
	currentState, err := round.FSM.CurrentState()
	if err != nil {
		a.log.WarnS(ctx, "Failed to get current state after confirmation",
			err, "round_id", msg.RoundID)
	} else if cs, ok := currentState.(*ConfirmedState); ok {
		// Register VTXO trees with the batch watcher for on-chain
		// monitoring.
		a.registerBatchesWithWatcher(
			ctx, msg.RoundID, msg.BlockHeight, cs.VTXOTrees,
		)
	}

	// Untrack all clients from this completed round.
	a.untrackRound(msg.RoundID)

	// Notify metrics actor of round confirmation.
	a.tellMetrics(ctx, &metrics.RoundCompletedMsg{
		RoundID:     msg.RoundID.String(),
		Status:      "confirmed",
		BlockHeight: uint32(msg.BlockHeight),
	})

	a.log.InfoS(ctx, "Round transaction confirmed",
		"round_id", msg.RoundID,
		"block_height", msg.BlockHeight,
		"num_confs", msg.NumConfs)

	return fn.Ok[ActorResp](nil)
}

// broadcastAndSubscribe broadcasts the round's signed transaction and
// subscribes to confirmation notifications.
func (a *Actor) broadcastAndSubscribe(ctx context.Context,
	req *BroadcastRoundReq) error {

	// Skip if ChainSourceActor is not configured (e.g., in tests).
	if a.cfg.ChainSourceActor == nil {
		a.log.DebugS(ctx, "Skipping broadcast - no chain source actor",
			"round_id", req.RoundID)

		return nil
	}

	// Broadcast the signed transaction to the network.
	txHash := req.SignedTx.TxHash()
	broadcastReq := &chainsource.BroadcastTxRequest{
		Tx:    req.SignedTx,
		Label: fmt.Sprintf("round-%s", req.RoundID),
	}

	broadcastFuture := a.cfg.ChainSourceActor.Ask(ctx, broadcastReq)
	broadcastResult := broadcastFuture.Await(ctx)
	if _, err := broadcastResult.Unpack(); err != nil {
		return fmt.Errorf("broadcast failed: %w", err)
	}

	a.log.InfoS(ctx, "Broadcast round transaction",
		"round_id", req.RoundID,
		"txid", txHash.String())

	// Subscribe to confirmation using actor mode. We create a mapped
	// reference that transforms a ConfirmationEvent to a ConfirmationMsg.
	// We use Tell (fire-and-forget) since we handle the confirmation
	// asynchronously via ConfirmationMsg. The height hint comes from
	// req.StartHeight which was captured when the round was created.
	callbackRef := chainsource.MapConfirmationEvent(
		a.cfg.SelfRef,
		func(event chainsource.ConfirmationEvent) ActorMsg {
			return &ConfirmationMsg{
				RoundID:     req.RoundID,
				BlockHeight: event.BlockHeight,
				BlockHash:   event.BlockHash,
				NumConfs:    event.NumConfs,
			}
		},
	)

	// Use the round ID as the caller ID for deterministic cancellation.
	// LND requires a pkScript for confirmation tracking - use the first
	// output (the batch output) since that's what we're watching.
	var pkScript []byte
	if len(req.SignedTx.TxOut) > 0 {
		pkScript = req.SignedTx.TxOut[0].PkScript
	}

	confReq := &chainsource.RegisterConfRequest{
		CallerID:    req.RoundID.String(),
		Txid:        &txHash,
		PkScript:    pkScript,
		TargetConfs: a.cfg.ConfirmationTarget,
		HeightHint:  req.StartHeight,
		NotifyActor: fn.Some(callbackRef),
	}

	// Use a background context for the confirmation subscription since it
	// needs to outlive the current request. The request context `ctx` would
	// be cancelled when this function returns, which would cancel the
	// ChainSourceActor's internal goroutines for monitoring confirmations.
	err := a.cfg.ChainSourceActor.Tell(context.Background(), confReq)
	if err != nil {
		return fmt.Errorf("subscribe confirmation: %w", err)
	}

	a.log.DebugS(ctx, "Subscribed to transaction confirmation",
		"round_id", req.RoundID,
		"txid", txHash.String(),
		"target_confs", a.cfg.ConfirmationTarget)

	return nil
}

// registerBatchesWithWatcher registers all VTXO trees from a confirmed round
// with the BatchWatcher for on-chain monitoring.
func (a *Actor) registerBatchesWithWatcher(ctx context.Context, roundID RoundID,
	blockHeight int32, vtxoTrees map[int]*tree.Tree) {

	// Type aliases for readability.
	type bwMsg = batchwatcher.BatchWatcherMsg
	type bwResp = batchwatcher.BatchWatcherResp

	a.cfg.BatchWatcher.WhenSome(func(ref actor.ActorRef[bwMsg, bwResp]) {
		// Calculate expiry height based on confirmation height and
		// sweep delay from terms.
		expiryHeight := uint32(blockHeight) + a.cfg.Terms.SweepDelay

		// Register each VTXO tree with the batch watcher.
		for outputIdx, vtxoTree := range vtxoTrees {
			if vtxoTree == nil {
				continue
			}

			// Create a deterministic BatchID from RoundID and
			// output index using UUID v5. This ensures the BatchID
			// encodes both values and is reproducible.
			batchIDName := fmt.Sprintf("%s-%d", roundID, outputIdx)
			batchID := batchwatcher.BatchID(uuid.NewSHA1(
				uuid.UUID(roundID), []byte(batchIDName),
			))

			req := &batchwatcher.RegisterBatchRequest{
				BatchID:            batchID,
				Tree:               vtxoTree,
				ConfirmationHeight: uint32(blockHeight),
				ExpiryHeight:       expiryHeight,
			}

			// Send registration request using fire-and-forget since
			// we don't need to wait for acknowledgment.
			if err := ref.Tell(ctx, req); err != nil {
				a.log.WarnS(ctx, "Failed to register batch with watcher",
					err,
					"round_id", roundID,
					"batch_id", batchID,
					"output_idx", outputIdx,
					slog.Uint64("expiry_height",
						uint64(expiryHeight)),
				)

				continue
			}

			a.log.InfoS(ctx, "Registered batch with watcher",
				"round_id", roundID,
				"batch_id", batchID,
				"output_idx", outputIdx,
				slog.Uint64("expiry_height", uint64(expiryHeight)))
		}
	})
}
