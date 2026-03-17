package darepod

import (
	"context"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/unroller"
	"github.com/lightninglabs/darepo-client/vtxo"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// FundingAddress returns a plain on-chain P2TR address from the
// internal wallet for receiving fee-funding UTXOs.
func (r *RPCServer) FundingAddress(ctx context.Context,
	_ *daemonrpc.FundingAddressRequest) (
	*daemonrpc.FundingAddressResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	if !r.server.lwWallet.IsSome() {
		return nil, status.Errorf(codes.FailedPrecondition,
			"funding address only available in lwwallet mode")
	}

	addr, err := r.server.lwWallet.UnsafeFromSome().NewAddress(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"generate address: %v", err)
	}

	return &daemonrpc.FundingAddressResponse{
		Address: addr.String(),
	}, nil
}

// ExitVTXO initiates unilateral exit for the specified VTXOs by
// broadcasting their presigned transaction trees on-chain.
func (r *RPCServer) ExitVTXO(ctx context.Context,
	req *daemonrpc.ExitVTXORequest) (
	*daemonrpc.ExitVTXOResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	if len(req.Outpoints) == 0 {
		return nil, status.Errorf(codes.InvalidArgument,
			"at least one outpoint is required")
	}

	// Parse outpoints.
	outpoints := make([]wire.OutPoint, 0, len(req.Outpoints))
	for _, opStr := range req.Outpoints {
		op, err := parseOutpointString(opStr)
		if err != nil {
			return nil, status.Errorf(
				codes.InvalidArgument,
				"invalid outpoint %q: %v", opStr, err,
			)
		}
		outpoints = append(outpoints, op)
	}

	// Validate that all VTXOs are in a live state before
	// sending to the unroller. This prevents attempting to
	// unroll VTXOs that are already spent, forfeited, or
	// in another non-live state.
	if r.server.vtxoStore != nil {
		for _, op := range outpoints {
			desc, err := r.server.vtxoStore.GetVTXO(
				ctx, op,
			)
			if err != nil {
				return nil, status.Errorf(
					codes.NotFound,
					"VTXO %s not found: %v",
					op, err,
				)
			}
			if desc.Status != vtxo.VTXOStatusLive {
				return nil, status.Errorf(
					codes.FailedPrecondition,
					"VTXO %s is %s, not live",
					op, desc.Status,
				)
			}
		}
	}

	// Send unroll request to the unroller actor.
	if !r.server.unrollerRef.IsSome() {
		return nil, status.Errorf(codes.FailedPrecondition,
			"unroller not available")
	}
	unrollerRef := r.server.unrollerRef.UnsafeFromSome()

	unrollReq := &unroller.UnrollRequest{
		TargetVTXOs: outpoints,
	}

	result := unrollerRef.Ask(ctx, unrollReq).Await(ctx)
	if _, err := result.Unpack(); err != nil {
		return nil, status.Errorf(codes.Internal,
			"unroll failed: %v", err)
	}

	// StartedCount reflects the number of outpoints requested.
	// Duplicates are silently skipped by the unroller, so this
	// may overcount actual new unrolls initiated.
	return &daemonrpc.ExitVTXOResponse{
		Status:       "started",
		StartedCount: int32(len(outpoints)),
	}, nil
}
