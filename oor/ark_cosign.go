package oor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// arkCoSignSigHashType is the only taproot sighash mode the operator will
// ever sign with on an Ark PSBT input, and the only mode it will tolerate
// on the client-supplied taproot script-spend signatures it co-signs.
//
// SigHashDefault is the BIP-341 "commit to everything" mode (all inputs,
// all outputs) and yields the compact 64-byte schnorr signature with no
// trailing flag byte. Weaker modes such as SIGHASH_NONE, SIGHASH_SINGLE,
// or any |SIGHASH_ANYONECANPAY variant would leave inputs and/or outputs
// un-committed; that lets a malicious OOR submitter recover the operator
// signature from the persisted Ark PSBT (exposed to recipients) and
// replay it onto a conflicting Ark transaction that redirects funds away
// from the validated recipient outputs. We therefore pin the operator's
// signature to SigHashDefault and reject any other client-supplied
// SigHash on the inputs we co-sign.
const arkCoSignSigHashType = txscript.SigHashDefault

// CoSignArkPSBT attaches the operator's tapscript signature to each Ark input.
//
// The client already commits to the intended leaf path by attaching the leaf
// script and its own signature material to the Ark PSBT. The operator signs
// that same leaf here so the persisted Ark PSBT is actually broadcastable
// during unilateral exit/unroll.
func CoSignArkPSBT(signer input.Signer, operatorKey keychain.KeyDescriptor,
	ark *psbt.Packet) (bool, error) {

	switch {
	case signer == nil:
		return false, fmt.Errorf("signer must be provided")

	case operatorKey.PubKey == nil:
		return false, fmt.Errorf("operator pubkey must be provided")

	case ark == nil || ark.UnsignedTx == nil:
		return false, fmt.Errorf("ark psbt must include unsigned tx")

	case len(ark.Inputs) != len(ark.UnsignedTx.TxIn):
		return false, fmt.Errorf("ark psbt input count mismatch")
	}

	prevOuts := make(
		map[wire.OutPoint]*wire.TxOut, len(ark.UnsignedTx.TxIn),
	)
	for i, txIn := range ark.UnsignedTx.TxIn {
		witnessUtxo := ark.Inputs[i].WitnessUtxo
		if witnessUtxo == nil {
			return false, fmt.Errorf("ark input %d missing "+
				"witness utxo", i)
		}

		prevOuts[txIn.PreviousOutPoint] = witnessUtxo
	}

	prevFetcher := txscript.NewMultiPrevOutFetcher(prevOuts)
	sigHashes := txscript.NewTxSigHashes(ark.UnsignedTx, prevFetcher)

	signedAny := false
	for i := range ark.Inputs {
		signed, err := coSignArkInput(
			signer, operatorKey, ark, i, prevFetcher, sigHashes,
		)
		if err != nil {
			return false, fmt.Errorf("co-sign ark input %d: %w", i,
				err)
		}

		signedAny = signedAny || signed
	}

	return signedAny, nil
}

func coSignArkInput(signer input.Signer, operatorKey keychain.KeyDescriptor,
	ark *psbt.Packet, inputIndex int,
	prevFetcher txscript.PrevOutputFetcher,
	sigHashes *txscript.TxSigHashes) (bool, error) {

	if ark == nil || ark.UnsignedTx == nil {
		return false, fmt.Errorf("ark psbt must include unsigned tx")
	}

	pInput := &ark.Inputs[inputIndex]
	if len(pInput.TaprootScriptSpendSig) == 0 {
		return false, nil
	}

	// Ark co-signing requires every taproot script-spend signature to
	// commit to ALL inputs and ALL outputs. Anything other than BIP-341
	// SIGHASH_DEFAULT (e.g. SIGHASH_NONE/SINGLE/ANYONECANPAY) lets the
	// operator's signature replay against a conflicting Ark spend that
	// redirects funds away from the validated recipients. We refuse to
	// co-sign such requests at the boundary where the untrusted client
	// PSBT enters the signing decision; arkSigningLeaf also rejects any
	// non-default client-supplied sighash to keep the owner half of the
	// witness on the same safe policy.
	leaf, err := arkSigningLeaf(pInput)
	if err != nil {
		return false, err
	}

	if !tapLeafScriptPushesPubKey(leaf.Script, operatorKey.PubKey) {
		return false, nil
	}

	witnessUtxo := pInput.WitnessUtxo
	if witnessUtxo == nil {
		return false, fmt.Errorf("missing witness utxo")
	}

	signDesc := &input.SignDescriptor{
		KeyDesc:           operatorKey,
		SignMethod:        input.TaprootScriptSpendSignMethod,
		Output:            witnessUtxo,
		HashType:          arkCoSignSigHashType,
		SigHashes:         sigHashes,
		PrevOutputFetcher: prevFetcher,
		InputIndex:        inputIndex,
		WitnessScript:     leaf.Script,
		ControlBlock:      leaf.ControlBlock,
	}

	sig, err := signer.SignOutputRaw(ark.UnsignedTx, signDesc)
	if err != nil {
		return false, fmt.Errorf("sign output: %w", err)
	}

	sigBytes := sig.Serialize()
	if len(sigBytes) == 0 {
		return false, fmt.Errorf("signer returned empty signature")
	}

	if err := addTaprootScriptSpendSig(
		pInput, operatorKey.PubKey, leaf.Script, sigBytes,
		signDesc.HashType,
	); err != nil {
		return false, err
	}

	if err := reorderTaprootScriptSpendSigs(
		pInput, operatorKey.PubKey, leaf.Script,
	); err != nil {
		return false, fmt.Errorf("reorder sigs: %w", err)
	}

	return true, nil
}

