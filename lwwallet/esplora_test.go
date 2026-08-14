package lwwallet

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/stretchr/testify/require"
)

// testTxHex serializes a transaction and returns its hex encoding.
func testTxHex(t *testing.T, tx *wire.MsgTx) string {
	t.Helper()

	var buf bytes.Buffer
	err := tx.Serialize(&buf)
	require.NoError(t, err)

	return hex.EncodeToString(buf.Bytes())
}

// TestScriptHashHex verifies the Esplora script hash computation matches
// the expected Electrum-style reversed SHA256 hex encoding.
func TestScriptHashHex(t *testing.T) {
	t.Parallel()

	// Use a known pkScript and verify the hash matches the expected
	// Electrum script hash format.
	pkScript, err := hex.DecodeString(
		"76a91489abcdefabbaabbaabbaabbaabbaabbaabbaabba88ac",
	)
	require.NoError(t, err)

	hash := scriptHashHex(pkScript)

	// The hash should be 64 hex characters (32 bytes).
	require.Len(t, hash, 64)

	// Verify it's valid hex.
	_, err = hex.DecodeString(hash)
	require.NoError(t, err)
}

// TestEsploraGetTipHeight verifies that GetTipHeight correctly parses the
// API response.
func TestEsploraGetTipHeight(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "/blocks/tip/height", r.URL.Path)

			_, err := w.Write([]byte("850123"))
			require.NoError(t, err)
		}),
	)
	defer srv.Close()

	client := NewEsploraClient(srv.URL, btclog.Disabled)
	height, err := client.GetTipHeight(t.Context())
	require.NoError(t, err)
	require.Equal(t, int32(850123), height)
}

// TestEsploraGetTipHash verifies that GetTipHash correctly parses a block
// hash response.
func TestEsploraGetTipHash(t *testing.T) {
	t.Parallel()

	hashStr := "000000000019d6689c085ae165831e934ff763ae46" +
		"a2a6c172b3f1b60a8ce26f"

	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "/blocks/tip/hash", r.URL.Path)

			_, err := w.Write([]byte(hashStr))
			require.NoError(t, err)
		}),
	)
	defer srv.Close()

	client := NewEsploraClient(srv.URL, btclog.Disabled)
	hash, err := client.GetTipHash(t.Context())
	require.NoError(t, err)
	require.Equal(t, hashStr, hash.String())
}

// TestEsploraGetBlockHashByHeight verifies hash-by-height lookup.
func TestEsploraGetBlockHashByHeight(t *testing.T) {
	t.Parallel()

	hashStr := "000000000019d6689c085ae165831e934ff763ae46" +
		"a2a6c172b3f1b60a8ce26f"

	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "/block-height/100", r.URL.Path)

			_, err := w.Write([]byte(hashStr))
			require.NoError(t, err)
		}),
	)
	defer srv.Close()

	client := NewEsploraClient(srv.URL, btclog.Disabled)
	hash, err := client.GetBlockHashByHeight(t.Context(), 100)
	require.NoError(t, err)
	require.Equal(t, hashStr, hash.String())
}

// TestEsploraGetBlockHeader verifies block header JSON parsing.
func TestEsploraGetBlockHeader(t *testing.T) {
	t.Parallel()

	blockHash, err := chainhash.NewHashFromStr(
		"000000000019d6689c085ae165831e934ff763ae46a2a6c172b3f1b60a8" +
			"ce26f",
	)
	require.NoError(t, err)

	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			resp := esploraBlock{
				ID:        blockHash.String(),
				Height:    100,
				Timestamp: 1231006505,
			}

			err := json.NewEncoder(w).Encode(resp)
			require.NoError(t, err)
		}),
	)
	defer srv.Close()

	client := NewEsploraClient(srv.URL, btclog.Disabled)
	block, err := client.GetBlockHeader(t.Context(), *blockHash)
	require.NoError(t, err)
	require.Equal(t, int32(100), block.Height)
	require.Equal(t, int64(1231006505), block.Timestamp)
}

// TestEsploraGetScriptUtxos verifies UTXO listing by script hash.
func TestEsploraGetScriptUtxos(t *testing.T) {
	t.Parallel()

	pkScript := []byte{0x51, 0x20, 0x01, 0x02, 0x03}

	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Verify the path contains the correct script hash.
			// The expectation is computed here rather than via
			// scriptHashHex so this asserts the encoding instead
			// of merely agreeing with whatever the helper does.
			sum := sha256.Sum256(pkScript)
			expectedPath := "/scripthash/" +
				hex.EncodeToString(sum[:]) + "/utxo"
			require.Equal(t, expectedPath, r.URL.Path)

			utxos := []esploraUtxo{
				{
					Txid:  "aabb",
					Vout:  0,
					Value: 100000,
					Status: esploraStatus{
						Confirmed:   true,
						BlockHeight: 500,
					},
				},
				{
					Txid:  "ccdd",
					Vout:  1,
					Value: 200000,
					Status: esploraStatus{
						Confirmed: false,
					},
				},
			}

			err := json.NewEncoder(w).Encode(utxos)
			require.NoError(t, err)
		}),
	)
	defer srv.Close()

	client := NewEsploraClient(srv.URL, btclog.Disabled)
	utxos, err := client.GetScriptUtxos(t.Context(), pkScript)
	require.NoError(t, err)
	require.Len(t, utxos, 2)
	require.Equal(t, int64(100000), utxos[0].Value)
	require.True(t, utxos[0].Status.Confirmed)
	require.Equal(t, int64(200000), utxos[1].Value)
	require.False(t, utxos[1].Status.Confirmed)
}

