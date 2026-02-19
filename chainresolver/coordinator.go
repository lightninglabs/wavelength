package chainresolver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// CoordinatorConfig holds configuration for the chain resolver coordinator.
type CoordinatorConfig struct {
	// Store provides persistence for resolver state.
	Store ChainResolverStore

	// OORStore provides OOR artifact lookup for checkpoint resolution.
	OORStore *db.OORArtifactPersistenceStore

	// ChainSource provides chain interaction primitives (broadcast,
	// spend/conf watches, block subscriptions).
	ChainSource actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]

	// ActorSystem is used for spawning sub-actors if needed.
	ActorSystem *actor.ActorSystem

	// Logger is the structured logger for this coordinator.
	Logger btclog.Logger

	// Wallet provides PSBT funding and signing for constructing
	// CPFP child transactions. When set together with
	// PackageRelayer, the coordinator uses package relay for
	// transactions with P2A anchor outputs. When nil, all
	// transactions are broadcast individually via chainsource.
	Wallet CPFPWallet

	// PackageRelayer submits parent+child packages atomically via
	// the submitpackage RPC. When set together with Wallet, the
	// coordinator uses package relay for transactions with P2A
	// anchor outputs. When nil, all transactions are broadcast
	// individually via chainsource.
	PackageRelayer PackageRelayer
}

// Coordinator is the single long-lived actor that manages per-VTXO resolver
// FSMs. It receives inbound messages from the VTXO FSM, user RPCs, and
// chainsource watches, routes them to the appropriate resolver, and
// translates resolver outbox messages into chainsource actor calls.
type Coordinator struct {
	cfg *CoordinatorConfig

	// resolvers tracks active per-VTXO resolver FSMs by outpoint.
	resolvers map[wire.OutPoint]*resolverInstance

	// selfRef is the coordinator's own actor ref, used for creating
	// mapped refs that route chainsource events back to the coordinator.
	selfRef actor.TellOnlyRef[ChainResolverMsg]
}

// resolverInstance wraps a resolver FSM's current state and environment.
type resolverInstance struct {
	state ResolverState
	env   *ResolverEnvironment
}

// NewCoordinator creates a new chain resolver coordinator.
func NewCoordinator(cfg *CoordinatorConfig) *Coordinator {
	return &Coordinator{
		cfg:       cfg,
		resolvers: make(map[wire.OutPoint]*resolverInstance),
	}
}

// Start initializes the coordinator by recovering persisted resolvers and
// subscribing to block epochs.
func (c *Coordinator) Start(ctx context.Context,
	selfRef actor.TellOnlyRef[ChainResolverMsg]) error {

	c.selfRef = selfRef

	// Recover persisted resolvers.
	if c.cfg.Store != nil {
		if err := c.recoverResolvers(ctx); err != nil {
			return fmt.Errorf("recover resolvers: %w", err)
		}
	}

	// Subscribe to block epochs for CSV tracking.
	if err := c.subscribeBlockEpochs(ctx); err != nil {
		return fmt.Errorf("subscribe block epochs: %w", err)
	}

	c.cfg.Logger.InfoS(ctx, "Chain resolver coordinator started",
		slog.Int("recovered_resolvers", len(c.resolvers)))

	return nil
}

// Stop gracefully shuts down the coordinator.
func (c *Coordinator) Stop(ctx context.Context) {
	c.unsubscribeBlockEpochs(ctx)

	c.cfg.Logger.InfoS(ctx, "Chain resolver coordinator stopped")
}

// Receive processes incoming messages and routes them to the appropriate
// resolver FSM.
func (c *Coordinator) Receive(ctx context.Context,
	msg ChainResolverMsg) fn.Result[ChainResolverResp] {

	switch m := msg.(type) {
	case *ExpiringVTXORequest:
		return c.handleExpiringVTXO(ctx, m)

	case *UserUnrollRequest:
		return c.handleUserUnroll(ctx, m)

	case *FraudReactiveRequest:
		return c.handleFraudReactive(ctx, m)

	case *SpendDetectedEvent:
		return c.handleSpendDetected(ctx, m)

	case *ConfDetectedEvent:
		return c.handleConfDetected(ctx, m)

	case *BlockEpochEvent:
		return c.handleBlockEpoch(ctx, m)

	default:
		return fn.Err[ChainResolverResp](
			fmt.Errorf("unknown message: %T", msg),
		)
	}
}

