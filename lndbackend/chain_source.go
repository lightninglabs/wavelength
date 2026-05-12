package lndbackend

import (
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo/rounds"
)

// ChainSource implements rounds.ChainSource by wrapping a bitcoind RPC
// client. This is used by the server to validate UTXOs for boarding requests.
type ChainSource struct {
	rpcClient *rpcclient.Client
}

// NewChainSource creates a new ChainSource backed by a bitcoind RPC
// connection.
func NewChainSource(rpcClient *rpcclient.Client) *ChainSource {
	return &ChainSource{
		rpcClient: rpcClient,
	}
}

// GetUTXO fetches the UTXO for the given outpoint from bitcoind. Returns an
// error if the UTXO doesn't exist or has been spent.
func (c *ChainSource) GetUTXO(outpoint wire.OutPoint) (*rounds.UTXO, error) {
	// Look up the UTXO. The second parameter (true) includes the mempool
	// in the search.
	txOutResult, err := c.rpcClient.GetTxOut(
		&outpoint.Hash, outpoint.Index, true,
	)
	if err != nil {
		return nil, fmt.Errorf("gettxout RPC failed for %s: %w",
			outpoint, err)
	}

	// If txOutResult is nil, the UTXO doesn't exist or has been spent.
	if txOutResult == nil {
		return nil, fmt.Errorf("UTXO not found or spent: %s", outpoint)
	}

	// Convert the result to a wire.TxOut.
	txOut, err := txOutResultToTxOut(txOutResult)
	if err != nil {
		return nil, fmt.Errorf("failed to convert TxOut: %w", err)
	}

	return &rounds.UTXO{
		Output:        txOut,
		Confirmations: txOutResult.Confirmations,
	}, nil
}

// txOutResultToTxOut converts a btcjson.GetTxOutResult to a wire.TxOut.
func txOutResultToTxOut(result *btcjson.GetTxOutResult) (*wire.TxOut, error) {
	// Convert value from BTC to satoshis.
	value, err := btcutil.NewAmount(result.Value)
	if err != nil {
		return nil, fmt.Errorf("invalid UTXO value %v: %w",
			result.Value, err)
	}

	// Decode the scriptPubKey from hex.
	scriptPubKey, err := hex.DecodeString(result.ScriptPubKey.Hex)
	if err != nil {
		return nil, fmt.Errorf("failed to decode scriptPubKey hex: %w",
			err)
	}

	return &wire.TxOut{
		Value:    int64(value),
		PkScript: scriptPubKey,
	}, nil
}

// Compile-time check that ChainSource implements rounds.ChainSource.
var _ rounds.ChainSource = (*ChainSource)(nil)
