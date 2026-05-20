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

// errSweepKeyUnknown is returned when the watcher's tree state carries
// no sweep-key information at all. The sweeper refuses to broadcast in
// this case so an operator restart does not silently sign with the
// configured key for a batch whose historical descriptor was never
// persisted (the exact wrong-key path #388 was filed about).
var errSweepKeyUnknown = errors.New("refusing to sweep: no per-batch " +
	"sweep-key descriptor persisted")

// errSweepKeyMismatch is returned when the persisted sweep pubkey
// differs from the configured one and the locator is unknown. Signing
// with the configured key would produce a witness that does not
// satisfy the historical tapleaf, so the sweeper refuses and surfaces
// the gap to the operator instead of looping on bad broadcasts.
var errSweepKeyMismatch = errors.New("refusing to sweep: persisted sweep " +
	"pubkey differs from configured key and locator is unknown")

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
// candidates at the given fee rate using the supplied sweep key. The
// sweep key is passed per-batch (rather than read from ActorConfig) so a
// configured-key rotation cannot strand pre-rotation batches: the
// signer always uses the historical descriptor that derived the
// tapleaf committed in the candidate outputs.
type SweepTxBuilder func(candidates []*batchwatcher.Output,
	sweepKey keychain.KeyDescriptor,
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
	consumedOps []wire.OutPoint
	returnOps   []wire.OutPoint
}

// pendingSweptCallback tracks a batch whose root has been swept on-chain but
// whose OnBatchSwept callback has not yet succeeded. The watcher unregisters
// the batch as soon as Tell enqueues, so this is the sole in-process retry
// surface for the durable VTXO-expiry transition.
type pendingSweptCallback struct {
	// vtxoOutpoints are the leaf VTXO outpoints derived once from the
	// notification's tree. We persist them rather than the tree itself so
	// the retry path has no chance of re-deriving a different set.
	vtxoOutpoints []wire.OutPoint

	// attempts counts failed callback invocations, used to compute
	// exponential backoff and to surface alerts when retries persistently
	// fail.
	attempts uint32

	// lastError is the most recent callback error, retained for alerts.
	lastError error
}

// subtreeSweptKey identifies a particular subtree sweep within a batch. A
// single batch can have multiple in-flight subtree sweeps from independently
// exposed branches, so the batch ID alone is not unique; we disambiguate by
// the txid of the swept subtree's root node.
type subtreeSweptKey struct {
	batchID     batchwatcher.BatchID
	subtreeTxid chainhash.Hash
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
	// confirmation. A single batch can have multiple in-flight sweeps
	// (for example, a subtree-branch sweep concurrent with a later
	// root sweep, or two distinct subtree sweeps from independently
	// exposed branches). Keying by (batchID, txid) keeps each broadcast
	// individually tracked so a second broadcast does not overwrite an
	// earlier one before it confirms, and so confirmations from reorged
	// or unrelated transactions cannot clear the wrong entry.
	pendingSweeps map[batchwatcher.BatchID]map[chainhash.Hash]*pendingSweep

	// pendingSweptCallbacks holds OnBatchSwept invocations that have not
	// yet succeeded. Tell from the watcher is fire-and-forget at the
	// mailbox boundary, so a callback failure cannot be surfaced back
	// upstream; we retain the derived outpoints here and drive timer-based
	// retries until the callback succeeds.
	pendingSweptCallbacks map[batchwatcher.BatchID]*pendingSweptCallback

	// pendingSubtreeSweptCallbacks holds OnBatchSwept invocations for
	// mid-tree branch sweeps that have not yet succeeded. The watcher
	// does not redeliver the subtree-swept notification once it has
	// notified, so without this in-process retry surface a transient DB
	// failure would silently leave the descendant VTXOs marked live.
	// Keyed by (batchID, subtreeTxid) because a single batch can have
	// multiple independent subtree sweeps in flight.
	pendingSubtreeSweptCallbacks map[subtreeSweptKey]*pendingSweptCallback

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
			map[batchwatcher.BatchID]map[chainhash.Hash]*pendingSweep,
		),
		pendingSweptCallbacks: make(
			map[batchwatcher.BatchID]*pendingSweptCallback,
		),
		pendingSubtreeSweptCallbacks: make(
			map[subtreeSweptKey]*pendingSweptCallback,
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

	case *BatchSweptCallbackRetryEvent:
		return a.handleBatchSweptCallbackRetry(ctx, m)

	case *BatchSubtreeSweptEvent:
		return a.handleBatchSubtreeSwept(ctx, m)

	case *BatchSubtreeSweptCallbackRetryEvent:
		return a.handleBatchSubtreeSweptCallbackRetry(ctx, m)

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
			"expiry_height", msg.Notification.ExpiryHeight,
		)

		a.expired[batchID] = &expiredBatch{
			expiryHeight: msg.Notification.ExpiryHeight,
			attempts:     0,
		}
	}

	// If there are any pending sweeps for this batch, only rebroadcast
	// when the current fee rate beats the best fee rate already in flight.
	// We compare against the maximum because rebroadcasting at the same
	// or lower rate would not RBF and would only churn state.
	if maxPending, hasPending := a.maxPendingFeeRate(batchID); hasPending {
		shouldBump, err := a.shouldBumpFee(ctx, maxPending)
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
			"old_fee_rate", maxPending,
		)
	}

	err := a.trySweep(ctx, batchID)
	if err != nil {
		a.handleSweepAttemptError(ctx, batchID, err)
	}

	return fn.Ok[Resp](nil)
}

