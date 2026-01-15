//nolint:ll
package round

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/lightninglabs/darepo-client/wallet"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
)

// Compile-time assertion that RoundClientActor implements actor.Stoppable.
var _ actor.Stoppable = (*RoundClientActor)(nil)

// RoundFSM wraps a state machine instance for a specific round.
type RoundFSM struct {
	// FSM is the state machine for this round. The baselib protofsm uses 3
	// type parameters: InternalEvent, OutboxEvent, Env.
	FSM *ClientStateMachine

	// Key is the current key for this round in the actor's map. It starts
	// as a TempRoundKey and is upgraded to a RoundID when the server
	// assigns one.
	Key RoundKey

	// RoundID is the unique identifier for this round, assigned by the
	// server. Zero value until the server assigns an ID.
	RoundID RoundID

	// TxID is the commitment transaction ID for this round.
	TxID chainhash.Hash

	// CommitmentTx is the commitment transaction as a PSBT, used for
	// registering confirmation notifications with the correct pkScript.
	CommitmentTx fn.Option[*psbt.Packet]
}

// RoundClientActor wraps the client boarding FSM in an actor interface. The
// actor manages the FSM lifecycle, handles incoming actor messages, converts
// them to FSM events, processes outbox messages, and integrates with the
// chainsource actor for chain monitoring.
//
// Architecture:
//   - Actor holds FSMs (protofsm.StateMachine) in a unified map.
//   - Rounds start with TempRoundKey, re-keyed to RoundID on server response.
//   - Actor receives actor messages (ClientMsg).
//   - Actor converts messages to FSM events.
//   - FSM processes events producing new state and outbox.
//   - Actor processes outbox by sending messages to server/chainsource.
type RoundClientActor struct {
	// cfg contains all the configuration for this actor.
	cfg *RoundClientConfig

	// log is the logger for this actor instance.
	log btclog.Logger

	// rounds tracks all round FSMs keyed by their RoundKey. Rounds start
	// with a TempRoundKey and are re-keyed to their server-assigned RoundID
	// when received via RoundJoined. This enables concurrent round assembly.
	rounds map[RoundKeyStr]*RoundFSM

	// commitmentTxIndex maps commitment transaction IDs to their round
	// keys for routing confirmation events.
	commitmentTxIndex map[chainhash.Hash]RoundKeyStr

	// env is the base FSM environment template containing all dependencies.
	// Each new round FSM gets a copy with a fresh StartHeight.
	env *ClientEnvironment
}

// RoundClientConfig houses the configuration for a RoundClientActor.
type RoundClientConfig struct {
	// Name uniquely identifies this actor instance.
	Name string

	// Logger is the logger for this actor instance. If nil, uses the global
	// package logger.
	Logger btclog.Logger

	// Wallet provides MuSig2 signing capabilities needed for round
	// participation. Boarding address creation is handled by the wallet
	// actor.
	Wallet ClientWallet

	// RoundStore persists round coordination and checkpointing.
	RoundStore RoundStore

	// VTXOStore persists off-chain balance.
	VTXOStore VTXOStore

	// OperatorTerms contains the operator's parameters.
	OperatorTerms *types.OperatorTerms

	// ServerConn is a reference to the ServerConnectionActor for sending
	// messages to the Ark server.
	ServerConn actor.TellOnlyRef[serverconn.ServerConnMsg]

	// ChainSource is a reference to the ChainSource actor for registering
	// confirmation notifications for commitment transactions and querying
	// block height.
	ChainSource actor.ActorRef[chainsource.ChainSourceMsg, chainsource.ChainSourceResp]

	// WalletActor is a reference to the Ark wallet actor. The round actor
	// registers to receive BoardingUtxoConfirmedEvent notifications when
	// new boarding UTXOs are confirmed.
	WalletActor actor.ActorRef[wallet.WalletMsg, wallet.WalletResp]

	// SelfRef is a reference to this actor for receiving asynchronous
	// notifications (e.g., confirmations from ChainSource).
	SelfRef actor.TellOnlyRef[ClientMsg]

	// ChainParams are the Bitcoin network parameters.
	ChainParams *chaincfg.Params

	// VTXOManager receives VTXO creation notifications after rounds
	// complete. The round actor forwards VTXOCreatedNotification messages
	// to spawn VTXO actors for newly created VTXOs. Uses actor.Message to
	// avoid import cycle with vtxo package. Optional - if nil,
	// notifications are not forwarded.
	VTXOManager actor.TellOnlyRef[actor.Message]
}

// NewRoundClientActor creates a new client actor with the provided
// configuration. FSMs are created on-demand when boarding intents arrive.
//
// The FSM uses interfaces directly and calls lib package functions as needed.
// Chain operations are handled via outbox messages (not direct calls).
func NewRoundClientActor(cfg *RoundClientConfig) fn.Result[*RoundClientActor] {
	// Use the configured logger, falling back to the global package logger.
	actorLog := cfg.Logger
	if actorLog == nil {
		actorLog = log
	}

	// Create base FSM environment template with direct interface
	// assignments. The FSM will call lib functions directly when needed
	// (e.g., lib.NewTreeSignerSession, signing helpers). StartHeight is set
	// to 0 here and will be set per-round when FSMs are created.
	env := &ClientEnvironment{
		RoundStore:    cfg.RoundStore,
		VTXOStore:     cfg.VTXOStore,
		Wallet:        cfg.Wallet,
		OperatorTerms: cfg.OperatorTerms,
		ChainParams:   cfg.ChainParams,
		Log:           actorLog,
	}

	if err := ValidateDelayParameters(
		cfg.OperatorTerms.SweepDelay, cfg.OperatorTerms.VTXOExitDelay,
	); err != nil {
		return fn.Err[*RoundClientActor](err)
	}

	// No FSM is created here. FSMs are created on-demand when boarding
	// intents arrive via createNewRound().
	return fn.Ok(&RoundClientActor{
		cfg:               cfg,
		log:               actorLog,
		rounds:            make(map[RoundKeyStr]*RoundFSM),
		commitmentTxIndex: make(map[chainhash.Hash]RoundKeyStr),
		env:               env,
	})
}

