package harness

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/darepo-client/assets"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightninglabs/taproot-assets/taprpc/mintrpc"
	"github.com/lightninglabs/taproot-assets/taprpc/universerpc"
	"github.com/lightninglabs/taproot-assets/universe"
	"github.com/stretchr/testify/require"
)

// TapClientHarness provides convenience methods for testing tapd operations.
type TapClientHarness struct {
	*assets.TapdClient

	name string
	h    *Harness
	t    *testing.T
}

// NewTapClientHarness creates a new TapClientHarness.
func NewTapClientHarness(h *Harness, name string, host string, tlsPath string,
	macPath string) *TapClientHarness {

	h.T.Helper()

	clientCfg := &assets.TapdConfig{
		Host:         host,
		TLSPath:      tlsPath,
		MacaroonPath: macPath,
	}

	client, err := assets.NewTapdClient(clientCfg)
	require.NoError(h.T, err, "failed to create tapd client")

	return &TapClientHarness{
		TapdClient: client,
		name:       name,
		h:          h,
		t:          h.T,
	}
}

// MintAsset mints a new asset with the given parameters and returns the minted
// asset information.
func (tc *TapClientHarness) MintAsset(name string, amount uint64,
	assetType taprpc.AssetType) *taprpc.Asset {

	tc.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()

	// Create mint request.
	mintReq := &mintrpc.MintAssetRequest{
		Asset: &mintrpc.MintAsset{
			AssetVersion: taprpc.AssetVersion_ASSET_VERSION_V1,
			AssetType:    assetType,
			Name:         name,
			Amount:       amount,
			AssetMeta: &taprpc.AssetMeta{
				Data: []byte(fmt.Sprintf("%s metadata", name)),
				Type: taprpc.AssetMetaType_META_TYPE_OPAQUE,
			},
		},
	}

	// Submit mint request to create a pending batch.
	tc.h.Logf("%s: minting asset %s (amount=%d)", tc.name, name, amount)
	resp, err := tc.TapdClient.MintAsset(ctx, mintReq)
	require.NoError(tc.t, err, "failed to mint asset")
	tc.h.Logf("%s: pending batch created with %d assets", tc.name,
		len(resp.PendingBatch.Assets))

	// Finalize the batch.
	tc.h.Logf("%s: finalizing batch...", tc.name)
	finalizeResp, err := tc.TapdClient.FinalizeBatch(
		ctx, &mintrpc.FinalizeBatchRequest{},
	)
	require.NoError(tc.t, err, "failed to finalize batch")
	tc.h.Logf("%s: batch finalized", tc.name)

	// Mine a block to confirm the batch.
	tc.h.Logf("%s: mining block to confirm batch...", tc.name)
	tc.h.Generate(1)

	// Wait for the batch to be confirmed.
	require.Eventually(tc.t, func() bool {
		batch, err := tc.TapdClient.ListBatches(
			ctx, &mintrpc.ListBatchRequest{
				Filter: &mintrpc.ListBatchRequest_BatchKey{
					BatchKey: finalizeResp.Batch.BatchKey,
				},
			},
		)
		if err != nil {
			return false
		}

		if len(batch.Batches) == 0 {
			return false
		}

		state := batch.Batches[0].Batch.State

		return state == mintrpc.BatchState_BATCH_STATE_FINALIZED
	}, defaultTimeout, pollInterval, "batch not confirmed")

	tc.h.Logf("%s: batch minting %s confirmed", tc.name, name)

	// List the minted asset.
	assets, err := tc.TapdClient.ListAssets(ctx, &taprpc.ListAssetRequest{})
	require.NoError(tc.t, err, "failed to list assets")

	// Find the asset we just minted.
	var mintedAsset *taprpc.Asset
	for _, asset := range assets.Assets {
		if asset.AssetGenesis.Name == name {
			mintedAsset = asset

			break
		}
	}
	require.NotNil(tc.t, mintedAsset, "minted asset not found")

	tc.h.Logf("%s: successfully minted asset %s (id=%x)", tc.name, name,
		mintedAsset.AssetGenesis.AssetId)

	return mintedAsset
}

// NewAddress creates a new address for receiving assets.
func (tc *TapClientHarness) NewAddress(assetID []byte, amount uint64) string {
	tc.h.T.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()

	addr, err := tc.TapdClient.NewAddr(ctx, &taprpc.NewAddrRequest{
		AssetId: assetID,
		Amt:     amount,
	})
	require.NoError(tc.t, err, "failed to create address")

	tc.h.Logf("%s: created address for asset %x (amount=%d): %s",
		tc.name, assetID, amount, addr.Encoded)

	return addr.Encoded
}

// SendAsset sends assets to the given address.
func (tc *TapClientHarness) SendAsset(addr string) *taprpc.SendAssetResponse {
	tc.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()

	tc.h.Logf("%s: sending asset to %s", tc.name, addr)
	resp, err := tc.TapdClient.SendAsset(ctx, &taprpc.SendAssetRequest{
		TapAddrs: []string{addr},
	})
	require.NoError(tc.t, err, "failed to send asset")

	tc.h.Logf("%s: send initiated, transfer txid: %x", tc.name,
		resp.Transfer.AnchorTxHash)

	return resp
}

