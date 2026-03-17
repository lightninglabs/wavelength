package lwwallet

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
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

// CPFPWallet defines the wallet operations needed by the chain backend
// to automatically construct CPFP child transactions for package relay.
// This interface is satisfied by the lwwallet.Wallet type.
type CPFPWallet interface {
	// ListUnspentWitness returns confirmed wallet UTXOs within the
	// given confirmation range.
	ListUnspentWitness(minConfs, maxConfs int32) ([]*lnwallet.Utxo, error)

	// NewAddress generates a new receiving address.
	NewAddress(ctx context.Context) (btcutil.Address, error)

	// ComputeInputScript generates a complete witness for a wallet-
	// owned input. The signer auto-detects the key and signing
	// method for standard script templates (P2TR, P2WKH, etc.).
	ComputeInputScript(tx *wire.MsgTx,
		signDesc *input.SignDescriptor) (*input.Script, error)
}

// ChainBackend implements chainsource.ChainBackend using an Esplora HTTP
// client with polling-based chain monitoring. The backend periodically
// polls the Esplora API for new blocks and checks pending confirmation
// and spend registrations against the current chain state.
type ChainBackend struct {
	esplora      *EsploraClient
	pollInterval time.Duration

	// wallet provides UTXO selection, address generation, and signing
	// for automatic CPFP child construction in SubmitPackage. When
	// set and the caller provides parents but no child, the backend
	// automatically builds a fee-paying child transaction. Set via
	// SetWallet after wallet initialization.
	wallet CPFPWallet

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

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewChainBackend creates a new Esplora-backed chain backend. The
// pollInterval controls how frequently the backend checks for new blocks
// and updates to pending registrations.
func NewChainBackend(esplora *EsploraClient,
	pollInterval time.Duration,
	logger btclog.Logger) *ChainBackend {

	return &ChainBackend{
		esplora:      esplora,
		pollInterval: pollInterval,
		log:          logger,
		confRegs:     make(map[uint64]*confRegistration),
		spendRegs:    make(map[uint64]*spendRegistration),
		blockRegs:    make(map[uint64]*blockRegistration),
		stopCh:       make(chan struct{}),
	}
}

// SetWallet attaches a wallet for automatic CPFP child construction.
// This must be called after the wallet is fully initialized but before
// SubmitPackage is used. The chain backend is created before the wallet
// in the initialization sequence, so this setter bridges the gap.
func (b *ChainBackend) SetWallet(w CPFPWallet) {
	b.mu.Lock()
	b.wallet = w
	b.mu.Unlock()
}

// Start initializes the chain backend by fetching the current chain tip
// and starting the polling loop.
func (b *ChainBackend) Start() error {
	// Fetch the initial chain tip. We get height first, then
	// resolve the hash for that specific height to avoid drift
	// if a new block arrives between the two HTTP calls.
	height, err := b.esplora.GetTipHeight()
	if err != nil {
		return fmt.Errorf("get initial tip: %w", err)
	}

	hash, err := b.esplora.GetBlockHashByHeight(height)
	if err != nil {
		return fmt.Errorf("get initial hash: %w", err)
	}

	b.mu.Lock()
	b.bestHeight = height
	b.bestHash = hash
	b.mu.Unlock()

	// Start the polling loop.
	b.wg.Add(1)
	go b.pollLoop()

	b.log.InfoS(context.Background(), "Chain backend started",
		slog.Int("tip_height", int(height)),
		slog.String("tip_hash", hash.String()))

	return nil
}

// Stop shuts down the polling loop and cleans up resources. Stop is
// idempotent and safe to call multiple times; the stop channel is
// closed exactly once.
func (b *ChainBackend) Stop() error {
	b.log.InfoS(context.Background(), "Stopping chain backend")

	b.stopOnce.Do(func() {
		close(b.stopCh)
	})
	b.wg.Wait()

	b.log.InfoS(context.Background(), "Chain backend stopped")

	return nil
}

// EstimateFee returns the estimated fee rate in satoshis per vbyte for a
// transaction to confirm within the target number of blocks.
func (b *ChainBackend) EstimateFee(_ context.Context,
	targetConf uint32) (btcutil.Amount, error) {

	estimates, err := b.esplora.GetFeeEstimates()
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
func (b *ChainBackend) BestBlock(_ context.Context) (int32,
	chainhash.Hash, error) {

	b.mu.Lock()
	height := b.bestHeight
	hash := b.bestHash
	b.mu.Unlock()

	return height, hash, nil
}

// TestMempoolAccept is not supported by the Esplora backend. This matches
// the LND backend behavior.
func (b *ChainBackend) TestMempoolAccept(_ context.Context,
	_ *wire.MsgTx) (bool, string, error) {

	return false, "", fmt.Errorf("test mempool accept not supported " +
		"by Esplora backend")
}

// BroadcastTx broadcasts a transaction via the Esplora API.
func (b *ChainBackend) BroadcastTx(_ context.Context,
	tx *wire.MsgTx, _ string) error {

	_, err := b.esplora.BroadcastTx(tx)
	if err != nil {
		return fmt.Errorf("broadcast tx: %w", err)
	}

	return nil
}

// RegisterConf registers for confirmation notifications of a transaction.
func (b *ChainBackend) RegisterConf(ctx context.Context,
	txid *chainhash.Hash, pkScript []byte, numConfs uint32,
	heightHint uint32,
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
		slog.Int("height_hint", int(heightHint)))

	cancelFn := func() {
		close(cancelCh)

		b.mu.Lock()
		delete(b.confRegs, id)
		b.mu.Unlock()
	}

	return &chainsource.ConfRegistration{
		Confirmed: confChan,
		Cancel:    cancelFn,
	}, nil
}

// RegisterSpend registers for spend notifications of a transaction output.
func (b *ChainBackend) RegisterSpend(ctx context.Context,
	outpoint *wire.OutPoint, pkScript []byte,
	heightHint uint32) (*chainsource.SpendRegistration, error) {

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
		slog.Int("height_hint", int(heightHint)))

	cancelFn := func() {
		close(cancelCh)

		b.mu.Lock()
		delete(b.spendRegs, id)
		b.mu.Unlock()
	}

	return &chainsource.SpendRegistration{
		Spend:  spendChan,
		Cancel: cancelFn,
	}, nil
}

