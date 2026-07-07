package wallet

import (
	"testing"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/txconfirm"
	"github.com/stretchr/testify/require"
)

// testWalletSweepDestPkScript returns a deterministic P2WKH destination script
// usable as the sweep output for preview tests.
func testWalletSweepDestPkScript(t *testing.T) txscript.PkScript {
	t.Helper()

	addr, err := btcaddr.NewAddressWitnessPubKeyHash(
		make([]byte, 20), &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	script, err := txscript.PayToAddrScript(addr)
	require.NoError(t, err)

	pkScript, err := txscript.ParsePkScript(script)
	require.NoError(t, err)

	return pkScript
}

// TestWalletSweepPreviewNoInputsCannotBroadcast asserts an empty UTXO set
// produces a preview that cannot broadcast and reports no confirmed inputs.
func TestWalletSweepPreviewNoInputsCannotBroadcast(t *testing.T) {
	t.Parallel()

	resp := walletSweepPreview(nil, testWalletSweepDestPkScript(t), 2)
	require.False(t, resp.CanBroadcast)
	require.Zero(t, resp.TotalInputSat)
	require.Contains(t, resp.FailureReason, "no confirmed")
}

// TestWalletSweepPreviewDustNetMessage asserts the preview refuses to broadcast
// when the net amount after fees does not clear the dust floor.
func TestWalletSweepPreviewDustNetMessage(t *testing.T) {
	t.Parallel()

	var hash [32]byte
	hash[0] = 2
	resp := walletSweepPreview([]*Utxo{{
		Outpoint: wire.OutPoint{
			Hash:  hash,
			Index: 0,
		},
		PkScript:      []byte{0x00, 0x14},
		Amount:        txconfirm.DustLimit + 10,
		Confirmations: 1,
	}}, testWalletSweepDestPkScript(t), 1)

	require.False(t, resp.CanBroadcast)
	require.Contains(t, resp.FailureReason, "dust")
}

// TestWalletSweepPreviewPositiveNetCanBroadcast asserts a comfortably-funded
// input set yields a broadcastable preview with consistent fee/net math.
func TestWalletSweepPreviewPositiveNetCanBroadcast(t *testing.T) {
	t.Parallel()

	var hash [32]byte
	hash[0] = 1
	resp := walletSweepPreview([]*Utxo{{
		Outpoint: wire.OutPoint{
			Hash:  hash,
			Index: 0,
		},
		PkScript:      []byte{0x00, 0x14},
		Amount:        btcutil.Amount(50_000),
		Confirmations: 1,
	}}, testWalletSweepDestPkScript(t), 2)

	require.True(t, resp.CanBroadcast, resp.FailureReason)
	require.Equal(t, int64(50_000), resp.TotalInputSat)
	require.Positive(t, resp.EstimatedFeeSat)
	require.Equal(
		t, resp.TotalInputSat-resp.EstimatedFeeSat, resp.NetAmountSat,
	)
	require.Len(t, resp.Inputs, 1)
}

// TestWalletSweepPreviewSkipsNilUtxos asserts nil entries in the UTXO slice
// are ignored without panicking and without inflating the input total.
func TestWalletSweepPreviewSkipsNilUtxos(t *testing.T) {
	t.Parallel()

	var hash [32]byte
	hash[0] = 3
	resp := walletSweepPreview([]*Utxo{nil, {
		Outpoint: wire.OutPoint{
			Hash:  hash,
			Index: 1,
		},
		PkScript:      []byte{0x00, 0x14},
		Amount:        btcutil.Amount(40_000),
		Confirmations: 6,
	}, nil}, testWalletSweepDestPkScript(t), 2)

	require.Len(t, resp.Inputs, 1)
	require.Equal(t, int64(40_000), resp.TotalInputSat)
}

// TestApplyWalletSweepFeeCapConfiguredMax asserts that when an operator max
// fee rate is configured, the cap clamps to it and leaves lower rates alone.
func TestApplyWalletSweepFeeCapConfiguredMax(t *testing.T) {
	t.Parallel()

	a := &Ark{walletSweepMaxFeeRate: 25}

	require.Equal(t, int64(24), a.applyWalletSweepFeeCap(24))
	require.Equal(t, int64(25), a.applyWalletSweepFeeCap(25))
	require.Equal(t, int64(25), a.applyWalletSweepFeeCap(250))
}

// TestApplyWalletSweepFeeCapDefaultMax asserts the H-2 fix: with no operator
// max configured the cap is NOT a no-op — it falls back to
// txconfirm.DefaultMaxFeeRateSatPerVByte and clamps a runaway rate to it.
func TestApplyWalletSweepFeeCapDefaultMax(t *testing.T) {
	t.Parallel()

	a := &Ark{walletSweepMaxFeeRate: 0}

	// Below the default ceiling, the rate passes through unchanged.
	require.Equal(t, int64(50), a.applyWalletSweepFeeCap(50))

	// At and above the default ceiling, the rate is clamped down — the
	// cap can never be skipped just because no operator max is set.
	require.Equal(
		t, txconfirm.DefaultMaxFeeRateSatPerVByte,
		a.applyWalletSweepFeeCap(
			txconfirm.DefaultMaxFeeRateSatPerVByte,
		),
	)
	require.Equal(
		t, txconfirm.DefaultMaxFeeRateSatPerVByte,
		a.applyWalletSweepFeeCap(
			txconfirm.DefaultMaxFeeRateSatPerVByte+1_000,
		),
	)
}

// TestWalletSweepDestScriptRejectsWrongNetwork asserts that a destination
// address minted for a different network is rejected by the network-validation
// guard rather than silently building a cross-network sweep.
func TestWalletSweepDestScriptRejectsWrongNetwork(t *testing.T) {
	t.Parallel()

	// The wallet is configured for regtest.
	a := &Ark{sweepChainParams: &chaincfg.RegressionNetParams}

	// Encode a mainnet address; decoding it against regtest must fail.
	mainnetAddr, err := btcaddr.NewAddressWitnessPubKeyHash(
		make([]byte, 20), &chaincfg.MainNetParams,
	)
	require.NoError(t, err)

	_, err = a.walletSweepDestScript(mainnetAddr.String())
	require.Error(t, err)
}

// TestWalletSweepDestScriptRejectsGarbage asserts a non-address string is
// rejected with a decode error.
func TestWalletSweepDestScriptRejectsGarbage(t *testing.T) {
	t.Parallel()

	a := &Ark{sweepChainParams: &chaincfg.RegressionNetParams}

	_, err := a.walletSweepDestScript("not-a-real-address")
	require.Error(t, err)
}

// TestWalletSweepDestScriptAcceptsCorrectNetwork asserts a regtest address
// decodes and produces a valid pkScript when the wallet is on regtest.
func TestWalletSweepDestScriptAcceptsCorrectNetwork(t *testing.T) {
	t.Parallel()

	a := &Ark{sweepChainParams: &chaincfg.RegressionNetParams}

	addr, err := btcaddr.NewAddressWitnessPubKeyHash(
		make([]byte, 20), &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	script, err := a.walletSweepDestScript(addr.String())
	require.NoError(t, err)
	require.NotEmpty(t, script.Script())
}

// TestBuildWalletSweepTxShape asserts the assembled unsigned sweep has one
// input per UTXO and a single destination output carrying the net amount.
func TestBuildWalletSweepTxShape(t *testing.T) {
	t.Parallel()

	destScript := testWalletSweepDestPkScript(t)

	var h1, h2 [32]byte
	h1[0] = 1
	h2[0] = 2
	utxos := []*Utxo{
		{
			Outpoint: wire.OutPoint{
				Hash:  h1,
				Index: 0,
			},
			Amount: btcutil.Amount(30_000),
		},
		nil,
		{
			Outpoint: wire.OutPoint{
				Hash:  h2,
				Index: 1,
			},
			Amount: btcutil.Amount(20_000),
		},
	}

	tx, err := buildWalletSweepTx(utxos, destScript, 45_000)
	require.NoError(t, err)
	require.Len(t, tx.TxIn, 2)
	require.Len(t, tx.TxOut, 1)
	require.Equal(t, int64(45_000), tx.TxOut[0].Value)
	require.Equal(t, walletSweepTxVersion, tx.Version)
}

// TestBuildWalletSweepTxRejectsDust asserts the builder fails closed when the
// net amount is at or below the dust floor.
func TestBuildWalletSweepTxRejectsDust(t *testing.T) {
	t.Parallel()

	destScript := testWalletSweepDestPkScript(t)
	_, err := buildWalletSweepTx(
		nil, destScript, int64(txconfirm.DustLimit),
	)
	require.Error(t, err)
}

// TestVerifyWalletSweepOutputsEqual asserts the post-sign guard accepts
// identical outputs and rejects value/script/count drift.
func TestVerifyWalletSweepOutputsEqual(t *testing.T) {
	t.Parallel()

	mk := func(value int64, script []byte) *wire.MsgTx {
		tx := wire.NewMsgTx(walletSweepTxVersion)
		tx.AddTxOut(&wire.TxOut{Value: value, PkScript: script})

		return tx
	}

	script := []byte{0x00, 0x14, 0x01}

	// Identical outputs pass.
	require.NoError(
		t,
		verifyWalletSweepOutputsEqual(
			mk(1_000, script), mk(1_000, script),
		),
	)

	// A changed value is rejected.
	require.Error(
		t,
		verifyWalletSweepOutputsEqual(
			mk(1_000, script), mk(900, script),
		),
	)

	// A changed script is rejected.
	require.Error(
		t,
		verifyWalletSweepOutputsEqual(
			mk(1_000, script),
			mk(
				1_000, []byte{0x00, 0x14, 0x02},
			),
		),
	)

	// A changed output count is rejected.
	extra := mk(1_000, script)
	extra.AddTxOut(&wire.TxOut{Value: 1, PkScript: script})
	require.Error(
		t,
		verifyWalletSweepOutputsEqual(
			mk(1_000, script), extra,
		),
	)

	// Nil transactions are rejected.
	require.Error(t, verifyWalletSweepOutputsEqual(nil, mk(1, script)))
}

// TestEstimateWalletSweepVSizeDispatchesScriptClass asserts the vsize estimator
// accounts for each input's witness class — a taproot-only set must estimate a
// different (smaller-witness) size than a legacy-P2PKH-only set.
func TestEstimateWalletSweepVSizeDispatchesScriptClass(t *testing.T) {
	t.Parallel()

	destScript := testWalletSweepDestPkScript(t)

	taproot := []*Utxo{{PkScript: taprootPkScript(t)}}
	legacy := []*Utxo{{PkScript: p2pkhPkScript(t)}}

	taprootSize := estimateWalletSweepVSize(taproot, destScript)
	legacySize := estimateWalletSweepVSize(legacy, destScript)

	require.Positive(t, taprootSize)
	require.Positive(t, legacySize)
	require.NotEqual(t, taprootSize, legacySize)
}

// taprootPkScript returns a deterministic P2TR script.
func taprootPkScript(t *testing.T) []byte {
	t.Helper()

	addr, err := btcaddr.NewAddressTaproot(
		make([]byte, 32), &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	script, err := txscript.PayToAddrScript(addr)
	require.NoError(t, err)

	return script
}

// p2pkhPkScript returns a deterministic legacy P2PKH script.
func p2pkhPkScript(t *testing.T) []byte {
	t.Helper()

	addr, err := btcaddr.NewAddressPubKeyHash(
		make([]byte, 20), &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	script, err := txscript.PayToAddrScript(addr)
	require.NoError(t, err)

	return script
}
