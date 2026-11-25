//go:build systest

package systest

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	tapgrpc "github.com/lightninglabs/tap-sdk/grpc"
	"github.com/lightninglabs/wavelength/harness"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
)

// TestAssetAddressReceive pays one Taproot Asset address of the daemon three
// times: while the daemon holds the mailbox of the address open, with proofs
// that reach the proof courier only after the daemon restarted, and while the
// daemon is down.
func TestAssetAddressReceive(t *testing.T) {
	ParallelN(t)

	h := NewSysTestHarness(t, func(opts *harness.Options) {
		opts.StartTapd = true
	})
	h.Harness.FundOperatorLND(btcutil.SatoshiPerBitcoin)

	// The harness tapd issues the asset, pays the address and is the
	// proof courier the address names.
	issuer := newAssetTestTapd(t, h)
	ref := mintAssetTestAsset(t, h, issuer, 1_000)

	daemon := startEmbeddedAssetDaemon(t, h, t.TempDir(), nil, ref)
	client := daemon.client
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()

	confirmed := waverpc.AssetReceiveStatus_ASSET_RECEIVE_STATUS_CONFIRMED
	detected := waverpc.AssetReceiveStatus_ASSET_RECEIVE_STATUS_DETECTED

	// The wallet has one address per asset.
	addrReq := &waverpc.NewAssetAddressRequest{AssetRef: ref.String()}
	addr, err := client.NewAssetAddress(ctx, addrReq)
	require.NoError(t, err)
	again, err := client.NewAssetAddress(ctx, addrReq)
	require.NoError(t, err)
	require.Equal(t, addr.Address, again.Address)

	// The daemon reads the mailbox of the address as the sender announces
	// the payment, without being asked to.
	first := payAssetAddress(t, h, issuer, addr.Address, 100)
	waitForAssetReceive(t, client, first, confirmed, false)
	require.EqualValues(t, 100, embeddedAssetUnits(t, client, ref))

	// The sender announces a transfer before it uploads the proofs. While
	// the courier refuses the proofs, the daemon knows of the second
	// payment and holds none of it.
	setAssetTestCourierInserts(t, issuer, ref, false)
	second := payAssetAddress(t, h, issuer, addr.Address, 200)
	announced := waitForAssetReceive(t, client, second, detected, false)
	require.Zero(t, announced.Amount)
	require.Empty(t, announced.AssetId)
	require.EqualValues(t, 100, embeddedAssetUnits(t, client, ref))

	// The daemon restarts between the announcement and the proofs, and
	// completes the receive once the sender's next attempt uploads them.
	daemon.stop()
	setAssetTestCourierInserts(t, issuer, ref, true)
	daemon = startEmbeddedAssetDaemon(t, h, daemon.dataDir, nil, ref)
	client = daemon.client
	waitForAssetReceive(t, client, second, confirmed, false)
	require.EqualValues(t, 300, embeddedAssetUnits(t, client, ref))

	// The courier keeps the announcement of a payment made while the
	// daemon is down, and the daemon reads it when it starts.
	daemon.stop()
	third := payAssetAddress(t, h, issuer, addr.Address, 300)
	daemon = startEmbeddedAssetDaemon(t, h, daemon.dataDir, nil, ref)
	client = daemon.client
	waitForAssetReceive(t, client, third, confirmed, false)
	require.EqualValues(t, 600, embeddedAssetUnits(t, client, ref))

	// Each payment lists once with its amount, oldest first, and the
	// wallet still has the one address.
	receives, err := client.ListAssetReceives(
		ctx, &waverpc.ListAssetReceivesRequest{},
	)
	require.NoError(t, err)
	require.Len(t, receives.Receives, 3)
	assetID, ok := ref.AssetID()
	require.True(t, ok)
	for idx, amount := range []uint64{100, 200, 300} {
		receive := receives.Receives[idx]
		require.Equal(t, confirmed, receive.Status)
		require.Equal(t, addr.Address, receive.Address)
		require.Equal(t, ref.String(), receive.AssetRef)
		require.Equal(t, assetID.String(), receive.AssetId)
		require.Equal(t, amount, receive.Amount)
		require.NotZero(t, receive.ConfirmationHeight)
	}

	again, err = client.NewAssetAddress(ctx, addrReq)
	require.NoError(t, err)
	require.Equal(t, addr.Address, again.Address)
	addrs, err := client.ListAssetAddresses(
		ctx, &waverpc.ListAssetAddressesRequest{},
	)
	require.NoError(t, err)
	require.Len(t, addrs.Addresses, 1)
	require.Equal(t, addr.Address, addrs.Addresses[0].Address)
	require.Equal(t, ref.String(), addrs.Addresses[0].AssetRef)
}

// setAssetTestCourierInserts makes the universe of the issuer take the
// proofs of ref, or refuse them.
func setAssetTestCourierInserts(t *testing.T, issuer *tapgrpc.Client,
	ref tapsdk.AssetRef, allow bool) {

	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	// A setting for one universe takes precedence over the courier's
	// setting for all of them.
	var perUniverse []tapsdk.AssetFederationSyncConfig
	for _, proofType := range []tapsdk.ProofType{
		tapsdk.ProofTypeIssuance, tapsdk.ProofTypeTransfer,
	} {
		perUniverse = append(
			perUniverse, tapsdk.AssetFederationSyncConfig{
				ID: tapsdk.UniverseIDFromRef(
					ref, proofType,
				),
				AllowSyncInsert: allow,
				AllowSyncExport: true,
			},
		)
	}
	require.NoError(
		t, issuer.SetFederationSyncConfig(ctx, nil, perUniverse),
	)
}
