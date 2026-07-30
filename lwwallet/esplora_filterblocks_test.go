package lwwallet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/stretchr/testify/require"
)

// fakeIndex is an Esplora stand-in that serves only the endpoints the
// index-native FilterBlocks uses: scripthash history and raw tx. It
// deliberately serves **no** block endpoint, so a FilterBlocks that falls
// back to downloading blocks fails loudly instead of passing quietly.
type fakeIndex struct {
	mu sync.Mutex

	// history maps a scripthash hex to its confirmed history, newest
	// first, exactly as Esplora orders it.
	history map[string][]esploraScriptTx

	// txs maps a txid string to the raw transaction.
	txs map[string]*wire.MsgTx

	// blocks and heights hold canonical block fixtures for fallback and
	// Rescan tests.
	blocks  map[string]*wire.MsgBlock
	heights map[int32]string

	// blockFetches counts hits on any block endpoint. The whole point
	// of the change is that this stays zero.
	blockFetches int

	// rawBlockFetches counts block-body downloads separately from the
	// small metadata requests Rescan always makes.
	rawBlockFetches int

	// pages caps entries per history response, mirroring Esplora's 25.
	pages int
}

// unsupportedAddress implements btcaddr.Address with a destination type that
// txscript.PayToAddrScript deliberately rejects.
type unsupportedAddress struct{}

// String returns the test address label.
func (unsupportedAddress) String() string {
	return "unsupported"
}

// EncodeAddress returns the test address label.
func (unsupportedAddress) EncodeAddress() string {
	return "unsupported"
}

// ScriptAddress returns no standard script payload.
func (unsupportedAddress) ScriptAddress() []byte {
	return nil
}

// IsForNet reports that the synthetic address is network agnostic.
func (unsupportedAddress) IsForNet(*chaincfg.Params) bool {
	return true
}

func newFakeIndex() *fakeIndex {
	return &fakeIndex{
		history: make(map[string][]esploraScriptTx),
		txs:     make(map[string]*wire.MsgTx),
		blocks:  make(map[string]*wire.MsgBlock),
		heights: make(map[int32]string),
		pages:   25,
	}
}

// addTx records a transaction as confirmed at height for every script it
// pays to, and (when spending is simulated) for the scripts named in
// alsoCredit — Esplora lists a transaction in the history of every script
// it touches, including the ones it spends *from*.
func (f *fakeIndex) addTx(t *testing.T, tx *wire.MsgTx, height int64,
	blockHash chainhash.Hash, alsoCredit ...[]byte) {

	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()

	txid := tx.TxHash()
	f.txs[txid.String()] = tx

	ref := esploraScriptTx{
		Txid: txid.String(),
		Status: esploraStatus{
			Confirmed:   true,
			BlockHeight: height,
			BlockHash:   blockHash.String(),
		},
	}

	scripts := make([][]byte, 0, len(tx.TxOut)+len(alsoCredit))
	for _, out := range tx.TxOut {
		scripts = append(scripts, out.PkScript)
	}
	scripts = append(scripts, alsoCredit...)

	for _, script := range scripts {
		h := scriptHashHex(script)
		// Newest first.
		f.history[h] = append([]esploraScriptTx{ref}, f.history[h]...)
	}
}

// addBlock records a canonical block fixture and returns its metadata.
func (f *fakeIndex) addBlock(t *testing.T, height int32,
	txs ...*wire.MsgTx) wtxmgr.BlockMeta {

	t.Helper()

	blockTime := time.Unix(1_700_000_000+int64(height), 0)
	block := &wire.MsgBlock{
		Header: wire.BlockHeader{
			Version:   2,
			Timestamp: blockTime,
			Nonce:     uint32(height),
		},
		Transactions: txs,
	}
	if len(txs) > 0 {
		block.Header.MerkleRoot = txs[0].TxHash()
	}
	blockHash := block.BlockHash()

	f.mu.Lock()
	f.blocks[blockHash.String()] = block
	f.heights[height] = blockHash.String()
	f.mu.Unlock()

	return wtxmgr.BlockMeta{
		Block: wtxmgr.Block{
			Hash:   blockHash,
			Height: height,
		},
		Time: blockTime,
	}
}