// handleExpiringVTXO spawns a new resolver FSM for an expiring VTXO. The
// resolver starts in BroadcastingTreeState since we need to actively
// initiate the unroll.
func (c *Coordinator) handleExpiringVTXO(ctx context.Context,
	msg *ExpiringVTXORequest) fn.Result[ChainResolverResp] {

	outpoint := msg.VTXO.Outpoint

	if _, exists := c.resolvers[outpoint]; exists {
		c.cfg.Logger.WarnS(ctx,
			"Resolver already exists for expiring VTXO", nil,
			slog.String("outpoint", outpoint.String()))

		return fn.Ok[ChainResolverResp](&ExpiringVTXOResponse{})
	}

	// Resolve OOR packages if available.
	resolverCtx, err := c.buildResolverContext(ctx, msg.VTXO)
	if err != nil {
		c.cfg.Logger.ErrorS(ctx,
			"Failed to build resolver context", err,
			slog.String("outpoint", outpoint.String()))

		return fn.Err[ChainResolverResp](err)
	}

	treePath := resolverCtx.TreePath
	maxLevel := 0
	if treePath != nil {
		maxLevel = treePath.Depth() - 1
	}

	initialState := &BroadcastingTreeState{
		Outpoint:            outpoint,
		Trigger:             ResolveTriggerExpiry,
		CurrentLevel:        0,
		MaxLevel:            maxLevel,
		ConfirmedLevels:     0,
		AlreadyOnChainLevel: -1,
	}

	c.spawnResolver(ctx, outpoint, initialState, resolverCtx)

	// Drive the initial state transition.
	c.driveResolver(ctx, outpoint, &StartResolveEvent{
		Trigger: ResolveTriggerExpiry,
	})

	c.cfg.Logger.InfoS(ctx, "Spawned expiry resolver",
		slog.String("outpoint", outpoint.String()),
		slog.Int("blocks_remaining", int(msg.BlocksRemaining)))

	return fn.Ok[ChainResolverResp](&ExpiringVTXOResponse{})
}

// handleUserUnroll spawns a new resolver FSM for a user-initiated unroll.
func (c *Coordinator) handleUserUnroll(ctx context.Context,
	msg *UserUnrollRequest) fn.Result[ChainResolverResp] {

	outpoint := msg.VTXO.Outpoint

	if _, exists := c.resolvers[outpoint]; exists {
		c.cfg.Logger.WarnS(ctx,
			"Resolver already exists for user unroll", nil,
			slog.String("outpoint", outpoint.String()))

		return fn.Ok[ChainResolverResp](&UserUnrollResponse{})
	}

	resolverCtx, err := c.buildResolverContext(ctx, msg.VTXO)
	if err != nil {
		c.cfg.Logger.ErrorS(ctx,
			"Failed to build resolver context", err,
			slog.String("outpoint", outpoint.String()))

		return fn.Err[ChainResolverResp](err)
	}

	treePath := resolverCtx.TreePath
	maxLevel := 0
	if treePath != nil {
		maxLevel = treePath.Depth() - 1
	}

	initialState := &BroadcastingTreeState{
		Outpoint:            outpoint,
		Trigger:             ResolveTriggerUser,
		CurrentLevel:        0,
		MaxLevel:            maxLevel,
		ConfirmedLevels:     0,
		AlreadyOnChainLevel: -1,
	}

	c.spawnResolver(ctx, outpoint, initialState, resolverCtx)

	c.driveResolver(ctx, outpoint, &StartResolveEvent{
		Trigger: ResolveTriggerUser,
	})

	c.cfg.Logger.InfoS(ctx, "Spawned user unroll resolver",
		slog.String("outpoint", outpoint.String()))

	return fn.Ok[ChainResolverResp](&UserUnrollResponse{})
}

