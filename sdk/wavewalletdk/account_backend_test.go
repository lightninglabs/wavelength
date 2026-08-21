//go:build !js

package wavewalletdk

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/btcwbackend"
	"github.com/lightninglabs/wavelength/lwwallet"
	"github.com/lightninglabs/wavelength/walletcore"
	"github.com/lightninglabs/wavelength/waved"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestExternalSeedIdentityMatchesSelfManagedBackends verifies the same
// externally derived entropy produces the same Wavelength identity through
// both supported self-managed wallet implementations. The chain transports
// differ, but their m/1017' key-family contract must not.
func TestExternalSeedIdentityMatchesSelfManagedBackends(t *testing.T) {
	entropy := testExternalSeedEntropy()

	var rawSeed [walletcore.SeedLen]byte
	copy(rawSeed[:], entropy[:])
	password := deriveExternalSeedDBPassword(entropy)

	genesisHash := chaincfg.RegressionNetParams.GenesisHash.String()

	// Serve a static genesis-only Esplora chain and a minimal fee response.
	// No external service is involved in the backend compatibility check.
	httpServer := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/blocks/tip/height":
				_, _ = w.Write([]byte("0"))

			case "/block-height/0":
				_, _ = w.Write([]byte(genesisHash))

			case "/block/" + genesisHash:
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id":        genesisHash,
					"height":    0,
					"timestamp": 1,
				})

			case "/fee":
				_, _ = w.Write([]byte(
					`{"fee_by_block_target":{"2":1000}}`,
				))

			default:
				http.NotFound(w, r)
			}
		},
	))
	t.Cleanup(httpServer.Close)

	lw, err := lwwallet.New(lwwallet.Config{
		Seed:           rawSeed[:],
		WalletPassword: password,
		EsploraURL:     httpServer.URL,
		ChainParams:    &chaincfg.RegressionNetParams,
		PollInterval:   time.Hour,
		RecoveryWindow: 1,
		DBDir:          t.TempDir(),
		Log:            fn.None[btclog.Logger](),
	})
	require.NoError(t, err)
	t.Cleanup(lw.Stop)
	require.NoError(t, lw.Start())

	btc, err := btcwbackend.New(btcwbackend.Config{
		Config: walletcore.Config{
			Seed:           rawSeed[:],
			WalletPassword: password,
			ChainParams:    &chaincfg.RegressionNetParams,
			RecoveryWindow: 1,
			DBDir:          t.TempDir(),
			Log:            fn.None[btclog.Logger](),
		},
		NeutrinoDataDir:      t.TempDir(),
		ConnectPeers:         []string{},
		FeeURL:               httpServer.URL + "/fee",
		DisableGlobalLoggers: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, btc.ChainBackend().Stop())
		btc.Stop()
	})

	// Start btcwallet's common wallet layer so it creates and unlocks the
	// m/1017' key scope. The neutrino chain service is already running; we
	// intentionally leave the higher chain notifier stopped because this is
	// a key-derivation test, not a synchronization test.
	require.NoError(t, btc.BtcWallet.Start())

	locator := keychain.KeyLocator{
		Family: keychain.KeyFamilyNodeKey,
		Index:  0,
	}
	lwIdentity, err := lw.DeriveKey(t.Context(), locator)
	require.NoError(t, err, "derive lwwallet identity")
	btcIdentity, err := btc.DeriveKey(t.Context(), locator)
	require.NoError(t, err, "derive btcwallet identity")
	require.Equal(
		t, lwIdentity.PubKey.SerializeCompressed(),
		btcIdentity.PubKey.SerializeCompressed(),
	)

	expectedIdentity, err := waved.WalletIdentityPubKeyFromSeed(
		rawSeed, "regtest",
	)
	require.NoError(t, err)
	require.Equal(
		t, expectedIdentity,
		hex.EncodeToString(
			lwIdentity.PubKey.SerializeCompressed(),
		),
	)
}
