package batchsweeper

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/timeout"
	"github.com/lightninglabs/darepo/batchwatcher"
	"github.com/lightninglabs/darepo/ledger"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

const (
	// estimatedBlockInterval is the assumed time between blocks used for
	// scheduling retries when outputs are not yet CSV-mature. This is only
	// a heuristic; actual retry cadence is capped by MaxRetryDelay.
	estimatedBlockInterval = 10 * time.Minute

	// defaultFeeTarget is the default confirmation target (in blocks) used
	// for fee estimation when not explicitly configured. 6 blocks provides
	// a reasonable balance between confirmation speed and fee cost.
	defaultFeeTarget = 6

	// defaultAlertThreshold is the number of failed sweep attempts before
	// an alert is logged at ErrorS level. This alerts operators to
	// investigate persistent sweep failures.
	defaultAlertThreshold = 10

	// defaultAlertRepeatInterval is the number of attempts between
	// repeated alerts after the initial alert threshold is reached.
	defaultAlertRepeatInterval = 100

	// defaultInitialRetryDelay is the starting delay for exponential
	// backoff when retrying sweep broadcasts via timer (used when block
	// subscription is not yet active).
	defaultInitialRetryDelay = time.Second

	// defaultMaxRetryDelay caps the exponential backoff to prevent
	// excessively long waits between retry attempts.
	defaultMaxRetryDelay = 5 * time.Minute

	// defaultSweepConfirmations is the number of confirmations required
	// before considering a sweep transaction confirmed.
	defaultSweepConfirmations = 3
)

// ActorConfig contains the configuration for creating a new BatchSweeperActor.
type ActorConfig struct {
	// Log is an optional logger. When None, logging is disabled.
	Log fn.Option[btclog.Logger]

	// BatchWatcher is a reference to the BatchWatcher actor for querying
	// tree state when building sweep transactions.
	BatchWatcher actor.ActorRef[
		batchwatcher.BatchWatcherMsg, batchwatcher.BatchWatcherResp,
	]

	// ChainSource is a reference to the ChainSource actor for chain tip
	// queries, fee estimates, and transaction broadcast.
	ChainSource actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]

	// SweepKey is the key descriptor for the operator's sweep key. This
	// key signs the unilateral CSV sweep leaf committed to in batch and
	// branch outputs.
	SweepKey keychain.KeyDescriptor

	// SweepDelay is the CSV delay (in blocks) used in the operator sweep
	// path.
	SweepDelay uint32

	// Signer signs sweep inputs. This is expected to be backed by the
	// operator wallet.
	Signer input.Signer

	// NewSweepPkScript returns a fresh destination script for a sweep
	// output. Each successful broadcast consumes the address, so the
	// next sweep will request a new one. The sweeper caches the script
	// until it is used in a broadcast to avoid unnecessary address
	// generation on retries.
	NewSweepPkScript func(ctx context.Context) ([]byte, error)

	// FeeTarget is the confirmation target used for fee estimation.
	FeeTarget uint32

	// BuildSweepTx builds and signs sweep transactions. When unset, the
	// actor uses the default builder based on the other sweep-related
	// configuration fields.
	BuildSweepTx SweepTxBuilder

	// TimeoutActor optionally schedules retries for sweep attempts when
	// block subscription is not yet active.
	TimeoutActor fn.Option[actor.TellOnlyRef[timeout.Msg]]

	// AlertThreshold is the number of failed sweep attempts before an
	// alert is logged at ErrorS level.
	AlertThreshold uint32

	// AlertRepeatInterval is the number of attempts between repeated
	// alerts after the initial threshold is reached.
	AlertRepeatInterval uint32

	// InitialRetryDelay is the initial delay used for sweep retries
	// when block subscription is not yet active.
	InitialRetryDelay time.Duration

	// MaxRetryDelay is the maximum delay used for sweep retries.
	MaxRetryDelay time.Duration

	// SweepConfirmations is the number of confirmations required before
	// considering a sweep transaction confirmed.
	SweepConfirmations uint32

	// OnBatchSwept is an optional callback invoked when the watcher
	// notifies that a batch has been fully swept. It receives the
	// VTXO outpoints extracted from the tree so the server can mark
	// them as expired.
	OnBatchSwept func(ctx context.Context,
		vtxoOutpoints []wire.OutPoint) error

	// SelfRef is a reference to this actor for receiving mapped
	// notifications and internal timer callbacks.
	SelfRef actor.TellOnlyRef[Msg]

	// LedgerRef is an optional reference to the ledger actor
	// for recording capital reclamation events when sweeps
	// confirm.
	LedgerRef fn.Option[actor.TellOnlyRef[ledger.LedgerMsg]]
}