// queryBestHeight queries the ChainSource for the current best block height.
// This wraps the Ask->Await->Unpack pattern for height queries, providing a
// clean interface for callers that need the current height.
func (a *RoundClientActor) queryBestHeight(ctx context.Context) (uint32, error) {
	heightFuture := a.cfg.ChainSource.Ask(ctx, &chainsource.BestHeightRequest{})
	heightResult := heightFuture.Await(ctx)

	heightResp, err := heightResult.Unpack()
	if err != nil {
		return 0, fmt.Errorf("failed to query best height: %w", err)
	}

	bestHeightResp, ok := heightResp.(*chainsource.BestHeightResponse)
	if !ok {
		return 0, fmt.Errorf("unexpected height response type: %T",
			heightResp)
	}

	return uint32(bestHeightResp.Height), nil
}

// createRoundFSMFromDB creates a new FSM instance for a specific round,
// restoring from checkpointed state. Uses FetchState to load both round data
// and FSM state atomically. Used when loading active rounds from database on
// startup.
func (a *RoundClientActor) createRoundFSMFromDB(ctx context.Context,
	roundID RoundID) (*RoundFSM, error) {

	round, state, err := a.cfg.RoundStore.FetchState(ctx, roundID)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch round state: %w", err)
	}

	// Use the StartHeight stored in the round when it was created. This
	// ensures we scan from the original starting point, not the current
	// height, which could miss confirmations if the tx was already mined.
	startHeight := round.StartHeight

	fsmPrefix := roundID.LogPrefix()
	fsmLogger := a.log.WithPrefix(fsmPrefix)

	env := &ClientEnvironment{
		RoundStore:    a.cfg.RoundStore,
		VTXOStore:     a.cfg.VTXOStore,
		Wallet:        a.cfg.Wallet,
		OperatorTerms: a.cfg.OperatorTerms,
		ChainParams:   a.cfg.ChainParams,
		Log:           fsmLogger,
		StartHeight:   startHeight,
	}
	fsmCfg := ClientStateMachineCfg{
		Logger:        fsmLogger,
		ErrorReporter: newContextErrorReporter(ctx, fsmPrefix),
		InitialState:  state,
		Env:           env,
	}
	fsm := protofsm.NewStateMachine(fsmCfg)
	fsm.Start(ctx)

	a.log.InfoS(ctx, "Created round FSM from checkpoint",
		slog.String("round_id", round.RoundID.String()),
		slog.String("initial_state", state.String()))

	txid := fn.MapOptionZ(
		round.CommitmentTx, func(p *psbt.Packet) chainhash.Hash {
			return p.UnsignedTx.TxHash()
		},
	)

	return &RoundFSM{
		FSM:          &fsm,
		Key:          roundID,
		RoundID:      round.RoundID,
		TxID:         txid,
		CommitmentTx: round.CommitmentTx,
	}, nil
}

// createNewRound creates a new round FSM with a temporary key when a boarding
// intent arrives. The round starts in Idle state and will be re-keyed to a
// server-assigned RoundID when RoundJoined is received.
func (a *RoundClientActor) createNewRound(ctx context.Context) (*RoundFSM, error) {
	tempKey, err := NewTempRoundKey()
	if err != nil {
		return nil, fmt.Errorf("failed to generate temp key: %w", err)
	}

	startHeight, err := a.queryBestHeight(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to query start height: %w", err)
	}

	fsmPrefix := tempKey.LogPrefix()
	fsmLogger := a.log.WithPrefix(fsmPrefix)

	env := &ClientEnvironment{
		RoundStore:    a.cfg.RoundStore,
		VTXOStore:     a.cfg.VTXOStore,
		Wallet:        a.cfg.Wallet,
		OperatorTerms: a.cfg.OperatorTerms,
		ChainParams:   a.cfg.ChainParams,
		Log:           fsmLogger,
		StartHeight:   startHeight,
	}
	fsmCfg := ClientStateMachineCfg{
		Logger:        fsmLogger,
		ErrorReporter: newContextErrorReporter(ctx, fsmPrefix),
		InitialState:  &Idle{},
		Env:           env,
	}
	fsm := protofsm.NewStateMachine(fsmCfg)
	fsm.Start(ctx)

	roundFSM := &RoundFSM{
		FSM: &fsm,
		Key: tempKey,
	}

	keyStr := RoundKeyStr(tempKey.KeyString())
	a.rounds[keyStr] = roundFSM

	a.log.InfoS(ctx, "Created new round FSM",
		slog.String("temp_key", tempKey.String()),
		slog.Int("start_height", int(startHeight)))

	return roundFSM, nil
}

// findIdleRound finds an existing round FSM that is in Idle state and can
// accept new boarding intents. Returns nil if no idle round exists.
func (a *RoundClientActor) findIdleRound() *RoundFSM {
	for _, roundFSM := range a.rounds {
		state, err := roundFSM.FSM.CurrentState()
		if err != nil {
			continue
		}

		if _, ok := state.(*Idle); ok {
			return roundFSM
		}
	}

	return nil
}

