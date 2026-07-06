package lwwallet

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wtxmgr"
)

// EsploraChainService implements btcwallet's chain.Interface using the
// Esplora REST API. It is a passive consumer of a shared TipPoller: it
// does not poll Esplora for new blocks itself, but instead subscribes
// to the poller's tip stream and translates each TipBlock event into
// the FilteredBlockConnected + BlockConnected notification pair that
// btcwallet's wallet syncer expects.
type EsploraChainService struct {
	esplora *EsploraClient

	// tipPoller is the shared tip-event source. The chain service
	// never owns this poller; lifecycle is the caller's
	// responsibility (typically the lwwallet integrated wallet).
	tipPoller *TipPoller

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
	watchedAddrs map[string]btcaddr.Address

	// bestBlock caches the current chain tip, updated on each
	// processed TipBlock event.
	bestBlock waddrmgr.BlockStamp

	// runCtx is the service lifecycle context captured at Start. It
	// bounds Esplora HTTP calls made through btcwallet's chain.Interface,
	// whose methods do not accept a context parameter.
	runCtx context.Context //nolint:containedctx

	// maxGapFillPerTipEvent caps the number of missed heights that a
	// single processTipEvent invocation will walk before yielding
	// back to the handleTipEvents loop. Initialized from
	// defaultMaxGapFillPerTipEvent and overridable via the
	// WithMaxGapFillPerTipEvent functional option (tests use this to
	// exercise the cap branch without revealing 256+ heights).
	maxGapFillPerTipEvent int32

	quit     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// EsploraChainServiceOption configures an EsploraChainService at
// construction time. Options are applied in order, so a later option
// overrides an earlier one for the same field.
type EsploraChainServiceOption func(*EsploraChainService)

// WithMaxGapFillPerTipEvent overrides the per-TipBlock gap-fill cap.
// Intended for tests that need to exercise the bounded-walk branch
// of fillGap; production callers should leave the default in place.
func WithMaxGapFillPerTipEvent(n int32) EsploraChainServiceOption {
	return func(s *EsploraChainService) {
		s.maxGapFillPerTipEvent = n
	}
}

// NewEsploraChainService creates a new chain.Interface backed by the
// Esplora REST API. The provided TipPoller drives new-block
// detection; the caller is responsible for starting and stopping it.
func NewEsploraChainService(esplora *EsploraClient, tipPoller *TipPoller,
	logger btclog.Logger,
	opts ...EsploraChainServiceOption) *EsploraChainService {

	s := &EsploraChainService{
		esplora:               esplora,
		tipPoller:             tipPoller,
		log:                   logger,
		notifications:         make(chan interface{}, 100),
		watchedAddrs:          make(map[string]btcaddr.Address),
		maxGapFillPerTipEvent: defaultMaxGapFillPerTipEvent,
		quit:                  make(chan struct{}),
	}

	for _, opt := range opts {
		opt(s)
	}

	return s
}

// Start seeds the initial chain tip from the configured TipPoller
// (which the caller must have started already) and spawns the
// goroutine that translates each TipBlock event into btcwallet
// chain notifications.
func (s *EsploraChainService) Start(ctx context.Context) error {
	//nolint:contextcheck // tip subscription lifecycle is owned by Stop
	tipHeight, tipHash, tipTime, sub, err :=
		s.tipPoller.BestBlockAndSubscribe()
	if err != nil {
		return fmt.Errorf("subscribe to tip poller: %w", err)
	}

	s.mu.Lock()
	s.bestBlock = waddrmgr.BlockStamp{
		Height:    tipHeight,
		Hash:      tipHash,
		Timestamp: tipTime,
	}
	s.runCtx = ctx
	s.mu.Unlock()

	// Send ClientConnected so btcwallet knows the chain backend
	// is ready.
	s.notifications <- chain.ClientConnected{}

	s.wg.Add(1)
	go s.handleTipEvents(ctx, sub)

	s.log.InfoS(ctx, "Esplora chain service started",
		slog.Int("tip_height", int(tipHeight)),
		slog.String("tip_hash", tipHash.String()),
	)

	return nil
}

// Stop signals the polling goroutine to exit. Stop is idempotent
// and safe to call concurrently from multiple goroutines; a
// sync.Once guards the close so two simultaneous Stop calls cannot
// both reach close(s.quit) and panic on a double close.
func (s *EsploraChainService) Stop() {
	s.stopOnce.Do(func() {
		s.log.InfoS(
			s.requestContext(),
			"Stopping Esplora chain service",
		)

		close(s.quit)
	})
}

// WaitForShutdown blocks until the polling goroutine has exited.
func (s *EsploraChainService) WaitForShutdown() {
	s.wg.Wait()
}

// requestContext returns the service lifecycle context captured by Start.
// Chain interface methods do not receive caller contexts, so they use this
// root to let daemon shutdown cancel in-flight Esplora HTTP requests.
func (s *EsploraChainService) requestContext() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.runCtx != nil {
		return s.runCtx
	}

	return context.Background()
}

