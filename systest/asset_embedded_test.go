//go:build systest

package systest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	tapgrpc "github.com/lightninglabs/tap-sdk/grpc"
	"github.com/lightninglabs/wavelength/harness"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	"github.com/lightninglabs/wavelength/waved"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// TestAssetDeposit runs the asset lifecycle on the embedded asset
// backend, with no tapd or lnd beside the daemon: it receives an asset on
// chain, deposits part of it with its own Bitcoin, keeps the surplus units as
// change and boards the deposit into a round. A restart resumes from the
// daemon's database.
func TestAssetDeposit(t *testing.T) {
	ParallelN(t)

	testAssetDeposit(t, waved.WalletTypeLwwallet)
}

// TestAssetDepositLndWallet runs the embedded asset lifecycle on an
// lnd daemon wallet, which derives the engine's keys and signs for it.
func TestAssetDepositLndWallet(t *testing.T) {
	ParallelN(t)

	testAssetDeposit(t, waved.WalletTypeLnd)
}

// testAssetDeposit runs the embedded asset lifecycle on a daemon
// wallet of the given type.
func testAssetDeposit(t *testing.T, walletType string) {
	h := NewSysTestHarness(t, func(opts *harness.Options) {
		opts.StartTapd = true
	})
	h.Harness.FundOperatorLND(btcutil.SatoshiPerBitcoin)

	// The harness tapd only issues the asset and sends it to the daemon.
	issuer := newAssetTestTapd(t, h)
	ref := mintAssetTestAsset(t, h, issuer, 1_000)

	var walletLnd *harness.LndInstance
	if walletType == waved.WalletTypeLnd {
		walletLnd = startEmbeddedAssetLnd(t, h)
	}

	daemon := startEmbeddedAssetDaemon(t, h, t.TempDir(), walletLnd, ref)
	client := daemon.client
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	receiveEmbeddedAsset(t, h, issuer, client, ref, 1_000)
	require.EqualValues(t, 1_000, embeddedAssetUnits(t, client, ref))

	// Esplora has no fee estimates on regtest, so the deposit names its
	// fee rate.
	req, deposit := confirmAssetTestDeposit(t, h, client, ref, 2)
	outpoint, err := wire.NewOutPointFromString(deposit.Outpoint)
	require.NoError(t, err)
	output := h.Harness.GetRawTransaction(
		outpoint.Hash.String(),
	).TxOut[outpoint.Index]

	// A retry under the request ID returns the confirmed deposit and
	// publishes nothing.
	retried, err := client.DepositAsset(ctx, req)
	require.NoError(t, err)
	require.Equal(t, deposit.Outpoint, retried.Outpoint)
	require.Equal(t, deposit.Status, retried.Status)
	require.Empty(t, h.Harness.MempoolTxIDs())

	// The 400 surplus units return to the daemon's asset wallet once the
	// deposit confirms.
	require.Eventually(t, func() bool {
		return embeddedAssetUnits(t, client, ref) == 400
	}, time.Minute, time.Second, "the asset wallet holds no change")

	// After a restart the daemon still holds the change and the deposit.
	daemon.stop()
	daemon = startEmbeddedAssetDaemon(
		t, h, daemon.dataDir, walletLnd, ref,
	)
	client = daemon.client
	require.EqualValues(t, 400, embeddedAssetUnits(t, client, ref))
	deposits, err := client.ListAssetDeposits(
		ctx, &waverpc.ListAssetDepositsRequest{},
	)
	require.NoError(t, err)
	require.Len(t, deposits.Deposits, 1)
	require.Equal(
		t, waverpc.AssetDepositStatus_ASSET_DEPOSIT_STATUS_CONFIRMED,
		deposits.Deposits[0].Status,
	)

	_, err = client.BoardAssetDeposit(
		ctx, &waverpc.BoardAssetDepositRequest{
			RequestId: "alice-1",
		},
	)
	require.NoError(t, err)

	var join *roundpb.JoinRoundRequest
	require.Eventually(t, func() bool {
		reqs := daemon.mailbox.joinRoundRequests()
		if len(reqs) == 0 {
			return false
		}
		join = reqs[len(reqs)-1]

		return true
	}, time.Minute, 100*time.Millisecond, "daemon never joined a round")

	// The join discloses the deposit with the proof the engine wrote.
	require.Len(t, join.BoardingRequests, 1)
	boarding := join.BoardingRequests[0]
	require.Equal(t, ref.String(), boarding.AssetRef)
	require.EqualValues(t, 600, boarding.AssetAmount)
	require.NotEmpty(t, boarding.AssetProof)
	composed, _, _, err := arkscript.ComposedBoardingScript(
		boarding.PolicyTemplate,
		[32]byte(boarding.AssetCommitmentLeafHash),
	)
	require.NoError(t, err)
	require.Equal(t, output.PkScript, composed)

	// The issuer, a real tapd, accepts the deposit's proof.
	verified, err := issuer.VerifyProof(ctx, boarding.AssetProof)
	require.NoError(t, err)
	require.True(t, verified.Valid)
	require.EqualValues(t, 600, verified.DecodedProof.Amount)

	require.Len(t, join.VtxoRequests, 2)
	require.Equal(t, ref.String(), join.VtxoRequests[0].AssetRef)
	require.EqualValues(t, 600, join.VtxoRequests[0].AssetAmount)
}

