package types

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
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
// semantic policy. Note that the client builds round-output templates with
// the operator key left as an unbound placeholder, so this returns the
// placeholder-derived script; use BoundPkScript when the concrete
// operator-bound script is required (quote echo check, co-sign validation,
// persistence).
func (r *VTXORequest) EffectivePkScript() ([]byte, error) {
	template, err := r.DecodePolicyTemplate()
	if err != nil {
		return nil, err
	}

	return template.PkScript()
}

// BoundPolicyTemplate decodes the request's placeholder policy template,
// binds operatorKey into every operator-key placeholder, and returns the
// resulting concrete template. The client builds round outputs with the
// unbound operator-key placeholder; the server binds its current operator
// key at admission and tells the client which key it used via the quote.
// The client uses this to re-derive the concrete output it must match
// against the server-built tree leaves and to persist the concrete bound
// template on its VTXO records.
func (r *VTXORequest) BoundPolicyTemplate(operatorKey *btcec.PublicKey) (
	*arkscript.PolicyTemplate, error) {

	template, err := r.DecodePolicyTemplate()
	if err != nil {
		return nil, err
	}

	return template.BindOperatorKey(operatorKey)
}

// BoundPkScript decodes the request's placeholder policy template, binds
// operatorKey into it, and returns the resulting concrete P2TR output
// script. This is the script the client byte-matches against the
// server-built tree leaf for this VTXO at co-signing time.
func (r *VTXORequest) BoundPkScript(operatorKey *btcec.PublicKey) ([]byte,
	error) {

	bound, err := r.BoundPolicyTemplate(operatorKey)
	if err != nil {
		return nil, err
	}

	return bound.PkScript()
}
