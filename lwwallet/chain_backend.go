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
	fn "github.com/lightningnetwork/lnd/fn/v2"
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

// regState is the lifecycle state of a conf/spend registration.
//
// stateWatching: no positive event has been delivered yet (or the
// last positive event was reorged out). The next checkSingle... that
// finds the tx confirmed / outpoint spent will emit Confirmed /
// Spend and transition to statePositive.
//
// statePositive: a Confirmed / Spend event has been delivered and the
// associated block-hash is cached on the registration. The next
// checkSingle... that does NOT find the tx confirmed / outpoint
// spent in the same block leaves the registration alone (the
// chainsource actor handles Done synthesis at FinalityDepth via
// block epochs). A reorg event that names the cached block-hash in
// its Disconnected set fires Reorged, resets cached state, and
// transitions back to stateWatching so the next re-check can fire
// Confirmed / Spend again on the new chain.
type regState uint8

const (
	stateWatching regState = iota
	statePositive
)

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

	// reorgChan is the channel that fires when a previously delivered
	// confirmation is reorged out of the canonical chain. Buffered to
	// 1; never written by the backend before stateWatching has been
	// re-entered. The value is the chainsource ordering sequence; this
	// backend does not stamp sequences (it emits conf/reorg from a
	// single ordered handler goroutine), so it always sends 0, which
	// the chainsource actor treats as always-apply.
	reorgChan chan uint64

	// doneChan is allocated for API symmetry with the chainsource
	// contract. The Esplora-backed backend does not write to it: the
	// chainsource ConfActor synthesizes Done at FinalityDepth from
	// block epochs once the backend stops re-firing events.
	doneChan chan struct{}

	// cancelCh signals that this registration has been cancelled.
	cancelCh chan struct{}

	// regMu guards the fields below that mutate over the
	// registration's lifetime. It is held only briefly during state
	// transitions; channel sends are performed without it.
	regMu sync.Mutex

	// state is the current lifecycle state.
	state regState

	// lastBlockHash is the BlockHash of the last delivered
	// confirmation when state == statePositive. Used to detect when
	// a reorg's Disconnected hash list invalidates this
	// registration's last event.
	lastBlockHash chainhash.Hash
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

	// reorgChan is the channel that fires when a previously delivered
	// spend is reorged out of the canonical chain. Buffered to 1.
	reorgChan chan uint64

	// doneChan is allocated for API symmetry with the chainsource
	// contract. The Esplora-backed backend does not write to it: the
	// chainsource SpendActor synthesizes Done at FinalityDepth.
	doneChan chan struct{}

	// cancelCh signals that this registration has been cancelled.
	cancelCh chan struct{}

	// regMu guards the fields below.
	regMu sync.Mutex

	// state is the current lifecycle state.
	state regState

	// lastSpenderHash is the SpenderTxHash of the last delivered
	// spend when state == statePositive.
	lastSpenderHash chainhash.Hash

	// lastSpendingBlockHash is the block hash that the last
	// delivered spend confirmed in, parsed from the same
	// outspend response that confirmed the spend. Reorg
	// comparison against ReorgEvent.Disconnected uses this so
	// the spend-watch path does not need an extra HTTP round-trip.
	lastSpendingBlockHash chainhash.Hash
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

	height, hash, _, chainSub, err :=
		b.tipPoller.BestBlockAndSubscribeChain()
	if err != nil {
		return fmt.Errorf("subscribe to tip poller: %w", err)
	}

	b.mu.Lock()
	b.bestHeight = height
	b.bestHash = hash
	b.mu.Unlock()

	b.wg.Add(1)
	go b.handleChainEvents(chainSub)

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
// The registration is reorg-aware: the returned ConfRegistration's
// Reorged channel fires when a previously delivered confirmation is
// reorged out of the canonical chain, and the Confirmed channel may
// fire again when the tx re-confirms on the new chain. The Done
// channel is allocated but never written by this backend; the
// chainsource ConfActor synthesizes Done at its configured
// FinalityDepth using block epochs.
func (b *ChainBackend) RegisterConf(ctx context.Context, txid *chainhash.Hash,
	pkScript []byte, numConfs uint32, heightHint uint32,
	includeBlock bool) (*chainsource.ConfRegistration, error) {

	confChan := make(chan *chainsource.TxConfirmation, 1)
	reorgChan := make(chan uint64, 1)
	doneChan := make(chan struct{}, 1)
	cancelCh := make(chan struct{})

	reg := &confRegistration{
		txid:         txid,
		pkScript:     pkScript,
		numConfs:     numConfs,
		heightHint:   heightHint,
		includeBlock: includeBlock,
		confChan:     confChan,
		reorgChan:    reorgChan,
		doneChan:     doneChan,
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
		b.cancelConfReg(id, reg)
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
		Reorged:   reorgChan,
		Done:      doneChan,
		Cancel:    cancelFn,
	}, nil
}

