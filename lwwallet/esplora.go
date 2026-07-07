package lwwallet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/chainbackends"
	"github.com/lightninglabs/neutrino/cache/lru"
	"golang.org/x/sync/singleflight"
)

// EsploraClient is an HTTP REST client for the Esplora/mempool.space API.
// It provides methods for querying chain state, fetching transactions and
// UTXOs, estimating fees, and broadcasting transactions. The client is
// safe for concurrent use.
//
// Hash-addressed responses (transactions, full blocks, block headers)
// are cached in process-local LRUs so repeated lookups for the same
// content do not re-issue HTTP requests. The caches are bounded by
// cumulative serialized byte size; see esplora_cache.go for the
// per-cache capacity constants. Live, mutable responses (tip height,
// address UTXOs, tx confirmation status, mempool fee estimates) are
// never cached.
type EsploraClient struct {
	// baseURL is the Esplora API root (e.g. "https://mempool.space/api").
	baseURL string

	// httpClient is the underlying HTTP client with a configured timeout.
	httpClient *http.Client

	// log is the structured logger for this Esplora client instance.
	log btclog.Logger

	// txCache memoizes /tx/:txid/raw responses keyed by txid. A
	// confirmed transaction is content-addressed by its txid so the
	// cached body can never go stale.
	txCache *lru.Cache[chainhash.Hash, cachedTx]

	// rawBlockCache memoizes /block/:hash/raw responses keyed by
	// block hash.
	rawBlockCache *lru.Cache[chainhash.Hash, cachedBlock]

	// rawHeaderCache memoizes /block/:hash/header responses keyed by
	// block hash.
	rawHeaderCache *lru.Cache[chainhash.Hash, cachedRawHeader]

	// blockHeaderCache memoizes /block/:hash JSON header responses
	// keyed by block hash.
	blockHeaderCache *lru.Cache[chainhash.Hash, cachedBlockHeader]

	// txSF, rawBlockSF, rawHeaderSF, and blockHeaderSF coalesce
	// concurrent cache misses for the same hash so that the four
	// content-addressed cache fillers above do not multiply HTTP
	// load against the rate-limited Esplora endpoint when several
	// consumers (typically chainBackend and chainSvc reacting to the
	// same TipPoller event) race to fetch the same block or
	// transaction. Each Group is keyed by the hash string and
	// re-checks the cache inside the work function so a sibling
	// fetch that populated the cache while we were waiting wins
	// without re-issuing the request.
	txSF          singleflight.Group
	rawBlockSF    singleflight.Group
	rawHeaderSF   singleflight.Group
	blockHeaderSF singleflight.Group
}

// NewEsploraClient creates a new Esplora REST API client. The baseURL should
// point to the API root without a trailing slash (e.g.
// "https://mempool.space/api").
func NewEsploraClient(baseURL string, logger btclog.Logger) *EsploraClient {
	return &EsploraClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		log: logger,
		txCache: lru.NewCache[chainhash.Hash, cachedTx](
			txCacheCapacity,
		),
		rawBlockCache: lru.NewCache[chainhash.Hash, cachedBlock](
			rawBlockCacheCapacity,
		),
		rawHeaderCache: lru.NewCache[chainhash.Hash, cachedRawHeader](
			rawHeaderCacheCapacity,
		),
		blockHeaderCache: lru.NewCache[
			chainhash.Hash, cachedBlockHeader,
		](
			blockHeaderCacheCapacity,
		),
	}
}

// esploraUtxo represents a single UTXO as returned by the Esplora
// /scripthash/:hash/utxo endpoint.
type esploraUtxo struct {
	// Txid is the transaction ID containing this output.
	Txid string `json:"txid"`

	// Vout is the output index within the transaction.
	Vout uint32 `json:"vout"`

	// Value is the output value in satoshis.
	Value int64 `json:"value"`

	// Status contains the confirmation status of the transaction.
	Status esploraStatus `json:"status"`
}

