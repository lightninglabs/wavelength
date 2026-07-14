package lwwallet

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/chainsource"
	"golang.org/x/sync/singleflight"
)

// singleflight keys for the broad re-check paths driven by tip
// events. Naming is intentionally stable so concurrent calls
// coalesce; the key strings are not load-bearing for behavior, just
// for deduplication.
const (
	sfKeyCheckConfirmations = "checkConfirmations"
	sfKeyCheckSpends        = "checkSpends"
)

// recheckHeartbeatInterval is the cadence at which handleTipEvents
// re-runs checkConfirmations / checkSpends in addition to its
// per-tip-event re-checks. Esplora can index a confirmed tx
// 1-3 seconds after the block lands, so the per-tip-event check
// may run before the status flip is visible — without a heartbeat
// the next re-check would not happen until the *following* block
// arrives, which on mainnet means up to ~10 minutes of latency for
// a status flip that would have been visible in seconds. 60s is
// the upper bound on perceived registration latency under the
// default 30s tip-poll cadence and is well below mainnet's ~10
// minute typical inter-block gap, so it does not meaningfully
// raise Esplora load over the default poll cadence.
const recheckHeartbeatInterval = 60 * time.Second

// confRegistration tracks a pending confirmation registration within the
// polling loop.
type confRegistration struct {
	// txid is the transaction ID to monitor (nil to match by script).
	txid *chainhash.Hash

	// pkScript is the output script to watch.
	pkScript []byte

	// numConfs is the target confirmation count.
	numConfs uint32

	// heightHint is the earliest block that could contain the tx.
	heightHint uint32

	// includeBlock requests the full block in the confirmation event.
	includeBlock bool

	// confChan is the channel to send the confirmation on.
	confChan chan *chainsource.TxConfirmation

	// cancelCh signals that this registration has been cancelled.
	cancelCh chan struct{}
}

// spendRegistration tracks a pending spend registration within the
// polling loop.
type spendRegistration struct {
	// outpoint is the output to monitor for spending.
	outpoint *wire.OutPoint

	// pkScript is the output script to watch.
	pkScript []byte

	// heightHint is the earliest block that could contain the spend.
	heightHint uint32

	// spendChan is the channel to send the spend detail on.
	spendChan chan *chainsource.SpendDetail

	// cancelCh signals that this registration has been cancelled.
	cancelCh chan struct{}
}

// blockRegistration tracks a block epoch subscription.
type blockRegistration struct {
	// epochChan is the channel to send new block epochs on.
	epochChan chan *chainsource.BlockEpoch

	// cancelCh signals that this registration has been cancelled.
	cancelCh chan struct{}
}

