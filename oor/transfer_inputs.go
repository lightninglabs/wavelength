package oor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/vtxo"
)

// TransferInput describes a spendable VTXO being used as an input to an
// outgoing OOR transfer.
//
// The VTXO descriptor provides everything needed for client-side signing
// (key descriptor + tapscript). The OwnerLeafScript is the draft
// checkpoint output leaf script committed to in the checkpoint output
// tap tree.
type TransferInput struct {
	// VTXO is the descriptor for the input VTXO being transferred.
	VTXO *vtxo.Descriptor

	// VTXOPolicyTemplate is the semantic arkscript policy
	// encoding for the spent input VTXO. When empty, standard
	// Ark ownership is derived from the descriptor's
	// owner/operator/expiry tuple.
	VTXOPolicyTemplate []byte

	// TaprootAssetRoot is the optional root of the Taproot Asset
	// commitment anchored in the spent VTXO. The semantic VTXO policy is
	// the sibling branch. When present, EffectiveSpendPath extends the
	// policy control block with this root before any signer sees it.
	TaprootAssetRoot *chainhash.Hash

	// OwnerLeafScript is the leaf script committed to the checkpoint
	// tap tree. When empty and VTXO.ClientKey + VTXO.OperatorKey are
	// set, it is auto-derived via MultiSigCollabTapLeaf.
	OwnerLeafScript []byte

	// OwnerLeafPolicy is the semantic owner-leaf policy encoding
	// corresponding to OwnerLeafScript. When present, higher layers can
	// reconstruct and validate the leaf without decompiling raw script.
	OwnerLeafPolicy []byte

	// CustomSpend overrides the default collaborative leaf signing
	// for non-standard VTXOs (e.g., vHTLC Claim path). When set,
	// checkpoint signing uses this spend path directly.
	CustomSpend *arkscript.SpendPath

	// CustomSpendKeys are the public keys required by CustomSpend in script
	// evaluation order. Witness assembly reverses this order because
	// CHECKSIGVERIFY consumes signatures from the top of the stack.
	CustomSpendKeys []*btcec.PublicKey

	// ExternalSignatures are tapscript signatures produced by additional
	// custom-spend participants before the local daemon adds its signature.
	ExternalSignatures []ExternalTaprootScriptSignature
}

// ExternalTaprootScriptSignature carries one externally produced tapscript
// signature for a custom OOR input.
type ExternalTaprootScriptSignature struct {
	// PubKey is the compressed public key that produced the signature.
	PubKey *btcec.PublicKey

	// WitnessScript is the tapscript leaf this signature commits to.
	WitnessScript []byte

	// Signature is the raw Schnorr signature, optionally followed by a
	// one-byte sighash type when SigHash is not SIGHASH_DEFAULT.
	Signature []byte

	// SigHash is the tapscript sighash type. Zero means SIGHASH_DEFAULT.
	SigHash txscript.SigHashType
}

// InputOutpoints returns the VTXO outpoints for the transfer inputs.
func InputOutpoints(inputs []TransferInput) []wire.OutPoint {
	outpoints := make([]wire.OutPoint, 0, len(inputs))
	for i := range inputs {
		outpoints = append(outpoints, inputs[i].VTXO.Outpoint)
	}

	return outpoints
}

