package lwwallet

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wtxmgr"
)

// EsploraChainService implements btcwallet's chain.Interface using the
// Esplora REST API. It provides the blockchain access layer that
// btcwallet needs for wallet synchronization, block notifications, and
// transaction broadcasting. The service polls Esplora for new blocks
// at a configurable interval and forwards BlockConnected notifications
// to btcwallet via the Notifications() channel.
type EsploraChainService struct {
	esplora      *EsploraClient
	pollInterval time.Duration

	// log is the structured logger for this chain service instance.
	log btclog.Logger

	// notifications is the channel for sending chain events to
	// btcwallet's wallet syncer. btcwallet reads from this channel
	// in its handleChainNotifications goroutine.
	notifications chan interface{}

	// mu protects watchedAddrs and bestBlock.
	mu sync.Mutex

	// watchedAddrs holds addresses registered via NotifyReceived.
	// These are used during block processing to detect relevant
	// transactions for RelevantTx notifications.
	watchedAddrs map[string]btcutil.Address

	// bestBlock caches the current chain tip, updated by the poll
	// loop on each new block.
	bestBlock waddrmgr.BlockStamp

	quit chan struct{}
	wg   sync.WaitGroup
}

// NewEsploraChainService creates a new chain.Interface backed by the
// Esplora REST API. The pollInterval controls how frequently the
// service checks for new blocks.
func NewEsploraChainService(esplora *EsploraClient,
	pollInterval time.Duration,
	logger btclog.Logger) *EsploraChainService {

	return &EsploraChainService{
		esplora:       esplora,
		pollInterval:  pollInterval,
		log:           logger,
		notifications: make(chan interface{}, 100),
		watchedAddrs:  make(map[string]btcutil.Address),
		quit:          make(chan struct{}),
	}
}

// Start fetches the initial chain tip and starts the polling goroutine
// that sends BlockConnected notifications to btcwallet.
func (s *EsploraChainService) Start() error {
	// Fetch the initial chain tip so BlockStamp() returns correct
	// values immediately. We get height first, then resolve the
	// hash for that specific height to avoid drift if a new block
	// arrives between the two HTTP calls.
	tipHeight, err := s.esplora.GetTipHeight()
	if err != nil {
		return fmt.Errorf("get initial tip height: %w", err)
	}

	tipHash, err := s.esplora.GetBlockHashByHeight(tipHeight)
	if err != nil {
		return fmt.Errorf("get initial tip hash: %w", err)
	}

	tipHeader, err := s.esplora.GetBlockHeader(tipHash)
	if err != nil {
		return fmt.Errorf("get initial tip header: %w", err)
	}

	s.mu.Lock()
	s.bestBlock = waddrmgr.BlockStamp{
		Height:    tipHeight,
		Hash:      tipHash,
		Timestamp: time.Unix(tipHeader.Timestamp, 0),
	}
	s.mu.Unlock()

	// Send ClientConnected so btcwallet knows the chain backend
	// is ready.
	s.notifications <- chain.ClientConnected{}

	s.wg.Add(1)
	go s.pollLoop()

	s.log.InfoS(context.Background(), "Esplora chain service started",
		slog.Int("tip_height", int(tipHeight)),
		slog.String("tip_hash", tipHash.String()))

	return nil
}

// Stop signals the polling goroutine to exit.
func (s *EsploraChainService) Stop() {
	select {
	case <-s.quit:
		// Already stopped.
		return

	default:
		s.log.InfoS(context.Background(),
			"Stopping Esplora chain service")

		close(s.quit)
	}
}

// WaitForShutdown blocks until the polling goroutine has exited.
func (s *EsploraChainService) WaitForShutdown() {
	s.wg.Wait()
}