// findAssemblingRound finds a round that is currently assembling intents.
// It prioritizes PendingRoundAssembly (which already has boarding inputs)
// over Idle rounds. This ensures VTXOs are attached to rounds that have
// inputs, preventing registration failures from empty input sets.
func (a *RoundClientActor) findAssemblingRound() *RoundFSM {
	// First pass: prefer rounds that already have boarding intents.
	for _, roundFSM := range a.rounds {
		state, err := roundFSM.FSM.CurrentState()
		if err != nil {
			continue
		}

		if _, ok := state.(*PendingRoundAssembly); ok {
			return roundFSM
		}
	}

	// Second pass: fall back to idle rounds.
	for _, roundFSM := range a.rounds {
		state, err := roundFSM.FSM.CurrentState()
		if err != nil {
			continue
		}

		if _, ok := state.(*Idle); ok {
			return roundFSM
		}
	}

	return nil
}

// findRoundByOutpoints finds a pending round (in RegistrationSentState) whose
// inputs match the given outpoints. Used to correlate RoundJoined responses to
// the correct pending round when multiple rounds are in-flight concurrently.
func (a *RoundClientActor) findRoundByOutpoints(
	boardingOutpoints, vtxoOutpoints []wire.OutPoint) *RoundFSM {

	// Build set of boarding outpoints for efficient lookup.
	boardingSet := fn.NewSet(boardingOutpoints...)

	// TODO: When VTXO operations (forfeit/leave/refresh) are implemented,
	// also match vtxoOutpoints against the round's involved VTXOs.
	_ = vtxoOutpoints

	for _, roundFSM := range a.rounds {
		state, err := roundFSM.FSM.CurrentState()
		if err != nil {
			continue
		}

		regState, ok := state.(*RegistrationSentState)
		if !ok {
			continue
		}

		// Check if this round's intents match the boarding outpoints.
		if a.intentsMatchOutpoints(regState.Intents.Boarding, boardingSet) {
			return roundFSM
		}
	}

	return nil
}

// intentsMatchOutpoints checks if a round's boarding intents exactly match the
// given set of outpoints.
func (a *RoundClientActor) intentsMatchOutpoints(
	intents []BoardingIntent, outpoints fn.Set[wire.OutPoint]) bool {

	if uint(len(intents)) != outpoints.Size() {
		return false
	}

	for _, intent := range intents {
		if !outpoints.Contains(intent.Outpoint) {
			return false
		}
	}

	return true
}

// registerCommitmentConfirmation registers for confirmation monitoring of a
// commitment transaction with the chainsource actor. The commitmentTx is used
// to extract the pkScript for LND's confirmation tracking.
func (a *RoundClientActor) registerCommitmentConfirmation(ctx context.Context,
	txid chainhash.Hash, commitmentTx fn.Option[*psbt.Packet]) {

	callerID := fmt.Sprintf("commitment-tx-%s", txid.String())

	mappedRef := chainsource.MapConfirmationEvent(
		a.cfg.SelfRef,
		func(ce chainsource.ConfirmationEvent) ClientMsg {
			return &ConfirmationEvent{
				Txid:          ce.Txid,
				BlockHeight:   ce.BlockHeight,
				Confirmations: ce.NumConfs,
				Tx:            ce.Tx,
			}
		},
	)

	// Extract pkScript from the commitment transaction's first output.
	// LND requires a pkScript for confirmation tracking.
	var pkScript []byte
	commitmentTx.WhenSome(func(packet *psbt.Packet) {
		if packet.UnsignedTx != nil && len(packet.UnsignedTx.TxOut) > 0 {
			pkScript = packet.UnsignedTx.TxOut[0].PkScript
		}
	})

	// Query ChainSource for current block height to use as HeightHint.
	// LND requires HeightHint > 0 for confirmation scanning.
	var heightHint uint32
	heightFuture := a.cfg.ChainSource.Ask(
		ctx, &chainsource.BestHeightRequest{},
	)
	heightResult := heightFuture.Await(ctx)
	heightResp, err := heightResult.Unpack()
	if err == nil {
		bestHeightResp, ok := heightResp.(*chainsource.BestHeightResponse)
		if ok {
			heightHint = uint32(bestHeightResp.Height)
		}
	} else {
		a.log.WarnS(ctx, "Failed to get best height for confirmation",
			err, slog.String("txid", txid.String()))
	}

	confReq := &chainsource.RegisterConfRequest{
		CallerID:    callerID,
		Txid:        &txid,
		PkScript:    pkScript,
		TargetConfs: a.cfg.OperatorTerms.MinConfirmations,
		HeightHint:  heightHint,
		NotifyActor: fn.Some(mappedRef),
	}

	// Use a background context for the confirmation registration. The
	// ConfActor needs a long-lived context that won't be cancelled when
	// the current message processing completes.
	a.cfg.ChainSource.Tell(context.Background(), confReq)
}

// askEventAndProcessOutbox sends an event to the FSM and processes any
// emitted outbox messages. This consolidates a common pattern throughout
// the actor where FSM events trigger outbox processing.
func (a *RoundClientActor) askEventAndProcessOutbox(
	ctx context.Context, fsm *ClientStateMachine, event ClientEvent) error {

	future := fsm.AskEvent(ctx, event)
	result := future.Await(ctx)

	events, err := result.Unpack()
	if err != nil {
		return err
	}

	if len(events) > 0 {
		if err := a.processOutbox(ctx, events); err != nil {
			return fmt.Errorf("failed to process outbox: %w", err)
		}
	}

	return nil
}