// ChainBackend implements chainsource.ChainBackend using an Esplora HTTP
// client. New-block detection is delegated to a shared TipPoller so the
// chain backend does not race independently against other Esplora
// consumers (notably the EsploraChainService that feeds btcwallet).
// On each TipBlock event the backend dispatches block epochs and re-
// checks pending confirmation and spend registrations.
type ChainBackend struct {
	esplora *EsploraClient

	// tipPoller is the shared tip-event source. The backend may
	// own this poller (when nobody else needs the tip stream) or
	// just subscribe to one started by another component such as
	// the wallet. ownsTipPoller distinguishes the two cases for
	// Start/Stop lifecycle management.
	tipPoller     *TipPoller
	ownsTipPoller bool

	// log is the structured logger for this chain backend instance.
	log btclog.Logger

	// mu protects bestHeight, bestHash, and the registration maps.
	// Using a single mutex for the chain tip avoids a race window
	// where height and hash could be read in an inconsistent state
	// (e.g. height updated but hash not yet).
	mu         sync.Mutex
	bestHeight int32
	bestHash   chainhash.Hash

	confRegs  map[uint64]*confRegistration
	spendRegs map[uint64]*spendRegistration
	blockRegs map[uint64]*blockRegistration
	nextRegID uint64

	// sf coalesces concurrent broad re-check goroutines so that N
	// rapid tip events do not produce N parallel scans of the
	// registration maps. The per-registration one-shot path at
	// register time is already O(1) by design; sf protects the
	// O(N) broadcast-time path from accidental amplification.
	sf singleflight.Group

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewChainBackend creates a new Esplora-backed chain backend with a
// dedicated TipPoller it owns end-to-end. Use this constructor when
// no other component (e.g. the wallet's btcwallet chain adapter)
// already runs a TipPoller against the same Esplora client. The
// pollInterval controls how frequently the underlying TipPoller
// asks Esplora for the latest tip.
func NewChainBackend(esplora *EsploraClient, pollInterval time.Duration,
	logger btclog.Logger) *ChainBackend {

	tp := NewTipPoller(esplora, pollInterval, logger)

	return &ChainBackend{
		esplora:       esplora,
		tipPoller:     tp,
		ownsTipPoller: true,
		log:           logger,
		confRegs:      make(map[uint64]*confRegistration),
		spendRegs:     make(map[uint64]*spendRegistration),
		blockRegs:     make(map[uint64]*blockRegistration),
		stopCh:        make(chan struct{}),
	}
}

// NewChainBackendWithPoller creates a chain backend that subscribes to
// an externally-managed TipPoller. The caller retains responsibility
// for starting and stopping the poller; the backend will not touch its
// lifecycle. Use this constructor when the same Esplora instance is
// shared across components (e.g. the lwwallet integrated wallet, where
// the wallet owns the TipPoller and both the chain backend and
// EsploraChainService subscribe to it).
//
// The tipPoller argument must be non-nil. Passing a nil poller would
// surface as a nil pointer dereference inside Start when the backend
// tries to subscribe; surfacing the misuse at construction time
// instead lets callers see the violation directly.
func NewChainBackendWithPoller(esplora *EsploraClient, tipPoller *TipPoller,
	logger btclog.Logger) (*ChainBackend, error) {

	if tipPoller == nil {
		return nil, fmt.Errorf("tip poller must be non-nil")
	}

	return &ChainBackend{
		esplora:       esplora,
		tipPoller:     tipPoller,
		ownsTipPoller: false,
		log:           logger,
		confRegs:      make(map[uint64]*confRegistration),
		spendRegs:     make(map[uint64]*spendRegistration),
		blockRegs:     make(map[uint64]*blockRegistration),
		stopCh:        make(chan struct{}),
	}, nil
}

// Start subscribes the chain backend to the configured TipPoller and
// begins dispatching tip events. When the backend owns its TipPoller,
// Start also starts the poller; otherwise the caller must have
// started it before calling Start.
func (b *ChainBackend) Start() error {
	if b.ownsTipPoller {
		if err := b.tipPoller.Start(); err != nil {
			return fmt.Errorf("start tip poller: %w", err)
		}
	}

	height, hash, _, sub, err := b.tipPoller.BestBlockAndSubscribe()
	if err != nil {
		return fmt.Errorf("subscribe to tip poller: %w", err)
	}

	b.mu.Lock()
	b.bestHeight = height
	b.bestHash = hash
	b.mu.Unlock()

	b.wg.Add(1)
	go b.handleTipEvents(sub)

	b.log.InfoS(context.Background(), "Chain backend started",
		slog.Int("tip_height", int(height)),
		slog.String("tip_hash", hash.String()),
	)

	return nil
}

// Stop unsubscribes from the TipPoller, drains the event handler
// goroutine, and (if the backend owns the poller) stops the poller
// itself. Stop is idempotent: the second call returns immediately
// after the first has finished.
func (b *ChainBackend) Stop() error {
	b.log.InfoS(context.Background(), "Stopping chain backend")

	b.stopOnce.Do(func() {
		close(b.stopCh)
	})

	if b.ownsTipPoller {
		b.tipPoller.Stop()
	}

	b.wg.Wait()

	b.log.InfoS(context.Background(), "Chain backend stopped")

	return nil
}

// EstimateFee returns the estimated fee rate in satoshis per vbyte for a
// transaction to confirm within the target number of blocks.
func (b *ChainBackend) EstimateFee(ctx context.Context, targetConf uint32) (
	btcutil.Amount, error) {

	estimates, err := b.esplora.GetFeeEstimates(ctx)
	if err != nil {
		return 0, fmt.Errorf("get fee estimates: %w", err)
	}

	if len(estimates) == 0 {
		return 0, fmt.Errorf("no fee estimates available")
	}

	// Find the closest confirmation target in the estimates map.
	// Esplora returns estimates keyed by confirmation target as strings.
	var (
		bestRate   float64
		bestTarget uint32 = math.MaxUint32
	)

	for targetStr, rate := range estimates {
		target, err := strconv.ParseUint(targetStr, 10, 32)
		if err != nil {
			continue
		}

		t := uint32(target)

		// Find the smallest target that is >= our desired target.
		// If none exists, fall back to the largest available target.
		if t >= targetConf && t < bestTarget {
			bestTarget = t
			bestRate = rate
		}
	}

	// If no target >= our desired was found, use the largest available
	// target (highest confirmation time = lowest fee).
	if bestTarget == math.MaxUint32 {
		var maxTarget uint32
		for targetStr, rate := range estimates {
			target, err := strconv.ParseUint(
				targetStr, 10, 32,
			)
			if err != nil {
				continue
			}

			if uint32(target) > maxTarget {
				maxTarget = uint32(target)
				bestRate = rate
			}
		}
	}

	// Ensure at least 1 sat/vB.
	if bestRate < 1.0 {
		bestRate = 1.0
	}

	return btcutil.Amount(math.Ceil(bestRate)), nil
}

// BestBlock returns the current best block height and hash.
func (b *ChainBackend) BestBlock(_ context.Context) (int32, chainhash.Hash,
	error) {

	b.mu.Lock()
	height := b.bestHeight
	hash := b.bestHash
	b.mu.Unlock()

	return height, hash, nil
}

// TestMempoolAccept is not supported by the Esplora backend. This matches
// the LND backend behavior.
func (b *ChainBackend) TestMempoolAccept(_ context.Context, _ ...*wire.MsgTx) (
	[]chainsource.MempoolAcceptResult, error) {

	return nil, fmt.Errorf("test mempool accept not supported by Esplora " +
		"backend")
}

// BroadcastTx broadcasts a transaction via the Esplora API.
func (b *ChainBackend) BroadcastTx(ctx context.Context, tx *wire.MsgTx,
	_ string) error {

	_, err := b.esplora.BroadcastTx(ctx, tx)
	if err != nil {
		return fmt.Errorf("broadcast tx: %w", err)
	}

	return nil
}

// SubmitPackage submits a parent+child package through the Esplora
// /txs/package endpoint. Transactions are serialized in dependency order with
// parents first and the fee-paying child last.
func (b *ChainBackend) SubmitPackage(ctx context.Context, parents []*wire.MsgTx,
	child *wire.MsgTx) error {

	if len(parents) == 0 {
		return fmt.Errorf("submit package: need at least one parent " +
			"transaction")
	}
	if child == nil {
		return fmt.Errorf("submit package: child transaction not " +
			"defined")
	}

	txHexes := make([]string, 0, len(parents)+1)
	for i, tx := range parents {
		var buf bytes.Buffer
		if err := tx.Serialize(&buf); err != nil {
			return fmt.Errorf("serialize parent %d: %w", i, err)
		}

		txHexes = append(txHexes, hex.EncodeToString(buf.Bytes()))
	}

	var childBuf bytes.Buffer
	if err := child.Serialize(&childBuf); err != nil {
		return fmt.Errorf("serialize child: %w", err)
	}
	txHexes = append(txHexes, hex.EncodeToString(childBuf.Bytes()))

	if err := b.esplora.SubmitPackage(ctx, txHexes); err != nil {
		return fmt.Errorf("submit package: %w", err)
	}

	b.log.InfoS(ctx, "Submitted transaction package via Esplora",
		slog.Int("parent_count", len(parents)),
	)

	return nil
}

// RegisterConf registers for confirmation notifications of a transaction.
func (b *ChainBackend) RegisterConf(ctx context.Context, txid *chainhash.Hash,
	pkScript []byte, numConfs uint32, heightHint uint32,
	includeBlock bool) (*chainsource.ConfRegistration, error) {

	confChan := make(chan *chainsource.TxConfirmation, 1)
	cancelCh := make(chan struct{})

	reg := &confRegistration{
		txid:         txid,
		pkScript:     pkScript,
		numConfs:     numConfs,
		heightHint:   heightHint,
		includeBlock: includeBlock,
		confChan:     confChan,
		cancelCh:     cancelCh,
	}

	b.mu.Lock()
	id := b.nextRegID
	b.nextRegID++
	b.confRegs[id] = reg
	b.mu.Unlock()

	var txidStr string
	if txid != nil {
		txidStr = txid.String()
	}

	b.log.DebugS(ctx, "Registered confirmation watch",
		slog.Uint64("reg_id", id),
		slog.String("txid", txidStr),
		slog.Int("num_confs", int(numConfs)),
		slog.Int("height_hint", int(heightHint)),
	)

	cancelFn := func() {
		close(cancelCh)

		b.mu.Lock()
		delete(b.confRegs, id)
		b.mu.Unlock()
	}

	// Run an immediate single-shot check scoped to JUST this
	// registration so a tx that is already buried beyond numConfs
	// at registration time fires synchronously rather than waiting
	// for the next tip event. The previous design re-iterated all
	// pending registrations here; with N concurrent registrations
	// (the boarding-on-restart flow) that produced an O(N²) HTTP
	// burst against Esplora — the very rate-limit problem this
	// PR set out to fix. The per-reg one-shot is O(1).
	//
	// The goroutine is tracked in b.wg so Stop() waits for any
	// in-flight registration check to complete before returning;
	// without that, a slow Esplora response could outlive the
	// chain backend and write into a torn-down subscriber.
	b.wg.Add(1)
	//nolint:contextcheck // registration cancel/Stop own one-shot lifetime
	go b.runConfOneShot(id, reg)

	return &chainsource.ConfRegistration{
		Confirmed: confChan,
		Cancel:    cancelFn,
	}, nil
}

// runConfOneShot performs the per-registration confirmation check
// triggered at RegisterConf time. It snapshots the current best
// height under the chain backend's lock, asks Esplora for this one
// registration's status, and delivers the result if confirmed. The
// goroutine exits on any of: cancellation, stopCh closure, a
// successful delivery, or a non-confirmed status.
func (b *ChainBackend) runConfOneShot(id uint64, reg *confRegistration) {
	defer b.wg.Done()

	select {
	case <-reg.cancelCh:
		return

	case <-b.stopCh:
		return

	default:
	}

	b.mu.Lock()
	currentHeight := b.bestHeight
	b.mu.Unlock()

	conf := b.checkSingleConf(reg, currentHeight)
	if conf == nil {
		return
	}

	select {
	case reg.confChan <- conf:
	case <-reg.cancelCh:
		return

	case <-b.stopCh:
		return
	}

	b.log.DebugS(
		context.Background(),
		"Confirmation registration fulfilled (one-shot)",
		slog.Uint64("reg_id", id),
		slog.Int("block_height", int(conf.BlockHeight)),
	)

	b.mu.Lock()
	delete(b.confRegs, id)
	b.mu.Unlock()
}

// RegisterSpend registers for spend notifications of a transaction output.
func (b *ChainBackend) RegisterSpend(ctx context.Context,
	outpoint *wire.OutPoint, pkScript []byte, heightHint uint32) (
	*chainsource.SpendRegistration, error) {

	spendChan := make(chan *chainsource.SpendDetail, 1)
	cancelCh := make(chan struct{})

	reg := &spendRegistration{
		outpoint:   outpoint,
		pkScript:   pkScript,
		heightHint: heightHint,
		spendChan:  spendChan,
		cancelCh:   cancelCh,
	}

	b.mu.Lock()
	id := b.nextRegID
	b.nextRegID++
	b.spendRegs[id] = reg
	b.mu.Unlock()

	var outpointStr string
	if outpoint != nil {
		outpointStr = outpoint.String()
	}

	b.log.DebugS(ctx, "Registered spend watch",
		slog.Uint64("reg_id", id),
		slog.String("outpoint", outpointStr),
		slog.Int("height_hint", int(heightHint)),
	)

	cancelFn := func() {
		close(cancelCh)

		b.mu.Lock()
		delete(b.spendRegs, id)
		b.mu.Unlock()
	}

	// Per-registration one-shot to handle outpoints that are
	// already spent at registration time. See RegisterConf for the
	// O(N²)-vs-O(1) rationale and the b.wg lifecycle note.
	b.wg.Add(1)
	//nolint:contextcheck // registration cancel/Stop own one-shot lifetime
	go b.runSpendOneShot(id, reg)

	return &chainsource.SpendRegistration{
		Spend:  spendChan,
		Cancel: cancelFn,
	}, nil
}

// runSpendOneShot performs the per-registration spend check
// triggered at RegisterSpend time. It exits on cancellation, stopCh
// closure, a successful delivery, or any non-spent / unconfirmed
// status; the broad checkSpends called from processTipEvent re-runs
// it on every tip advance.
func (b *ChainBackend) runSpendOneShot(id uint64, reg *spendRegistration) {
	defer b.wg.Done()

	select {
	case <-reg.cancelCh:
		return

	case <-b.stopCh:
		return

	default:
	}

	if reg.outpoint == nil {
		return
	}

	detail := b.checkSingleSpend(reg)
	if detail == nil {
		return
	}

	select {
	case reg.spendChan <- detail:
	case <-reg.cancelCh:
		return

	case <-b.stopCh:
		return
	}

	b.log.DebugS(context.Background(),
		"Spend registration fulfilled (one-shot)",
		slog.Uint64("reg_id", id),
		slog.String("outpoint", reg.outpoint.String()),
		slog.String("spender_txid",
			detail.SpenderTxHash.String()))

	b.mu.Lock()
	delete(b.spendRegs, id)
	b.mu.Unlock()
}

// RegisterBlocks registers for new block notifications.
func (b *ChainBackend) RegisterBlocks(_ context.Context) (
	*chainsource.BlockRegistration, error) {

	epochChan := make(chan *chainsource.BlockEpoch, 10)
	cancelCh := make(chan struct{})

	reg := &blockRegistration{
		epochChan: epochChan,
		cancelCh:  cancelCh,
	}

	b.mu.Lock()
	id := b.nextRegID
	b.nextRegID++
	b.blockRegs[id] = reg
	b.mu.Unlock()

	cancelFn := func() {
		close(cancelCh)

		b.mu.Lock()
		delete(b.blockRegs, id)
		b.mu.Unlock()
	}

	return &chainsource.BlockRegistration{
		Epochs: epochChan,
		Cancel: cancelFn,
	}, nil
}

// handleTipEvents drains TipBlock events from the shared poller and
// translates them into chain backend work: emit a BlockEpoch to each
// block-registration subscriber, advance the cached tip, and re-check
// pending confirmation/spend registrations.
//
// On stopCh the loop exits and cancels its subscription so the
// poller does not waste effort fanning to a dead consumer. The
// subscription's Quit channel covers the inverse direction: if the
// poller is shut down externally, we exit promptly without waiting
// for stopCh.
func (b *ChainBackend) handleTipEvents(sub *TipSubscription) {
	defer b.wg.Done()
	defer sub.Cancel()

	heartbeat := time.NewTicker(recheckHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case event, ok := <-sub.Updates():
			if !ok {
				return
			}

			b.processTipEvent(event)

		case <-heartbeat.C:
			// Re-run the broad checks even when the tip
			// hasn't moved. Esplora's status indexer lags
			// the block-found event by 1-3 seconds, so a
			// processTipEvent that ran before the indexer
			// caught up would otherwise not retry until
			// the next block lands. Coalesced via the same
			// singleflight keys used by processTipEvent so
			// a tip event arriving on the same tick does
			// not produce two parallel scans.
			b.runRecheckHeartbeat()

		case <-sub.Quit():
			return

		case <-b.stopCh:
			return
		}
	}
}

