//go:build systest

package systest

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	tapgrpc "github.com/lightninglabs/tap-sdk/grpc"
	"github.com/lightninglabs/tap-sdk/macaroon"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
)

// confirmAssetTestDeposit deposits 600 units of ref through waved, mines the
// deposit and waits until its proof confirms. A zero fee rate leaves the rate
// to the daemon's fee estimate.
func confirmAssetTestDeposit(t *testing.T, h *SysTestHarness,
	client waverpc.DaemonServiceClient, ref tapsdk.AssetRef,
	satPerVByte uint64) (*waverpc.DepositAssetRequest,
	*waverpc.AssetDeposit) {

	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	req := &waverpc.DepositAssetRequest{
		RequestId:   "alice-1",
		AssetRef:    ref.String(),
		AssetAmount: 600,
		SatPerVbyte: satPerVByte,
		MaxFeeSat:   50_000,
	}
	deposit, err := client.DepositAsset(ctx, req)
	require.NoError(t, err)
	require.Equal(
		t, waverpc.AssetDepositStatus_ASSET_DEPOSIT_STATUS_PUBLISHED,
		deposit.Status,
	)
	// Two 1,000-sat VTXO minimums plus twice the 1,000-sat round fee.
	require.EqualValues(t, 4_000, deposit.ValueSat)

	outpoint, err := wire.NewOutPointFromString(deposit.Outpoint)
	require.NoError(t, err)
	h.Harness.WaitMempoolTx(outpoint.Hash.String())

	// The deposit confirms at the operator's two-block boarding depth.
	h.Harness.Generate(2)

	confirmed := waverpc.AssetDepositStatus_ASSET_DEPOSIT_STATUS_CONFIRMED
	require.Eventually(t, func() bool {
		resp, err := client.ListAssetDeposits(
			ctx, &waverpc.ListAssetDepositsRequest{},
		)
		if err != nil || len(resp.Deposits) != 1 {
			return false
		}
		deposit = resp.Deposits[0]

		return deposit.Status == confirmed
	}, time.Minute, time.Second, "asset deposit never confirmed: %v",
		deposit)
	require.NotZero(t, deposit.ConfirmedHeight)
	require.Empty(t, deposit.Error)

	return req, deposit
}

// newAssetTestTapd connects a tap-sdk client to the harness tapd.
func newAssetTestTapd(t *testing.T, h *SysTestHarness) *tapgrpc.Client {
	t.Helper()

	client, err := tapgrpc.NewClient(&tapgrpc.Config{
		Host: net.JoinHostPort(
			"127.0.0.1", h.Harness.TapdGRPCPort,
		),
		Network:  tapsdk.Network("regtest"),
		TLS:      tapgrpc.TLSFromPath(h.Harness.TapdTLSCertPath()),
		Macaroon: macaroon.FromPath(h.Harness.TapdMacaroonPath()),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	return client
}

// mintAssetTestAsset mints an ungrouped fungible asset in the harness tapd
// and waits until tapd holds it.
func mintAssetTestAsset(t *testing.T, h *SysTestHarness, tapd *tapgrpc.Client,
	amount uint64) tapsdk.AssetRef {

	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	_, err := tapd.MintAsset(ctx, &tapsdk.MintAssetRequest{
		Asset: &tapsdk.MintAsset{
			AssetType:     tapsdk.AssetTypeNormal,
			Name:          "deposit-test",
			InitialSupply: amount,
			AssetMeta: &tapsdk.AssetMeta{
				Data: []byte("deposit test asset"),
			},
		},
		ShortResponse: true,
	})
	require.NoError(t, err)
	batch, err := tapd.FinalizeBatch(ctx, &tapsdk.FinalizeBatchRequest{
		ShortResponse: true,
	})
	require.NoError(t, err)
	h.Harness.WaitMempoolTx(batch.BatchTxid)
	h.Harness.Generate(1)

	var ref tapsdk.AssetRef
	require.Eventually(t, func() bool {
		utxos, err := tapd.ListUtxos(ctx, &tapsdk.ListUtxosRequest{})
		if err != nil {
			return false
		}
		for _, utxo := range utxos {
			for _, asset := range utxo.Assets {
				if asset != nil && asset.Amount == amount {
					ref = asset.AssetRef
				}
			}
		}

		return ref != ""
	}, time.Minute, time.Second, "minted asset never appeared")

	return ref
}

// startAssetTestOperator runs a fake operator mailbox that quotes the test
// round fee, and returns its address.
func startAssetTestOperator(t *testing.T) (string, *fakeMailboxServer) {
	t.Helper()

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorInfo := &arkrpc.GetInfoResponse{
		Pubkey:             operatorPriv.PubKey().SerializeCompressed(),
		BoardingExitDelay:  144,
		VtxoExitDelay:      144,
		DustLimit:          testDustLimitSat,
		MinVtxoAmountSat:   1_000,
		MinOperatorFee:     testOperatorFeeSat,
		MinConfirmations:   2,
		SelectedArkVersion: arkrpc.ArkProtocolVersionV1,
		ArkVersionPolicies: []*arkrpc.ArkVersionPolicy{{
			Version: arkrpc.ArkProtocolVersionV1,
			State:   arkrpc.ArkVersionPolicy_STATE_ACTIVE,
		}},
	}
	mailboxAddr, mailbox, stopMailbox := startFakeMailboxServer(
		t,
		serverconn.PubKeyMailboxID(
			operatorPriv.PubKey(),
		),
		operatorInfo,
	)
	t.Cleanup(stopMailbox)
	mailbox.setFeeQuote(&arkrpc.EstimateFeeResponse{
		TotalFeeSat: testOperatorFeeSat,
	})

	return mailboxAddr, mailbox
}

// assetTestWalletFunds is what the daemon wallet starts with.
const assetTestWalletFunds = btcutil.Amount(100_000)
