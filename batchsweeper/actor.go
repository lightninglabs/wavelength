package batchsweeper

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo/batchwatcher"
	"github.com/lightninglabs/darepo/timeout"
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

	// defaultMaxRetryAttempts is the default maximum number of broadcast
	// retries before giving up on a sweep. This limit prevents infinite
	// retries when there's a fundamental issue with the transaction (e.g.,
	// invalid signatures, dust outputs). The operator should investigate
	// after this limit is reached.
	defaultMaxRetryAttempts = 10

	// defaultInitialRetryDelay is the starting delay for exponential
	// backoff when retrying sweep broadcasts.
	defaultInitialRetryDelay = time.Second

	// defaultMaxRetryDelay caps the exponential backoff to prevent
	// excessively long waits between retry attempts.
	defaultMaxRetryDelay = 5 * time.Minute
)

// ActorConfig contains the configuration for creating a new BatchSweeperActor.
type ActorConfig struct {
	// Logger is used for structured logging.
	Logger btclog.Logger

	// BatchWatcher is a reference to the BatchWatcher actor for querying
	// tree state and unregistering batches after sweeping.
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

	// SweepPkScript is the destination script for sweep outputs.
	SweepPkScript []byte

	// FeeTarget is the confirmation target used for fee estimation.
	FeeTarget uint32

	// BuildSweepTx builds and signs sweep transactions. When unset, the
	// actor uses the default builder based on the other sweep-related
	// configuration fields.
	BuildSweepTx SweepTxBuilder

	// TimeoutActor optionally schedules retries for sweep attempts.
	TimeoutActor fn.Option[actor.TellOnlyRef[timeout.Msg]]

	// MaxRetryAttempts is the maximum number of retry attempts for a
	// single batch sweep.
	MaxRetryAttempts uint32

	// InitialRetryDelay is the initial delay used for sweep retries.
	InitialRetryDelay time.Duration

	// MaxRetryDelay is the maximum delay used for sweep retries.
	MaxRetryDelay time.Duration

	// SelfRef is a reference to this actor for receiving mapped
	// notifications and internal timer callbacks.
	SelfRef actor.TellOnlyRef[Msg]
}

// SweepTxBuilder constructs a sweep transaction spending the provided
// candidates at the given fee rate.
type SweepTxBuilder func(candidates []*batchwatcher.Output,
	feeRate btcutil.Amount) (*wire.MsgTx, error)

// expiredBatch tracks expiry and retry state for a batch.
type expiredBatch struct {
	expiryHeight uint32
	attempts     uint32
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
}

// NewActor creates a new BatchSweeperActor with the provided configuration.
func NewActor(cfg *ActorConfig) *Actor {
	if cfg.FeeTarget == 0 {
		cfg.FeeTarget = defaultFeeTarget
	}

	if cfg.MaxRetryAttempts == 0 {
		cfg.MaxRetryAttempts = defaultMaxRetryAttempts
	}

	if cfg.InitialRetryDelay == 0 {
		cfg.InitialRetryDelay = defaultInitialRetryDelay
	}

	if cfg.MaxRetryDelay == 0 {
		cfg.MaxRetryDelay = defaultMaxRetryDelay
	}

	return &Actor{
		cfg: cfg,
		log: cfg.Logger,
		expired: make(
			map[batchwatcher.BatchID]*expiredBatch,
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

	default:
		return fn.Err[Resp](fmt.Errorf("unknown message type: %T", m))
	}
}

// handleBatchExpired processes a batch expiry notification.
func (a *Actor) handleBatchExpired(ctx context.Context,
	msg *BatchExpiredEvent) fn.Result[Resp] {

	if msg.Notification == nil {
		return fn.Err[Resp](fmt.Errorf("nil batch expiry notification"))
	}

	a.log.InfoS(ctx, "Batch expired",
		"batch_id", msg.Notification.BatchID,
		"expiry_height", msg.Notification.ExpiryHeight)

	a.expired[msg.Notification.BatchID] = &expiredBatch{
		expiryHeight: msg.Notification.ExpiryHeight,
		attempts:     0,
	}

	err := a.trySweep(ctx, msg.Notification.BatchID)
	if err != nil {
		a.handleSweepAttemptError(ctx, msg.Notification.BatchID, err)
	}

	return fn.Ok[Resp](nil)
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

// handleSweepAttemptError logs a sweep attempt error and schedules a retry when
// appropriate.
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

	a.scheduleRetry(ctx, batchID, delayHint, countAttempt)
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
		builder = func(candidates []*batchwatcher.Output,
			feeRate btcutil.Amount) (*wire.MsgTx, error) {

			return buildSignedSweepTx(
				candidates, a.cfg.SweepKey, a.cfg.SweepDelay,
				a.cfg.SweepPkScript, feeRate, a.cfg.Signer,
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

	a.log.InfoS(ctx, "Broadcast batch sweep transaction",
		"batch_id", batchID,
		"txid", sweepTx.TxHash(),
		"num_inputs", len(candidates),
		"fee_rate_sat_vb", feeRate)

	if batch, ok := a.expired[batchID]; ok {
		batch.attempts = 0
	}

	return nil
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

// scheduleRetry schedules a sweep retry if a timeout actor is configured and
// the retry limit has not been exceeded.
func (a *Actor) scheduleRetry(ctx context.Context, batchID batchwatcher.BatchID,
	delayHint time.Duration, countAttempt bool) {

	state, ok := a.expired[batchID]
	if !ok {
		return
	}

	if countAttempt {
		if state.attempts >= a.cfg.MaxRetryAttempts {
			a.log.WarnS(ctx, "Sweep retry attempts exhausted", nil,
				"batch_id", batchID,
				"attempts", state.attempts)

			return
		}

		state.attempts++
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

		ref.Tell(ctx, &timeout.ScheduleTimeoutRequest{
			ID:       timeoutID,
			Duration: delay,
			Callback: callbackRef,
		})

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