// cancelConfReg tears down a confirmation registration. It is safe
// to invoke from any state; the close(cancelCh) is guarded by a
// once-style check to make double-Cancel a no-op.
func (b *ChainBackend) cancelConfReg(id uint64, reg *confRegistration) {
	reg.regMu.Lock()
	select {
	case <-reg.cancelCh:
		// Already cancelled.
		reg.regMu.Unlock()

		return

	default:
	}
	close(reg.cancelCh)
	reg.regMu.Unlock()

	b.mu.Lock()
	delete(b.confRegs, id)
	b.mu.Unlock()
}

// runConfOneShot performs the per-registration confirmation check
// triggered at RegisterConf time. It snapshots the current best
// height under the chain backend's lock, asks Esplora for this one
// registration's status, and delivers the result if confirmed. The
// registration is NOT deleted after delivery: a reorg may later
// reset it to stateWatching and the next re-check must fire
// Confirmed again on the new chain.
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

	b.deliverConfIfNew(id, reg, currentHeight)
}

// deliverConfIfNew runs a confirmation re-check and fires the
// Confirmed channel iff the registration is in stateWatching and a
// confirmation is now available. It is the single delivery path
// shared by the one-shot at registration time and the broad
// re-check driven by tip / reorg events. The registration's regMu
// is held only across the state read and the state transition; the
// channel send happens outside the lock so a slow consumer never
// blocks the broad re-check goroutine.
func (b *ChainBackend) deliverConfIfNew(id uint64, reg *confRegistration,
	currentHeight int32) {

	reg.regMu.Lock()
	if reg.state == statePositive {
		reg.regMu.Unlock()

		return
	}
	reg.regMu.Unlock()

	conf := b.checkSingleConf(reg, currentHeight)
	if conf == nil {
		return
	}

	// Re-check state under the lock: a concurrent reorg handler
	// could have flipped us back to stateWatching after the check
	// started, but it could not have flipped us forward to
	// statePositive (that's exclusively this function's job).
	reg.regMu.Lock()
	if reg.state == statePositive {
		reg.regMu.Unlock()

		return
	}
	reg.state = statePositive
	if conf.BlockHash != nil {
		reg.lastBlockHash = *conf.BlockHash
	}
	reg.regMu.Unlock()

	select {
	case reg.confChan <- conf:
	case <-reg.cancelCh:
		return

	case <-b.stopCh:
		return
	}

	b.log.DebugS(
		context.Background(),
		"Confirmation registration fulfilled",
		slog.Uint64("reg_id", id),
		slog.Int("block_height", int(conf.BlockHeight)),
	)
}

// RegisterSpend registers for spend notifications of a transaction output.
// The registration is reorg-aware: the returned SpendRegistration's
// Reorged channel fires when a previously delivered spend is reorged
// out of the canonical chain, and the Spend channel may fire again
// when the outpoint is re-spent on the new chain. The Done channel
// is allocated but never written by this backend; the chainsource
// SpendActor synthesizes Done at its configured FinalityDepth.
func (b *ChainBackend) RegisterSpend(ctx context.Context,
	outpoint *wire.OutPoint, pkScript []byte, heightHint uint32) (
	*chainsource.SpendRegistration, error) {

	spendChan := make(chan *chainsource.SpendDetail, 1)
	reorgChan := make(chan uint64, 1)
	doneChan := make(chan struct{}, 1)
	cancelCh := make(chan struct{})

	reg := &spendRegistration{
		outpoint:   outpoint,
		pkScript:   pkScript,
		heightHint: heightHint,
		spendChan:  spendChan,
		reorgChan:  reorgChan,
		doneChan:   doneChan,
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
		b.cancelSpendReg(id, reg)
	}

	// Per-registration one-shot to handle outpoints that are
	// already spent at registration time. See RegisterConf for the
	// O(N²)-vs-O(1) rationale and the b.wg lifecycle note.
	b.wg.Add(1)
	//nolint:contextcheck // registration cancel/Stop own one-shot lifetime
	go b.runSpendOneShot(id, reg)

	return &chainsource.SpendRegistration{
		Spend:   spendChan,
		Reorged: reorgChan,
		Done:    doneChan,
		Cancel:  cancelFn,
	}, nil
}

