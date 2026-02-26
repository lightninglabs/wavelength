package lwwallet

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/stretchr/testify/require"
)

// mockEsploraServer creates a test HTTP server that simulates
// an Esplora API. The handler is fully customizable via the
// provided handlerFn.
func mockEsploraServer(t *testing.T,
	handlerFn http.HandlerFunc) *httptest.Server {

	t.Helper()

	srv := httptest.NewServer(handlerFn)
	t.Cleanup(srv.Close)

	return srv
}

// TestChainBackendStartStop verifies that the chain backend can
// be started and stopped cleanly.
func TestChainBackendStartStop(t *testing.T) {
	t.Parallel()

	srv := mockEsploraServer(
		t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/blocks/tip/height":
				fmt.Fprint(w, "100")

			case "/block-height/100":
				h := chainhash.HashH([]byte("test"))
				fmt.Fprint(w, h.String())

			default:
				http.Error(
					w, "not found",
					http.StatusNotFound,
				)
			}
		},
	)

	esplora := NewEsploraClient(srv.URL, btclog.Disabled)
	backend := NewChainBackend(esplora, 50*time.Millisecond, btclog.Disabled)

	err := backend.Start()
	require.NoError(t, err)

	// Verify BestBlock returns the initial tip.
	height, hash, err := backend.BestBlock(t.Context())
	require.NoError(t, err)
	require.Equal(t, int32(100), height)
	require.NotEqual(t, chainhash.Hash{}, hash)

	err = backend.Stop()
	require.NoError(t, err)
}

// TestChainBackendBlockNotification verifies that new blocks
// trigger notifications to registered block subscribers.
func TestChainBackendBlockNotification(t *testing.T) {
	t.Parallel()

	var (
		mu          sync.Mutex
		tipHeight   int32 = 100
		blockHashes       = make(map[int32]chainhash.Hash)
	)

	// Pre-generate block hashes.
	for h := int32(100); h <= 103; h++ {
		blockHashes[h] = chainhash.HashH(
			[]byte(fmt.Sprintf("block-%d", h)),
		)
	}

	srv := mockEsploraServer(
		t, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			currentHeight := tipHeight
			mu.Unlock()

			switch r.URL.Path {
			case "/blocks/tip/height":
				fmt.Fprintf(w, "%d", currentHeight)

			case "/blocks/tip/hash":
				mu.Lock()
				h := blockHashes[tipHeight]
				mu.Unlock()
				fmt.Fprint(w, h.String())

			default:
				handleBlockReqs(
					t, w, r, &mu, blockHashes,
				)
			}
		},
	)

	esplora := NewEsploraClient(srv.URL, btclog.Disabled)
	backend := NewChainBackend(esplora, 50*time.Millisecond, btclog.Disabled)

	err := backend.Start()
	require.NoError(t, err)
	defer func() {
		require.NoError(t, backend.Stop())
	}()

	// Register for blocks.
	blockReg, err := backend.RegisterBlocks(t.Context())
	require.NoError(t, err)
	defer blockReg.Cancel()

	// Advance the tip.
	mu.Lock()
	tipHeight = 101
	mu.Unlock()

	// Wait for the block notification.
	select {
	case epoch := <-blockReg.Epochs:
		require.Equal(t, int32(101), epoch.Height)
		require.Equal(t, blockHashes[101], epoch.Hash)

	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for block notification")
	}
}

// handleBlockReqs handles /block-height/:height and /block/:hash
// requests in the mock Esplora server.
func handleBlockReqs(t *testing.T, w http.ResponseWriter,
	r *http.Request, mu *sync.Mutex,
	blockHashes map[int32]chainhash.Hash) {

	t.Helper()

	// Handle /block-height/:height.
	var height int32
	if _, err := fmt.Sscanf(
		r.URL.Path, "/block-height/%d", &height,
	); err == nil {
		mu.Lock()
		h, ok := blockHashes[height]
		mu.Unlock()

		if ok {
			fmt.Fprint(w, h.String())
		} else {
			http.Error(
				w, "not found",
				http.StatusNotFound,
			)
		}

		return
	}

	// Handle /block/:hash (block header).
	for _, h := range blockHashes {
		hashStr := h.String()
		path := "/block/" + hashStr
		if r.URL.Path == path {
			resp := esploraBlock{
				ID:        hashStr,
				Height:    100,
				Timestamp: 1700000000,
			}

			err := json.NewEncoder(w).Encode(resp)
			require.NoError(t, err)

			return
		}
	}

	http.Error(w, "not found", http.StatusNotFound)
}

// TestChainBackendEstimateFee verifies fee estimation from
// Esplora data.
func TestChainBackendEstimateFee(t *testing.T) {
	t.Parallel()

	srv := mockEsploraServer(
		t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/blocks/tip/height":
				fmt.Fprint(w, "100")

			case "/block-height/100":
				h := chainhash.HashH([]byte("test"))
				fmt.Fprint(w, h.String())

			case "/fee-estimates":
				estimates := map[string]float64{
					"1":  25.5,
					"3":  15.2,
					"6":  10.1,
					"25": 5.3,
				}

				err := json.NewEncoder(w).Encode(
					estimates,
				)
				require.NoError(t, err)

			default:
				http.Error(
					w, "not found",
					http.StatusNotFound,
				)
			}
		},
	)

	esplora := NewEsploraClient(srv.URL, btclog.Disabled)
	backend := NewChainBackend(esplora, time.Hour, btclog.Disabled)

	err := backend.Start()
	require.NoError(t, err)
	defer func() {
		require.NoError(t, backend.Stop())
	}()

	// Request fee for 3-block target.
	fee, err := backend.EstimateFee(t.Context(), 3)
	require.NoError(t, err)

	// Should pick the "3" bucket: 15.2 -> ceil -> 16.
	require.Equal(t, int64(16), int64(fee))

	// Request fee for 10-block target (should pick "25" bucket
	// as closest >= target).
	fee, err = backend.EstimateFee(t.Context(), 10)
	require.NoError(t, err)

	// Should pick "25" bucket: 5.3 -> ceil -> 6.
	require.Equal(t, int64(6), int64(fee))
}