// esploraStatus represents the confirmation status of a transaction.
type esploraStatus struct {
	// Confirmed indicates whether the transaction is confirmed.
	Confirmed bool `json:"confirmed"`

	// BlockHeight is the block height where the transaction was confirmed.
	// Zero if unconfirmed.
	BlockHeight int64 `json:"block_height"`

	// BlockHash is the hex-encoded block hash. Empty if unconfirmed.
	BlockHash string `json:"block_hash"`
}

// esploraTxStatus represents the full status response for a single
// transaction from the /tx/:txid/status endpoint.
type esploraTxStatus struct {
	// Confirmed indicates whether the transaction is confirmed.
	Confirmed bool `json:"confirmed"`

	// BlockHeight is the confirmation height (0 if unconfirmed).
	BlockHeight uint32 `json:"block_height"`

	// BlockHash is the hex-encoded confirmation block hash.
	BlockHash string `json:"block_hash"`
}

// esploraOutspend represents the response from the
// /tx/:txid/outspend/:vout endpoint.
type esploraOutspend struct {
	// Spent indicates whether the output has been spent.
	Spent bool `json:"spent"`

	// Txid is the spending transaction ID (empty if unspent).
	Txid string `json:"txid"`

	// Vin is the input index in the spending transaction.
	Vin uint32 `json:"vin"`

	// Status contains the confirmation status of the spending
	// transaction.
	Status esploraStatus `json:"status"`
}

// esploraBlock represents the JSON response for a block from the
// /block/:hash endpoint.
type esploraBlock struct {
	// ID is the hex-encoded block hash.
	ID string `json:"id"`

	// Height is the block height.
	Height int32 `json:"height"`

	// Timestamp is the block timestamp (unix seconds).
	Timestamp int64 `json:"timestamp"`
}

// scriptHashHex computes the Esplora script hash for a pkScript. The hash
// is SHA256(pkScript) with the bytes reversed and hex-encoded, matching
// Electrum's script hash format used by Esplora.
func scriptHashHex(pkScript []byte) string {
	h := sha256.Sum256(pkScript)

	// Reverse the hash bytes (Electrum convention).
	for i, j := 0, len(h)-1; i < j; i, j = i+1, j-1 {
		h[i], h[j] = h[j], h[i]
	}

	return hex.EncodeToString(h[:])
}

// GetTipHeight returns the current best block height.
func (c *EsploraClient) GetTipHeight(ctx context.Context) (int32, error) {
	body, err := c.get(ctx, "/blocks/tip/height")
	if err != nil {
		return 0, fmt.Errorf("get tip height: %w", err)
	}

	height, err := strconv.ParseInt(strings.TrimSpace(string(body)), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parse tip height: %w", err)
	}

	return int32(height), nil
}

// GetTipHash returns the current best block hash.
func (c *EsploraClient) GetTipHash(ctx context.Context) (chainhash.Hash,
	error) {

	body, err := c.get(ctx, "/blocks/tip/hash")
	if err != nil {
		return chainhash.Hash{}, fmt.Errorf("get tip hash: %w", err)
	}

	hash, err := chainhash.NewHashFromStr(strings.TrimSpace(string(body)))
	if err != nil {
		return chainhash.Hash{}, fmt.Errorf("parse tip hash: %w", err)
	}

	return *hash, nil
}

