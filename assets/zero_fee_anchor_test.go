package assets_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightninglabs/darepo-client/assets"
	"github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

type walletKitFundingShim struct {
	kit lndclient.WalletKitClient
}

func newWalletKitFundingShim(
	kit lndclient.WalletKitClient) *walletKitFundingShim {

	return &walletKitFundingShim{kit: kit}
}

func (w *walletKitFundingShim) FundPsbt(ctx context.Context,
	packet *psbt.Packet, changeIndex int, feeRate chainfee.SatPerKWeight) (
	*psbt.Packet, error) {

	var buf bytes.Buffer
	if err := packet.Serialize(&buf); err != nil {
		return nil, err
	}

	coinSelect := &walletrpc.PsbtCoinSelect{
		Psbt: buf.Bytes(),
		ChangeOutput: &walletrpc.PsbtCoinSelect_ExistingOutputIndex{
			ExistingOutputIndex: int32(changeIndex),
		},
	}

	req := &walletrpc.FundPsbtRequest{
		Template: &walletrpc.FundPsbtRequest_CoinSelect{
			CoinSelect: coinSelect,
		},
		Fees: &walletrpc.FundPsbtRequest_SatPerKw{
			SatPerKw: uint64(feeRate),
		},
		SpendUnconfirmed: true,
	}

	funded, _, _, err := w.kit.FundPsbt(ctx, req)
	if err != nil {
		return nil, err
	}

	return funded, nil
}

func (w *walletKitFundingShim) SignPsbt(ctx context.Context,
	packet *psbt.Packet) (
	*psbt.Packet, error,
) {

	return w.kit.SignPsbt(ctx, packet)
}

