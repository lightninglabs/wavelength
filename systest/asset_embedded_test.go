//go:build systest

package systest

import (
	"context"
	"errors"
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
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestEmbeddedAssetDeposit runs the asset lifecycle on the embedded asset
// backend. The daemon runs on its own wallet with no tapd or lnd beside it:
// it receives an asset on chain, deposits part of it with its own Bitcoin,
// keeps the surplus units as change and boards the deposit into a round. A
// restart resumes from the daemon's database.
func TestEmbeddedAssetDeposit(t *testing.T) {
	ParallelN(t)

	h := NewSysTestHarness(t, func(opts *harness.Options) {
		opts.StartTapd = true
	})
	h.Harness.FundOperatorLND(btcutil.SatoshiPerBitcoin)

	// The harness tapd only issues the asset and sends it to the daemon.
	issuer := newAssetTestTapd(t, h)
	ref := mintAssetTestAsset(t, h, issuer, 1_000)

	daemon := startEmbeddedAssetDaemon(t, h, t.TempDir())
	client := daemon.client
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	receiveEmbeddedAsset(t, h, issuer, client, ref, 1_000)
	require.EqualValues(t, 1_000, embeddedAssetUnits(t, client, ref))

	// Esplora has no fee estimates on regtest, so the deposit names its
	// fee rate.
	_, deposit := confirmAssetTestDeposit(t, h, client, ref, 2)
	outpoint, err := wire.NewOutPointFromString(deposit.Outpoint)
	require.NoError(t, err)
	output := h.Harness.GetRawTransaction(
		outpoint.Hash.String(),
	).TxOut[outpoint.Index]

	// The 400 surplus units return to the daemon's asset wallet once the
	// deposit confirms.
	require.Eventually(t, func() bool {
		return embeddedAssetUnits(t, client, ref) == 400
	}, time.Minute, time.Second, "the asset wallet holds no change")

	// After a restart the daemon still holds the change and the deposit.
	daemon.stop()
	daemon = startEmbeddedAssetDaemon(t, h, daemon.dataDir)
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

// embeddedAssetDaemon is a running waved with the embedded asset backend.
type embeddedAssetDaemon struct {
	client  waverpc.DaemonServiceClient
	mailbox *fakeMailboxServer
	server  *waved.Server
	dataDir string
	stop    func()
}

// embeddedAssetTestPassword encrypts the test daemon's wallet.
var embeddedAssetTestPassword = []byte("embedded-assets")

// startEmbeddedAssetDaemon runs waved on a light wallet with the embedded
// asset backend. A fresh data directory gets a new wallet with funds; an
// existing one is unlocked.
func startEmbeddedAssetDaemon(t *testing.T, h *SysTestHarness,
	dataDir string) *embeddedAssetDaemon {

	t.Helper()

	mailboxAddr, mailbox := startAssetTestOperator(t)

	cfg := waved.DefaultConfig()
	cfg.DataDir = dataDir
	cfg.Network = "regtest"
	cfg.DebugLevel = "info"
	cfg.Wallet.Type = waved.WalletTypeLwwallet
	cfg.Wallet.EsploraURL = h.Harness.EsploraURL
	cfg.Server.Host = mailboxAddr
	cfg.Server.Insecure = true
	cfg.RPC.ListenAddr = "127.0.0.1:0"
	cfg.RPC.Gateway.ListenAddr = "127.0.0.1:0"
	cfg.RPC.NoTLS = true
	cfg.RPC.NoMacaroons = true
	cfg.Assets = &waved.AssetsConfig{
		Backend: waved.AssetBackendEmbedded,
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

	return &embeddedAssetDaemon{
		client:  client,
		mailbox: mailbox,
		server:  server,
		dataDir: dataDir,
		stop:    stop,
	}
}

// receiveEmbeddedAsset sends units of ref from the issuer to keys the daemon
// derives, and imports the transfer's proof into the daemon's asset wallet.
func receiveEmbeddedAsset(t *testing.T, h *SysTestHarness,
	issuer *tapgrpc.Client, client waverpc.DaemonServiceClient,
	ref tapsdk.AssetRef, amount uint64) {

	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	keys, err := client.NewAssetReceiveKeys(
		ctx, &waverpc.NewAssetReceiveKeysRequest{},
	)
	require.NoError(t, err)
	scriptKey, err := tapsdk.ParsePubKey(keys.ScriptKey)
	require.NoError(t, err)
	rawScriptKey, err := tapsdk.ParsePubKey(keys.RawScriptKey)
	require.NoError(t, err)
	anchorKey, err := tapsdk.ParsePubKey(keys.AnchorInternalKey)
	require.NoError(t, err)

	issuerWallet := tapsdk.NewWallet(issuer, tapsdk.Network("regtest"))
	transfer, err := issuerWallet.NewInteractiveTxBuilder().
		SetAsset(ref, amount).
		SetReceiverKeys(tapsdk.DerivedKeys{
			ScriptKey: tapsdk.ScriptKey{
				PubKey: scriptKey,
				KeyDesc: tapsdk.KeyDescriptor{
					RawKeyBytes: rawScriptKey,
				},
			},
			InternalKey: tapsdk.InternalKey{PubKey: anchorKey},
		}).
		Execute(ctx)
	require.NoError(t, err)

	h.Harness.WaitMempoolTx(transfer.AnchorTxid)
	h.Harness.Generate(1)

	var received *tapsdk.TransferOutput
	for idx := range transfer.Outputs {
		if transfer.Outputs[idx].ScriptKey == scriptKey {
			received = &transfer.Outputs[idx]
		}
	}
	require.NotNil(t, received, "transfer has no output for the daemon")

	// The issuer writes the proof once it sees the confirmation.
	var proofFile []byte
	require.Eventually(t, func() bool {
		exported, err := issuer.ExportProof(
			ctx, tapsdk.AssetRefFromAssetID(received.IssuanceID),
			received.ScriptKey, &received.AnchorOutpoint,
		)
		if err != nil || exported == nil {
			return false
		}
		proofFile = exported.RawProofFile

		return len(proofFile) != 0
	}, time.Minute, time.Second, "issuer never exported the proof")

	// The daemon accepts the proof once its chain backend has the block.
	var imported *waverpc.OwnedAsset
	require.Eventually(t, func() bool {
		imported, err = client.ImportAssetProof(
			ctx, &waverpc.ImportAssetProofRequest{
				ProofFile: proofFile,
				AssetRef:  ref.String(),
			},
		)

		return err == nil
	}, time.Minute, time.Second, "daemon never imported the proof: %v",
		err)
	require.Equal(t, amount, imported.Amount)
	require.Equal(t, keys.ScriptKey, imported.ScriptKey)
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
