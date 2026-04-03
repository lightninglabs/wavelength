package darepo

import (
	"context"
	"fmt"
	"strconv"

	"github.com/lightninglabs/darepo/db"
	"github.com/lightninglabs/darepo/metrics"
	"github.com/lightninglabs/lndclient"
)

// systemStatsAdapter implements metrics.SystemStatsQuerier by
// combining database queries (via db.Store) with LND wallet queries
// (via lndclient.LightningClient).
type systemStatsAdapter struct {
	store     *db.Store
	lndClient lndclient.LightningClient
}

// newSystemStatsAdapter creates a new adapter that reads system stats
// from the provided database store and LND client.
func newSystemStatsAdapter(store *db.Store,
	lndClient lndclient.LightningClient) *systemStatsAdapter {

	return &systemStatsAdapter{
		store:     store,
		lndClient: lndClient,
	}
}

// GetVTXOStatsByStatus implements metrics.SystemStatsQuerier.
func (a *systemStatsAdapter) GetVTXOStatsByStatus(
	ctx context.Context) ([]metrics.VTXOStatRow, error) {

	rows, err := a.store.Queries.GetVTXOStatsByStatus(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]metrics.VTXOStatRow, 0, len(rows))
	for _, r := range rows {
		result = append(result, metrics.VTXOStatRow{
			Status:     r.Status,
			Count:      r.Count,
			TotalValue: toInt64(r.TotalValue),
		})
	}

	return result, nil
}

// GetRoundStatsByStatus implements metrics.SystemStatsQuerier.
func (a *systemStatsAdapter) GetRoundStatsByStatus(
	ctx context.Context) ([]metrics.StatusCountRow, error) {

	rows, err := a.store.Queries.GetRoundStatsByStatus(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]metrics.StatusCountRow, 0, len(rows))
	for _, r := range rows {
		result = append(result, metrics.StatusCountRow{
			Status: r.Status,
			Count:  r.Count,
		})
	}

	return result, nil
}

// GetOORSessionStatsByState implements metrics.SystemStatsQuerier.
func (a *systemStatsAdapter) GetOORSessionStatsByState(
	ctx context.Context) ([]metrics.StatusCountRow, error) {

	rows, err := a.store.Queries.GetOORSessionStatsByState(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]metrics.StatusCountRow, 0, len(rows))
	for _, r := range rows {
		result = append(result, metrics.StatusCountRow{
			Status: r.State,
			Count:  r.Count,
		})
	}

	return result, nil
}

// GetWalletBalance implements metrics.SystemStatsQuerier.
func (a *systemStatsAdapter) GetWalletBalance(
	ctx context.Context) (*metrics.WalletBalanceInfo, error) {

	balance, err := a.lndClient.WalletBalance(ctx)
	if err != nil {
		return nil, err
	}

	return &metrics.WalletBalanceInfo{
		Confirmed:   int64(balance.Confirmed),
		Unconfirmed: int64(balance.Unconfirmed),
	}, nil
}

// toInt64 converts a database aggregate value to int64. SQL aggregate
// functions like SUM return driver-dependent types: int64 on SQLite,
// but potentially []uint8 or string on PostgreSQL. This helper handles
// the common representations so the metrics gauge is accurate
// regardless of the backend.
func toInt64(v interface{}) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case int32:
		return int64(n)
	case []uint8:
		i, _ := strconv.ParseInt(string(n), 10, 64)
		return i
	case string:
		i, _ := strconv.ParseInt(n, 10, 64)
		return i
	default:
		// Fall back to Sprintf for any other numeric type the
		// driver might produce.
		s := fmt.Sprintf("%v", v)
		i, _ := strconv.ParseInt(s, 10, 64)
		return i
	}
}
