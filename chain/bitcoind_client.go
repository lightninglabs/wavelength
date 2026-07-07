package chain

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire/v2"
)

// BitcoindRPCClient extends the chain.BitcoindClient with additional methods
// that are not yet available in the standard btcd RPC client.
//
// TODO: remove this once https://github.com/btcsuite/btcwallet/pull/1009 is
// merged.
type BitcoindRPCClient struct {
	rpcClient *rpcclient.Client
}

// NewBitcoindRPCClient creates a new BitcoindRPCClient from an existing
// rpcclient.Client.
func NewBitcoindRPCClient(rpcClient *rpcclient.Client) *BitcoindRPCClient {
	return &BitcoindRPCClient{
		rpcClient: rpcClient,
	}
}

// SubmitPackage broadcasts a parents+child package atomically.
//
// `parents` must contain at least one transaction. The returned slice
// holds the txids of every transaction that entered the mempool.
//
// `maxFeeRateBTCPerVByte` caps the effective feerate the node will
// accept for the entire package, expressed in BTC per virtual byte.
// Pass nil to leave the limit unset.
//
// If the active backend cannot relay packages, ErrUnimplemented is
// returned.
//
//nolint:err113
func (c *BitcoindRPCClient) SubmitPackage(parents []*wire.MsgTx,
	child *wire.MsgTx, maxFeeRateBTCPerVByte *float64) (
	*btcjson.SubmitPackageResult, error) {

	// Sanity check inputs.
	if len(parents) == 0 {
		return nil, errors.New("submitpackage: need at least one " +
			"parent txn")
	}

	if child == nil {
		return nil, errors.New("submitpackage: child txn not defined")
	}

	// Prepare the hex encoded transactions to that we'll add the request.
	toHex := func(tx *wire.MsgTx) (string, error) {
		var buf bytes.Buffer
		if err := tx.Serialize(&buf); err != nil {
			return "", err
		}

		return hex.EncodeToString(buf.Bytes()), nil
	}

	rawTxs := make([]string, 0, len(parents)+1)

	for i, ch := range parents {
		h, err := toHex(ch)
		if err != nil {
			return nil, fmt.Errorf("cannot serialize parent txn "+
				"%d: %w", i, err)
		}

		rawTxs = append(rawTxs, h)
	}

	h, err := toHex(child)
	if err != nil {
		return nil, fmt.Errorf("cannot serialize child txn: %w", err)
	}

	rawTxs = append(rawTxs, h)

	// Parameter 1: The package (array of hex strings).
	rawTxsJSON, err := json.Marshal(rawTxs)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal raw txs array: %w",
			err)
	}

	params := []json.RawMessage{rawTxsJSON}

	// Parameter 2: maxfeerate (optional, numeric, in BTC/kvB)
	if maxFeeRateBTCPerVByte != nil {
		// Convert BTC/vByte to BTC/kvB.
		const btcPerVByteToKvB = 1000
		maxFeeRateBTCPerKvB := *maxFeeRateBTCPerVByte * btcPerVByteToKvB
		maxFeeRateJSON, err := json.Marshal(maxFeeRateBTCPerKvB)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal "+
				"maxfeerate: %w", err)
		}

		params = append(params, maxFeeRateJSON)
	}

	resp, err := c.rpcClient.RawRequest("submitpackage", params)
	if err != nil {
		return nil, err
	}

	// Unmarshall the response.
	var result btcjson.SubmitPackageResult
	if err := result.UnmarshalJSON(resp); err != nil {
		return nil, err
	}

	return &result, nil
}