// GetBlockHeader returns block metadata (height, timestamp) for the given
// block hash. Results are memoized in blockHeaderCache because the
// header for a confirmed block hash is immutable. Concurrent misses
// for the same hash are coalesced via blockHeaderSF.
func (c *EsploraClient) GetBlockHeader(ctx context.Context,
	blockHash chainhash.Hash) (*esploraBlock, error) {

	if cached, err := c.blockHeaderCache.Get(blockHash); err == nil &&
		cached.header != nil {
		return cached.header, nil
	}

	v, err, _ := c.blockHeaderSF.Do(blockHash.String(),
		func() (interface{}, error) {
			// A sibling caller may have populated the cache
			// while we were waiting on the singleflight slot;
			// re-check before issuing an HTTP request.
			if cached, cErr := c.blockHeaderCache.Get(
				blockHash,
			); cErr == nil && cached.header != nil {
				return cached.header, nil
			}

			body, err := c.get(ctx, "/block/"+blockHash.String())
			if err != nil {
				return nil, fmt.Errorf("get block header: %w",
					err)
			}

			var block esploraBlock
			if err := json.Unmarshal(body, &block); err != nil {
				return nil, fmt.Errorf("parse block header: %w",
					err)
			}

			// Verify the response actually describes the
			// block we asked for before populating the
			// cache. Without this check a buggy, MITM'd, or
			// compromised Esplora endpoint could pin an
			// arbitrary entry under blockHash for the rest
			// of this process's lifetime — none of the four
			// content-addressed caches have a TTL.
			gotID, err := chainhash.NewHashFromStr(block.ID)
			if err != nil {
				return nil, fmt.Errorf("parse block id %q: %w",
					block.ID, err)
			}
			if *gotID != blockHash {
				return nil, fmt.Errorf("block id mismatch: "+
					"got %s, want %s", gotID, blockHash)
			}

			if _, putErr := c.blockHeaderCache.Put(
				blockHash, cachedBlockHeader{
					header: &block,
				},
			); putErr != nil {

				c.log.WarnS(ctx,
					"Block header cache Put failed",
					putErr,
					slog.String(
						"hash", blockHash.String(),
					))
			}

			return &block, nil
		})
	if err != nil {
		return nil, err
	}

	block, ok := v.(*esploraBlock)
	if !ok {
		return nil, fmt.Errorf("block header singleflight returned %T",
			v)
	}

	return block, nil
}

// GetBlockHashByHeight returns the block hash at the given height.
func (c *EsploraClient) GetBlockHashByHeight(ctx context.Context,
	height int32) (chainhash.Hash, error) {

	body, err := c.get(
		ctx, "/block-height/"+strconv.FormatInt(int64(height), 10),
	)
	if err != nil {
		return chainhash.Hash{}, fmt.Errorf("get block hash at height "+
			"%d: %w", height, err)
	}

	hash, err := chainhash.NewHashFromStr(strings.TrimSpace(string(body)))
	if err != nil {
		return chainhash.Hash{}, fmt.Errorf("parse block hash: %w", err)
	}

	return *hash, nil
}

// GetRawBlockHeader returns the deserialized 80-byte block header for
// the given block hash. The Esplora /block/:hash/header endpoint returns
// the header as a hex-encoded string which we decode and deserialize
// into a wire.BlockHeader. Results are memoized in rawHeaderCache
// because the header for a confirmed block hash is immutable.
// Concurrent misses for the same hash are coalesced via rawHeaderSF.
func (c *EsploraClient) GetRawBlockHeader(ctx context.Context,
	blockHash chainhash.Hash) (*wire.BlockHeader, error) {

	if cached, err := c.rawHeaderCache.Get(blockHash); err == nil &&
		cached.header != nil {
		return cached.header, nil
	}

	v, err, _ := c.rawHeaderSF.Do(blockHash.String(),
		func() (interface{}, error) {
			if cached, cErr := c.rawHeaderCache.Get(
				blockHash,
			); cErr == nil && cached.header != nil {
				return cached.header, nil
			}

			body, err := c.get(
				ctx, "/block/"+blockHash.String()+"/header",
			)
			if err != nil {
				return nil, fmt.Errorf("get raw block "+
					"header: %w", err)
			}

			// The response is a hex-encoded 80-byte block
			// header.
			headerBytes, err := hex.DecodeString(
				strings.TrimSpace(
					string(body),
				),
			)
			if err != nil {
				return nil, fmt.Errorf("decode block "+
					"header hex: %w", err)
			}

			var header wire.BlockHeader
			err = header.Deserialize(
				bytes.NewReader(headerBytes),
			)
			if err != nil {
				return nil, fmt.Errorf("deserialize block "+
					"header: %w", err)
			}

			// Hash-verify before caching. See GetBlockHeader
			// for the rationale.
			if got := header.BlockHash(); got != blockHash {
				return nil, fmt.Errorf("raw header hash "+
					"mismatch: got %s, want %s", got,
					blockHash)
			}

			if _, putErr := c.rawHeaderCache.Put(
				blockHash, cachedRawHeader{
					header: &header,
				},
			); putErr != nil {

				c.log.WarnS(ctx,
					"Raw header cache Put failed",
					putErr,
					slog.String(
						"hash", blockHash.String(),
					))
			}

			return &header, nil
		})
	if err != nil {
		return nil, err
	}

	header, ok := v.(*wire.BlockHeader)
	if !ok {
		return nil, fmt.Errorf("raw header singleflight returned %T", v)
	}

	return header, nil
}