// OnStop implements actor.Stoppable to gracefully shut down all FSMs when the
// actor is stopping. This prevents goroutine leaks by stopping all round FSMs.
func (a *RoundClientActor) OnStop(ctx context.Context) error {
	a.log.InfoS(ctx, "Stopping round client actor",
		slog.Int("rounds", len(a.rounds)))

	// Stop all round FSMs.
	for keyStr, roundFSM := range a.rounds {
		a.log.DebugS(ctx, "Stopping round FSM",
			slog.String("key", string(keyStr)))

		roundFSM.FSM.Stop()
	}

	a.log.InfoS(ctx, "Round client actor stopped")

	return nil
}

// Start initializes the actor by registering with the wallet actor to receive
// boarding UTXO confirmation notifications, and resuming any active rounds.
// This should be called once after actor creation to restore state.
func (a *RoundClientActor) Start(ctx context.Context) error {
	a.log.InfoS(ctx, "Starting round client actor",
		slog.String("name", a.cfg.Name))

	// Register with the wallet actor to receive BoardingUtxoConfirmedEvent
	// notifications. The wallet handles all boarding address monitoring and
	// will notify us when new UTXOs are confirmed.
	mappedRef := actor.NewMapInputRef(
		a.cfg.SelfRef,
		func(evt wallet.BoardingUtxoConfirmedEvent) ClientMsg {
			return &WalletBoardingConfirmed{
				Intent: evt.BoardingIntent,
			}
		},
	)

	// Request all historical confirmations. The wallet will send backlog
	// events for any confirmed intents.
	regReq := &wallet.RegisterConfirmationNotifierRequest{
		NotifierID:    fmt.Sprintf("round-actor-%s", a.cfg.Name),
		NotifyActor:   mappedRef,
		BacklogHeight: fn.None[int32](),
		MinConf:       fn.Some(a.cfg.OperatorTerms.MinConfirmations),
	}

	future := a.cfg.WalletActor.Ask(ctx, regReq)
	result := future.Await(ctx)
	if result.IsErr() {
		return fmt.Errorf("register with wallet: %w", result.Err())
	}

	a.log.InfoS(ctx, "Registered with wallet actor for boarding confirmations",
		slog.Int("min_confirmations", int(a.cfg.OperatorTerms.MinConfirmations)))

	// Load active rounds (commitment tx broadcast, not yet confirmed) and
	// resume their FSMs. These rounds have server-assigned RoundIDs from the
	// checkpoint.
	activeRounds, err := a.cfg.RoundStore.ListActiveRounds(ctx)
	if err != nil {
		return fmt.Errorf("failed to load active rounds: %w", err)
	}

	a.log.InfoS(ctx, "Loaded active rounds from database",
		slog.Int("count", len(activeRounds)))

	for _, round := range activeRounds {
		roundFSM, err := a.createRoundFSMFromDB(ctx, round.RoundID)
		if err != nil {
			return fmt.Errorf("failed to create FSM for "+
				"round %s: %w", round.RoundID, err)
		}

		// Use the RoundID as the key (already server-assigned at
		// checkpoint).
		keyStr := RoundKeyStr(round.RoundID.KeyString())
		a.rounds[keyStr] = roundFSM

		// Register for confirmation of the commitment tx for this
		// round.
		if !roundFSM.TxID.IsEqual(&chainhash.Hash{}) {
			a.commitmentTxIndex[roundFSM.TxID] = keyStr
			a.registerCommitmentConfirmation(
				ctx, roundFSM.TxID, round.CommitmentTx,
			)

			a.log.InfoS(ctx, "Resumed round awaiting confirmation",
				slog.String("round_id", round.RoundID.String()),
				slog.String("commitment_txid", roundFSM.TxID.String()))
		}
	}

	a.log.InfoS(ctx, "Round client actor started")

	return nil
}

// Receive processes an actor message and returns a response. This is the main
// entry point for the actor.
func (a *RoundClientActor) Receive(ctx context.Context,
	msg ClientMsg) fn.Result[ClientResp] {

	switch m := msg.(type) {
	case *WalletBoardingConfirmed:
		return a.handleWalletBoardingConfirmed(ctx, m)

	case *RegisterVTXORequestsRequest:
		return a.handleVTXORequests(ctx, m)

	case *ServerMessageNotification:
		return a.handleServerMessage(ctx, m)

	case *GetClientStateRequest:
		return a.handleGetState(ctx, m)

	case *CancelRoundRequest:
		return a.handleCancelRound(ctx, m)

	case *ConfirmationEvent:
		return a.handleConfirmation(ctx, m)

	default:
		return fn.Err[ClientResp](fmt.Errorf(
			"unknown message type: %T", msg))
	}
}

