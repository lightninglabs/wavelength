package wallet

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
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
// See vtxo.(*Descriptor).RefreshOutputTemplate for the full rationale; the
// wallet-level descriptor type carries the same fields and needs the same
// rewrite so the explicit RefreshVTXOs RPC path emits a join request whose
// new-output template commits to the operator's current key.
//
// Only the standard Ark VTXO shape is supported. Non-standard shapes
// (vHTLC, custom) return ErrRefreshOperatorKeyUnsupported so callers can
// surface the limitation rather than silently producing a misshaped
// template.
func (d *VTXODescriptor) RefreshOutputTemplate(
	currentOperatorKey *btcec.PublicKey) ([]byte, error) {

	if d == nil {
		return nil, fmt.Errorf("wallet VTXO descriptor must be " +
			"provided")
	}

	if currentOperatorKey == nil {
		return nil, fmt.Errorf("current operator key must be provided")
	}

	if len(d.PolicyTemplate) == 0 {
		return nil, fmt.Errorf("wallet VTXO descriptor policy " +
			"template must be provided")
	}

	// Wrap both decode failures with ErrRefreshOperatorKeyUnsupported so
	// callers can branch with a single errors.Is check, mirroring
	// vtxo.(*Descriptor).RefreshOutputTemplate. Without this, a
	// DecodePolicyTemplate failure (malformed bytes or a future
	// non-TLV shape) would propagate as a plain error and the caller's
	// fallback path would diverge between the wallet and vtxo sides
	// for the same input.
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
		params.OwnerKey, currentOperatorKey, params.ExitDelay,
	)
}
