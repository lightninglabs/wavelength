package lwwallet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
)

// EsploraClient is an HTTP REST client for the Esplora/mempool.space API.
// It provides methods for querying chain state, fetching transactions and
// UTXOs, estimating fees, and broadcasting transactions. The client is
// safe for concurrent use.
type EsploraClient struct {
	// baseURL is the Esplora API root (e.g. "https://mempool.space/api").
	baseURL string

	// httpClient is the underlying HTTP client with a configured timeout.
	httpClient *http.Client

	// log is the structured logger for this Esplora client instance.
	log btclog.Logger
}

// NewEsploraClient creates a new Esplora REST API client. The baseURL should
// point to the API root without a trailing slash (e.g.
// "https://mempool.space/api").
func NewEsploraClient(baseURL string,
	logger btclog.Logger) *EsploraClient {

	return &EsploraClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		log: logger,
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
func (c *EsploraClient) GetTipHeight() (int32, error) {
	body, err := c.get("/blocks/tip/height")
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
func (c *EsploraClient) GetTipHash() (chainhash.Hash, error) {
	body, err := c.get("/blocks/tip/hash")
	if err != nil {
		return chainhash.Hash{}, fmt.Errorf("get tip hash: %w", err)
	}

	hash, err := chainhash.NewHashFromStr(strings.TrimSpace(string(body)))
	if err != nil {
		return chainhash.Hash{}, fmt.Errorf(
			"parse tip hash: %w", err,
		)
	}

	return *hash, nil
}

// GetBlockHeader returns block metadata (height, timestamp) for the given
// block hash.
func (c *EsploraClient) GetBlockHeader(
	blockHash chainhash.Hash) (*esploraBlock, error) {

	body, err := c.get("/block/" + blockHash.String())
	if err != nil {
		return nil, fmt.Errorf("get block header: %w", err)
	}

	var block esploraBlock
	if err := json.Unmarshal(body, &block); err != nil {
		return nil, fmt.Errorf("parse block header: %w", err)
	}

	return &block, nil
}

// GetBlockHashByHeight returns the block hash at the given height.
func (c *EsploraClient) GetBlockHashByHeight(
	height int32) (chainhash.Hash, error) {

	body, err := c.get(
		"/block-height/" + strconv.FormatInt(int64(height), 10),
	)
	if err != nil {
		return chainhash.Hash{}, fmt.Errorf(
			"get block hash at height %d: %w", height, err,
		)
	}

	hash, err := chainhash.NewHashFromStr(strings.TrimSpace(string(body)))
	if err != nil {
		return chainhash.Hash{}, fmt.Errorf(
			"parse block hash: %w", err,
		)
	}

	return *hash, nil
}

// GetRawBlockHeader returns the deserialized 80-byte block header for
// the given block hash. The Esplora /block/:hash/header endpoint returns
// the header as a hex-encoded string which we decode and deserialize
// into a wire.BlockHeader.
func (c *EsploraClient) GetRawBlockHeader(
	blockHash chainhash.Hash) (*wire.BlockHeader, error) {

	body, err := c.get(
		"/block/" + blockHash.String() + "/header",
	)
	if err != nil {
		return nil, fmt.Errorf("get raw block header: %w", err)
	}

	// The response is a hex-encoded 80-byte block header.
	headerBytes, err := hex.DecodeString(
		strings.TrimSpace(string(body)),
	)
	if err != nil {
		return nil, fmt.Errorf(
			"decode block header hex: %w", err,
		)
	}

	var header wire.BlockHeader
	err = header.Deserialize(bytes.NewReader(headerBytes))
	if err != nil {
		return nil, fmt.Errorf(
			"deserialize block header: %w", err,
		)
	}

	return &header, nil
}

// GetRawBlock returns the raw serialized block bytes for the given hash.
func (c *EsploraClient) GetRawBlock(
	blockHash chainhash.Hash) (*wire.MsgBlock, error) {

	body, err := c.get("/block/" + blockHash.String() + "/raw")
	if err != nil {
		return nil, fmt.Errorf("get raw block: %w", err)
	}

	var block wire.MsgBlock
	err = block.Deserialize(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("deserialize block: %w", err)
	}

	return &block, nil
}

// GetScriptUtxos returns all UTXOs for the given pkScript.
func (c *EsploraClient) GetScriptUtxos(
	pkScript []byte) ([]esploraUtxo, error) {

	hash := scriptHashHex(pkScript)
	path := "/scripthash/" + hash + "/utxo"
	body, err := c.get(path)
	if err != nil {
		return nil, fmt.Errorf("get script utxos: %w", err)
	}

	var utxos []esploraUtxo
	if err := json.Unmarshal(body, &utxos); err != nil {
		return nil, fmt.Errorf("parse script utxos: %w", err)
	}

	// Log raw response when no UTXOs found to aid debugging.
	if len(utxos) == 0 {
		c.log.DebugS(context.Background(),
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
func (c *EsploraClient) GetAddressUtxos(
	address string) ([]esploraUtxo, error) {

	path := "/address/" + address + "/utxo"
	body, err := c.get(path)
	if err != nil {
		return nil, fmt.Errorf("get address utxos: %w", err)
	}

	var utxos []esploraUtxo
	if err := json.Unmarshal(body, &utxos); err != nil {
		return nil, fmt.Errorf("parse address utxos: %w", err)
	}

	// Log raw response when no UTXOs found to aid debugging.
	if len(utxos) == 0 {
		c.log.DebugS(context.Background(),
			"Esplora address query returned empty",
			slog.String("path", path),
			slog.String("raw_response",
				string(body)))
	}

	return utxos, nil
}

// GetTxStatus returns the confirmation status for a transaction.
func (c *EsploraClient) GetTxStatus(
	txid chainhash.Hash) (*esploraTxStatus, error) {

	body, err := c.get("/tx/" + txid.String() + "/status")
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
func (c *EsploraClient) GetRawTx(
	txid chainhash.Hash) (*wire.MsgTx, error) {

	body, err := c.get("/tx/" + txid.String() + "/raw")
	if err != nil {
		return nil, fmt.Errorf("get raw tx: %w", err)
	}

	var tx wire.MsgTx
	err = tx.Deserialize(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("deserialize tx: %w", err)
	}

	return &tx, nil
}

// BroadcastTx broadcasts a raw transaction to the network. Returns the
// txid string on success.
func (c *EsploraClient) BroadcastTx(tx *wire.MsgTx) (string, error) {
	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		return "", fmt.Errorf("serialize tx: %w", err)
	}

	txHex := hex.EncodeToString(buf.Bytes())

	body, err := c.post("/tx", txHex)
	if err != nil {
		return "", fmt.Errorf("broadcast tx: %w", err)
	}

	return strings.TrimSpace(string(body)), nil
}

// GetFeeEstimates returns the fee estimates from the Esplora API. The
// returned map has string keys representing confirmation targets and float64
// values representing fee rates in sat/vB.
func (c *EsploraClient) GetFeeEstimates() (map[string]float64, error) {
	body, err := c.get("/fee-estimates")
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
func (c *EsploraClient) GetOutspend(txid chainhash.Hash,
	vout uint32) (*esploraOutspend, error) {

	path := fmt.Sprintf(
		"/tx/%s/outspend/%d", txid.String(), vout,
	)

	body, err := c.get(path)
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
func (c *EsploraClient) get(path string) ([]byte, error) {
	resp, err := c.httpClient.Get(c.baseURL + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s",
			resp.StatusCode, string(body))
	}

	return body, nil
}

// TestMempoolAccept validates transactions against mempool policy
// without broadcasting them. This uses the Esplora POST /txs/test
// endpoint which proxies to Bitcoin Core's testmempoolaccept RPC.
func (c *EsploraClient) TestMempoolAccept(
	txns []*wire.MsgTx,
	maxFeeRate float64) ([]testMempoolAcceptResult, error) {

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
		url += fmt.Sprintf(
			"?maxfeerate=%f", maxFeeRate,
		)
	}

	resp, err := c.httpClient.Post(
		url, "application/json",
		bytes.NewReader(jsonBody),
	)
	if err != nil {
		return nil, fmt.Errorf("test mempool accept: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf(
			"read test mempool response: %w", err,
		)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s",
			resp.StatusCode, string(respBody))
	}

	var results []testMempoolAcceptResult
	if err := json.Unmarshal(respBody, &results); err != nil {
		return nil, fmt.Errorf(
			"parse test mempool response: %w", err,
		)
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

// post performs an HTTP POST request with a text body and returns the
// response body.
func (c *EsploraClient) post(path string,
	body string) ([]byte, error) {

	resp, err := c.httpClient.Post(
		c.baseURL+path, "text/plain",
		strings.NewReader(body),
	)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s",
			resp.StatusCode, string(respBody))
	}

	return respBody, nil
}