// handleFraudReactive spawns a new resolver FSM in WatchingCommitmentState
// for a fraud-reactive VTXO. The resolver watches the batch outpoint for
// spends by the counterparty (prior OOR sender), then broadcasts the
// remaining tree levels and checkpoint transactions.
func (c *Coordinator) handleFraudReactive(ctx context.Context,
	msg *FraudReactiveRequest) fn.Result[ChainResolverResp] {

	outpoint := msg.VTXO.Outpoint

	if _, exists := c.resolvers[outpoint]; exists {
		c.cfg.Logger.WarnS(ctx,
			"Resolver already exists for fraud reactive",
			nil,
			slog.String("outpoint", outpoint.String()))

		return fn.Ok[ChainResolverResp](
			&FraudReactiveResponse{},
		)
	}

	resolverCtx, err := c.buildResolverContext(ctx, msg.VTXO)
	if err != nil {
		c.cfg.Logger.ErrorS(ctx,
			"Failed to build resolver context", err,
			slog.String("outpoint", outpoint.String()))

		return fn.Err[ChainResolverResp](err)
	}

	initialState := &WatchingCommitmentState{
		Trigger:  ResolveTriggerFraudReactive,
		Outpoint: outpoint,
	}

	c.spawnResolver(ctx, outpoint, initialState, resolverCtx)

	// Register a spend watch on the batch outpoint so the
	// coordinator is notified when the counterparty broadcasts
	// the tree root transaction.
	treePath := resolverCtx.TreePath
	if treePath != nil {
		c.handleRegisterSpendWatch(ctx, outpoint,
			&RegisterSpendWatchOutMsg{
				CallerID: fmt.Sprintf(
					"resolver.%s.batch",
					outpoint.String(),
				),
				Outpoint: treePath.BatchOutpoint,
				PkScript: treePath.BatchOutput.PkScript,
			},
		)
	}

	c.cfg.Logger.InfoS(ctx, "Spawned fraud-reactive resolver",
		slog.String("outpoint", outpoint.String()))

	return fn.Ok[ChainResolverResp](&FraudReactiveResponse{})
}

// handleSpendDetected routes a spend event to the appropriate resolver.
func (c *Coordinator) handleSpendDetected(ctx context.Context,
	msg *SpendDetectedEvent) fn.Result[ChainResolverResp] {

	c.driveResolver(ctx, msg.ResolverID, &SpendDetectedResolverEvent{
		SpendingTx:     msg.SpendingTx,
		SpendingHeight: msg.SpendingHeight,
	})

	return fn.Ok[ChainResolverResp](&EventRoutedResponse{})
}

// handleConfDetected routes a confirmation event to the appropriate resolver.
func (c *Coordinator) handleConfDetected(ctx context.Context,
	msg *ConfDetectedEvent) fn.Result[ChainResolverResp] {

	resolver, exists := c.resolvers[msg.ResolverID]
	if !exists {
		c.cfg.Logger.DebugS(ctx,
			"Conf event for unknown resolver",
			slog.String("resolver_id", msg.ResolverID.String()))

		return fn.Ok[ChainResolverResp](&EventRoutedResponse{})
	}

	// Map the conf event to the appropriate resolver event based on
	// the current state.
	var resolverEvent ResolverEvent

	switch resolver.state.(type) {
	case *BroadcastingTreeState:
		resolverEvent = &TreeLevelConfirmedEvent{
			Txid:        msg.Txid,
			BlockHeight: msg.BlockHeight,
		}

	case *BroadcastingCheckpointsState:
		state := resolver.state.(*BroadcastingCheckpointsState)
		resolverEvent = &CheckpointConfirmedEvent{
			PackageIdx:  state.CurrentPackageIdx,
			BlockHeight: msg.BlockHeight,
		}

	default:
		c.cfg.Logger.DebugS(ctx,
			"Conf event in unexpected state",
			slog.String("resolver_id", msg.ResolverID.String()),
			slog.String("state", resolver.state.String()))

		return fn.Ok[ChainResolverResp](&EventRoutedResponse{})
	}

	c.driveResolver(ctx, msg.ResolverID, resolverEvent)

	return fn.Ok[ChainResolverResp](&EventRoutedResponse{})
}

