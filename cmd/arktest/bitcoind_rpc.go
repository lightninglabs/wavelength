//go:build itest

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type bitcoindRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      string `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type bitcoindRPCResponse struct {
	Result json.RawMessage   `json:"result"`
	Error  *bitcoindRPCError `json:"error"`
}

type bitcoindRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func callBitcoindRPC(
	ctx context.Context, state *harnessState, method string,
	params []any, out any,
) error {

	if state == nil {
		return fmt.Errorf("state is nil")
	}

	reqBody, err := json.Marshal(bitcoindRPCRequest{
		JSONRPC: "1.0",
		ID:      "arktest",
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return err
	}

	url := "http://" + state.BitcoindRPC
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, url, bytes.NewReader(reqBody),
	)
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(state.BitcoindRPCUser, state.BitcoindRPCPass)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	// bitcoind returns HTTP 500 for RPC-level errors (bad address, bad
	// params, etc.) with the actual error in the JSON body, so try the
	// JSON decode regardless of status code; only fall back to the
	// HTTP status error if the body isn't a valid RPC response.
	var rpcResp bitcoindRPCResponse
	decodeErr := json.NewDecoder(resp.Body).Decode(&rpcResp)
	if decodeErr == nil && rpcResp.Error != nil {
		return fmt.Errorf("bitcoind RPC %s failed (%d): %s",
			method, rpcResp.Error.Code, rpcResp.Error.Message)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("bitcoind RPC status %s", resp.Status)
	}

	if decodeErr != nil {
		return decodeErr
	}

	if out == nil {
		return nil
	}

	return json.Unmarshal(rpcResp.Result, out)
}
