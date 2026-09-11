//go:build systest

package systest

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/waved"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/stretchr/testify/require"
)

// TestAccountBalanceReporting exercises real LND account aggregation through
// the daemon RPC and HTTP metrics. A watch-only co-tenant account is sufficient
// to reproduce the reporting bug; this test makes no claim about spending it.
func TestAccountBalanceReporting(t *testing.T) {
	ParallelN(t)
	metricsAddr := newLoopbackAddr(t)
	fixture := newDirectedSendFixture(t, func(cfg *waved.Config) {
		cfg.Metrics.ListenAddr = metricsAddr
		cfg.Lnd.Account = ""
	})
	h := fixture.harness.Harness
	tenant := h.StartAdditionalLND("balance-tenant")
	accounts, err := tenant.Client.WalletKit.ListAccounts(
		t.Context(),
		"default", walletrpc.AddressType_TAPROOT_PUBKEY,
	)
	require.NoError(t, err)
	require.Len(t, accounts, 1)
	ctx, timeout, raw := h.LND.WalletKit.RawClientWithMacAuth(t.Context())
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, err = raw.ImportAccount(ctx, &walletrpc.ImportAccountRequest{
		Name:              "co-tenant",
		ExtendedPublicKey: accounts[0].ExtendedPublicKey,
		AddressType:       walletrpc.AddressType_TAPROOT_PUBKEY,
	})
	require.NoError(t, err)

	for account, amount := range map[string]btcutil.Amount{
		"default": 100_000, "co-tenant": 250_000,
	} {
		addr, err := h.LND.WalletKit.NextAddr(
			t.Context(), account,
			walletrpc.AddressType_TAPROOT_PUBKEY, false,
		)
		require.NoError(t, err)
		h.Faucet(addr.String(), amount)
	}
	h.GenerateAndWait(1)
	// The old wrapper must observe both accounts; this negative control
	// ensures a missing co-tenant credit cannot make the test pass.
	require.Eventually(t, func() bool {
		balance, err := h.LND.Client.WalletBalance(t.Context())

		return err == nil && balance.Confirmed == 350_000
	}, 30*time.Second, 100*time.Millisecond)

	resp, err := fixture.client.GetBalance(
		t.Context(), &waverpc.GetBalanceRequest{},
	)
	require.NoError(t, err)
	require.EqualValues(t, 100_000, resp.OnchainWalletConfirmedSat)

	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(
		t.Context(), http.MethodGet, "http://"+metricsAddr+"/metrics",
		nil,
	)
	require.NoError(t, err)
	scrape, err := client.Do(req)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, scrape.Body.Close())
	}()
	require.Equal(t, http.StatusOK, scrape.StatusCode)
	body, err := io.ReadAll(scrape.Body)
	require.NoError(t, err)
	require.Contains(
		t, string(body),
		"waved_wallet_confirmed_satoshis 100000\n",
	)
	require.Contains(
		t, string(body),
		"waved_wallet_unconfirmed_satoshis 0\n",
	)
}