// GetBestBlock returns the hash and height of the current best block.
// Height is fetched first, then the hash is resolved for that specific
// height to avoid returning mismatched values if a new block arrives
// between the two HTTP calls.
func (s *EsploraChainService) GetBestBlock() (
	*chainhash.Hash, int32, error) {

	height, err := s.esplora.GetTipHeight()
	if err != nil {
		return nil, 0, fmt.Errorf(
			"get best block height: %w", err,
		)
	}

	hash, err := s.esplora.GetBlockHashByHeight(height)
	if err != nil {
		return nil, 0, fmt.Errorf("get best block hash: %w", err)
	}

	return &hash, height, nil
}

// GetBlock returns the full deserialized block for the given hash.
func (s *EsploraChainService) GetBlock(
	hash *chainhash.Hash) (*wire.MsgBlock, error) {

	return s.esplora.GetRawBlock(*hash)
}

// GetBlockHash returns the block hash at the given height. The height
// is int64 to match the chain.Interface signature.
func (s *EsploraChainService) GetBlockHash(
	height int64) (*chainhash.Hash, error) {

	hash, err := s.esplora.GetBlockHashByHeight(int32(height))
	if err != nil {
		return nil, err
	}

	return &hash, nil
}

// GetBlockHeader returns the deserialized block header for the given
// hash. This calls the Esplora /block/:hash/header endpoint.
func (s *EsploraChainService) GetBlockHeader(
	hash *chainhash.Hash) (*wire.BlockHeader, error) {

	return s.esplora.GetRawBlockHeader(*hash)
}

// IsCurrent returns true because the Esplora backend is assumed to
// always be synced with the network.
func (s *EsploraChainService) IsCurrent() bool {
	return true
}

// FilterBlocks scans a batch of blocks for transactions relevant to
// the provided addresses and watched outpoints. For each block in the
// request, the full block is fetched and every transaction is checked
// for outputs matching external/internal addresses and inputs spending
// watched outpoints. Returns the first block containing any match.
func (s *EsploraChainService) FilterBlocks(
	req *chain.FilterBlocksRequest) (
	*chain.FilterBlocksResponse, error) {

	// Build a pkScript lookup table from the address sets so we
	// can efficiently match transaction outputs.
	addrScripts := make(map[string]addressMatch)

	for scopedIdx, addr := range req.ExternalAddrs {
		pkScript, err := txscript.PayToAddrScript(addr)
		if err != nil {
			continue
		}

		addrScripts[string(pkScript)] = addressMatch{
			scope:    scopedIdx.Scope,
			index:    scopedIdx.Index,
			external: true,
		}
	}

	for scopedIdx, addr := range req.InternalAddrs {
		pkScript, err := txscript.PayToAddrScript(addr)
		if err != nil {
			continue
		}

		addrScripts[string(pkScript)] = addressMatch{
			scope:    scopedIdx.Scope,
			index:    scopedIdx.Index,
			external: false,
		}
	}

	for batchIdx, blockMeta := range req.Blocks {
		block, err := s.esplora.GetRawBlock(blockMeta.Hash)
		if err != nil {
			return nil, fmt.Errorf(
				"get block %s: %w",
				blockMeta.Hash, err,
			)
		}

		resp := s.filterBlock(
			block, blockMeta, uint32(batchIdx),
			addrScripts, req.WatchedOutPoints,
		)

		if resp != nil {
			return resp, nil
		}
	}

	return nil, nil
}

// addressMatch pairs an address's key scope and index with whether
// it is external or internal. This is used to populate the
// FoundExternalAddrs / FoundInternalAddrs maps in the filter response.
type addressMatch struct {
	scope    waddrmgr.KeyScope
	index    uint32
	external bool
}

