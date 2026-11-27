package waved

import (
	"context"
	"sort"

	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// assetWallet returns the SDK wallet of the connected asset backend.
func (r *RPCServer) assetWallet() (*tapsdk.Wallet, error) {
	client := r.server.assetClient
	if client == nil {
		return nil, status.Error(
			codes.FailedPrecondition,
			"the asset wallet requires an asset backend",
		)
	}
	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	return tapsdk.NewWallet(
		client, tapsdk.Network(r.server.cfg.Network),
	), nil
}

// NewAssetFundingAddress returns a new address of the daemon's backing
// wallet, whose coins pay for asset deposits.
func (r *RPCServer) NewAssetFundingAddress(ctx context.Context,
	_ *waverpc.NewAssetFundingAddressRequest) (*waverpc.AssetFundingAddress,
	error) {

	if _, err := r.assetWallet(); err != nil {
		return nil, err
	}

	addr, err := r.server.NewWalletAddress(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "derive asset "+
			"funding address: %v", err)
	}

	return &waverpc.AssetFundingAddress{Address: addr}, nil
}

// ListOwnedAssets returns the assets the daemon's asset wallet holds on
// chain.
func (r *RPCServer) ListOwnedAssets(ctx context.Context,
	_ *waverpc.ListOwnedAssetsRequest) (*waverpc.ListOwnedAssetsResponse,
	error) {

	resp := &waverpc.ListOwnedAssetsResponse{}
	if r.server.assetClient == nil {
		return resp, nil
	}

	wallet, err := r.assetWallet()
	if err != nil {
		return nil, err
	}

	utxos, err := wallet.Client().ListUtxos(
		ctx, &tapsdk.ListUtxosRequest{},
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list owned "+
			"assets: %v", err)
	}

	for _, utxo := range utxos {
		if utxo == nil {
			continue
		}
		for _, held := range utxo.Assets {
			if held == nil {
				continue
			}
			issuanceID := held.Genesis.IssuanceID
			resp.Assets = append(resp.Assets, &waverpc.OwnedAsset{
				AssetRef:       held.AssetRef.String(),
				AssetId:        issuanceID.String(),
				Amount:         held.Amount,
				Outpoint:       utxo.OutPoint.String(),
				ScriptKey:      held.ScriptKey.PubKey[:],
				AnchorValueSat: utxo.AmtSat,
			})
		}
	}
	sort.Slice(resp.Assets, func(i, j int) bool {
		a, b := resp.Assets[i], resp.Assets[j]
		if a.Outpoint != b.Outpoint {
			return a.Outpoint < b.Outpoint
		}

		return a.AssetId < b.AssetId
	})

	return resp, nil
}
