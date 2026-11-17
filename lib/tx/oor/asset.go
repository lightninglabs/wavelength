package oor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
)

// MaxRecipientAssetRefBytes bounds the asset reference a recipient output
// names.
const MaxRecipientAssetRefBytes = 512

// RecipientAsset describes the Taproot Asset an Ark recipient output carries.
type RecipientAsset struct {
	// Root is the Taproot Asset commitment root. The output pays to the
	// policy root and this root as siblings.
	Root chainhash.Hash

	// Ref names the asset, as a tap-sdk asset reference.
	Ref string

	// Amount is the number of asset units the output holds. The output's
	// Value is its separate Bitcoin carrier.
	Amount uint64
}

// RoutingPkScript returns the script that addresses the output to its owner.
// An asset output pays to its policy composed with the asset root, so it
// routes through the uncomposed policy script, which the owner knows before
// the asset root exists.
func (o RecipientOutput) RoutingPkScript() ([]byte, error) {
	if o.Asset == nil {
		return bytes.Clone(o.PkScript), nil
	}

	template, err := arkscript.DecodePolicyTemplate(o.VTXOPolicyTemplate)
	if err != nil {
		return nil, fmt.Errorf("decode asset output policy: %w", err)
	}

	return template.PkScript()
}

// ValidateAsset checks that an asset output pays to its policy composed with
// its asset root. Outputs without an asset pass unchanged.
func (o RecipientOutput) ValidateAsset() error {
	if o.Asset == nil {
		return nil
	}

	switch {
	case o.Asset.Ref == "":
		return fmt.Errorf("asset output ref must be provided")

	case len(o.Asset.Ref) > MaxRecipientAssetRefBytes:
		return fmt.Errorf("asset output ref exceeds %d bytes",
			MaxRecipientAssetRefBytes)

	case o.Asset.Amount == 0:
		return fmt.Errorf("asset output amount must be positive")

	case len(o.VTXOPolicyTemplate) == 0:
		return fmt.Errorf("asset output policy must be provided")
	}

	template, err := arkscript.DecodePolicyTemplate(o.VTXOPolicyTemplate)
	if err != nil {
		return fmt.Errorf("decode asset output policy: %w", err)
	}
	policy, err := template.Compile()
	if err != nil {
		return fmt.Errorf("compile asset output policy: %w", err)
	}
	composed, err := arkscript.ComposeWithSiblingRoot(
		policy, o.Asset.Root,
	)
	if err != nil {
		return fmt.Errorf("compose asset output policy: %w", err)
	}
	pkScript, err := txscript.PayToTaprootScript(composed.OutputKey())
	if err != nil {
		return fmt.Errorf("derive asset output script: %w", err)
	}
	if !bytes.Equal(pkScript, o.PkScript) {
		return fmt.Errorf("asset output script does not commit to " +
			"its policy and asset root")
	}

	return nil
}