// filterBlock checks a single block for transactions matching the
// given address scripts or watched outpoints. Returns nil if no
// matches are found.
func (s *EsploraChainService) filterBlock(
	block *wire.MsgBlock, meta wtxmgr.BlockMeta,
	batchIdx uint32, addrScripts map[string]addressMatch,
	watchedOPs map[wire.OutPoint]btcutil.Address,
) *chain.FilterBlocksResponse {

	var (
		foundExternal = make(
			map[waddrmgr.KeyScope]map[uint32]struct{},
		)
		foundInternal = make(
			map[waddrmgr.KeyScope]map[uint32]struct{},
		)
		foundOutpoints = make(
			map[wire.OutPoint]btcutil.Address,
		)
		relevantTxns []*wire.MsgTx
	)

	matched := false

	for _, tx := range block.Transactions {
		txMatched := false

		// Check outputs against watched addresses.
		for _, txOut := range tx.TxOut {
			match, ok := addrScripts[string(txOut.PkScript)]
			if !ok {
				continue
			}

			txMatched = true

			if match.external {
				if foundExternal[match.scope] == nil {
					foundExternal[match.scope] =
						make(map[uint32]struct{})
				}

				foundExternal[match.scope][match.index] =
					struct{}{}
			} else {
				if foundInternal[match.scope] == nil {
					foundInternal[match.scope] =
						make(map[uint32]struct{})
				}

				foundInternal[match.scope][match.index] =
					struct{}{}
			}
		}

		// Check inputs against watched outpoints.
		for _, txIn := range tx.TxIn {
			addr, ok := watchedOPs[txIn.PreviousOutPoint]
			if !ok {
				continue
			}

			txMatched = true
			foundOutpoints[txIn.PreviousOutPoint] = addr
		}

		if txMatched {
			matched = true
			relevantTxns = append(relevantTxns, tx)
		}
	}

	if !matched {
		return nil
	}

	return &chain.FilterBlocksResponse{
		BatchIndex:         batchIdx,
		BlockMeta:          meta,
		FoundExternalAddrs: foundExternal,
		FoundInternalAddrs: foundInternal,
		FoundOutPoints:     foundOutpoints,
		RelevantTxns:       relevantTxns,
	}
}

// BlockStamp returns the cached best block stamp. This is updated by
// the polling loop whenever a new block is detected.
func (s *EsploraChainService) BlockStamp() (
	*waddrmgr.BlockStamp, error) {

	s.mu.Lock()
	stamp := s.bestBlock
	s.mu.Unlock()

	return &stamp, nil
}

// SendRawTransaction broadcasts a signed transaction to the network
// via the Esplora API. The allowHighFees parameter is ignored since
// Esplora does not support fee-rate validation before broadcast.
func (s *EsploraChainService) SendRawTransaction(tx *wire.MsgTx,
	allowHighFees bool) (*chainhash.Hash, error) {

	_, err := s.esplora.BroadcastTx(tx)
	if err != nil {
		return nil, fmt.Errorf("broadcast tx: %w", err)
	}

	txHash := tx.TxHash()

	return &txHash, nil
}

