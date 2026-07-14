package vtxo

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
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

	if d.TaprootAssetRoot == nil {
		return template.PkScript()
	}

	compiled, err := template.Compile()
	if err != nil {
		return nil, fmt.Errorf("compile VTXO policy: %w", err)
	}

	composed, err := arkscript.ComposeWithSiblingRoot(
		compiled, *d.TaprootAssetRoot,
	)
	if err != nil {
		return nil, fmt.Errorf("compose Taproot Asset root: %w", err)
	}

	pkScript, err := txscript.PayToTaprootScript(composed.OutputKey())
	if err != nil {
		return nil, fmt.Errorf("derive composed VTXO pkscript: %w", err)
	}

	return pkScript, nil
}

// EffectiveStandardSpendInfo derives a standard Ark policy spend path and
// extends its control block with the Taproot Asset root when this descriptor
// represents an asset-bearing VTXO.
func (d *Descriptor) EffectiveStandardSpendInfo(leafIndex int) (
	*arkscript.SpendInfo, error) {

	if d == nil {
		return nil, fmt.Errorf("descriptor must be provided")
	}

	ownerKey := d.ClientKey.PubKey
	operatorKey := d.OperatorKey
	exitDelay := d.RelativeExpiry
	if len(d.PolicyTemplate) > 0 {
		params, err := d.DecodeStandardPolicyTemplate()
		if err != nil {
			return nil, err
		}

		ownerKey = params.OwnerKey
		operatorKey = params.OperatorKey
		exitDelay = params.ExitDelay
	}

	if ownerKey == nil || operatorKey == nil || exitDelay == 0 {
		return nil, fmt.Errorf("standard VTXO policy parameters must " +
			"be provided")
	}

	policy, err := arkscript.NewVTXOPolicy(
		ownerKey, operatorKey, exitDelay,
	)
	if err != nil {
		return nil, fmt.Errorf("build standard VTXO policy: %w", err)
	}

	var spendInfo *arkscript.SpendInfo
	switch leafIndex {
	case 0:
		spendInfo, err = policy.CollabSpendInfo()

	case 1:
		spendInfo, err = policy.ExitSpendInfo()

	default:
		return nil, fmt.Errorf("standard VTXO leaf index %d out "+
			"of range", leafIndex)
	}
	if err != nil {
		return nil, err
	}

	if d.TaprootAssetRoot == nil {
		return spendInfo, nil
	}

	composed, err := arkscript.ComposeWithSiblingRoot(
		policy.CompiledPolicy, *d.TaprootAssetRoot,
	)
	if err != nil {
		return nil, fmt.Errorf("compose Taproot Asset root: %w", err)
	}

	canonicalIndex := policy.ScriptIndex(spendInfo.WitnessScript)
	if canonicalIndex < 0 {
		return nil, fmt.Errorf("standard VTXO spend leaf not found")
	}

	return composed.SpendInfo(canonicalIndex)
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

// RefreshOutputTemplate returns the policy template that should be used for
// the NEW VTXO output that a refresh round mints from this descriptor.
//
// The descriptor's stored PolicyTemplate field carries the operator key the
// VTXO was originally created under (call that K1). When the operator has
// since rotated to a different long-term key (K2), reusing the stored bytes
// verbatim ships K1 inside the JoinRoundRequest's new VTXO template — the
// server's rounds validator then rejects the request with
// ErrOperatorKeyMismatch.
//
// The fix path: rebuild the new output's template with the caller-supplied
// current operator key while preserving the owner key and exit delay that
// the existing descriptor commits to. This intentionally only touches the
// new output side; spend-time material for the old VTXO (forfeit witnesses,
// unilateral exit script) still has to use K1 because that is what the
// original output's taproot tree committed to.
//
// Only the standard Ark VTXO shape is supported here. Custom shapes (vHTLC,
// etc.) return ErrRefreshOperatorKeyUnsupported so callers can surface the
// limitation explicitly rather than silently producing a misshaped template.
//
// A nil currentOperatorKey returns an error so callers that have not wired
// the operator-terms cache yet fail loudly instead of producing a template
// with a zero key.
func (d *Descriptor) RefreshOutputTemplate(
	currentOperatorKey *btcec.PublicKey) ([]byte, error) {

	if d == nil {
		return nil, fmt.Errorf("descriptor must be provided")
	}

	if currentOperatorKey == nil {
		return nil, fmt.Errorf("current operator key must be provided")
	}

	// Decode the stored template once so we can lift the owner key and
	// exit delay back out — those still belong to the holder of this
	// VTXO and survive the operator rotation untouched.
	params, err := d.DecodeStandardPolicyTemplate()
	if err != nil {
		return nil, fmt.Errorf("%w: %w",
			ErrRefreshOperatorKeyUnsupported, err)
	}

	return arkscript.EncodeStandardVTXOTemplate(
		params.OwnerKey, currentOperatorKey, params.ExitDelay,
	)
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