func (f *fakeIndex) serve(t *testing.T) *EsploraChainService {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()

			path := r.URL.Path
			switch {
			case strings.HasPrefix(path, "/scripthash/") &&
				strings.Contains(path, "/txs"):

				parts := strings.Split(path, "/")
				hash := parts[2]
				refs := f.history[hash]

				// /txs/chain/:txid pages after that txid.
				if idx := strings.Index(
					path, "/chain/",
				); idx >= 0 {

					after := path[idx+len("/chain/"):]
					for i, ref := range refs {
						if ref.Txid == after {
							refs = refs[i+1:]

							break
						}
					}
				}
				if len(refs) > f.pages {
					refs = refs[:f.pages]
				}
				require.NoError(
					t, json.NewEncoder(w).Encode(refs),
				)

			case strings.HasPrefix(path, "/tx/") &&
				strings.HasSuffix(path, "/raw"):

				txid := strings.TrimSuffix(
					strings.TrimPrefix(path, "/tx/"),
					"/raw",
				)
				tx, ok := f.txs[txid]
				if !ok {
					w.WriteHeader(http.StatusNotFound)

					return
				}
				var buf bytes.Buffer
				require.NoError(t, tx.Serialize(&buf))
				_, err := w.Write(buf.Bytes())
				require.NoError(t, err)

			case path == "/blocks/tip/height":
				var tip int32
				for height := range f.heights {
					if height > tip {
						tip = height
					}
				}
				_, err := fmt.Fprint(w, tip)
				require.NoError(t, err)

			case strings.HasPrefix(path, "/block-height/"):
				f.blockFetches++
				height, err := strconv.ParseInt(
					strings.TrimPrefix(
						path, "/block-height/",
					),
					10,
					32,
				)
				require.NoError(t, err)
				hash, ok := f.heights[int32(height)]
				if !ok {
					w.WriteHeader(http.StatusNotFound)

					return
				}
				_, err = fmt.Fprint(w, hash)
				require.NoError(t, err)

			case strings.HasPrefix(path, "/block/") &&
				strings.HasSuffix(path, "/raw"):

				f.blockFetches++
				f.rawBlockFetches++
				hash := strings.TrimSuffix(
					strings.TrimPrefix(path, "/block/"),
					"/raw",
				)
				block, ok := f.blocks[hash]
				if !ok {
					w.WriteHeader(http.StatusNotFound)

					return
				}
				var buf bytes.Buffer
				require.NoError(t, block.Serialize(&buf))
				_, err := w.Write(buf.Bytes())
				require.NoError(t, err)

			case strings.HasPrefix(path, "/block/"):
				f.blockFetches++
				hash := strings.TrimPrefix(path, "/block/")
				block, ok := f.blocks[hash]
				if !ok {
					w.WriteHeader(http.StatusNotFound)

					return
				}
				var height int32
				for candidate, blockHash := range f.heights {
					if blockHash == hash {
						height = candidate
						break
					}
				}
				timestamp := block.Header.Timestamp.Unix()
				require.NoError(
					t,
					json.NewEncoder(
						w).Encode(
						esploraBlock{
							ID:        hash,
							Height:    height,
							Timestamp: timestamp,
						},
					),
				)

			case strings.HasPrefix(path, "/block"):
				f.blockFetches++
				w.WriteHeader(http.StatusInternalServerError)

			default:
				w.WriteHeader(http.StatusNotFound)
			}
		},
	))
	t.Cleanup(srv.Close)

	esplora := NewEsploraClient(srv.URL, btclog.Disabled)

	return &EsploraChainService{
		esplora:       esplora,
		log:           btclog.Disabled,
		notifications: make(chan interface{}, 100),
		runCtx:        t.Context(),
		quit:          make(chan struct{}),
	}
}

// testScript derives a deterministic p2wkh script and its address.
func testScript(t *testing.T, seed byte) ([]byte, btcaddr.Address) {
	t.Helper()

	hash := make([]byte, 20)
	for i := range hash {
		hash[i] = seed
	}
	addr, err := btcaddr.NewAddressWitnessPubKeyHash(
		hash, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	script, err := txscript.PayToAddrScript(addr)
	require.NoError(t, err)

	return script, addr
}

func blockMetaAt(height int32, seed byte) wtxmgr.BlockMeta {
	var h chainhash.Hash
	h[0] = seed

	return wtxmgr.BlockMeta{
		Block: wtxmgr.Block{
			Height: height,
			Hash:   h,
		},
		Time: time.Unix(1_700_000_000+int64(height), 0),
	}
}

func payTo(script []byte, value int64) *wire.MsgTx {
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 0xffffffff},
	})
	tx.AddTxOut(wire.NewTxOut(value, script))

	return tx
}