// RegisterBlocks registers for new block notifications.
func (b *ChainBackend) RegisterBlocks(
	_ context.Context) (*chainsource.BlockRegistration, error) {

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

// pollLoop is the main polling goroutine. It periodically checks for new
// blocks and processes pending registrations.
func (b *ChainBackend) pollLoop() {
	defer b.wg.Done()

	ticker := time.NewTicker(b.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			b.poll()

		case <-b.stopCh:
			return
		}
	}
}

// poll performs a single polling iteration: checks for new blocks and
// processes pending confirmation and spend registrations.
func (b *ChainBackend) poll() {
	// Check for new blocks.
	newHeight, err := b.esplora.GetTipHeight()
	if err != nil {
		b.log.WarnS(context.Background(), "Poll tip height failed", err)
		return
	}

	b.mu.Lock()
	oldHeight := b.bestHeight
	b.mu.Unlock()

	if newHeight <= oldHeight {
		// No new blocks, but still check registrations in case
		// of reorgs or newly broadcast transactions.
		b.checkConfirmations()
		b.checkSpends()
		return
	}

	// Process each new block height.
	for height := oldHeight + 1; height <= newHeight; height++ {
		hash, err := b.esplora.GetBlockHashByHeight(height)
		if err != nil {
			b.log.WarnS(
				context.Background(),
				"Poll block hash failed", err,
			)

			return
		}

		// Fetch block metadata for timestamp.
		blockInfo, err := b.esplora.GetBlockHeader(hash)
		if err != nil {
			b.log.WarnS(
				context.Background(),
				"Poll block header failed", err,
			)

			return
		}

		b.log.DebugS(context.Background(), "New block processed",
			slog.Int("height", int(height)),
			slog.String("hash", hash.String()))

		// Notify block subscribers and update the best known
		// tip atomically under the same lock.
		epoch := &chainsource.BlockEpoch{
			Hash:      hash,
			Height:    height,
			Timestamp: blockInfo.Timestamp,
		}

		b.mu.Lock()
		b.bestHeight = height
		b.bestHash = hash

		for _, reg := range b.blockRegs {
			select {
			case reg.epochChan <- epoch:

			case <-reg.cancelCh:

			default:
				// Channel full, skip this block for this
				// subscriber. The subscriber will catch up
				// on the next poll.
			}
		}
		b.mu.Unlock()
	}

	// Check registrations after processing new blocks.
	b.checkConfirmations()
	b.checkSpends()
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

	status, err := b.esplora.GetTxStatus(*reg.txid)
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

	tx, err := b.esplora.GetRawTx(*reg.txid)
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
		block, err := b.esplora.GetRawBlock(*blockHash)
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

	utxos, err := b.esplora.GetScriptUtxos(reg.pkScript)
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

		tx, err := b.esplora.GetRawTx(*txid)
		if err != nil {
			continue
		}

		conf := &chainsource.TxConfirmation{
			BlockHash:   blockHash,
			BlockHeight: uint32(utxo.Status.BlockHeight),
			Tx:          tx,
		}

		if reg.includeBlock {
			block, err := b.esplora.GetRawBlock(*blockHash)
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

		outspend, err := b.esplora.GetOutspend(
			reg.outpoint.Hash, reg.outpoint.Index,
		)
		if err != nil {
			continue
		}

		if !outspend.Spent {
			continue
		}

		if !outspend.Status.Confirmed {
			continue
		}

		// Parse the spending transaction ID.
		spenderHash, err := chainhash.NewHashFromStr(outspend.Txid)
		if err != nil {
			continue
		}

		// Fetch the full spending transaction.
		spendingTx, err := b.esplora.GetRawTx(*spenderHash)
		if err != nil {
			continue
		}

		detail := &chainsource.SpendDetail{
			SpentOutPoint:     reg.outpoint,
			SpenderTxHash:     spenderHash,
			SpendingTx:        spendingTx,
			SpenderInputIndex: outspend.Vin,
			SpendingHeight:    int32(outspend.Status.BlockHeight),
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
			slog.String("spender_txid",
				spenderHash.String()))

		// Remove the fulfilled registration.
		b.mu.Lock()
		delete(b.spendRegs, id)
		b.mu.Unlock()
	}
}

