package waved

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	btcwalletbase "github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightninglabs/wavelength/lwwallet"
	"github.com/lightninglabs/wavelength/wallet"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

// TestLwUnrollWalletListUnspentExcludesImported is a regression test for
// wavelength#831. On the lwwallet (esplora) backend, CPFP fee selection
// must only surface wallet-derived, key-spendable outputs. Imported,
// watch-only taproot outputs (boarding outputs imported via
// ImportTaprootScript) have no BIP32 derivation and no spendable key, so the
// FinalizePsbt path cannot sign them and finalization fails with "PSBT is not
// finalizable", forcing a zero-fee parent broadcast that does not relay.
//
// The default-account ListUnspentWitness filter already excludes these
// imported outputs. The bug was an all-accounts fallback that re-admitted
// them; this test pins the corrected behaviour: the wallet's own derived
// output is offered as a fee input, while an imported boarding output is not.
func TestLwUnrollWalletListUnspentExcludesImported(t *testing.T) {
	t.Parallel()

	w := newFundedLwWallet(t)
	ctx := t.Context()
	uw := &lwUnrollWallet{Wallet: w}

	// Import a watch-only boarding output and fund it as the wallet's only
	// confirmed UTXO. This is the field scenario: the default account is
	// empty (the boarding output lives in the imported account), so the
	// regression was an all-accounts fallback that surfaced this
	// unspendable output as a CPFP fee input.
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	importedAddr, err := w.BoardingBackend().ImportTaprootScript(
		ctx, &waddrmgr.Tapscript{
			Type:          waddrmgr.TaprootFullKeyOnly,
			FullOutputKey: privKey.PubKey(),
		},
	)
	require.NoError(t, err)
	importedScript, err := txscript.PayToAddrScript(importedAddr)
	require.NoError(t, err)
	importedOp := fundConfirmedUTXO(t, w, importedScript)

	// Sanity check the harness: an all-accounts enumeration must see the
	// imported output, otherwise the exclusion assertion below would be
	// vacuous.
	allUtxos, err := w.BtcWallet.ListUnspentWitness(0, math.MaxInt32, "")
	require.NoError(t, err)
	require.True(
		t, containsOutpoint(allUtxos, importedOp),
		"harness must fund the imported boarding output",
	)

	// With only the imported boarding output present, fee selection must
	// return nothing rather than offering the unspendable output.
	feeInputs, err := uw.ListUnspent(ctx, 0, math.MaxInt32)
	require.NoError(t, err)
	require.False(
		t, containsFeeInput(feeInputs, importedOp),
		"imported boarding output must not be a fee input",
	)
	require.Empty(t, feeInputs, "no spendable fee inputs available")

	// A wallet-derived taproot output (the kind lwwallet hands back for
	// change and sweep outputs) must, by contrast, be offered as a fee
	// input.
	derivedAddr, err := w.NewAddress(ctx)
	require.NoError(t, err)
	derivedScript, err := txscript.PayToAddrScript(derivedAddr)
	require.NoError(t, err)
	derivedOp := fundConfirmedUTXO(t, w, derivedScript)

	feeInputs, err = uw.ListUnspent(ctx, 0, math.MaxInt32)
	require.NoError(t, err)
	require.True(
		t, containsFeeInput(feeInputs, derivedOp),
		"derived output must be a valid fee input",
	)
	require.False(
		t, containsFeeInput(feeInputs, importedOp),
		"imported boarding output must remain excluded",
	)
}

// newFundedLwWallet builds and starts a real lwwallet backed by a mock
// Esplora server pinned at the regtest genesis block.
func newFundedLwWallet(t *testing.T) *lwwallet.Wallet {
	t.Helper()

	var seed [32]byte
	for i := range seed {
		seed[i] = byte(i + 1)
	}

	tipHash := chaincfg.RegressionNetParams.GenesisHash.String()
	handler := func(rw http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/blocks/tip/height":
			_, err := rw.Write([]byte("0"))
			require.NoError(t, err)

		case "/block-height/0":
			_, err := rw.Write([]byte(tipHash))
			require.NoError(t, err)

		case "/block/" + tipHash:
			err := json.NewEncoder(rw).Encode(map[string]any{
				"id":        tipHash,
				"height":    0,
				"timestamp": 1,
			})
			require.NoError(t, err)

		default:
			http.NotFound(rw, r)
		}
	}
	esplora := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(esplora.Close)

	w, err := lwwallet.New(lwwallet.Config{
		Seed:           seed[:],
		WalletPassword: []byte("test-password"),
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

	return w
}

// fundConfirmedUTXO records a confirmed credit for the given pkScript directly
// in btcwallet's transaction store and returns its outpoint. This mirrors
// btcwallet's own addUtxo test helper: it inserts a transaction paying the
// script and marks its sole output as a credit at the genesis block.
func fundConfirmedUTXO(t *testing.T, w *lwwallet.Wallet,
	pkScript []byte) wire.OutPoint {

	t.Helper()

	const amount = 1_000_000
	tx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{},
		},
		TxOut: []*wire.TxOut{
			wire.NewTxOut(amount, pkScript),
		},
	}

	internal, ok := w.BtcWallet.InternalWallet().(*btcwalletbase.Wallet)
	require.True(t, ok, "unexpected internal wallet type")

	var buf bytes.Buffer
	require.NoError(t, tx.Serialize(&buf))
	rec, err := wtxmgr.NewTxRecord(buf.Bytes(), time.Now())
	require.NoError(t, err)

	block := &wtxmgr.BlockMeta{
		Block: wtxmgr.Block{
			Hash:   *chaincfg.RegressionNetParams.GenesisHash,
			Height: 0,
		},
		Time: time.Unix(1, 0),
	}

	require.NoError(
		t,
		walletdb.Update(
			internal.Database(),
			func(dbtx walletdb.ReadWriteTx) error {
				ns := dbtx.ReadWriteBucket([]byte("wtxmgr"))
				if err := internal.TxStore.InsertTx(
					ns, rec, block,
				); err != nil {
					return err
				}

				return internal.TxStore.AddCredit(
					ns, rec, block, 0, false,
				)
			},
		),
	)

	return wire.OutPoint{Hash: tx.TxHash(), Index: 0}
}

// containsOutpoint reports whether the given lnwallet UTXOs include op.
func containsOutpoint(utxos []*lnwallet.Utxo, op wire.OutPoint) bool {
	for _, u := range utxos {
		if u.OutPoint == op {
			return true
		}
	}

	return false
}

// containsFeeInput reports whether the given fee inputs include op.
func containsFeeInput(utxos []*wallet.Utxo, op wire.OutPoint) bool {
	for _, u := range utxos {
		if u.Outpoint == op {
			return true
		}
	}

	return false
}