// GetRawBlock returns the raw serialized block bytes for the given
// hash. Results are memoized in rawBlockCache because a confirmed
// block's contents are content-addressed by their hash. Concurrent
// misses for the same hash are coalesced via rawBlockSF — full
// mainnet blocks approach 4 MiB so collapsing a thundering herd is
// load-bearing for the rate-limit budget of the Esplora endpoint.
//
//nolint:dupl
func (c *EsploraClient) GetRawBlock(ctx context.Context,
	blockHash chainhash.Hash) (*wire.MsgBlock, error) {

	if cached, err := c.rawBlockCache.Get(blockHash); err == nil &&
		cached.block != nil {
		return cached.block, nil
	}

	v, err, _ := c.rawBlockSF.Do(blockHash.String(),
		func() (interface{}, error) {
			if cached, cErr := c.rawBlockCache.Get(
				blockHash,
			); cErr == nil && cached.block != nil {
				return cached.block, nil
			}

			body, err := c.get(
				ctx, "/block/"+blockHash.String()+"/raw",
			)
			if err != nil {
				return nil, fmt.Errorf("get raw block: %w", err)
			}

			var block wire.MsgBlock
			err = block.Deserialize(bytes.NewReader(body))
			if err != nil {
				return nil, fmt.Errorf("deserialize block: %w",
					err)
			}

			// Hash-verify before caching. See GetBlockHeader
			// for the rationale.
			if got := block.BlockHash(); got != blockHash {
				return nil, fmt.Errorf("raw block hash "+
					"mismatch: got %s, want %s", got,
					blockHash)
			}

			if _, putErr := c.rawBlockCache.Put(
				blockHash, cachedBlock{
					block: &block,
					size:  uint64(len(body)),
				},
			); putErr != nil {

				c.log.WarnS(ctx,
					"Raw block cache Put failed",
					putErr,
					slog.String(
						"hash", blockHash.String(),
					))
			}

			return &block, nil
		})
	if err != nil {
		return nil, err
	}

	block, ok := v.(*wire.MsgBlock)
	if !ok {
		return nil, fmt.Errorf("raw block singleflight returned %T", v)
	}

	return block, nil
}

// GetScriptUtxos returns all UTXOs for the given pkScript.
func (c *EsploraClient) GetScriptUtxos(ctx context.Context, pkScript []byte) (
	[]esploraUtxo, error) {

	hash := scriptHashHex(pkScript)
	path := "/scripthash/" + hash + "/utxo"
	body, err := c.get(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("get script utxos: %w", err)
	}

	var utxos []esploraUtxo
	if err := json.Unmarshal(body, &utxos); err != nil {
		return nil, fmt.Errorf("parse script utxos: %w", err)
	}

	// Log raw response when no UTXOs found to aid debugging.
	if len(utxos) == 0 {
		c.log.DebugS(ctx,
			"Esplora returned empty UTXO list",
			slog.String("path", path),
			slog.String("raw_response",
				string(body)))
	}

	return utxos, nil
}