const (
	// cpfpChildVSizeEstimate is the estimated virtual size of a CPFP
	// child with 2 inputs (P2A anchor + P2TR wallet UTXO) and 1 P2TR
	// output (change). Used for package fee calculation.
	cpfpChildVSizeEstimate = 155

	// cpfpDustLimit is the minimum value for a change output. Below
	// this, the change is donated as additional fee.
	cpfpDustLimit = btcutil.Amount(330)

	// cpfpMaxConfs is the maximum confirmation count when listing
	// wallet UTXOs for fee input selection.
	cpfpMaxConfs = int32(999999)

	// maxCPFPFeeRate is the maximum fee rate (sat/vB) the chain
	// backend will pay for auto-constructed CPFP children.
	// Protects against fee spikes draining wallet funds.
	maxCPFPFeeRate = btcutil.Amount(500)
)

// SubmitPackage submits a parent+child transaction package via the
// Esplora /txs/package HTTP API. This is required for V3 transactions
// with ephemeral anchors that cannot be broadcast individually.
//
// When child is nil and a CPFPWallet is configured, the backend
// automatically constructs a CPFP child that spends the parent's P2A
// anchor output plus a confirmed wallet UTXO for fees. The child is
// signed using the wallet's signing capabilities.
func (b *ChainBackend) SubmitPackage(ctx context.Context,
	parents []*wire.MsgTx, child *wire.MsgTx) error {

	// If no child provided, auto-construct a CPFP child using the
	// wallet. This is the primary code path for lwwallet mode.
	b.mu.Lock()
	w := b.wallet
	b.mu.Unlock()

	if child == nil && w != nil && len(parents) > 0 {
		var err error
		child, err = b.buildCPFPChild(ctx, parents)
		if err != nil {
			return fmt.Errorf("build CPFP child: %w", err)
		}
	}

	// Collect all transactions: parents first, then child.
	allTxs := make([]*wire.MsgTx, 0, len(parents)+1)
	allTxs = append(allTxs, parents...)
	if child != nil {
		allTxs = append(allTxs, child)
	}

	// Serialize each transaction to hex.
	txHexes := make([]string, 0, len(allTxs))
	for _, tx := range allTxs {
		var buf bytes.Buffer
		if err := tx.Serialize(&buf); err != nil {
			return fmt.Errorf("serialize tx: %w", err)
		}
		txHexes = append(txHexes, hex.EncodeToString(buf.Bytes()))
	}

	b.log.InfoS(ctx, "Submitting package via esplora",
		slog.Int("tx_count", len(txHexes)))

	if err := b.esplora.SubmitPackage(txHexes); err != nil {
		return fmt.Errorf("esplora submit package: %w", err)
	}

	b.log.InfoS(ctx, "Package submitted successfully via esplora")

	return nil
}

