package lwwallet

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestBoardingBackendDuplicateImportRestoresTracking verifies restart recovery
// for lwwallet's in-memory boarding-address filter. btcwallet persists the
// imported script, but the address filter starts empty after process restart,
// so duplicate imports must still repopulate it.
func TestBoardingBackendDuplicateImportRestoresTracking(t *testing.T) {
	t.Parallel()

	var seed [32]byte
	for i := range seed {
		seed[i] = byte(i + 1)
	}

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
	defer esplora.Close()

	w, err := New(Config{
		Seed:           seed[:],
		WalletPassword: []byte("test-wallet-password"),
		EsploraURL:     esplora.URL,
		ChainParams:    &chaincfg.RegressionNetParams,
		PollInterval:   time.Hour,
		RecoveryWindow: 10,
		DBDir:          t.TempDir(),
		Log:            fn.None[btclog.Logger](),
	})
	require.NoError(t, err)
	require.NoError(t, w.Start())
	t.Cleanup(w.Stop)

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	script := &waddrmgr.Tapscript{
		Type:          waddrmgr.TaprootFullKeyOnly,
		FullOutputKey: privKey.PubKey(),
	}

	backend := w.BoardingBackend()
	addr, err := backend.ImportTaprootScript(t.Context(), script)
	require.NoError(t, err)
	require.Contains(t, backend.SnapshotAddrs(), addr.String())

	backend.Mu.Lock()
	for key := range backend.ImportedAddrs {
		delete(backend.ImportedAddrs, key)
	}
	backend.Mu.Unlock()

	duplicateAddr, err := backend.ImportTaprootScript(t.Context(), script)
	require.NoError(t, err)
	require.Equal(t, addr.String(), duplicateAddr.String())
	require.Contains(t, backend.SnapshotAddrs(), addr.String())
}