// Validate performs basic structural validation. For custom spend
// paths, the TapScript and ClientKey requirements are relaxed since
// the spend path carries its own signing context.
func (i *TransferInput) Validate() error {
	switch {
	case i == nil:
		return fmt.Errorf("transfer input must be provided")

	case i.VTXO == nil:
		return fmt.Errorf("vtxo must be provided")

	case i.VTXO.Amount <= 0:
		return fmt.Errorf("vtxo amount must be positive")

	case len(i.VTXO.PkScript) == 0:
		return fmt.Errorf("vtxo pkScript must be provided")

	case !i.IsCustomSpend() && i.VTXO.TapScript == nil:
		return fmt.Errorf("vtxo tapscript must be provided")

	case !i.IsCustomSpend() && i.VTXO.ClientKey.PubKey == nil:
		return fmt.Errorf("vtxo client key must be provided")
	}
	if (i.VTXO.TaprootAssetRef == "") !=
		(i.VTXO.TaprootAssetAmount == 0) {
		return fmt.Errorf("vtxo asset ref and amount must both be " +
			"provided")
	}
	if len(i.VTXO.TaprootAssetRef) > MaxTaprootAssetRefBytes {
		return fmt.Errorf("vtxo asset ref exceeds %d bytes",
			MaxTaprootAssetRefBytes)
	}
	if i.VTXO.TaprootAssetRef != "" &&
		i.VTXO.TaprootAssetRoot == nil {
		return fmt.Errorf("vtxo asset metadata requires a commitment " +
			"root")
	}
	if (i.TaprootAssetRoot == nil) !=
		(i.VTXO.TaprootAssetRoot == nil) {
		return fmt.Errorf("transfer input and vtxo asset roots " +
			"disagree")
	}
	if i.TaprootAssetRoot != nil &&
		*i.TaprootAssetRoot != *i.VTXO.TaprootAssetRoot {
		return fmt.Errorf("transfer input and vtxo asset roots " +
			"disagree")
	}

	defaultLeaf, defaultPolicy, err := defaultOwnerLeaf(
		i.VTXO.ClientKey.PubKey, i.VTXO.OperatorKey,
	)
	if err != nil {
		return err
	}

	if len(i.VTXOPolicyTemplate) == 0 {
		i.VTXOPolicyTemplate, err = i.defaultVTXOPolicyTemplate()
		if err != nil {
			return err
		}
	}

	if i.TaprootAssetRoot != nil {
		err := validateTaprootAssetPkScript(
			i.VTXOPolicyTemplate, *i.TaprootAssetRoot,
			i.VTXO.PkScript,
		)
		if err != nil {
			return err
		}
	}

	if len(i.OwnerLeafPolicy) == 0 && len(defaultPolicy) > 0 {
		switch {
		case len(i.OwnerLeafScript) == 0:
			i.OwnerLeafPolicy = defaultPolicy

		case bytes.Equal(i.OwnerLeafScript, defaultLeaf):
			i.OwnerLeafPolicy = defaultPolicy
		}
	}

	compiledLeaf, err := compileOwnerLeaf(i.OwnerLeafPolicy)
	if err != nil {
		return err
	}

	switch {
	case len(i.OwnerLeafScript) == 0 && len(compiledLeaf) > 0:
		i.OwnerLeafScript = compiledLeaf

	case len(i.OwnerLeafScript) > 0 && len(compiledLeaf) > 0:
		if string(i.OwnerLeafScript) != string(compiledLeaf) {
			return fmt.Errorf("owner leaf script and policy " +
				"mismatch")
		}
	}

	// Auto-derive the default collaborative owner leaf when neither
	// representation was provided.
	if len(i.OwnerLeafScript) == 0 {
		if len(defaultLeaf) == 0 || len(defaultPolicy) == 0 {
			return fmt.Errorf("owner leaf script must be provided")
		}

		i.OwnerLeafScript = defaultLeaf
		i.OwnerLeafPolicy = defaultPolicy
	}

	if len(i.OwnerLeafPolicy) == 0 {
		return fmt.Errorf("owner leaf policy must be provided")
	}

	if i.CustomSpend != nil && len(i.CustomSpendKeys) == 0 &&
		len(i.VTXOPolicyTemplate) > 0 {

		keys, err := customSpendKeys(
			i.VTXOPolicyTemplate, i.CustomSpend,
		)
		if err != nil {
			return err
		}

		i.CustomSpendKeys = keys
	}

	return nil
}

// validateTaprootAssetPkScript proves that the semantic Ark policy and asset
// commitment root derive the actual VTXO output script. This check prevents a
// caller from supplying a valid asset root that belongs to a different
// anchor output.
func validateTaprootAssetPkScript(policyTemplate []byte,
	assetRoot chainhash.Hash, pkScript []byte) error {

	template, err := arkscript.DecodePolicyTemplate(policyTemplate)
	if err != nil {
		return fmt.Errorf("decode asset-bearing vtxo policy: %w", err)
	}

	compiled, err := template.Compile()
	if err != nil {
		return fmt.Errorf("compile asset-bearing vtxo policy: %w", err)
	}

	composed, err := arkscript.ComposeWithSiblingRoot(compiled, assetRoot)
	if err != nil {
		return fmt.Errorf("compose asset-bearing vtxo policy: %w", err)
	}

	wantPkScript, err := txscript.PayToTaprootScript(composed.OutputKey())
	if err != nil {
		return fmt.Errorf("derive asset-bearing vtxo pkscript: %w", err)
	}
	if !bytes.Equal(wantPkScript, pkScript) {
		return fmt.Errorf("taproot asset root and vtxo pkscript " +
			"mismatch")
	}

	return nil
}