// SweepTxBuilder constructs a sweep transaction spending the provided
// candidates at the given fee rate.
type SweepTxBuilder func(candidates []*batchwatcher.Output,
	feeRate btcutil.Amount) (*wire.MsgTx, error)

// expiredBatch tracks expiry and retry state for a batch.
type expiredBatch struct {
	expiryHeight uint32
	attempts     uint32
	lastError    error
}

// pendingSweep tracks a broadcast sweep that is awaiting confirmation.
type pendingSweep struct {
	txid        chainhash.Hash
	batchID     batchwatcher.BatchID
	broadcastAt time.Time
	feeRate     btcutil.Amount
	numInputs   int
	sweepAmount int64
}

// retryableError is returned by trySweep when the operation should be retried.
// This is used to distinguish expected "not yet possible" conditions from
// internal or unexpected failures.
type retryableError struct {
	err          error
	delayHint    time.Duration
	countAttempt bool
}

// Error returns the error string.
func (e *retryableError) Error() string {
	return e.err.Error()
}

// Unwrap returns the underlying error.
func (e *retryableError) Unwrap() error {
	return e.err
}

// newRetryableError wraps an error as retryable and optionally provides a
// delay hint for scheduling.
func newRetryableError(err error, delayHint time.Duration,
	countAttempt bool) error {

	return &retryableError{
		err:          err,
		delayHint:    delayHint,
		countAttempt: countAttempt,
	}
}

// Actor is the BatchSweeperActor that builds and broadcasts sweep transactions
// for operator-controlled outputs once a batch expires.
type Actor struct {
	cfg *ActorConfig
	log btclog.Logger

	// expired tracks batches that have reached expiry and are eligible for
	// sweeping.
	expired map[batchwatcher.BatchID]*expiredBatch

	// pendingSweeps tracks broadcast sweep transactions awaiting
	// confirmation, keyed by batch ID.
	pendingSweeps map[batchwatcher.BatchID]*pendingSweep

	// cachedSweepPkScript holds a pre-generated sweep destination
	// script so retries reuse the same address. Cleared after a
	// successful broadcast so the next sweep gets a fresh address.
	cachedSweepPkScript []byte
}

// NewActor creates a new BatchSweeperActor with the provided configuration.
func NewActor(cfg *ActorConfig) *Actor {
	if cfg.FeeTarget == 0 {
		cfg.FeeTarget = defaultFeeTarget
	}

	if cfg.AlertThreshold == 0 {
		cfg.AlertThreshold = defaultAlertThreshold
	}

	if cfg.AlertRepeatInterval == 0 {
		cfg.AlertRepeatInterval = defaultAlertRepeatInterval
	}

	if cfg.InitialRetryDelay == 0 {
		cfg.InitialRetryDelay = defaultInitialRetryDelay
	}

	if cfg.MaxRetryDelay == 0 {
		cfg.MaxRetryDelay = defaultMaxRetryDelay
	}

	if cfg.SweepConfirmations == 0 {
		cfg.SweepConfirmations = defaultSweepConfirmations
	}

	return &Actor{
		cfg: cfg,
		log: cfg.Log.UnwrapOr(btclog.Disabled),
		expired: make(
			map[batchwatcher.BatchID]*expiredBatch,
		),
		pendingSweeps: make(
			map[batchwatcher.BatchID]*pendingSweep,
		),
	}
}

