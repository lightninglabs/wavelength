package rounds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/timeout"
	"github.com/lightninglabs/darepo/batch"
	"github.com/lightninglabs/darepo/batchwatcher"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightninglabs/darepo/fees"
	"github.com/lightninglabs/darepo/ledger"
	"github.com/lightninglabs/darepo/metrics"
	"github.com/lightninglabs/darepo/vtxo"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"golang.org/x/time/rate"
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

	// SkipQuoteHandshake bypasses the seal-time quote fan-out when
	// a SealEvent fires, transitioning directly to
	// BatchBuildingState. Intended only for pre-#270 unit tests
	// that drive the FSM without exercising the quote handshake.
	SkipQuoteHandshake bool

	// ShouldSeal is an optional predicate evaluated after each
	// successful client join. When it returns true the round is
	// sealed immediately without waiting for the registration
	// timeout. A nil predicate is equivalent to "never seal early".
	ShouldSeal SealPredicate

	// RoundTickInterval is the cadence at which a periodic
	// TickEvent is delivered to each round's FSM. The actor
	// schedules a recurring tick on round creation when this is
	// non-zero. Zero disables periodic ticks (event-driven seals
	// only).
	RoundTickInterval time.Duration

	// MetricsActor is an optional reference to the centralized
	// metrics actor. When set, the rounds actor sends metric
	// events here instead of calling Prometheus directly.
	MetricsActor fn.Option[actor.TellOnlyRef[metrics.Msg]]

	// VTXOEventPublisher publishes VTXO lifecycle events to the
	// indexer after round confirmation. When nil, events are not
	// published.
	VTXOEventPublisher VTXOEventPublisher

	// FeeCalculator computes dynamic fees based on the current
	// fee schedule and treasury utilization. When nil, the
	// flat MinOperatorFee from Terms is used instead.
	FeeCalculator *fees.Calculator

	// TreasuryTracker provides current utilization for
	// congestion pricing. When nil, utilization is assumed to
	// be zero.
	TreasuryTracker *fees.TreasuryTracker

	// LedgerRef is the actor reference for the ledger
	// accounting actor. When non-nil, round lifecycle events
	// are forwarded via fire-and-forget Tell.
	LedgerRef actor.TellOnlyRef[ledger.LedgerMsg]

	// JoinRequestRate is the steady-state allowance for per-client
	// JoinRoundRequest deliveries to the rounds actor. The actor
	// installs a token-bucket limiter (golang.org/x/time/rate) keyed by
	// ClientID and silently drops requests once the bucket is empty.
	// The legitimate restart-replay flow needs one initial join plus
	// one replay per round, so a generous burst with a slow refill
	// covers honest clients while neutering re-register floods. Zero
	// uses the package default (DefaultJoinRequestRate).
	JoinRequestRate rate.Limit

	// JoinRequestBurst is the bucket size for the per-client
	// JoinRoundRequest limiter. Zero uses the package default
	// (DefaultJoinRequestBurst).
	JoinRequestBurst int
}

// DefaultJoinRequestRate is the steady-state replenish rate for the
// per-client JoinRoundRequest limiter (one token every two seconds).
// A healthy client sends one initial join and at most one replay
// per round, so this is generous in absolute terms while still
// dropping a tight flood from a misbehaving or compromised peer.
var DefaultJoinRequestRate = rate.Every(2 * time.Second)

// DefaultJoinRequestBurst is the default bucket size for the
// per-client JoinRoundRequest limiter. Burst > 1 absorbs honest retry
// under transient network failures without dropping the legitimate
// restart-replay shape.
const DefaultJoinRequestBurst = 3

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

	// joinLimiters are per-client token-bucket limiters guarding the
	// JoinRoundRequest path against re-register floods. Entries are
	// created on first request from a client and reaped when the
	// client is no longer in any tracked round (see untrackRound).
	// Access is serialized through the actor's Receive loop, so no
	// additional locking is required.
	joinLimiters map[clientconn.ClientID]*rate.Limiter

	// joinLimit is the rate.Limit used to construct new entries in
	// joinLimiters. Resolved once at actor construction from the
	// ActorConfig.
	joinLimit rate.Limit

	// joinBurst is the burst size used to construct new entries in
	// joinLimiters. Resolved once at actor construction from the
	// ActorConfig.
	joinBurst int

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

	joinLimit := cfg.JoinRequestRate
	if joinLimit == 0 {
		joinLimit = DefaultJoinRequestRate
	}
	joinBurst := cfg.JoinRequestBurst
	if joinBurst == 0 {
		joinBurst = DefaultJoinRequestBurst
	}

	return &Actor{
		cfg:          cfg,
		log:          cfg.Log.UnwrapOr(btclog.Disabled),
		rounds:       make(map[RoundID]*RoundFSM),
		clientRounds: clientRounds,
		joinLimiters: make(map[clientconn.ClientID]*rate.Limiter),
		joinLimit:    joinLimit,
		joinBurst:    joinBurst,
	}
}