// GetBestBlock returns the hash and height of the current best block.
// The shared TipPoller already maintains a consistent (height, hash,
// timestamp) triple under its own mutex, resolved against the height
// it actually observed; reading that snapshot here avoids two live
// HTTP round trips per call (and the original TOCTOU between the two
// independent fetches that this method used to defend against).
func (s *EsploraChainService) GetBestBlock() (*chainhash.Hash, int32, error) {
	height, hash, _ := s.tipPoller.BestBlock()

	return &hash, height, nil
}

// GetBlock returns the full deserialized block for the given hash.
func (s *EsploraChainService) GetBlock(hash *chainhash.Hash) (*wire.MsgBlock,
	error) {

	return s.esplora.GetRawBlock(s.requestContext(), *hash)
}

// GetBlockHash returns the block hash at the given height. The height
// is int64 to match the chain.Interface signature.
func (s *EsploraChainService) GetBlockHash(height int64) (*chainhash.Hash,
	error) {

	hash, err := s.esplora.GetBlockHashByHeight(
		s.requestContext(), int32(height),
	)
	if err != nil {
		return nil, err
	}

	return &hash, nil
}

// GetBlockHeader returns the deserialized block header for the given
// hash. This calls the Esplora /block/:hash/header endpoint.
func (s *EsploraChainService) GetBlockHeader(hash *chainhash.Hash) (
	*wire.BlockHeader, error) {

	return s.esplora.GetRawBlockHeader(s.requestContext(), *hash)
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
func (s *EsploraChainService) FilterBlocks(req *chain.FilterBlocksRequest) (
	*chain.FilterBlocksResponse, error) {

	ctx := s.requestContext()

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
		block, err := s.esplora.GetRawBlock(
			ctx, blockMeta.Hash,
		)
		if err != nil {
			return nil, fmt.Errorf("get block %s: %w",
				blockMeta.Hash, err)
		}

		resp := s.filterBlock(
			block, blockMeta, uint32(batchIdx), addrScripts,
			req.WatchedOutPoints,
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
	watchedOPs map[wire.OutPoint]btcaddr.Address,
) *chain.FilterBlocksResponse {

	var (
		foundExternal = make(
			map[waddrmgr.KeyScope]map[uint32]struct{},
		)
		foundInternal = make(
			map[waddrmgr.KeyScope]map[uint32]struct{},
		)
		foundOutpoints = make(
			map[wire.OutPoint]btcaddr.Address,
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
func (s *EsploraChainService) BlockStamp() (*waddrmgr.BlockStamp, error) {
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

	ctx := s.requestContext()
	_, err := s.esplora.BroadcastTx(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("broadcast tx: %w", err)
	}

	txHash := tx.TxHash()

	return &txHash, nil
}

// SubmitPackage is not supported by the Esplora backend. Esplora is a REST
// block-explorer API with no package-relay (submitpackage) endpoint, so
// atomic parent+child submission is unavailable; deployments that need
// v3/TRUC CPFP package relay must use a bitcoind- or lnd-backed chain source.
func (s *EsploraChainService) SubmitPackage(_ []*wire.MsgTx, _ *float64) (
	*btcjson.SubmitPackageResult, error) {

	return nil, fmt.Errorf("SubmitPackage not supported by Esplora chain " +
		"backend")
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
	addrs []btcaddr.Address,
	outpoints map[wire.OutPoint]btcaddr.Address) error {

	ctx := s.requestContext()

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
	startBlock, err := s.esplora.GetBlockHeader(
		ctx, *startHash,
	)
	if err != nil {
		return fmt.Errorf("get start block header: %w", err)
	}

	startHeight := startBlock.Height

	// Get the current chain tip.
	tipHeight, err := s.esplora.GetTipHeight(ctx)
	if err != nil {
		return fmt.Errorf("get tip height for rescan: %w", err)
	}

	s.log.InfoS(ctx, "Starting chain rescan",
		slog.Int("start_height", int(startHeight)),
		slog.Int("tip_height", int(tipHeight)),
		slog.Int("watched_addrs", len(addrs)),
		slog.Int("watched_outpoints", len(outpoints)),
	)

	// Collect all notifications first, then flush them
	// asynchronously. See method doc for deadlock rationale.
	var pending []interface{}

	// Walk each block from start to tip.
	for height := startHeight; height <= tipHeight; height++ {
		blockHash, err := s.esplora.GetBlockHashByHeight(
			ctx, height,
		)
		if err != nil {
			return fmt.Errorf("get block hash at %d: %w", height,
				err)
		}

		block, err := s.esplora.GetRawBlock(
			ctx, blockHash,
		)
		if err != nil {
			return fmt.Errorf("get block at %d: %w", height, err)
		}

		blockHeader, err := s.esplora.GetBlockHeader(
			ctx, blockHash,
		)
		if err != nil {
			return fmt.Errorf("get block header at %d: %w", height,
				err)
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

	s.log.InfoS(ctx, "Chain rescan complete",
		slog.Int("tip_height", int(tipHeight)),
		slog.Int("pending_notifications", len(pending)),
	)

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
	outpoints map[wire.OutPoint]btcaddr.Address) bool {

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
func (s *EsploraChainService) NotifyReceived(addrs []btcaddr.Address) error {
	s.mu.Lock()
	for _, addr := range addrs {
		s.watchedAddrs[addr.String()] = addr
	}
	totalWatched := len(s.watchedAddrs)
	s.mu.Unlock()

	s.log.DebugS(
		s.requestContext(),
		"Registered addresses for notifications",
		slog.Int("new_addrs", len(addrs)),
		slog.Int("total_watched", totalWatched),
	)

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
func (s *EsploraChainService) TestMempoolAccept(txns []*wire.MsgTx,
	maxFeeRate float64) ([]*btcjson.TestMempoolAcceptResult, error) {

	esploraResults, err := s.esplora.TestMempoolAccept(
		s.requestContext(), txns, maxFeeRate,
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

// handleTipEvents drains TipBlock events from the shared poller and
// translates each event into the FilteredBlockConnected +
// BlockConnected notification pair that btcwallet's wallet syncer
// expects. The loop exits when the chain service is stopped, when
// the poller signals shutdown via Quit, or when the subscription's
// Updates channel is closed by Cancel.
func (s *EsploraChainService) handleTipEvents(ctx context.Context,
	sub *TipSubscription) {

	defer s.wg.Done()
	defer sub.Cancel()

	for {
		select {
		case event, ok := <-sub.Updates():
			if !ok {
				return
			}

			s.processTipEvent(ctx, event)

		case <-sub.Quit():
			return

		case <-s.quit:
			return
		}
	}
}

// defaultMaxGapFillPerTipEvent caps how many missed heights one
// processTipEvent invocation will walk before yielding back to the
// handleTipEvents loop. The cap is a safety belt for pathological
// cases (a long Esplora outage where the chain service has fallen far
// behind the live tip and no Rescan was triggered) — under steady-
// state operation each TipBlock event triggers at most one gap-fill
// height, so the cap is essentially never hit. 256 is well above any
// realistic burst of consecutive previously-failed events while still
// bounding each invocation's wall-clock to well under one second of
// Esplora HTTP under typical mempool.space latency. Overridable via
// WithMaxGapFillPerTipEvent for tests that need to exercise the
// per-event cap branch without producing 256+ heights of traffic.
const defaultMaxGapFillPerTipEvent int32 = 256

// processTipEvent applies one TipBlock to btcwallet's notification
// channel. The chain service owns its own delivery cursor
// (s.bestBlock); this function first walks any gap between
// s.bestBlock.Height and event.Height-1 — re-emitting heights that
// were missed by previously failed events — and only then processes
// the current event. The per-height worker (deliverHeight) advances
// s.bestBlock monotonically on success and leaves it untouched on
// failure, so a transient /block/:hash/raw outage no longer strands a
// height between btcwallet's view and the chain tip: the next
// TipBlock event re-walks whatever stretch is missing.
//
// The full block is only fetched when there is at least one watched
// address; without watchers there can be no relevant transactions, so
// the EsploraClient's raw-block call (and the associated bandwidth)
// is skipped.
func (s *EsploraChainService) processTipEvent(ctx context.Context,
	event *TipBlock) {

	if event == nil || event.Header == nil {
		return
	}

	// Build pkScript lookup from currently watched addresses once
	// and pass it through gap-fill and live-event delivery so every
	// block processed in this invocation sees the same watch set;
	// without this, an address added mid-walk would be visible only
	// for the live event and not for back-filled heights.
	s.mu.Lock()
	lastDelivered := s.bestBlock.Height
	watchedScripts := make(map[string]struct{}, len(s.watchedAddrs))
	for _, addr := range s.watchedAddrs {
		pkScript, err := txscript.PayToAddrScript(addr)
		if err != nil {
			continue
		}

		watchedScripts[string(pkScript)] = struct{}{}
	}
	s.mu.Unlock()

	// Duplicate or out-of-order event. This can happen on a
	// subscribe-time race or if a retry path emits an event already
	// covered by an earlier successful delivery. Nothing to do.
	if event.Height <= lastDelivered {
		return
	}

	// Re-emit anything between the last successful delivery and
	// the live event. fillGap returns false either because a per-
	// height fetch or send failed (in which case s.bestBlock is
	// left at the last delivered height and the next TipBlock
	// event will retry) or because the gap exceeded the per-event
	// cap (same outcome, the remaining stretch picks up next tick).
	if event.Height > lastDelivered+1 {
		if !s.fillGap(
			ctx, lastDelivered, event.Height-1, watchedScripts,
		) {
			return
		}
	}

	// Deliver the live event. Failure here leaves s.bestBlock at
	// the height the gap-fill walked up to, so the next event
	// retries this one — we intentionally drop the return value
	// because processTipEvent has no further work to do either way.
	_ = s.deliverHeight(
		ctx, event.Height, event.Hash,
		time.Unix(event.Header.Timestamp, 0), watchedScripts,
	)
}

// fillGap delivers every height in (start, end] in order. For each
// intermediate height the block header is served warm from
// EsploraClient.blockHeaderCache (TipPoller already populated it
// during its own per-height walk), but the height→hash lookup is a
// live /block-height/:h request — EsploraClient deliberately does not
// cache mutable height→hash mappings, so gap-fill pays one small HTTP
// round trip per missed height for that step. Work is bounded by
// defaultMaxGapFillPerTipEvent so a very deep gap does not hold the
// handleTipEvents goroutine for longer than one poll interval.
// Returns true only when the full (start, end] range was delivered;
// a false return means the caller should yield and rely on the next
// TipBlock event to continue from s.bestBlock.
func (s *EsploraChainService) fillGap(ctx context.Context,
	start, end int32, watchedScripts map[string]struct{}) bool {

	walkEnd := end
	if end-start > s.maxGapFillPerTipEvent {
		walkEnd = start + s.maxGapFillPerTipEvent
	}

	for h := start + 1; h <= walkEnd; h++ {
		hash, err := s.esplora.GetBlockHashByHeight(ctx, h)
		if err != nil {
			s.log.WarnS(
				ctx,
				"Chain service gap-fill hash fetch failed",
				err,
				slog.Int("height", int(h)),
			)

			return false
		}

		header, err := s.esplora.GetBlockHeader(ctx, hash)
		if err != nil {
			s.log.WarnS(
				ctx,
				"Chain service gap-fill header fetch failed",
				err,
				slog.Int("height", int(h)),
				slog.String("hash", hash.String()),
			)

			return false
		}

		ok := s.deliverHeight(
			ctx, h, hash, time.Unix(header.Timestamp, 0),
			watchedScripts,
		)
		if !ok {
			return false
		}
	}

	return walkEnd == end
}

// deliverHeight emits the FilteredBlockConnected + BlockConnected
// notification pair for a single height and advances s.bestBlock on
// success. Returns true iff both notifications were sent and
// s.bestBlock was advanced. A failure leaves s.bestBlock untouched so
// processTipEvent / fillGap can retry the same height on the next
// tip event.
//
// We use a select with quit on each send to prevent blocking
// indefinitely if the notification channel is full during initial
// sync (when handleChainNotifications is busy with syncWithChain /
// recovery). A shutdown mid-send returns without advancing
// s.bestBlock, so the next startup's first TipBlock event re-emits
// the same height — the cursor invariant survives crashes.
func (s *EsploraChainService) deliverHeight(ctx context.Context,
	height int32, hash chainhash.Hash, blockTime time.Time,
	watchedScripts map[string]struct{}) bool {

	blockMeta := wtxmgr.BlockMeta{
		Block: wtxmgr.Block{
			Hash:   hash,
			Height: height,
		},
		Time: blockTime,
	}

	// Filter block for relevant transactions if we have any
	// watched addresses. This requires fetching the full block
	// from Esplora; the EsploraClient memoizes the raw block by
	// hash so concurrent consumers (e.g. boarding tx-proof
	// builders) reuse the response.
	var relevantTxs []*wtxmgr.TxRecord
	if len(watchedScripts) > 0 {
		block, err := s.esplora.GetRawBlock(ctx, hash)
		if err != nil {
			s.log.WarnS(ctx, "Chain service block fetch failed",
				err,
				slog.Int("height", int(height)),
			)

			return false
		}

		relevantTxs = s.filterBlockTxs(
			ctx, block, watchedScripts, blockTime,
		)
	}

	// Send FilteredBlockConnected with relevant transactions so
	// btcwallet processes them via addRelevantTx. This is how
	// btcwallet learns about transactions paying to wallet-owned
	// addresses. ctx.Done() gives the goroutine a second exit if the
	// caller cancels the lifecycle context without going through
	// Stop, so a full notifications buffer cannot wedge shutdown.
	select {
	case s.notifications <- chain.FilteredBlockConnected{
		Block:       &blockMeta,
		RelevantTxs: relevantTxs,
	}:

	case <-ctx.Done():
		return false

	case <-s.quit:
		return false
	}

	// Send BlockConnected to update btcwallet's sync height.
	// FilteredBlockConnected only processes transactions but does
	// not update the sync height.
	select {
	case s.notifications <- chain.BlockConnected(blockMeta):
	case <-ctx.Done():
		return false

	case <-s.quit:
		return false
	}

	s.mu.Lock()
	s.bestBlock = waddrmgr.BlockStamp{
		Height:    height,
		Hash:      hash,
		Timestamp: blockTime,
	}
	s.mu.Unlock()

	return true
}

// filterBlockTxs checks all transactions in the block against the
// watched pkScripts and returns TxRecords for matching transactions.
func (s *EsploraChainService) filterBlockTxs(ctx context.Context,
	block *wire.MsgBlock,
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
		s.log.DebugS(ctx, "Found relevant transaction in block",
			slog.String("txid", txHash.String()),
			slog.Int("num_outputs", len(tx.TxOut)),
		)

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