// Receive processes incoming messages for the BatchSweeperActor.
func (a *Actor) Receive(ctx context.Context, msg Msg) fn.Result[Resp] {
	switch m := msg.(type) {
	case *BatchExpiredEvent:
		return a.handleBatchExpired(ctx, m)

	case *TreeStateChangedEvent:
		return a.handleTreeStateChanged(ctx, m)

	case *SweepRetryEvent:
		return a.handleSweepRetry(ctx, m)

	case *SweepConfirmedEvent:
		return a.handleSweepConfirmed(ctx, m)

	case *BatchSweptEvent:
		return a.handleBatchSwept(ctx, m)

	default:
		return fn.Err[Resp](fmt.Errorf("unknown message type: %T", m))
	}
}

// handleBatchExpired processes a batch expiry notification. This is called
// both when a batch first expires and on subsequent blocks (as a retry
// trigger from BatchWatcher). The handler preserves attempt counts for
// existing batches. For batches with pending sweeps, it checks if the
// current fee rate is higher and rebroadcasts with the bumped fee if so.
func (a *Actor) handleBatchExpired(ctx context.Context,
	msg *BatchExpiredEvent) fn.Result[Resp] {

	if msg.Notification == nil {
		return fn.Err[Resp](fmt.Errorf("nil batch expiry notification"))
	}

	batchID := msg.Notification.BatchID

	// Only log and initialize tracking for newly expired batches.
	// Re-notifications from BatchWatcher preserve existing state.
	if _, alreadyExpired := a.expired[batchID]; !alreadyExpired {
		a.log.InfoS(ctx, "Batch expired",
			"batch_id", batchID,
			"expiry_height", msg.Notification.ExpiryHeight)

		a.expired[batchID] = &expiredBatch{
			expiryHeight: msg.Notification.ExpiryHeight,
			attempts:     0,
		}
	}

	// If there's a pending sweep, check if we should bump the fee.
	if pending, hasPending := a.pendingSweeps[batchID]; hasPending {
		shouldBump, err := a.shouldBumpFee(ctx, pending)
		if err != nil {
			a.log.DebugS(ctx, "Fee rate query failed for bump check",
				err, "batch_id", batchID)

			return fn.Ok[Resp](nil)
		}

		if !shouldBump {
			return fn.Ok[Resp](nil)
		}

		a.log.DebugS(ctx, "Bumping fee for pending sweep",
			"batch_id", batchID,
			"old_fee_rate", pending.feeRate)
	}

	err := a.trySweep(ctx, batchID)
	if err != nil {
		a.handleSweepAttemptError(ctx, batchID, err)
	}

	return fn.Ok[Resp](nil)
}

// shouldBumpFee checks if the current fee rate is higher than the pending
// sweep's fee rate, indicating we should rebroadcast with the higher fee.
func (a *Actor) shouldBumpFee(ctx context.Context,
	pending *pendingSweep) (bool, error) {

	currentFeeRate, err := a.queryFeeRate(ctx)
	if err != nil {
		return false, err
	}

	return currentFeeRate > pending.feeRate, nil
}

// handleTreeStateChanged processes a tree state change notification.
// When the BatchWatcher detects tree unrolls (presigned node spends), it
// notifies us so we can attempt sweeping newly available outputs. This is
// important because:
//  1. Initial sweep may have failed if no outputs were CSV-mature yet.
//  2. Tree unrolls create new operator-controlled outputs that become
//     sweepable after the CSV delay.
//  3. We only attempt sweeps for batches we're already tracking (expired).
func (a *Actor) handleTreeStateChanged(ctx context.Context,
	msg *TreeStateChangedEvent) fn.Result[Resp] {

	if msg.Notification == nil {
		return fn.Err[Resp](fmt.Errorf("nil tree state notification"))
	}

	a.log.TraceS(ctx, "Tree state changed",
		"batch_id", msg.Notification.BatchID)

	// Only attempt sweeping for batches we're already tracking. Batches
	// are added to expired map when they reach expiry height.
	if _, ok := a.expired[msg.Notification.BatchID]; !ok {
		return fn.Ok[Resp](nil)
	}

	err := a.trySweep(ctx, msg.Notification.BatchID)
	if err != nil {
		a.handleSweepAttemptError(ctx, msg.Notification.BatchID, err)
	}

	return fn.Ok[Resp](nil)
}