// Start initializes the actor. It loads any pending rounds from storage that
// need to be tracked until confirmation, then creates a new live round FSM to
// accept registrations.
func (a *Actor) Start(ctx context.Context) error {
	if a.cfg.VTXOLocker == nil {
		return errors.New("vtxo locker not configured")
	}

	// Validate that the UTXO lock duration is long enough to cover
	// the worst-case round lifetime. This prevents silent lease
	// expiry mid-round which could lead to double-spend attempts.
	if err := a.cfg.Terms.ValidateFundPsbtLockDuration(); err != nil {
		return fmt.Errorf("invalid terms: %w", err)
	}

	// Under the #270 seal-time fee handshake the fee calculator is
	// the sole authority for operator fees: there is no flat-fee
	// fallback. All three of FeeEstimator, FeeCalculator, and
	// TreasuryTracker must be wired or the seal-time builder would
	// nil-deref on the first round. LedgerRef is required for fee
	// booking at round confirmation. Fail the boot here instead of
	// letting the first join request crash or silently admit a
	// round whose accounting is never persisted.
	if a.cfg.FeeEstimator == nil {
		return fmt.Errorf("FeeEstimator must be configured")
	}
	if a.cfg.FeeCalculator == nil {
		return fmt.Errorf("FeeCalculator must be configured")
	}
	if a.cfg.TreasuryTracker == nil {
		return fmt.Errorf("TreasuryTracker must be configured")
	}
	if a.cfg.LedgerRef == nil {
		return fmt.Errorf("LedgerRef must be configured")
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
		// Clean up empty client entries. Also drop the client's
		// join-request limiter — it serves no purpose once the
		// client has no active rounds, and keeping it would let
		// joinLimiters grow without bound on long-lived operators.
		if len(a.clientRounds[clientID]) == 0 {
			delete(a.clientRounds, clientID)
			delete(a.joinLimiters, clientID)
		}
	}
}

// allowJoinRequest returns true if the per-client JoinRoundRequest
// limiter has a token available for clientID, consuming the token as
// a side effect. The limiter neutralizes re-register floods (the new
// replacement semantic at IntentCollectingState ClientJoinIntentEvent
// runs a full validateJoinRequestForAdmission per request, including
// BIP-322 script-engine execution and chain RPC, so unbounded retries
// would amplify into a per-client DoS). The legitimate
// restart-replay shape needs exactly one initial join and at most
// one replay per round, well under the default burst.
func (a *Actor) allowJoinRequest(clientID clientconn.ClientID) bool {
	limiter, ok := a.joinLimiters[clientID]
	if !ok {
		limiter = rate.NewLimiter(a.joinLimit, a.joinBurst)
		a.joinLimiters[clientID] = limiter
	}

	return limiter.Allow()
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
			"round_id", round.RoundID,
		)
	}

	return nil
}

// restoreSweepKey rebuilds the operator's sweep key descriptor from a
// persisted round. New rows carry both the compressed pubkey and the
// LND key locator, yielding a complete descriptor. Pre-migration rows
// carry only the pubkey: we return that with a zero KeyLocator so
// downstream consumers can distinguish "locator unknown" from a fresh
// descriptor and refuse to silently sign with whatever key the actor is
// currently configured with (see batchsweeper.trySweep). A nil
// SweepKey on the round (e.g. very old test fixtures) yields the zero
// descriptor.
func restoreSweepKey(round *Round) keychain.KeyDescriptor {
	if round.SweepKey == nil {
		return keychain.KeyDescriptor{}
	}

	desc := keychain.KeyDescriptor{
		PubKey: round.SweepKey,
	}
	if round.SweepKeyLocator != nil {
		desc.KeyLocator = *round.SweepKeyLocator
	}

	return desc
}