// arkSigningLeaf resolves the leaf script the operator should co-sign for an
// Ark PSBT input from the client-supplied taproot script-spend signatures and
// leaf scripts attached to the PSBT.
//
// It also enforces that every client-supplied TaprootScriptSpendSig.SigHash
// equals arkCoSignSigHashType (BIP-341 SIGHASH_DEFAULT). The operator never
// trusts the SigHash field for its own signing decision — it always signs
// with arkCoSignSigHashType — but the owner half of the witness is persisted
// alongside the operator signature and broadcast as one package, so a weak
// owner sighash would still let an attacker replay the joint witness onto a
// conflicting Ark spend. Reject such requests at the boundary instead.
func arkSigningLeaf(in *psbt.PInput) (*psbt.TaprootTapLeafScript, error) {
	if in == nil {
		return nil, fmt.Errorf("psbt input must be provided")
	}

	var targetLeafHash []byte

	for i := range in.TaprootScriptSpendSig {
		sigRec := in.TaprootScriptSpendSig[i]
		if sigRec == nil {
			continue
		}

		if sigRec.SigHash != arkCoSignSigHashType {
			return nil, fmt.Errorf("ark psbt taproot script spend "+
				"sig %d uses disallowed sighash %d: only "+
				"SIGHASH_DEFAULT is permitted", i,
				sigRec.SigHash)
		}

		if len(targetLeafHash) == 0 {
			targetLeafHash = append(
				[]byte(nil), sigRec.LeafHash...,
			)

			continue
		}

		if !bytes.Equal(targetLeafHash, sigRec.LeafHash) {
			return nil, fmt.Errorf("taproot signatures reference " +
				"multiple leaf hashes")
		}
	}

	for i := range in.TaprootLeafScript {
		leaf := in.TaprootLeafScript[i]
		if leaf == nil {
			continue
		}

		if len(targetLeafHash) == 0 {
			return leaf, nil
		}

		leafHash := txscript.NewTapLeaf(
			leaf.LeafVersion, leaf.Script,
		).TapHash()
		if bytes.Equal(leafHash[:], targetLeafHash) {
			return leaf, nil
		}
	}

	if len(targetLeafHash) != 0 {
		return nil, fmt.Errorf("taproot leaf script not found")
	}

	return nil, fmt.Errorf("missing taproot leaf script")
}

func reorderTaprootScriptSpendSigs(in *psbt.PInput,
	operatorKey *btcec.PublicKey, leafScript []byte) error {

	if in == nil || operatorKey == nil || len(leafScript) == 0 {
		return nil
	}

	operatorSig, err := findArkTaprootScriptSpendSig(
		in, operatorKey, leafScript,
	)
	if err != nil {
		return fmt.Errorf("find operator sig for reorder: %w", err)
	}
	if operatorSig == nil {
		return fmt.Errorf("operator sig not found after signing")
	}

	reordered := make(
		[]*psbt.TaprootScriptSpendSig, 0, len(in.TaprootScriptSpendSig),
	)
	reordered = append(reordered, operatorSig)

	for i := range in.TaprootScriptSpendSig {
		sigRec := in.TaprootScriptSpendSig[i]
		if sigRec == nil || sigRec == operatorSig {
			continue
		}

		reordered = append(reordered, sigRec)
	}

	in.TaprootScriptSpendSig = reordered

	return nil
}

// tapLeafScriptPushesPubKey returns true when the script contains the target
// x-only pubkey as a pushed data element rather than as incidental raw bytes.
func tapLeafScriptPushesPubKey(script []byte, pubKey *btcec.PublicKey) bool {
	if len(script) == 0 || pubKey == nil {
		return false
	}

	wantPub := schnorr.SerializePubKey(pubKey)
	tokenizer := txscript.MakeScriptTokenizer(0, script)
	for tokenizer.Next() {
		if bytes.Equal(tokenizer.Data(), wantPub) {
			return true
		}
	}

	return false
}

func findArkTaprootScriptSpendSig(in *psbt.PInput, pubKey *btcec.PublicKey,
	leafScript []byte) (*psbt.TaprootScriptSpendSig, error) {

	if in == nil {
		return nil, fmt.Errorf("psbt input must be provided")
	}

	if pubKey == nil {
		return nil, fmt.Errorf("pubkey must be provided")
	}

	leafHash := txscript.NewBaseTapLeaf(leafScript).TapHash()
	wantPub := schnorr.SerializePubKey(pubKey)

	for i := range in.TaprootScriptSpendSig {
		sigRec := in.TaprootScriptSpendSig[i]
		if sigRec == nil {
			continue
		}

		if !bytes.Equal(sigRec.XOnlyPubKey, wantPub) {
			continue
		}

		if !bytes.Equal(sigRec.LeafHash, leafHash[:]) {
			continue
		}

		return sigRec, nil
	}

	return nil, fmt.Errorf("missing taproot script spend signature")
}
