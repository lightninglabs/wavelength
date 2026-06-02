package esplorarpc

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/chainbackends"
	"github.com/stretchr/testify/require"
)

// Compile-time assertion that the Esplora submitter satisfies the shared
// chainbackends.PackageSubmitter interface so it drops into the lnd and
// neutrino chain backends without adapter glue.
var _ chainbackends.PackageSubmitter = (*PackageSubmitter)(nil)

// zeroHash is a valid 64-hex-character txid/wtxid placeholder for the
// per-tx result map in canned Esplora responses.
const zeroHash = "0000000000000000000000000000000000000000000000000000" +
	"000000000000"

// newTestTx builds a minimal v3 transaction. The lockTime is used to make
// the parent and child serialize to distinct hex so ordering can be
// asserted from the request body.
func newTestTx(lockTime uint32) *wire.MsgTx {
	tx := wire.NewMsgTx(3)
	tx.LockTime = lockTime
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 0},
		Sequence:         wire.MaxTxInSequenceNum,
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    1000,
		PkScript: []byte{0x51},
	})

	return tx
}

// txHex returns the hex-encoded wire serialization of tx.
func txHex(t *testing.T, tx *wire.MsgTx) string {
	t.Helper()

	var buf bytes.Buffer
	require.NoError(t, tx.Serialize(&buf))

	return hex.EncodeToString(buf.Bytes())
}

// TestSubmitPackageSuccess verifies that parents and child are POSTed to
// /txs/package in dependency order and a success result is returned.
func TestSubmitPackageSuccess(t *testing.T) {
	parent := newTestTx(1)
	child := newTestTx(2)
	wantHexes := []string{txHex(t, parent), txHex(t, child)}

	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, http.MethodPost, r.Method)
			require.Equal(t, packagePath, r.URL.Path)
			require.Equal(
				t, "application/json",
				r.Header.Get("Content-Type"),
			)

			var got []string
			require.NoError(
				t, json.NewDecoder(r.Body).Decode(&got),
			)
			require.Equal(t, wantHexes, got)

			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"package_msg":"success",`+
				`"tx-results":{"`+zeroHash+`":{"txid":"`+
				zeroHash+`"}}}`)
		},
	))
	defer srv.Close()

	submitter, err := New(srv.URL)
	require.NoError(t, err)

	result, err := submitter.SubmitPackage(
		t.Context(), []*wire.MsgTx{parent}, child, nil,
	)
	require.NoError(t, err)
	require.Equal(t, "success", result.PackageMsg)
	require.Len(t, result.TxResults, 1)
}

// TestSubmitPackageRejection verifies that a package-relay rejection is
// returned as a populated result (not a Go error): the caller's backend
// classifies PackageMsg / per-tx errors, exactly as for bitcoindrpc.
func TestSubmitPackageRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w,
				`{"package_msg":"transaction failed",`+
					`"tx-results":{"`+zeroHash+
					`":{"txid":"`+zeroHash+
					`","error":"min relay fee not met"}}}`)
		},
	))
	defer srv.Close()

	submitter, err := New(srv.URL)
	require.NoError(t, err)

	result, err := submitter.SubmitPackage(
		t.Context(), []*wire.MsgTx{newTestTx(1)}, newTestTx(2), nil,
	)
	require.NoError(t, err)
	require.NotEqual(t, "success", result.PackageMsg)

	txResult, ok := result.TxResults[zeroHash]
	require.True(t, ok)
	require.NotNil(t, txResult.Error)
	require.Equal(t, "min relay fee not met", *txResult.Error)
}

// TestSubmitPackageHTTPError verifies that a non-2xx response carrying a
// non-JSON body surfaces an HTTP-level error rather than a decode error.
func TestSubmitPackageHTTPError(t *testing.T) {
	srv := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, "bad request")
			},
		),
	)
	defer srv.Close()

	submitter, err := New(srv.URL)
	require.NoError(t, err)

	_, err = submitter.SubmitPackage(
		t.Context(), []*wire.MsgTx{newTestTx(1)}, newTestTx(2), nil,
	)
	require.ErrorContains(t, err, "HTTP 400")
}

// TestSubmitPackageNilChild verifies the child is required.
func TestSubmitPackageNilChild(t *testing.T) {
	submitter, err := New("https://example.com/api")
	require.NoError(t, err)

	_, err = submitter.SubmitPackage(t.Context(), nil, nil, nil)
	require.ErrorContains(t, err, "child")
}

// TestNewBaseURLNormalization covers scheme defaulting, trailing-slash
// trimming, and rejection of empty / unsupported inputs.
func TestNewBaseURLNormalization(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr string
	}{{
		name:  "bare host defaults to https",
		input: "blockstream.info/api",
		want:  "https://blockstream.info/api",
	}, {
		name:  "trailing slash trimmed",
		input: "https://mempool.space/api/",
		want:  "https://mempool.space/api",
	}, {
		name:  "explicit http preserved",
		input: "http://127.0.0.1:3002",
		want:  "http://127.0.0.1:3002",
	}, {
		name:    "empty rejected",
		input:   "  ",
		wantErr: "required",
	}, {
		name:    "unsupported scheme rejected",
		input:   "ftp://example.com",
		wantErr: "unsupported",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			submitter, err := New(tc.input)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.want, submitter.url)
		})
	}
}
