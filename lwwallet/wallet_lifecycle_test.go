package lwwallet

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btclog/v2"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// newTestEsplora spins up a minimal Esplora stub that serves a static
// genesis-only chain, which is all wallet lifecycle tests need.
func newTestEsplora(t *testing.T) *httptest.Server {
	t.Helper()

	tipHash := chaincfg.RegressionNetParams.GenesisHash.String()
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/blocks/tip/height":
			_, err := w.Write([]byte("0"))
			require.NoError(t, err)

		case "/block-height/0":
			_, err := w.Write([]byte(tipHash))
			require.NoError(t, err)

		case "/block/" + tipHash:
			err := json.NewEncoder(w).Encode(esploraBlock{
				ID:        tipHash,
				Height:    0,
				Timestamp: 1,
			})
			require.NoError(t, err)

		default:
			http.NotFound(w, r)
		}
	}
	esplora := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(esplora.Close)

	return esplora
}

// testWalletConfig returns a lifecycle-test config for the given
// database directory, seed, and password.
func testWalletConfig(esploraURL, dbDir string, seed, password []byte) Config {
	return Config{
		Seed:           seed,
		WalletPassword: password,
		EsploraURL:     esploraURL,
		ChainParams:    &chaincfg.RegressionNetParams,
		PollInterval:   time.Hour,
		RecoveryWindow: 10,
		DBDir:          dbDir,
		Log:            fn.None[btclog.Logger](),
	}
}

// TestWalletCreateOpenLifecycle verifies the create/open contract: a
// seed creates the wallet database under the supplied passphrase, and
// subsequent opens need only the passphrase.
func TestWalletCreateOpenLifecycle(t *testing.T) {
	t.Parallel()

	esplora := newTestEsplora(t)
	dbDir := t.TempDir()

	var seed [32]byte
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	password := []byte("lifecycle-password")

	// Opening before any wallet exists must fail loudly rather than
	// silently creating a wallet with a random seed.
	_, err := New(testWalletConfig(esplora.URL, dbDir, nil, password))
	require.ErrorIs(t, err, ErrWalletNotFound)

	// Create the wallet from the seed.
	exists, err := WalletExists(
		testWalletConfig(esplora.URL, dbDir, nil, password),
	)
	require.NoError(t, err)
	require.False(t, exists)

	w, err := New(testWalletConfig(esplora.URL, dbDir, seed[:], password))
	require.NoError(t, err)
	require.NoError(t, w.Start())

	// Derive a fixed key as a wallet identity fingerprint. DeriveKey
	// (unlike NewAddress) is deterministic for a fixed locator, so it
	// proves the reopened wallet is backed by the same seed.
	identityLoc := keychain.KeyLocator{
		Family: keychain.KeyFamilyNodeKey,
		Index:  0,
	}
	firstKey, err := w.DeriveKey(t.Context(), identityLoc)
	require.NoError(t, err)

	w.Stop()

	exists, err = WalletExists(
		testWalletConfig(esplora.URL, dbDir, nil, password),
	)
	require.NoError(t, err)
	require.True(t, exists)

	// Re-creating over an existing wallet database must be refused:
	// btcwallet would silently ignore the new seed and open the old
	// wallet.
	_, err = New(testWalletConfig(esplora.URL, dbDir, seed[:], password))
	require.ErrorIs(t, err, ErrWalletExists)

	// Reopen with the passphrase only and confirm it is the same
	// wallet by deriving the same address chain.
	w, err = New(testWalletConfig(esplora.URL, dbDir, nil, password))
	require.NoError(t, err)
	require.NoError(t, w.Start())
	t.Cleanup(w.Stop)

	reopenedKey, err := w.DeriveKey(t.Context(), identityLoc)
	require.NoError(t, err)
	require.Equal(
		t, firstKey.PubKey.SerializeCompressed(),
		reopenedKey.PubKey.SerializeCompressed(),
	)
}

// TestWalletWrongPasswordRetry verifies that a failed unlock (wrong
// private passphrase) closes the wallet database again so an immediate
// retry with the correct password succeeds instead of deadlocking on
// the database's exclusive lock.
func TestWalletWrongPasswordRetry(t *testing.T) {
	t.Parallel()

	esplora := newTestEsplora(t)
	dbDir := t.TempDir()

	var seed [32]byte
	for i := range seed {
		seed[i] = byte(i + 2)
	}
	password := []byte("correct-password")

	w, err := New(testWalletConfig(esplora.URL, dbDir, seed[:], password))
	require.NoError(t, err)
	require.NoError(t, w.Start())
	w.Stop()

	// A wrong passphrase opens the database in New (the public
	// passphrase is static) and fails at Start when btcwallet
	// unlocks the private key material.
	w, err = New(
		testWalletConfig(
			esplora.URL, dbDir, nil, []byte("wrong-password"),
		),
	)
	require.NoError(t, err)
	err = w.Start()
	require.ErrorContains(t, err, "invalid passphrase")

	// The failed Start must have unwound cleanly: retrying with the
	// correct password would block on the bbolt file lock if the
	// database were still open.
	w, err = New(testWalletConfig(esplora.URL, dbDir, nil, password))
	require.NoError(t, err)
	require.NoError(t, w.Start())
	t.Cleanup(w.Stop)
}