// runRecheckHeartbeat is the heartbeat-tick path that re-runs
// checkConfirmations and checkSpends without a new tip event. It
// goes through the same singleflight keys as processTipEvent so a
// concurrent tip-driven scan and a heartbeat-driven scan share one
// in-flight call rather than running in parallel.
func (b *ChainBackend) runRecheckHeartbeat() {
	_, _, _ = b.sf.Do(sfKeyCheckConfirmations,
		func() (interface{}, error) {
			b.checkConfirmations()

			return nil, nil
		})

	_, _, _ = b.sf.Do(sfKeyCheckSpends,
		func() (interface{}, error) {
			b.checkSpends()

			return nil, nil
		})
}

// processTipEvent applies one TipBlock to the registration maps:
// fans out a BlockEpoch to every block subscriber, advances the
// cached tip under the same lock, and re-checks confirmation +
// spend registrations.
func (b *ChainBackend) processTipEvent(event *TipBlock) {
	if event == nil || event.Header == nil {
		return
	}

	b.log.DebugS(context.Background(), "New block processed",
		slog.Int("height", int(event.Height)),
		slog.String("hash", event.Hash.String()),
	)

	epoch := &chainsource.BlockEpoch{
		Hash:      event.Hash,
		Height:    event.Height,
		Timestamp: event.Header.Timestamp,
	}

	// Snapshot the registration set under the lock and update the
	// cached tip, then drop the lock before fanning out. Holding
	// b.mu across N channel sends serializes every other lock
	// acquirer (registrations, confirmation checks) behind the
	// fan-out, and a future maintainer who removes the `default:`
	// case below would silently introduce a deadlock — separating
	// the snapshot from the send makes that mistake impossible.
	b.mu.Lock()
	b.bestHeight = event.Height
	b.bestHash = event.Hash
	regs := make([]*blockRegistration, 0, len(b.blockRegs))
	for _, reg := range b.blockRegs {
		regs = append(regs, reg)
	}
	b.mu.Unlock()

	for _, reg := range regs {
		select {
		case reg.epochChan <- epoch:
		case <-reg.cancelCh:
		default:
			// Channel full, skip this block for this
			// subscriber. The subscriber will catch up on
			// the next event.
		}
	}

	// Coalesce concurrent broad re-checks via singleflight: if a
	// previous tip event's checkConfirmations / checkSpends is
	// still running when a new tip event arrives we share its
	// result rather than starting a parallel scan. Each scan is
	// already O(N) HTTP calls in the registration count; without
	// this guard a fast burst of blocks would multiply the load
	// against an already-rate-limited Esplora.
	_, _, _ = b.sf.Do(sfKeyCheckConfirmations,
		func() (interface{}, error) {
			b.checkConfirmations()

			return nil, nil
		})

	_, _, _ = b.sf.Do(sfKeyCheckSpends,
		func() (interface{}, error) {
			b.checkSpends()

			return nil, nil
		})
}