// Rescan walks the chain from the block identified by startHash
// forward to the current tip, checking each block for transactions
// that match the given addresses or outpoints.
//
// Block data is fetched synchronously, but notifications are flushed
// to the notification channel in a background goroutine. This avoids
// a deadlock with btcwallet's handleChainNotifications: during initial
// sync, handleChainNotifications blocks on the rescan result (via
// rescanWithTarget → SubmitRescan), while Rescan tries to send
// FilteredBlockConnected notifications to the same channel that
// handleChainNotifications reads from. If the channel buffer fills,
// both sides block permanently. Sending notifications asynchronously
// lets Rescan return immediately, unblocking handleChainNotifications
// to drain the queued notifications.
func (s *EsploraChainService) Rescan(startHash *chainhash.Hash,
	addrs []btcutil.Address,
	outpoints map[wire.OutPoint]btcutil.Address) error {

	// Build the pkScript lookup from addresses.
	addrScripts := make(map[string]struct{})
	for _, addr := range addrs {
		pkScript, err := txscript.PayToAddrScript(addr)
		if err != nil {
			continue
		}

		addrScripts[string(pkScript)] = struct{}{}
	}

	// Look up the starting block height.
	startBlock, err := s.esplora.GetBlockHeader(*startHash)
	if err != nil {
		return fmt.Errorf("get start block header: %w", err)
	}

	startHeight := startBlock.Height

	// Get the current chain tip.
	tipHeight, err := s.esplora.GetTipHeight()
	if err != nil {
		return fmt.Errorf("get tip height for rescan: %w", err)
	}

	s.log.InfoS(context.Background(), "Starting chain rescan",
		slog.Int("start_height", int(startHeight)),
		slog.Int("tip_height", int(tipHeight)),
		slog.Int("watched_addrs", len(addrs)),
		slog.Int("watched_outpoints", len(outpoints)))

	// Collect all notifications first, then flush them
	// asynchronously. See method doc for deadlock rationale.
	var pending []interface{}

	// Walk each block from start to tip.
	for height := startHeight; height <= tipHeight; height++ {
		blockHash, err := s.esplora.GetBlockHashByHeight(height)
		if err != nil {
			return fmt.Errorf(
				"get block hash at %d: %w", height, err,
			)
		}

		block, err := s.esplora.GetRawBlock(blockHash)
		if err != nil {
			return fmt.Errorf(
				"get block at %d: %w", height, err,
			)
		}

		blockHeader, err := s.esplora.GetBlockHeader(blockHash)
		if err != nil {
			return fmt.Errorf(
				"get block header at %d: %w", height, err,
			)
		}

		blockMeta := wtxmgr.BlockMeta{
			Block: wtxmgr.Block{
				Hash:   blockHash,
				Height: height,
			},
			Time: time.Unix(blockHeader.Timestamp, 0),
		}

		// Filter transactions in this block.
		var relevantTxs []*wtxmgr.TxRecord
		for _, tx := range block.Transactions {
			if !s.txMatchesRescan(
				tx, addrScripts, outpoints,
			) {

				continue
			}

			rec, err := wtxmgr.NewTxRecordFromMsgTx(
				tx, blockMeta.Time,
			)
			if err != nil {
				continue
			}

			relevantTxs = append(relevantTxs, rec)
		}

		// Queue FilteredBlockConnected so btcwallet processes
		// any relevant transactions via addRelevantTx.
		pending = append(pending,
			chain.FilteredBlockConnected{
				Block:       &blockMeta,
				RelevantTxs: relevantTxs,
			},
		)

		// Queue BlockConnected so btcwallet advances its
		// sync height via connectBlock → SetSyncedTo.
		// This must follow FilteredBlockConnected so that
		// relevant transactions are recorded before the
		// sync height advances past them.
		pending = append(pending,
			chain.BlockConnected(blockMeta),
		)
	}

	s.log.InfoS(context.Background(), "Chain rescan complete",
		slog.Int("tip_height", int(tipHeight)),
		slog.Int("pending_notifications", len(pending)))

	// Flush collected notifications in a background goroutine
	// so this function returns without blocking on channel
	// capacity.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		for _, n := range pending {
			select {
			case s.notifications <- n:

			case <-s.quit:
				return
			}
		}
	}()

	return nil
}

// txMatchesRescan checks whether a transaction matches any of the
// rescan criteria (address scripts or watched outpoints).
func (s *EsploraChainService) txMatchesRescan(tx *wire.MsgTx,
	addrScripts map[string]struct{},
	outpoints map[wire.OutPoint]btcutil.Address) bool {

	// Check outputs against watched address scripts.
	for _, txOut := range tx.TxOut {
		if _, ok := addrScripts[string(txOut.PkScript)]; ok {
			return true
		}
	}

	// Check inputs against watched outpoints.
	for _, txIn := range tx.TxIn {
		if _, ok := outpoints[txIn.PreviousOutPoint]; ok {
			return true
		}
	}

	return false
}