// loadRoundFSM creates a new FSM for a persisted round, starting in
// FinalizedState. This is used to restore rounds that were finalized but not
// yet confirmed on-chain. After creating the FSM, it re-subscribes to
// confirmation notifications for the round's transaction.
func (a *Actor) loadRoundFSM(ctx context.Context, round *Round) (*RoundFSM,
	error) {

	// Create the FSM starting in FinalizedState since the round was
	// already signed and persisted. ChangeOutputIdx and
	// ConnectorOutputIndices are restored from the store so the
	// ledger classifier can short-circuit external_* booking for
	// round-attributable outputs even after a mid-flight restart.
	// Pre-migration rows read back ChangeOutputIdx=-1 via the
	// column default, matching the prior fail-open behavior.
	// MiningFeeSat stays zero on reload because the PSBT is not
	// persisted -- the ledger handler skips the mining_fees leg
	// cleanly on zero, so accounting is at worst incomplete (fee
	// expense not booked) rather than incorrect.
	//
	// SweepKey is rebuilt from the persisted pubkey + locator (when
	// present) so a confirmation arriving after restart wires the
	// batch watcher with the historical descriptor. Pre-migration
	// rows carry only the pubkey; the locator stays zero and
	// downstream sweep refuses to silently fall back to a rotated
	// configured key (see batchsweeper.trySweep).
	sweepKey := restoreSweepKey(round)
	initialState := &FinalizedState{
		ClientRegistrations:    round.ClientRegistrations,
		FinalTx:                round.FinalTx,
		VTXOTrees:              round.VTXOTrees,
		ForfeitInfos:           round.ForfeitInfos,
		ChangeOutputIdx:        round.ChangeOutputIdx,
		ConnectorOutputIndices: round.ConnectorOutputIndices,
		SweepKey:               sweepKey,
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
		SkipQuoteHandshake:     a.cfg.SkipQuoteHandshake,
		ShouldSeal:             a.cfg.ShouldSeal,
		FeeCalculator:          a.cfg.FeeCalculator,
		TreasuryTracker:        a.cfg.TreasuryTracker,
		LedgerRef:              a.cfg.LedgerRef,
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

// scheduleRoundTick installs a recurring TickEvent for roundID. Each fire
// arrives back at the actor as a TickFiredMsg via MapTickFired and is
// translated into a TickEvent injection by handleTickFired.
func (a *Actor) scheduleRoundTick(ctx context.Context, roundID RoundID) {
	tickID := makeTimeoutID(roundID, TimeoutPhaseTick)

	callbackRef := timeout.MapTickFired(
		a.cfg.SelfRef,
		func(fired timeout.TickFiredMsg) ActorMsg {
			return &TickFiredMsg{
				TickID: fired.ID,
			}
		},
	)

	req := &timeout.ScheduleRecurringTickRequest{
		ID:       tickID,
		Interval: a.cfg.RoundTickInterval,
		Callback: callbackRef,
	}
	if err := a.cfg.TimeoutActor.Tell(ctx, req); err != nil {
		a.log.WarnS(ctx, "Failed to schedule round tick", err,
			"round_id", roundID)
	}
}

// cancelRoundTick cancels the recurring tick for roundID via the
// timeout actor. The timeout actor treats Cancel as a no-op for
// unscheduled IDs, so this is safe to call whether or not a tick was
// ever scheduled or has already been cancelled.
func (a *Actor) cancelRoundTick(ctx context.Context, roundID RoundID) error {
	return a.cfg.TimeoutActor.Tell(ctx, &timeout.CancelTimeoutRequest{
		ID: makeTimeoutID(roundID, TimeoutPhaseTick),
	})
}

// Receive processes an actor message and returns a response. This is the main
// entry point for the actor.
func (a *Actor) Receive(ctx context.Context,
	msg ActorMsg) fn.Result[ActorResp] {

	a.log.DebugS(ctx, "Received actor message",
		slog.String("msg_type", msg.MessageType()),
	)

	switch m := msg.(type) {
	case *JoinRoundRequest:
		return a.handleJoinRoundRequest(ctx, m)

	case *TimeoutMsg:
		return a.handleTimeout(ctx, m)

	case *TickFiredMsg:
		return a.handleTickFired(ctx, m)

	case *RoundMsg:
		return a.handleRoundEvent(ctx, m)

	case *ConfirmationMsg:
		return a.handleConfirmation(ctx, m)

	case *TriggerBatchMsg:
		return a.handleTriggerBatch(ctx, m)

	case *GetClientRoundsRequest:
		return a.handleGetClientRounds(ctx, m)

	case *GetRoundStatusReq:
		return a.handleGetRoundStatus(ctx, m)

	default:
		a.log.WarnS(ctx, "Unknown message type",
			nil,
			slog.String("msg_type", msg.MessageType()),
		)

		return fn.Err[ActorResp](
			fmt.Errorf("unknown message type: %T", m),
		)
	}
}

// handleRoundEvent processes RoundMsg messages by forwarding the contained
// Event to the specified round's FSM.
func (a *Actor) handleRoundEvent(ctx context.Context,
	msg *RoundMsg) fn.Result[ActorResp] {

	round := a.getRound(msg.RoundID)
	if round == nil {
		return fn.Err[ActorResp](
			fmt.Errorf("round %s not found", msg.RoundID),
		)
	}

	err := a.askEventAndProcessOutbox(
		ctx, msg.RoundID, round.FSM, msg.Event,
	)
	if err != nil {
		return fn.Err[ActorResp](
			fmt.Errorf("FSM error processing event: %w", err),
		)
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
		return fn.Err[ActorResp](fmt.Errorf("no active round to seal"))
	}

	roundID := currentRound.RoundID

	state, err := currentRound.FSM.CurrentState()
	if err != nil {
		return fn.Err[ActorResp](
			fmt.Errorf("get current round state: %w", err),
		)
	}

	if err := ensureTriggerableRound(roundID, state); err != nil {
		return fn.Err[ActorResp](err)
	}

	a.log.InfoS(ctx, "Manual batch trigger received",
		slog.String("round_id", roundID.String()),
	)

	err = a.askEventAndProcessOutbox(
		ctx, roundID, currentRound.FSM, &SealEvent{},
	)
	if err != nil {
		return fn.Err[ActorResp](
			fmt.Errorf("FSM error processing seal: %w", err),
		)
	}

	return fn.Ok[ActorResp](&TriggerBatchResp{
		RoundID: roundID,
	})
}

type roundState = protofsm.State[Event, OutboxEvent, *Environment]

// ensureTriggerableRound rejects manual batch triggers for live rounds that
// cannot be sealed. In particular, a Created round has no admitted clients, so
// injecting SealEvent would be ignored by the FSM while returning success to
// the caller.
func ensureTriggerableRound(roundID RoundID, state roundState) error {
	switch state.(type) {
	case *CreatedState:
		return fmt.Errorf("cannot trigger batch for round %s: no "+
			"registered clients", roundID)

	case *IntentCollectingState:
		return nil

	default:
		return fmt.Errorf("internal error: current round %s is in "+
			"unexpected state %T", roundID, state)
	}
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

// handleGetRoundStatus processes a GetRoundStatusReq by snapshotting
// the requested round's current FSM state and filling in a
// GetRoundStatusResp. The response is a read-only projection —
// populating each field is safe from any state (zero-valued counts
// fall through for states that don't track a given metric). If the
// round is not live (either never created or already finalized and
// cleaned up), RoundNotFound is set and all other fields are left
// at their zero values.
func (a *Actor) handleGetRoundStatus(_ context.Context,
	msg *GetRoundStatusReq) fn.Result[ActorResp] {

	round := a.getRound(msg.RoundID)
	if round == nil {
		return fn.Ok[ActorResp](&GetRoundStatusResp{
			RoundID:       msg.RoundID,
			RoundNotFound: true,
		})
	}

	state, err := round.FSM.CurrentState()
	if err != nil {
		return fn.Ok[ActorResp](&GetRoundStatusResp{
			RoundID:       msg.RoundID,
			RoundNotFound: true,
		})
	}

	resp := &GetRoundStatusResp{
		RoundID:   msg.RoundID,
		StateName: state.String(),
	}

	switch s := state.(type) {
	case *IntentCollectingState:
		resp.IntentCount = uint32(len(s.ClientRegistrations))

	case *QuoteSentState:
		resp.IntentCount = uint32(len(s.ClientRegistrations))
		resp.QuotesSent = uint32(len(s.Quotes))
		for _, st := range s.Status {
			switch st {
			case QuoteAccepted:
				resp.QuotesAccepted++

			case QuoteRejected:
				resp.QuotesRejected++

			case QuoteTimedOut:
				resp.QuotesTimedOut++
			}
		}
		resp.CurrentSealPass = s.SealPass
		resp.QuoteExpiresAt = s.QuoteExpires.Unix()
	}

	return fn.Ok[ActorResp](resp)
}

// askEventAndProcessOutbox sends an event to the FSM and processes any emitted
// outbox messages. This consolidates a common pattern throughout the actor
// where FSM events trigger outbox processing.
func (a *Actor) askEventAndProcessOutbox(ctx context.Context, roundID RoundID,
	fsm *StateMachine, event Event) error {

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
func (a *Actor) askAndDrive(ctx context.Context, _ RoundID, fsm *StateMachine,
	event Event) ([]OutboxEvent, error) {

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
			a.log.WarnS(ctx, "Outbox dispatch failed",
				wrapped,
				logFields,
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
			// Skip re-registration responses: the FSM emits a
			// fresh ClientSuccessResp on every replay, but the
			// client is already counted from the original
			// admission. Bumping the join metric again would
			// drift per-round and aggregate counters by the
			// number of replays.
			if successResp, ok := m.(*ClientSuccessResp); ok &&
				!successResp.IsReregistration {

				a.trackClientJoin(
					ctx, successResp.Client,
					successResp.RoundID,
				)
			}

			sendReq := &clientconn.SendServerEventRequest{
				Message: m,
			}
			if successResp, ok := m.(*ClientSuccessResp); ok &&
				successResp.IsReregistration {

				identity := reregistrationSuccessMsgID(
					successResp,
				)
				sendReq.MailboxIdentity = identity
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

		case *RoundTickFiredReq:
			// Bump the per-result counter so operators can
			// alert on stuck rounds (sustained skipped_empty)
			// or measure tick-driven seal cadence.
			a.tellMetrics(ctx, &metrics.RoundTickFiredMsg{
				RoundID: m.RoundID.String(),
				Result:  string(m.Result),
			})

		case *RoundSealedReq:
			// Notify metrics actor that registration closed.
			a.tellMetrics(ctx, &metrics.RoundSealedMsg{
				RoundID: m.SealedRoundID.String(),
			})

			// Cancel any pending recurring tick for the sealed
			// round. cancelRoundTick is a no-op when no tick
			// was scheduled, so this is safe whether or not
			// RoundTickInterval was configured.
			err := a.cancelRoundTick(ctx, m.SealedRoundID)
			if err != nil {
				appendOutboxErr(
					"cancel tick on seal", msg, err,
				)
			}

			// Round has been sealed - create a new round for new
			// registrations.
			newRound, err := a.newRoundFSM(ctx)
			if err != nil {
				return fmt.Errorf("failed to create new "+
					"round: %w", err)
			}

			a.rounds[newRound.RoundID] = newRound
			a.currentRoundID = newRound.RoundID

			a.log.InfoS(ctx, "Created new round after sealing",
				"sealed_round", m.SealedRoundID,
				"new_round", newRound.RoundID,
			)

		case *RoundFailedReq:
			// Round has failed - clean up and create a new round if
			// this was the current round.
			a.log.ErrorS(ctx, "Round failed",
				fmt.Errorf("round failed: %s", m.Reason),
				"round_id", m.FailedRoundID,
				"reason", m.Reason)

			// Cancel any pending recurring tick for the failed
			// round (best-effort; cancelRoundTick is a no-op
			// when no tick was scheduled or it was already
			// cancelled).
			if err := a.cancelRoundTick(
				ctx, m.FailedRoundID,
			); err != nil {

				appendOutboxErr(
					"cancel tick on fail", msg, err,
				)
			}

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
				return fmt.Errorf("failed to broadcast round "+
					"%s: %w", m.RoundID, err)
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

	case *RoundTickFiredReq:
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

	fsm := a.buildAndStartRoundFSM(
		ctx, roundID, &CreatedState{}, startHeight,
	)

	// Schedule the recurring round tick if the operator configured a
	// non-zero interval. We do this imperatively rather than via the
	// FSM outbox so the tick is active even for empty rounds, where it
	// records skipped_empty until a client joins. The tick is cancelled
	// on RoundSealedReq / RoundFailedReq. Restored rounds loaded via
	// loadRoundFSM start in FinalizedState which has no TickEvent
	// handler, so we only schedule from the new-round path.
	if a.cfg.RoundTickInterval > 0 {
		a.scheduleRoundTick(ctx, roundID)
	}

	return fsm, nil
}

// handleJoinRoundRequest processes a JoinRoundRequest message by forwarding it
// to the current round FSM.
func (a *Actor) handleJoinRoundRequest(ctx context.Context,
	msg *JoinRoundRequest) fn.Result[ActorResp] {

	// Rate-limit per-client. The IntentCollectingState replacement
	// path is observably more expensive than the previous
	// duplicate-reject branch, so an unrestricted re-register flood
	// would saturate the actor loop (and the chain source pool) with
	// BIP-322 verifications and locker churn. Drop silently when the
	// bucket is empty so a flooding client gets no signal and an
	// honest client suffers nothing.
	if !a.allowJoinRequest(msg.ClientID) {
		a.log.WarnS(ctx, "Join request rate-limited, dropping",
			nil,
			slog.String("client_id", string(msg.ClientID)),
		)

		return fn.Ok[ActorResp](nil)
	}

	currentRound := a.getCurrentRound()
	if currentRound == nil {
		return fn.Err[ActorResp](
			fmt.Errorf("no current round available"),
		)
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
					slog.String(
						"type",
						fmt.Sprintf("%T", heightResp),
					),
				)
			} else {
				currentBlockHeight = uint32(
					bestHeightResp.Height,
				)
			}
		}
	}

	// Convert the actor message to an FSM event.
	joinEvent := &ClientJoinIntentEvent{
		ClientID:           msg.ClientID,
		Request:            msg.Request,
		CurrentBlockHeight: currentBlockHeight,
	}

	err := a.askEventAndProcessOutbox(
		ctx, currentRound.RoundID, currentRound.FSM, joinEvent,
	)
	if err != nil {
		return fn.Err[ActorResp](
			fmt.Errorf("FSM error processing join request: %w",
				err),
		)
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
			"phase", phase,
		)

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

	case TimeoutPhaseQuote:
		// Quote phase timeouts fan out into per-client
		// QuoteTimeoutEvents so each flip goes through the
		// FSM's status map individually (idempotent against
		// clients who resolved between timer scheduling and
		// timer firing). Look up the current state to find
		// the pending clients and their bound quote_ids.
		return a.fanOutQuoteTimeouts(ctx, roundID, round.FSM)

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
		return fn.Err[ActorResp](
			fmt.Errorf("FSM error processing %s timeout: %w",
				phase, err),
		)
	}

	return fn.Ok[ActorResp](nil)
}

// handleTickFired translates a *timeout.TickFiredMsg fire (delivered as
// rounds.TickFiredMsg) into a TickEvent injected into the round FSM. The
// tick ID has the composite "roundID:tick" shape, so we reuse
// parseTimeoutID for symmetry with handleTimeout. A tick whose round has
// already been sealed/failed (and therefore been removed from a.rounds)
// is dropped on the floor; the actor cancels the underlying recurring
// entry on RoundSealedReq/RoundFailedReq, but a fire that was already
// in-flight at the moment of seal can still arrive here, and that's
// fine.
func (a *Actor) handleTickFired(ctx context.Context,
	msg *TickFiredMsg) fn.Result[ActorResp] {

	roundID, phase, err := parseTimeoutID(msg.TickID)
	if err != nil {
		a.log.WarnS(ctx, "Failed to parse tick ID", err,
			"tick_id", string(msg.TickID))

		return fn.Ok[ActorResp](nil)
	}

	if phase != TimeoutPhaseTick {
		// Defensive: TickFiredMsg should only ever carry the tick
		// phase. Anything else is a bug in the schedule path.
		a.log.WarnS(ctx, "Ignoring tick fire with non-tick phase", nil,
			"round_id", roundID,
			"phase", phase)

		return fn.Ok[ActorResp](nil)
	}

	round := a.getRound(roundID)
	if round == nil {
		// Stale tick for a round we no longer track. Could happen
		// if a fire crossed a seal/fail. Cancel best-effort to
		// stop the underlying ticker.
		a.log.DebugS(ctx, "Ignoring tick for unknown round",
			"round_id", roundID,
		)

		_ = a.cancelRoundTick(ctx, roundID)

		return fn.Ok[ActorResp](nil)
	}

	err = a.askEventAndProcessOutbox(
		ctx, roundID, round.FSM, &TickEvent{},
	)
	if err != nil {
		return fn.Err[ActorResp](
			fmt.Errorf("FSM error processing tick: %w", err),
		)
	}

	return fn.Ok[ActorResp](nil)
}

// fanOutQuoteTimeouts turns a single TimeoutPhaseQuote firing into
// per-client QuoteTimeoutEvents. The actor looks up the current
// QuoteSentState (silently no-ops if the FSM has already advanced
// past it), iterates the Status map for QuotePending clients, and
// fires one per-client QuoteTimeoutEvent carrying the active
// quote_id. Per-client dispatch keeps the FSM-level handler
// straightforward (it only reasons about one client at a time) and
// preserves the quote_id stale-check path (a timer fired after a
// reseal will carry stale quote_ids and be dropped at the FSM
// boundary).
func (a *Actor) fanOutQuoteTimeouts(ctx context.Context, roundID RoundID,
	fsm *StateMachine) fn.Result[ActorResp] {

	state, err := fsm.CurrentState()
	if err != nil {
		a.log.WarnS(ctx, "Quote timeout: FSM unavailable", err,
			"round_id", roundID)

		return fn.Ok[ActorResp](nil)
	}

	qs, ok := state.(*QuoteSentState)
	if !ok {

		// FSM advanced past QuoteSentState before the timer
		// fired; nothing to fan out.
		return fn.Ok[ActorResp](nil)
	}

	for cid, status := range qs.Status {
		if status != QuotePending {
			continue
		}

		q, ok := qs.Quotes[cid]
		if !ok || q == nil {
			continue
		}

		timeoutEvt := &QuoteTimeoutEvent{
			ClientID: cid,
			QuoteID:  q.QuoteID,
		}

		err := a.askEventAndProcessOutbox(
			ctx, roundID, fsm, timeoutEvt,
		)
		if err != nil {
			return fn.Err[ActorResp](
				fmt.Errorf("quote timeout fan-out: %w", err),
			)
		}
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
		return fn.Err[ActorResp](
			fmt.Errorf("FSM error processing confirmation: %w",
				err),
		)
	}

	// Get the confirmed state to access VTXOTrees for batch watcher
	// registration.
	currentState, err := round.FSM.CurrentState()
	if err != nil {
		a.log.WarnS(ctx, "Failed to get current state after "+
			"confirmation",
			err, "round_id", msg.RoundID)
	} else if cs, ok := currentState.(*ConfirmedState); ok {
		// Register VTXO trees with the batch watcher for on-chain
		// monitoring. Use the per-round SweepKey from ConfirmedState
		// (originally captured at finalization, preserved across
		// restart via loadRoundFSM) so the watcher pins each batch to
		// the historical descriptor rather than the operator's
		// currently configured sweep key.
		//
		// Failure here is money-loss-radius: an unregistered tree
		// never produces fraud/expiry notifications. We deliberately
		// do NOT leave the round tracked on failure -- ConfirmedState
		// is terminal in the FSM and there is no retry path, so
		// keeping per-round state in memory would just leak forever.
		// Instead we convert the silent loss into a LOUD observable
		// loss: bump a dedicated operator-alert counter, run the same
		// cleanup as the success path, then propagate the error up
		// the actor framework. There is no automatic redrive; the
		// operator must inspect the alert and trigger a manual
		// re-registration (e.g. by replaying confirmation via the
		// chain source actor once the watcher is healthy).
		regErr := a.registerBatchesWithWatcher(
			ctx, msg.RoundID, msg.BlockHeight, cs.VTXOTrees,
			cs.SweepKey,
		)
		if regErr != nil {
			// Count one failure per affected batch so the
			// operator alert rate reflects the number of
			// unwatched trees, not just rounds. We use the
			// errors.Join unwrap to surface the per-batch
			// errors aggregated by registerBatchesWithWatcher.
			batchCount := countWatcherBatchFailures(regErr)
			a.tellMetrics(ctx,
				&metrics.BatchWatcherRegisterFailedMsg{
					RoundID:    msg.RoundID.String(),
					BatchCount: batchCount,
				},
			)

			// Clean up tracked state and emit the terminal
			// RoundCompleted metric (with a distinct status so
			// dashboards differentiate it from a clean
			// confirmation). Leaving the round in a.rounds
			// would be a memory leak with no recovery path,
			// so we drop it from the FSM map as well as the
			// per-client tracking (untrackRound only clears
			// the latter).
			a.untrackRound(msg.RoundID)
			delete(a.rounds, msg.RoundID)
			a.tellMetrics(ctx, &metrics.RoundCompletedMsg{
				RoundID:     msg.RoundID.String(),
				Status:      "confirmed_watcher_failed",
				BlockHeight: uint32(msg.BlockHeight),
			})

			return fn.Err[ActorResp](
				fmt.Errorf("batch watcher registration "+
					"failed for round %s; round state "+
					"cleaned up, operator must redrive "+
					"manually (no automatic retry): %w",
					msg.RoundID, regErr),
			)
		}

		// Publish VTXO created events to the indexer so
		// recipients with registered receive scripts are
		// notified about their new VTXOs.
		a.publishVTXOEvents(ctx, msg.RoundID, cs)
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
		"num_confs", msg.NumConfs,
	)

	return fn.Ok[ActorResp](nil)
}

// countWatcherBatchFailures returns the number of per-batch failures
// aggregated inside a watcher registration error. registerBatches-
// WithWatcher wraps the per-batch errors with errors.Join so we can
// unwrap and count. Falls back to 1 if the structure does not match
// (e.g., a non-aggregated error) so the operator alert never under-
// counts.
func countWatcherBatchFailures(err error) int {
	type unwrapMulti interface {
		Unwrap() []error
	}

	// registerBatchesWithWatcher wraps errors.Join(...) inside an
	// fmt.Errorf with %w, so the joined error is one Unwrap() hop
	// away. Walk a couple of levels to be safe against future
	// wrapping changes.
	cur := err
	for i := 0; i < 4 && cur != nil; i++ {
		if multi, ok := cur.(unwrapMulti); ok {
			n := len(multi.Unwrap())
			if n > 0 {
				return n
			}
		}
		cur = errors.Unwrap(cur)
	}

	return 1
}

// broadcastAndSubscribe broadcasts the round's signed transaction and
// subscribes to confirmation notifications.
func (a *Actor) broadcastAndSubscribe(ctx context.Context,
	req *BroadcastRoundReq) error {

	// Skip if ChainSourceActor is not configured (e.g., in tests).
	if a.cfg.ChainSourceActor == nil {
		a.log.DebugS(ctx, "Skipping broadcast - no chain source actor",
			"round_id", req.RoundID,
		)

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
		"txid", txHash.String(),
	)

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
		"target_confs", a.cfg.ConfirmationTarget,
	)

	return nil
}

// batchWatcherEnqueueMaxAttempts is the total number of Tell attempts
// (initial + retries) used when enqueuing a RegisterBatchRequest to the
// BatchWatcher actor. A small bounded retry shields confirmation handling
// from transient mailbox-context races during shutdown/restart, while
// still failing fast enough to surface a stuck or terminated watcher to
// the caller. Tuned conservatively: confirmations are infrequent (one
// per round per confirmation block), so the extra latency cost of a few
// retries is negligible compared to losing watcher coverage.
const batchWatcherEnqueueMaxAttempts = 3

// batchWatcherEnqueueRetryDelay is the fixed backoff between Tell
// attempts when enqueuing a RegisterBatchRequest. Kept short because
// the only retryable failures are transient context/mailbox states.
// Retries are SERIAL across batches inside a single
// registerBatchesWithWatcher call, so the worst-case stall for one
// confirmation is
// N_batches * (batchWatcherEnqueueMaxAttempts - 1) *
// batchWatcherEnqueueRetryDelay
// in the all-fail case. Confirmations are infrequent (~one per
// confirmation block) so the bound is acceptable; if VTXO trees per
// round ever grow large enough to make this matter, parallelize the
// per-batch enqueue rather than shortening the delay.
const batchWatcherEnqueueRetryDelay = 50 * time.Millisecond

// with the BatchWatcher for on-chain monitoring. The sweepKey argument is the
// per-round descriptor captured at finalization (and reloaded across restart);
// it overrides the actor's currently-configured key so post-rotation restarts
// still sign each batch with the descriptor that built its tapleaf. It returns
// an error if the BatchWatcher is configured but one or more batches could not
// be enqueued for registration. Returning the failure is critical: missing
// watcher registration disables fraud/expiry response for that batch and can
// lead to operator or client fund loss. The caller is responsible for keeping
// the round tracked (so the operator notices the inconsistency) and surfacing
// the failure upstream. A bounded retry-with-backoff is attempted per batch to
// absorb transient mailbox states; see batchWatcherEnqueueMaxAttempts /
// batchWatcherEnqueueRetryDelay for tuning.
func (a *Actor) registerBatchesWithWatcher(ctx context.Context, roundID RoundID,
	blockHeight int32, vtxoTrees map[int]*tree.Tree,
	sweepKey keychain.KeyDescriptor) error {

	// If no watcher is configured we have nothing to register. This is a
	// supported deployment mode (e.g., unit tests, watcher disabled by
	// operator) and not an error.
	if a.cfg.BatchWatcher.IsNone() {
		return nil
	}
	ref := a.cfg.BatchWatcher.UnsafeFromSome()

	// Calculate expiry height based on confirmation height and sweep
	// delay from terms.
	expiryHeight := uint32(blockHeight) + a.cfg.Terms.SweepDelay

	// Collect every per-batch failure so we surface a complete picture
	// to the caller instead of stopping at the first error. Each entry
	// keeps the original error for diagnostics; the caller decides how
	// to react (operator alerting, leaving the round tracked, etc.).
	var failures []error

	// Register each VTXO tree with the batch watcher.
	for outputIdx, vtxoTree := range vtxoTrees {
		if vtxoTree == nil {
			continue
		}

		// Create the deterministic BatchID for this round/output
		// pair so watcher state can be recovered and inspected
		// consistently.
		batchID := batchwatcher.BatchIDForRoundOutput(
			uuid.UUID(roundID), outputIdx,
		)

		req := &batchwatcher.RegisterBatchRequest{
			BatchID:            batchID,
			Tree:               vtxoTree,
			ConfirmationHeight: uint32(blockHeight),
			ExpiryHeight:       expiryHeight,
			// Pass the descriptor used to derive the sweep
			// tapleaf at finalization time so the sweeper
			// signs with the matching historical locator
			// rather than whatever key the actor is currently
			// configured with. This descriptor is carried on
			// ConfirmedState (set at finalization and preserved
			// across a restart via loadRoundFSM) so a
			// confirmation arriving after a sweep-key rotation
			// still binds the pre-rotation descriptor to the
			// batch.
			SweepKey: sweepKey,
		}

		// Send registration request using fire-and-forget Tell with
		// a bounded retry. Tell enqueues into the watcher's mailbox;
		// failures here mean the message was never accepted, not
		// that processing failed. Failing to enqueue is a money-risk
		// condition because the watcher will not monitor this batch
		// for spends, sweeps, or expiry.
		err := a.tellBatchWatcherWithRetry(ctx, ref, req)
		if err != nil {
			// Classify the log level: externally-triggered
			// shutdown/cancellation errors (actor terminated,
			// mailbox closed, context cancel/deadline) are
			// expected during graceful shutdown and must not
			// page operators. Anything else is an internal-bug
			// signal and warrants ErrorS per the project's
			// log-level convention (CLAUDE.md: error level is
			// only for internal bugs, never external triggers).
			// The operator-alert signal lives on the
			// BatchWatcherRegisterFailures counter, not on the
			// log level, so this classification only affects
			// log volume during shutdown, not alerting.
			logKVs := []any{
				"round_id", roundID,
				"batch_id", batchID,
				"output_idx", outputIdx,
				slog.Uint64(
					"expiry_height", uint64(expiryHeight),
				),
			}
			if isShutdownErr(err) {
				a.log.WarnS(ctx, "Failed to register "+
					"batch with watcher (shutdown); "+
					"batch will not be monitored "+
					"on-chain", err, logKVs...)
			} else {
				a.log.ErrorS(ctx, "Failed to register "+
					"batch with watcher; batch will "+
					"not be monitored on-chain", err,
					logKVs...)
			}

			failures = append(
				failures, fmt.Errorf("batch %s (output "+
					"%d): %w", batchID, outputIdx, err),
			)

			continue
		}

		a.log.InfoS(ctx, "Registered batch with watcher",
			"round_id", roundID,
			"batch_id", batchID,
			"output_idx", outputIdx,
			slog.Uint64(
				"expiry_height", uint64(expiryHeight),
			))
	}

	if len(failures) > 0 {
		return fmt.Errorf("failed to register %d batch(es) with "+
			"watcher for round %s: %w", len(failures), roundID,
			errors.Join(failures...))
	}

	return nil
}

// isShutdownErr reports whether an error originates from an
// externally-triggered shutdown or cancellation (graceful actor
// termination, mailbox closure, caller-context cancel/deadline).
// Used to demote enqueue-failure log level from Error to Warn during
// shutdown so we do not page operators on graceful drain. The
// operator-alert signal for missing watcher coverage lives on the
// BatchWatcherRegisterFailures counter, which is incremented regardless
// of log level.
func isShutdownErr(err error) bool {
	return errors.Is(err, actor.ErrActorTerminated) ||
		errors.Is(err, actor.ErrMailboxClosed) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

// tellBatchWatcherWithRetry enqueues a RegisterBatchRequest with the
// BatchWatcher actor, retrying a bounded number of times on transient
// errors. Caller-context cancellation is treated as terminal (no
// retries), since the parent operation has been aborted and any retry
// would race against actor shutdown.
func (a *Actor) tellBatchWatcherWithRetry(ctx context.Context,
	ref actor.ActorRef[
		batchwatcher.BatchWatcherMsg,
		batchwatcher.BatchWatcherResp,
	],
	req *batchwatcher.RegisterBatchRequest) error {

	var lastErr error
	for attempt := 0; attempt < batchWatcherEnqueueMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		err := ref.Tell(ctx, req)
		if err == nil {
			return nil
		}

		// Caller context cancellations are terminal; do not retry.
		if errors.Is(err, context.Canceled) ||
			errors.Is(err, context.DeadlineExceeded) {
			return err
		}

		lastErr = err

		// Sleep before the next attempt, but bail out immediately
		// if the caller's context fires during the delay.
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-time.After(batchWatcherEnqueueRetryDelay):
		}
	}

	return lastErr
}