// WaitForAssetBalance waits until the asset balance matches the expected
// amount.
func (tc *TapClientHarness) WaitForAssetBalance(assetID []byte,
	expectedAmount uint64) {

	tc.t.Helper()

	tc.h.Logf("%s: waiting for asset %x balance to be %d", tc.name,
		assetID, expectedAmount)

	require.Eventually(tc.t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(),
			defaultTimeout)
		defer cancel()

		balance, err := tc.TapdClient.GetAssetBalance(ctx, assetID)
		require.NoError(tc.t, err, "failed to get asset balance")

		return balance == expectedAmount
	}, defaultTimeout, pollInterval,
		"asset balance did not reach expected amount")

	tc.h.Logf("%s: asset %x balance is %d", tc.name, assetID,
		expectedAmount)
}

// SyncUniverse syncs this tapd instance with the harness's main tapd
// universe.
func (tc *TapClientHarness) SyncUniverse() {
	tc.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()

	// Use the universe host suitable for container-to-container
	// communication.
	universeHost := tc.h.TapdUniverseHost()

	tc.h.Logf("%s: adding universe federation server %s", tc.name,
		universeHost)

	// Add federation server.
	_, err := tc.TapdClient.AddFederationServer(
		ctx, &universerpc.AddFederationServerRequest{
			Servers: []*universerpc.UniverseFederationServer{
				{
					Host: universeHost,
				},
			},
		},
	)
	// Ignore duplicate/unusable errors when the peer already has
	// federation state configured.
	if err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(
			msg, universe.ErrDuplicateUniverse.Error(),
		):
			err = nil

		case strings.Contains(
			msg, "cannot add ourselves as a federation member",
		):
			err = nil
		}
	}
	require.NoError(tc.t, err)

	// Now manually trigger a full sync. Retry if sync is already in progress
	// (can happen when multiple clients sync concurrently).
	var syncErr error
	for i := 0; i < 10; i++ {
		_, syncErr = tc.TapdClient.SyncUniverse(
			ctx, &universerpc.SyncRequest{
				UniverseHost: universeHost,
				SyncMode:     universerpc.UniverseSyncMode_SYNC_FULL,
			},
		)
		if syncErr == nil {
			break
		}

		// Retry on "sync is already in progress" error.
		if strings.Contains(syncErr.Error(), "sync is already in progress") {
			time.Sleep(500 * time.Millisecond)
			continue
		}

		// Non-retryable error, bail out.
		break
	}
	require.NoError(tc.t, syncErr)

	tc.h.Logf("%s: synced with universe %s", tc.name, universeHost)
}

// WaitForProofReceived waits until a proof is received for the given address.
func (tc *TapClientHarness) WaitForProofReceived(addr string) {
	tc.t.Helper()

	tc.h.Logf("%s: waiting for proof to be received for address %s...",
		tc.name, addr)

	ctx, cancel := context.WithTimeout(tc.t.Context(), defaultTimeout)
	defer cancel()

	eventsChan, errChan, err := tc.TapdClient.WaitForReceiveComplete(
		ctx, addr, time.Time{},
	)
	require.NoError(tc.t, err, "failed to wait for receive complete")

	select {
	case <-eventsChan:
		tc.h.Logf("%s: proof received for address %s", tc.name, addr)

	case err := <-errChan:
		require.NoError(tc.t, err, "error while waiting for proof")

	case <-ctx.Done():
		tc.t.Fatalf("timeout waiting for proof for address %s", addr)
	}
}

// TransferMatcher is a function that matches an asset transfer from a list of
// transfers. It returns the matching transfer and its output index.
type TransferMatcher func(*testing.T, []*taprpc.AssetTransfer) (
	*taprpc.AssetTransfer, int)

// WaitForTransfer waits until the provided matcher function finds a matching
// transfer in the list of transfers. The matcher function should return the
// matching transfer, its output index, and an error if something went wrong.
func (tc *TapClientHarness) WaitForTransfer(matcher TransferMatcher) (
	*taprpc.AssetTransfer, int) {

	tc.t.Helper()

	var (
		foundTransfer *taprpc.AssetTransfer
		foundIndex    int
	)

	require.Eventually(tc.t, func() bool {
		ctx, cancel := context.WithTimeout(
			context.Background(), defaultTimeout,
		)
		defer cancel()

		transfers, err := tc.TapdClient.ListTransfers(
			ctx, &taprpc.ListTransfersRequest{},
		)
		if err != nil {
			tc.h.Logf("%s: error listing transfers: %v", tc.name,
				err)
			return false
		}
		// Look for matching transfer
		transfer, outIdx := matcher(tc.t, transfers.Transfers)
		if transfer == nil {
			return false
		}

		foundTransfer = transfer
		foundIndex = outIdx

		return true
	}, defaultTimeout, pollInterval, "transfer not confirmed")

	return foundTransfer, foundIndex
}

// PubKey returns the lnd node identity public key backing this tap client.
func (tc *TapClientHarness) PubKey() (*btcec.PublicKey, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()

	info, err := tc.h.LND.Client.GetInfo(ctx)
	if err != nil {
		return nil, err
	}

	return btcec.ParsePubKey(info.IdentityPubkey[:])
}