// EffectiveVTXOPolicyTemplate returns the semantic policy encoding for the
// spent input VTXO.
func (i *TransferInput) EffectiveVTXOPolicyTemplate() ([]byte, error) {
	if err := i.Validate(); err != nil {
		return nil, err
	}

	if len(i.VTXOPolicyTemplate) == 0 {
		return nil, fmt.Errorf("vtxo policy template must be provided")
	}

	return bytes.Clone(i.VTXOPolicyTemplate), nil
}

// EffectiveSpendPath returns the explicit spend path for the checkpoint spend
// of the input VTXO.
func (i *TransferInput) EffectiveSpendPath() (*arkscript.SpendPath, error) {
	if err := i.Validate(); err != nil {
		return nil, err
	}

	if i.CustomSpend != nil {
		return i.CustomSpend, nil
	}

	template, err := arkscript.DecodePolicyTemplate(i.VTXOPolicyTemplate)
	if err != nil {
		return nil, fmt.Errorf("decode vtxo policy template: %w", err)
	}

	params, err := arkscript.DecodeStandardVTXOParams(template)
	if err != nil {
		return nil, fmt.Errorf("derive default spend path: %w", err)
	}

	policy, err := arkscript.NewVTXOPolicy(
		params.OwnerKey, params.OperatorKey, params.ExitDelay,
	)
	if err != nil {
		return nil, fmt.Errorf("build standard vtxo policy: %w", err)
	}

	info, err := policy.CollabSpendInfo()
	if err != nil {
		return nil, fmt.Errorf("derive collab spend info: %w", err)
	}
	if i.TaprootAssetRoot != nil {
		leafIndex := policy.ScriptIndex(info.WitnessScript)
		if leafIndex < 0 {
			return nil, fmt.Errorf("derive collab spend leaf index")
		}

		composed, err := arkscript.ComposeWithSiblingRoot(
			policy.CompiledPolicy, *i.TaprootAssetRoot,
		)
		if err != nil {
			return nil, fmt.Errorf("compose vtxo asset root: %w",
				err)
		}
		info, err = composed.SpendInfo(leafIndex)
		if err != nil {
			return nil, fmt.Errorf("derive composed collab spend "+
				"info: %w", err)
		}
	}

	return &arkscript.SpendPath{
		SpendInfo: info,
	}, nil
}

// IsCustomSpend returns true when this input uses a non-standard spend
// path (e.g., vHTLC Claim) rather than the default collaborative VTXO
// leaf.
func (i *TransferInput) IsCustomSpend() bool {
	return i.CustomSpend != nil
}

// customSpendKeys returns the signing keys required by spendPath in script
// order by matching the spend path witness script to the semantic policy leaf.
func customSpendKeys(policyTemplate []byte,
	spendPath *arkscript.SpendPath) ([]*btcec.PublicKey, error) {

	if spendPath == nil {
		return nil, fmt.Errorf("custom spend path must be provided")
	}

	template, err := arkscript.DecodePolicyTemplate(policyTemplate)
	if err != nil {
		return nil, fmt.Errorf("decode custom spend policy: %w", err)
	}

	for _, leaf := range template.Leaves {
		script, err := leaf.Script()
		if err != nil {
			return nil, fmt.Errorf("compile custom spend leaf: %w",
				err)
		}

		if !bytes.Equal(script, spendPath.WitnessScript) {
			continue
		}

		keys := multisigKeys(leaf.Node)
		if len(keys) == 0 {
			return nil, fmt.Errorf("custom spend leaf has no " +
				"multisig keys")
		}

		return keys, nil
	}

	return nil, fmt.Errorf("custom spend leaf not found in policy")
}

// multisigKeys extracts the first multisig key set from a semantic node.
func multisigKeys(node arkscript.Node) []*btcec.PublicKey {
	switch n := node.(type) {
	case *arkscript.Multisig:
		return append([]*btcec.PublicKey(nil), n.Keys...)

	case *arkscript.Condition:
		return multisigKeys(n.Inner)

	case *arkscript.CSV:
		return multisigKeys(n.Inner)

	default:
		return nil
	}
}