// NotifyReceived registers addresses for transaction notifications.
// When a new block arrives and contains a transaction paying to one of
// these addresses, a RelevantTx notification is sent. btcwallet calls
// this for each new address it generates (NewAddress,
// ImportTaprootScript, etc.) so the chain backend can include
// transactions to those addresses in FilteredBlockConnected
// notifications.
func (s *EsploraChainService) NotifyReceived(
	addrs []btcutil.Address) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, addr := range addrs {
		s.watchedAddrs[addr.String()] = addr
	}

	s.log.DebugS(context.Background(),
		"Registered addresses for notifications",
		slog.Int("new_addrs", len(addrs)),
		slog.Int("total_watched", len(s.watchedAddrs)))

	return nil
}

// NotifyBlocks is a no-op because the polling loop always sends
// BlockConnected notifications for new blocks.
func (s *EsploraChainService) NotifyBlocks() error {
	return nil
}

// Notifications returns the read-only channel that btcwallet uses to
// receive chain events (BlockConnected, FilteredBlockConnected,
// RelevantTx, RescanProgress, RescanFinished, ClientConnected).
func (s *EsploraChainService) Notifications() <-chan interface{} {
	return s.notifications
}

// BackEnd returns the name of this chain backend implementation.
func (s *EsploraChainService) BackEnd() string {
	return "esplora"
}

// TestMempoolAccept validates transactions against mempool policy
// without broadcasting them. This uses the Esplora POST /txs/test
// endpoint which proxies to Bitcoin Core's testmempoolaccept RPC.
func (s *EsploraChainService) TestMempoolAccept(
	txns []*wire.MsgTx,
	maxFeeRate float64) ([]*btcjson.TestMempoolAcceptResult, error) {

	esploraResults, err := s.esplora.TestMempoolAccept(
		txns, maxFeeRate,
	)
	if err != nil {
		return nil, err
	}

	// Convert from Esplora response format to btcjson format.
	results := make(
		[]*btcjson.TestMempoolAcceptResult, len(esploraResults),
	)
	for i, r := range esploraResults {
		result := &btcjson.TestMempoolAcceptResult{
			Txid:         r.Txid,
			Wtxid:        r.Wtxid,
			Allowed:      r.Allowed,
			Vsize:        r.Vsize,
			RejectReason: r.RejectReason,
		}

		if r.Fees != nil {
			result.Fees = &btcjson.TestMempoolAcceptFees{
				Base: r.Fees.Base,
			}
		}

		results[i] = result
	}

	return results, nil
}

// MapRPCErr passes through errors unchanged since the Esplora backend
// does not use RPC error codes.
func (s *EsploraChainService) MapRPCErr(err error) error {
	return err
}

// pollLoop periodically checks for new blocks and sends
// BlockConnected notifications to btcwallet. The loop runs until
// Stop() is called.
func (s *EsploraChainService) pollLoop() {
	defer s.wg.Done()

	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.pollForBlocks()

		case <-s.quit:
			return
		}
	}
}

