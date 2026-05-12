// Package bitcoindrpc provides a direct-to-bitcoind JSON-RPC
// implementation of chainbackends.PackageSubmitter, used by the
// production daemon (and by integration tests) when the LND v3
// submitpackage path is not available.
//
// The shape of the helper is deliberately narrow: it knows how to call
// bitcoind's submitpackage method and nothing else, so it can be wired
// into darepod.Config without pulling in harness-only code or opening
// the rest of a bitcoind RPC surface.
package bitcoindrpc

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// SubmitPackage implements chainbackends.PackageSubmitter by calling
// bitcoind's submitpackage JSON-RPC method.
func (s *PackageSubmitter) SubmitPackage(ctx context.Context,
	parents []*wire.MsgTx, child *wire.MsgTx, _ *float64) (
	*btcjson.SubmitPackageResult, error) {

	// Serialize all transactions to hex.
	txHexes := make([]string, 0, len(parents)+1)
	for _, tx := range parents {
		var buf bytes.Buffer
		if err := tx.Serialize(&buf); err != nil {
			return nil, fmt.Errorf("serialize parent: %w", err)
		}

		txHexes = append(txHexes, hex.EncodeToString(buf.Bytes()))
	}

	var childBuf bytes.Buffer
	if err := child.Serialize(&childBuf); err != nil {
		return nil, fmt.Errorf("serialize child: %w", err)
	}

	txHexes = append(txHexes, hex.EncodeToString(childBuf.Bytes()))

	// Build the JSON-RPC request. Set maxfeerate=0 to disable the
	// per-transaction feerate limit. The CPFP child intentionally has
	// a high feerate to pay for the zero-fee parent, and the default
	// maxfeerate (0.10 BTC/kvB) would reject it.
	rpcReq := map[string]any{
		"jsonrpc": "1.0",
		"id":      "darepo-client",
		"method":  "submitpackage",
		"params": []any{
			txHexes,
			0,
		},
	}

	body, err := json.Marshal(rpcReq)
	if err != nil {
		return nil, fmt.Errorf("marshal rpc request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(
		ctx, http.MethodPost, s.url, bytes.NewReader(body),
	)
	if err != nil {
		return nil, fmt.Errorf("create http request: %w", err)
	}

	httpReq.SetBasicAuth(s.user, s.password)
	httpReq.Header.Set("Content-Type", "application/json")

	// URL is the operator-configured bitcoind JSON-RPC endpoint,
	// not caller-controlled input; it is the same s.url used for
	// every other request on this submitter.
	resp, err := s.client.Do(httpReq) //nolint:gosec // G704: trusted URL
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	// Parse the JSON-RPC response. bitcoind returns HTTP 500 with a
	// JSON-RPC error envelope for method-level errors, so we try to
	// decode the body first and only fall back to an HTTP-level error
	// when the body does not parse (e.g. HTML error pages from 401/
	// 403/proxy errors).
	var rpcResp struct {
		Result *btcjson.SubmitPackageResult `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("submit package HTTP %d: %s",
				resp.StatusCode, string(respBody))
		}

		return nil, fmt.Errorf("unmarshal response: %w (body: %s)", err,
			string(respBody))
	}

	if rpcResp.Error != nil {
		return nil, fmt.Errorf("bitcoind error %d: %s",
			rpcResp.Error.Code, rpcResp.Error.Message)
	}

	if rpcResp.Result == nil {
		return nil, fmt.Errorf("nil result from submitpackage "+
			"(body: %s)", string(respBody))
	}

	return rpcResp.Result, nil
}