// handleBlockEpoch broadcasts block epochs to all resolvers that are waiting
// for CSV delays.
func (c *Coordinator) handleBlockEpoch(ctx context.Context,
	msg *BlockEpochEvent) fn.Result[ChainResolverResp] {

	for outpoint, resolver := range c.resolvers {
		checkpointState, ok :=
			resolver.state.(*BroadcastingCheckpointsState)
		if !ok || !checkpointState.WaitingForCSV {
			continue
		}

		// Check if CSV delay has elapsed.
		csvTarget := checkpointState.LastConfHeight +
			int32(checkpointState.CSVDelay)

		if msg.Height >= csvTarget {
			c.driveResolver(ctx, outpoint, &CSVMaturedEvent{
				CurrentHeight: msg.Height,
			})
		}
	}

	return fn.Ok[ChainResolverResp](&EventRoutedResponse{})
}

// spawnResolver creates a new resolver instance and adds it to the active
// resolver map.
func (c *Coordinator) spawnResolver(ctx context.Context,
	outpoint wire.OutPoint, initialState ResolverState,
	resolverCtx *ResolverContext) {

	env := NewResolverEnvironment(
		fmt.Sprintf("resolver.%s", outpoint.String()),
		resolverCtx,
	)

	c.resolvers[outpoint] = &resolverInstance{
		state: initialState,
		env:   env,
	}

	c.cfg.Logger.DebugS(ctx, "Spawned resolver",
		slog.String("outpoint", outpoint.String()),
		slog.String("initial_state", initialState.String()))
}

// driveResolver processes an event through a resolver FSM and handles the
// resulting outbox messages.
func (c *Coordinator) driveResolver(ctx context.Context,
	outpoint wire.OutPoint, event ResolverEvent) {

	resolver, exists := c.resolvers[outpoint]
	if !exists {
		c.cfg.Logger.WarnS(ctx,
			"Event for unknown resolver", nil,
			slog.String("outpoint", outpoint.String()),
			slog.String("event", fmt.Sprintf("%T", event)))

		return
	}

	transition, err := resolver.state.ProcessEvent(
		ctx, event, resolver.env,
	)
	if err != nil {
		c.cfg.Logger.ErrorS(ctx, "Resolver FSM error", err,
			slog.String("outpoint", outpoint.String()),
			slog.String("state", resolver.state.String()),
			slog.String("event", fmt.Sprintf("%T", event)))

		return
	}

	// Update the resolver state.
	nextState, ok := transition.NextState.(ResolverState)
	if !ok {
		c.cfg.Logger.ErrorS(ctx,
			"Unexpected state type from resolver", nil,
			slog.String("outpoint", outpoint.String()),
			slog.String("type", fmt.Sprintf("%T",
				transition.NextState)))

		return
	}

	resolver.state = nextState

	// Process outbox messages.
	transition.NewEvents.WhenSome(func(emitted ResolverEmittedEvent) {
		c.processOutbox(ctx, outpoint, emitted.Outbox)
	})

	// If the resolver reached a terminal state, clean it up.
	if nextState.IsTerminal() {
		c.cfg.Logger.InfoS(ctx, "Resolver reached terminal state",
			slog.String("outpoint", outpoint.String()),
			slog.String("state", nextState.String()))

		delete(c.resolvers, outpoint)
	}
}

// processOutbox translates resolver outbox messages into chainsource actor
// calls, persistence operations, and other side effects.
func (c *Coordinator) processOutbox(ctx context.Context,
	resolverID wire.OutPoint, outbox []ResolverOutMsg) {

	for _, msg := range outbox {
		switch m := msg.(type) {
		case *BroadcastTxOutMsg:
			c.handleBroadcastTx(ctx, m)

		case *RegisterSpendWatchOutMsg:
			c.handleRegisterSpendWatch(ctx, resolverID, m)

		case *RegisterConfWatchOutMsg:
			c.handleRegisterConfWatch(ctx, resolverID, m)

		case *ResolverStatusUpdateOutMsg:
			c.handleStatusUpdate(ctx, m)

		case *ResolverCompletedOutMsg:
			c.handleResolverCompleted(ctx, m)
		}
	}
}