// handleSweepRetry processes a retry trigger and attempts sweeping again.
func (a *Actor) handleSweepRetry(ctx context.Context,
	msg *SweepRetryEvent) fn.Result[Resp] {

	a.log.DebugS(ctx, "Sweep retry triggered",
		"batch_id", msg.BatchID)

	err := a.trySweep(ctx, msg.BatchID)
	if err != nil {
		a.handleSweepAttemptError(ctx, msg.BatchID, err)
	}

	return fn.Ok[Resp](nil)
}

// handleSweepAttemptError logs a sweep attempt error, schedules a retry, and
// emits alerts when failure count exceeds thresholds.
func (a *Actor) handleSweepAttemptError(ctx context.Context,
	batchID batchwatcher.BatchID, err error) {

	var (
		delayHint    time.Duration
		countAttempt = true
	)

	var retryable *retryableError
	if errors.As(err, &retryable) {
		delayHint = retryable.delayHint
		countAttempt = retryable.countAttempt

		a.log.DebugS(ctx, "Sweep attempt needs retry", retryable.Unwrap(),
			"batch_id", batchID,
			"delay_hint", delayHint)
	} else {
		a.log.WarnS(ctx, "Sweep attempt failed", err,
			"batch_id", batchID)
	}

	// Store the last error for alerting context.
	if state, ok := a.expired[batchID]; ok {
		state.lastError = err
	}

	a.scheduleRetry(ctx, batchID, delayHint, countAttempt)

	// Check if we need to emit an alert after incrementing the attempt
	// count. Alert at initial threshold and then at regular intervals.
	a.maybeAlert(ctx, batchID)
}

// maybeAlert logs an ErrorS alert if the batch has exceeded the alert
// threshold for sweep failures. Alerts are emitted at the initial threshold
// and then repeated at configured intervals.
func (a *Actor) maybeAlert(ctx context.Context, batchID batchwatcher.BatchID) {
	state, ok := a.expired[batchID]
	if !ok {
		return
	}

	// Don't alert if below threshold.
	if state.attempts < a.cfg.AlertThreshold {
		return
	}

	// Alert at the initial threshold.
	if state.attempts == a.cfg.AlertThreshold {
		a.log.ErrorS(ctx, "Sweep failures exceeded alert threshold",
			state.lastError,
			"batch_id", batchID,
			"attempts", state.attempts,
			"expiry_height", state.expiryHeight)

		return
	}

	// After initial threshold, alert at regular intervals.
	attemptsSinceThreshold := state.attempts - a.cfg.AlertThreshold
	if attemptsSinceThreshold%a.cfg.AlertRepeatInterval == 0 {
		a.log.ErrorS(ctx, "Sweep failures continue",
			state.lastError,
			"batch_id", batchID,
			"attempts", state.attempts,
			"expiry_height", state.expiryHeight)
	}
}