// publishVTXOEvents iterates the confirmed round's VTXO tree leaves
// and publishes VTXO_CREATED events to the indexer. Clients that
// registered receive scripts matching a leaf's pkScript will be
// notified, enabling in-round VTXO receipt.
func (a *Actor) publishVTXOEvents(ctx context.Context, roundID RoundID,
	cs *ConfirmedState) {

	if a.cfg.VTXOEventPublisher == nil {
		return
	}

	// Compute batch expiry as absolute height: confirmation height
	// plus the operator's sweep delay. The client expects an
	// absolute height in this field.
	batchExpiry := int32(0)
	if a.cfg.Terms != nil {
		batchExpiry = cs.BlockHeight +
			int32(a.cfg.Terms.SweepDelay)
	}
	relativeExpiry := uint32(0)
	if a.cfg.Terms != nil {
		relativeExpiry = a.cfg.Terms.VTXOExitDelay
	}

	// The commitment tx ID is the hash of the signed commitment
	// transaction, used by the client to distinguish it from
	// leaf txids.
	var commitTxID []byte
	if cs.FinalTx != nil {
		txHash := cs.FinalTx.TxHash()
		commitTxID = txHash[:]
	}

	for _, vtxoTree := range cs.VTXOTrees {
		// VTXOTrees is a map keyed by commitment tx output
		// index. A nil value can occur if tree construction
		// was skipped for that output (e.g. boarding-only
		// outputs that have no VTXO sub-tree).
		if vtxoTree == nil {
			continue
		}

		err := vtxoTree.Root.ForEachLeaf(func(node *tree.Node) error {
			outpoint, opErr := node.GetNonAnchorOutpoint()
			if opErr != nil {
				return opErr
			}

			pkScript := node.Outputs[0].PkScript
			value := node.Outputs[0].Value

			pubErr := a.cfg.VTXOEventPublisher.PublishVTXOCreated(
				ctx, pkScript, *outpoint, value,
				roundID.String(), batchExpiry, relativeExpiry,
				arkrpc.VTXOOrigin_VTXO_ORIGIN_IN_ROUND,
				commitTxID,
			)
			if pubErr != nil {
				a.log.WarnS(ctx,
					"Failed to publish VTXO "+
						"event", pubErr,
					"outpoint",
					outpoint.String())
			}

			return nil
		})
		if err != nil {
			a.log.WarnS(ctx,
				"Failed to iterate VTXO tree for "+
					"event publishing", err)
		}
	}
}