// handleWalletBoardingConfirmed processes a boarding UTXO confirmation event
// from the wallet actor. This creates the FSM event and drives the state
// machine forward. The wallet handles all persistence; we just react.
func (a *RoundClientActor) handleWalletBoardingConfirmed(ctx context.Context,
	msg *WalletBoardingConfirmed) fn.Result[ClientResp] {

	walletIntent := msg.Intent
	if walletIntent == nil {
		return fn.Err[ClientResp](fmt.Errorf(
			"wallet boarding confirmed event missing intent"))
	}

	a.log.InfoS(ctx, "Received boarding UTXO confirmation from wallet",
		btclog.Fmt("outpoint", "%v", walletIntent.Outpoint),
		slog.Int("amount", int(walletIntent.ChainInfo.Amount)),
		slog.Int("conf_height", int(walletIntent.ChainInfo.ConfHeight)))

	// Find an existing Idle round or create a new one.
	roundFSM := a.findIdleRound()
	if roundFSM == nil {
		var err error
		roundFSM, err = a.createNewRound(ctx)
		if err != nil {
			return fn.Err[ClientResp](fmt.Errorf(
				"failed to create round for boarding: %w", err))
		}
	}

	// Create the FSM event from the wallet's confirmed intent. Wallet only
	// notifies after min confs, so we set confirmations to 1. Include the
	// Address and TxProof for building the Request.
	confirmEvt := &BoardingUTXOConfirmed{
		Outpoint:      walletIntent.Outpoint,
		Address:       walletIntent.Address,
		BlockHeight:   walletIntent.ChainInfo.ConfHeight,
		BlockHash:     walletIntent.ChainInfo.ConfHash,
		Confirmations: int32(a.cfg.OperatorTerms.MinConfirmations),
		Tx:            walletIntent.ChainInfo.ConfTx,
		TxProof:       walletIntent.ChainInfo.TxProof,
	}

	// Drive the FSM with the confirmation event.
	err := a.askEventAndProcessOutbox(ctx, roundFSM.FSM, confirmEvt)
	if err != nil {
		return fn.Err[ClientResp](fmt.Errorf(
			"FSM error processing boarding confirmation: %w", err))
	}

	return fn.Ok[ClientResp](nil)
}

// handleVTXORequests processes client-submitted VTXO requests and forwards
// them to an idle round FSM. If no idle round exists, a new one is created.
func (a *RoundClientActor) handleVTXORequests(ctx context.Context,
	msg *RegisterVTXORequestsRequest) fn.Result[ClientResp] {

	if len(msg.Amounts) == 0 {
		return fn.Err[ClientResp](fmt.Errorf(
			"VTXO request amounts are empty",
		))
	}

	requests := make([]types.VTXORequest, 0, len(msg.Amounts))
	for i, amount := range msg.Amounts {
		if amount <= 0 {
			return fn.Err[ClientResp](fmt.Errorf(
				"VTXO amount %d is invalid: %v", i, amount,
			))
		}

		req, err := a.buildVTXORequest(ctx, amount)
		if err != nil {
			return fn.Err[ClientResp](fmt.Errorf(
				"build VTXO request %d: %w", i, err,
			))
		}

		requests = append(requests, *req)
	}

	a.log.InfoS(ctx, "Received VTXO requests",
		slog.Int("count", len(requests)))

	// Find an existing assembling round (Idle or PendingRoundAssembly) or
	// create a new one. This allows VTXOs to join a round that already has
	// boarding intents being assembled.
	roundFSM := a.findAssemblingRound()
	if roundFSM == nil {
		var err error
		roundFSM, err = a.createNewRound(ctx)
		if err != nil {
			return fn.Err[ClientResp](fmt.Errorf(
				"create new round for VTXO requests: %w", err,
			))
		}
	}

	event := &VTXORequestsReceived{
		Requests: requests,
	}

	err := a.askEventAndProcessOutbox(ctx, roundFSM.FSM, event)
	if err != nil {
		return fn.Err[ClientResp](fmt.Errorf(
			"FSM error processing VTXO requests: %w", err,
		))
	}

	return fn.Ok[ClientResp](&RegisterVTXORequestsResponse{
		Success: true,
	})
}

// buildVTXORequest derives a signing key and constructs a VTXO request for
// the provided amount.
func (a *RoundClientActor) buildVTXORequest(ctx context.Context,
	amount btcutil.Amount) (*types.VTXORequest, error) {

	keyDesc, err := a.cfg.Wallet.DeriveNextKey(
		ctx, keychain.KeyFamilyMultiSig,
	)
	if err != nil {
		return nil, fmt.Errorf("derive signing key: %w", err)
	}

	operatorKey := a.cfg.OperatorTerms.PubKey
	expiry := a.cfg.OperatorTerms.VTXOExitDelay
	desc, err := tree.NewVTXODescriptor(
		amount, keyDesc.PubKey, operatorKey, expiry,
	)
	if err != nil {
		return nil, fmt.Errorf("build VTXO descriptor for amount %v, "+
			"client %x, operator %x, expiry %d: %w",
			amount, keyDesc.PubKey.SerializeCompressed(),
			operatorKey.SerializeCompressed(), expiry, err)
	}

	return &types.VTXORequest{
		Amount:      amount,
		PkScript:    desc.PkScript,
		Expiry:      expiry,
		ClientKey:   keyDesc.PubKey,
		OperatorKey: operatorKey,
		SigningKey:  *keyDesc,
	}, nil
}