// trySweep attempts to build, sign, and broadcast a sweep transaction for a
// batch. It returns an error if a retry should be scheduled.
func (a *Actor) trySweep(ctx context.Context,
	batchID batchwatcher.BatchID) error {

	treeState, err := a.queryTreeState(ctx, batchID)
	if err != nil {
		return err
	}

	if treeState == nil {
		a.log.DebugS(ctx, "Batch not found in watcher",
			"batch_id", batchID)

		return nil
	}

	bestHeight, err := a.queryBestHeight(ctx)
	if err != nil {
		return err
	}

	feeRate, err := a.queryFeeRate(ctx)
	if err != nil {
		return err
	}

	candidates := selectSweepCandidates(
		treeState, uint32(bestHeight), a.cfg.SweepDelay,
	)
	if len(candidates) == 0 {
		nextHeight, found, err := nextSweepMaturityHeight(
			treeState, uint32(bestHeight), a.cfg.SweepDelay,
		)
		if err != nil {
			return err
		}

		if !found {
			a.log.DebugS(ctx, "No sweep candidates found",
				"batch_id", batchID,
				"best_height", bestHeight)

			return nil
		}

		blocksRemaining := nextHeight - uint32(bestHeight)

		a.log.DebugS(ctx, "Sweep candidates not yet CSV-mature",
			"batch_id", batchID,
			"next_maturity_height", nextHeight,
			"blocks_remaining", blocksRemaining)
		delayHint, overflow := blocksToDuration(
			blocksRemaining, estimatedBlockInterval,
		)
		if overflow {
			delayHint = a.cfg.MaxRetryDelay
		}

		return newRetryableError(
			fmt.Errorf("no sweep candidates are CSV-mature yet"),
			delayHint, false,
		)
	}

	builder := a.cfg.BuildSweepTx
	if builder == nil {
		// Lazily generate a sweep destination, caching it so
		// retries reuse the same address.
		sweepPkScript, err := a.sweepPkScript(ctx)
		if err != nil {
			return err
		}

		builder = func(candidates []*batchwatcher.Output,
			feeRate btcutil.Amount) (*wire.MsgTx, error) {

			return buildSignedSweepTx(
				candidates, a.cfg.SweepKey,
				a.cfg.SweepDelay, sweepPkScript,
				feeRate, a.cfg.Signer,
			)
		}
	}

	sweepTx, err := builder(candidates, feeRate)
	if err != nil {
		return err
	}

	broadcastReq := &chainsource.BroadcastTxRequest{
		Tx:    sweepTx,
		Label: fmt.Sprintf("batch-sweep-%s", batchID),
	}

	broadcastResult := a.cfg.ChainSource.Ask(ctx, broadcastReq).Await(ctx)
	if _, err := broadcastResult.Unpack(); err != nil {
		return newRetryableError(
			fmt.Errorf("broadcast failed: %w", err), 0, true,
		)
	}

	txid := sweepTx.TxHash()

	a.log.InfoS(ctx, "Broadcast batch sweep transaction",
		"batch_id", batchID,
		"txid", txid,
		"num_inputs", len(candidates),
		"fee_rate_sat_vb", feeRate)

	// The address was consumed by a successful broadcast, so clear
	// the cache so the next sweep gets a fresh destination.
	a.cachedSweepPkScript = nil

	// Compute the total value of swept inputs for ledger
	// tracking.
	var sweepAmount int64
	for _, c := range candidates {
		sweepAmount += c.TxOut.Value
	}

	// Track this pending sweep and register for confirmation
	// notification.
	a.pendingSweeps[batchID] = &pendingSweep{
		txid:        txid,
		batchID:     batchID,
		broadcastAt: time.Now(),
		feeRate:     feeRate,
		numInputs:   len(candidates),
		sweepAmount: sweepAmount,
	}

	err = a.registerSweepConfirmation(
		ctx, batchID, &txid, sweepTx.TxOut[0].PkScript,
		uint32(bestHeight),
	)
	if err != nil {
		return newRetryableError(err, 0, true)
	}

	return nil
}