// TestClaimAssetDeposit claims a confirmed deposit that no round took
// on the asset engine. The claim spends the deposit through its
// owner's exit leaf once the exit delay passes, and returns the asset to the
// daemon's asset wallet. The claim confirms while the daemon is down, and a
// restart finishes it from the daemon's database.
func TestClaimAssetDeposit(t *testing.T) {
	ParallelN(t)

	h := NewSysTestHarness(t, func(opts *harness.Options) {
		opts.StartTapd = true
	})
	h.Harness.FundOperatorLND(btcutil.SatoshiPerBitcoin)

	// The harness tapd only issues the asset and sends it to the daemon.
	issuer := newAssetTestTapd(t, h)
	ref := mintAssetTestAsset(t, h, issuer, 1_000)

	daemon := startEmbeddedAssetDaemon(t, h, t.TempDir(), nil, ref)
	client := daemon.client
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	receiveEmbeddedAsset(t, h, issuer, client, ref, 1_000)
	confirmAssetTestDeposit(t, h, client, ref, 2)
	require.Eventually(t, func() bool {
		return embeddedAssetUnits(t, client, ref) == 400
	}, time.Minute, time.Second, "the asset wallet holds no change")

	claimReq := &waverpc.ClaimAssetDepositRequest{RequestId: "alice-1"}
	_, err := client.ClaimAssetDeposit(ctx, claimReq)
	require.Equal(t, codes.FailedPrecondition, status.Code(err), err)
	require.ErrorContains(t, err, "can be claimed from height")

	// The exit leaf opens after the 144-block boarding exit delay.
	h.Harness.Generate(144)

	var claimed *waverpc.AssetDeposit
	require.Eventually(t, func() bool {
		claimed, err = client.ClaimAssetDeposit(ctx, claimReq)

		return err == nil
	}, time.Minute, time.Second, "claim refused: %v", err)
	require.NotEmpty(t, claimed.ClaimTxid)
	require.False(t, claimed.Claimed)

	// txconfirm relays the zero-fee claim with a CPFP child. The claim is
	// final six blocks deep, which it reaches while the daemon is down.
	h.Harness.WaitMempoolTx(claimed.ClaimTxid)
	daemon.stop()
	h.Harness.Generate(6)

	daemon = startEmbeddedAssetDaemon(t, h, daemon.dataDir, nil, ref)
	client = daemon.client
	require.Eventually(t, func() bool {
		resp, err := client.ListAssetDeposits(
			ctx, &waverpc.ListAssetDepositsRequest{},
		)
		if err != nil || len(resp.Deposits) != 1 {
			return false
		}
		claimed = resp.Deposits[0]

		return claimed.Claimed
	}, time.Minute, time.Second, "claim never confirmed: %v", claimed)

	// The asset wallet holds the claimed units, anchored in the claim's
	// first output, beside the change.
	owned, err := client.ListOwnedAssets(
		ctx, &waverpc.ListOwnedAssetsRequest{},
	)
	require.NoError(t, err)
	var claimedUnits uint64
	for _, held := range owned.Assets {
		if held.Outpoint == claimed.ClaimTxid+":0" {
			claimedUnits += held.Amount
		}
	}
	require.EqualValues(t, 600, claimedUnits)
	require.EqualValues(t, 1_000, embeddedAssetUnits(t, client, ref))

	// A claimed deposit cannot board, and claiming again changes nothing.
	_, err = client.BoardAssetDeposit(
		ctx, &waverpc.BoardAssetDepositRequest{
			RequestId: "alice-1",
		},
	)
	require.Equal(t, codes.FailedPrecondition, status.Code(err), err)
	again, err := client.ClaimAssetDeposit(ctx, claimReq)
	require.NoError(t, err)
	require.Equal(t, claimed.ClaimTxid, again.ClaimTxid)
	require.True(t, again.Claimed)
}