// checkConfirmations iterates over all pending confirmation registrations
// and checks their status via the Esplora API.
func (b *ChainBackend) checkConfirmations() {
	b.mu.Lock()
	regs := make(map[uint64]*confRegistration, len(b.confRegs))
	for id, reg := range b.confRegs {
		regs[id] = reg
	}
	currentHeight := b.bestHeight
	b.mu.Unlock()

	for id, reg := range regs {
		// Skip cancelled registrations.
		select {
		case <-reg.cancelCh:
			continue

		default:
		}

		conf := b.checkSingleConf(reg, currentHeight)
		if conf == nil {
			continue
		}

		// Send the confirmation.
		select {
		case reg.confChan <- conf:
		case <-reg.cancelCh:
			continue
		}

		b.log.DebugS(context.Background(),
			"Confirmation registration fulfilled",
			slog.Uint64("reg_id", id),
			slog.Int("block_height",
				int(conf.BlockHeight)))

		// Remove the fulfilled registration.
		b.mu.Lock()
		delete(b.confRegs, id)
		b.mu.Unlock()
	}
}

// checkSpends iterates over all pending spend registrations and checks
// their status via the Esplora API.
// checkSingleConf checks whether a single confirmation registration is
// satisfied. When the registration has a txid, it queries the tx status
// directly. When the txid is nil, it scans the script's UTXOs to find
// a confirmed transaction matching the pkScript.
func (b *ChainBackend) checkSingleConf(reg *confRegistration,
	currentHeight int32) *chainsource.TxConfirmation {

	if reg.txid != nil {
		return b.checkConfByTxid(reg, currentHeight)
	}

	return b.checkConfByScript(reg, currentHeight)
}

