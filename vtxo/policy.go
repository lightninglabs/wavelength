package vtxo

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
)

// EffectivePolicyTemplate returns the semantic policy for the VTXO.
func (d *Descriptor) EffectivePolicyTemplate() ([]byte, error) {
	if d == nil {
		return nil, fmt.Errorf("descriptor must be provided")
	}

	if len(d.PolicyTemplate) == 0 {
		return nil, fmt.Errorf("descriptor policy template must be " +
			"provided")
	}

	return bytes.Clone(d.PolicyTemplate), nil
}

// DecodePolicyTemplate decodes the semantic policy for the VTXO.
func (d *Descriptor) DecodePolicyTemplate() (*arkscript.PolicyTemplate, error) {
	raw, err := d.EffectivePolicyTemplate()
	if err != nil {
		return nil, err
	}

	return arkscript.DecodePolicyTemplate(raw)
}

// EffectivePkScript returns the VTXO pkScript derived from the semantic
// policy.
func (d *Descriptor) EffectivePkScript() ([]byte, error) {
	template, err := d.DecodePolicyTemplate()
	if err != nil {
		return nil, err
	}

	return template.PkScript()
}

// DecodeStandardPolicyTemplate extracts the standard VTXO parameters when this
// descriptor uses the default Ark policy shape.
func (d *Descriptor) DecodeStandardPolicyTemplate() (
	*arkscript.StandardVTXOParams, error) {

	template, err := d.DecodePolicyTemplate()
	if err != nil {
		return nil, err
	}

	return arkscript.DecodeStandardVTXOParams(template)
}

// StandardTapScript derives the standard tapscript for descriptors that use
// the default Ark policy shape.
func (d *Descriptor) StandardTapScript() (*waddrmgr.Tapscript, error) {
	params, err := d.DecodeStandardPolicyTemplate()
	if err != nil {
		return nil, err
	}

	return arkscript.VTXOTapScript(
		params.OwnerKey, params.OperatorKey, params.ExitDelay,
	)
}