func TestAssetZeroValueBTCAnchorPackage(t *testing.T) {
	opts := harness.DefaultOptions()
	opts.StartTapd = true
	opts.GroupName = "asset-zero-fee-anchor"

	h := harness.NewHarness(t, &opts)
	h.Start()
	t.Cleanup(h.Stop)

	const scriptOnly = false
	const csvDelay = uint32(2)

	f := newBoardingFixtureWithAliceBoardingFunded(h, scriptOnly, csvDelay)

	builder := assets.NewAssetTxBuilder(f.assetID, f.chainParams)

	inputMuSig := &assets.MuSig2Spec{
		Participants: []assets.MuSig2Participant{
			{
				Role: assets.SignerRole("user"),
				PubKey: f.userKey.PubKey().
					SerializeCompressed(),
			},
			{
				Role: assets.SignerRole("operator"),
				PubKey: f.operatorKey.PubKey().
					SerializeCompressed(),
			},
		},
		SortKeys: true,
		Tweaks: assets.MuSig2Tweaks{
			TaprootBIP0086Tweak: true,
		},
	}

	err := builder.AddAssetInput(assets.InputConfig{
		ProofFile: f.boardingProof.RawProofFile,
		AnchorKey: assets.AnchorKeySpec{
			Mode:   assets.AnchorKeyModeMuSig2,
			MuSig2: inputMuSig,
		},
		Closures: []assets.ScriptClosure{
			csvClosureScript(f.userKey.PubKey(), csvDelay),
		},
	})
	require.NoError(t, err)

	destScriptKey, destInternalKeyDesc, err :=
		f.operatorClient.DeriveNewKeys(t.Context())
	require.NoError(t, err)

	err = builder.AddAssetOutput(assets.OutputConfig{
		Amount: f.asset.Amount,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key: schnorr.SerializePubKey(
				destInternalKeyDesc.PubKey,
			),
		},
		Script: assets.OpTrueScriptWithWalletKey(
			&destScriptKey, destInternalKeyDesc.PubKey,
		),
	})
	require.NoError(t, err)

	anchorSpec := assets.NewEphemeralBTCAnchorSpec()
	anchorSpec.Description = "anchor-0"
	err = builder.AddBTCAnchor(anchorSpec)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), defaultTimeout)
	defer cancel()

	plan, err := builder.Compile(ctx)
	require.NoError(t, err)
	require.Len(t, plan.BTCAnchors, 1)
	require.Equal(t, int64(0), plan.BTCAnchors[0].ValueSat)

	commitOpts := assets.CommitOptions{SkipWalletFunding: true}
	err = builder.Commit(
		ctx, f.operatorClient.AssetWalletClient, commitOpts,
	)
	require.NoError(t, err)

	btcAnchors := builder.BTCAnchors()
	require.Len(t, btcAnchors, 1)
	require.GreaterOrEqual(t, btcAnchors[0].OutputIndex, 0)
	require.Equal(t, int64(0), btcAnchors[0].ValueSat)

	anchorPsbt := builder.AnchorPsbt()
	require.NotNil(t, anchorPsbt)
	anchorIdx := btcAnchors[0].OutputIndex
	require.Less(t, anchorIdx, len(anchorPsbt.UnsignedTx.TxOut))
	require.Equal(t, int64(0), anchorPsbt.UnsignedTx.TxOut[anchorIdx].Value)

	digest, err := builder.GetKeySpendSigHash(0)
	require.NoError(t, err)

	_, _, taprootRoot, err := builder.GetTaprootRoots(0, "")
	require.NoError(t, err)

	// Set up the taproot tweak for MuSig2 signing.
	allPubKeys := []*btcec.PublicKey{
		f.userKey.PubKey(), f.operatorKey.PubKey(),
	}
	tweaks := &input.MuSig2Tweaks{
		TaprootBIP0086Tweak: false,
		TaprootTweak:        taprootRoot,
	}

	// Create signers and sessions for both parties.
	userSigner := assets.NewLocalMuSig2Signer(f.userKey)
	userSession, err := userSigner.MuSig2CreateSession(
		input.MuSig2Version100RC2, keychain.KeyLocator{},
		allPubKeys, tweaks, nil, nil,
	)
	require.NoError(t, err)

	operatorSigner := assets.NewLocalMuSig2Signer(f.operatorKey)
	operatorSession, err := operatorSigner.MuSig2CreateSession(
		input.MuSig2Version100RC2, keychain.KeyLocator{},
		allPubKeys, tweaks, nil, nil,
	)
	require.NoError(t, err)

	// Exchange nonces.
	_, err = userSigner.MuSig2RegisterNonces(
		userSession.SessionID,
		[][musig2.PubNonceSize]byte{operatorSession.PublicNonce},
	)
	require.NoError(t, err)

	_, err = operatorSigner.MuSig2RegisterNonces(
		operatorSession.SessionID,
		[][musig2.PubNonceSize]byte{userSession.PublicNonce},
	)
	require.NoError(t, err)

	// Both parties create partial signatures. MuSig2Sign registers the
	// signature internally in the session.
	_, err = userSigner.MuSig2Sign(userSession.SessionID, digest, false)
	require.NoError(t, err)
	operatorPartial, err := operatorSigner.MuSig2Sign(
		operatorSession.SessionID, digest, false,
	)
	require.NoError(t, err)

	// Combine signatures. Pass only the other party's signature since our
	// signature is already included from MuSig2Sign.
	finalSig, haveAll, err := userSigner.MuSig2CombineSig(
		userSession.SessionID,
		[]*musig2.PartialSignature{operatorPartial},
	)
	require.NoError(t, err)
	require.True(t, haveAll)

	err = builder.ApplyKeySpendSignature(0, finalSig.Serialize())
	require.NoError(t, err)

	finalPsbt, err := builder.FinalizeAnchor(ctx, h.LND.WalletKit)
	require.NoError(t, err)

	require.NoError(t, psbt.MaybeFinalizeAll(finalPsbt))

	anchorTx, err := psbt.Extract(finalPsbt)
	require.NoError(t, err)
	anchorWeight := blockchain.GetTransactionWeight(btcutil.NewTx(anchorTx))
	t.Logf("anchor version=%d weight=%d", anchorTx.Version, anchorWeight)
	anchorHash := anchorTx.TxHash().String()
	t.Logf("anchor txid=%s", anchorHash)
	for idx, txIn := range anchorTx.TxIn {
		t.Logf("anchor input %d prev=%s:%d", idx,
			txIn.PreviousOutPoint.Hash, txIn.PreviousOutPoint.Index)
		t.Logf("anchor input %d sequence=%d", idx, txIn.Sequence)
	}
	for idx, out := range anchorTx.TxOut {
		t.Logf("anchor output %d value=%d", idx, out.Value)
	}

	_, err = builder.Publish(
		ctx, f.operatorClient.TapdClient, "ark-zero-fee",
		assets.PublishOptions{SkipBroadcast: true},
	)
	require.NoError(t, err)

	btcClient, err := h.BitcoindClient()
	require.NoError(t, err)

	rpcClient, err := h.BitcoinRPCClient()
	require.NoError(t, err)
	t.Cleanup(func() { rpcClient.Shutdown() })

	changeAddr, err := rpcClient.GetNewAddress("")
	require.NoError(t, err)

	walletShim := newWalletKitFundingShim(h.LND.WalletKit)
	childPsbt, childTx, err := builder.BuildAnchorChild(ctx, walletShim,
		assets.AnchorChildOptions{
			ChangeAddress: changeAddr,
			FeeRate:       chainfee.SatPerKWeight(10_000),
		},
	)
	require.NoError(t, err)
	require.NotNil(t, childPsbt)

	childWeight := blockchain.GetTransactionWeight(btcutil.NewTx(childTx))
	t.Logf("child version=%d weight=%d", childTx.Version, childWeight)

	childHash := childTx.TxHash().String()
	t.Logf("child txid=%s", childHash)

	for idx, txIn := range childTx.TxIn {
		t.Logf("child input %d prev=%s:%d", idx,
			txIn.PreviousOutPoint.Hash, txIn.PreviousOutPoint.Index)
	}

	var feeInputs, feeOutputs int64
	for i, in := range childPsbt.Inputs {
		switch {
		case in.WitnessUtxo != nil:
			feeInputs += in.WitnessUtxo.Value

		case in.NonWitnessUtxo != nil:
			prevIdx := childPsbt.UnsignedTx.TxIn[i].
				PreviousOutPoint.Index
			feeInputs += in.NonWitnessUtxo.TxOut[prevIdx].Value
		}
	}

	for idx, out := range childTx.TxOut {
		feeOutputs += out.Value
		t.Logf("child output %d value=%d", idx, out.Value)
	}
	t.Logf("child fee=%d sats", feeInputs-feeOutputs)

	anchorHex := txToHex(anchorTx)
	t.Logf("anchor hex=%s", anchorHex)

	childHex := txToHex(childTx)
	t.Logf("child hex=%s", childHex)

	submitPackageRes, err := btcClient.SubmitPackage(
		[]*wire.MsgTx{anchorTx}, childTx, nil,
	)
	t.Logf("Submitted zero-fee anchor package to bitcoind, result: %v",
		spew.Sdump(submitPackageRes))

	require.NoError(t, err)
	require.NotNil(t, submitPackageRes)
	for txid, res := range submitPackageRes.TxResults {
		if res.Error != nil {
			t.Fatalf("package tx %s rejected: %s", txid, *res.Error)
		}
	}
	t.Logf("SubmitPackage accepted package into mempool")

	h.GenerateAndWait(1)
}

func txToHex(tx *wire.MsgTx) string {
	var buf bytes.Buffer
	_ = tx.Serialize(&buf)
	return hex.EncodeToString(buf.Bytes())
}
