package bip322

import (
	"fmt"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

// TxSigner signs the provided to_sign transaction in place.
//
// Implementations can use the provided prev-output fetcher and sighash cache
// to produce witnesses for input 0 and any additional proof-of-funds inputs.
type TxSigner interface {
	// SignBIP322 signs the provided to_sign transaction in place.
	SignBIP322(toSpend *wire.MsgTx, toSign *wire.MsgTx,
		prevFetcher txscript.PrevOutputFetcher,
		hashCache *txscript.TxSigHashes) error
}

// FinalizeToSignPSBT finalizes a signed to_sign PSBT and extracts the
// full-format BIP-322 signature payload.
func FinalizeToSignPSBT(packet *psbt.Packet) (*Sig, error) {
	if packet == nil {
		return nil, fmt.Errorf("to_sign psbt must be provided")
	}

	err := psbt.MaybeFinalizeAll(packet)
	if err != nil {
		return nil, fmt.Errorf("finalize to_sign psbt: %w", err)
	}

	if !packet.IsComplete() {
		return nil, fmt.Errorf("to_sign psbt is incomplete")
	}

	toSign, err := psbt.Extract(packet)
	if err != nil {
		return nil, fmt.Errorf("extract to_sign psbt: %w", err)
	}

	return &Sig{
		ToSign: toSign,
	}, nil
}

// BuildAndSignFullTx builds to_spend and to_sign transactions, invokes the
// transaction signer, and returns the full-format BIP-322 signature payload.
func BuildAndSignFullTx(message []byte, messageChallenge []byte,
	signer TxSigner, opts ...ToSignOption) (*Sig, error) {

	if signer == nil {
		return nil, fmt.Errorf("tx signer must be provided")
	}

	messageHash := MessageHash(message)
	toSpend, err := BuildToSpend(messageHash, messageChallenge)
	if err != nil {
		return nil, err
	}

	buildOpts, err := applyAndValidateToSignOptions(toSpend, opts)
	if err != nil {
		return nil, err
	}

	toSign, err := buildToSignTxFromOptions(toSpend, buildOpts)
	if err != nil {
		return nil, err
	}

	prevFetcher, err := buildToSignPrevOutputFetcher(
		toSpend, toSign, buildOpts.additionalInputs,
	)
	if err != nil {
		return nil, err
	}

	hashCache := txscript.NewTxSigHashes(toSign, prevFetcher)
	err = signer.SignBIP322(toSpend, toSign, prevFetcher, hashCache)
	if err != nil {
		return nil, fmt.Errorf("sign to_sign tx: %w", err)
	}

	return &Sig{
		ToSign: toSign,
	}, nil
}

// buildToSignPrevOutputFetcher constructs the prev-output fetcher required for
// custom tx signers.
func buildToSignPrevOutputFetcher(toSpend *wire.MsgTx, toSign *wire.MsgTx,
	additionalInputs []AdditionalInput) (txscript.PrevOutputFetcher,
	error) {

	if toSpend == nil {
		return nil, fmt.Errorf("to_spend transaction must be provided")
	}

	if toSign == nil {
		return nil, fmt.Errorf("to_sign transaction must be provided")
	}

	if len(toSign.TxIn) != len(additionalInputs)+1 {
		return nil, fmt.Errorf("to_sign input count %d does not match "+
			"additional input count %d", len(toSign.TxIn),
			len(additionalInputs))
	}

	if len(toSpend.TxOut) == 0 {
		return nil, fmt.Errorf("to_spend output must be provided")
	}

	prevOuts := make(map[wire.OutPoint]*wire.TxOut, len(toSign.TxIn))
	prevOuts[toSign.TxIn[0].PreviousOutPoint] = cloneTxOut(toSpend.TxOut[0])

	for i := 0; i < len(additionalInputs); i++ {
		additionalInput := additionalInputs[i]
		if additionalInput.WitnessUtxo == nil {
			return nil, fmt.Errorf("additional input %d witness "+
				"utxo must be provided", i+1)
		}

		if len(additionalInput.WitnessUtxo.PkScript) == 0 {
			return nil, fmt.Errorf("additional input %d witness "+
				"utxo script must be provided", i+1)
		}

		txOut := cloneTxOut(additionalInput.WitnessUtxo)
		prevOuts[toSign.TxIn[i+1].PreviousOutPoint] = txOut
	}

	return txscript.NewMultiPrevOutFetcher(prevOuts), nil
}

// cloneTxOut deep-copies a TxOut so callers can safely mutate their buffers
// after calling into this package.
func cloneTxOut(src *wire.TxOut) *wire.TxOut {
	if src == nil {
		return nil
	}

	return &wire.TxOut{
		Value:    src.Value,
		PkScript: cloneBytes(src.PkScript),
	}
}