// embeddedAssetDaemon is a running waved with the asset engine.
type embeddedAssetDaemon struct {
	client  waverpc.DaemonServiceClient
	mailbox *fakeMailboxServer
	server  *waved.Server
	dataDir string
	stop    func()
}

// embeddedAssetTestPassword encrypts the test daemon's wallet.
var embeddedAssetTestPassword = []byte("embedded-assets")

// startEmbeddedAssetLnd starts an lnd for the daemon wallet and funds it.
func startEmbeddedAssetLnd(t *testing.T,
	h *SysTestHarness) *harness.LndInstance {

	t.Helper()

	walletLnd := h.Harness.StartAdditionalLND("embedded-assets")
	addr, err := walletLnd.Client.WalletKit.NextAddr(
		t.Context(),
		"default", walletrpc.AddressType_TAPROOT_PUBKEY, false,
	)
	require.NoError(t, err)
	h.Harness.Faucet(addr.String(), assetTestWalletFunds)
	h.Harness.GenerateAndWait(1)
	require.Eventually(t, func() bool {
		balance, err := walletLnd.Client.Client.WalletBalance(
			t.Context(),
		)

		return err == nil && balance.Confirmed == assetTestWalletFunds
	}, time.Minute, time.Second, "daemon wallet never funded")

	return walletLnd
}

