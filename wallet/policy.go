package wallet

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/lightninglabs/darepo-client/lib/arkscript"
)

// ErrRefreshOperatorKeyUnsupported is returned by RefreshOutputTemplate when
// the descriptor's stored policy is not the standard Ark VTXO shape. The
// operator key sits in a fixed position in standard policies but not in
// custom ones (e.g. vHTLC), so a structural rewrite there could shift the
// wrong field. Callers must either keep the input shape (and fail at the
// rounds validator if the rotation still applies) or surface this error to
// the user with rotation-specific UX.
var ErrRefreshOperatorKeyUnsupported = errors.New("refresh-time operator key " +
	"rewrite only supported for standard VTXO policies")

// EffectivePolicyTemplate returns the semantic policy for the VTXO.
func (d *VTXODescriptor) EffectivePolicyTemplate() ([]byte, error) {
	if d == nil {
		return nil, fmt.Errorf("wallet VTXO descriptor must be " +
			"provided")
	}

	if len(d.PolicyTemplate) == 0 {
		return nil, fmt.Errorf("wallet VTXO descriptor policy " +
			"template must be provided")
	}

	return bytes.Clone(d.PolicyTemplate), nil
}

// RefreshOutputTemplate returns the policy template that should be used for
// the NEW VTXO output that a refresh round mints from this descriptor.
//
// Under the server-injected-operator-key design the client no longer commits
// to a concrete operator key in its round outputs: it builds the new output
// template with the unbound operator-key placeholder
// (arkscript.OperatorKeyPlaceholder) and the server binds its current
// operator key at admission. This sidesteps the refresh-after-rotation
// problem entirely — the refreshed output is never tied to the old (or any)
// concrete operator key on the client side. The owner key and exit delay are
// carried over from the input descriptor.
//
// Only the standard Ark VTXO shape is supported. Non-standard shapes
// (vHTLC, custom) return ErrRefreshOperatorKeyUnsupported so callers can
// surface the limitation rather than silently producing a misshaped
// template.
func (d *VTXODescriptor) RefreshOutputTemplate() ([]byte, error) {
	if d == nil {
		return nil, fmt.Errorf("wallet VTXO descriptor must be " +
			"provided")
	}

	if len(d.PolicyTemplate) == 0 {
		return nil, fmt.Errorf("wallet VTXO descriptor policy " +
			"template must be provided")
	}

	// Wrap decode failures with ErrRefreshOperatorKeyUnsupported so the
	// wallet and vtxo refresh paths report the same explicit unsupported
	// result for malformed or custom policy templates.
	template, err := arkscript.DecodePolicyTemplate(d.PolicyTemplate)
	if err != nil {
		return nil, fmt.Errorf("%w: decode stored policy template: %w",
			ErrRefreshOperatorKeyUnsupported, err)
	}

	params, err := arkscript.DecodeStandardVTXOParams(template)
	if err != nil {
		return nil, fmt.Errorf("%w: %w",
			ErrRefreshOperatorKeyUnsupported, err)
	}

	return arkscript.EncodeStandardVTXOTemplate(
		params.OwnerKey, &arkscript.OperatorKeyPlaceholder,
		params.ExitDelay,
	)
}