// cancelSpendReg tears down a spend registration. Idempotent: a
// double-Cancel is treated as a no-op.
func (b *ChainBackend) cancelSpendReg(id uint64, reg *spendRegistration) {
	reg.regMu.Lock()
	select {
	case <-reg.cancelCh:
		reg.regMu.Unlock()

		return

	default:
	}
	close(reg.cancelCh)
	reg.regMu.Unlock()

	b.mu.Lock()
	delete(b.spendRegs, id)
	b.mu.Unlock()
}

// runSpendOneShot performs the per-registration spend check
// triggered at RegisterSpend time. The registration is NOT deleted
// after delivery: a reorg may later reset it to stateWatching and
// the next re-check must fire Spend again on the new chain.
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

	b.deliverSpendIfNew(id, reg)
}

// deliverSpendIfNew runs a spend re-check and fires the Spend
// channel iff the registration is in stateWatching and a spend is
// now available. Mirror of deliverConfIfNew; see that function's
// comment for the regMu / channel-send ordering rationale.
func (b *ChainBackend) deliverSpendIfNew(id uint64, reg *spendRegistration) {
	reg.regMu.Lock()
	if reg.state == statePositive {
		reg.regMu.Unlock()

		return
	}
	reg.regMu.Unlock()

	// checkSingleSpend returns the spending block hash alongside
	// the detail (parsed from the same /outspend response that
	// confirmed the spend). A zero hash here means the response
	// did not parse and reorgSpendReg will fall back to a
	// conservative re-check rather than try to match against
	// ReorgEvent.Disconnected.
	detail, spendingBlockHash, err := b.checkSingleSpend(reg)
	if err != nil {
		b.log.DebugS(
			context.Background(),
			"Spend status temporarily unavailable",
			slog.Uint64("reg_id", id),
			btclog.Fmt("err", "%v", err),
		)

		return
	}
	if detail == nil {
		return
	}

	reg.regMu.Lock()
	if reg.state == statePositive {
		reg.regMu.Unlock()

		return
	}
	reg.state = statePositive
	if detail.SpenderTxHash != nil {
		reg.lastSpenderHash = *detail.SpenderTxHash
	}
	reg.lastSpendingBlockHash = spendingBlockHash
	reg.regMu.Unlock()

	select {
	case reg.spendChan <- detail:
	case <-reg.cancelCh:
		return

	case <-b.stopCh:
		return
	}

	b.log.DebugS(context.Background(),
		"Spend registration fulfilled",
		slog.Uint64("reg_id", id),
		slog.String("outpoint", reg.outpoint.String()),
		slog.String("spender_txid",
			detail.SpenderTxHash.String()))
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

// processReorgEvent reconciles all active registrations with one
// ReorgEvent. It is invoked from the unified handleChainEvents
// goroutine in producer order: a ReorgEvent is fully processed
// (registration state reset, Reorged channels fired) before any
// subsequent TipBlock dispatches block epochs or runs broad
// re-check on the replacement chain. Registrations whose last
// positive event names a disconnected hash are reset and
// re-checked; all others are left alone (the broad tip-driven
// re-check still runs separately).
func (b *ChainBackend) processReorgEvent(event *ReorgEvent) {
	if event == nil {
		return
	}

	b.log.InfoS(context.Background(), "Processing reorg",
		slog.Int("fork_height", int(event.ForkHeight)),
		slog.Int("disconnected", len(event.Disconnected)),
		slog.Int("connected", len(event.Connected)),
	)

	disconnectedSet := make(
		map[chainhash.Hash]struct{}, len(event.Disconnected),
	)
	for _, hash := range event.Disconnected {
		disconnectedSet[hash] = struct{}{}
	}

	// Snapshot the registration sets under the chain backend
	// lock so we don't hold it across the per-registration
	// channel sends below.
	b.mu.Lock()
	confRegs := make(map[uint64]*confRegistration, len(b.confRegs))
	for id, reg := range b.confRegs {
		confRegs[id] = reg
	}
	spendRegs := make(map[uint64]*spendRegistration, len(b.spendRegs))
	for id, reg := range b.spendRegs {
		spendRegs[id] = reg
	}
	currentHeight := b.bestHeight
	b.mu.Unlock()

	for id, reg := range confRegs {
		b.reorgConfReg(id, reg, disconnectedSet, currentHeight)
	}
	for id, reg := range spendRegs {
		b.reorgSpendReg(id, reg, disconnectedSet)
	}
}

// reorgConfReg processes one confirmation registration against a
// reorg event. If the registration is in statePositive and its
// last-known block hash is in disconnectedSet, fire Reorged, reset
// to stateWatching, and run a fresh check that may immediately
// fire Confirmed against the new chain.
func (b *ChainBackend) reorgConfReg(id uint64, reg *confRegistration,
	disconnectedSet map[chainhash.Hash]struct{}, currentHeight int32) {

	select {
	case <-reg.cancelCh:
		return

	default:
	}

	reg.regMu.Lock()
	if reg.state != statePositive {
		reg.regMu.Unlock()

		return
	}
	cachedHash := reg.lastBlockHash
	reg.regMu.Unlock()

	// Fast path: the registration's cached block hash appears in
	// the reorg event's disconnected set. This covers every
	// reorg of a block we previously broadcast.
	_, fastHit := disconnectedSet[cachedHash]

	if !fastHit {
		// Fallback: the registration may have delivered against
		// a block older than the poller's seeded hash history
		// (e.g. on a fresh daemon where RegisterConf landed an
		// immediate historical positive). The reorg's
		// disconnected set is bounded by recentHashes, so a
		// reorg deep enough to invalidate that historical block
		// would not appear here. Re-query canonical status and
		// compare against the cached block hash.
		//
		// confirmedBlockHash returns a tri-state so we never
		// fire a spurious reorg: Some(h) means the tx is still
		// confirmed in block h (independent of the numConfs
		// threshold), None means it is definitively unconfirmed,
		// and a non-nil error means canonical status could not
		// be determined right now.
		gotHash, err := b.confirmedBlockHash(reg)
		switch {
		// Transient backend failure: we cannot tell whether this
		// is a reorg. Leave the registration in statePositive and
		// bail; a later tip or reorg event re-evaluates once the
		// backend recovers. Firing Reorged here would strand the
		// consumer on a false alarm.
		case err != nil:
			b.log.DebugS(
				context.Background(),
				"Skipping conf reorg eval; canonical "+
					"status undeterminable",
				slog.Uint64("reg_id", id),
				btclog.Fmt("err", "%v", err),
			)

			return

		// Still confirmed in the same block: definitively not a
		// reorg of this registration.
		case gotHash.IsSome() &&
			gotHash.UnsafeFromSome() == cachedHash:
			return
		}

		// Otherwise the tx is confirmed elsewhere or definitively
		// unconfirmed: fall through and fire Reorged.
	}

	reg.regMu.Lock()
	if reg.state != statePositive {
		reg.regMu.Unlock()

		return
	}
	reg.state = stateWatching
	reg.lastBlockHash = chainhash.Hash{}
	reg.regMu.Unlock()

	// Fire Reorged non-blocking: the channel is buffered to 1
	// and the consumer (chainsource conf actor) is single-
	// threaded so a missed coalesced reorg is correct — the
	// consumer will re-query state anyway.
	select {
	case reg.reorgChan <- uint64(0):
	case <-reg.cancelCh:
		return

	default:
		b.log.DebugS(
			context.Background(),
			"Conf reorged signal coalesced",
			slog.Uint64("reg_id", id),
		)
	}

	b.log.InfoS(
		context.Background(),
		"Conf registration reorged; re-checking",
		slog.Uint64("reg_id", id),
	)

	// Re-check now so a re-confirmation on the new chain fires
	// Confirmed in the same reorg-handler turn.
	b.deliverConfIfNew(id, reg, currentHeight)
}

// reorgSpendReg is the spend-side equivalent of reorgConfReg.
func (b *ChainBackend) reorgSpendReg(id uint64, reg *spendRegistration,
	disconnectedSet map[chainhash.Hash]struct{}) {

	select {
	case <-reg.cancelCh:
		return

	default:
	}

	reg.regMu.Lock()
	if reg.state != statePositive {
		reg.regMu.Unlock()

		return
	}
	cachedBlockHash := reg.lastSpendingBlockHash
	cachedSpenderHash := reg.lastSpenderHash
	reg.regMu.Unlock()

	// Fast path: the cached spending-block hash appears in the
	// reorg event's disconnected set. Empty cachedBlockHash
	// (e.g. delivered before the outspend response was parseable)
	// falls through to the fallback re-query rather than being
	// treated as an automatic hit.
	fastHit := false
	if cachedBlockHash != (chainhash.Hash{}) {
		_, fastHit = disconnectedSet[cachedBlockHash]
	}

	if !fastHit {
		// Fallback: re-query canonical chain. Covers (a) regs
		// delivered against a block older than the poller's
		// seeded hash history, and (b) regs whose
		// cachedBlockHash is zero because the outspend response
		// was unparseable at delivery time. If the outpoint is
		// still spent by the same spender in the same block,
		// no reorg for this registration.
		current, currentBlock, err := b.checkSingleSpend(reg)
		if err != nil {
			b.log.DebugS(
				context.Background(),
				"Skipping spend reorg eval; canonical "+
					"status undeterminable",
				slog.Uint64("reg_id", id),
				btclog.Fmt("err", "%v", err),
			)

			return
		}
		if current != nil &&
			current.SpenderTxHash != nil &&
			*current.SpenderTxHash == cachedSpenderHash &&
			cachedBlockHash != (chainhash.Hash{}) &&
			currentBlock == cachedBlockHash {
			return
		}
	}

	reg.regMu.Lock()
	if reg.state != statePositive {
		reg.regMu.Unlock()

		return
	}
	reg.state = stateWatching
	reg.lastSpenderHash = chainhash.Hash{}
	reg.lastSpendingBlockHash = chainhash.Hash{}
	reg.regMu.Unlock()

	select {
	case reg.reorgChan <- uint64(0):
	case <-reg.cancelCh:
		return

	default:
		b.log.DebugS(
			context.Background(),
			"Spend reorged signal coalesced",
			slog.Uint64("reg_id", id),
		)
	}

	b.log.InfoS(
		context.Background(),
		"Spend registration reorged; re-checking",
		slog.Uint64("reg_id", id),
	)

	b.deliverSpendIfNew(id, reg)
}

// handleChainEvents drains the unified chain stream and dispatches
// each event in producer order. Using a single subscription + single
// goroutine guarantees that a ReorgEvent is fully processed
// (registration state reset, Reorged channels fired) before any
// post-reorg TipBlock drives block-epoch dispatch or broad re-check
// — otherwise a block-epoch driven Confirmed could land on a stale
// registration before reorgConfReg has a chance to reset it.
func (b *ChainBackend) handleChainEvents(sub *ChainSubscription) {
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

			switch {
			case event.Reorg != nil:
				b.processReorgEvent(event.Reorg)

			case event.Tip != nil:
				b.processTipEvent(event.Tip)
			}

		case <-heartbeat.C:
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

// checkConfirmations iterates over all pending confirmation
// registrations and re-checks their status via the Esplora API.
// Registrations already in statePositive are skipped: the reorg
// handler is responsible for transitioning them back to
// stateWatching, and an unconditional re-check would just burn
// Esplora calls for no behavior change (the chainsource actor's
// FinalityDepth synthesis handles Done from block epochs).
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

		b.deliverConfIfNew(id, reg, currentHeight)
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

// confirmedBlockHash reports the block hash a registration's transaction is
// currently confirmed in, independent of the registration's numConfs
// threshold. It exists for the reorg-detection fallback, which must
// distinguish three states that checkSingleConf collapses into a single nil:
//
//   - Some(hash), nil: the tx is confirmed in block hash (any depth);
//   - None, nil:        the tx is definitively unconfirmed (genuine reorg);
//   - None, err:        canonical status could not be determined right now
//     (transient backend error) — callers MUST NOT treat this as a reorg.
//
// Unlike checkSingleConf it never gates on numConfs, because a reorg that
// merely reduces a tx's confirmation depth without moving it out of its block
// is not a reorg of that registration.
func (b *ChainBackend) confirmedBlockHash(reg *confRegistration) (
	fn.Option[chainhash.Hash], error) {

	none := fn.None[chainhash.Hash]()

	// Explicit-txid registrations resolve via a direct status lookup.
	if reg.txid != nil {
		status, err := b.esplora.GetTxStatus(
			context.Background(), *reg.txid,
		)
		if err != nil {
			return none, err
		}
		if !status.Confirmed {
			return none, nil
		}

		hash, err := chainhash.NewHashFromStr(status.BlockHash)
		if err != nil {
			return none, err
		}

		return fn.Some(*hash), nil
	}

	// Script registrations resolve via the first confirmed UTXO paying
	// the watched script.
	utxos, err := b.esplora.GetScriptUtxos(
		context.Background(), reg.pkScript,
	)
	if err != nil {
		return none, err
	}

	for _, utxo := range utxos {
		if !utxo.Status.Confirmed {
			continue
		}

		hash, err := chainhash.NewHashFromStr(utxo.Status.BlockHash)
		if err != nil {
			return none, err
		}

		return fn.Some(*hash), nil
	}

	return none, nil
}

// checkSpends iterates over all pending spend registrations and
// re-checks their status via the Esplora API. Registrations in
// statePositive are skipped — see checkConfirmations for the
// rationale.
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

		b.deliverSpendIfNew(id, reg)
	}
}