// GetAddressUtxos returns all UTXOs for the given address string.
// This uses the /address/:address/utxo endpoint which avoids the need
// to compute a scripthash. The response format is identical to the
// scripthash UTXO endpoint.
func (c *EsploraClient) GetAddressUtxos(ctx context.Context, address string) (
	[]esploraUtxo, error) {

	path := "/address/" + address + "/utxo"
	body, err := c.get(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("get address utxos: %w", err)
	}

	var utxos []esploraUtxo
	if err := json.Unmarshal(body, &utxos); err != nil {
		return nil, fmt.Errorf("parse address utxos: %w", err)
	}

	// Log raw response when no UTXOs found to aid debugging.
	if len(utxos) == 0 {
		c.log.DebugS(ctx,
			"Esplora address query returned empty",
			slog.String("path", path),
			slog.String("raw_response",
				string(body)))
	}

	return utxos, nil
}

// GetTxStatus returns the confirmation status for a transaction.
func (c *EsploraClient) GetTxStatus(ctx context.Context, txid chainhash.Hash) (
	*esploraTxStatus, error) {

	body, err := c.get(ctx, "/tx/"+txid.String()+"/status")
	if err != nil {
		return nil, fmt.Errorf("get tx status: %w", err)
	}

	var status esploraTxStatus
	if err := json.Unmarshal(body, &status); err != nil {
		return nil, fmt.Errorf("parse tx status: %w", err)
	}

	return &status, nil
}

// GetRawTx returns the raw serialized transaction bytes for a txid.
// Results are memoized in txCache because a confirmed transaction's
// contents are content-addressed by its txid. Concurrent misses for
// the same txid are coalesced via txSF.
//
//nolint:dupl
func (c *EsploraClient) GetRawTx(ctx context.Context, txid chainhash.Hash) (
	*wire.MsgTx, error) {

	if cached, err := c.txCache.Get(txid); err == nil &&
		cached.tx != nil {
		return cached.tx, nil
	}

	v, err, _ := c.txSF.Do(txid.String(),
		func() (interface{}, error) {
			if cached, cErr := c.txCache.Get(
				txid,
			); cErr == nil && cached.tx != nil {
				return cached.tx, nil
			}

			body, err := c.get(ctx, "/tx/"+txid.String()+"/raw")
			if err != nil {
				return nil, fmt.Errorf("get raw tx: %w", err)
			}

			var tx wire.MsgTx
			err = tx.Deserialize(bytes.NewReader(body))
			if err != nil {
				return nil, fmt.Errorf("deserialize tx: %w",
					err)
			}

			// Hash-verify before caching. See GetBlockHeader
			// for the rationale. TxHash returns the
			// witness-stripped txid, which is what
			// /tx/:txid/raw is keyed by.
			if got := tx.TxHash(); got != txid {
				return nil, fmt.Errorf("tx hash mismatch: got "+
					"%s, want %s", got, txid)
			}

			if _, putErr := c.txCache.Put(txid, cachedTx{
				tx:   &tx,
				size: uint64(len(body)),
			}); putErr != nil {

				c.log.WarnS(ctx, "Tx cache Put failed",
					putErr,
					slog.String("txid", txid.String()),
				)
			}

			return &tx, nil
		})
	if err != nil {
		return nil, err
	}

	tx, ok := v.(*wire.MsgTx)
	if !ok {
		return nil, fmt.Errorf("raw tx singleflight returned %T", v)
	}

	return tx, nil
}

// BroadcastTx broadcasts a raw transaction to the network. Returns the
// txid string on success.
func (c *EsploraClient) BroadcastTx(ctx context.Context, tx *wire.MsgTx) (
	string, error) {

	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		return "", fmt.Errorf("serialize tx: %w", err)
	}

	txHex := hex.EncodeToString(buf.Bytes())

	body, err := c.post(ctx, "/tx", txHex)
	if err != nil {
		return "", fmt.Errorf("broadcast tx: %w", err)
	}

	return strings.TrimSpace(string(body)), nil
}