// sweepPkScript returns a cached sweep destination script, generating a
// fresh one via NewSweepPkScript if the cache is empty. The cache is
// cleared after a successful broadcast so the next sweep gets a new
// address.
func (a *Actor) sweepPkScript(ctx context.Context) ([]byte, error) {
	if len(a.cachedSweepPkScript) > 0 {
		return a.cachedSweepPkScript, nil
	}

	if a.cfg.NewSweepPkScript == nil {
		return nil, fmt.Errorf("NewSweepPkScript not configured")
	}

	pkScript, err := a.cfg.NewSweepPkScript(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate sweep pkscript: %w",
			err)
	}

	a.cachedSweepPkScript = pkScript

	return pkScript, nil
}

// queryTreeState retrieves the current tree state for a batch from the
// BatchWatcher.
func (a *Actor) queryTreeState(ctx context.Context,
	batchID batchwatcher.BatchID) (*batchwatcher.BatchTreeState, error) {

	req := &batchwatcher.GetTreeStateRequest{
		BatchID: batchID,
	}

	result := a.cfg.BatchWatcher.Ask(ctx, req).Await(ctx)
	respVal, err := result.Unpack()
	if err != nil {
		return nil, fmt.Errorf("tree state query failed: %w", err)
	}

	treeResp, ok := respVal.(*batchwatcher.GetTreeStateResponse)
	if !ok {
		return nil, fmt.Errorf("unexpected response type: %T", respVal)
	}

	if !treeResp.Found {
		return nil, nil
	}

	return treeResp.TreeState, nil
}

// queryBestHeight queries the current best block height from ChainSource.
// Returns an error if the height is invalid (negative or zero).
func (a *Actor) queryBestHeight(ctx context.Context) (int32, error) {
	req := &chainsource.BestHeightRequest{}
	result := a.cfg.ChainSource.Ask(ctx, req).Await(ctx)
	respVal, err := result.Unpack()
	if err != nil {
		return 0, fmt.Errorf("best height query failed: %w", err)
	}

	bestResp, ok := respVal.(*chainsource.BestHeightResponse)
	if !ok {
		return 0, fmt.Errorf("unexpected response type: %T", respVal)
	}

	if bestResp.Height < 0 {
		return 0, fmt.Errorf("unexpected negative best height: %d",
			bestResp.Height)
	}

	return bestResp.Height, nil
}

// queryFeeRate queries an estimated fee rate from ChainSource.
func (a *Actor) queryFeeRate(ctx context.Context) (btcutil.Amount, error) {
	req := &chainsource.FeeEstimateRequest{
		TargetConf: a.cfg.FeeTarget,
	}

	result := a.cfg.ChainSource.Ask(ctx, req).Await(ctx)
	respVal, err := result.Unpack()
	if err != nil {
		return 0, fmt.Errorf("fee estimate query failed: %w", err)
	}

	feeResp, ok := respVal.(*chainsource.FeeEstimateResponse)
	if !ok {
		return 0, fmt.Errorf("unexpected response type: %T", respVal)
	}

	return feeResp.SatPerVByte, nil
}

// registerSweepConfirmation registers for confirmation notification of a
// broadcast sweep transaction. The heightHint should be the current best block
// height to avoid unnecessary rescans of historical blocks.
func (a *Actor) registerSweepConfirmation(ctx context.Context,
	batchID batchwatcher.BatchID, txid *chainhash.Hash, pkScript []byte,
	heightHint uint32) error {

	// Create a mapped reference that transforms ConfirmationEvent to
	// SweepConfirmedEvent.
	mappedRef := chainsource.MapConfirmationEvent(
		a.cfg.SelfRef,
		func(conf chainsource.ConfirmationEvent) Msg {
			return &SweepConfirmedEvent{
				BatchID:     batchID,
				Txid:        conf.Txid,
				BlockHeight: conf.BlockHeight,
			}
		},
	)

	req := &chainsource.RegisterConfRequest{
		CallerID:    fmt.Sprintf("batchsweeper-conf-%s", batchID),
		Txid:        txid,
		PkScript:    pkScript,
		TargetConfs: a.cfg.SweepConfirmations,
		HeightHint:  heightHint,
		NotifyActor: fn.Some(mappedRef),
	}

	// Use a background context because the confirmation subscription
	// must outlive the current batch-expiry handler invocation.
	bgCtx := context.Background()
	if err := a.cfg.ChainSource.Tell(bgCtx, req); err != nil {
		return fmt.Errorf("register sweep confirmation: %w",
			err)
	}

	a.log.DebugS(ctx, "Registered for sweep confirmation",
		"batch_id", batchID,
		"txid", txid,
		"target_confs", a.cfg.SweepConfirmations,
		"height_hint", heightHint)

	return nil
}