// handleRoundJoined handles the RoundJoined event which requires special
// re-keying logic. It matches the accepted outpoints to find the correct
// pending round, then re-keys the round from its TempRoundKey to the
// server-assigned RoundID.
func (a *RoundClientActor) handleRoundJoined(ctx context.Context,
	event *RoundJoined) fn.Result[ClientResp] {

	// Find the pending round by matching outpoints. Currently we only match
	// boarding outpoints, but this will be extended for VTXO operations
	// (forfeit, leave, refresh) when implemented.
	roundFSM := a.findRoundByOutpoints(
		event.AcceptedBoardingOutpoints,
		event.AcceptedVTXOOutpoints,
	)
	if roundFSM == nil {
		return fn.Err[ClientResp](fmt.Errorf(
			"no pending round matches: boarding=%v, vtxo=%v",
			event.AcceptedBoardingOutpoints,
			event.AcceptedVTXOOutpoints))
	}

	// Re-key: Remove old temp key, add with new RoundID.
	oldKeyStr := RoundKeyStr(roundFSM.Key.KeyString())
	delete(a.rounds, oldKeyStr)

	newKeyStr := RoundKeyStr(event.RoundID.KeyString())
	roundFSM.Key = event.RoundID
	roundFSM.RoundID = event.RoundID
	a.rounds[newKeyStr] = roundFSM

	a.log.InfoS(ctx, "Re-keyed round from temp to assigned",
		slog.String("old_key", string(oldKeyStr)),
		slog.String("round_id", event.RoundID.String()),
		slog.Int("num_boarding", len(event.AcceptedBoardingOutpoints)),
		slog.Int("num_vtxo", len(event.AcceptedVTXOOutpoints)))

	// Now process the event normally.
	err := a.askEventAndProcessOutbox(ctx, roundFSM.FSM, event)
	if err != nil {
		return fn.Err[ClientResp](fmt.Errorf(
			"FSM error processing RoundJoined: %w", err))
	}

	return fn.Ok[ClientResp](&ServerMessageResponse{Success: true})
}

// extractRoundID returns the RoundID from events that carry one. Returns the
// zero value for events without a RoundID field.
func extractRoundID(event ClientEvent) (RoundID, bool) {
	switch e := event.(type) {
	case *RoundJoined:
		return e.RoundID, true

	case *CommitmentTxBuilt:
		return e.RoundID, true

	case *NoncesAggregated:
		return e.RoundID, true

	case *OperatorSigned:
		return e.RoundID, true

	case *AwaitingBoardingSigs:
		return e.RoundID, true

	default:
		return RoundID{}, false
	}
}

// handleServerMessage processes a message from the server (delivered via
// Outbox). The actor routes the message to the appropriate FSM based on the
// event type and RoundID.
func (a *RoundClientActor) handleServerMessage(ctx context.Context,
	msg *ServerMessageNotification) fn.Result[ClientResp] {

	// RoundJoined requires special handling for re-keying.
	if joined, ok := msg.Message.(*RoundJoined); ok {
		return a.handleRoundJoined(ctx, joined)
	}

	// Try to route by RoundID first.
	roundID, hasRoundID := extractRoundID(msg.Message)

	var roundFSM *RoundFSM
	if hasRoundID {
		keyStr := RoundKeyStr(roundID.KeyString())
		var exists bool
		roundFSM, exists = a.rounds[keyStr]
		if !exists {
			return fn.Err[ClientResp](fmt.Errorf(
				"no round for ID: %s", roundID))
		}

		a.log.DebugS(ctx, "Routing server message by RoundID",
			slog.String("event_type", fmt.Sprintf("%T", msg.Message)),
			slog.String("round_id", roundID.String()))
	} else {
		// Events without RoundID (e.g., RegistrationRequested,
		// BoardingFailed) are routed to a pending (temp-keyed) round.
		// This supports events that arrive before the server assigns a
		// RoundID.
		roundFSM = a.findPendingRound()
		if roundFSM == nil {
			return fn.Err[ClientResp](fmt.Errorf(
				"no pending round for event %T", msg.Message))
		}

		a.log.DebugS(ctx, "Routing server message to pending round",
			slog.String("event_type", fmt.Sprintf("%T", msg.Message)),
			slog.String("key", roundFSM.Key.KeyString()))
	}

	err := a.askEventAndProcessOutbox(ctx, roundFSM.FSM, msg.Message)
	if err != nil {
		return fn.Err[ClientResp](fmt.Errorf(
			"FSM error processing server message: %w", err))
	}

	return fn.Ok[ClientResp](&ServerMessageResponse{
		Success: true,
	})
}

// findPendingRound returns a round with a temp key (not yet assigned a RoundID
// by the server). Returns nil if no pending rounds exist.
func (a *RoundClientActor) findPendingRound() *RoundFSM {
	for _, roundFSM := range a.rounds {
		if roundFSM.Key.IsTemp() {
			return roundFSM
		}
	}

	return nil
}

// handleGetState returns the current FSM state for monitoring/debugging.
// This includes all round FSMs (both temp-keyed and RoundID-keyed).
func (a *RoundClientActor) handleGetState(ctx context.Context,
	_ *GetClientStateRequest) fn.Result[ClientResp] {

	states := make(map[string]FSMStateInfo)

	for keyStr, roundFSM := range a.rounds {
		roundState, err := roundFSM.FSM.CurrentState()
		if err != nil {
			a.log.WarnS(ctx, "Failed to get FSM state for round", err,
				slog.String("key", string(keyStr)))

			continue
		}

		clientState, ok := roundState.(ClientState)
		if !ok {
			a.log.WarnS(ctx, "Round FSM state is not a ClientState", nil,
				slog.String("key", string(keyStr)),
				slog.String("state_type", fmt.Sprintf("%T", roundState)))

			continue
		}

		states[string(keyStr)] = FSMStateInfo{
			State:   clientState,
			IsTemp:  roundFSM.Key.IsTemp(),
			RoundID: roundFSM.RoundID,
		}
	}

	return fn.Ok[ClientResp](&GetClientStateResponse{
		States: states,
	})
}

