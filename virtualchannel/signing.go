package virtualchannel

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// SigningInput is the local VTXO metadata required to sign one backing input.
type SigningInput struct {
	BackingVTXO

	PkScript       []byte
	PolicyTemplate []byte
	ClientKey      keychain.KeyDescriptor
}

// InputSignature is one owner signature for a virtual-channel backing input.
type InputSignature struct {
	OutPoint  wire.OutPoint
	Signature []byte
}

// SignBackingInputs signs every VTXO input of the virtual-channel backing
// parent with the local owner key.
func SignBackingInputs(signer input.Signer, backingTx *wire.MsgTx,
	inputs []SigningInput) ([]InputSignature, error) {

	switch {
	case signer == nil:
		return nil, fmt.Errorf("signer must be provided")

	case backingTx == nil:
		return nil, fmt.Errorf("backing tx must be provided")

	case len(backingTx.TxIn) == 0:
		return nil, fmt.Errorf("backing tx must have inputs")

	case len(inputs) == 0:
		return nil, fmt.Errorf("signing inputs must be provided")
	}

	inputByOutpoint := make(map[wire.OutPoint]SigningInput, len(inputs))
	prevOuts := make(map[wire.OutPoint]*wire.TxOut, len(inputs))
	for _, signingInput := range inputs {
		if signingInput.Amount <= 0 {
			return nil, fmt.Errorf("backing VTXO %s amount must "+
				"be positive", signingInput.OutPoint)
		}
		if len(signingInput.PkScript) == 0 {
			return nil, fmt.Errorf("backing VTXO %s pkScript "+
				"is empty", signingInput.OutPoint)
		}
		if signingInput.ClientKey.PubKey == nil {
			return nil, fmt.Errorf("backing VTXO %s client key "+
				"is missing", signingInput.OutPoint)
		}
		if _, ok := inputByOutpoint[signingInput.OutPoint]; ok {
			return nil, fmt.Errorf("duplicate signing input %s",
				signingInput.OutPoint)
		}

		prevOut := &wire.TxOut{
			Value:    int64(signingInput.Amount),
			PkScript: append([]byte(nil), signingInput.PkScript...),
		}
		inputByOutpoint[signingInput.OutPoint] = signingInput
		prevOuts[signingInput.OutPoint] = prevOut
	}

	prevFetcher := txscript.NewMultiPrevOutFetcher(prevOuts)
	sigHashes := txscript.NewTxSigHashes(backingTx, prevFetcher)

	sigs := make([]InputSignature, 0, len(backingTx.TxIn))
	for inputIndex, txIn := range backingTx.TxIn {
		outpoint := txIn.PreviousOutPoint
		signingInput, ok := inputByOutpoint[outpoint]
		if !ok {
			return nil, fmt.Errorf("missing signing input for %s",
				outpoint)
		}

		spendPath, err := standardCollabSpendPath(
			signingInput.PolicyTemplate, signingInput.PkScript,
			signingInput.ClientKey.PubKey,
		)
		if err != nil {
			return nil, fmt.Errorf("backing VTXO %s: %w", outpoint,
				err)
		}

		prevOut := prevOuts[outpoint]
		signDesc := spendPath.SpendInfo.BuildSignDescriptor(
			signingInput.ClientKey, prevOut, sigHashes, prevFetcher,
			inputIndex,
		)
		sig, err := signer.SignOutputRaw(backingTx, signDesc)
		if err != nil {
			return nil, fmt.Errorf("sign backing input %s: %w",
				outpoint, err)
		}
		sigBytes := sig.Serialize()
		if len(sigBytes) == 0 {
			return nil, fmt.Errorf("signer returned empty " +
				"signature")
		}

		sigs = append(sigs, InputSignature{
			OutPoint:  outpoint,
			Signature: sigBytes,
		})
	}

	return sigs, nil
}

func standardCollabSpendPath(policyTemplate, pkScript []byte,
	clientKey *btcec.PublicKey) (*arkscript.SpendPath, error) {

	if len(policyTemplate) == 0 {
		return nil, fmt.Errorf("policy template must be provided")
	}
	template, err := arkscript.DecodePolicyTemplate(policyTemplate)
	if err != nil {
		return nil, fmt.Errorf("decode policy template: %w", err)
	}
	if !template.MatchesPkScript(pkScript) {
		return nil, fmt.Errorf("policy template does not match " +
			"pkScript")
	}

	params, err := arkscript.DecodeStandardVTXOParams(template)
	if err != nil {
		return nil, fmt.Errorf("decode standard VTXO policy: %w", err)
	}
	if !sameXOnlyPubKey(params.OwnerKey, clientKey) {
		return nil, fmt.Errorf("client key does not own VTXO policy")
	}

	node, err := standardCollabNode(template, params)
	if err != nil {
		return nil, err
	}

	compiled, err := template.Compile()
	if err != nil {
		return nil, fmt.Errorf("compile policy template: %w", err)
	}
	spendPath, err := compiled.SpendPathForNode(node, nil)
	if err != nil {
		return nil, fmt.Errorf("derive collaborative spend path: %w",
			err)
	}
	if err := spendPath.VerifyBindsToPkScript(pkScript); err != nil {
		return nil, fmt.Errorf("collaborative spend path does not "+
			"bind to pkScript: %w", err)
	}

	return spendPath, nil
}

func standardCollabNode(template *arkscript.PolicyTemplate,
	params *arkscript.StandardVTXOParams) (arkscript.Node, error) {

	for i := range template.Leaves {
		node := template.Leaves[i].Node
		multisig, ok := node.(*arkscript.Multisig)
		if !ok || len(multisig.Keys) != 2 {
			continue
		}
		if !arkscript.ContainsKey(node, params.OwnerKey) ||
			!arkscript.ContainsKey(node, params.OperatorKey) {

			continue
		}
		if arkscript.DeriveSequence(node) != wire.MaxTxInSequenceNum ||
			arkscript.DeriveLockTime(node) != 0 {
			return nil, fmt.Errorf("collaborative leaf has " +
				"unexpected tx context")
		}

		return node, nil
	}

	return nil, fmt.Errorf("standard collaborative leaf not found")
}

func sameXOnlyPubKey(a, b *btcec.PublicKey) bool {
	if a == nil || b == nil {
		return false
	}

	return bytes.Equal(
		schnorr.SerializePubKey(a), schnorr.SerializePubKey(b),
	)
}