// startEmbeddedAssetDaemon runs waved with the asset engine, on
// walletLnd if set and on a light wallet otherwise, against an operator that
// admits ref into rounds. A fresh light wallet is created with funds; an
// existing one is unlocked.
func startEmbeddedAssetDaemon(t *testing.T, h *SysTestHarness, dataDir string,
	walletLnd *harness.LndInstance,
	ref tapsdk.AssetRef) *embeddedAssetDaemon {

	t.Helper()

	mailboxAddr, mailbox := startAssetTestOperator(t, ref)

	cfg := waved.DefaultConfig()
	cfg.DataDir = dataDir
	cfg.Network = "regtest"
	cfg.DebugLevel = "info"
	cfg.Wallet.Type = waved.WalletTypeLwwallet
	cfg.Wallet.EsploraURL = h.Harness.EsploraURL
	if walletLnd != nil {
		cfg.Wallet.Type = waved.WalletTypeLnd
		cfg.Lnd.Host = net.JoinHostPort("localhost", walletLnd.GRPCPort)
		cfg.Lnd.TLSPath = walletLnd.TLSCert
		cfg.Lnd.MacaroonPath = walletLnd.Macaroon
	}
	cfg.Server.Host = mailboxAddr
	cfg.Server.Insecure = true
	cfg.RPC.ListenAddr = "127.0.0.1:0"
	cfg.RPC.Gateway.ListenAddr = "127.0.0.1:0"
	cfg.RPC.NoTLS = true
	cfg.RPC.NoMacaroons = true

	// The harness tapd is the proof courier of the daemon's addresses.
	// The tapd reaches itself under its container name, and the daemon
	// reaches it at the published port.
	courierHost := h.Harness.TapdUniverseHost()
	cfg.Assets = &waved.AssetsConfig{
		Enabled: true,
		Courier: waved.AssetCourierConfig{
			Addr: "authmailbox+universerpc://" + courierHost,
			Endpoints: []string{
				courierHost + "=" + net.JoinHostPort(
					"127.0.0.1", h.Harness.TapdGRPCPort,
				) + "=" + h.Harness.TapdTLSCertPath(),
			},
		},
	}

	server, err := waved.NewServer(cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(h.Context())
	errChan := make(chan error, 1)
	go func() {
		errChan <- server.RunWithContext(ctx)
	}()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true

		cancel()
		select {
		case err := <-errChan:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Logf("waved stopped: %v", err)
			}

		case <-time.After(30 * time.Second):
			t.Errorf("waved did not stop")
		}
	}
	t.Cleanup(stop)

	require.Eventually(t, func() bool {
		return server.RPCAddr() != nil
	}, time.Minute, 50*time.Millisecond, "waved RPC never listened")

	conn, err := grpc.NewClient(
		server.RPCAddr().String(),
		grpc.WithTransportCredentials(
			insecure.NewCredentials(),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	client := waverpc.NewDaemonServiceClient(conn)

	rpcCtx, rpcCancel := context.WithTimeout(ctx, time.Minute)
	defer rpcCancel()

	daemon := &embeddedAssetDaemon{
		client:  client,
		mailbox: mailbox,
		server:  server,
		dataDir: dataDir,
		stop:    stop,
	}

	// An lnd wallet needs no creation or unlock.
	if walletLnd != nil {
		require.NoError(t, server.WaitForWalletServicesReady(rpcCtx))
		waitForDaemonReady(t, client)

		return daemon
	}

	// A fresh directory has no wallet yet.
	var info *waverpc.GetInfoResponse
	require.Eventually(t, func() bool {
		info, err = client.GetInfo(rpcCtx, &waverpc.GetInfoRequest{})

		return err == nil
	}, time.Minute, 100*time.Millisecond, "waved RPC never answered")

	fresh := info.GetWalletState() == waverpc.WalletState_WALLET_STATE_NONE
	if fresh {
		seed, err := client.GenSeed(rpcCtx, &waverpc.GenSeedRequest{})
		require.NoError(t, err)
		_, err = client.InitWallet(rpcCtx, &waverpc.InitWalletRequest{
			Mnemonic:       seed.Mnemonic,
			WalletPassword: embeddedAssetTestPassword,
		})
		require.NoError(t, err)
	} else {
		_, err = client.UnlockWallet(
			rpcCtx, &waverpc.UnlockWalletRequest{
				WalletPassword: embeddedAssetTestPassword,
			},
		)
		require.NoError(t, err)
	}

	require.NoError(t, server.WaitForWalletServicesReady(rpcCtx))
	waitForDaemonReady(t, client)

	if fresh {
		funding, err := client.NewAssetFundingAddress(
			rpcCtx, &waverpc.NewAssetFundingAddressRequest{},
		)
		require.NoError(t, err)
		h.Harness.Faucet(funding.Address, assetTestWalletFunds)
		h.Harness.GenerateAndWait(1)
		require.Eventually(t, func() bool {
			utxos, err := server.ListWalletUnspent(
				rpcCtx, 1, 9_999_999,
			)

			return err == nil && len(utxos) == 1
		}, time.Minute, time.Second, "daemon wallet never funded")
	}

	return daemon
}

// receiveEmbeddedAsset pays units of ref from the issuer to the daemon's
// Taproot Asset address for it, and waits until the daemon holds them. The
// daemon learns of the payment from the mailbox of the issuer, which is the
// proof courier of the address.
func receiveEmbeddedAsset(t *testing.T, h *SysTestHarness,
	issuer *tapgrpc.Client, client waverpc.DaemonServiceClient,
	ref tapsdk.AssetRef, amount uint64) {

	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	addr, err := client.NewAssetAddress(
		ctx, &waverpc.NewAssetAddressRequest{
			AssetRef: ref.String(),
		},
	)
	require.NoError(t, err)

	txid := payAssetAddress(t, h, issuer, addr.Address, amount)
	received := waitForAssetReceive(
		t, client, txid,
		waverpc.AssetReceiveStatus_ASSET_RECEIVE_STATUS_CONFIRMED, true,
	)
	require.Equal(t, addr.Address, received.Address)
	require.Equal(t, amount, received.Amount)
}

// payAssetAddress sends amount units from the issuer to a Taproot Asset
// address, mines the transfer and returns the ID of its anchor transaction.
func payAssetAddress(t *testing.T, h *SysTestHarness, issuer *tapgrpc.Client,
	addr string, amount uint64) string {

	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	issuerWallet := tapsdk.NewWallet(issuer, tapsdk.Network("regtest"))
	transfer, err := issuerWallet.Send(
		ctx, addr, tapsdk.WithAmount(amount),
	)
	require.NoError(t, err)

	h.Harness.WaitMempoolTx(transfer.AnchorTxid)
	h.Harness.Generate(1)

	return transfer.AnchorTxid
}

// assetReceiveWait is what a wait for a receive last saw of the daemon.
type assetReceiveWait struct {
	listed []*waverpc.AssetReceive
	err    error
}

// String describes the last observation when the wait fails.
func (w *assetReceiveWait) String() string {
	return fmt.Sprintf("listed %v, last error %v", w.listed, w.err)
}

// waitForAssetReceive waits until the daemon lists exactly one receive of
// the transaction with the given status, and returns it. With sync set it
// asks the daemon to read its mailboxes while it waits, and otherwise relies
// on the mailboxes the daemon holds open.
func waitForAssetReceive(t *testing.T, client waverpc.DaemonServiceClient,
	txid string, want waverpc.AssetReceiveStatus,
	sync bool) *waverpc.AssetReceive {

	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	var (
		found *waverpc.AssetReceive
		seen  assetReceiveWait
	)
	require.Eventually(t, func() bool {
		if sync {
			_, seen.err = client.SyncAssetReceives(
				ctx, &waverpc.SyncAssetReceivesRequest{},
			)
		}

		resp, err := client.ListAssetReceives(
			ctx, &waverpc.ListAssetReceivesRequest{},
		)
		if err != nil {
			seen.err = err

			return false
		}
		seen.listed = resp.Receives

		// An output lists once per asset ID, and these transfers
		// carry one.
		var matches []*waverpc.AssetReceive
		for _, receive := range resp.Receives {
			if strings.HasPrefix(receive.Outpoint, txid+":") {
				matches = append(matches, receive)
			}
		}
		if len(matches) != 1 || matches[0].Status != want {
			return false
		}
		found = matches[0]

		return true
	}, 3*time.Minute, time.Second, "receive of %s never reached %v: %v",
		txid, want, &seen)

	return found
}

// embeddedAssetUnits sums the units of ref in the daemon's asset wallet.
func embeddedAssetUnits(t *testing.T, client waverpc.DaemonServiceClient,
	ref tapsdk.AssetRef) uint64 {

	t.Helper()

	owned, err := client.ListOwnedAssets(
		t.Context(), &waverpc.ListOwnedAssetsRequest{},
	)
	require.NoError(t, err)

	var units uint64
	for _, held := range owned.Assets {
		heldRef, err := tapsdk.ParseAssetRef(held.AssetRef)
		require.NoError(t, err)
		if heldRef.Equivalent(ref) {
			units += held.Amount
		}
	}

	return units
}