// TestChainBackendTestMempoolAccept verifies that
// TestMempoolAccept returns an unsupported error.
func TestChainBackendTestMempoolAccept(t *testing.T) {
	t.Parallel()

	esplora := NewEsploraClient("http://unused", btclog.Disabled)
	backend := NewChainBackend(esplora, time.Hour, btclog.Disabled)

	ok, reason, err := backend.TestMempoolAccept(
		t.Context(), nil,
	)
	require.Error(t, err)
	require.False(t, ok)
	require.Empty(t, reason)
	require.Contains(t, err.Error(), "not supported")
}

// TestChainBackendConfRegistration verifies that confirmation
// registrations fire when a transaction reaches the target
// confirmation count.
func TestChainBackendConfRegistration(t *testing.T) {
	t.Parallel()

	txid := chainhash.HashH([]byte("test-tx"))

	srv := mockEsploraServer(
		t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/blocks/tip/height":
				fmt.Fprint(w, "105")

			case "/block-height/105":
				h := chainhash.HashH(
					[]byte("block-105"),
				)
				fmt.Fprint(w, h.String())

			case "/tx/" + txid.String() + "/status":
				status := esploraTxStatus{
					Confirmed:   true,
					BlockHeight: 100,
					BlockHash: chainhash.HashH(
						[]byte("block-100"),
					).String(),
				}

				err := json.NewEncoder(w).Encode(
					status,
				)
				require.NoError(t, err)

			case "/tx/" + txid.String() + "/raw":
				_, err := w.Write(minimalRawTx())
				require.NoError(t, err)

			default:
				http.Error(
					w, "not found",
					http.StatusNotFound,
				)
			}
		},
	)

	esplora := NewEsploraClient(srv.URL, btclog.Disabled)
	backend := NewChainBackend(esplora, 50*time.Millisecond, btclog.Disabled)

	err := backend.Start()
	require.NoError(t, err)
	defer func() {
		require.NoError(t, backend.Stop())
	}()

	// Register for 3 confirmations. Tx is at height 100, tip
	// is 105, so it has 6 confirmations - should fire
	// immediately.
	confReg, err := backend.RegisterConf(
		t.Context(), &txid, nil, 3, 99, false,
	)
	require.NoError(t, err)
	defer confReg.Cancel()

	select {
	case conf := <-confReg.Confirmed:
		require.NotNil(t, conf)
		require.Equal(t, uint32(100), conf.BlockHeight)

	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for confirmation")
	}
}

// TestChainBackendSpendRegistration verifies that spend
// registrations fire when an output is spent.
func TestChainBackendSpendRegistration(t *testing.T) {
	t.Parallel()

	txid := chainhash.HashH([]byte("funding-tx"))
	spenderTxid := chainhash.HashH([]byte("spending-tx"))

	srv := mockEsploraServer(
		t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/blocks/tip/height":
				fmt.Fprint(w, "100")

			case "/block-height/100":
				h := chainhash.HashH(
					[]byte("block-100"),
				)
				fmt.Fprint(w, h.String())

			case "/tx/" + txid.String() + "/outspend/0":
				outspend := esploraOutspend{
					Spent: true,
					Txid:  spenderTxid.String(),
					Vin:   1,
					Status: esploraStatus{
						Confirmed:   true,
						BlockHeight: 99,
					},
				}

				err := json.NewEncoder(w).Encode(
					outspend,
				)
				require.NoError(t, err)

			case "/tx/" + spenderTxid.String() + "/raw":
				_, err := w.Write(minimalRawTx())
				require.NoError(t, err)

			default:
				http.Error(
					w, "not found",
					http.StatusNotFound,
				)
			}
		},
	)

	esplora := NewEsploraClient(srv.URL, btclog.Disabled)
	backend := NewChainBackend(esplora, 50*time.Millisecond, btclog.Disabled)

	err := backend.Start()
	require.NoError(t, err)
	defer func() {
		require.NoError(t, backend.Stop())
	}()

	outpoint := &chainhash.Hash{}
	copy(outpoint[:], txid[:])

	spendReg, err := backend.RegisterSpend(
		t.Context(),
		&wire.OutPoint{Hash: txid, Index: 0},
		nil, 90,
	)
	require.NoError(t, err)
	defer spendReg.Cancel()

	select {
	case spend := <-spendReg.Spend:
		require.NotNil(t, spend)
		require.Equal(t, uint32(1), spend.SpenderInputIndex)
		require.Equal(t, int32(99), spend.SpendingHeight)

	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for spend notification")
	}
}

// minimalRawTx returns a minimal valid serialized Bitcoin
// transaction suitable for testing deserialization.
func minimalRawTx() []byte {
	// Version 2, 1 input (coinbase-like), 1 output.
	return []byte{
		// Version.
		0x02, 0x00, 0x00, 0x00,
		// Input count.
		0x01,
		// Previous output hash (32 bytes of zeros).
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		// Previous output index.
		0x00, 0x00, 0x00, 0x00,
		// Script length.
		0x00,
		// Sequence.
		0xff, 0xff, 0xff, 0xff,
		// Output count.
		0x01,
		// Value (50000 sats).
		0x50, 0xc3, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		// Script length + script (OP_TRUE).
		0x01, 0x51,
		// Locktime.
		0x00, 0x00, 0x00, 0x00,
	}
}
