package wallet

import (
	"bytes"
	"fmt"
)

// EffectivePolicyTemplate returns the semantic policy for the VTXO.
func (d *VTXODescriptor) EffectivePolicyTemplate() ([]byte, error) {
	if d == nil {
		return nil, fmt.Errorf(
			"wallet VTXO descriptor must be provided",
		)
	}

	if len(d.PolicyTemplate) == 0 {
		return nil, fmt.Errorf(
			"wallet VTXO descriptor policy template " +
				"must be provided",
		)
	}

	return bytes.Clone(d.PolicyTemplate), nil
}
