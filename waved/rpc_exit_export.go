package waved

import (
	"context"
	"errors"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/recovery"
	"github.com/lightninglabs/wavelength/unroll"
	"github.com/lightninglabs/wavelength/vtxo"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ExitExportRequest selects ancestry and optionally authorizes a final sweep
// paying SweepAddress. An empty address exports ancestry without signing.
type ExitExportRequest struct {
	Outpoint     string
	SweepAddress string
}

// ExitExport holds chain identity alongside independently verifiable ancestry.
type ExitExport struct {
	Network      string
	GenesisHash  string
	Ancestry     *unroll.SignedAncestry
	SweepAddress string
	FinalSweep   *wire.MsgTx
}

// ExportExit reads signed ancestry from local storage without admitting an
// unroll job or inspecting the backing wallet's fee inputs. External executors
// supply their own CPFP funding. A destination opts into signing a final sweep
// without waiting for maturity, estimating fees, or selecting wallet inputs.
func (r *RPCServer) ExportExit(ctx context.Context, req *ExitExportRequest) (
	*ExitExport, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, status.Error(
			codes.InvalidArgument, "request is nil",
		)
	}
	target, err := parseOutpointString(req.Outpoint)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid "+
			"outpoint: %v", err)
	}
	if r.server.vtxoStore == nil || r.server.chainParams == nil {
		return nil, status.Error(
			codes.Unavailable,
			"VTXO store or network not initialized",
		)
	}

	resolver := &unroll.DescriptorLineageResolver{
		VTXOStore: r.server.vtxoStore,
	}
	// Avoid assigning a typed nil to the resolver's interface field.
	if artifacts := r.newLocalOORArtifactStore(); artifacts != nil {
		resolver.ArtifactStore = artifacts
	}
	material, err := resolver.ResolveLineage(ctx, target)
	if err != nil {
		code := codes.FailedPrecondition
		if errors.Is(err, unroll.ErrUnrollTargetNotFound) {
			code = codes.NotFound
		}

		return nil, status.Errorf(code, "resolve exit ancestry: %v",
			err)
	}
	ancestry, err := unroll.BuildSignedAncestry(material)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "export "+
			"signed ancestry: %v", err)
	}

	result := &ExitExport{
		Network:     r.server.chainParams.Name,
		GenesisHash: r.server.chainParams.GenesisHash.String(),
		Ancestry:    ancestry,
	}
	if req.SweepAddress != "" {
		result.FinalSweep, err = r.presignExportSweep(
			ctx, ancestry.Proof, req.SweepAddress,
		)
		if err != nil {
			return nil, err
		}
		result.SweepAddress = req.SweepAddress
	}

	return result, nil
}

// presignExportSweep binds a user destination to a signed timeout spend. It
// never derives a wallet destination or funds any fee input.
func (r *RPCServer) presignExportSweep(ctx context.Context,
	proof *recovery.Proof, address string) (*wire.MsgTx, error) {

	destination, err := btcaddr.DecodeAddress(address, r.server.chainParams)
	if err != nil || !destination.IsForNet(r.server.chainParams) {
		return nil, status.Error(
			codes.InvalidArgument, "sweep address is invalid "+
				"or belongs to another network",
		)
	}
	script, err := txscript.PayToAddrScript(destination)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "sweep "+
			"destination script: %v", err)
	}
	desc, err := r.server.vtxoStore.GetVTXO(ctx, proof.TargetOutpoint())
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "load "+
			"sweep descriptor: %v", err)
	}
	if desc.TaprootAssetRoot != nil {
		return nil, status.Error(
			codes.FailedPrecondition,
			vtxo.ErrAssetVTXORequiresTransition.Error(),
		)
	}
	if r.server.cfg == nil || r.server.cfg.Wallet == nil {
		return nil, status.Error(
			codes.Unavailable, "sweep signer is not initialized",
		)
	}
	signer, err := r.server.newSweepWallet()
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "sweep signer: %v",
			err)
	}
	sweep, err := unroll.BuildSignedExitSweep(desc, proof, script, signer)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "presign "+
			"exit sweep: %v", err)
	}

	return sweep, nil
}