// checkSingleSpend resolves the spend status of a single spend
// registration via Esplora. A nil detail and nil error means the outpoint is
// definitively not confirmed-spent (or the registration has no outpoint). A
// non-nil error means canonical status could not be determined and MUST NOT be
// interpreted as a reorg. The second return value is the block hash containing
// the spending tx; a zero hash means only that the hash was unparseable while
// the spend itself was valid.
func (b *ChainBackend) checkSingleSpend(reg *spendRegistration) (
	*chainsource.SpendDetail, chainhash.Hash, error) {

	if reg.outpoint == nil {
		return nil, chainhash.Hash{}, nil
	}

	outspend, err := b.esplora.GetOutspend(
		context.Background(), reg.outpoint.Hash, reg.outpoint.Index,
	)
	if err != nil {
		return nil, chainhash.Hash{}, err
	}

	if !outspend.Spent || !outspend.Status.Confirmed {
		return nil, chainhash.Hash{}, nil
	}

	spenderHash, err := chainhash.NewHashFromStr(outspend.Txid)
	if err != nil {
		return nil, chainhash.Hash{}, err
	}

	spendingTx, err := b.esplora.GetRawTx(
		context.Background(), *spenderHash,
	)
	if err != nil {
		return nil, chainhash.Hash{}, err
	}

	// outspend.Status.BlockHash is hex from the same /outspend
	// response that confirmed the spend; an unparseable value is
	// not fatal — the spend itself is valid and the reorg path can
	// fall back to a conservative re-check (see reorgSpendReg).
	var spendingBlockHash chainhash.Hash
	if outspend.Status.BlockHash != "" {
		h, err := chainhash.NewHashFromStr(outspend.Status.BlockHash)
		if err == nil {
			spendingBlockHash = *h
		}
	}

	return &chainsource.SpendDetail{
		SpentOutPoint:     reg.outpoint,
		SpenderTxHash:     spenderHash,
		SpendingTx:        spendingTx,
		SpenderInputIndex: outspend.Vin,
		SpendingHeight:    int32(outspend.Status.BlockHeight),
	}, spendingBlockHash, nil
}

// Compile-time check that ChainBackend implements
// chainsource.ChainBackend.
var _ chainsource.ChainBackend = (*ChainBackend)(nil)
