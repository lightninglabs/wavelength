package waved

import (
	"context"
	"errors"
	"fmt"
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

// NewAssetReceiveKeys derives the script key and anchor internal key a
// sender needs to transfer an asset to the daemon's asset wallet.
func (r *RPCServer) NewAssetReceiveKeys(ctx context.Context,
	_ *waverpc.NewAssetReceiveKeysRequest) (*waverpc.AssetReceiveKeys,
	error) {

	wallet, err := r.assetWallet()
	if err != nil {
		return nil, err
	}

	keys, err := wallet.DeriveKeys(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "derive asset "+
			"receive keys: %v", err)
	}

	return &waverpc.AssetReceiveKeys{
		ScriptKey:         keys.ScriptKey.PubKey[:],
		RawScriptKey:      keys.ScriptKey.KeyDesc.RawKeyBytes[:],
		AnchorInternalKey: keys.InternalKey.PubKey[:],
	}, nil
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

// ImportAssetProof verifies a received asset's proof file and adds the asset
// to the daemon's asset wallet.
func (r *RPCServer) ImportAssetProof(ctx context.Context,
	req *waverpc.ImportAssetProofRequest) (*waverpc.OwnedAsset, error) {

	if len(req.GetProofFile()) == 0 {
		return nil, status.Error(
			codes.InvalidArgument, "proof_file is required",
		)
	}
	ref, err := tapsdk.ParseAssetRef(req.GetAssetRef())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument,
			"asset_ref: %v", err)
	}

	wallet, err := r.assetWallet()
	if err != nil {
		return nil, err
	}

	// Naming the asset is how the caller accepts its group.
	if engine := r.server.embeddedAssets; engine != nil {
		if groupKey, ok := ref.GroupKey(); ok {
			engine.TrustGroupKey(groupKey)
		}
	}

	// The proof must be of the asset the caller named before anything is
	// stored.
	tip, err := decodeAssetProofTip(
		ctx, wallet.Client(), req.GetProofFile(),
	)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "asset "+
			"proof: %v", err)
	}
	if !tip.AssetRef.Equivalent(ref) {
		return nil, status.Errorf(codes.InvalidArgument, "asset proof "+
			"is of %s, not %s", tip.AssetRef, ref)
	}

	// The engine holds the proofs of a file before it verifies them, so
	// it is given only a file that verifies. tapd verifies each proof
	// as the import inserts it, and can verify a transfer of a grouped
	// asset only once the import has inserted the group's issuance.
	if r.server.embeddedAssets != nil {
		verified, err := wallet.Client().VerifyProof(
			ctx, req.GetProofFile(),
		)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "verify "+
				"asset proof: %v", err)
		}
		if verified == nil || !verified.Valid {
			return nil, status.Error(
				codes.InvalidArgument,
				"asset proof is not valid",
			)
		}
	}

	_, err = wallet.ImportProofFile(ctx, &tapsdk.ProofFile{
		RawProofFile: req.GetProofFile(),
	})
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "import "+
			"asset proof: %v", err)
	}

	owned := &waverpc.OwnedAsset{
		AssetRef:  tip.AssetRef.String(),
		AssetId:   tip.IssuanceID.String(),
		Amount:    tip.Amount,
		Outpoint:  tip.Outpoint.String(),
		ScriptKey: tip.ScriptKey[:],
	}

	// The anchor's Bitcoin value is a property of the wallet's output,
	// not of the proof's asset.
	utxos, err := wallet.Client().ListUtxos(
		ctx, &tapsdk.ListUtxosRequest{},
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list owned "+
			"assets: %v", err)
	}
	if utxo := utxos[owned.Outpoint]; utxo != nil {
		owned.AnchorValueSat = utxo.AmtSat
	}

	return owned, nil
}

// decodeAssetProofTip decodes the last proof of a proof file, which is the
// proof of the asset output the file transfers. It verifies nothing.
func decodeAssetProofTip(ctx context.Context, client tapsdk.ProofClient,
	proofFile []byte) (*tapsdk.DecodedProof, error) {

	proofs, err := client.UnpackProofFile(ctx, proofFile)
	if err != nil {
		return nil, fmt.Errorf("unpack proof file: %w", err)
	}
	if len(proofs) == 0 {
		return nil, errors.New("proof file holds no proof")
	}

	tip, err := client.DecodeProof(ctx, proofs[len(proofs)-1])
	if err != nil {
		return nil, fmt.Errorf("decode proof: %w", err)
	}
	if tip == nil {
		return nil, errors.New("proof file holds no proof")
	}

	return tip, nil
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