// handleSweepConfirmed processes a sweep confirmation notification and cleans
// up tracking state.
func (a *Actor) handleSweepConfirmed(ctx context.Context,
	msg *SweepConfirmedEvent) fn.Result[Resp] {

	pending, ok := a.pendingSweeps[msg.BatchID]
	if !ok {
		a.log.WarnS(ctx, "Received confirmation for unknown pending sweep",
			nil,
			"batch_id", msg.BatchID,
			"txid", msg.Txid)

		return fn.Ok[Resp](nil)
	}

	a.log.InfoS(ctx, "Sweep transaction confirmed",
		"batch_id", msg.BatchID,
		"txid", msg.Txid,
		"block_height", msg.BlockHeight,
		"fee_rate_sat_vb", pending.feeRate,
		"num_inputs", pending.numInputs)

	// Notify the ledger actor of capital reclamation. The
	// absolute mining fee is derived from the sweep tx
	// directly where available; producers that have not yet
	// captured the fee leave MiningFeeSat zero and the ledger
	// handler skips the mining_fees leg.
	a.cfg.LedgerRef.WhenSome(func(
		ref actor.TellOnlyRef[ledger.LedgerMsg]) {

		tellErr := ref.Tell(
			ctx, &ledger.SweepCompletedMsg{
				BatchID:            msg.BatchID,
				ReclaimedAmountSat: pending.sweepAmount,
				Count:              int32(pending.numInputs),
				BlockHeight:        uint32(msg.BlockHeight),
				FeeRateSatVB:       int64(pending.feeRate),
			},
		)
		if tellErr != nil {
			a.log.WarnS(
				ctx,
				"Failed to notify ledger of "+
					"sweep completion",
				tellErr,
			)
		}
	})

	// Clean up tracking state. The watcher self-unregisters and sends
	// a BatchSweptNotification when it detects the batch root spend,
	// so VTXO marking is handled in handleBatchSwept rather than here.
	delete(a.pendingSweeps, msg.BatchID)
	delete(a.expired, msg.BatchID)

	return fn.Ok[Resp](nil)
}

// handleBatchSwept processes a notification from the watcher that a batch was
// fully swept by a non-tree transaction. The watcher has already self-
// unregistered, so we just extract VTXO outpoints from the carried tree and
// invoke the OnBatchSwept callback.
func (a *Actor) handleBatchSwept(ctx context.Context,
	msg *BatchSweptEvent) fn.Result[Resp] {

	if msg.Notification == nil {
		return fn.Err[Resp](fmt.Errorf("nil batch swept notification"))
	}

	batchID := msg.Notification.BatchID

	a.log.InfoS(ctx, "Batch swept notification received",
		"batch_id", batchID)

	// Clean up tracking state since the watcher has already
	// self-unregistered and won't send further notifications.
	delete(a.expired, batchID)
	delete(a.pendingSweeps, batchID)

	if a.cfg.OnBatchSwept == nil {
		return fn.Ok[Resp](nil)
	}

	batchTree := msg.Notification.Tree
	if batchTree == nil || batchTree.Root == nil {
		a.log.WarnS(ctx,
			"Batch swept notification has nil tree",
			nil, "batch_id", batchID)

		return fn.Ok[Resp](nil)
	}

	var vtxoOutpoints []wire.OutPoint
	for node := range batchTree.Root.NodesIter() {
		if !node.IsLeaf() {
			continue
		}

		txid, err := node.TXID()
		if err != nil {
			return fn.Err[Resp](fmt.Errorf(
				"compute leaf TXID for batch %s: %w",
				batchID, err,
			))
		}

		// The VTXO output is at index 0 of each leaf transaction.
		vtxoOutpoints = append(vtxoOutpoints, wire.OutPoint{
			Hash:  txid,
			Index: 0,
		})
	}

	if len(vtxoOutpoints) > 0 {
		err := a.cfg.OnBatchSwept(ctx, vtxoOutpoints)
		if err != nil {
			return fn.Err[Resp](fmt.Errorf(
				"mark swept VTXOs for batch %s: %w",
				batchID, err,
			))
		}

		a.log.InfoS(ctx, "Marked swept VTXOs as expired",
			"batch_id", batchID,
			"num_vtxos", len(vtxoOutpoints))
	}

	return fn.Ok[Resp](nil)
}

