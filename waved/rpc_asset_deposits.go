package waved

import (
	"context"

	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// DepositAsset funds a boarding output holding a Taproot Asset from the tapd
// wallet, or resumes the deposit with the same request ID.
func (r *RPCServer) DepositAsset(ctx context.Context,
	req *waverpc.DepositAssetRequest) (*waverpc.AssetDeposit, error) {

	deposits := r.server.assetDeposits
	if deposits == nil {
		return nil, status.Error(
			codes.FailedPrecondition, "asset deposits require "+
				"assets.backend=tapd and assets.lnd.host",
		)
	}
	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	deposit, err := deposits.Deposit(ctx, req)
	if err != nil {
		return nil, err
	}

	return assetDepositToRPC(deposit), nil
}

// ListAssetDeposits returns the asset deposits of this daemon.
func (r *RPCServer) ListAssetDeposits(ctx context.Context,
	_ *waverpc.ListAssetDepositsRequest) (
	*waverpc.ListAssetDepositsResponse, error) {

	resp := &waverpc.ListAssetDepositsResponse{}
	deposits := r.server.assetDeposits
	if deposits == nil {
		return resp, nil
	}

	stored, err := deposits.List(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list asset "+
			"deposits: %v", err)
	}
	for _, deposit := range stored {
		resp.Deposits = append(
			resp.Deposits, assetDepositToRPC(deposit),
		)
	}

	return resp, nil
}
