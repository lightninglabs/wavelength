package harness

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/wire"
)

// BitcoindPackageSubmitter implements chainbackends.PackageSubmitter
// via direct JSON-RPC calls to a bitcoind node. Used by the itest
// harness to provide package submission without going through LND.
type BitcoindPackageSubmitter struct {
	url      string
	user     string
	password string
}

// NewBitcoindPackageSubmitter creates a new submitter that talks to
// the given bitcoind JSON-RPC endpoint.
func NewBitcoindPackageSubmitter(host, user,
	password string) *BitcoindPackageSubmitter {

	return &BitcoindPackageSubmitter{
		url:      fmt.Sprintf("http://%s", host),
		user:     user,
		password: password,
	}
}

// SubmitPackage implements chainbackends.PackageSubmitter by calling
// bitcoind's submitpackage JSON-RPC method.
func (s *BitcoindPackageSubmitter) SubmitPackage(
	parents []*wire.MsgTx, child *wire.MsgTx,
	_ *float64) (*btcjson.SubmitPackageResult, error) {

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
		"id":      "harness",
		"method":  "submitpackage",
		"params":  []any{txHexes, 0},
	}

	body, err := json.Marshal(rpcReq)
	if err != nil {
		return nil, fmt.Errorf("marshal rpc request: %w", err)
	}

	httpReq, err := http.NewRequest(
		http.MethodPost, s.url, bytes.NewReader(body),
	)
	if err != nil {
		return nil, fmt.Errorf("create http request: %w", err)
	}

	httpReq.SetBasicAuth(s.user, s.password)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	// Parse the JSON-RPC response.
	var rpcResp struct {
		Result *btcjson.SubmitPackageResult `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w (body: %s)",
			err, string(respBody))
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
