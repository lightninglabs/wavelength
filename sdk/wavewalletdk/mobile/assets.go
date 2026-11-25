//go:build mobile && wavewalletrpc && swapruntime

package mobile

import (
	"context"
	"time"

	"github.com/lightninglabs/wavelength/sdk/wavewalletdk"
)

// assetWriteTimeout bounds an asset call that fetches or verifies proofs, or
// funds and publishes a transaction.
const assetWriteTimeout = 2 * time.Minute

// NewAssetReceiveKeys derives the keys a sender transfers a Taproot Asset to.
// The response is wavewalletdk.AssetReceiveKeys.
func NewAssetReceiveKeys() ([]byte, error) {
	client, parentCtx, err := activeClient()
	if err != nil {
		return nil, err
	}
	ctx, cancel := readContext(parentCtx)
	defer cancel()

	keys, err := client.NewAssetReceiveKeys(ctx)
	if err != nil {
		return nil, err
	}

	return marshal(keys)
}

// NewAssetFundingAddress returns a new backing-wallet address whose coins pay
// for asset deposits. The response is wavewalletdk.AssetFundingAddress.
func NewAssetFundingAddress() ([]byte, error) {
	client, parentCtx, err := activeClient()
	if err != nil {
		return nil, err
	}
	ctx, cancel := readContext(parentCtx)
	defer cancel()

	addr, err := client.NewAssetFundingAddress(ctx)
	if err != nil {
		return nil, err
	}

	return marshal(addr)
}

// ImportAssetProof adds a received asset to the asset wallet. reqJSON decodes
// to wavewalletdk.ImportAssetProofRequest; the response is
// wavewalletdk.OwnedAsset.
func ImportAssetProof(reqJSON []byte) ([]byte, error) {
	client, parentCtx, err := activeClient()
	if err != nil {
		return nil, err
	}

	var req wavewalletdk.ImportAssetProofRequest
	if err := decode(reqJSON, &req); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(parentCtx, assetWriteTimeout)
	defer cancel()

	held, err := client.ImportAssetProof(ctx, req)
	if err != nil {
		return nil, err
	}

	return marshal(held)
}

// ListOwnedAssets returns the assets the asset wallet holds on chain, as a
// JSON array of wavewalletdk.OwnedAsset.
func ListOwnedAssets() ([]byte, error) {
	client, parentCtx, err := activeClient()
	if err != nil {
		return nil, err
	}
	ctx, cancel := readContext(parentCtx)
	defer cancel()

	assets, err := client.ListOwnedAssets(ctx)
	if err != nil {
		return nil, err
	}

	return marshal(assets)
}

// NewAssetAddress returns the wallet's Taproot Asset address for an asset,
// which it creates on the first call. reqJSON decodes to
// wavewalletdk.NewAssetAddressRequest; the response is
// wavewalletdk.AssetAddress.
func NewAssetAddress(reqJSON []byte) ([]byte, error) {
	client, parentCtx, err := activeClient()
	if err != nil {
		return nil, err
	}

	var req wavewalletdk.NewAssetAddressRequest
	if err := decode(reqJSON, &req); err != nil {
		return nil, err
	}

	ctx, cancel := readContext(parentCtx)
	defer cancel()

	addr, err := client.NewAssetAddress(ctx, req)
	if err != nil {
		return nil, err
	}

	return marshal(addr)
}

// ListAssetAddresses returns the wallet's Taproot Asset addresses, as a JSON
// array of wavewalletdk.AssetAddress.
func ListAssetAddresses() ([]byte, error) {
	client, parentCtx, err := activeClient()
	if err != nil {
		return nil, err
	}
	ctx, cancel := readContext(parentCtx)
	defer cancel()

	addrs, err := client.ListAssetAddresses(ctx)
	if err != nil {
		return nil, err
	}

	return marshal(addrs)
}

// ListAssetReceives returns the transfers sent to the wallet's Taproot
// Asset addresses, as a JSON array of wavewalletdk.AssetReceive.
func ListAssetReceives() ([]byte, error) {
	client, parentCtx, err := activeClient()
	if err != nil {
		return nil, err
	}
	ctx, cancel := readContext(parentCtx)
	defer cancel()

	receives, err := client.ListAssetReceives(ctx)
	if err != nil {
		return nil, err
	}

	return marshal(receives)
}

// SyncAssetReceives reads the proof courier mailbox of every address once
// and works once on every unfinished receive. An app calls it when it
// returns to the foreground, where no mailbox connection survived.
func SyncAssetReceives() error {
	client, parentCtx, err := activeClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(parentCtx, assetWriteTimeout)
	defer cancel()

	return client.SyncAssetReceives(ctx)
}

// DepositAsset funds a boarding output holding a Taproot Asset. reqJSON
// decodes to wavewalletdk.DepositAssetRequest; the response is
// wavewalletdk.AssetDeposit. A retry with the same RequestID resumes the
// deposit.
func DepositAsset(reqJSON []byte) ([]byte, error) {
	client, parentCtx, err := activeClient()
	if err != nil {
		return nil, err
	}

	var req wavewalletdk.DepositAssetRequest
	if err := decode(reqJSON, &req); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(parentCtx, assetWriteTimeout)
	defer cancel()

	deposit, err := client.DepositAsset(ctx, req)
	if err != nil {
		return nil, err
	}

	return marshal(deposit)
}

// ListAssetDeposits returns the wallet's asset deposits, as a JSON array of
// wavewalletdk.AssetDeposit.
func ListAssetDeposits() ([]byte, error) {
	client, parentCtx, err := activeClient()
	if err != nil {
		return nil, err
	}
	ctx, cancel := readContext(parentCtx)
	defer cancel()

	deposits, err := client.ListAssetDeposits(ctx)
	if err != nil {
		return nil, err
	}

	return marshal(deposits)
}

// BoardAssetDeposit joins the next round with a confirmed asset deposit.
// reqJSON decodes to wavewalletdk.BoardAssetDepositRequest; the response is
// wavewalletdk.AssetDeposit.
func BoardAssetDeposit(reqJSON []byte) ([]byte, error) {
	client, parentCtx, err := activeClient()
	if err != nil {
		return nil, err
	}

	var req wavewalletdk.BoardAssetDepositRequest
	if err := decode(reqJSON, &req); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(parentCtx, assetWriteTimeout)
	defer cancel()

	deposit, err := client.BoardAssetDeposit(ctx, req)
	if err != nil {
		return nil, err
	}

	return marshal(deposit)
}