// checkConfByTxid checks confirmation status for a registration that
// has an explicit txid.
func (b *ChainBackend) checkConfByTxid(reg *confRegistration,
	currentHeight int32) *chainsource.TxConfirmation {

	status, err := b.esplora.GetTxStatus(context.Background(), *reg.txid)
	if err != nil {
		return nil
	}

	if !status.Confirmed {
		return nil
	}

	// Check if we have enough confirmations.
	confHeight := int32(status.BlockHeight)
	confs := currentHeight - confHeight + 1
	if confs < int32(reg.numConfs) {
		return nil
	}

	// Build the confirmation event.
	blockHash, err := chainhash.NewHashFromStr(status.BlockHash)
	if err != nil {
		return nil
	}

	tx, err := b.esplora.GetRawTx(context.Background(), *reg.txid)
	if err != nil {
		return nil
	}

	conf := &chainsource.TxConfirmation{
		BlockHash:   blockHash,
		BlockHeight: status.BlockHeight,
		Tx:          tx,
	}

	// Fetch the full block if requested.
	if reg.includeBlock {
		block, err := b.esplora.GetRawBlock(
			context.Background(), *blockHash,
		)
		if err == nil {
			conf.Block = block
		}
	}

	return conf
}

// checkConfByScript checks confirmation status by scanning the pkScript's
// UTXOs for a confirmed transaction. This is used when txid is nil and
// the caller wants to monitor for any payment to the script.
func (b *ChainBackend) checkConfByScript(reg *confRegistration,
	currentHeight int32) *chainsource.TxConfirmation {

	utxos, err := b.esplora.GetScriptUtxos(
		context.Background(), reg.pkScript,
	)
	if err != nil {
		return nil
	}

	for _, utxo := range utxos {
		if !utxo.Status.Confirmed {
			continue
		}

		confHeight := int32(utxo.Status.BlockHeight)
		confs := currentHeight - confHeight + 1
		if confs < int32(reg.numConfs) {
			continue
		}

		txid, err := chainhash.NewHashFromStr(utxo.Txid)
		if err != nil {
			continue
		}

		blockHash, err := chainhash.NewHashFromStr(
			utxo.Status.BlockHash,
		)
		if err != nil {
			continue
		}

		tx, err := b.esplora.GetRawTx(context.Background(), *txid)
		if err != nil {
			continue
		}

		conf := &chainsource.TxConfirmation{
			BlockHash:   blockHash,
			BlockHeight: uint32(utxo.Status.BlockHeight),
			Tx:          tx,
		}

		if reg.includeBlock {
			block, err := b.esplora.GetRawBlock(
				context.Background(), *blockHash,
			)
			if err == nil {
				conf.Block = block
			}
		}

		return conf
	}

	return nil
}

