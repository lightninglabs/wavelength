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
func validateSpendPathAgainstPolicy(template *arkscript.PolicyTemplate,
	spendPath *arkscript.SpendPath) error {

	_, err := resolveSpendPathLeaf(template, spendPath)

	return err
}

// resolveSpendPathLeaf locates the semantic leaf of `template` whose compiled
// script+control block match `spendPath`. Returning the matched AST node
// lets callers run AST-level checks (e.g. arkscript.ContainsKey) on the
// exact leaf that will execute at spend time — byte-level scans of the
// compiled witness script can miss pushed data that happens to equal an
// operator key. A nil node is never returned on success.
func resolveSpendPathLeaf(template *arkscript.PolicyTemplate,
	spendPath *arkscript.SpendPath) (arkscript.Node, error) {

	if template == nil {
		return nil, fmt.Errorf("policy template must be provided")
	}
	if spendPath == nil {
		return nil, fmt.Errorf("spend path must be provided")
	}

	compiled, err := template.Compile()
	if err != nil {
		return nil, fmt.Errorf("compile policy template: %w", err)
	}

	for i := range compiled.Leaves {
		info, err := compiled.SpendInfo(i)
		if err != nil {
			return nil, fmt.Errorf("derive compiled spend info: %w",
				err)
		}

		if !bytes.Equal(info.WitnessScript, spendPath.WitnessScript) {
			continue
		}

		if !bytes.Equal(info.ControlBlock, spendPath.ControlBlock) {
			continue
		}

		// The compiled script uniquely identifies the AST leaf in
		// the template (LeafTemplate.Script is deterministic); find
		// the template entry whose compiled script matches so we
		// can return its AST node for downstream checks.
		for j := range template.Leaves {
			leafScript, err := template.Leaves[j].Script()
			if err != nil {
				return nil, fmt.Errorf("compile template leaf "+
					"%d: %w", j, err)
			}

			if bytes.Equal(
				leafScript, spendPath.WitnessScript,
			) {
				return template.Leaves[j].Node, nil
			}
		}

		// A compiled-tree leaf that matches but has no template
		// origin would be a library bug (Compile must only emit
		// leaves it was given). Surface it explicitly rather than
		// returning (nil, nil) and having the caller silently skip
		// the AST check.
		return nil, fmt.Errorf("spend path matches compiled leaf " +
			"with no AST origin")
	}

	return nil, fmt.Errorf("spend path is not a leaf of vtxo policy " +
		"template")
}