// shouldBumpFee checks if the current fee rate is higher than the supplied
// reference rate, indicating we should rebroadcast with a higher fee. The
// reference rate is the highest fee rate currently in flight for the batch
// so that we never rebroadcast at the same or lower rate (which would not
// RBF).
func (a *Actor) shouldBumpFee(ctx context.Context,
	referenceFeeRate btcutil.Amount) (bool, error) {

	currentFeeRate, err := a.queryFeeRate(ctx)
	if err != nil {
		return false, err
	}

	return currentFeeRate > referenceFeeRate, nil
}

// maxPendingFeeRate returns the highest fee rate among in-flight sweeps for
// the batch. The second return value reports whether any sweep was found.
func (a *Actor) maxPendingFeeRate(batchID batchwatcher.BatchID) (btcutil.Amount,
	bool) {

	sweeps, ok := a.pendingSweeps[batchID]
	if !ok || len(sweeps) == 0 {
		return 0, false
	}

	var highest btcutil.Amount
	for _, p := range sweeps {
		if p.feeRate > highest {
			highest = p.feeRate
		}
	}

	return highest, true
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

	a.log.TraceS(
		ctx, "Tree state changed", "batch_id", msg.Notification.BatchID,
	)

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
			"batch_id", batchID,
		)

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
				"best_height", bestHeight,
			)

			return nil
		}

		blocksRemaining := nextHeight - uint32(bestHeight)

		a.log.DebugS(ctx, "Sweep candidates not yet CSV-mature",
			"batch_id", batchID,
			"next_maturity_height", nextHeight,
			"blocks_remaining", blocksRemaining,
		)
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

	// Resolve the sweep key for this specific batch. The watcher
	// captured the key descriptor that derived the tapleaf committed
	// in this tree at registration time, so we sign with the
	// historical key rather than whatever sweep key the actor was
	// configured with at restart.
	//
	// Two degraded inputs need handling. (1) The watcher hands us a
	// fully zero descriptor: nothing was persisted for this round, so
	// we cannot verify which key built the tapleaf. (2) The
	// descriptor carries only a pubkey (pre-migration row: the
	// locator columns did not exist when the round was inserted): we
	// know the historical pubkey but not its locator.
	//
	// In case (2), if the configured key's pubkey matches the
	// persisted one, the configured locator is by definition the
	// right one -- the key has not been rotated since this round was
	// finalized -- so we use cfg.SweepKey. If the pubkeys differ, the
	// configured key has rotated and signing with it would produce a
	// witness that does not satisfy the historical tapleaf,
	// reproducing the exact bug #388 was filed about. We refuse
	// instead and surface the gap via ErrorS so the operator can
	// intervene (e.g. point the daemon at the pre-rotation key) or
	// backfill the locator before the batch expires beyond recovery.
	sweepKey, refuseErr := a.resolveSweepKey(
		ctx, batchID, treeState.SweepKey,
	)
	if refuseErr != nil {
		return refuseErr
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
			sweepKey keychain.KeyDescriptor,
			feeRate btcutil.Amount) (*wire.MsgTx, error) {

			return buildSignedSweepTx(
				candidates, sweepKey, a.cfg.SweepDelay,
				sweepPkScript, feeRate, a.cfg.Signer,
			)
		}
	}

	sweepTx, err := builder(candidates, sweepKey, feeRate)
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
		"fee_rate_sat_vb", feeRate,
	)

	// The address was consumed by a successful broadcast, so clear
	// the cache so the next sweep gets a fresh destination.
	a.cachedSweepPkScript = nil

	// Capture the consumed / return outpoints and the net sweep
	// amount for ledger attribution. The sweep tx has exactly
	// one return output back to the treasury wallet at index
	// zero; every tx input is a sweep-consumption outpoint.
	//
	// The reported sweep amount is the return output value (net
	// of miner fee), not the sum of input values: the ledger
	// handler books RecordRoundSweep(ReclaimedAmountSat) as a
	// treasury_wallet credit, and only the net actually lands
	// on-chain. Until MiningFeeSat is populated by the producer,
	// sending gross input value here would overstate
	// treasury_wallet by the miner fee every sweep.
	consumed := make([]wire.OutPoint, 0, len(candidates))
	for _, c := range candidates {
		consumed = append(consumed, c.Outpoint)
	}

	var (
		sweepAmount int64
		returns     []wire.OutPoint
	)
	if len(sweepTx.TxOut) > 0 {
		sweepAmount = sweepTx.TxOut[0].Value
		returns = append(returns, wire.OutPoint{
			Hash:  txid,
			Index: 0,
		})
	}

	// Track this pending sweep and register for confirmation
	// notification. Multiple sweeps can race for the same batch
	// (subtree branches plus an eventual root sweep), so each is
	// indexed by its own txid rather than overwriting per-batch state.
	byTxid, ok := a.pendingSweeps[batchID]
	if !ok {
		byTxid = make(map[chainhash.Hash]*pendingSweep)
		a.pendingSweeps[batchID] = byTxid
	}
	byTxid[txid] = &pendingSweep{
		txid:        txid,
		batchID:     batchID,
		broadcastAt: time.Now(),
		feeRate:     feeRate,
		numInputs:   len(candidates),
		sweepAmount: sweepAmount,
		consumedOps: consumed,
		returnOps:   returns,
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

// resolveSweepKey picks the sweep descriptor used to sign a batch's
// timeout spend. See the call site in trySweep for the case analysis;
// the returned non-nil error is the refuse path (#388 wrong-key
// scenario) and is intentionally not wrapped as a retryableError so the
// caller does not keep broadcasting bad witnesses while the operator
// investigates.
func (a *Actor) resolveSweepKey(ctx context.Context,
	batchID batchwatcher.BatchID, persisted keychain.KeyDescriptor) (
	keychain.KeyDescriptor, error) {

	// Fully populated descriptor: the migration captured both the
	// pubkey and the locator, so we sign with the historical key.
	if persisted.PubKey != nil &&
		persisted.KeyLocator != (keychain.KeyLocator{}) {
		return persisted, nil
	}

	// Locator unknown (pre-migration row carrying only the pubkey):
	// only the configured key is safe to use, and only when its
	// pubkey still matches the persisted one.
	if persisted.PubKey != nil {
		cfgPub := a.cfg.SweepKey.PubKey
		if cfgPub != nil && cfgPub.IsEqual(persisted.PubKey) {
			a.log.WarnS(ctx, "Pre-migration round missing sweep "+
				"key locator; configured key matches "+
				"persisted pubkey so the configured locator "+
				"is safe to reuse. Backfill the locator to "+
				"silence this warning.", nil,
				"batch_id", batchID,
			)

			return a.cfg.SweepKey, nil
		}

		// Operator misconfiguration (or rotation that pre-dated the
		// migration) is an external trigger, so log at WarnS here;
		// the existing alert threshold in maybeAlert escalates to
		// ErrorS once retries pile up, giving operators a stable
		// rate-limited signal without spamming error-level logs on
		// every block.
		a.log.WarnS(ctx, "Refusing to sweep with mismatched sweep "+
			"key", errSweepKeyMismatch,
			"batch_id", batchID,
		)

		return keychain.KeyDescriptor{}, errSweepKeyMismatch
	}

	// Zero descriptor: no per-round sweep info at all. We cannot
	// verify the configured key matches the historical one, so
	// refuse rather than risk reproducing #388.
	a.log.WarnS(ctx, "Refusing to sweep batch with no persisted "+
		"sweep-key metadata", errSweepKeyUnknown,
		"batch_id", batchID,
	)

	return keychain.KeyDescriptor{}, errSweepKeyUnknown
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
		return nil, fmt.Errorf("generate sweep pkscript: %w", err)
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
		return fmt.Errorf("register sweep confirmation: %w", err)
	}

	a.log.DebugS(ctx, "Registered for sweep confirmation",
		"batch_id", batchID,
		"txid", txid,
		"target_confs", a.cfg.SweepConfirmations,
		"height_hint", heightHint,
	)

	return nil
}