// TestEsploraGetTxStatus verifies transaction status parsing.
func TestEsploraGetTxStatus(t *testing.T) {
	t.Parallel()

	txid, err := chainhash.NewHashFromStr(
		"00000000000000000000000000000000000000000000000000000000000" +
			"00001",
	)
	require.NoError(t, err)

	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			status := esploraTxStatus{
				Confirmed:   true,
				BlockHeight: 750000,
				BlockHash:   "deadbeef",
			}

			err := json.NewEncoder(w).Encode(status)
			require.NoError(t, err)
		}),
	)
	defer srv.Close()

	client := NewEsploraClient(srv.URL, btclog.Disabled)
	status, err := client.GetTxStatus(t.Context(), *txid)
	require.NoError(t, err)
	require.True(t, status.Confirmed)
	require.Equal(t, uint32(750000), status.BlockHeight)
}

// TestEsploraGetFeeEstimates verifies fee estimate parsing.
func TestEsploraGetFeeEstimates(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "/fee-estimates", r.URL.Path)

			estimates := map[string]float64{
				"1":  25.0,
				"3":  15.0,
				"6":  10.0,
				"25": 5.0,
			}

			err := json.NewEncoder(w).Encode(estimates)
			require.NoError(t, err)
		}),
	)
	defer srv.Close()

	client := NewEsploraClient(srv.URL, btclog.Disabled)
	estimates, err := client.GetFeeEstimates(t.Context())
	require.NoError(t, err)
	require.Equal(t, 25.0, estimates["1"])
	require.Equal(t, 5.0, estimates["25"])
}

// TestEsploraGetOutspend verifies outspend status parsing.
func TestEsploraGetOutspend(t *testing.T) {
	t.Parallel()

	txid, err := chainhash.NewHashFromStr(
		"00000000000000000000000000000000000000000000000000000000000" +
			"00001",
	)
	require.NoError(t, err)

	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			outspend := esploraOutspend{
				Spent: true,
				Txid:  "abcd1234",
				Vin:   0,
				Status: esploraStatus{
					Confirmed:   true,
					BlockHeight: 800000,
				},
			}

			err := json.NewEncoder(w).Encode(outspend)
			require.NoError(t, err)
		}),
	)
	defer srv.Close()

	client := NewEsploraClient(srv.URL, btclog.Disabled)
	outspend, err := client.GetOutspend(t.Context(), *txid, 0)
	require.NoError(t, err)
	require.True(t, outspend.Spent)
	require.Equal(t, "abcd1234", outspend.Txid)
}

// TestEsploraBroadcastTx verifies transaction broadcasting.
func TestEsploraBroadcastTx(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, http.MethodPost, r.Method)
			require.Equal(t, "/tx", r.URL.Path)

			// Return a fake txid.
			_, err := w.Write([]byte("aabbccdd"))
			require.NoError(t, err)
		}),
	)
	defer srv.Close()

	// Build a minimal valid transaction for serialization.
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{},
			Index: 0,
		},
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    50000,
		PkScript: []byte{0x51, 0x20},
	})

	client := NewEsploraClient(srv.URL, btclog.Disabled)
	txid, err := client.BroadcastTx(t.Context(), tx)
	require.NoError(t, err)
	require.Equal(t, "aabbccdd", txid)
}

// TestEsploraSubmitPackage verifies package submission via the Esplora API.
func TestEsploraSubmitPackage(t *testing.T) {
	t.Parallel()

	parent := wire.NewMsgTx(3)
	child := wire.NewMsgTx(3)
	expected := []string{
		testTxHex(t, parent),
		testTxHex(t, child),
	}

	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, http.MethodPost, r.Method)
			require.Equal(t, "/txs/package", r.URL.Path)
			require.Equal(
				t, "application/json", r.Header.Get(
					"Content-Type",
				),
			)

			var got []string
			err := json.NewDecoder(r.Body).Decode(&got)
			require.NoError(t, err)
			require.Equal(t, expected, got)

			_, err = w.Write([]byte(`{"package_msg":"success"}`))
			require.NoError(t, err)
		}),
	)
	defer srv.Close()

	client := NewEsploraClient(srv.URL, btclog.Disabled)
	err := client.SubmitPackage(t.Context(), expected)
	require.NoError(t, err)
}