// handleCancelRound attempts to cancel a pending round participation.
// If a RoundKey is specified in the request, that round is cancelled;
// otherwise, the first temp-keyed round is cancelled.
func (a *RoundClientActor) handleCancelRound(ctx context.Context,
	req *CancelRoundRequest) fn.Result[ClientResp] {

	a.log.InfoS(ctx, "Cancelling round participation by user request")

	// Find the round to cancel.
	var targetFSM *RoundFSM
	if req.RoundKey.IsSome() {
		// Cancel specific round by key.
		keyStr := req.RoundKey.UnsafeFromSome()
		var exists bool
		targetFSM, exists = a.rounds[keyStr]
		if !exists {
			return fn.Ok[ClientResp](&CancelRoundResponse{
				Success: false,
				Error:   fmt.Sprintf("no round with key: %s", keyStr),
			})
		}
	} else {
		// Cancel the first temp-keyed round.
		for _, roundFSM := range a.rounds {
			if roundFSM.Key.IsTemp() {
				targetFSM = roundFSM
				break
			}
		}
	}

	if targetFSM == nil {
		return fn.Ok[ClientResp](&CancelRoundResponse{
			Success: false,
			Error:   "no pending round to cancel",
		})
	}

	// Inject a BoardingFailed event to transition the FSM to failed state.
	// This will trigger any cleanup logic in the FSM transitions.
	cancelEvent := &BoardingFailed{
		Reason:      "User requested cancellation",
		Error:       fmt.Errorf("round cancelled by user"),
		Recoverable: true,
	}

	err := a.askEventAndProcessOutbox(ctx, targetFSM.FSM, cancelEvent)
	if err != nil {
		a.log.WarnS(ctx, "Failed to cancel round", err)

		return fn.Ok[ClientResp](&CancelRoundResponse{
			Success: false,
			Error:   fmt.Sprintf("failed to cancel: %v", err),
		})
	}

	// Remove the cancelled round from the map.
	keyStr := RoundKeyStr(targetFSM.Key.KeyString())
	delete(a.rounds, keyStr)

	a.log.InfoS(ctx, "Round participation cancelled successfully")

	return fn.Ok[ClientResp](&CancelRoundResponse{
		Success: true,
	})
}

// onRoundComplete is called when a round finishes successfully. This removes
// the round from active tracking and archives the round data.
func (a *RoundClientActor) onRoundComplete(ctx context.Context, roundID RoundID,
	txid chainhash.Hash, confInfo ConfInfo) error {

	a.log.InfoS(ctx, "Round completed successfully",
		slog.String("round_id", roundID.String()),
		slog.String("commitment_txid", txid.String()),
		slog.Int("conf_height", int(confInfo.Height)))

	keyStr := RoundKeyStr(roundID.KeyString())
	if roundFSM, exists := a.rounds[keyStr]; exists {
		roundFSM.FSM.Stop()
		delete(a.rounds, keyStr)
	}
	delete(a.commitmentTxIndex, txid)

	return a.cfg.RoundStore.FinalizeRound(ctx, roundID, txid, confInfo)
}

// handleConfirmation processes a commitment transaction confirmation event
// from ChainSource. Boarding address confirmations are now handled via
// WalletBoardingConfirmed events from the wallet actor.
//
// Concurrency: The actor framework serializes all messages through Receive(),
// so no synchronization is needed for rounds map access.
func (a *RoundClientActor) handleConfirmation(ctx context.Context,
	event *ConfirmationEvent) fn.Result[ClientResp] {

	a.log.InfoS(ctx, "Received commitment transaction confirmation",
		slog.String("txid", event.Txid.String()),
		slog.Int("block_height", int(event.BlockHeight)),
		slog.Int("confirmations", int(event.Confirmations)))

	// Look up the round by commitment transaction index.
	keyStr, exists := a.commitmentTxIndex[event.Txid]
	if !exists {
		// Not a commitment tx we're tracking. This shouldn't happen
		// since we only register for commitment tx confirmations.
		// Log for observability.
		a.log.WarnS(ctx, "Commitment tx not in index", nil,
			slog.String("txid", event.Txid.String()))

		return fn.Ok[ClientResp](nil)
	}

	// Route to the specific round's FSM.
	roundFSM, exists := a.rounds[keyStr]
	if !exists {
		return fn.Err[ClientResp](fmt.Errorf(
			"round FSM not found for key %s", keyStr))
	}

	a.log.InfoS(ctx, "Routing confirmation to round FSM",
		slog.String("key", string(keyStr)),
		slog.String("round_id", roundFSM.RoundID.String()))

	confirmEvt := &BoardingConfirmed{
		TxID:          event.Txid,
		BlockHeight:   event.BlockHeight,
		BlockHash:     event.BlockHash,
		Confirmations: int32(event.Confirmations),
	}

	err := a.askEventAndProcessOutbox(ctx, roundFSM.FSM, confirmEvt)
	if err != nil {
		return fn.Err[ClientResp](fmt.Errorf(
			"FSM error processing commitment confirmation: %w", err))
	}

	return fn.Ok[ClientResp](nil)
}