// TestFilterBlocksUsesIndexNotBlocks pins the core property: the response
// is derived from the address index and no block is ever downloaded. The
// fake serves 500 on any block endpoint, so a regression to per-block
// scanning fails rather than silently costing a round trip per block.
func TestFilterBlocksUsesIndexNotBlocks(t *testing.T) {
	t.Parallel()

	idx := newFakeIndex()
	script, addr := testScript(t, 0x01)

	meta := blockMetaAt(200, 0xaa)
	tx := payTo(script, 50_000)
	idx.addTx(t, tx, 200, meta.Hash)

	svc := idx.serve(t)

	scope := waddrmgr.KeyScope{Purpose: 84, Coin: 1}
	resp, err := svc.FilterBlocks(&chain.FilterBlocksRequest{
		Blocks: []wtxmgr.BlockMeta{
			blockMetaAt(199, 0xa9), meta, blockMetaAt(201, 0xab),
		},
		ExternalAddrs: map[waddrmgr.ScopedIndex]btcaddr.Address{
			{Scope: scope, Index: 7}: addr,
		},
		InternalAddrs:    map[waddrmgr.ScopedIndex]btcaddr.Address{},
		WatchedOutPoints: map[wire.OutPoint]btcaddr.Address{},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	require.EqualValues(
		t, 1, resp.BatchIndex,
		"must point at the matching block's index within the request",
	)
	require.Equal(t, int32(200), resp.BlockMeta.Height)
	require.Contains(t, resp.FoundExternalAddrs, scope)
	require.Contains(t, resp.FoundExternalAddrs[scope], uint32(7))
	require.Len(t, resp.RelevantTxns, 1)
	require.Equal(t, tx.TxHash(), resp.RelevantTxns[0].TxHash())

	require.Zero(
		t, idx.blockFetches, "FilterBlocks must not download blocks",
	)
}

// TestFilterBlocksFindsFullySpentAddress is the case a UTXO-based probe
// gets wrong. The address received and then spent everything, so it has no
// UTXOs — but BIP44 discovery must still count it as used, or the horizon
// stops expanding and funds further along the branch are never found.
func TestFilterBlocksFindsFullySpentAddress(t *testing.T) {
	t.Parallel()

	idx := newFakeIndex()
	script, addr := testScript(t, 0x02)

	fundMeta := blockMetaAt(300, 0xb0)
	fund := payTo(script, 80_000)
	idx.addTx(t, fund, 300, fundMeta.Hash)

	// Spend every output back out again; the script keeps a history
	// entry for the spending tx but owns no UTXO afterwards.
	spendMeta := blockMetaAt(301, 0xb1)
	spend := wire.NewMsgTx(2)
	fundHash := fund.TxHash()
	spend.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: fundHash, Index: 0},
	})
	other, _ := testScript(t, 0x03)
	spend.AddTxOut(wire.NewTxOut(70_000, other))
	idx.addTx(t, spend, 301, spendMeta.Hash, script)

	svc := idx.serve(t)

	scope := waddrmgr.KeyScope{Purpose: 84, Coin: 1}
	resp, err := svc.FilterBlocks(&chain.FilterBlocksRequest{
		Blocks: []wtxmgr.BlockMeta{fundMeta, spendMeta},
		ExternalAddrs: map[waddrmgr.ScopedIndex]btcaddr.Address{
			{Scope: scope, Index: 3}: addr,
		},
		InternalAddrs:    map[waddrmgr.ScopedIndex]btcaddr.Address{},
		WatchedOutPoints: map[wire.OutPoint]btcaddr.Address{},
	})
	require.NoError(t, err)
	require.NotNil(
		t, resp, "a fully spent address is still a used address",
	)
	require.Equal(t, int32(300), resp.BlockMeta.Height)
	require.Contains(t, resp.FoundExternalAddrs[scope], uint32(3))
}