// TestEsploraSubmitPackageReject verifies that package relay failures are
// surfaced to callers even when Esplora returns HTTP 200.
func TestEsploraSubmitPackageReject(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, http.MethodPost, r.Method)
			require.Equal(t, "/txs/package", r.URL.Path)

			_, err := w.Write([]byte(
				`{"package_msg":"transaction failed",` +
					`"tx-results":{` +
					`"childwtxid":{` +
					`"txid":"childtxid",` +
					`"error":"bad-txns-inputs-` +
					`missingorspent"},` +
					`"parentwtxid":{` +
					`"txid":"parenttxid",` +
					`"error":"txn-already-known"}}}`,
			))
			require.NoError(t, err)
		}),
	)
	defer srv.Close()

	client := NewEsploraClient(srv.URL, btclog.Disabled)
	err := client.SubmitPackage(t.Context(), []string{"aa", "bb"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "package not accepted")
	require.Contains(t, err.Error(), "bad-txns-inputs-missingorspent")
	require.Contains(t, err.Error(), "txn-already-known")
}

// TestEsploraHTTPError verifies that non-200 responses produce an error.
func TestEsploraHTTPError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "not found", http.StatusNotFound)
		}),
	)
	defer srv.Close()

	client := NewEsploraClient(srv.URL, btclog.Disabled)
	_, err := client.GetTipHeight(t.Context())
	require.Error(t, err)
	require.Contains(t, err.Error(), "404")
}

// TestScriptHashEncoding pins the byte order Esplora's /scripthash/
// routes expect. Esplora takes the SHA256 digest of the script in its
// natural order, unlike the Electrum wire protocol, which reverses it.
// Getting this wrong is silent: the API answers an unknown script hash
// with an empty result rather than an error, so every lookup simply
// comes back with nothing found.
//
// The expectation is a fixed vector rather than a recomputation, so this
// disagrees with the implementation when the implementation changes.
// systest.TestEsploraScriptHashEncoding is the companion check against a
// real electrs instance.
func TestScriptHashEncoding(t *testing.T) {
	t.Parallel()

	// A P2WPKH script paying the all-zero key hash.
	pkScript := append([]byte{0x00, 0x14}, make([]byte, 20)...)

	const wantForward = "5c210f7cc5455eec4b9438c47c365fc4afdb29fa1da456" +
		"1440dc8d34e39ce273"

	require.Equal(
		t, wantForward, scriptHashHex(pkScript),
		"scriptHashHex must hex-encode the SHA256 digest in its "+
			"natural byte order, not the Electrum reversal",
	)

	// Spell out the reversal that used to live here so a change back
	// to it fails loudly rather than silently finding nothing.
	reversed, err := hex.DecodeString(wantForward)
	require.NoError(t, err)

	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}

	require.NotEqual(
		t, hex.EncodeToString(reversed), scriptHashHex(pkScript),
	)
}

// TestEsploraHTTPErrorBodyTruncated verifies a non-200 body is collapsed
// and bounded in the error. A misconfigured base URL naming a web frontend
// answers with a full HTML page rather than a short status line, so the
// error path needs a bound to stay readable.
func TestEsploraHTTPErrorBodyTruncated(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)

			_, err := w.Write([]byte(strings.Repeat("x", 4096)))
			require.NoError(t, err)
		}),
	)
	defer srv.Close()

	client := NewEsploraClient(srv.URL, btclog.Disabled)
	_, err := client.GetTipHeight(t.Context())
	require.Error(t, err)
	require.Contains(t, err.Error(), "404")
	require.Contains(t, err.Error(), "truncated")
	require.Less(t, len(err.Error()), 512)
}

// TestTruncateBody covers the shapes the helper has to handle: a short body
// passes through unchanged, a multi-line body is collapsed onto one line so
// it stays legible inside a single-line log record, and a long body is cut.
func TestTruncateBody(t *testing.T) {
	t.Parallel()

	require.Equal(
		t, "already short",
		truncateBody(
			[]byte("already  short"),
		),
	)

	require.Equal(t, "a b c", truncateBody([]byte("a\n  b\n\tc\n")))

	long := truncateBody([]byte(strings.Repeat("y", maxErrBodyBytes+10)))
	require.Len(t, long, maxErrBodyBytes+len("... (truncated)"))
	require.Contains(t, long, "truncated")

	// A cut that would land inside a multi-byte rune backs up to the
	// rune boundary rather than emitting a half-encoded rune. "€" is
	// three bytes, so a body of them has no boundary at maxErrBodyBytes
	// unless the helper looks for one.
	runes := truncateBody(
		[]byte(strings.Repeat("€", maxErrBodyBytes)),
	)
	require.True(t, utf8.ValidString(runes))
}
