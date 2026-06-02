package bitcoindrpc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/wire"
)

// TestPublishTransactionCallsSendRawTransaction verifies that the direct
// broadcaster path bypasses lnd and calls bitcoind's sendrawtransaction RPC.
func TestPublishTransactionCallsSendRawTransaction(t *testing.T) {
	t.Parallel()

	tx := wire.NewMsgTx(2)
	tx.AddTxOut(&wire.TxOut{
		Value:    1000,
		PkScript: []byte{0x51},
	})

	txHex, err := txToHex(tx)
	if err != nil {
		t.Fatalf("serialize tx: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			user, pass, ok := r.BasicAuth()
			if !ok || user != "user" || pass != "pass" {
				t.Fatalf("unexpected basic auth: %v %q %q", ok,
					user, pass)
			}

			var req struct {
				Method string            `json:"method"`
				Params []json.RawMessage `json:"params"`
			}
			if err := json.NewDecoder(r.Body).Decode(
				&req,
			); err != nil {

				t.Fatalf("decode request: %v", err)
			}

			if req.Method != "sendrawtransaction" {
				t.Fatalf("unexpected method %q", req.Method)
			}
			if len(req.Params) != 1 {
				t.Fatalf("expected 1 param, got %d",
					len(req.Params))
			}

			var gotTxHex string
			if err := json.Unmarshal(
				req.Params[0], &gotTxHex,
			); err != nil {

				t.Fatalf("decode tx hex: %v", err)
			}
			if gotTxHex != txHex {
				t.Fatalf("unexpected tx hex %q", gotTxHex)
			}

			_, _ = w.Write([]byte(`{"result":"txid","error":null}`))
		},
	))
	defer server.Close()

	submitter := New(
		strings.TrimPrefix(server.URL, "http://"),
		"user", "pass",
	)
	err = submitter.PublishTransaction(context.Background(), tx, "ignored")
	if err != nil {
		t.Fatalf("publish tx: %v", err)
	}
}

// TestPublishTransactionTreatsAlreadyKnownAsSuccess verifies retrying backing
// parent publication is idempotent once bitcoind already knows the tx.
func TestPublishTransactionTreatsAlreadyKnownAsSuccess(t *testing.T) {
	t.Parallel()

	tx := wire.NewMsgTx(2)
	tx.AddTxOut(&wire.TxOut{
		Value:    1000,
		PkScript: []byte{0x51},
	})

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{
				"result": null,
				"error": {
					"code": -27,
					"message": "transaction already exists in blockchain"
				}
			}`))
		},
	))
	defer server.Close()

	submitter := New(
		strings.TrimPrefix(server.URL, "http://"),
		"user", "pass",
	)
	err := submitter.PublishTransaction(context.Background(), tx, "ignored")
	if err != nil {
		t.Fatalf("publish already-known tx: %v", err)
	}
}
