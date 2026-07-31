package lwwallet

import (
	"context"
	"encoding/json"
	"fmt"
)

// esploraScriptStatSet is one half of an Esplora scripthash stats
// response: the confirmed (chain) or unconfirmed (mempool) tally of
// outputs paying to and spending from a script.
type esploraScriptStatSet struct {
	// FundedTxoCount is the number of outputs paying to the script.
	FundedTxoCount int `json:"funded_txo_count"`

	// SpentTxoCount is the number of those outputs already spent.
	SpentTxoCount int `json:"spent_txo_count"`

	// TxCount is the number of transactions referencing the script,
	// whether they pay to it or spend from it.
	TxCount int `json:"tx_count"`
}

// esploraScriptStats is the response from /scripthash/:hash. It is the
// cheapest "has this script ever been used?" probe the Esplora index
// offers: one small JSON document per script, with no dependence on how
// long the chain is.
type esploraScriptStats struct {
	// ChainStats tallies confirmed activity.
	ChainStats esploraScriptStatSet `json:"chain_stats"`

	// MempoolStats tallies unconfirmed activity.
	MempoolStats esploraScriptStatSet `json:"mempool_stats"`
}

// Used reports whether the index has ever seen this script on chain or
// in the mempool.
func (s *esploraScriptStats) Used() bool {
	return s.ChainStats.TxCount > 0 || s.MempoolStats.TxCount > 0
}

// esploraScriptTx is one entry of a scripthash transaction history
// page. Only the fields needed to walk history backwards and read
// confirmation heights are decoded.
type esploraScriptTx struct {
	// Txid is the hex-encoded transaction ID.
	Txid string `json:"txid"`

	// Status carries the confirmation state and height.
	Status esploraStatus `json:"status"`
}

// GetScriptStats returns the Esplora index's activity tally for the
// given pkScript. Responses are never cached: an unused script can
// become used at any time, and a stale "unused" answer would silently
// narrow wallet recovery.
func (c *EsploraClient) GetScriptStats(ctx context.Context, pkScript []byte) (
	*esploraScriptStats, error) {

	path := "/scripthash/" + scriptHashHex(pkScript)
	body, err := c.get(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("get script stats: %w", err)
	}

	var stats esploraScriptStats
	if err := json.Unmarshal(body, &stats); err != nil {
		return nil, fmt.Errorf("parse script stats: %w", err)
	}

	return &stats, nil
}

// GetScriptChainTxs returns one page of confirmed transactions for the
// given pkScript, newest first. Esplora pages this endpoint at 25
// entries; pass the last txid of a page as lastSeenTxid to fetch the
// next one, and treat an empty page as the end of history.
func (c *EsploraClient) GetScriptChainTxs(ctx context.Context, pkScript []byte,
	lastSeenTxid string) ([]esploraScriptTx, error) {

	path := "/scripthash/" + scriptHashHex(pkScript) + "/txs/chain"
	if lastSeenTxid != "" {
		path += "/" + lastSeenTxid
	}

	body, err := c.get(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("get script chain txs: %w", err)
	}

	var txs []esploraScriptTx
	if err := json.Unmarshal(body, &txs); err != nil {
		return nil, fmt.Errorf("parse script chain txs: %w", err)
	}

	return txs, nil
}