// handleSweepConfirmed processes a sweep confirmation notification and cleans
// up tracking state. The confirming txid must match a tracked pending sweep
// for this batch; mismatches (e.g. due to a reorg or an unrelated tx
// confirming at a watched outpoint) are logged and ignored so that an
// unrelated event cannot evict in-flight sweep bookkeeping.
func (a *Actor) handleSweepConfirmed(ctx context.Context,
	msg *SweepConfirmedEvent) fn.Result[Resp] {

	confTxid := chainhash.Hash(msg.Txid)

	byTxid, ok := a.pendingSweeps[msg.BatchID]
	if !ok || len(byTxid) == 0 {
		a.log.WarnS(ctx, "Received confirmation for unknown pending "+
			"sweep",
			nil,
			"batch_id", msg.BatchID,
			"txid", confTxid)

		return fn.Ok[Resp](nil)
	}

	pending, ok := byTxid[confTxid]
	if !ok {
		// A confirmation whose txid does not match any in-flight
		// sweep for this batch is suspect (reorg, replacement, or
		// caller mismatch). Leave the pending bookkeeping intact so
		// the legitimate confirmation can still clear it.
		a.log.WarnS(ctx, "Sweep confirmation txid does not match "+
			"any pending sweep",
			nil,
			"batch_id", msg.BatchID,
			"conf_txid", confTxid,
			"pending_count", len(byTxid))

		return fn.Ok[Resp](nil)
	}

	a.log.InfoS(ctx, "Sweep transaction confirmed",
		"batch_id", msg.BatchID,
		"txid", msg.Txid,
		"block_height", msg.BlockHeight,
		"fee_rate_sat_vb", pending.feeRate,
		"num_inputs", pending.numInputs,
	)

	// Notify the ledger actor of capital reclamation. The
	// absolute mining fee is derived from the sweep tx
	// directly where available; producers that have not yet
	// captured the fee leave MiningFeeSat zero and the ledger
	// handler skips the mining_fees leg.
	a.cfg.LedgerRef.WhenSome(func(ref actor.TellOnlyRef[ledger.LedgerMsg]) {
		tellErr := ref.Tell(
			ctx, &ledger.SweepCompletedMsg{
				BatchID:            msg.BatchID,
				ReclaimedAmountSat: pending.sweepAmount,
				Count:              int32(pending.numInputs),
				BlockHeight:        uint32(msg.BlockHeight),
				FeeRateSatVB:       int64(pending.feeRate),
				ConsumedOutpoints:  pending.consumedOps,
				ReturnOutpoints:    pending.returnOps,
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
	//
	// We only drop the per-txid entry that just confirmed; any other
	// in-flight sweeps for this batch (e.g. a separate subtree branch
	// or a pending fee-bump rebroadcast) stay tracked so their own
	// confirmations can still be reconciled. The expired-bookkeeping
	// entry is removed only once all in-flight sweeps for the batch
	// have cleared, preserving retry/alert state for any sweep that
	// has not yet confirmed.
	delete(byTxid, confTxid)
	if len(byTxid) == 0 {
		delete(a.pendingSweeps, msg.BatchID)
		delete(a.expired, msg.BatchID)
	}

	return fn.Ok[Resp](nil)
}

// handleBatchSwept processes a notification from the watcher that a batch was
// fully swept by a non-tree transaction. The watcher unregisters the batch
// as soon as Tell enqueues here, so this handler owns the durable VTXO-
// expiry transition end-to-end: there will be no upstream redelivery from
// the chain layer until the operator restarts (which replays via the
// durable mailbox).
//
// Failure mode handling: if OnBatchSwept fails on the first attempt we
// retain the derived outpoints in pendingSweptCallbacks and schedule a
// timer-driven retry. Local tracking state (expired/pendingSweeps) is only
// dropped once the callback has succeeded, so a transient DB error cannot
// silently leave VTXOs in "live" status (see issue #364).
func (a *Actor) handleBatchSwept(ctx context.Context,
	msg *BatchSweptEvent) fn.Result[Resp] {

	if msg.Notification == nil {
		return fn.Err[Resp](fmt.Errorf("nil batch swept notification"))
	}

	batchID := msg.Notification.BatchID

	a.log.InfoS(ctx, "Batch swept notification received",
		"batch_id", batchID,
	)

	// A nil OnBatchSwept callback is a wiring bug: without it we cannot
	// mark the tree's VTXOs as expired and a stale "live" status would
	// later let them be accepted as forfeit inputs in a round. Fail
	// loudly so the misconfiguration surfaces rather than corrupting
	// state silently.
	if a.cfg.OnBatchSwept == nil {
		return fn.Err[Resp](
			fmt.Errorf("OnBatchSwept callback not configured; "+
				"cannot expire VTXOs for swept batch %s",
				batchID),
		)
	}

	// If we already have a pending callback for this batch, we are
	// receiving a redundant notification (e.g. operator restart replayed
	// the durable mailbox). The previously-derived outpoints are
	// authoritative; just kick the callback again to make progress.
	if _, ok := a.pendingSweptCallbacks[batchID]; ok {
		return a.runBatchSweptCallback(ctx, batchID)
	}

	batchTree := msg.Notification.Tree
	if batchTree == nil || batchTree.Root == nil {
		return fn.Err[Resp](
			fmt.Errorf("batch swept notification for %s has nil "+
				"tree; cannot derive VTXO outpoints", batchID),
		)
	}

	var vtxoOutpoints []wire.OutPoint
	for node := range batchTree.Root.NodesIter() {
		if !node.IsLeaf() {
			continue
		}

		txid, err := node.TXID()
		if err != nil {
			return fn.Err[Resp](
				fmt.Errorf("compute leaf TXID for batch "+
					"%s: %w", batchID, err),
			)
		}

		// The VTXO output is at index 0 of each leaf transaction.
		vtxoOutpoints = append(vtxoOutpoints, wire.OutPoint{
			Hash:  txid,
			Index: 0,
		})
	}

	// Persist the derived outpoints BEFORE attempting the callback. A
	// failure path needs the durable record to drive timer retries — if
	// we only added the entry on error we would race with the very first
	// attempt's failure handler.
	a.pendingSweptCallbacks[batchID] = &pendingSweptCallback{
		vtxoOutpoints: vtxoOutpoints,
	}

	return a.runBatchSweptCallback(ctx, batchID)
}

// handleBatchSweptCallbackRetry retries a previously-failed OnBatchSwept
// invocation. The retry is driven by the TimeoutActor; if the batch's
// pending entry has since been cleared (e.g. a concurrent restart-replay
// already succeeded), the retry is a no-op.
func (a *Actor) handleBatchSweptCallbackRetry(ctx context.Context,
	msg *BatchSweptCallbackRetryEvent) fn.Result[Resp] {

	if _, ok := a.pendingSweptCallbacks[msg.BatchID]; !ok {
		a.log.TraceS(
			ctx, "Batch swept callback retry no longer pending",
			"batch_id", msg.BatchID,
		)

		return fn.Ok[Resp](nil)
	}

	return a.runBatchSweptCallback(ctx, msg.BatchID)
}

// runBatchSweptCallback invokes OnBatchSwept for the given batch using the
// previously-derived outpoints. On success it clears all tracking state
// associated with the batch. On failure it increments the attempt counter,
// schedules a retry via the TimeoutActor, and propagates the error so the
// actor framework can surface it.
func (a *Actor) runBatchSweptCallback(ctx context.Context,
	batchID batchwatcher.BatchID) fn.Result[Resp] {

	pending, ok := a.pendingSweptCallbacks[batchID]
	if !ok {

		// Defensive: callers (handleBatchSwept/handleRetry) only
		// invoke this with a known-pending batch. Treat a missing
		// entry as a benign race.
		return fn.Ok[Resp](nil)
	}

	if len(pending.vtxoOutpoints) > 0 {
		err := a.cfg.OnBatchSwept(ctx, pending.vtxoOutpoints)
		if err != nil {
			pending.attempts++
			pending.lastError = err

			a.scheduleBatchSweptCallbackRetry(ctx, batchID)
			a.maybeAlertBatchSweptCallback(ctx, batchID)

			return fn.Err[Resp](
				fmt.Errorf("mark swept VTXOs for batch %s: %w",
					batchID, err),
			)
		}

		a.log.InfoS(ctx, "Marked swept VTXOs as expired",
			"batch_id", batchID,
			"num_vtxos", len(pending.vtxoOutpoints),
			"attempts", pending.attempts+1,
		)
	}

	// VTXOs are now durably marked expired. Drop all in-memory tracking
	// for this batch: pendingSweptCallbacks first (it owns the retry
	// surface), then subtree callbacks made redundant by the root sweep,
	// then the sweep-attempt bookkeeping that the watcher will no longer
	// re-trigger.
	delete(a.pendingSweptCallbacks, batchID)
	for key := range a.pendingSubtreeSweptCallbacks {
		if key.batchID == batchID {
			delete(a.pendingSubtreeSweptCallbacks, key)
		}
	}
	delete(a.expired, batchID)
	delete(a.pendingSweeps, batchID)

	return fn.Ok[Resp](nil)
}

// scheduleBatchSweptCallbackRetry schedules a timer-driven retry of the
// OnBatchSwept callback for the given batch. Without a configured
// TimeoutActor the actor logs a warning and relies on a future restart to
// replay the durable mailbox; this matches the existing best-effort
// behaviour of scheduleRetry.
func (a *Actor) scheduleBatchSweptCallbackRetry(ctx context.Context,
	batchID batchwatcher.BatchID) {

	pending, ok := a.pendingSweptCallbacks[batchID]
	if !ok {
		return
	}

	delay := retryDelay(
		a.cfg.InitialRetryDelay, a.cfg.MaxRetryDelay, pending.attempts,
	)
	if delay > a.cfg.MaxRetryDelay {
		delay = a.cfg.MaxRetryDelay
	}

	a.cfg.TimeoutActor.WhenSome(func(ref actor.TellOnlyRef[timeout.Msg]) {
		timeoutID := timeout.ID(
			fmt.Sprintf("batchsweeper-swept-cb-%s", batchID),
		)

		callbackRef := timeout.MapTimeoutExpired(
			a.cfg.SelfRef,
			func(_ timeout.ExpiredMsg) Msg {
				return &BatchSweptCallbackRetryEvent{
					BatchID: batchID,
				}
			},
		)

		req := &timeout.ScheduleTimeoutRequest{
			ID:       timeoutID,
			Duration: delay,
			Callback: callbackRef,
		}
		if err := ref.Tell(ctx, req); err != nil {
			a.log.WarnS(
				ctx, "Unable to schedule batch-swept "+
					"callback retry timer", err,
				"batch_id", batchID,
				"timeout_id", timeoutID,
			)

			return
		}

		a.log.DebugS(ctx, "Scheduled batch-swept callback retry",
			"batch_id", batchID,
			"attempts", pending.attempts,
			"delay", delay,
		)
	})
}

// maybeAlertBatchSweptCallback emits an alert log when callback retries
// exceed the configured threshold. Alerting matches the sweep-attempt
// alert cadence so persistent failures surface to operators identically
// regardless of which leg of the pipeline is stuck.
func (a *Actor) maybeAlertBatchSweptCallback(ctx context.Context,
	batchID batchwatcher.BatchID) {

	pending, ok := a.pendingSweptCallbacks[batchID]
	if !ok {
		return
	}

	if pending.attempts < a.cfg.AlertThreshold {
		return
	}

	if pending.attempts == a.cfg.AlertThreshold {
		a.log.ErrorS(
			ctx, "Batch-swept callback retries exceeded "+
				"alert threshold", pending.lastError,
			"batch_id", batchID,
			"attempts", pending.attempts,
		)

		return
	}

	since := pending.attempts - a.cfg.AlertThreshold
	if since%a.cfg.AlertRepeatInterval == 0 {
		a.log.ErrorS(
			ctx, "Batch-swept callback retries continue",
			pending.lastError,
			"batch_id", batchID,
			"attempts", pending.attempts,
		)
	}
}

// handleBatchSubtreeSwept processes a notification that an exposed mid-tree
// branch output was swept after expiry. The watcher continues to monitor the
// rest of the tree, so we do not touch pendingSweeps / expired bookkeeping
// here. We extract the descendant VTXO leaf outpoints from the swept
// subtree and invoke OnBatchSwept so the storage layer marks those VTXOs
// expired — preventing them from being re-entered into a later round as
// forfeit inputs once they have been invalidated on-chain.
//
// Failure mode handling mirrors handleBatchSwept: if OnBatchSwept fails on
// the first attempt we retain the derived outpoints in
// pendingSubtreeSweptCallbacks and schedule a timer-driven retry. The
// watcher does not redeliver the subtree-swept notification, so without
// this durable retry surface a transient DB error would silently leave the
// descendant VTXOs marked live.
func (a *Actor) handleBatchSubtreeSwept(ctx context.Context,
	msg *BatchSubtreeSweptEvent) fn.Result[Resp] {

	if msg.Notification == nil {
		return fn.Err[Resp](
			fmt.Errorf("nil batch subtree-swept notification"),
		)
	}

	batchID := msg.Notification.BatchID
	subtree := msg.Notification.SubtreeRoot

	a.log.InfoS(ctx, "Batch subtree-swept notification received",
		"batch_id", batchID,
	)

	// A nil OnBatchSwept callback is a wiring bug: without it we cannot
	// mark the subtree's VTXOs as expired and a stale "live" status would
	// later let them be accepted as forfeit inputs in a round. Fail
	// loudly so the misconfiguration surfaces rather than corrupting
	// state silently (mirrors handleBatchSwept).
	if a.cfg.OnBatchSwept == nil {
		return fn.Err[Resp](
			fmt.Errorf("OnBatchSwept callback not configured; "+
				"cannot expire VTXOs for swept subtree of "+
				"batch %s", batchID),
		)
	}

	// A nil subtree means the watcher could not identify which leaves
	// were invalidated. Returning ok would silently drop the expiry
	// signal; we surface this as an error for symmetry with the
	// root-sweep nil-tree guard.
	if subtree == nil {
		return fn.Err[Resp](
			fmt.Errorf("batch subtree-swept notification for %s "+
				"has nil subtree root; cannot derive VTXO "+
				"outpoints", batchID),
		)
	}

	subtreeTxid, err := subtree.TXID()
	if err != nil {
		return fn.Err[Resp](
			fmt.Errorf("compute subtree root TXID for batch "+
				"%s: %w", batchID, err),
		)
	}

	key := subtreeSweptKey{
		batchID:     batchID,
		subtreeTxid: subtreeTxid,
	}

	// A redundant notification (e.g. operator restart replayed the
	// durable mailbox) reuses the already-derived outpoints; just kick
	// the callback again to make progress.
	if _, ok := a.pendingSubtreeSweptCallbacks[key]; ok {
		return a.runBatchSubtreeSweptCallback(ctx, key)
	}

	var vtxoOutpoints []wire.OutPoint
	for leaf := range subtree.LeavesIter() {
		txid, err := leaf.TXID()
		if err != nil {
			return fn.Err[Resp](
				fmt.Errorf("compute leaf TXID for batch %s "+
					"subtree: %w", batchID, err),
			)
		}

		// The VTXO output is at index 0 of each leaf transaction.
		vtxoOutpoints = append(vtxoOutpoints, wire.OutPoint{
			Hash:  txid,
			Index: 0,
		})
	}

	if len(vtxoOutpoints) == 0 {
		return fn.Ok[Resp](nil)
	}

	// Persist the derived outpoints BEFORE attempting the callback so the
	// failure path has a durable record to drive timer retries.
	a.pendingSubtreeSweptCallbacks[key] = &pendingSweptCallback{
		vtxoOutpoints: vtxoOutpoints,
	}

	return a.runBatchSubtreeSweptCallback(ctx, key)
}

// handleBatchSubtreeSweptCallbackRetry retries a previously-failed
// OnBatchSwept invocation for a subtree sweep. The retry is driven by the
// TimeoutActor; if the pending entry has since been cleared (e.g. a
// concurrent restart-replay already succeeded), the retry is a no-op.
func (a *Actor) handleBatchSubtreeSweptCallbackRetry(ctx context.Context,
	msg *BatchSubtreeSweptCallbackRetryEvent) fn.Result[Resp] {

	key := subtreeSweptKey{
		batchID:     msg.BatchID,
		subtreeTxid: msg.SubtreeTxid,
	}

	if _, ok := a.pendingSubtreeSweptCallbacks[key]; !ok {
		a.log.TraceS(
			ctx, "Subtree-swept callback retry no longer pending",
			"batch_id", msg.BatchID, "subtree_txid",
			msg.SubtreeTxid,
		)

		return fn.Ok[Resp](nil)
	}

	return a.runBatchSubtreeSweptCallback(ctx, key)
}

// runBatchSubtreeSweptCallback invokes OnBatchSwept for the given subtree
// sweep using the previously-derived outpoints. On success it clears the
// tracking entry. On failure it increments the attempt counter, schedules a
// retry, and propagates the error so the actor framework can surface it.
func (a *Actor) runBatchSubtreeSweptCallback(ctx context.Context,
	key subtreeSweptKey) fn.Result[Resp] {

	pending, ok := a.pendingSubtreeSweptCallbacks[key]
	if !ok {

		// Defensive: callers only invoke this with a known-pending
		// key. Treat a missing entry as a benign race.
		return fn.Ok[Resp](nil)
	}

	if len(pending.vtxoOutpoints) > 0 {
		err := a.cfg.OnBatchSwept(ctx, pending.vtxoOutpoints)
		if err != nil {
			pending.attempts++
			pending.lastError = err

			a.scheduleBatchSubtreeSweptCallbackRetry(ctx, key)
			a.maybeAlertBatchSubtreeSweptCallback(ctx, key)

			return fn.Err[Resp](
				fmt.Errorf("mark swept subtree VTXOs for "+
					"batch %s: %w", key.batchID, err),
			)
		}

		a.log.InfoS(ctx, "Marked subtree-swept VTXOs as expired",
			"batch_id", key.batchID,
			"num_vtxos", len(pending.vtxoOutpoints),
			"attempts", pending.attempts+1,
		)
	}

	delete(a.pendingSubtreeSweptCallbacks, key)

	return fn.Ok[Resp](nil)
}

// scheduleBatchSubtreeSweptCallbackRetry schedules a timer-driven retry of
// the OnBatchSwept callback for the given subtree sweep. Without a
// configured TimeoutActor the actor logs a warning and relies on a future
// restart to replay the durable mailbox; this matches the existing
// best-effort behaviour of scheduleBatchSweptCallbackRetry.
func (a *Actor) scheduleBatchSubtreeSweptCallbackRetry(ctx context.Context,
	key subtreeSweptKey) {

	pending, ok := a.pendingSubtreeSweptCallbacks[key]
	if !ok {
		return
	}

	delay := retryDelay(
		a.cfg.InitialRetryDelay, a.cfg.MaxRetryDelay, pending.attempts,
	)
	if delay > a.cfg.MaxRetryDelay {
		delay = a.cfg.MaxRetryDelay
	}

	a.cfg.TimeoutActor.WhenSome(func(ref actor.TellOnlyRef[timeout.Msg]) {
		timeoutID := timeout.ID(
			fmt.Sprintf("batchsweeper-subtree-cb-%s-%s",
				key.batchID, key.subtreeTxid),
		)

		callbackRef := timeout.MapTimeoutExpired(
			a.cfg.SelfRef,
			func(_ timeout.ExpiredMsg) Msg {
				return &BatchSubtreeSweptCallbackRetryEvent{
					BatchID:     key.batchID,
					SubtreeTxid: key.subtreeTxid,
				}
			},
		)

		req := &timeout.ScheduleTimeoutRequest{
			ID:       timeoutID,
			Duration: delay,
			Callback: callbackRef,
		}
		if err := ref.Tell(ctx, req); err != nil {
			a.log.WarnS(
				ctx, "Unable to schedule subtree-swept "+
					"callback retry timer", err,
				"batch_id", key.batchID,
				"subtree_txid", key.subtreeTxid,
				"timeout_id", timeoutID,
			)

			return
		}

		a.log.DebugS(ctx, "Scheduled subtree-swept callback retry",
			"batch_id", key.batchID,
			"subtree_txid", key.subtreeTxid,
			"attempts", pending.attempts,
			"delay", delay,
		)
	})
}

// maybeAlertBatchSubtreeSweptCallback emits an alert log when callback
// retries exceed the configured threshold. Mirrors
// maybeAlertBatchSweptCallback for the subtree path.
func (a *Actor) maybeAlertBatchSubtreeSweptCallback(ctx context.Context,
	key subtreeSweptKey) {

	pending, ok := a.pendingSubtreeSweptCallbacks[key]
	if !ok {
		return
	}

	if pending.attempts < a.cfg.AlertThreshold {
		return
	}

	if pending.attempts == a.cfg.AlertThreshold {
		a.log.ErrorS(
			ctx, "Subtree-swept callback retries exceeded "+
				"alert threshold", pending.lastError,
			"batch_id", key.batchID,
			"subtree_txid", key.subtreeTxid,
			"attempts", pending.attempts,
		)

		return
	}

	since := pending.attempts - a.cfg.AlertThreshold
	if since%a.cfg.AlertRepeatInterval == 0 {
		a.log.ErrorS(
			ctx, "Subtree-swept callback retries continue",
			pending.lastError,
			"batch_id", key.batchID,
			"subtree_txid", key.subtreeTxid,
			"attempts", pending.attempts,
		)
	}
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
			"delay", delay,
		)
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
func blocksToDuration(blocks uint32,
	interval time.Duration) (time.Duration, bool) {

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