// GetFeeEstimates returns the fee estimates from the Esplora API. The
// returned map has string keys representing confirmation targets and float64
// values representing fee rates in sat/vB.
func (c *EsploraClient) GetFeeEstimates(ctx context.Context) (
	map[string]float64, error) {

	body, err := c.get(ctx, "/fee-estimates")
	if err != nil {
		return nil, fmt.Errorf("get fee estimates: %w", err)
	}

	var estimates map[string]float64
	if err := json.Unmarshal(body, &estimates); err != nil {
		return nil, fmt.Errorf("parse fee estimates: %w", err)
	}

	return estimates, nil
}

// GetOutspend returns the spending status of a specific output.
func (c *EsploraClient) GetOutspend(ctx context.Context, txid chainhash.Hash,
	vout uint32) (*esploraOutspend, error) {

	path := fmt.Sprintf("/tx/%s/outspend/%d", txid.String(), vout)

	body, err := c.get(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("get outspend: %w", err)
	}

	var outspend esploraOutspend
	if err := json.Unmarshal(body, &outspend); err != nil {
		return nil, fmt.Errorf("parse outspend: %w", err)
	}

	return &outspend, nil
}

// get performs an HTTP GET request and returns the response body.
func (c *EsploraClient) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet, c.baseURL+path, nil,
	)
	if err != nil {
		return nil, fmt.Errorf("build GET request: %w", err)
	}

	resp, err := c.httpClient.Do(req) //nolint:gosec // G704: trusted URL
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode,
			string(body))
	}

	return body, nil
}

