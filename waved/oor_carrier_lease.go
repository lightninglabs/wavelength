package waved

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/oor"
	"github.com/lightninglabs/wavelength/vtxo"
)

// leaseOORCarrier reserves one operator carrier-float VTXO covering
// requiredSat over the same direct ArkService connection GetInfo and
// EstimateFee use. The response is bound locally before anything spends it:
// the returned policy must compile to the returned pkScript, contain the
// current operator collab key, and be owned by the operator's advertised
// carrier float key.
func (s *Server) leaseOORCarrier(ctx context.Context,
	terms *types.OperatorTerms, requiredSat btcutil.Amount) (
	*oor.OORCarrierLease, error) {

	if terms == nil || terms.OORCarrierPubKey == nil {
		return nil, fmt.Errorf("operator does not fund OOR carriers")
	}
	if requiredSat <= 0 {
		return nil, fmt.Errorf("required carrier value must be " +
			"positive")
	}

	client := s.operatorArkClient()
	if client == nil {
		return nil, fmt.Errorf("operator connection not initialized")
	}

	resp, err := client.LeaseOORCarrier(ctx, &arkrpc.LeaseOORCarrierRequest{
		RequiredSat: int64(requiredSat),
	})
	if err != nil {
		return nil, fmt.Errorf("LeaseOORCarrier RPC: %w", err)
	}

	outpoint, err := parseOutpointString(resp.GetOutpoint())
	if err != nil {
		return nil, fmt.Errorf("parse leased float outpoint %q: %w",
			resp.GetOutpoint(), err)
	}

	lease := &oor.OORCarrierLease{
		Outpoint:       outpoint,
		Value:          btcutil.Amount(resp.GetValueSat()),
		PolicyTemplate: bytes.Clone(resp.GetVtxoPolicyTemplate()),
		PkScript:       bytes.Clone(resp.GetPkScript()),
		ExpiresAtUnix:  resp.GetExpiresAtUnix(),
	}
	if err := lease.Validate(); err != nil {
		return nil, err
	}
	if lease.Value < requiredSat {
		return nil, fmt.Errorf("leased float value %d sat is below "+
			"required %d sat", lease.Value, requiredSat)
	}

	return lease, nil
}

// BuildOperatorFundedTransferInput constructs the foreign transfer input for
// a leased operator carrier-float VTXO. It mirrors the custom-input
// descriptor construction, but stamps no local client key: the float owner is
// the operator's advertised carrier key and the operator signs both legs.
func BuildOperatorFundedTransferInput(lease *oor.OORCarrierLease,
	floatKey, operatorKey *btcec.PublicKey) (oor.TransferInput, error) {

	if err := lease.Validate(); err != nil {
		return oor.TransferInput{}, err
	}
	if floatKey == nil || operatorKey == nil {
		return oor.TransferInput{}, fmt.Errorf("carrier float and " +
			"operator keys are required")
	}

	template, err := arkscript.DecodePolicyTemplate(lease.PolicyTemplate)
	if err != nil {
		return oor.TransferInput{}, fmt.Errorf("decode float "+
			"policy: %w", err)
	}

	// Bind the semantic policy to the leased output script before any
	// value is planned against it, exactly like custom inputs do.
	if !template.MatchesPkScript(lease.PkScript) {
		return oor.TransferInput{}, fmt.Errorf("float policy does " +
			"not match the leased pkScript")
	}

	nodes := make([]arkscript.Node, len(template.Leaves))
	for i, leaf := range template.Leaves {
		nodes[i] = leaf.Node
	}
	err = arkscript.ValidatePolicy(nodes, arkscript.PolicyValidationOpts{
		OperatorKey: operatorKey,
	})
	if err != nil {
		return oor.TransferInput{}, fmt.Errorf("invalid float "+
			"policy: %w", err)
	}

	params, err := arkscript.DecodeStandardVTXOParams(template)
	if err != nil {
		return oor.TransferInput{}, fmt.Errorf("decode float policy "+
			"params: %w", err)
	}

	// The float must be owned by the advertised carrier key: anything
	// else is not the operator's money and must not be spent unsigned.
	if !bytes.Equal(
		schnorr.SerializePubKey(params.OwnerKey),
		schnorr.SerializePubKey(floatKey),
	) {
		return oor.TransferInput{}, fmt.Errorf("float policy owner " +
			"is not the advertised carrier key")
	}

	// The owner leaf is the float policy's own collab leaf; owner-leaf
	// normalization deliberately skips operator-funded inputs.
	ownerLeafPolicy, err := arkscript.LeafTemplate{
		Node: &arkscript.Multisig{
			Keys: []*btcec.PublicKey{
				params.OwnerKey,
				params.OperatorKey,
			},
		},
	}.Encode()
	if err != nil {
		return oor.TransferInput{}, fmt.Errorf("encode float owner "+
			"leaf: %w", err)
	}

	input := oor.TransferInput{
		VTXO: &vtxo.Descriptor{
			Outpoint:       lease.Outpoint,
			Amount:         lease.Value,
			PolicyTemplate: bytes.Clone(lease.PolicyTemplate),
			PkScript:       bytes.Clone(lease.PkScript),
			OperatorKey:    params.OperatorKey,
			RelativeExpiry: params.ExitDelay,
		},
		VTXOPolicyTemplate: bytes.Clone(lease.PolicyTemplate),
		OwnerLeafPolicy:    ownerLeafPolicy,
		OperatorFunded:     true,
	}
	if err := input.Validate(); err != nil {
		return oor.TransferInput{}, err
	}

	return input, nil
}