// processOutbox processes messages emitted by the FSM via Outbox and routes
// them to the appropriate destination (server or chainsource).
func (a *RoundClientActor) processOutbox(ctx context.Context,
	outbox []ClientOutMsg) error {

	for _, msg := range outbox {
		// Check if this message should be sent to the server. All
		// server-bound messages implement the ServerMessage interface.
		if serverMsg, ok := msg.(serverconn.ServerMessage); ok {
			sendReq := &serverconn.SendClientEventRequest{
				Message: serverMsg,
			}
			a.cfg.ServerConn.Tell(ctx, sendReq)

			continue
		}

		// Handle non-server messages.
		switch m := msg.(type) {
		case *RegisterConfirmationRequest:
			// FSM emitted a confirmation request. Complete it with
			// the NotifyActor field pointing to ourselves and send
			// to ChainSource.
			var sessionID string
			switch {
			case len(m.PkScript) > 0:
				sessionID = hex.EncodeToString(m.PkScript)

			case m.Txid != nil:
				sessionID = m.Txid.String()

			default:
				sessionID = "unknown"
			}
			callerID := fmt.Sprintf(
				"boarding-%s-%s", sessionID, m.CallerID,
			)

			// Use the shared mapper helper so ChainSource can
			// deliver confirmation events directly without an
			// intermediate actor.
			mappedRef := chainsource.MapConfirmationEvent(
				a.cfg.SelfRef,
				func(ce chainsource.ConfirmationEvent) ClientMsg {
					return &ConfirmationEvent{
						Txid:          ce.Txid,
						BlockHeight:   ce.BlockHeight,
						Confirmations: ce.NumConfs,
						Tx:            ce.Tx,
					}
				},
			)

			// Query ChainSource for current block height to use as
			// HeightHint. LND requires HeightHint > 0 for
			// confirmation scanning.
			heightHint := m.HeightHint
			if heightHint == 0 {
				heightFuture := a.cfg.ChainSource.Ask(
					ctx, &chainsource.BestHeightRequest{},
				)
				heightResult := heightFuture.Await(ctx)
				heightResp, err := heightResult.Unpack()
				if err != nil {
					return fmt.Errorf("get best height "+
						"for confirmation: %w", err)
				}
				bestHeightResp, ok := heightResp.(*chainsource.BestHeightResponse)
				if !ok {
					return fmt.Errorf("unexpected " +
						"height response type")
				}
				heightHint = uint32(bestHeightResp.Height)
			}

			// Build the complete RegisterConfRequest with the
			// mapper as the NotifyActor target.
			confReq := &chainsource.RegisterConfRequest{
				CallerID:    callerID,
				Txid:        m.Txid,
				PkScript:    m.PkScript,
				TargetConfs: m.TargetConfs,
				HeightHint:  heightHint,
				NotifyActor: fn.Some(mappedRef),
			}

			a.log.InfoS(ctx, "Sending RegisterConfRequest to ChainSource",
				slog.String("caller_id", callerID),
				slog.Int("pkscript_len", len(m.PkScript)),
				slog.Int("height_hint", int(heightHint)),
				slog.Int("target_confs", int(m.TargetConfs)))

			// Use a background context for the confirmation
			// registration. The ConfActor needs a long-lived context
			// that won't be cancelled when the current message
			// processing completes.
			a.cfg.ChainSource.Tell(context.Background(), confReq)

		case *VTXOCreatedNotification:
			// Forward to VTXO manager to spawn actors for the new
			// VTXOs if configured.
			if a.cfg.VTXOManager != nil {
				a.cfg.VTXOManager.Tell(ctx, m)
			}

		case *RoundCompletedNotification:
			a.log.InfoS(ctx, "Processing round completion notification",
				slog.String("round_id", m.RoundID.String()),
				slog.String("txid", m.TxID.String()))

			// Round FSM reached ConfirmedState. Perform actor
			// cleanup.
			err := a.onRoundComplete(
				ctx, m.RoundID, m.TxID, m.ConfInfo,
			)
			if err != nil {
				return fmt.Errorf("failed to complete "+
					"round %s: %w", m.RoundID, err)
			}

		case *RoundCheckpointedNotification:
			a.log.InfoS(ctx, "Processing round checkpoint notification",
				slog.String("round_id", m.RoundID.String()))

			// Find the round by its RoundID (should already be
			// re-keyed at this point).
			keyStr := RoundKeyStr(m.RoundID.KeyString())
			roundFSM, exists := a.rounds[keyStr]
			if !exists {
				return fmt.Errorf(
					"round not found for checkpoint: %s",
					m.RoundID)
			}

			// Get the current state to extract commitment tx info.
			state, err := roundFSM.FSM.CurrentState()
			if err != nil {
				return fmt.Errorf(
					"failed to get state: %w", err)
			}

			inputSigState, ok := state.(*InputSigSentState)
			if !ok {
				return fmt.Errorf("round not in "+
					"InputSigSentState, got %T", state)
			}

			// Update round FSM with commitment tx info.
			txid := inputSigState.CommitmentTx.UnsignedTx.TxHash()
			roundFSM.TxID = txid
			roundFSM.CommitmentTx = fn.Some(inputSigState.CommitmentTx)

			// Index for confirmation routing and register.
			a.commitmentTxIndex[txid] = keyStr
			a.registerCommitmentConfirmation(
				ctx, txid, roundFSM.CommitmentTx,
			)

			a.log.InfoS(ctx, "Round checkpoint processed",
				slog.String("round_id", m.RoundID.String()),
				slog.String("commitment_txid", txid.String()))

		case *RoundFailedNotification:
			// Round entered failed state. Log for observability.
			roundIDStr := "none"
			m.RoundID.WhenSome(func(id RoundID) {
				roundIDStr = id.String()
			})
			a.log.WarnS(ctx, "Round failed", nil,
				slog.String("round_id", roundIDStr),
				slog.String("reason", m.Reason),
				slog.Bool("recoverable", m.Recoverable))

		default:
			// Unknown outbox message type. Log for debugging.
			a.log.DebugS(ctx, "Ignoring unknown outbox message type",
				slog.String("type", fmt.Sprintf("%T", msg)),
			)
		}
	}

	return nil
}
