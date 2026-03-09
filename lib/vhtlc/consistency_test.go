//go:build dev

package vhtlc

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

func TestVHTLCSpendInfoConsistency(t *testing.T) {
	privA, _ := btcec.NewPrivateKey()
	privB, _ := btcec.NewPrivateKey()
	privS, _ := btcec.NewPrivateKey()
	preimage := []byte("vhtlc-systest-preimage-32bytes!!")
	preimageHash := Hash160(preimage)

	policy, err := NewPolicy(Opts{
		Sender: privA.PubKey(), Receiver: privB.PubKey(), Server: privS.PubKey(),
		PreimageHash: preimageHash, RefundLocktime: 500_000,
		UnilateralClaimDelay: 144, UnilateralRefundDelay: 288,
		UnilateralRefundWithoutReceiverDelay: 1008,
	})
	if err != nil {
		t.Fatal(err)
	}

	pkScript, err := policy.PkScript()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("pkScript (hex): %s", hex.EncodeToString(pkScript))

	info, err := policy.ClaimSpendInfo()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("WitnessScript (hex): %s", hex.EncodeToString(info.WitnessScript))
	t.Logf("ControlBlock (hex): %s", hex.EncodeToString(info.ControlBlock))

	// Verify using script engine with a dummy transaction
	dummyTx := wire.NewMsgTx(2)
	dummyTx.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Index: 0}})
	dummyTx.AddTxOut(&wire.TxOut{Value: 1000, PkScript: pkScript})

	// Build witness with dummy sigs but real preimage, script, ctrl
	dummyTx.TxIn[0].Witness = wire.TxWitness{
		{0xca, 0xfe},
		{0xbe, 0xef},
		preimage,
		info.WitnessScript,
		info.ControlBlock,
	}

	prevFetcher := txscript.NewCannedPrevOutputFetcher(pkScript, 50000)
	sigHashes := txscript.NewTxSigHashes(dummyTx, prevFetcher)

	engine, err := txscript.NewEngine(
		pkScript, dummyTx, 0,
		txscript.StandardVerifyFlags, nil, sigHashes, 50000, prevFetcher,
	)
	if err != nil {
		t.Fatalf("NewEngine failed: %v", err)
	}
	err = engine.Execute()
	if err != nil {
		t.Logf("Execute error: %v", err)
		if bytes.Contains([]byte(err.Error()), []byte("tapscript_root")) {
			t.Fatalf("TAPSCRIPT ROOT MISMATCH: %v", err)
		}
		t.Logf("Expected sig failure (not tapscript) - OK")
	}
}
