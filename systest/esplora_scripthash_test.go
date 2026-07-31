//go:build systest

package systest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/harness"
	"github.com/lightninglabs/wavelength/lwwallet"
	"github.com/stretchr/testify/require"
)

// TestEsploraScriptHashEncoding pins down which byte order the harness
// electrs expects on /scripthash/:hash. Start-block discovery probes
// that endpoint, so getting the convention wrong makes every address
// look unused and silently stamps a restored wallet past its own funds.
func TestEsploraScriptHashEncoding(t *testing.T) {
	h := harness.NewHarness(t, nil)
	h.Start()
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	var seed [32]byte
	for i := range seed {
		seed[i] = byte(i + 31)
	}

	w := newSyncTestWallet(t, h.EsploraURL, t.TempDir(), seed[:])
	require.NoError(t, w.Start())
	t.Cleanup(w.Stop)
	require.NoError(t, w.WaitForSync(ctx))

	addr, err := w.NewAddress(ctx)
	require.NoError(t, err)

	pkScript, err := txscript.PayToAddrScript(addr)
	require.NoError(t, err)

	h.Faucet(addr.String(), btcutil.Amount(40_000))
	h.Generate(1)

	sum := sha256.Sum256(pkScript)

	forward := hex.EncodeToString(sum[:])

	rev := sum
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	reversed := hex.EncodeToString(rev[:])

	// Poll the address endpoint first so the assertions below are not
	// racing the indexer.
	addrURL := h.EsploraURL + "/address/" + addr.String()

	indexed := func() bool {
		return txCountAt(t, ctx, addrURL) > 0
	}

	require.Eventually(
		t, indexed, time.Minute, 500*time.Millisecond,
		"electrs never indexed the funding",
	)

	byAddr := txCountAt(t, ctx, addrURL)
	fwd := txCountAt(t, ctx, h.EsploraURL+"/scripthash/"+forward)
	revd := txCountAt(t, ctx, h.EsploraURL+"/scripthash/"+reversed)

	t.Logf("address     tx_count = %d", byAddr)
	t.Logf("sha256      tx_count = %d  (%s)", fwd, forward)
	t.Logf("sha256 rev  tx_count = %d  (%s)", revd, reversed)

	require.Positive(t, byAddr, "address endpoint should see the funding")

	// Assert the direction, not merely that one of the two orders
	// works. Both are computed here, so one of them necessarily
	// matches, and an either-or assertion would hold no matter which
	// order the client itself uses.
	require.Equal(
		t, byAddr, fwd,
		"electrs indexes scripts under the forward SHA256 digest",
	)
	require.Zero(
		t, revd, "the Electrum reversal must not resolve, or the "+
			"two conventions would be indistinguishable here",
	)

	// Finally, run the real client against the same script. This is
	// what ties lwwallet's own encoding to the byte order established
	// above, rather than leaving it to a stub that agrees with
	// whatever the client does.
	client := lwwallet.NewEsploraClient(h.EsploraURL, btclog.Disabled)

	stats, err := client.GetScriptStats(ctx, pkScript)
	require.NoError(t, err)
	require.True(
		t, stats.Used(),
		"lwwallet's script hash encoding does not find a funded "+
			"script that electrs has indexed",
	)
}

// txCountAt fetches chain_stats.tx_count from an Esplora stats URL.
func txCountAt(t *testing.T, ctx context.Context, url string) int {
	t.Helper()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	if resp.StatusCode != http.StatusOK {
		t.Logf("%s -> %d %s", url, resp.StatusCode, body)

		return -1
	}

	var stats struct {
		ChainStats struct {
			TxCount int `json:"tx_count"`
		} `json:"chain_stats"`
	}
	require.NoError(t, json.Unmarshal(body, &stats))

	return stats.ChainStats.TxCount
}