// handleBroadcastTx broadcasts a transaction via the chainsource actor.
// If the transaction contains a P2A anchor output and package relay is
// configured, it constructs a CPFP child and submits the parent+child
// as an atomic package instead.
func (c *Coordinator) handleBroadcastTx(ctx context.Context,
	msg *BroadcastTxOutMsg) {

	// Check if package relay is available and the tx has a P2A
	// anchor output.
	anchorIdx := findAnchorOutput(msg.Tx)
	if anchorIdx >= 0 && c.cfg.Wallet != nil &&
		c.cfg.PackageRelayer != nil {

		c.handlePackageBroadcast(ctx, msg)

		return
	}

	// Fall back to single-tx broadcast via chainsource.
	req := &chainsource.BroadcastTxRequest{
		Tx:    msg.Tx,
		Label: msg.Label,
	}

	future := c.cfg.ChainSource.Ask(ctx, req)
	result := future.Await(ctx)
	if result.IsErr() {
		c.cfg.Logger.WarnS(ctx,
			"Failed to broadcast transaction",
			result.Err(),
			slog.String("label", msg.Label))
	}
}

// defaultFeeConfTarget is the confirmation target used when estimating
// fee rates for CPFP child transactions.
const defaultFeeConfTarget = 6

// handlePackageBroadcast constructs a CPFP child transaction for a
// parent that has a P2A anchor output and submits the parent+child as
// an atomic package via the PackageRelayer.
func (c *Coordinator) handlePackageBroadcast(ctx context.Context,
	msg *BroadcastTxOutMsg) {

	// Estimate the current fee rate from the chainsource.
	feeRate, err := c.estimateFeeRate(ctx)
	if err != nil {
		c.cfg.Logger.WarnS(ctx,
			"Failed to estimate fee for package relay",
			err,
			slog.String("label", msg.Label))

		return
	}

	// Build the CPFP child transaction.
	childTx, err := buildCPFPChild(
		ctx, c.cfg.Wallet, msg.Tx, feeRate,
	)
	if err != nil {
		c.cfg.Logger.WarnS(ctx,
			"Failed to build CPFP child", err,
			slog.String("label", msg.Label))

		return
	}

	// Submit the parent+child package atomically.
	err = c.cfg.PackageRelayer.SubmitPackage(
		ctx, []*wire.MsgTx{msg.Tx}, childTx,
	)
	if err != nil {
		c.cfg.Logger.WarnS(ctx,
			"Failed to submit package", err,
			slog.String("label", msg.Label))

		return
	}

	c.cfg.Logger.InfoS(ctx, "Package relay broadcast succeeded",
		slog.String("label", msg.Label),
		slog.String("parent_txid", msg.Tx.TxHash().String()),
		slog.String("child_txid", childTx.TxHash().String()))
}

// estimateFeeRate queries the chainsource actor for the current fee
// rate. Returns the fee rate in sat/vByte.
func (c *Coordinator) estimateFeeRate(
	ctx context.Context) (btcutil.Amount, error) {

	req := &chainsource.FeeEstimateRequest{
		TargetConf: defaultFeeConfTarget,
	}

	future := c.cfg.ChainSource.Ask(ctx, req)
	result := future.Await(ctx)
	if result.IsErr() {
		return 0, fmt.Errorf("fee estimate: %w", result.Err())
	}

	resp, err := result.Unpack()
	if err != nil {
		return 0, fmt.Errorf("fee estimate unpack: %w", err)
	}

	feeResp, ok := resp.(*chainsource.FeeEstimateResponse)
	if !ok {
		return 0, fmt.Errorf("unexpected fee estimate response "+
			"type: %T", resp)
	}

	return feeResp.SatPerVByte, nil
}

// handleRegisterSpendWatch registers a spend watch with the chainsource
// actor and maps the resulting SpendEvent back to a SpendDetectedEvent
// for this resolver.
func (c *Coordinator) handleRegisterSpendWatch(ctx context.Context,
	resolverID wire.OutPoint, msg *RegisterSpendWatchOutMsg) {

	// Create a mapped ref that converts chainsource SpendEvent to our
	// SpendDetectedEvent with the resolver ID.
	spendRef := chainsource.MapSpendEvent(
		c.selfRef,
		func(evt chainsource.SpendEvent) ChainResolverMsg {
			return &SpendDetectedEvent{
				ResolverID:     resolverID,
				SpendingTx:     evt.SpendingTx,
				SpendingTxid:   evt.SpendingTxid,
				SpendingHeight: evt.SpendingHeight,
			}
		},
	)

	req := &chainsource.RegisterSpendRequest{
		CallerID:    msg.CallerID,
		Outpoint:    &msg.Outpoint,
		PkScript:    msg.PkScript,
		HeightHint:  msg.HeightHint,
		NotifyActor: fn.Some(spendRef),
	}

	future := c.cfg.ChainSource.Ask(ctx, req)
	result := future.Await(ctx)
	if result.IsErr() {
		c.cfg.Logger.WarnS(ctx,
			"Failed to register spend watch",
			result.Err(),
			slog.String("caller_id", msg.CallerID))
	}
}

