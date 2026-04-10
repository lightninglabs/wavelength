package oor

import (
	"bytes"
	"fmt"

	"github.com/lightninglabs/darepo-client/lib/arkscript"
)

// decodeDescriptorPolicyTemplate decodes the semantic VTXO policy carried by
// a signing descriptor.
func decodeDescriptorPolicyTemplate(desc VTXOSigningDescriptor) (
	*arkscript.PolicyTemplate, error) {

	if len(desc.VTXOPolicyTemplate) == 0 {
		return nil, fmt.Errorf("vtxo policy template must be provided")
	}

	template, err := arkscript.DecodePolicyTemplate(desc.VTXOPolicyTemplate)
	if err != nil {
		return nil, fmt.Errorf("decode vtxo policy template: %w", err)
	}

	return template, nil
}

// decodeDescriptorSpendPath decodes the explicit spend path selected for the
// checkpoint spend of a descriptor input.
func decodeDescriptorSpendPath(desc VTXOSigningDescriptor) (
	*arkscript.SpendPath, error) {

	if len(desc.SpendPath) == 0 {
		return nil, fmt.Errorf("spend path must be provided")
	}

	spendPath, err := arkscript.DecodeSpendPath(desc.SpendPath)
	if err != nil {
		return nil, fmt.Errorf("decode spend path: %w", err)
	}

	return spendPath, nil
}

// validateSpendPathAgainstPolicy verifies that the explicit spend path binds
// to one of the compiled leaves of the semantic policy.
func validateSpendPathAgainstPolicy(
	template *arkscript.PolicyTemplate,
	spendPath *arkscript.SpendPath) error {

	if template == nil {
		return fmt.Errorf("policy template must be provided")
	}
	if spendPath == nil {
		return fmt.Errorf("spend path must be provided")
	}

	compiled, err := template.Compile()
	if err != nil {
		return fmt.Errorf("compile policy template: %w", err)
	}

	for i := range compiled.Leaves {
		info, err := compiled.SpendInfo(i)
		if err != nil {
			return fmt.Errorf("derive compiled spend info: %w", err)
		}

		if !bytes.Equal(info.WitnessScript, spendPath.WitnessScript) {
			continue
		}

		if !bytes.Equal(info.ControlBlock, spendPath.ControlBlock) {
			continue
		}

		return nil
	}

	return fmt.Errorf("spend path is not a leaf of vtxo policy template")
}