// buildCPFPChild constructs and signs a CPFP child transaction that
// spends the P2A anchor output from the last parent plus a confirmed
// wallet UTXO for fees. The child pays enough fee to cover the entire
// parent+child package at the current fee rate.
func (b *ChainBackend) buildCPFPChild(ctx context.Context,
	parents []*wire.MsgTx) (*wire.MsgTx, error) {

	// Find the P2A anchor in the last parent (the one the child
	// must spend from).
	lastParent := parents[len(parents)-1]
	anchorIdx := -1
	var anchorOut *wire.TxOut
	for i, out := range lastParent.TxOut {
		if bytes.Equal(out.PkScript, scripts.AnchorPkScript) {
			anchorIdx = i
			anchorOut = out
			break
		}
	}

	if anchorIdx < 0 {
		return nil, fmt.Errorf("no P2A anchor output found in " +
			"parent tx")
	}

	// Estimate total package weight for fee calculation.
	var totalParentVSize int64
	for _, parent := range parents {
		w := estimateWeightCB(parent)
		totalParentVSize += (w + 3) / 4
	}

	// Get fee rate estimate (6-block target). Fall back to
	// 1 sat/vB on regtest where fee estimation is unavailable.
	feeRate, err := b.EstimateFee(ctx, 6)
	if err != nil {
		b.log.WarnS(ctx, "Fee estimation failed, using "+
			"fallback 1 sat/vB", err)
		feeRate = 1
	}

	// Ensure minimum 1 sat/vB.
	if feeRate < 1 {
		feeRate = 1
	}

	// Cap the fee rate to protect against fee spikes.
	if feeRate > maxCPFPFeeRate {
		b.log.WarnS(ctx, "Fee rate exceeds cap", nil,
			slog.Int64("estimated", int64(feeRate)),
			slog.Int64("cap", int64(maxCPFPFeeRate)))
		feeRate = maxCPFPFeeRate
	}

	totalFee := feeRate * btcutil.Amount(
		totalParentVSize+cpfpChildVSizeEstimate,
	)
	if totalFee < 1 {
		totalFee = 1
	}

	// Sanity check: refuse to proceed if the total fee exceeds
	// the value of the parent's non-anchor outputs. This prevents
	// spending more on fees than the VTXO is worth.
	var parentValue btcutil.Amount
	for _, out := range lastParent.TxOut {
		if !bytes.Equal(out.PkScript, scripts.AnchorPkScript) {
			parentValue += btcutil.Amount(out.Value)
		}
	}
	if totalFee > parentValue {
		return nil, fmt.Errorf(
			"total fee (%d sat) exceeds parent VTXO "+
				"value (%d sat), refusing to broadcast",
			int64(totalFee), int64(parentValue))
	}

	b.log.InfoS(ctx, "Building CPFP child",
		slog.Int64("parent_vsize", totalParentVSize),
		slog.Int64("fee_rate", int64(feeRate)),
		slog.Int64("total_fee", int64(totalFee)),
		slog.Int64("anchor_value", anchorOut.Value))

	// Select a confirmed wallet UTXO for fee payment. V3 rules
	// require the child to have at most one unconfirmed input (the
	// anchor), so the fee input must be confirmed.
	feeUTXO, err := b.selectFeeUTXO(totalFee, nil)
	if err != nil {
		return nil, fmt.Errorf("select fee UTXO: %w", err)
	}

	// Get a change address from the wallet.
	b.mu.Lock()
	walletRef := b.wallet
	b.mu.Unlock()

	changeAddr, err := walletRef.NewAddress(ctx)
	if err != nil {
		return nil, fmt.Errorf("get change address: %w", err)
	}

	changePkScript, err := txscript.PayToAddrScript(changeAddr)
	if err != nil {
		return nil, fmt.Errorf("encode change script: %w", err)
	}

	// Build the unsigned V3 CPFP child.
	parentTxid := lastParent.TxHash()
	childTx := wire.NewMsgTx(3)

	// Input 0: P2A anchor (anyone-can-spend, empty witness).
	childTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  parentTxid,
			Index: uint32(anchorIdx),
		},
		Sequence: wire.MaxTxInSequenceNum,
	})

	// Input 1: confirmed wallet UTXO for fee payment.
	childTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: feeUTXO.OutPoint,
		Sequence:         wire.MaxTxInSequenceNum,
	})

	// Output: change back to wallet. The anchor value (240 sats)
	// contributes to the fee budget.
	changeValue := btcutil.Amount(anchorOut.Value) +
		feeUTXO.Value - totalFee
	if changeValue < 0 {
		return nil, fmt.Errorf(
			"insufficient funds: anchor(%d) + utxo(%d) < fee(%d)",
			anchorOut.Value, int64(feeUTXO.Value),
			int64(totalFee))
	}

	// If change is above dust, add a change output.
	if changeValue >= cpfpDustLimit {
		childTx.AddTxOut(&wire.TxOut{
			Value:    int64(changeValue),
			PkScript: changePkScript,
		})
	}

	// Sign the child transaction.
	// Input 0 (P2A anchor): empty witness (anyone-can-spend).
	childTx.TxIn[0].Witness = wire.TxWitness{}

	// Input 1 (wallet UTXO): sign via ComputeInputScript.
	feeOutput := &wire.TxOut{
		Value:    int64(feeUTXO.Value),
		PkScript: feeUTXO.PkScript,
	}

	// Build a PrevOutputFetcher for taproot sighash computation.
	prevOuts := txscript.NewMultiPrevOutFetcher(
		map[wire.OutPoint]*wire.TxOut{
			childTx.TxIn[0].PreviousOutPoint: anchorOut,
			childTx.TxIn[1].PreviousOutPoint: feeOutput,
		},
	)

	sigHashes := txscript.NewTxSigHashes(childTx, prevOuts)

	signDesc := &input.SignDescriptor{
		Output:            feeOutput,
		HashType:          txscript.SigHashDefault,
		SigHashes:         sigHashes,
		InputIndex:        1,
		PrevOutputFetcher: prevOuts,
	}

	inputScript, err := walletRef.ComputeInputScript(
		childTx, signDesc,
	)
	if err != nil {
		return nil, fmt.Errorf("sign wallet input: %w", err)
	}

	childTx.TxIn[1].Witness = inputScript.Witness
	if inputScript.SigScript != nil {
		childTx.TxIn[1].SignatureScript = inputScript.SigScript
	}

	childTxid := childTx.TxHash()
	b.log.InfoS(ctx, "CPFP child built and signed",
		slog.String("child_txid", childTxid.String()),
		slog.String("parent_txid", parentTxid.String()),
		slog.Int("child_inputs", len(childTx.TxIn)),
		slog.Int("child_outputs", len(childTx.TxOut)),
		slog.Int64("change_value", int64(changeValue)),
		slog.Int64("fee_utxo_value", int64(feeUTXO.Value)))

	return childTx, nil
}