// pollForBlocks checks for new blocks since the last known tip and
// sends FilteredBlockConnected and BlockConnected notifications for
// each new block. FilteredBlockConnected carries relevant transactions
// (matching watched addresses) so btcwallet can track UTXOs, while
// BlockConnected updates the wallet's sync height.
func (s *EsploraChainService) pollForBlocks() {
	newHeight, err := s.esplora.GetTipHeight()
	if err != nil {
		s.log.WarnS(context.Background(),
			"Chain service poll tip height failed", err)

		return
	}

	s.mu.Lock()
	oldHeight := s.bestBlock.Height
	s.mu.Unlock()

	if newHeight <= oldHeight {
		return
	}

	s.log.DebugS(context.Background(),
		"New blocks detected",
		slog.Int("old_height", int(oldHeight)),
		slog.Int("new_height", int(newHeight)))

	// Process each new block in order.
	for height := oldHeight + 1; height <= newHeight; height++ {
		blockHash, err := s.esplora.GetBlockHashByHeight(height)
		if err != nil {
			s.log.WarnS(context.Background(),
				"Chain service poll block hash failed",
				err)

			return
		}

		blockInfo, err := s.esplora.GetBlockHeader(blockHash)
		if err != nil {
			s.log.WarnS(context.Background(),
				"Chain service poll block info failed",
				err)

			return
		}

		blockMeta := wtxmgr.BlockMeta{
			Block: wtxmgr.Block{
				Hash:   blockHash,
				Height: height,
			},
			Time: time.Unix(blockInfo.Timestamp, 0),
		}

		// Build pkScript lookup from currently watched addresses
		// so we can detect relevant transactions in this block.
		s.mu.Lock()
		watchedScripts := make(map[string]struct{},
			len(s.watchedAddrs))
		for _, addr := range s.watchedAddrs {
			pkScript, err := txscript.PayToAddrScript(addr)
			if err != nil {
				continue
			}

			watchedScripts[string(pkScript)] = struct{}{}
		}
		s.mu.Unlock()

		// Filter block for relevant transactions if we have
		// any watched addresses. This requires fetching the
		// full block from Esplora.
		var relevantTxs []*wtxmgr.TxRecord
		if len(watchedScripts) > 0 {
			block, err := s.esplora.GetRawBlock(blockHash)
			if err != nil {
				s.log.WarnS(context.Background(),
					"Chain service poll block "+
						"fetch failed", err)

				return
			}

			relevantTxs = s.filterBlockTxs(
				block, watchedScripts,
				blockMeta.Time,
			)
		}

		// Send FilteredBlockConnected with relevant
		// transactions so btcwallet processes them via
		// addRelevantTx. This is how btcwallet learns about
		// transactions paying to wallet-owned addresses.
		//
		// We use select with quit to prevent blocking
		// indefinitely if the channel is full during initial
		// sync (when handleChainNotifications is busy with
		// syncWithChain/recovery).
		select {
		case s.notifications <- chain.FilteredBlockConnected{
			Block:       &blockMeta,
			RelevantTxs: relevantTxs,
		}:

		case <-s.quit:
			return
		}

		// Send BlockConnected to update btcwallet's sync
		// height. FilteredBlockConnected only processes
		// transactions but does not update the sync height.
		select {
		case s.notifications <- chain.BlockConnected(
			blockMeta,
		):

		case <-s.quit:
			return
		}

		// Update the cached best block.
		s.mu.Lock()
		s.bestBlock = waddrmgr.BlockStamp{
			Height:    height,
			Hash:      blockHash,
			Timestamp: blockMeta.Time,
		}
		s.mu.Unlock()
	}
}

// filterBlockTxs checks all transactions in the block against the
// watched pkScripts and returns TxRecords for matching transactions.
func (s *EsploraChainService) filterBlockTxs(block *wire.MsgBlock,
	watchedScripts map[string]struct{},
	blockTime time.Time) []*wtxmgr.TxRecord {

	var records []*wtxmgr.TxRecord

	for _, tx := range block.Transactions {
		if !s.txMatchesScripts(tx, watchedScripts) {
			continue
		}

		rec, err := wtxmgr.NewTxRecordFromMsgTx(tx, blockTime)
		if err != nil {
			continue
		}

		txHash := tx.TxHash()
		s.log.DebugS(context.Background(),
			"Found relevant transaction in block",
			slog.String("txid", txHash.String()),
			slog.Int("num_outputs", len(tx.TxOut)))

		records = append(records, rec)
	}

	return records
}

// txMatchesScripts checks whether any transaction output pays to one
// of the watched pkScripts.
func (s *EsploraChainService) txMatchesScripts(tx *wire.MsgTx,
	watchedScripts map[string]struct{}) bool {

	for _, txOut := range tx.TxOut {
		if _, ok := watchedScripts[string(txOut.PkScript)]; ok {
			return true
		}
	}

	return false
}

// Compile-time check that EsploraChainService implements
// chain.Interface.
var _ chain.Interface = (*EsploraChainService)(nil)
