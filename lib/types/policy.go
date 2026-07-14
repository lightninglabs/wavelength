package types

import (
	"bytes"
	"fmt"

	"github.com/lightninglabs/wavelength/lib/arkscript"
)

// EffectivePolicyTemplate returns the semantic boarding policy.
func (r *BoardingRequest) EffectivePolicyTemplate() ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("boarding request must be provided")
	}

	if len(r.PolicyTemplate) == 0 {
		return nil, fmt.Errorf("boarding request policy template " +
			"must be provided")
	}

	return bytes.Clone(r.PolicyTemplate), nil
}

// DecodePolicyTemplate decodes the semantic boarding policy.
func (r *BoardingRequest) DecodePolicyTemplate() (*arkscript.PolicyTemplate,
	error) {

	raw, err := r.EffectivePolicyTemplate()
	if err != nil {
		return nil, err
	}

	return arkscript.DecodePolicyTemplate(raw)
}

// DecodeStandardPolicyTemplate decodes the semantic boarding policy and
// extracts the standard Ark boarding parameters.
func (r *BoardingRequest) DecodeStandardPolicyTemplate() (
	*arkscript.StandardVTXOParams, error) {

	template, err := r.DecodePolicyTemplate()
	if err != nil {
		return nil, err
	}

	return arkscript.DecodeStandardVTXOParams(template)
}

// EffectivePolicyTemplate returns the semantic VTXO policy.
func (r *VTXORequest) EffectivePolicyTemplate() ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("vtxo request must be provided")
	}

	if len(r.PolicyTemplate) == 0 {
		return nil, fmt.Errorf("vtxo request policy template must be " +
			"provided")
	}

	return bytes.Clone(r.PolicyTemplate), nil
}

// DecodePolicyTemplate decodes the semantic VTXO policy.
func (r *VTXORequest) DecodePolicyTemplate() (*arkscript.PolicyTemplate,
	error) {

	raw, err := r.EffectivePolicyTemplate()
	if err != nil {
		return nil, err
	}

	return arkscript.DecodePolicyTemplate(raw)
}

// DecodeStandardPolicyTemplate decodes the semantic VTXO policy and extracts
// the standard Ark VTXO parameters when the request uses the default output
// shape.
func (r *VTXORequest) DecodeStandardPolicyTemplate() (
	*arkscript.StandardVTXOParams, error) {

	template, err := r.DecodePolicyTemplate()
	if err != nil {
		return nil, err
	}

	return arkscript.DecodeStandardVTXOParams(template)
}

// EffectivePkScript returns the requested output script derived from the
// semantic policy.
func (r *VTXORequest) EffectivePkScript() ([]byte, error) {
	template, err := r.DecodePolicyTemplate()
	if err != nil {
		return nil, err
	}

	return template.PkScript()
}