// TestFilterBlocksReturnsLowestMatchingBlock pins the ordering the
// recovery loop depends on. The caller trims its batch past the returned
// index, so returning a later match would skip discovery in the blocks
// between and under-expand the horizon.
func TestFilterBlocksReturnsLowestMatchingBlock(t *testing.T) {
	t.Parallel()

	idx := newFakeIndex()
	early, earlyAddr := testScript(t, 0x11)
	late, lateAddr := testScript(t, 0x12)

	earlyMeta := blockMetaAt(400, 0xc0)
	lateMeta := blockMetaAt(410, 0xc1)

	// Register the later block first so the result cannot depend on
	// insertion or map order.
	idx.addTx(t, payTo(late, 10_000), 410, lateMeta.Hash)
	idx.addTx(t, payTo(early, 20_000), 400, earlyMeta.Hash)

	svc := idx.serve(t)

	scope := waddrmgr.KeyScope{Purpose: 84, Coin: 1}
	blocks := []wtxmgr.BlockMeta{
		blockMetaAt(399, 0xbf), earlyMeta,
		blockMetaAt(405, 0xc5), lateMeta,
	}
	resp, err := svc.FilterBlocks(&chain.FilterBlocksRequest{
		Blocks: blocks,
		ExternalAddrs: map[waddrmgr.ScopedIndex]btcaddr.Address{
			{Scope: scope, Index: 1}: earlyAddr,
			{Scope: scope, Index: 2}: lateAddr,
		},
		InternalAddrs:    map[waddrmgr.ScopedIndex]btcaddr.Address{},
		WatchedOutPoints: map[wire.OutPoint]btcaddr.Address{},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, int32(400), resp.BlockMeta.Height)
	require.EqualValues(t, 1, resp.BatchIndex)
}

// TestFilterBlocksIgnoresHeightsOutsideRequest pins the clamp. The index
// is live and can report a height the caller did not ask about — a block
// mined since the batch was built, or one a reorg has retracted. Reporting
// it would attribute a transaction to a block the wallet has no record of.
func TestFilterBlocksIgnoresHeightsOutsideRequest(t *testing.T) {
	t.Parallel()

	idx := newFakeIndex()
	script, addr := testScript(t, 0x21)

	// Only history is at 900; the request covers 500-502.
	outside := blockMetaAt(900, 0xd9)
	idx.addTx(t, payTo(script, 5_000), 900, outside.Hash)

	svc := idx.serve(t)

	scope := waddrmgr.KeyScope{Purpose: 84, Coin: 1}
	resp, err := svc.FilterBlocks(&chain.FilterBlocksRequest{
		Blocks: []wtxmgr.BlockMeta{
			blockMetaAt(500, 0xd0), blockMetaAt(501, 0xd1),
			blockMetaAt(502, 0xd2),
		},
		ExternalAddrs: map[waddrmgr.ScopedIndex]btcaddr.Address{
			{Scope: scope, Index: 0}: addr,
		},
		InternalAddrs:    map[waddrmgr.ScopedIndex]btcaddr.Address{},
		WatchedOutPoints: map[wire.OutPoint]btcaddr.Address{},
	})
	require.NoError(t, err)
	require.Nil(
		t, resp, "a match outside the requested range must not be "+
			"reported; nil is the contract's batch-complete signal",
	)
}

// TestFilterBlocksSkipsUnconfirmed pins that mempool entries are dropped.
// FilterBlocks is a confirmed-only path, and an unconfirmed entry carries
// height 0, which would otherwise be attributed to whatever block sits at
// that index.
func TestFilterBlocksSkipsUnconfirmed(t *testing.T) {
	t.Parallel()

	idx := newFakeIndex()
	script, addr := testScript(t, 0x31)

	tx := payTo(script, 1_000)
	txid := tx.TxHash()
	idx.txs[txid.String()] = tx
	idx.history[scriptHashHex(script)] = []esploraScriptTx{{
		Txid: txid.String(),
		Status: esploraStatus{
			Confirmed: false,
		},
	}}

	svc := idx.serve(t)

	scope := waddrmgr.KeyScope{Purpose: 84, Coin: 1}
	resp, err := svc.FilterBlocks(&chain.FilterBlocksRequest{
		Blocks: []wtxmgr.BlockMeta{blockMetaAt(0, 0xe0)},
		ExternalAddrs: map[waddrmgr.ScopedIndex]btcaddr.Address{
			{Scope: scope, Index: 0}: addr,
		},
		InternalAddrs:    map[waddrmgr.ScopedIndex]btcaddr.Address{},
		WatchedOutPoints: map[wire.OutPoint]btcaddr.Address{},
	})
	require.NoError(t, err)
	require.Nil(t, resp, "unconfirmed history must not match a block")
}

// TestFilterBlocksEmptyBatch pins the trivial contract case: no blocks
// means no match, and no requests at all.
func TestFilterBlocksEmptyBatch(t *testing.T) {
	t.Parallel()

	idx := newFakeIndex()
	svc := idx.serve(t)

	resp, err := svc.FilterBlocks(&chain.FilterBlocksRequest{})
	require.NoError(t, err)
	require.Nil(t, resp)
	require.Zero(t, idx.blockFetches)
}

// TestFilterBlocksPagesHistory pins the paging walk. Esplora returns 25
// confirmed entries per page; an address whose recent history is longer
// than that would otherwise never reveal its older, in-range activity.
func TestFilterBlocksPagesHistory(t *testing.T) {
	t.Parallel()

	idx := newFakeIndex()
	idx.pages = 5
	script, addr := testScript(t, 0x41)

	// Add the oldest entry first, then the recent entries in ascending
	// order. addTx prepends, leaving the history newest-first with the
	// target three pages back.
	target := blockMetaAt(1_000, 0xf0)
	idx.addTx(t, payTo(script, 999), 1_000, target.Hash)
	for i := 0; i < 11; i++ {
		h := int64(1_100 + i)
		meta := blockMetaAt(int32(h), byte(0x80+i))
		idx.addTx(t, payTo(script, int64(1_000+i)), h, meta.Hash)
	}

	// Newest-first ordering: the oldest entry must be last.
	require.Equal(
		t, 12, len(idx.history[scriptHashHex(script)]),
		"fixture should hold every entry",
	)

	svc := idx.serve(t)

	scope := waddrmgr.KeyScope{Purpose: 84, Coin: 1}
	resp, err := svc.FilterBlocks(&chain.FilterBlocksRequest{
		Blocks: []wtxmgr.BlockMeta{target},
		ExternalAddrs: map[waddrmgr.ScopedIndex]btcaddr.Address{
			{Scope: scope, Index: 5}: addr,
		},
		InternalAddrs:    map[waddrmgr.ScopedIndex]btcaddr.Address{},
		WatchedOutPoints: map[wire.OutPoint]btcaddr.Address{},
	})
	require.NoError(t, err)
	require.NotNil(
		t, resp,
		"in-range history beyond the first page must still be found",
	)
	require.Equal(t, int32(1_000), resp.BlockMeta.Height)
	require.Contains(t, resp.FoundExternalAddrs[scope], uint32(5))
}

// TestFilterBlocksFindsWatchedOutpointSpend pins that a transaction
// spending a watched outpoint is found even when it pays none of the
// derived addresses — and that the outpoint's own address is not reported
// as a found address, which would push the recovery horizon on evidence
// the caller never asked about.
func TestFilterBlocksFindsWatchedOutpointSpend(t *testing.T) {
	t.Parallel()

	idx := newFakeIndex()
	watchedScript, watchedAddr := testScript(t, 0x51)
	strangerScript, _ := testScript(t, 0x52)

	fund := payTo(watchedScript, 40_000)
	fundHash := fund.TxHash()

	spendMeta := blockMetaAt(600, 0xe6)
	spend := wire.NewMsgTx(2)
	spend.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: fundHash, Index: 0},
	})
	spend.AddTxOut(wire.NewTxOut(30_000, strangerScript))
	idx.addTx(t, spend, 600, spendMeta.Hash, watchedScript)

	svc := idx.serve(t)

	watched := wire.OutPoint{Hash: fundHash, Index: 0}
	resp, err := svc.FilterBlocks(&chain.FilterBlocksRequest{
		Blocks:        []wtxmgr.BlockMeta{spendMeta},
		ExternalAddrs: map[waddrmgr.ScopedIndex]btcaddr.Address{},
		InternalAddrs: map[waddrmgr.ScopedIndex]btcaddr.Address{},
		WatchedOutPoints: map[wire.OutPoint]btcaddr.Address{
			watched: watchedAddr,
		},
	})
	require.NoError(t, err)
	require.NotNil(t, resp, "a watched-outpoint spend is relevant")
	require.Contains(t, resp.FoundOutPoints, watched)
	require.Empty(
		t, resp.FoundExternalAddrs, "the watched outpoint's "+
			"address must not be reported as a discovered address",
	)
	require.Zero(t, idx.blockFetches)
}