// scheduleRetry schedules a timer-based sweep retry when a specific delay is
// needed (e.g., waiting for CSV maturity). For normal failures, BatchWatcher
// will re-notify us each block, so timer-based retries are only used when
// there's a delay hint indicating we should wait longer than one block.
func (a *Actor) scheduleRetry(ctx context.Context, batchID batchwatcher.BatchID,
	delayHint time.Duration, countAttempt bool) {

	state, ok := a.expired[batchID]
	if !ok {
		return
	}

	if countAttempt {
		state.attempts++
	}

	// If no delay hint, rely on per-block retries from BatchWatcher instead
	// of scheduling a timer.
	if delayHint == 0 {
		return
	}

	delay := retryDelay(
		a.cfg.InitialRetryDelay, a.cfg.MaxRetryDelay, state.attempts,
	)
	if delayHint > delay {
		delay = delayHint
	}

	if delay > a.cfg.MaxRetryDelay {
		delay = a.cfg.MaxRetryDelay
	}

	a.cfg.TimeoutActor.WhenSome(func(ref actor.TellOnlyRef[timeout.Msg]) {
		timeoutID := timeout.ID(fmt.Sprintf("batchsweeper-%s", batchID))

		callbackRef := timeout.MapTimeoutExpired(
			a.cfg.SelfRef,
			func(_ timeout.ExpiredMsg) Msg {
				return &SweepRetryEvent{
					BatchID: batchID,
				}
			},
		)

		if err := ref.Tell(ctx, &timeout.ScheduleTimeoutRequest{
			ID:       timeoutID,
			Duration: delay,
			Callback: callbackRef,
		}); err != nil {
			a.log.WarnS(ctx, "Unable to schedule sweep retry timer",
				err,
				"batch_id", batchID,
				"timeout_id", timeoutID)
		}

		a.log.DebugS(ctx, "Scheduled sweep retry",
			"batch_id", batchID,
			"attempt", state.attempts,
			"count_attempt", countAttempt,
			"delay", delay)
	})
}

// retryDelay computes an exponential backoff delay.
func retryDelay(initial, maxDelay time.Duration, attempt uint32) time.Duration {
	if attempt == 0 {
		return initial
	}

	delay := initial
	for i := uint32(1); i < attempt; i++ {
		if delay >= maxDelay/2 {
			return maxDelay
		}

		delay *= 2
	}

	if delay > maxDelay {
		return maxDelay
	}

	return delay
}

// blocksToDuration converts a block count to an estimated wall-clock duration,
// returning whether the conversion overflowed.
func blocksToDuration(blocks uint32, interval time.Duration) (time.Duration,
	bool) {

	if blocks == 0 {
		return 0, false
	}

	intervalNanos := interval.Nanoseconds()
	if intervalNanos <= 0 {
		return 0, true
	}

	blocksNanos := int64(blocks)
	if blocksNanos > (1<<62)/intervalNanos {
		return 0, true
	}

	return time.Duration(blocksNanos * intervalNanos), false
}