// handleRegisterConfWatch registers a confirmation watch with the chainsource
// actor and maps the resulting ConfirmationEvent back to a ConfDetectedEvent
// for this resolver.
func (c *Coordinator) handleRegisterConfWatch(ctx context.Context,
	resolverID wire.OutPoint, msg *RegisterConfWatchOutMsg) {

	// Create a mapped ref that converts chainsource ConfirmationEvent to
	// our ConfDetectedEvent with the resolver ID.
	confRef := chainsource.MapConfirmationEvent(
		c.selfRef,
		func(evt chainsource.ConfirmationEvent) ChainResolverMsg {
			return &ConfDetectedEvent{
				ResolverID:  resolverID,
				Txid:        evt.Txid,
				BlockHeight: evt.BlockHeight,
				Tx:          evt.Tx,
			}
		},
	)

	txid := msg.Txid
	req := &chainsource.RegisterConfRequest{
		CallerID:    msg.CallerID,
		Txid:        &txid,
		PkScript:    msg.PkScript,
		TargetConfs: msg.TargetConfs,
		HeightHint:  msg.HeightHint,
		NotifyActor: fn.Some(confRef),
	}

	future := c.cfg.ChainSource.Ask(ctx, req)
	result := future.Await(ctx)
	if result.IsErr() {
		c.cfg.Logger.WarnS(ctx,
			"Failed to register conf watch",
			result.Err(),
			slog.String("caller_id", msg.CallerID))
	}
}

// handleStatusUpdate persists the resolver state for crash recovery.
func (c *Coordinator) handleStatusUpdate(ctx context.Context,
	msg *ResolverStatusUpdateOutMsg) {

	if c.cfg.Store == nil {
		return
	}

	err := c.cfg.Store.SaveResolverState(
		ctx, msg.Outpoint, msg.StateName, msg.StateDetails,
	)
	if err != nil {
		c.cfg.Logger.ErrorS(ctx,
			"Failed to persist resolver state", err,
			slog.String("outpoint", msg.Outpoint.String()),
			slog.String("state", msg.StateName))
	}
}

// handleResolverCompleted cleans up a completed resolver's persisted state.
func (c *Coordinator) handleResolverCompleted(ctx context.Context,
	msg *ResolverCompletedOutMsg) {

	if c.cfg.Store == nil {
		return
	}

	err := c.cfg.Store.DeleteResolverState(ctx, msg.Outpoint)
	if err != nil {
		c.cfg.Logger.ErrorS(ctx,
			"Failed to delete resolver state", err,
			slog.String("outpoint", msg.Outpoint.String()))
	}

	c.cfg.Logger.InfoS(ctx, "Resolver completed",
		slog.String("outpoint", msg.Outpoint.String()),
		slog.Bool("success", msg.Success),
		slog.String("reason", msg.Reason))
}

// buildResolverContext constructs the immutable resolver context from a VTXO
// descriptor by looking up the tree path and OOR packages.
func (c *Coordinator) buildResolverContext(ctx context.Context,
	vtxoDesc *vtxo.Descriptor) (*ResolverContext, error) {

	resolverCtx := &ResolverContext{
		VTXO:     vtxoDesc,
		TreePath: vtxoDesc.TreePath,
	}

	// Look up OOR packages if the OOR store is available.
	if c.cfg.OORStore != nil {
		oorPkgs, err := c.cfg.OORStore.ResolveUnrollPackages(
			ctx, vtxoDesc.Outpoint,
		)
		if err != nil {
			// OOR package lookup failure is not fatal; the VTXO
			// may not have OOR packages.
			c.cfg.Logger.DebugS(ctx,
				"No OOR packages found for VTXO",
				slog.String(
					"outpoint",
					vtxoDesc.Outpoint.String(),
				),
				slog.String("error", err.Error()))
		} else {
			resolverCtx.OORPackages = oorPkgs
		}
	}

	return resolverCtx, nil
}

