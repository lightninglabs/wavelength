//go:build systest

package systest

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	tapgrpc "github.com/lightninglabs/tap-sdk/grpc"
	"github.com/lightninglabs/tap-sdk/macaroon"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/harness"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/lightninglabs/wavelength/waved"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestAssetDeposit funds an asset deposit from tapd through waved. The
// deposit confirms with its proof, a retry under its request ID returns it
// without funding again, and tapd keeps the surplus units as change.
func TestAssetDeposit(t *testing.T) {
	ParallelN(t)

	h := NewSysTestHarness(t, func(opts *harness.Options) {
		opts.StartTapd = true
	})
	h.Harness.FundOperatorLND(btcutil.SatoshiPerBitcoin)

	tapd := newAssetTestTapd(t, h)
	ref := mintAssetTestAsset(t, h, tapd, 1_000)

	client := startAssetDepositDaemon(t, h)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	req := &waverpc.DepositAssetRequest{
		RequestId:   "alice-1",
		AssetRef:    ref.String(),
		AssetAmount: 600,
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

	// The retry returns the confirmed deposit and publishes nothing.
	retried, err := client.DepositAsset(ctx, req)
	require.NoError(t, err)
	require.Equal(t, deposit.Outpoint, retried.Outpoint)
	require.Equal(t, deposit.Status, retried.Status)
	require.Empty(t, h.Harness.MempoolTxIDs())

	// The 400 surplus units are back in tapd as spendable change.
	require.Eventually(t, func() bool {
		return assetTestUtxoUnits(t, tapd, ref) == 400
	}, time.Minute, time.Second, "tapd holds no asset change")
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

// assetTestUtxoUnits sums the units of ref that tapd holds.
func assetTestUtxoUnits(t *testing.T, tapd *tapgrpc.Client,
	ref tapsdk.AssetRef) uint64 {

	t.Helper()

	utxos, err := tapd.ListUtxos(t.Context(), &tapsdk.ListUtxosRequest{})
	require.NoError(t, err)

	var units uint64
	for _, utxo := range utxos {
		for _, asset := range utxo.Assets {
			if asset != nil && asset.AssetRef.Equivalent(ref) {
				units += asset.Amount
			}
		}
	}

	return units
}

// startAssetDepositDaemon runs waved on the harness lnd with the tapd asset
// backend and the same lnd signing deposits.
func startAssetDepositDaemon(t *testing.T,
	h *SysTestHarness) waverpc.DaemonServiceClient {

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

	lndDataDir := filepath.Join(h.Harness.BaseDir(), "lnd")
	lnd := &waved.AssetLndConfig{
		Host:    net.JoinHostPort("localhost", h.Harness.LNDGRPCPort),
		TLSPath: filepath.Join(lndDataDir, "tls.cert"),
		MacaroonPath: filepath.Join(
			lndDataDir, "data", "chain", "bitcoin", "regtest",
			"admin.macaroon",
		),
	}

	cfg := waved.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.Network = "regtest"
	cfg.DebugLevel = "info"
	cfg.Wallet.Type = waved.WalletTypeLnd
	cfg.Lnd.Host = lnd.Host
	cfg.Lnd.TLSPath = lnd.TLSPath
	cfg.Lnd.MacaroonPath = lnd.MacaroonPath
	cfg.Server.Host = mailboxAddr
	cfg.Server.Insecure = true
	cfg.RPC.ListenAddr = "127.0.0.1:0"
	cfg.RPC.Gateway.ListenAddr = "127.0.0.1:0"
	cfg.RPC.NoTLS = true
	cfg.RPC.NoMacaroons = true
	cfg.Assets = &waved.AssetsConfig{
		Backend: waved.AssetBackendTapd,
		Tapd: &waved.TapdConfig{
			Host: net.JoinHostPort(
				"127.0.0.1", h.Harness.TapdGRPCPort,
			),
			TLSPath:      h.Harness.TapdTLSCertPath(),
			MacaroonPath: h.Harness.TapdMacaroonPath(),
		},
		Lnd: lnd,
	}

	server, err := waved.NewServer(cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(h.Context())
	errChan := make(chan error, 1)
	go func() {
		errChan <- server.RunWithContext(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errChan:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Logf("waved stopped: %v", err)
			}

		case <-time.After(30 * time.Second):
			t.Errorf("waved did not stop")
		}
	})

	readyCtx, readyCancel := context.WithTimeout(ctx, time.Minute)
	defer readyCancel()
	require.NoError(t, server.WaitForWalletServicesReady(readyCtx))

	conn, err := grpc.NewClient(
		server.RPCAddr().String(),
		grpc.WithTransportCredentials(
			insecure.NewCredentials(),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	client := waverpc.NewDaemonServiceClient(conn)
	waitForDaemonReady(t, client)

	return client
}
