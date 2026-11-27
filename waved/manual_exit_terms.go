package waved

import (
	"context"
	"errors"
	"strings"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/txconfirm"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ManualExitTerms are the user-selected final payout and initial fee rate.
type ManualExitTerms struct {
	SweepAddress       string
	FeeRateSatPerVByte int64
}

// validateManualExitTerms validates the rate and turns a network address into
// the exact script that will be persisted with the VTXO.
func (r *RPCServer) validateManualExitTerms(terms ManualExitTerms) (string,
	*actormsg.ManualExitOptions, error) {

	address := strings.TrimSpace(terms.SweepAddress)
	rate := terms.FeeRateSatPerVByte
	if address == "" && rate == 0 {
		return "", nil, nil
	}

	maxRate := r.server.unrollMaxFeeRate()
	if maxRate == 0 {
		maxRate = txconfirm.DefaultMaxFeeRateSatPerVByte
	}
	if rate < 0 || rate > maxRate {
		return "", nil, status.Errorf(codes.InvalidArgument, "fee "+
			"rate must be between 1 and %d sat/vB, or 0 for "+
			"automatic", maxRate)
	}
	options := &actormsg.ManualExitOptions{FeeRateSatPerVByte: rate}
	if address == "" {
		return "", options, nil
	}
	if r.server.chainParams == nil {
		return "", nil, status.Error(
			codes.Unavailable, "wallet network not initialized",
		)
	}
	destination, err := btcaddr.DecodeAddress(address, r.server.chainParams)
	if err != nil || !destination.IsForNet(r.server.chainParams) {
		return "", nil, status.Error(
			codes.InvalidArgument,
			"Enter a valid Bitcoin address for this network.",
		)
	}

	script, err := txscript.PayToAddrScript(destination)
	if err != nil || len(script) > 34 {
		return "", nil, status.Error(
			codes.InvalidArgument,
			"Use a standard Bitcoin address for the exit.",
		)
	}
	options.SweepScript = script

	return destination.EncodeAddress(), options, nil
}

// validateManualExitTarget limits overrides to standard Bitcoin timeout jobs.
// Specialized recovery paths may require different signing and payout rules.
func validateManualExitTarget(desc *vtxo.Descriptor,
	options *actormsg.ManualExitOptions) error {

	if options == nil {
		return nil
	}
	if desc == nil {
		return status.Error(
			codes.NotFound, "Exit funds were not found.",
		)
	}
	template, err := arkscript.DecodePolicyTemplate(desc.PolicyTemplate)
	if err != nil || !arkscript.IsStandardVTXOTemplate(template) ||
		desc.TaprootAssetRoot != nil {
		return status.Error(
			codes.FailedPrecondition,
			"Custom exit options require a standard bitcoin VTXO.",
		)
	}

	return nil
}

// stampExitSweepAddress reads the chosen payout from the exit's durable VTXO.
func (r *RPCServer) stampExitSweepAddress(ctx context.Context,
	resp *waverpc.GetUnrollStatusResponse, outpoint wire.OutPoint) error {

	if r.server.vtxoStore == nil || r.server.chainParams == nil {
		return nil
	}
	desc, err := r.server.vtxoStore.GetVTXO(ctx, outpoint)
	if errors.Is(err, vtxo.ErrVTXONotFound) {
		return nil
	}
	if err != nil {
		return status.Errorf(codes.Unavailable, "read exit payout: %v",
			err)
	}
	if desc == nil || desc.ManualExitOptions == nil ||
		len(desc.ManualExitOptions.SweepScript) == 0 {
		return nil
	}

	_, addresses, _, err := txscript.ExtractPkScriptAddrs(
		desc.ManualExitOptions.SweepScript, r.server.chainParams,
	)
	if err != nil || len(addresses) != 1 {
		return status.Error(
			codes.Internal, "cannot decode persisted exit payout",
		)
	}
	resp.SweepAddress = addresses[0].EncodeAddress()

	return nil
}