// recoverResolvers loads persisted resolver states from the database and
// reconstructs the FSMs.
func (c *Coordinator) recoverResolvers(ctx context.Context) error {
	outpoints, err := c.cfg.Store.ListActiveResolvers(ctx)
	if err != nil {
		return fmt.Errorf("list active resolvers: %w", err)
	}

	for _, outpoint := range outpoints {
		stateName, details, err := c.cfg.Store.GetResolverState(
			ctx, outpoint,
		)
		if err != nil {
			c.cfg.Logger.ErrorS(ctx,
				"Failed to load resolver state", err,
				slog.String("outpoint", outpoint.String()))

			continue
		}

		state, err := deserializeResolverState(
			outpoint, stateName, details,
		)
		if err != nil {
			c.cfg.Logger.ErrorS(ctx,
				"Failed to deserialize resolver state", err,
				slog.String("outpoint", outpoint.String()),
				slog.String("state", stateName))

			continue
		}

		// We need the VTXO descriptor to build the resolver context,
		// but for recovery we'd need a VTXO store. For now, create a
		// minimal context. The full integration will wire this up.
		env := NewResolverEnvironment(
			fmt.Sprintf("resolver.%s", outpoint.String()),
			&ResolverContext{},
		)

		c.resolvers[outpoint] = &resolverInstance{
			state: state,
			env:   env,
		}

		c.cfg.Logger.InfoS(ctx, "Recovered resolver",
			slog.String("outpoint", outpoint.String()),
			slog.String("state", stateName))
	}

	return nil
}

// deserializeResolverState reconstructs a resolver state from its persisted
// state name and details.
func deserializeResolverState(outpoint wire.OutPoint, stateName string,
	details []byte) (ResolverState, error) {

	switch stateName {
	case "broadcasting_tree":
		var state BroadcastingTreeState
		if err := json.Unmarshal(details, &state); err != nil {
			return nil, fmt.Errorf(
				"unmarshal broadcasting_tree: %w", err,
			)
		}

		return &state, nil

	case "broadcasting_checkpoints":
		var state BroadcastingCheckpointsState
		if err := json.Unmarshal(details, &state); err != nil {
			return nil, fmt.Errorf(
				"unmarshal broadcasting_checkpoints: %w", err,
			)
		}

		return &state, nil

	case "watching_commitment":
		return &WatchingCommitmentState{
			Trigger:  ResolveTriggerFraudReactive,
			Outpoint: outpoint,
		}, nil

	case "resolved":
		return &ResolvedState{Outpoint: outpoint}, nil

	case "failed":
		return &FailedState{
			Outpoint: outpoint,
			Reason:   "recovered from storage",
		}, nil

	default:
		return nil, fmt.Errorf("unknown state: %s", stateName)
	}
}

// subscribeBlockEpochs registers for block notifications with chainsource.
func (c *Coordinator) subscribeBlockEpochs(ctx context.Context) error {
	epochRef := chainsource.MapBlockEpoch(
		c.selfRef,
		func(epoch chainsource.BlockEpoch) ChainResolverMsg {
			return &BlockEpochEvent{
				Height:    epoch.Height,
				Hash:      epoch.Hash,
				Timestamp: epoch.Timestamp,
			}
		},
	)

	req := &chainsource.SubscribeBlocksRequest{
		CallerID:    "chain-resolver",
		NotifyActor: fn.Some(epochRef),
	}

	future := c.cfg.ChainSource.Ask(ctx, req)
	result := future.Await(ctx)
	if result.IsErr() {
		return fmt.Errorf("subscribe blocks: %w", result.Err())
	}

	return nil
}

// unsubscribeBlockEpochs cancels the block epoch subscription.
func (c *Coordinator) unsubscribeBlockEpochs(ctx context.Context) {
	err := c.cfg.ChainSource.Tell(
		ctx, &chainsource.UnsubscribeBlocksRequest{
			CallerID: "chain-resolver",
		},
	)
	if err != nil {
		c.cfg.Logger.WarnS(ctx,
			"Failed to unsubscribe from blocks", err)
	}
}
