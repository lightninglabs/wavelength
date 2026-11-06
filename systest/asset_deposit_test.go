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
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	tapgrpc "github.com/lightninglabs/tap-sdk/grpc"
	"github.com/lightninglabs/tap-sdk/macaroon"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/harness"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/bip322"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/lightninglabs/wavelength/waved"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// TestAssetDeposit deposits tapd assets through waved. The daemon wallet pays
// the deposit's Bitcoin, the deposit confirms with its proof, a retry under its
// request ID returns it without funding again, and tapd keeps the surplus
// units as change.
func TestAssetDeposit(t *testing.T) {
	ParallelN(t)

	h := NewSysTestHarness(t, func(opts *harness.Options) {
		opts.StartTapd = true
	})
	h.Harness.FundOperatorLND(btcutil.SatoshiPerBitcoin)

	tapd := newAssetTestTapd(t, h)
	ref := mintAssetTestAsset(t, h, tapd, 1_000)

	client, _, walletLnd := startAssetDepositDaemon(t, h)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	walletUtxos := assetTestWalletUtxos(t, walletLnd)
	anchors := assetTestAnchors(t, tapd)
	walletBefore, err := walletLnd.Client.Client.WalletBalance(ctx)
	require.NoError(t, err)

	req, deposit := confirmAssetTestDeposit(t, h, client, ref)

	// The deposit spends tapd's asset anchors and the daemon wallet's
	// coins, and nothing else of the lnd behind tapd.
	outpoint, err := wire.NewOutPointFromString(deposit.Outpoint)
	require.NoError(t, err)
	depositTx := h.Harness.GetRawTransaction(outpoint.Hash.String())
	var walletIn, anchorIn int
	for _, txIn := range depositTx.TxIn {
		if _, ok := walletUtxos[txIn.PreviousOutPoint]; ok {
			walletIn++
			continue
		}
		_, ok := anchors[txIn.PreviousOutPoint]
		require.True(
			t, ok, "deposit spends %v from neither tapd nor the "+
				"daemon wallet", txIn.PreviousOutPoint,
		)
		anchorIn++
	}
	require.Positive(t, walletIn)
	require.Positive(t, anchorIn)

	// The daemon wallet's balance drops by what it spent, less its
	// change.
	walletAfter := assetTestWalletUtxos(t, walletLnd)
	var spent, change int64
	for _, txIn := range depositTx.TxIn {
		spent += walletUtxos[txIn.PreviousOutPoint]
	}
	for idx := range depositTx.TxOut {
		change += walletAfter[wire.OutPoint{
			Hash:  outpoint.Hash,
			Index: uint32(idx),
		}]
	}
	require.Positive(t, change)
	balance, err := walletLnd.Client.Client.WalletBalance(ctx)
	require.NoError(t, err)
	require.Equal(
		t, walletBefore.Confirmed-btcutil.Amount(spent-change),
		balance.Confirmed,
	)

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

// TestBoardAssetDeposit boards a confirmed asset deposit. The daemon joins the
// next round with the deposit's asset disclosure, an asset VTXO and a BTC
// change VTXO, and the join authorization proves the deposit against its
// on-chain output.
func TestBoardAssetDeposit(t *testing.T) {
	ParallelN(t)

	h := NewSysTestHarness(t, func(opts *harness.Options) {
		opts.StartTapd = true
	})
	h.Harness.FundOperatorLND(btcutil.SatoshiPerBitcoin)

	tapd := newAssetTestTapd(t, h)
	ref := mintAssetTestAsset(t, h, tapd, 1_000)

	client, mailbox, _ := startAssetDepositDaemon(t, h)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	_, deposit := confirmAssetTestDeposit(t, h, client, ref)
	outpoint, err := wire.NewOutPointFromString(deposit.Outpoint)
	require.NoError(t, err)
	output := h.Harness.GetRawTransaction(
		outpoint.Hash.String(),
	).TxOut[outpoint.Index]

	board := &waverpc.BoardAssetDepositRequest{RequestId: "alice-1"}
	_, err = client.BoardAssetDeposit(ctx, board)
	require.NoError(t, err)

	var join *roundpb.JoinRoundRequest
	require.Eventually(t, func() bool {
		reqs := mailbox.joinRoundRequests()
		if len(reqs) == 0 {
			return false
		}
		join = reqs[len(reqs)-1]

		return true
	}, time.Minute, 100*time.Millisecond, "daemon never joined a round")

	// The disclosure reproduces the deposit script, which is how the
	// operator binds the claimed asset to the confirmed output.
	require.Len(t, join.BoardingRequests, 1)
	boarding := join.BoardingRequests[0]
	require.Equal(t, ref.String(), boarding.AssetRef)
	require.EqualValues(t, 600, boarding.AssetAmount)
	require.NotEmpty(t, boarding.AssetProof)
	require.Len(t, boarding.AssetWitness, 2)
	composed, _, _, err := arkscript.ComposedBoardingScript(
		boarding.PolicyTemplate,
		[32]byte(boarding.AssetCommitmentLeafHash),
	)
	require.NoError(t, err)
	require.Equal(t, output.PkScript, composed)

	// The asset VTXO holds the minimum VTXO amount, and the BTC VTXO
	// takes the rest after the fee.
	require.Len(t, join.VtxoRequests, 2)
	asset, change := join.VtxoRequests[0], join.VtxoRequests[1]
	require.Equal(t, ref.String(), asset.AssetRef)
	require.EqualValues(t, 600, asset.AssetAmount)
	require.EqualValues(t, 1_000, asset.TargetAmountSat)
	require.True(t, asset.FixedAmount)
	require.Empty(t, change.AssetRef)
	require.True(t, change.IsChange)
	require.EqualValues(
		t, 4_000-1_000-testOperatorFeeSat, change.TargetAmountSat,
	)

	// The join authorization proves the deposit through its composed
	// exit spend.
	require.NotNil(t, join.Auth)
	identifier, err := btcec.ParsePubKey(join.Identifier)
	require.NoError(t, err)
	challenge, err := bip322.JoinRoundMessageChallenge(identifier)
	require.NoError(t, err)
	sig, err := bip322.DecodeSig(join.Auth.Signature)
	require.NoError(t, err)
	intent := &bip322.Intent{
		Payload:    join.Auth.Message,
		ValidFrom:  join.Auth.ValidFrom,
		ValidUntil: join.Auth.ValidUntil,
	}
	message, err := intent.SigningMessage()
	require.NoError(t, err)
	result := bip322.ValidateAuthPkg(&bip322.AuthPkg{
		Message:          message,
		MessageChallenge: challenge,
		Sig:              sig,
		ProofPrevOutputs: map[wire.OutPoint]*wire.TxOut{
			*outpoint: output,
		},
	})
	require.Equal(
		t, bip322.VerificationStateValid, result.State, result.Reason,
	)

	// The round still holds the deposit, so a second board is refused.
	_, err = client.BoardAssetDeposit(ctx, board)
	require.Equal(t, codes.AlreadyExists, status.Code(err))
}

// confirmAssetTestDeposit deposits 600 units of ref through waved, mines the
// deposit and waits until its proof confirms.
func confirmAssetTestDeposit(t *testing.T, h *SysTestHarness,
	client waverpc.DaemonServiceClient,
	ref tapsdk.AssetRef) (*waverpc.DepositAssetRequest,
	*waverpc.AssetDeposit) {

	t.Helper()

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

// startAssetDepositDaemon runs waved with the tapd asset backend. The daemon
// wallet is a funded lnd of its own, apart from the harness lnd behind tapd
// that signs tapd's asset inputs.
func startAssetDepositDaemon(t *testing.T, h *SysTestHarness) (
	waverpc.DaemonServiceClient, *fakeMailboxServer, *harness.LndInstance) {

	t.Helper()

	walletLnd := h.Harness.StartAdditionalLND("asset-depositor")
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
	cfg.Lnd.Host = net.JoinHostPort("localhost", walletLnd.GRPCPort)
	cfg.Lnd.TLSPath = walletLnd.TLSCert
	cfg.Lnd.MacaroonPath = walletLnd.Macaroon
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

	return client, mailbox, walletLnd
}

// assetTestWalletFunds is what the daemon wallet starts with.
const assetTestWalletFunds = btcutil.Amount(100_000)

// assetTestWalletUtxos returns the values of an lnd's confirmed coins.
func assetTestWalletUtxos(t *testing.T,
	lnd *harness.LndInstance) map[wire.OutPoint]int64 {

	t.Helper()

	utxos, err := lnd.Client.WalletKit.ListUnspent(t.Context(), 1, 0)
	require.NoError(t, err)
	values := make(map[wire.OutPoint]int64, len(utxos))
	for _, utxo := range utxos {
		values[utxo.OutPoint] = int64(utxo.Value)
	}

	return values
}

// assetTestAnchors returns the outpoints of tapd's asset anchors.
func assetTestAnchors(t *testing.T,
	tapd *tapgrpc.Client) map[wire.OutPoint]struct{} {

	t.Helper()

	utxos, err := tapd.ListUtxos(t.Context(), &tapsdk.ListUtxosRequest{})
	require.NoError(t, err)
	anchors := make(map[wire.OutPoint]struct{}, len(utxos))
	for _, utxo := range utxos {
		anchors[wire.OutPoint{
			Hash:  chainhash.Hash(utxo.OutPoint.Txid),
			Index: utxo.OutPoint.Index,
		}] = struct{}{}
	}

	return anchors
}
