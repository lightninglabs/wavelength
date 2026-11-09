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
			codes.FailedPrecondition,
			"asset deposits require assets.backend=tapd",
		)
	}
	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	deposit, err := deposits.Deposit(ctx, req)
	if err != nil {
		return nil, err
	}

	return assetDepositToRPC(deposit, ""), nil
}

// BoardAssetDeposit joins the next round with a confirmed asset deposit.
func (r *RPCServer) BoardAssetDeposit(ctx context.Context,
	req *waverpc.BoardAssetDepositRequest) (*waverpc.AssetDeposit, error) {

	deposits := r.server.assetDeposits
	if deposits == nil {
		return nil, status.Error(
			codes.FailedPrecondition,
			"asset deposits require assets.backend=tapd",
		)
	}
	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	deposit, err := deposits.Board(ctx, req.GetRequestId())
	if err != nil {
		return nil, err
	}

	return assetDepositToRPC(deposit, ""), nil
}

// ClaimAssetDeposit claims a confirmed asset deposit that no round holds back
// to the tapd wallet through the owner's exit path.
func (r *RPCServer) ClaimAssetDeposit(ctx context.Context,
	req *waverpc.ClaimAssetDepositRequest) (*waverpc.AssetDeposit, error) {

	deposits := r.server.assetDeposits
	if deposits == nil {
		return nil, status.Error(
			codes.FailedPrecondition,
			"asset deposits require assets.backend=tapd",
		)
	}
	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	deposit, err := deposits.Claim(ctx, req.GetRequestId())
	if err != nil {
		return nil, err
	}
	claim, err := deposits.ClaimOf(ctx, req.GetRequestId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load asset deposit "+
			"claim: %v", err)
	}

	return applyAssetDepositClaim(assetDepositToRPC(deposit, ""), claim),
		nil
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
		roundID, err := deposits.Round(ctx, deposit.Request.RequestID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "load asset "+
				"deposit round: %v", err)
		}
		claim, err := deposits.ClaimOf(ctx, deposit.Request.RequestID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "load asset "+
				"deposit claim: %v", err)
		}
		resp.Deposits = append(
			resp.Deposits,
			applyAssetDepositClaim(
				assetDepositToRPC(deposit, roundID), claim,
			),
		)
	}

	return resp, nil
}