// selectFeeUTXO finds a confirmed wallet UTXO with sufficient value
// to cover the required fee. Selects the smallest sufficient UTXO to
// minimize change size. The optional exclude map tracks outpoints
// already selected, preventing double-spend within a batch.
func (b *ChainBackend) selectFeeUTXO(
	minValue btcutil.Amount,
	exclude map[wire.OutPoint]bool) (*lnwallet.Utxo, error) {

	b.mu.Lock()
	walletForUTXO := b.wallet
	b.mu.Unlock()

	utxos, err := walletForUTXO.ListUnspentWitness(1, cpfpMaxConfs)
	if err != nil {
		return nil, fmt.Errorf("list unspent: %w", err)
	}

	if len(utxos) == 0 {
		return nil, fmt.Errorf("no confirmed wallet UTXOs available")
	}

	// Sort by value ascending to pick smallest sufficient UTXO.
	sort.Slice(utxos, func(i, j int) bool {
		return utxos[i].Value < utxos[j].Value
	})

	for _, utxo := range utxos {
		if exclude != nil && exclude[utxo.OutPoint] {
			continue
		}

		if utxo.Value >= minValue {
			return utxo, nil
		}
	}

	return nil, fmt.Errorf(
		"no UTXO with sufficient value (need %d sat, "+
			"best has %d sat)",
		int64(minValue), int64(utxos[len(utxos)-1].Value))
}

// estimateWeightCB computes the transaction weight including witness
// data. Weight = base_size * 3 + total_size (BIP 141).
func estimateWeightCB(tx *wire.MsgTx) int64 {
	baseSize := int64(tx.SerializeSizeStripped())
	totalSize := int64(tx.SerializeSize())
	return baseSize*3 + totalSize
}

// Compile-time check that ChainBackend implements
// chainsource.ChainBackend.
var _ chainsource.ChainBackend = (*ChainBackend)(nil)