// TestFilterBlocksPagesEntireBoundaryHeight verifies that pagination cannot
// stop while more entries may remain at the request's minimum height.
func TestFilterBlocksPagesEntireBoundaryHeight(t *testing.T) {
	t.Parallel()

	idx := newFakeIndex()
	idx.pages = 2
	ownedScript, ownedAddr := testScript(t, 0x53)
	otherScript, _ := testScript(t, 0x54)
	meta := blockMetaAt(650, 0xe7)

	fund := payTo(ownedScript, 50_000)
	outpoint := wire.OutPoint{Hash: fund.TxHash(), Index: 0}
	spend := wire.NewMsgTx(2)
	spend.AddTxIn(&wire.TxIn{PreviousOutPoint: outpoint})
	spend.AddTxOut(wire.NewTxOut(40_000, otherScript))

	// addTx prepends history. Add the true spend first so two false
	// candidates at the same height fill the first page ahead of it.
	idx.addTx(t, spend, 650, meta.Hash, ownedScript)
	idx.addTx(t, payTo(ownedScript, 2_000), 650, meta.Hash)
	idx.addTx(t, payTo(ownedScript, 1_000), 650, meta.Hash)

	svc := idx.serve(t)
	resp, err := svc.FilterBlocks(&chain.FilterBlocksRequest{
		Blocks:        []wtxmgr.BlockMeta{meta},
		ExternalAddrs: map[waddrmgr.ScopedIndex]btcaddr.Address{},
		InternalAddrs: map[waddrmgr.ScopedIndex]btcaddr.Address{},
		WatchedOutPoints: map[wire.OutPoint]btcaddr.Address{
			outpoint: ownedAddr,
		},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, spend.TxHash(), resp.RelevantTxns[0].TxHash())
	require.Zero(t, idx.blockFetches)
}

// TestFilterBlocksFallsBackForUnsupportedOutpointAddress preserves the old
// block-scanning behavior when an outpoint address cannot become a probe
// script.
func TestFilterBlocksFallsBackForUnsupportedOutpointAddress(t *testing.T) {
	t.Parallel()

	idx := newFakeIndex()
	otherScript, _ := testScript(t, 0x55)
	var previous chainhash.Hash
	previous[0] = 0x55
	outpoint := wire.OutPoint{Hash: previous, Index: 1}
	spend := wire.NewMsgTx(2)
	spend.AddTxIn(&wire.TxIn{PreviousOutPoint: outpoint})
	spend.AddTxOut(wire.NewTxOut(40_000, otherScript))
	meta := idx.addBlock(t, 660, spend)

	svc := idx.serve(t)
	resp, err := svc.FilterBlocks(&chain.FilterBlocksRequest{
		Blocks:        []wtxmgr.BlockMeta{meta},
		ExternalAddrs: map[waddrmgr.ScopedIndex]btcaddr.Address{},
		InternalAddrs: map[waddrmgr.ScopedIndex]btcaddr.Address{},
		WatchedOutPoints: map[wire.OutPoint]btcaddr.Address{
			outpoint: unsupportedAddress{},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, spend.TxHash(), resp.RelevantTxns[0].TxHash())
	require.Equal(t, 1, idx.rawBlockFetches)
}

// TestFilterBlocksCancellationSkipsFallback exercises the lifecycle-context
// guard that prevents a canceled chain service from starting a block scan.
func TestFilterBlocksCancellationSkipsFallback(t *testing.T) {
	t.Parallel()

	idx := newFakeIndex()
	script, addr := testScript(t, 0x56)
	tx := payTo(script, 50_000)
	meta := idx.addBlock(t, 670, tx)
	idx.addTx(t, tx, 670, meta.Hash)

	svc := idx.serve(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc.runCtx = ctx

	scope := waddrmgr.KeyScope{Purpose: 84, Coin: 1}
	resp, err := svc.FilterBlocks(&chain.FilterBlocksRequest{
		Blocks: []wtxmgr.BlockMeta{meta},
		ExternalAddrs: map[waddrmgr.ScopedIndex]btcaddr.Address{
			{Scope: scope, Index: 0}: addr,
		},
		InternalAddrs:    map[waddrmgr.ScopedIndex]btcaddr.Address{},
		WatchedOutPoints: map[wire.OutPoint]btcaddr.Address{},
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, resp)
	require.Zero(t, idx.rawBlockFetches)
}

// TestFilterBlocksContinuesAfterFalseCandidate covers a history entry that
// touches a watched-outpoint script without spending the watched outpoint.
// The matcher must continue to the later, actual spend rather than treating
// the earliest index candidate as the answer for the whole batch.
func TestFilterBlocksContinuesAfterFalseCandidate(t *testing.T) {
	t.Parallel()

	idx := newFakeIndex()
	ownedScript, ownedAddr := testScript(t, 0x61)
	otherScript, _ := testScript(t, 0x62)

	fundMeta := blockMetaAt(700, 0x71)
	fund := payTo(ownedScript, 50_000)
	idx.addTx(t, fund, 700, fundMeta.Hash)

	spendMeta := blockMetaAt(701, 0x72)
	spend := wire.NewMsgTx(2)
	outpoint := wire.OutPoint{Hash: fund.TxHash(), Index: 0}
	spend.AddTxIn(&wire.TxIn{PreviousOutPoint: outpoint})
	spend.AddTxOut(wire.NewTxOut(40_000, otherScript))
	idx.addTx(t, spend, 701, spendMeta.Hash, ownedScript)

	svc := idx.serve(t)
	resp, err := svc.FilterBlocks(&chain.FilterBlocksRequest{
		Blocks:        []wtxmgr.BlockMeta{fundMeta, spendMeta},
		ExternalAddrs: map[waddrmgr.ScopedIndex]btcaddr.Address{},
		InternalAddrs: map[waddrmgr.ScopedIndex]btcaddr.Address{},
		WatchedOutPoints: map[wire.OutPoint]btcaddr.Address{
			outpoint: ownedAddr,
		},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, int32(701), resp.BlockMeta.Height)
	require.Contains(t, resp.FoundOutPoints, outpoint)
	require.Zero(t, idx.blockFetches)
}

// TestFilterBlocksOrdersParentsBeforeChildren ensures index and map order
// cannot make btcwallet process a same-block child before its wallet-owned
// parent.
func TestFilterBlocksOrdersParentsBeforeChildren(t *testing.T) {
	t.Parallel()

	idx := newFakeIndex()
	script, addr := testScript(t, 0x63)
	meta := blockMetaAt(710, 0x73)

	parent := payTo(script, 50_000)
	child := wire.NewMsgTx(2)
	child.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  parent.TxHash(),
			Index: 0,
		},
	})
	child.AddTxOut(wire.NewTxOut(40_000, script))

	// Add the parent last so the index returns it before the child and
	// neither insertion order nor map order already satisfies the test.
	idx.addTx(t, child, 710, meta.Hash)
	idx.addTx(t, parent, 710, meta.Hash)

	scope := waddrmgr.KeyScope{Purpose: 84, Coin: 1}
	svc := idx.serve(t)
	resp, err := svc.FilterBlocks(&chain.FilterBlocksRequest{
		Blocks: []wtxmgr.BlockMeta{meta},
		ExternalAddrs: map[waddrmgr.ScopedIndex]btcaddr.Address{
			{Scope: scope, Index: 0}: addr,
		},
		InternalAddrs:    map[waddrmgr.ScopedIndex]btcaddr.Address{},
		WatchedOutPoints: map[wire.OutPoint]btcaddr.Address{},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Len(t, resp.RelevantTxns, 2)
	require.Equal(t, parent.TxHash(), resp.RelevantTxns[0].TxHash())
	require.Equal(t, child.TxHash(), resp.RelevantTxns[1].TxHash())
}

// TestFilterBlocksFallsBackAfterHistoryCap ensures a long script history
// never becomes a successful partial answer. Once the page budget is
// exhausted, FilterBlocks must scan the canonical requested block.
func TestFilterBlocksFallsBackAfterHistoryCap(t *testing.T) {
	t.Parallel()

	idx := newFakeIndex()
	idx.pages = 1
	script, addr := testScript(t, 0x64)
	targetTx := payTo(script, 50_000)
	target := idx.addBlock(t, 720, targetTx)
	idx.addTx(t, targetTx, 720, target.Hash)

	for i := int32(0); i <= maxScriptTxPages; i++ {
		height := 800 + i
		meta := blockMetaAt(height, byte(i))
		idx.addTx(
			t,
			payTo(
				script, int64(1_000+i),
			),
			int64(height),
			meta.Hash,
		)
	}

	scope := waddrmgr.KeyScope{Purpose: 84, Coin: 1}
	svc := idx.serve(t)
	resp, err := svc.FilterBlocks(&chain.FilterBlocksRequest{
		Blocks: []wtxmgr.BlockMeta{target},
		ExternalAddrs: map[waddrmgr.ScopedIndex]btcaddr.Address{
			{Scope: scope, Index: 0}: addr,
		},
		InternalAddrs:    map[waddrmgr.ScopedIndex]btcaddr.Address{},
		WatchedOutPoints: map[wire.OutPoint]btcaddr.Address{},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, target.Hash, resp.BlockMeta.Hash)
	require.Equal(t, 1, idx.rawBlockFetches)
}

// TestFilterBlocksFallsBackOnBlockMismatch ensures a same-height history
// entry from an orphaned block is never attributed to the canonical block.
func TestFilterBlocksFallsBackOnBlockMismatch(t *testing.T) {
	t.Parallel()

	idx := newFakeIndex()
	script, addr := testScript(t, 0x65)
	tx := payTo(script, 50_000)
	canonical := idx.addBlock(t, 730, tx)
	orphan := blockMetaAt(730, 0x7f)
	idx.addTx(t, tx, 730, orphan.Hash)

	scope := waddrmgr.KeyScope{Purpose: 84, Coin: 1}
	svc := idx.serve(t)
	resp, err := svc.FilterBlocks(&chain.FilterBlocksRequest{
		Blocks: []wtxmgr.BlockMeta{canonical},
		ExternalAddrs: map[waddrmgr.ScopedIndex]btcaddr.Address{
			{Scope: scope, Index: 0}: addr,
		},
		InternalAddrs:    map[waddrmgr.ScopedIndex]btcaddr.Address{},
		WatchedOutPoints: map[wire.OutPoint]btcaddr.Address{},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, canonical.Hash, resp.BlockMeta.Hash)
	require.Equal(t, 1, idx.rawBlockFetches)
}

// TestRescanUsesIndexAndOrdersDependencies exercises the public Rescan
// contract. It must emit the filtered notification before BlockConnected,
// avoid block bodies on a consistent index, and preserve transaction
// dependency order.
func TestRescanUsesIndexAndOrdersDependencies(t *testing.T) {
	t.Parallel()

	idx := newFakeIndex()
	script, addr := testScript(t, 0x66)
	parent := payTo(script, 50_000)
	child := wire.NewMsgTx(2)
	child.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  parent.TxHash(),
			Index: 0,
		},
	})
	child.AddTxOut(wire.NewTxOut(40_000, script))
	meta := idx.addBlock(t, 740, parent, child)
	idx.addTx(t, child, 740, meta.Hash)
	idx.addTx(t, parent, 740, meta.Hash)

	svc := idx.serve(t)
	require.NoError(
		t,
		svc.Rescan(
			&meta.Hash, []btcaddr.Address{addr},
			map[wire.OutPoint]btcaddr.Address{},
		),
	)

	filtered := requireNotification[chain.FilteredBlockConnected](
		t, svc.notifications,
	)
	require.Equal(t, meta.Hash, filtered.Block.Hash)
	require.Len(t, filtered.RelevantTxs, 2)
	require.Equal(t, parent.TxHash(), filtered.RelevantTxs[0].Hash)
	require.Equal(t, child.TxHash(), filtered.RelevantTxs[1].Hash)

	connected := requireNotification[chain.BlockConnected](
		t, svc.notifications,
	)
	require.Equal(t, meta.Hash, wtxmgr.BlockMeta(connected).Hash)
	require.Zero(t, idx.rawBlockFetches)
}

// TestRescanFallsBackOnBlockMismatch verifies that Rescan checks indexed
// block identity and scans the canonical block when history describes an
// orphan at the same height.
func TestRescanFallsBackOnBlockMismatch(t *testing.T) {
	t.Parallel()

	idx := newFakeIndex()
	script, addr := testScript(t, 0x67)
	tx := payTo(script, 50_000)
	canonical := idx.addBlock(t, 750, tx)
	orphan := blockMetaAt(750, 0x7e)
	idx.addTx(t, tx, 750, orphan.Hash)

	svc := idx.serve(t)
	require.NoError(
		t,
		svc.Rescan(
			&canonical.Hash, []btcaddr.Address{addr},
			map[wire.OutPoint]btcaddr.Address{},
		),
	)

	filtered := requireNotification[chain.FilteredBlockConnected](
		t, svc.notifications,
	)
	require.Equal(t, canonical.Hash, filtered.Block.Hash)
	require.Len(t, filtered.RelevantTxs, 1)
	require.Equal(t, tx.TxHash(), filtered.RelevantTxs[0].Hash)
	require.Equal(t, 1, idx.rawBlockFetches)
}

// TestRescanFallsBackForUnsupportedOutpointAddress verifies that an address
// which cannot become an index probe does not hide an outpoint spend.
func TestRescanFallsBackForUnsupportedOutpointAddress(t *testing.T) {
	t.Parallel()

	idx := newFakeIndex()
	otherScript, _ := testScript(t, 0x68)
	var previous chainhash.Hash
	previous[0] = 0x68
	outpoint := wire.OutPoint{Hash: previous, Index: 2}
	spend := wire.NewMsgTx(2)
	spend.AddTxIn(&wire.TxIn{PreviousOutPoint: outpoint})
	spend.AddTxOut(wire.NewTxOut(40_000, otherScript))
	meta := idx.addBlock(t, 760, spend)

	svc := idx.serve(t)
	require.NoError(
		t,
		svc.Rescan(
			&meta.Hash, nil, map[wire.OutPoint]btcaddr.Address{
				outpoint: unsupportedAddress{},
			},
		),
	)

	filtered := requireNotification[chain.FilteredBlockConnected](
		t, svc.notifications,
	)
	require.Len(t, filtered.RelevantTxs, 1)
	require.Equal(t, spend.TxHash(), filtered.RelevantTxs[0].Hash)
	require.Equal(t, 1, idx.rawBlockFetches)
}

// TestRescanTrustsEmptyIndex documents the unavoidable completeness boundary:
// a successful empty history is treated as authoritative, so Rescan advances
// without downloading a block that contains unindexed wallet activity.
func TestRescanTrustsEmptyIndex(t *testing.T) {
	t.Parallel()

	idx := newFakeIndex()
	script, addr := testScript(t, 0x69)
	tx := payTo(script, 50_000)
	meta := idx.addBlock(t, 770, tx)

	// Deliberately omit idx.addTx. The canonical block contains the wallet
	// transaction, but the otherwise healthy index reports empty history.
	svc := idx.serve(t)
	require.NoError(
		t,
		svc.Rescan(
			&meta.Hash, []btcaddr.Address{addr},
			map[wire.OutPoint]btcaddr.Address{},
		),
	)

	filtered := requireNotification[chain.FilteredBlockConnected](
		t, svc.notifications,
	)
	require.Empty(t, filtered.RelevantTxs)
	require.Zero(t, idx.rawBlockFetches)
}

// requireNotification waits for a notification of the requested concrete
// type and fails with a bounded timeout.
func requireNotification[T any](t *testing.T,
	notifications <-chan interface{}) T {

	t.Helper()

	select {
	case notification := <-notifications:
		typed, ok := notification.(T)
		require.True(t, ok, "unexpected notification %T", notification)

		return typed

	case <-time.After(5 * time.Second):
		require.FailNow(t, "notification timeout")

		var zero T

		return zero
	}
}