// defaultVTXOPolicyTemplate derives the standard semantic policy for inputs
// that still use the canonical Ark owner/operator/CSV shape.
func (i *TransferInput) defaultVTXOPolicyTemplate() ([]byte, error) {
	if i == nil || i.VTXO == nil {
		return nil, fmt.Errorf("transfer input vtxo must be provided")
	}

	if i.VTXO.ClientKey.PubKey == nil || i.VTXO.OperatorKey == nil {
		return nil, nil
	}

	if i.VTXO.RelativeExpiry == 0 {
		return nil, nil
	}

	policy, err := arkscript.EncodeStandardVTXOTemplate(
		i.VTXO.ClientKey.PubKey, i.VTXO.OperatorKey,
		i.VTXO.RelativeExpiry,
	)
	if err != nil {
		return nil, fmt.Errorf("encode standard vtxo policy: %w", err)
	}

	template, err := arkscript.DecodePolicyTemplate(policy)
	if err != nil {
		return nil, fmt.Errorf("decode derived standard policy: %w",
			err)
	}

	if i.TaprootAssetRoot == nil {
		if !template.MatchesPkScript(i.VTXO.PkScript) {
			return nil, nil
		}

		return policy, nil
	}

	compiled, err := template.Compile()
	if err != nil {
		return nil, fmt.Errorf("compile standard vtxo policy: %w", err)
	}
	composed, err := arkscript.ComposeWithSiblingRoot(
		compiled, *i.TaprootAssetRoot,
	)
	if err != nil {
		return nil, fmt.Errorf("compose standard vtxo asset root: %w",
			err)
	}
	pkScript, err := txscript.PayToTaprootScript(composed.OutputKey())
	if err != nil {
		return nil, fmt.Errorf("derive composed vtxo pkscript: %w", err)
	}
	if !bytes.Equal(pkScript, i.VTXO.PkScript) {
		return nil, nil
	}

	return policy, nil
}

// CheckpointInput converts the OOR transfer input into the common tx
// builder checkpoint input type.
func (i *TransferInput) CheckpointInput() (oortx.CheckpointInput, error) {
	err := i.Validate()
	if err != nil {
		return oortx.CheckpointInput{}, err
	}

	return oortx.CheckpointInput{
		SpentVTXO: oortx.SpentVTXORef{
			Outpoint: i.VTXO.Outpoint,
			Output: &wire.TxOut{
				Value:    int64(i.VTXO.Amount),
				PkScript: i.VTXO.PkScript,
			},
		},
		OwnerLeafScript: i.OwnerLeafScript,
		OwnerLeafPolicy: i.OwnerLeafPolicy,
	}, nil
}

// compileOwnerLeaf compiles the semantic owner-leaf policy when present.
func compileOwnerLeaf(ownerLeafPolicy []byte) ([]byte, error) {
	if len(ownerLeafPolicy) == 0 {
		return nil, nil
	}

	leaf, err := arkscript.DecodeLeafTemplate(ownerLeafPolicy)
	if err != nil {
		return nil, fmt.Errorf("decode owner leaf policy: %w", err)
	}

	script, err := leaf.Script()
	if err != nil {
		return nil, fmt.Errorf("compile owner leaf policy: %w", err)
	}

	return script, nil
}

// defaultOwnerLeaf derives the standard owner/operator collaborative leaf
// and its semantic policy encoding when both keys are available.
func defaultOwnerLeaf(ownerKey, operatorKey *btcec.PublicKey) ([]byte, []byte,
	error) {

	if ownerKey == nil || operatorKey == nil {
		return nil, nil, nil
	}

	leaf, err := arkscript.MultiSigCollabTapLeaf(
		ownerKey, operatorKey,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("derive owner leaf: %w", err)
	}

	leafPolicy, err := arkscript.LeafTemplate{
		Node: &arkscript.Multisig{
			Keys: []*btcec.PublicKey{
				ownerKey,
				operatorKey,
			},
		},
	}.Encode()
	if err != nil {
		return nil, nil, fmt.Errorf("encode owner leaf policy: %w", err)
	}

	return leaf.Script, leafPolicy, nil
}
