// Package bitcoindrpc provides direct-to-bitcoind JSON-RPC helpers used by
// the production daemon and integration tests when publication must bypass
// lnd's wallet publish path.
//
// The shape of the helper is deliberately narrow: it knows how to call the
// transaction publication methods darepo needs without pulling in harness-only
// code or opening the rest of a bitcoind RPC surface.
package bitcoindrpc

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/wire"
)

// submitTimeout bounds the submitpackage RPC so a wedged bitcoind
// can't stall the caller for the full parent context timeout (test or
// production).
const submitTimeout = 30 * time.Second

// PackageSubmitter implements chainbackends.PackageSubmitter via
// direct JSON-RPC calls to a bitcoind node.
type PackageSubmitter struct {
	url      string
	user     string
	password string

	// client is a dedicated HTTP client so we don't share state with
	// http.DefaultClient, and carries a timeout as a backstop for
	// callers that pass in a never-cancelled context.
	client *http.Client
}

// New creates a submitter that talks to the given bitcoind JSON-RPC
// endpoint. The host string is of the form "host:port"; the URL is
// built with an "http://" scheme because bitcoind's JSON-RPC server
// is plain HTTP by default, and TLS termination (when present) is
// expected to be handled by a separately configured reverse proxy.
func New(host, user, password string) *PackageSubmitter {
	return &PackageSubmitter{
		url:      fmt.Sprintf("http://%s", host),
		user:     user,
		password: password,
		client: &http.Client{
			Timeout: submitTimeout,
		},
	}
}

// PublishTransaction implements chainbackends.TxBroadcaster by calling
// bitcoind's sendrawtransaction JSON-RPC method. The label is intentionally
// ignored because this direct publication path bypasses wallet accounting.
func (s *PackageSubmitter) PublishTransaction(ctx context.Context,
	tx *wire.MsgTx, _ string) error {

	txHex, err := txToHex(tx)
	if err != nil {
		return fmt.Errorf("serialize tx: %w", err)
	}

	var result string
	err = s.call(ctx, "sendrawtransaction", []any{txHex}, &result)
	if err != nil {
		if isAlreadyKnownTxError(err) {
			return nil
		}

		return err
	}

	return nil
}

// SubmitPackage implements chainbackends.PackageSubmitter by calling
// bitcoind's submitpackage JSON-RPC method.
func (s *PackageSubmitter) SubmitPackage(ctx context.Context,
	parents []*wire.MsgTx, child *wire.MsgTx, _ *float64) (
	*btcjson.SubmitPackageResult, error) {

	// Serialize all transactions to hex.
	txHexes := make([]string, 0, len(parents)+1)
	for _, tx := range parents {
		txHex, err := txToHex(tx)
		if err != nil {
			return nil, fmt.Errorf("serialize parent: %w", err)
		}

		txHexes = append(txHexes, txHex)
	}

	childHex, err := txToHex(child)
	if err != nil {
		return nil, fmt.Errorf("serialize child: %w", err)
	}

	txHexes = append(txHexes, childHex)

	// Build the JSON-RPC request. Set maxfeerate=0 to disable the
	// per-transaction feerate limit. The CPFP child intentionally has
	// a high feerate to pay for the zero-fee parent, and the default
	// maxfeerate (0.10 BTC/kvB) would reject it.
	var result btcjson.SubmitPackageResult
	err = s.call(ctx, "submitpackage", []any{txHexes, 0}, &result)
	if err != nil {
		return nil, err
	}

	return &result, nil
}

// txToHex serializes a Bitcoin transaction to wire-format hex.
func txToHex(tx *wire.MsgTx) (string, error) {
	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		return "", err
	}

	return hex.EncodeToString(buf.Bytes()), nil
}

// call sends a JSON-RPC request to the configured bitcoind endpoint.
func (s *PackageSubmitter) call(ctx context.Context, method string,
	params []any, result any) error {

	rpcReq := map[string]any{
		"jsonrpc": "1.0",
		"id":      "darepo-client",
		"method":  method,
		"params":  params,
	}

	body, err := json.Marshal(rpcReq)
	if err != nil {
		return fmt.Errorf("marshal rpc request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(
		ctx, http.MethodPost, s.url, bytes.NewReader(body),
	)
	if err != nil {
		return fmt.Errorf("create http request: %w", err)
	}

	httpReq.SetBasicAuth(s.user, s.password)
	httpReq.Header.Set("Content-Type", "application/json")

	// URL is the operator-configured bitcoind JSON-RPC endpoint,
	// not caller-controlled input; it is the same s.url used for
	// every other request on this submitter.
	resp, err := s.client.Do(httpReq) //nolint:gosec // G704: trusted URL
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	// Parse the JSON-RPC response. bitcoind returns HTTP 500 with a
	// JSON-RPC error envelope for method-level errors, so we try to
	// decode the body first and only fall back to an HTTP-level error
	// when the body does not parse (e.g. HTML error pages from 401/
	// 403/proxy errors).
	var rpcResp struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}

	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s HTTP %d: %s", method,
				resp.StatusCode, string(respBody))
		}

		return fmt.Errorf("unmarshal response: %w (body: %s)", err,
			string(respBody))
	}

	if rpcResp.Error != nil {
		return rpcResp.Error
	}

	if len(rpcResp.Result) == 0 || string(rpcResp.Result) == "null" {
		return fmt.Errorf("nil result from %s (body: %s)", method,
			string(respBody))
	}

	if err := json.Unmarshal(rpcResp.Result, result); err != nil {
		return fmt.Errorf("unmarshal %s result: %w (body: %s)", method,
			err, string(respBody))
	}

	return nil
}

// rpcError is the JSON-RPC error envelope returned by bitcoind.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Error implements error.
func (e *rpcError) Error() string {
	return fmt.Sprintf("bitcoind error %d: %s", e.Code, e.Message)
}

// isAlreadyKnownTxError reports whether sendrawtransaction rejected a tx
// because that exact transaction is already known by bitcoind.
func isAlreadyKnownTxError(err error) bool {
	var rpcErr *rpcError
	if !errors.As(err, &rpcErr) {
		return false
	}

	if rpcErr.Code == int(btcjson.ErrRPCVerifyAlreadyInChain) {
		return true
	}

	msg := strings.ToLower(rpcErr.Message)

	return strings.Contains(msg, "already have transaction") ||
		strings.Contains(msg, "already in mempool") ||
		strings.Contains(msg, "already exists in blockchain") ||
		strings.Contains(msg, "transaction already exists")
}