// TestMempoolAccept validates transactions against mempool policy
// without broadcasting them. This uses the Esplora POST /txs/test
// endpoint which proxies to Bitcoin Core's testmempoolaccept RPC.
func (c *EsploraClient) TestMempoolAccept(ctx context.Context,
	txns []*wire.MsgTx, maxFeeRate float64) ([]testMempoolAcceptResult,
	error) {

	// Serialize each transaction to hex.
	txHexes := make([]string, 0, len(txns))
	for _, tx := range txns {
		var buf bytes.Buffer
		if err := tx.Serialize(&buf); err != nil {
			return nil, fmt.Errorf("serialize tx: %w", err)
		}

		txHexes = append(
			txHexes, hex.EncodeToString(buf.Bytes()),
		)
	}

	jsonBody, err := json.Marshal(txHexes)
	if err != nil {
		return nil, fmt.Errorf("marshal tx hexes: %w", err)
	}

	// Build the URL with optional maxfeerate query parameter.
	url := c.baseURL + "/txs/test"
	if maxFeeRate > 0 {
		url += fmt.Sprintf("?maxfeerate=%f", maxFeeRate)
	}

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, url, bytes.NewReader(jsonBody),
	)
	if err != nil {
		return nil, fmt.Errorf("build test mempool request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req) //nolint:gosec // G704: trusted URL
	if err != nil {
		return nil, fmt.Errorf("test mempool accept: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read test mempool response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode,
			string(respBody))
	}

	var results []testMempoolAcceptResult
	if err := json.Unmarshal(respBody, &results); err != nil {
		return nil, fmt.Errorf("parse test mempool response: %w", err)
	}

	return results, nil
}

// testMempoolAcceptResult represents the result of testing a single
// transaction against mempool policy via Bitcoin Core's
// testmempoolaccept RPC (proxied through Esplora).
type testMempoolAcceptResult struct {
	// Txid is the transaction hash.
	Txid string `json:"txid"`

	// Wtxid is the witness transaction hash.
	Wtxid string `json:"wtxid"`

	// Allowed indicates whether the transaction would be accepted.
	Allowed bool `json:"allowed"`

	// Vsize is the virtual transaction size in vbytes.
	Vsize int32 `json:"vsize"`

	// Fees contains fee information (only when Allowed is true).
	Fees *testMempoolAcceptFees `json:"fees,omitempty"`

	// RejectReason is the rejection reason (only when Allowed is
	// false).
	//nolint:tagliatelle
	RejectReason string `json:"reject-reason,omitempty"`
}

// testMempoolAcceptFees contains fee details for accepted
// transactions.
type testMempoolAcceptFees struct {
	// Base is the transaction fee in BTC.
	Base float64 `json:"base"`
}

// submitPackageResponse describes the Esplora /txs/package response body.
type submitPackageResponse struct {
	// PackageMsg reports whether the package was accepted.
	PackageMsg string `json:"package_msg"`

	// TxResults maps per-tx results by wtxid.
	TxResults map[string]submitPackageTxResult `json:"tx-results"` //nolint:tagliatelle,ll
}

// submitPackageTxResult describes one transaction's package-relay result.
type submitPackageTxResult struct {
	// Txid is the transaction ID for this package member.
	Txid string `json:"txid"`

	// Error is populated when package relay rejected this tx.
	Error *string `json:"error"`
}

// SubmitPackage posts a package of raw transaction hex strings to the
// Esplora /txs/package endpoint for atomic package relay. Transactions must be
// ordered by dependency with parents first and child last. The context
// controls cancellation/timeout for the HTTP request.
func (c *EsploraClient) SubmitPackage(ctx context.Context,
	txHexes []string) error {

	jsonBody, err := json.Marshal(txHexes)
	if err != nil {
		return fmt.Errorf("marshal package txs: %w", err)
	}

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.baseURL+"/txs/package",
		bytes.NewReader(jsonBody),
	)
	if err != nil {
		return fmt.Errorf("build package request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req) //nolint:gosec // G704: trusted URL
	if err != nil {
		return fmt.Errorf("submit package: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read package response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("submit package HTTP %d: %s", resp.StatusCode,
			string(respBody))
	}

	if len(respBody) > 0 {
		c.log.DebugS(ctx, "Package response",
			slog.String("body", string(respBody)),
		)
	}

	if len(respBody) == 0 {
		return nil
	}

	var packageResp submitPackageResponse
	if err := json.Unmarshal(respBody, &packageResp); err != nil {
		return fmt.Errorf("decode package response: %w", err)
	}

	// Surface per-tx results as typed *chainbackends.PackageTxError
	// values so callers can errors.Is against rpcclient sentinels
	// instead of substring-matching the raw reject reason. The Esplora
	// txid field is a hex string; an unparseable txid is logged and
	// dropped rather than failing the whole error path, since the
	// reject string is still preserved verbatim in the PackageTxError.
	var txErrors []error
	for wtxid, txResult := range packageResp.TxResults {
		if txResult.Error == nil || *txResult.Error == "" {
			continue
		}

		txid, err := chainhash.NewHashFromStr(txResult.Txid)
		if err != nil {
			c.log.WarnS(
				ctx,
				"Esplora package result has unparseable txid",
				err,
				slog.String("wtxid", wtxid),
				slog.String("txid_raw", txResult.Txid),
			)

			txid = &chainhash.Hash{}
		}

		txErrors = append(
			txErrors, chainbackends.NewPackageTxError(
				wtxid, *txid, *txResult.Error,
			),
		)
	}

	if packageResp.PackageMsg != "success" || len(txErrors) > 0 {
		if len(txErrors) == 0 {
			return fmt.Errorf("package not accepted: %s",
				packageResp.PackageMsg)
		}

		return fmt.Errorf("package not accepted: %s: %w",
			packageResp.PackageMsg, errors.Join(txErrors...))
	}

	return nil
}

// post performs an HTTP POST request with a text body and returns the
// response body.
func (c *EsploraClient) post(ctx context.Context, path string, body string) (
	[]byte, error) {

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.baseURL+path, strings.NewReader(body),
	)
	if err != nil {
		return nil, fmt.Errorf("build POST request: %w", err)
	}
	req.Header.Set("Content-Type", "text/plain")

	resp, err := c.httpClient.Do(req) //nolint:gosec // G704: trusted URL
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode,
			string(respBody))
	}

	return respBody, nil
}
