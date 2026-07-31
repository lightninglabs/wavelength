package lwwallet

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/stretchr/testify/require"
)

// TestEsploraGetScriptStats verifies the script-usage probe hits the
// right route and reads both the confirmed and unconfirmed tallies.
func TestEsploraGetScriptStats(t *testing.T) {
	t.Parallel()

	pkScript := []byte{0x51, 0x20, 0x0a, 0x0b}

	sum := sha256.Sum256(pkScript)
	wantPath := "/scripthash/" + hex.EncodeToString(sum[:])

	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, wantPath, r.URL.Path)

			err := json.NewEncoder(w).Encode(esploraScriptStats{
				ChainStats: esploraScriptStatSet{
					FundedTxoCount: 2,
					SpentTxoCount:  1,
					TxCount:        3,
				},
			})
			require.NoError(t, err)
		}),
	)
	defer srv.Close()

	client := NewEsploraClient(srv.URL, btclog.Disabled)

	stats, err := client.GetScriptStats(t.Context(), pkScript)
	require.NoError(t, err)
	require.Equal(t, 3, stats.ChainStats.TxCount)
	require.True(t, stats.Used())
}

// TestEsploraScriptStatsUsed checks the used/unused verdict, including
// the mempool-only case: a script funded but not yet mined still counts
// as used, since a wallet must not conclude it has no history while a
// payment to it is sitting in the mempool.
func TestEsploraScriptStatsUsed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		stats esploraScriptStats
		want  bool
	}{{
		name: "never seen",
		want: false,
	}, {
		name: "confirmed history",
		stats: esploraScriptStats{
			ChainStats: esploraScriptStatSet{
				TxCount: 1,
			},
		},
		want: true,
	}, {
		name: "mempool only",
		stats: esploraScriptStats{
			MempoolStats: esploraScriptStatSet{
				TxCount: 1,
			},
		},
		want: true,
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, test.want, test.stats.Used())
		})
	}
}

// TestEsploraGetScriptChainTxs verifies the confirmed-history route,
// including the continuation form used to page backwards through a
// script's history toward its earliest appearance.
func TestEsploraGetScriptChainTxs(t *testing.T) {
	t.Parallel()

	pkScript := []byte{0x00, 0x14, 0x01}

	sum := sha256.Sum256(pkScript)
	base := "/scripthash/" + hex.EncodeToString(sum[:]) + "/txs/chain"

	const lastSeen = "beef"

	var gotPaths []string

	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPaths = append(gotPaths, r.URL.Path)

			err := json.NewEncoder(w).Encode([]esploraScriptTx{{
				Txid: "aabb",
				Status: esploraStatus{
					Confirmed:   true,
					BlockHeight: 4321,
				},
			}})
			require.NoError(t, err)
		}),
	)
	defer srv.Close()

	client := NewEsploraClient(srv.URL, btclog.Disabled)

	txs, err := client.GetScriptChainTxs(t.Context(), pkScript, "")
	require.NoError(t, err)
	require.Len(t, txs, 1)
	require.Equal(t, int64(4321), txs[0].Status.BlockHeight)
	require.True(t, txs[0].Status.Confirmed)

	_, err = client.GetScriptChainTxs(t.Context(), pkScript, lastSeen)
	require.NoError(t, err)

	require.Equal(t, []string{base, base + "/" + lastSeen}, gotPaths)
}