// checkSpends iterates over all pending spend registrations and checks
// their status via the Esplora API.
func (b *ChainBackend) checkSpends() {
	b.mu.Lock()
	regs := make(map[uint64]*spendRegistration, len(b.spendRegs))
	for id, reg := range b.spendRegs {
		regs[id] = reg
	}
	b.mu.Unlock()

	for id, reg := range regs {
		// Skip cancelled registrations.
		select {
		case <-reg.cancelCh:
			continue

		default:
		}

		if reg.outpoint == nil {
			continue
		}

		detail := b.checkSingleSpend(reg)
		if detail == nil {
			continue
		}

		// Send the spend detail.
		select {
		case reg.spendChan <- detail:
		case <-reg.cancelCh:
			continue
		}

		b.log.DebugS(context.Background(),
			"Spend registration fulfilled",
			slog.Uint64("reg_id", id),
			slog.String("outpoint",
				reg.outpoint.String()),
			slog.String(
				"spender_txid", detail.SpenderTxHash.String(),
			))

		// Remove the fulfilled registration.
		b.mu.Lock()
		delete(b.spendRegs, id)
		b.mu.Unlock()
	}
}

// checkSingleSpend resolves the spend status of a single spend
// registration via Esplora. Returns nil when the outpoint is not yet
// confirmed-spent, when any HTTP / parse error occurs, or when the
// registration has no outpoint. The caller is responsible for
// delivery, logging, and removing the fulfilled registration; this
// helper only resolves the on-chain question.
func (b *ChainBackend) checkSingleSpend(
	reg *spendRegistration) *chainsource.SpendDetail {

	if reg.outpoint == nil {
		return nil
	}

	outspend, err := b.esplora.GetOutspend(
		context.Background(), reg.outpoint.Hash, reg.outpoint.Index,
	)
	if err != nil {
		return nil
	}

	if !outspend.Spent || !outspend.Status.Confirmed {
		return nil
	}

	spenderHash, err := chainhash.NewHashFromStr(outspend.Txid)
	if err != nil {
		return nil
	}

	spendingTx, err := b.esplora.GetRawTx(
		context.Background(), *spenderHash,
	)
	if err != nil {
		return nil
	}

	return &chainsource.SpendDetail{
		SpentOutPoint:     reg.outpoint,
		SpenderTxHash:     spenderHash,
		SpendingTx:        spendingTx,
		SpenderInputIndex: outspend.Vin,
		SpendingHeight:    int32(outspend.Status.BlockHeight),
	}
}

// Compile-time check that ChainBackend implements
// chainsource.ChainBackend.
var _ chainsource.ChainBackend = (*ChainBackend)(nil)
