package assets_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightninglabs/darepo-client/assets"
	"github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/taproot-assets/address"
	tapasset "github.com/lightninglabs/taproot-assets/asset"
	tapcommitment "github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/rpcutils"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightninglabs/taproot-assets/tapscript"
	"github.com/lightninglabs/taproot-assets/vm"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Test Constants and Helpers
// ============================================================================

const (
	defaultTimeout  = 30 * time.Second
	testAssetAmount = 100_000
)

// testKeyFromSeed generates a deterministic private key from a seed byte.
func testKeyFromSeed(t *testing.T, seed byte) *btcec.PrivateKey {
	t.Helper()

	var privKeyBytes [32]byte
	for i := range privKeyBytes {
		privKeyBytes[i] = seed
	}

	privKey, _ := btcec.PrivKeyFromBytes(privKeyBytes[:])

	return privKey
}

// walletKitFundingShim adapts lndclient.WalletKitClient for CPFP child funding.
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

func txToHex(tx *wire.MsgTx) string {
	var buf bytes.Buffer
	_ = tx.Serialize(&buf)

	return hex.EncodeToString(buf.Bytes())
}

func setPsbtInputKeyDesc(packet *psbt.Packet, inputIndex int,
	keyDesc keychain.KeyDescriptor, coinType uint32) error {

	if packet == nil {
		return fmt.Errorf("packet is nil")
	}

	if inputIndex < 0 || inputIndex >= len(packet.Inputs) {
		return fmt.Errorf("input index %d out of range", inputIndex)
	}

	deriv, taprootDeriv, _ := btcwallet.Bip32DerivationFromKeyDesc(
		keyDesc, coinType,
	)

	packet.Inputs[inputIndex].Bip32Derivation = []*psbt.Bip32Derivation{
		deriv,
	}
	packet.Inputs[inputIndex].TaprootBip32Derivation =
		[]*psbt.TaprootBip32Derivation{
			taprootDeriv,
		}

	return nil
}

// witnessProgramFromOpTrueWitness extracts the output key from a taproot
// script-path witness.
//
//nolint:unused
func witnessProgramFromOpTrueWitness(wit wire.TxWitness) ([]byte, error) {
	if len(wit) != 2 {
		return nil, fmt.Errorf("expected 2-item OP_TRUE witness, "+
			"got %d", len(wit))
	}

	tapScript := wit[0]
	controlBlockBytes := wit[1]

	controlBlock, err := txscript.ParseControlBlock(controlBlockBytes)
	if err != nil {
		return nil, fmt.Errorf("parse control block: %w", err)
	}

	tapLeaf := txscript.NewBaseTapLeaf(tapScript)
	rootHash := tapLeaf.TapHash()

	outputKey := txscript.ComputeTaprootOutputKey(
		controlBlock.InternalKey, rootHash[:],
	)

	return schnorr.SerializePubKey(outputKey), nil
}

// ============================================================================
// Basic Builder Tests
// ============================================================================

// TestAssetTxBuilderBasicSweep demonstrates the simplest complete flow through
// the AssetTxBuilder without OnboardingKit. It mints an asset, sends it to a
// standard tapd address, then sweeps it to an OP_TRUE output using the builder.
// This exercises the tapd-managed key path where PopulateTapdKeyInfo queries
// tapd for BIP32 derivation info so LND can sign.
func TestAssetTxBuilderBasicSweep(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	opts := harness.DefaultOptions()
	opts.StartTapd = true
	opts.GroupName = "asset-builder-basic-sweep"

	h := harness.NewHarness(t, &opts)
	h.Start()
	t.Cleanup(h.Stop)

	chainParams := address.ParamsForChain(
		chaincfg.RegressionNetParams.Name,
	)

	// Create a single tapd node with alice (mints and sends to self).
	alice := h.NewTapdHarness("alice")
	t.Cleanup(alice.Stop)
	aliceClient := alice.NewTapClientHarness()
	t.Cleanup(aliceClient.Close)

	// Fund alice.
	harness.FundNode(h, alice.LND)

	// Mint asset with alice.
	asset := aliceClient.MintAsset(
		"BUX", testAssetAmount, taprpc.AssetType_NORMAL,
	)
	require.NotNil(t, asset)

	var assetID tapasset.ID
	copy(assetID[:], asset.AssetGenesis.AssetId)

	ctx, cancel := context.WithTimeout(t.Context(), defaultTimeout)
	defer cancel()

	// Create a standard tapd address and send asset to self. This creates
	// a tapd-managed output that we'll spend using the builder.
	receiveAddr, err := aliceClient.NewAddr(ctx, &taprpc.NewAddrRequest{
		AssetId:        assetID[:],
		Amt:            testAssetAmount,
		AssetVersion:   taprpc.AssetVersion_ASSET_VERSION_V1,
		AddressVersion: taprpc.AddrVersion_ADDR_VERSION_V1,
	})
	require.NoError(t, err)
	t.Logf("Receive address: %s", receiveAddr.Encoded)
	t.Logf("Receive address internal key: %x", receiveAddr.InternalKey)

	// Send asset to the receive address.
	aliceClient.SendAsset(receiveAddr.Encoded)
	h.GenerateAndWait(1)

	// Wait for the receive to complete.
	aliceClient.WaitForAssetBalance(assetID[:], testAssetAmount)

	// Get the proof for the received asset.
	proofResp, err := aliceClient.ExportProof(
		ctx, &taprpc.ExportProofRequest{
			AssetId:   assetID[:],
			ScriptKey: receiveAddr.ScriptKey,
		},
	)
	require.NoError(t, err)
	require.NotEmpty(t, proofResp.RawProofFile)
	t.Logf("Got proof file (len=%d)", len(proofResp.RawProofFile))

	// Use InputConfigFromProof to create the input config. This extracts
	// the internal key and sets mode to AnchorKeyModeTapdManaged.
	inputCfg, err := assets.InputConfigFromProof(proofResp.RawProofFile)
	require.NoError(t, err)
	require.Equal(
		t, assets.AnchorKeyModeTapdManaged, inputCfg.AnchorKey.Mode,
	)
	t.Logf("Input internal key from proof: %x", inputCfg.AnchorKey.Key)

	// Build a sweep transaction using the builder.
	builder := assets.NewAssetTxBuilder(assetID, &chainParams)

	err = builder.AddAssetInput(inputCfg)
	require.NoError(t, err)

	// Sweep to an OP_TRUE output for simplicity. Use NUMS as the anchor
	// internal key since this is an anyone-can-spend output.
	err = builder.AddAssetOutput(assets.OutputConfig{
		Amount: testAssetAmount,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(tapasset.NUMSPubKey),
		},
		Script: assets.OpTrueScript(),
	})
	require.NoError(t, err)

	_, err = builder.Compile(ctx)
	require.NoError(t, err)

	err = builder.Commit(
		ctx, aliceClient.AssetWalletClient,
		assets.CommitOptions{FeeRate: chainfee.SatPerVByte(10)},
	)
	require.NoError(t, err)

	// Sign the active virtual packet. When spending tapd-managed script
	// keys, tapd needs to attach the virtual input witness as it holds
	// the key material.
	err = builder.SignVirtualPackets(ctx, aliceClient.AssetWalletClient)
	require.NoError(t, err)

	// Populate tapd key info so LND can sign. This queries tapd for the key
	// locator and populates TaprootBip32Derivation in the PSBT.
	err = builder.PopulateTapdKeyInfo(
		ctx, aliceClient.TapdClient,
		chaincfg.RegressionNetParams.HDCoinType,
	)
	require.NoError(t, err)

	// Verify the PSBT now has derivation info.
	anchorPsbt := builder.AnchorPsbt()
	require.NotNil(t, anchorPsbt)
	require.NotEmpty(t, anchorPsbt.Inputs)
	require.NotEmpty(t, anchorPsbt.Inputs[0].TaprootBip32Derivation,
		"TaprootBip32Derivation should be populated")
	taprootBip32 := anchorPsbt.Inputs[0].TaprootBip32Derivation[0]
	require.NotEmpty(t, taprootBip32.Bip32Path,
		"Bip32Path should be populated")
	t.Logf("PSBT input 0 has BIP32 derivation path: %v",
		anchorPsbt.Inputs[0].TaprootBip32Derivation[0].Bip32Path)

	// Finalize the anchor - LND should be able to sign now that it has
	// the key derivation info.
	_, err = builder.FinalizeAnchor(ctx, alice.LND.WalletKit)
	require.NoError(t, err)
	t.Logf("FinalizeAnchor succeeded - LND was able to sign")

	// Publish the transfer.
	resp, err := builder.Publish(
		ctx, aliceClient.TapdClient, "basic-sweep",
		assets.PublishOptions{},
	)
	require.NoError(t, err)
	t.Logf("Published basic sweep: %x", resp.Transfer.AnchorTxHash)

	// Mine the transaction.
	h.GenerateAndWait(1)
	t.Logf("Test completed successfully")
}

// ============================================================================
// Zero-Fee Anchor and Package Relay Tests
// ============================================================================

// TestAssetTxBuilderZeroFeeAnchor tests the zero-fee anchor package relay flow.
// It creates an asset transfer with a zero-fee anchor, builds a CPFP child
// transaction, and submits both as a package to bitcoind.
//
// This test uses standard tapd addresses (not OnboardingKit) to demonstrate
// the builder's ability to handle tapd-managed keys with ephemeral anchors.
func TestAssetTxBuilderZeroFeeAnchor(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	opts := harness.DefaultOptions()
	opts.StartTapd = true
	opts.GroupName = "asset-zero-fee-anchor"

	h := harness.NewHarness(t, &opts)
	h.Start()
	t.Cleanup(h.Stop)

	chainParams := address.ParamsForChain(
		chaincfg.RegressionNetParams.Name,
	)

	// Create alice who will mint and hold the asset.
	alice := h.NewTapdHarness("alice")
	t.Cleanup(alice.Stop)
	aliceClient := alice.NewTapClientHarness()
	t.Cleanup(aliceClient.Close)

	// Fund alice's LND wallet.
	harness.FundNode(h, alice.LND)

	ctx, cancel := context.WithTimeout(t.Context(), defaultTimeout)
	defer cancel()

	// Mint asset with alice.
	asset := aliceClient.MintAsset(
		"BUX", testAssetAmount, taprpc.AssetType_NORMAL,
	)
	require.NotNil(t, asset)

	var assetID tapasset.ID
	copy(assetID[:], asset.AssetGenesis.AssetId)

	// Create a standard tapd address and send to self.
	receiveAddr, err := aliceClient.NewAddr(ctx, &taprpc.NewAddrRequest{
		AssetId:        assetID[:],
		Amt:            testAssetAmount,
		AssetVersion:   taprpc.AssetVersion_ASSET_VERSION_V1,
		AddressVersion: taprpc.AddrVersion_ADDR_VERSION_V1,
	})
	require.NoError(t, err)
	t.Logf("Receive address: %s", receiveAddr.Encoded)

	aliceClient.SendAsset(receiveAddr.Encoded)
	h.GenerateAndWait(1)

	// Wait for the transfer to complete.
	aliceClient.WaitForAssetBalance(assetID[:], testAssetAmount)

	// Export the proof for the received asset.
	proofResp, err := aliceClient.ExportProof(
		ctx, &taprpc.ExportProofRequest{
			AssetId:   assetID[:],
			ScriptKey: receiveAddr.ScriptKey,
		},
	)
	require.NoError(t, err)

	// Create input config from proof (tapd-managed key).
	inputCfg, err := assets.InputConfigFromProof(proofResp.RawProofFile)
	require.NoError(t, err)

	// Build a sweep transaction with zero-fee anchor.
	builder := assets.NewAssetTxBuilder(assetID, &chainParams)

	err = builder.AddAssetInput(inputCfg)
	require.NoError(t, err)

	// Output to OP_TRUE with NUMS anchor key.
	err = builder.AddAssetOutput(assets.OutputConfig{
		Amount: testAssetAmount,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(tapasset.NUMSPubKey),
		},
		Script: assets.OpTrueScript(),
	})
	require.NoError(t, err)

	// Add ephemeral anchor for CPFP.
	builder.AddEphemeralAnchor()

	plan, err := builder.Compile(ctx)
	require.NoError(t, err)
	require.NotNil(t, plan.EphemeralAnchor)

	// Commit with skip wallet funding (zero-fee tx).
	commitOpts := assets.CommitOptions{SkipWalletFunding: true}
	err = builder.Commit(ctx, aliceClient.AssetWalletClient, commitOpts)
	require.NoError(t, err)

	// Sign the active virtual packet so tapd attaches the TAP VM input
	// witness for the tapd-managed script key.
	err = builder.SignVirtualPackets(ctx, aliceClient.AssetWalletClient)
	require.NoError(t, err)

	ephemeralAnchor := builder.EphemeralAnchor()
	require.NotNil(t, ephemeralAnchor)
	require.GreaterOrEqual(t, ephemeralAnchor.OutputIndex, 0)

	anchorPsbt := builder.AnchorPsbt()
	require.NotNil(t, anchorPsbt)
	anchorIdx := ephemeralAnchor.OutputIndex
	require.Less(t, anchorIdx, len(anchorPsbt.UnsignedTx.TxOut))
	require.Equal(t, int64(0), anchorPsbt.UnsignedTx.TxOut[anchorIdx].Value)

	// Populate tapd key info so LND can sign.
	err = builder.PopulateTapdKeyInfo(
		ctx, aliceClient.TapdClient,
		chaincfg.RegressionNetParams.HDCoinType,
	)
	require.NoError(t, err)

	// Finalize the anchor PSBT.
	finalPsbt, err := builder.FinalizeAnchor(ctx, alice.LND.WalletKit)
	require.NoError(t, err)
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

	// Skip broadcast for now - we'll use package relay.
	_, err = builder.Publish(
		ctx, aliceClient.TapdClient, "ark-zero-fee",
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

	walletShim := newWalletKitFundingShim(alice.LND.WalletKit)
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

// ============================================================================
// Chained Sweep Tests
// ============================================================================

// TestAssetTxBuilderChainedSweeps tests two chained asset sweeps.
// The test builds and broadcasts both packages together, updates proofs
// with block metadata, sweeps back to wallet, and verifies balances.
//
// Flow:
// 1. Mint asset and send to tapd address
// 2. First sweep: tapd-managed → OP_TRUE (zero-fee + CPFP)
// 3. Second sweep: OP_TRUE → OP_TRUE (zero-fee + CPFP, using unconfirmed proof)
// 4. Submit packages, mine, update proofs
// 5. Sweep back to wallet and verify balance.
func TestAssetTxBuilderChainedSweeps(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	opts := harness.DefaultOptions()
	opts.StartTapd = true
	opts.GroupName = "chained-asset-sweeps"

	h := harness.NewHarness(t, &opts)
	h.Start()
	t.Cleanup(h.Stop)

	chainParams := address.ParamsForChain(
		chaincfg.RegressionNetParams.Name,
	)

	// Create alice who will mint and manage the asset.
	alice := h.NewTapdHarness("alice")
	t.Cleanup(alice.Stop)
	aliceClient := alice.NewTapClientHarness()
	t.Cleanup(aliceClient.Close)

	// Fund alice's LND wallet and harness LND for CPFP children.
	harness.FundNode(h, alice.LND)
	harness.FundNode(h, h.LND)

	// Use a longer timeout since test has multiple sweeps.
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	// Mint asset with alice.
	asset := aliceClient.MintAsset(
		"BUX", testAssetAmount, taprpc.AssetType_NORMAL,
	)
	require.NotNil(t, asset)

	var assetID tapasset.ID
	copy(assetID[:], asset.AssetGenesis.AssetId)

	// Create a standard tapd address and send to self first.
	receiveAddr, err := aliceClient.NewAddr(ctx, &taprpc.NewAddrRequest{
		AssetId: assetID[:],
		Amt:     testAssetAmount,
		// Use V1 assets so we can materialize and attach the strippable
		// TxWitnesses ourselves when constructing chained proofs.
		AssetVersion: taprpc.AssetVersion_ASSET_VERSION_V1,
	})
	require.NoError(t, err)

	aliceClient.SendAsset(receiveAddr.Encoded)
	h.GenerateAndWait(1)
	aliceClient.WaitForAssetBalance(assetID[:], testAssetAmount)

	// Export the proof for the received asset.
	proofResp, err := aliceClient.ExportProof(
		ctx, &taprpc.ExportProofRequest{
			AssetId:   assetID[:],
			ScriptKey: receiveAddr.ScriptKey,
		},
	)
	require.NoError(t, err)

	// ====================================================================
	// First Sweep: tapd-managed → OP_TRUE(destKey1) with zero-fee anchor.
	// ====================================================================

	builder1 := assets.NewAssetTxBuilder(assetID, &chainParams)

	// Use InputConfigFromProof for tapd-managed input.
	inputCfg1, err := assets.InputConfigFromProof(proofResp.RawProofFile)
	require.NoError(t, err)

	err = builder1.AddAssetInput(inputCfg1)
	require.NoError(t, err)

	// Generate a local key for the first sweep output.
	destKey1, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	err = builder1.AddAssetOutput(assets.OutputConfig{
		Amount: testAssetAmount,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(destKey1.PubKey()),
		},
		Script: assets.OpTrueUniqueScript(destKey1.PubKey()),
	})
	require.NoError(t, err)

	// Add zero-fee anchor.
	builder1.AddEphemeralAnchor()

	_, err = builder1.Compile(ctx)
	require.NoError(t, err)

	err = builder1.Commit(
		ctx, aliceClient.AssetWalletClient,
		assets.CommitOptions{SkipWalletFunding: true},
	)
	require.NoError(t, err)

	// Sign the virtual packet with tapd so the resulting proof contains a
	// valid V1 witness for the tapd-managed input.
	err = builder1.SignVirtualPackets(ctx, aliceClient.AssetWalletClient)
	require.NoError(t, err)

	// Populate tapd key info so LND can sign the tapd-managed input.
	err = builder1.PopulateTapdKeyInfo(
		ctx, aliceClient.TapdClient,
		chaincfg.RegressionNetParams.HDCoinType,
	)
	require.NoError(t, err)

	// Finalize anchor.
	finalPsbt1, err := builder1.FinalizeAnchor(ctx, alice.LND.WalletKit)
	require.NoError(t, err)

	require.NoError(t, psbt.MaybeFinalizeAll(finalPsbt1))

	anchorTx1, err := psbt.Extract(finalPsbt1)
	require.NoError(t, err)

	// ====================================================================
	// Build CPFP child for first anchor.
	// ====================================================================

	btcClient, err := h.BitcoindClient()
	require.NoError(t, err)

	rpcClient, err := h.BitcoinRPCClient()
	require.NoError(t, err)
	t.Cleanup(func() { rpcClient.Shutdown() })

	changeAddr, err := rpcClient.GetNewAddress("")
	require.NoError(t, err)

	// Use harness LND for CPFP child funding.
	walletShim := newWalletKitFundingShim(h.LND.WalletKit)

	childPsbt1, childTx1, err := builder1.BuildAnchorChild(
		ctx, walletShim,
		assets.AnchorChildOptions{
			ChangeAddress: changeAddr,
			FeeRate:       chainfee.SatPerKWeight(10_000),
		},
	)
	require.NoError(t, err)
	require.NotNil(t, childPsbt1)

	// Generate unconfirmed proof for chaining into the second sweep.
	unconfirmedProof1, err := builder1.Proof(0, nil)
	require.NoError(t, err)

	// ====================================================================
	// Second Sweep: OP_TRUE(destKey1) → OP_TRUE(destKey2), zero-fee anchor.
	// ====================================================================

	builder2 := assets.NewAssetTxBuilder(assetID, &chainParams)

	// Decode proof to get taproot tweak for signing.
	proofFile1, err := proof.DecodeFile(unconfirmedProof1)
	require.NoError(t, err)
	lastProof1, err := proofFile1.LastProof()
	require.NoError(t, err)

	// Use the unconfirmed proof as input with static key (we control it).
	err = builder2.AddAssetInput(assets.InputConfig{
		ProofFile: unconfirmedProof1,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(destKey1.PubKey()),
		},
	})
	require.NoError(t, err)

	// Generate key for second sweep output.
	destKey2, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	err = builder2.AddAssetOutput(assets.OutputConfig{
		Amount: testAssetAmount,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(destKey2.PubKey()),
		},
		Script: assets.OpTrueUniqueScript(destKey2.PubKey()),
	})
	require.NoError(t, err)

	// Add zero-fee anchor for second sweep.
	builder2.AddEphemeralAnchor()

	_, err = builder2.Compile(ctx)
	require.NoError(t, err)

	err = builder2.Commit(
		ctx, aliceClient.AssetWalletClient,
		assets.CommitOptions{SkipWalletFunding: true},
	)
	require.NoError(t, err)

	// Sign with the tweaked destKey1.
	taprootTweak2, err := assets.GenTaprootRootFromProof(lastProof1)
	require.NoError(t, err)

	tweakedKey1 := txscript.TweakTaprootPrivKey(*destKey1, taprootTweak2)

	digest2, err := builder2.GetKeySpendSigHash(0)
	require.NoError(t, err)

	sig2, err := schnorr.Sign(tweakedKey1, digest2[:])
	require.NoError(t, err)

	err = builder2.ApplyKeySpendSignature(0, sig2.Serialize())
	require.NoError(t, err)

	finalPsbt2, err := builder2.FinalizeAnchor(ctx, alice.LND.WalletKit)
	require.NoError(t, err)

	require.NoError(t, psbt.MaybeFinalizeAll(finalPsbt2))

	anchorTx2, err := psbt.Extract(finalPsbt2)
	require.NoError(t, err)

	// Capture transfer data for proof construction after broadcast.
	transferData2, err := builder2.GetTransferData()
	require.NoError(t, err)

	// ====================================================================
	// Build CPFP child for second anchor.
	// ====================================================================

	childPsbt2, childTx2, err := builder2.BuildAnchorChild(
		ctx, walletShim,
		assets.AnchorChildOptions{
			ChangeAddress: changeAddr,
			FeeRate:       chainfee.SatPerKWeight(10_000),
		},
	)
	require.NoError(t, err)
	require.NotNil(t, childPsbt2)

	// ====================================================================
	// Broadcast packages in dependency order and update proofs.
	// ====================================================================

	// Submit package for first sweep.
	submitRes1, err := btcClient.SubmitPackage(
		[]*wire.MsgTx{anchorTx1}, childTx1, nil,
	)
	require.NoError(t, err)
	require.NotNil(t, submitRes1)
	for txid, res := range submitRes1.TxResults {
		if res.Error != nil {
			t.Fatalf("tx %s rejected: %s", txid, *res.Error)
		}
	}

	// Mine first sweep and confirm proof.
	minedBlocks1 := h.GenerateAndWait(1)
	minedBlock1 := minedBlocks1[0]

	blockHash1, err := chainhash.NewHashFromStr(minedBlock1.Header.Hash)
	require.NoError(t, err)

	rawBlock1, err := rpcClient.GetBlock(blockHash1)
	require.NoError(t, err)

	var txIndex1 int
	var found1 bool
	anchorTx1Hash := anchorTx1.TxHash()
	for i, tx := range rawBlock1.Transactions {
		txHash := tx.TxHash()
		if txHash.IsEqual(&anchorTx1Hash) {
			txIndex1 = i
			found1 = true

			break
		}
	}
	require.True(t, found1, "anchor1 not found in mined block")

	// For the first sweep, the input is a tapd-managed BIP86 key. Since we
	// called Publish() before mining, tapd already registered the transfer
	// and will automatically update its proof when the block is mined.
	//
	// We still need the confirmed proof for the second sweep's
	// BuildProofFromTransferData call, so we generate it here.
	confirmedProof1, err := builder1.Proof(0, &assets.ProofParams{
		Block:       rawBlock1,
		BlockHeight: uint32(minedBlock1.Header.Height),
		TxIndex:     txIndex1,
	})
	require.NoError(t, err)

	proofFile1Confirmed, err := proof.DecodeFile(confirmedProof1)
	require.NoError(t, err)
	lastProof1Confirmed, err := proofFile1Confirmed.LastProof()
	require.NoError(t, err)

	genesisPoint1 := lastProof1Confirmed.Asset.Genesis.FirstPrevOut.String()
	verifyResp1, err := aliceClient.TapdClient.VerifyProof(
		ctx, &taprpc.ProofFile{
			RawProofFile: confirmedProof1,
			GenesisPoint: genesisPoint1,
		},
	)
	require.NoError(t, err)
	require.True(t, verifyResp1.Valid)

	// Submit package for second sweep.
	submitRes2, err := btcClient.SubmitPackage(
		[]*wire.MsgTx{anchorTx2}, childTx2, nil,
	)
	require.NoError(t, err)
	require.NotNil(t, submitRes2)
	for txid, res := range submitRes2.TxResults {
		if res.Error != nil {
			t.Fatalf("tx %s rejected: %s", txid, *res.Error)
		}
	}

	// Mine second sweep and update proof.
	minedBlocks2 := h.GenerateAndWait(1)
	minedBlock2 := minedBlocks2[0]

	blockHash2, err := chainhash.NewHashFromStr(minedBlock2.Header.Hash)
	require.NoError(t, err)

	rawBlock2, err := rpcClient.GetBlock(blockHash2)
	require.NoError(t, err)

	var txIndex2 int
	var found2 bool
	anchorTx2Hash := anchorTx2.TxHash()
	for i, tx := range rawBlock2.Transactions {
		txHash := tx.TxHash()
		if txHash.IsEqual(&anchorTx2Hash) {
			txIndex2 = i
			found2 = true

			break
		}
	}
	require.True(t, found2, "anchor2 not found in mined block")

	finalProof2, err := assets.BuildProofFromTransferData(
		transferData2, [][]byte{confirmedProof1}, 0,
		&assets.ProofParams{
			Block:       rawBlock2,
			BlockHeight: uint32(minedBlock2.Header.Height),
			TxIndex:     txIndex2,
		},
	)
	require.NoError(t, err)

	proofFile2, err := proof.DecodeFile(finalProof2)
	require.NoError(t, err)
	lastProof2, err := proofFile2.LastProof()
	require.NoError(t, err)

	genesisPoint2 := lastProof2.Asset.Genesis.FirstPrevOut.String()
	verifyResp2, err := aliceClient.TapdClient.VerifyProof(
		ctx, &taprpc.ProofFile{
			RawProofFile: finalProof2,
			GenesisPoint: genesisPoint2,
		},
	)
	require.NoError(t, err)
	require.True(t, verifyResp2.Valid)

	// ====================================================================
	// Sweep back to wallet.
	// ====================================================================

	builder3 := assets.NewAssetTxBuilder(assetID, &chainParams)

	// Use the second sweep's proof as input.
	err = builder3.AddAssetInput(assets.InputConfig{
		ProofFile: finalProof2,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(destKey2.PubKey()),
		},
	})
	require.NoError(t, err)

	// Derive wallet script key for final output.
	walletScriptKey, walletInternalKey, err := aliceClient.TapdClient.
		DeriveNewKeys(ctx)
	require.NoError(t, err)

	err = builder3.AddAssetOutput(assets.OutputConfig{
		Amount: testAssetAmount,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeTapdManaged,
			Key:  walletInternalKey.PubKey.SerializeCompressed(),
		},
		Script: assets.DirectWalletScript(&walletScriptKey),
	})
	require.NoError(t, err)

	_, err = builder3.Compile(ctx)
	require.NoError(t, err)

	err = builder3.Commit(
		ctx, aliceClient.AssetWalletClient,
		assets.CommitOptions{FeeRate: chainfee.SatPerVByte(10)},
	)
	require.NoError(t, err)

	// Sign with the tweaked destKey2.
	proofFile2ForSpend, err := proof.DecodeFile(finalProof2)
	require.NoError(t, err)
	lastProof2ForSpend, err := proofFile2ForSpend.LastProof()
	require.NoError(t, err)

	taprootTweak3, err := assets.GenTaprootRootFromProof(
		lastProof2ForSpend,
	)
	require.NoError(t, err)

	tweakedKey2 := txscript.TweakTaprootPrivKey(*destKey2, taprootTweak3)

	digest3, err := builder3.GetKeySpendSigHash(0)
	require.NoError(t, err)

	sig3, err := schnorr.Sign(tweakedKey2, digest3[:])
	require.NoError(t, err)

	err = builder3.ApplyKeySpendSignature(0, sig3.Serialize())
	require.NoError(t, err)

	_, err = builder3.FinalizeAnchor(ctx, alice.LND.WalletKit)
	require.NoError(t, err)

	transferData3, err := builder3.GetTransferData()
	require.NoError(t, err)

	_, err = builder3.Publish(
		ctx, aliceClient.TapdClient, "sweep-to-wallet",
		assets.PublishOptions{},
	)
	require.NoError(t, err)

	// Mine and verify balance.
	minedBlocks3 := h.GenerateAndWait(1)
	minedBlock3 := minedBlocks3[0]

	blockHash3, err := chainhash.NewHashFromStr(minedBlock3.Header.Hash)
	require.NoError(t, err)

	rawBlock3, err := rpcClient.GetBlock(blockHash3)
	require.NoError(t, err)

	anchorPsbt3 := builder3.AnchorPsbt()
	require.NotNil(t, anchorPsbt3)
	anchorTx3, err := psbt.Extract(anchorPsbt3)
	require.NoError(t, err)

	var txIndex3 int
	var found3 bool
	anchorTx3Hash := anchorTx3.TxHash()
	for i, tx := range rawBlock3.Transactions {
		txHash := tx.TxHash()
		if txHash.IsEqual(&anchorTx3Hash) {
			txIndex3 = i
			found3 = true

			break
		}
	}
	require.True(t, found3, "anchor3 not found in mined block")

	finalProof3, err := assets.BuildProofFromTransferData(
		transferData3, [][]byte{finalProof2}, 0,
		&assets.ProofParams{
			Block:       rawBlock3,
			BlockHeight: uint32(minedBlock3.Header.Height),
			TxIndex:     txIndex3,
		},
	)
	require.NoError(t, err)

	_, err = aliceClient.TapdClient.ImportProofFile(ctx, finalProof3)
	require.NoError(t, err)

	// Verify final balance.
	require.Eventually(t, func() bool {
		checkCtx, cancel := context.WithTimeout(
			t.Context(), defaultTimeout,
		)
		defer cancel()

		allTypes := &taprpc.ScriptKeyTypeQuery_AllTypes{
			AllTypes: true,
		}
		balances, err := aliceClient.ListBalances(
			checkCtx, &taprpc.ListBalancesRequest{
				GroupBy: &taprpc.ListBalancesRequest_AssetId{
					AssetId: true,
				},
				AssetFilter: assetID[:],
				ScriptKeyType: &taprpc.ScriptKeyTypeQuery{
					Type: allTypes,
				},
			},
		)
		if err != nil {
			return false
		}

		assetKey := hex.EncodeToString(assetID[:])
		balanceEntry, ok := balances.AssetBalances[assetKey]

		return ok && balanceEntry.Balance == testAssetAmount
	}, defaultTimeout, 200*time.Millisecond, "asset balance did not update")
}

// ============================================================================
// Script Closure Tests
// ============================================================================

// TestAssetTxBuilderScriptClosureCSV tests spending an asset via CSV timeout
// script path. The asset is sent to a NUMS internal key with a CSV closure,
// then swept after the timelock expires.
func TestAssetTxBuilderScriptClosureCSV(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	opts := harness.DefaultOptions()
	opts.StartTapd = true
	opts.GroupName = "asset-script-csv"

	h := harness.NewHarness(t, &opts)
	h.Start()
	t.Cleanup(h.Stop)

	chainParams := address.ParamsForChain(
		chaincfg.RegressionNetParams.Name,
	)

	alice := h.NewTapdHarness("alice")
	t.Cleanup(alice.Stop)
	aliceClient := alice.NewTapClientHarness()
	t.Cleanup(aliceClient.Close)

	harness.FundNode(h, alice.LND)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// Mint asset.
	asset := aliceClient.MintAsset(
		"CSV", testAssetAmount, taprpc.AssetType_NORMAL,
	)
	require.NotNil(t, asset)

	var assetID tapasset.ID
	copy(assetID[:], asset.AssetGenesis.AssetId)

	// Send asset to tapd address first.
	receiveAddr, err := aliceClient.NewAddr(ctx, &taprpc.NewAddrRequest{
		AssetId:        assetID[:],
		Amt:            testAssetAmount,
		AssetVersion:   taprpc.AssetVersion_ASSET_VERSION_V1,
		AddressVersion: taprpc.AddrVersion_ADDR_VERSION_V1,
	})
	require.NoError(t, err)
	aliceClient.SendAsset(receiveAddr.Encoded)
	h.GenerateAndWait(1)
	aliceClient.WaitForAssetBalance(assetID[:], testAssetAmount)

	// Export proof for the received asset.
	proofResp, err := aliceClient.ExportProof(
		ctx, &taprpc.ExportProofRequest{
			AssetId:   assetID[:],
			ScriptKey: receiveAddr.ScriptKey,
		},
	)
	require.NoError(t, err)

	inputCfg, err := assets.InputConfigFromProof(proofResp.RawProofFile)
	require.NoError(t, err)

	// Create a CSV closure with a short delay (2 blocks).
	csvDelay := uint32(2)
	csvKey := testKeyFromSeed(t, 0x10)
	csvPubKey := csvKey.PubKey()
	csvClosure := (&assets.CSVClosure{
		Key:   csvPubKey,
		Delay: csvDelay,
	}).ScriptClosure()

	// Build transfer to NUMS internal key with CSV closure.
	builder := assets.NewAssetTxBuilder(assetID, &chainParams)
	err = builder.AddAssetInput(inputCfg)
	require.NoError(t, err)

	// Output uses NUMS internal key (unspendable key path) with CSV
	// closure.
	err = builder.AddAssetOutput(assets.OutputConfig{
		Amount: testAssetAmount,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(tapasset.NUMSPubKey),
		},
		Closures: []assets.ScriptClosure{csvClosure},
		Script:   assets.OpTrueScript(),
	})
	require.NoError(t, err)

	_, err = builder.Compile(ctx)
	require.NoError(t, err)

	err = builder.Commit(
		ctx, aliceClient.AssetWalletClient,
		assets.CommitOptions{FeeRate: chainfee.SatPerVByte(10)},
	)
	require.NoError(t, err)

	err = builder.SignVirtualPackets(ctx, aliceClient.AssetWalletClient)
	require.NoError(t, err)

	err = builder.PopulateTapdKeyInfo(
		ctx, aliceClient.TapdClient,
		chaincfg.RegressionNetParams.HDCoinType,
	)
	require.NoError(t, err)

	_, err = builder.FinalizeAnchor(ctx, alice.LND.WalletKit)
	require.NoError(t, err)

	resp, err := builder.Publish(
		ctx, aliceClient.TapdClient, "csv-setup",
		assets.PublishOptions{},
	)
	require.NoError(t, err)
	t.Logf("Published CSV-locked transfer: %x", resp.Transfer.AnchorTxHash)

	// Get proof for the CSV-locked output.
	td, err := builder.GetTransferData()
	require.NoError(t, err)

	anchorPsbt := builder.AnchorPsbt()
	anchorTx, err := psbt.Extract(anchorPsbt)
	require.NoError(t, err)

	// Mine the transaction.
	minedBlocks := h.GenerateAndWait(1)
	minedBlock := minedBlocks[0]

	// Get full block data.
	rpcClient, err := h.BitcoinRPCClient()
	require.NoError(t, err)
	t.Cleanup(func() { rpcClient.Shutdown() })

	blockHash, err := chainhash.NewHashFromStr(minedBlock.Header.Hash)
	require.NoError(t, err)

	rawBlock, err := rpcClient.GetBlock(blockHash)
	require.NoError(t, err)

	anchorTxHash := anchorTx.TxHash()
	txIndex := -1
	for i, tx := range rawBlock.Transactions {
		txHash := tx.TxHash()
		if txHash.IsEqual(&anchorTxHash) {
			txIndex = i
			break
		}
	}
	require.NotEqual(t, -1, txIndex)

	csvProof, err := assets.BuildProofFromTransferData(
		td, [][]byte{proofResp.RawProofFile}, 0,
		&assets.ProofParams{
			Block:       rawBlock,
			BlockHeight: uint32(minedBlock.Header.Height),
			TxIndex:     txIndex,
		},
	)
	require.NoError(t, err)
	t.Logf("Generated CSV-locked proof (len=%d)", len(csvProof))

	// Now sweep using CSV script path after timelock expires.
	// Mine blocks to satisfy CSV.
	h.GenerateAndWait(int(csvDelay))

	// Create sweep builder.
	sweepBuilder := assets.NewAssetTxBuilder(assetID, &chainParams)

	sweepInputCfg := assets.InputConfig{
		ProofFile: csvProof,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(tapasset.NUMSPubKey),
		},
		Closures: []assets.ScriptClosure{csvClosure},
		Sequence: csvDelay,
	}
	err = sweepBuilder.AddAssetInput(sweepInputCfg)
	require.NoError(t, err)

	// Sweep to simple OP_TRUE.
	err = sweepBuilder.AddAssetOutput(assets.OutputConfig{
		Amount: testAssetAmount,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(tapasset.NUMSPubKey),
		},
		Script: assets.OpTrueScript(),
	})
	require.NoError(t, err)

	sweepBuilder.AddEphemeralAnchor()

	_, err = sweepBuilder.Compile(ctx)
	require.NoError(t, err)

	err = sweepBuilder.Commit(
		ctx, aliceClient.AssetWalletClient,
		assets.CommitOptions{SkipWalletFunding: true},
	)
	require.NoError(t, err)

	// Prepare script spend for CSV closure.
	details, err := sweepBuilder.PrepareScriptSpend(0, "csv")
	require.NoError(t, err)
	require.NotNil(t, details)
	t.Logf("CSV script spend sighash: %x", details.SigHash)

	// Sign with the CSV key.
	sig, err := schnorr.Sign(csvKey, details.SigHash[:])
	require.NoError(t, err)

	csvKeyHex := hex.EncodeToString(schnorr.SerializePubKey(csvPubKey))
	signatures := map[string][]byte{
		csvKeyHex: sig.Serialize(),
	}

	err = sweepBuilder.ApplyScriptSpend(details, signatures)
	require.NoError(t, err)

	sweepPsbt := sweepBuilder.AnchorPsbt()
	sweepTx, err := psbt.Extract(sweepPsbt)
	require.NoError(t, err)
	t.Logf("CSV sweep txid: %s", sweepTx.TxHash())

	// Build CPFP child and submit package.
	btcClient, err := h.BitcoindClient()
	require.NoError(t, err)

	changeAddr, err := rpcClient.GetNewAddress("")
	require.NoError(t, err)

	walletShim := newWalletKitFundingShim(alice.LND.WalletKit)
	_, childTx, err := sweepBuilder.BuildAnchorChild(ctx, walletShim,
		assets.AnchorChildOptions{
			ChangeAddress: changeAddr,
			FeeRate:       chainfee.SatPerKWeight(10_000),
		},
	)
	require.NoError(t, err)

	_, err = btcClient.SubmitPackage(
		[]*wire.MsgTx{sweepTx}, childTx, nil,
	)
	require.NoError(t, err)
	t.Logf("CSV sweep package submitted successfully")

	h.GenerateAndWait(1)
	t.Logf("CSV script closure test completed")
}

// TestAssetTxBuilderScriptClosureCollab tests spending via collaborative 2-of-2
// multisig script path. Uses CollabMultisigClosure with owner and cosigner.
func TestAssetTxBuilderScriptClosureCollab(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	opts := harness.DefaultOptions()
	opts.StartTapd = true
	opts.GroupName = "asset-script-collab"

	h := harness.NewHarness(t, &opts)
	h.Start()
	t.Cleanup(h.Stop)

	chainParams := address.ParamsForChain(
		chaincfg.RegressionNetParams.Name,
	)

	alice := h.NewTapdHarness("alice")
	t.Cleanup(alice.Stop)
	aliceClient := alice.NewTapClientHarness()
	t.Cleanup(aliceClient.Close)

	harness.FundNode(h, alice.LND)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// Mint asset.
	asset := aliceClient.MintAsset(
		"COLLAB", testAssetAmount, taprpc.AssetType_NORMAL,
	)
	require.NotNil(t, asset)

	var assetID tapasset.ID
	copy(assetID[:], asset.AssetGenesis.AssetId)

	// Send to tapd address.
	receiveAddr, err := aliceClient.NewAddr(ctx, &taprpc.NewAddrRequest{
		AssetId:        assetID[:],
		Amt:            testAssetAmount,
		AssetVersion:   taprpc.AssetVersion_ASSET_VERSION_V1,
		AddressVersion: taprpc.AddrVersion_ADDR_VERSION_V1,
	})
	require.NoError(t, err)
	aliceClient.SendAsset(receiveAddr.Encoded)
	h.GenerateAndWait(1)
	aliceClient.WaitForAssetBalance(assetID[:], testAssetAmount)

	proofResp, err := aliceClient.ExportProof(
		ctx, &taprpc.ExportProofRequest{
			AssetId:   assetID[:],
			ScriptKey: receiveAddr.ScriptKey,
		},
	)
	require.NoError(t, err)

	inputCfg, err := assets.InputConfigFromProof(proofResp.RawProofFile)
	require.NoError(t, err)

	// Create collaborative multisig closure.
	ownerKey := testKeyFromSeed(t, 0x20)
	cosignerKey := testKeyFromSeed(t, 0x21)
	ownerPubKey := ownerKey.PubKey()
	cosignerPubKey := cosignerKey.PubKey()

	collabClosure := (&assets.CollabMultisigClosure{
		OwnerKey:    ownerPubKey,
		CosignerKey: cosignerPubKey,
	}).ScriptClosure()

	// Build transfer to NUMS with collab closure.
	builder := assets.NewAssetTxBuilder(assetID, &chainParams)
	err = builder.AddAssetInput(inputCfg)
	require.NoError(t, err)

	err = builder.AddAssetOutput(assets.OutputConfig{
		Amount: testAssetAmount,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(tapasset.NUMSPubKey),
		},
		Closures: []assets.ScriptClosure{collabClosure},
		Script:   assets.OpTrueScript(),
	})
	require.NoError(t, err)

	_, err = builder.Compile(ctx)
	require.NoError(t, err)

	err = builder.Commit(
		ctx, aliceClient.AssetWalletClient,
		assets.CommitOptions{FeeRate: chainfee.SatPerVByte(10)},
	)
	require.NoError(t, err)

	err = builder.SignVirtualPackets(ctx, aliceClient.AssetWalletClient)
	require.NoError(t, err)

	err = builder.PopulateTapdKeyInfo(
		ctx, aliceClient.TapdClient,
		chaincfg.RegressionNetParams.HDCoinType,
	)
	require.NoError(t, err)

	_, err = builder.FinalizeAnchor(ctx, alice.LND.WalletKit)
	require.NoError(t, err)

	resp, err := builder.Publish(
		ctx, aliceClient.TapdClient, "collab-setup",
		assets.PublishOptions{},
	)
	require.NoError(t, err)
	t.Logf("Published collab-locked transfer: %x",
		resp.Transfer.AnchorTxHash)

	// Get proof for the collab-locked output.
	td, err := builder.GetTransferData()
	require.NoError(t, err)

	anchorPsbt := builder.AnchorPsbt()
	anchorTx, err := psbt.Extract(anchorPsbt)
	require.NoError(t, err)

	// Mine the transaction.
	minedBlocks := h.GenerateAndWait(1)
	minedBlock := minedBlocks[0]

	// Get full block data.
	rpcClient, err := h.BitcoinRPCClient()
	require.NoError(t, err)
	t.Cleanup(func() { rpcClient.Shutdown() })

	blockHash, err := chainhash.NewHashFromStr(minedBlock.Header.Hash)
	require.NoError(t, err)

	rawBlock, err := rpcClient.GetBlock(blockHash)
	require.NoError(t, err)

	anchorTxHash := anchorTx.TxHash()
	txIndex := -1
	for i, tx := range rawBlock.Transactions {
		txHash := tx.TxHash()
		if txHash.IsEqual(&anchorTxHash) {
			txIndex = i
			break
		}
	}
	require.NotEqual(t, -1, txIndex)

	collabProof, err := assets.BuildProofFromTransferData(
		td, [][]byte{proofResp.RawProofFile}, 0,
		&assets.ProofParams{
			Block:       rawBlock,
			BlockHeight: uint32(minedBlock.Header.Height),
			TxIndex:     txIndex,
		},
	)
	require.NoError(t, err)
	t.Logf("Generated collab-locked proof (len=%d)", len(collabProof))

	// Sweep using collaborative multisig script path.
	sweepBuilder := assets.NewAssetTxBuilder(assetID, &chainParams)

	sweepInputCfg := assets.InputConfig{
		ProofFile: collabProof,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(tapasset.NUMSPubKey),
		},
		Closures: []assets.ScriptClosure{collabClosure},
	}
	err = sweepBuilder.AddAssetInput(sweepInputCfg)
	require.NoError(t, err)

	err = sweepBuilder.AddAssetOutput(assets.OutputConfig{
		Amount: testAssetAmount,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(tapasset.NUMSPubKey),
		},
		Script: assets.OpTrueScript(),
	})
	require.NoError(t, err)

	sweepBuilder.AddEphemeralAnchor()

	_, err = sweepBuilder.Compile(ctx)
	require.NoError(t, err)

	err = sweepBuilder.Commit(
		ctx, aliceClient.AssetWalletClient,
		assets.CommitOptions{SkipWalletFunding: true},
	)
	require.NoError(t, err)

	// Prepare script spend for collab closure.
	details, err := sweepBuilder.PrepareScriptSpend(0, "collab_multisig")
	require.NoError(t, err)
	require.NotNil(t, details)
	t.Logf("Collab script spend sighash: %x", details.SigHash)

	// Sign with both keys.
	ownerSig, err := schnorr.Sign(ownerKey, details.SigHash[:])
	require.NoError(t, err)

	cosignerSig, err := schnorr.Sign(cosignerKey, details.SigHash[:])
	require.NoError(t, err)

	ownerKeyHex := hex.EncodeToString(schnorr.SerializePubKey(ownerPubKey))
	cosignerKeyHex := hex.EncodeToString(
		schnorr.SerializePubKey(cosignerPubKey),
	)
	signatures := map[string][]byte{
		ownerKeyHex:    ownerSig.Serialize(),
		cosignerKeyHex: cosignerSig.Serialize(),
	}

	err = sweepBuilder.ApplyScriptSpend(details, signatures)
	require.NoError(t, err)

	sweepPsbt := sweepBuilder.AnchorPsbt()
	sweepTx, err := psbt.Extract(sweepPsbt)
	require.NoError(t, err)
	t.Logf("Collab sweep txid: %s", sweepTx.TxHash())

	// Build CPFP child and submit package.
	btcClient, err := h.BitcoindClient()
	require.NoError(t, err)

	changeAddr, err := rpcClient.GetNewAddress("")
	require.NoError(t, err)

	walletShim := newWalletKitFundingShim(alice.LND.WalletKit)
	_, childTx, err := sweepBuilder.BuildAnchorChild(ctx, walletShim,
		assets.AnchorChildOptions{
			ChangeAddress: changeAddr,
			FeeRate:       chainfee.SatPerKWeight(10_000),
		},
	)
	require.NoError(t, err)

	_, err = btcClient.SubmitPackage(
		[]*wire.MsgTx{sweepTx}, childTx, nil,
	)
	require.NoError(t, err)
	t.Logf("Collab sweep package submitted successfully")

	h.GenerateAndWait(1)
	t.Logf("Collab multisig script closure test completed")
}

// TestAssetTxBuilderMultipleClosures tests an asset locked with multiple script
// closures (CSV timeout + collaborative multisig). Tests spending via each
// path to verify the tapscript tree is constructed correctly.
func TestAssetTxBuilderMultipleClosures(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	opts := harness.DefaultOptions()
	opts.StartTapd = true
	opts.GroupName = "asset-multi-closure"

	h := harness.NewHarness(t, &opts)
	h.Start()
	t.Cleanup(h.Stop)

	chainParams := address.ParamsForChain(
		chaincfg.RegressionNetParams.Name,
	)

	alice := h.NewTapdHarness("alice")
	t.Cleanup(alice.Stop)
	aliceClient := alice.NewTapClientHarness()
	t.Cleanup(aliceClient.Close)

	harness.FundNode(h, alice.LND)

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	// Mint asset.
	asset := aliceClient.MintAsset(
		"MULTI", testAssetAmount*2, taprpc.AssetType_NORMAL,
	)
	require.NotNil(t, asset)

	var assetID tapasset.ID
	copy(assetID[:], asset.AssetGenesis.AssetId)

	// Create keys for closures.
	csvKey := testKeyFromSeed(t, 0x30)
	ownerKey := testKeyFromSeed(t, 0x31)
	cosignerKey := testKeyFromSeed(t, 0x32)

	csvDelay := uint32(2)

	// Create both closures.
	csvClosure := (&assets.CSVClosure{
		Key:   csvKey.PubKey(),
		Delay: csvDelay,
	}).ScriptClosure()

	collabClosure := (&assets.CollabMultisigClosure{
		OwnerKey:    ownerKey.PubKey(),
		CosignerKey: cosignerKey.PubKey(),
	}).ScriptClosure()

	closures := []assets.ScriptClosure{collabClosure, csvClosure}

	// Send to tapd address.
	receiveAddr, err := aliceClient.NewAddr(ctx, &taprpc.NewAddrRequest{
		AssetId:        assetID[:],
		Amt:            testAssetAmount * 2,
		AssetVersion:   taprpc.AssetVersion_ASSET_VERSION_V1,
		AddressVersion: taprpc.AddrVersion_ADDR_VERSION_V1,
	})
	require.NoError(t, err)
	aliceClient.SendAsset(receiveAddr.Encoded)
	h.GenerateAndWait(1)
	aliceClient.WaitForAssetBalance(assetID[:], testAssetAmount*2)

	proofResp, err := aliceClient.ExportProof(
		ctx, &taprpc.ExportProofRequest{
			AssetId:   assetID[:],
			ScriptKey: receiveAddr.ScriptKey,
		},
	)
	require.NoError(t, err)

	inputCfg, err := assets.InputConfigFromProof(proofResp.RawProofFile)
	require.NoError(t, err)

	// Build transfer to NUMS with both closures.
	builder := assets.NewAssetTxBuilder(assetID, &chainParams)
	err = builder.AddAssetInput(inputCfg)
	require.NoError(t, err)

	err = builder.AddAssetOutput(assets.OutputConfig{
		Amount: testAssetAmount * 2,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(tapasset.NUMSPubKey),
		},
		Closures: closures,
		Script:   assets.OpTrueScript(),
	})
	require.NoError(t, err)

	_, err = builder.Compile(ctx)
	require.NoError(t, err)

	err = builder.Commit(
		ctx, aliceClient.AssetWalletClient,
		assets.CommitOptions{FeeRate: chainfee.SatPerVByte(10)},
	)
	require.NoError(t, err)

	err = builder.SignVirtualPackets(ctx, aliceClient.AssetWalletClient)
	require.NoError(t, err)

	err = builder.PopulateTapdKeyInfo(
		ctx, aliceClient.TapdClient,
		chaincfg.RegressionNetParams.HDCoinType,
	)
	require.NoError(t, err)

	_, err = builder.FinalizeAnchor(ctx, alice.LND.WalletKit)
	require.NoError(t, err)

	resp, err := builder.Publish(
		ctx, aliceClient.TapdClient, "multi-setup",
		assets.PublishOptions{},
	)
	require.NoError(t, err)
	t.Logf("Published multi-closure transfer: %x",
		resp.Transfer.AnchorTxHash)

	// Get proof.
	td, err := builder.GetTransferData()
	require.NoError(t, err)

	anchorPsbt := builder.AnchorPsbt()
	anchorTx, err := psbt.Extract(anchorPsbt)
	require.NoError(t, err)

	// Mine the transaction.
	minedBlocks := h.GenerateAndWait(1)
	minedBlock := minedBlocks[0]

	// Get full block data.
	rpcClient, err := h.BitcoinRPCClient()
	require.NoError(t, err)
	t.Cleanup(func() { rpcClient.Shutdown() })

	blockHash, err := chainhash.NewHashFromStr(minedBlock.Header.Hash)
	require.NoError(t, err)

	rawBlock, err := rpcClient.GetBlock(blockHash)
	require.NoError(t, err)

	anchorTxHash := anchorTx.TxHash()
	txIndex := -1
	for i, tx := range rawBlock.Transactions {
		txHash := tx.TxHash()
		if txHash.IsEqual(&anchorTxHash) {
			txIndex = i
			break
		}
	}
	require.NotEqual(t, -1, txIndex)

	multiProof, err := assets.BuildProofFromTransferData(
		td, [][]byte{proofResp.RawProofFile}, 0,
		&assets.ProofParams{
			Block:       rawBlock,
			BlockHeight: uint32(minedBlock.Header.Height),
			TxIndex:     txIndex,
		},
	)
	require.NoError(t, err)
	t.Logf("Generated multi-closure proof (len=%d)", len(multiProof))

	numsKey := schnorr.SerializePubKey(tapasset.NUMSPubKey)

	// Test 1: Spend via collaborative path (immediate).
	t.Run("collab_path", func(t *testing.T) {
		sweepBuilder := assets.NewAssetTxBuilder(assetID, &chainParams)

		sweepInputCfg := assets.InputConfig{
			ProofFile: multiProof,
			AnchorKey: assets.AnchorKeySpec{
				Mode: assets.AnchorKeyModeStatic,
				Key:  numsKey,
			},
			Closures: closures,
		}
		err = sweepBuilder.AddAssetInput(sweepInputCfg)
		require.NoError(t, err)

		err = sweepBuilder.AddAssetOutput(assets.OutputConfig{
			Amount: testAssetAmount * 2,
			AnchorKey: assets.AnchorKeySpec{
				Mode: assets.AnchorKeyModeStatic,
				Key:  numsKey,
			},
			Script: assets.OpTrueScript(),
		})
		require.NoError(t, err)

		sweepBuilder.AddEphemeralAnchor()

		_, err = sweepBuilder.Compile(ctx)
		require.NoError(t, err)

		err = sweepBuilder.Commit(
			ctx, aliceClient.AssetWalletClient,
			assets.CommitOptions{SkipWalletFunding: true},
		)
		require.NoError(t, err)

		// Use collab_multisig closure.
		details, err := sweepBuilder.PrepareScriptSpend(
			0, "collab_multisig",
		)
		require.NoError(t, err)
		require.NotNil(t, details)
		require.Equal(t, "collab_multisig", details.ClosureID)

		ownerSig, err := schnorr.Sign(ownerKey, details.SigHash[:])
		require.NoError(t, err)

		cosignerSig, err := schnorr.Sign(
			cosignerKey, details.SigHash[:],
		)
		require.NoError(t, err)

		ownerKeyHex := hex.EncodeToString(
			schnorr.SerializePubKey(ownerKey.PubKey()),
		)
		cosignerKeyHex := hex.EncodeToString(
			schnorr.SerializePubKey(cosignerKey.PubKey()),
		)
		signatures := map[string][]byte{
			ownerKeyHex:    ownerSig.Serialize(),
			cosignerKeyHex: cosignerSig.Serialize(),
		}

		err = sweepBuilder.ApplyScriptSpend(details, signatures)
		require.NoError(t, err)

		// Verify the witness was applied.
		sweepPsbt := sweepBuilder.AnchorPsbt()
		require.NotEmpty(t, sweepPsbt.Inputs[0].FinalScriptWitness)
		t.Logf("Collab path witness applied successfully")
	})

	// Test 2: Verify CSV path would work (don't actually spend).
	t.Run("csv_path_verification", func(t *testing.T) {
		// Mine blocks to satisfy CSV first.
		h.GenerateAndWait(int(csvDelay))

		sweepBuilder := assets.NewAssetTxBuilder(assetID, &chainParams)

		sweepInputCfg := assets.InputConfig{
			ProofFile: multiProof,
			AnchorKey: assets.AnchorKeySpec{
				Mode: assets.AnchorKeyModeStatic,
				Key:  numsKey,
			},
			Closures: closures,
			Sequence: csvDelay,
		}
		err = sweepBuilder.AddAssetInput(sweepInputCfg)
		require.NoError(t, err)

		err = sweepBuilder.AddAssetOutput(assets.OutputConfig{
			Amount: testAssetAmount * 2,
			AnchorKey: assets.AnchorKeySpec{
				Mode: assets.AnchorKeyModeStatic,
				Key:  numsKey,
			},
			Script: assets.OpTrueScript(),
		})
		require.NoError(t, err)

		sweepBuilder.AddEphemeralAnchor()

		_, err = sweepBuilder.Compile(ctx)
		require.NoError(t, err)

		err = sweepBuilder.Commit(
			ctx, aliceClient.AssetWalletClient,
			assets.CommitOptions{SkipWalletFunding: true},
		)
		require.NoError(t, err)

		// Use csv closure.
		details, err := sweepBuilder.PrepareScriptSpend(0, "csv")
		require.NoError(t, err)
		require.NotNil(t, details)
		require.Equal(t, "csv", details.ClosureID)

		csvSig, err := schnorr.Sign(csvKey, details.SigHash[:])
		require.NoError(t, err)

		csvKeyHex := hex.EncodeToString(
			schnorr.SerializePubKey(csvKey.PubKey()),
		)
		signatures := map[string][]byte{
			csvKeyHex: csvSig.Serialize(),
		}

		err = sweepBuilder.ApplyScriptSpend(details, signatures)
		require.NoError(t, err)

		sweepPsbt := sweepBuilder.AnchorPsbt()
		require.NotEmpty(t, sweepPsbt.Inputs[0].FinalScriptWitness)
		t.Logf("CSV path witness applied successfully")
	})

	t.Logf("Multiple closures test completed")
}

// ============================================================================
// BTC I/O Tests
// ============================================================================

// TestAssetTxBuilderBtcOutput tests adding a BTC-only output alongside an asset
// transfer. This is useful for scenarios like ARK where connector outputs need
// to be included in the anchor transaction.
func TestAssetTxBuilderBtcOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	opts := harness.DefaultOptions()
	opts.StartTapd = true
	opts.GroupName = "asset-btc-output"

	h := harness.NewHarness(t, &opts)
	h.Start()
	t.Cleanup(h.Stop)

	chainParams := address.ParamsForChain(
		chaincfg.RegressionNetParams.Name,
	)

	alice := h.NewTapdHarness("alice")
	t.Cleanup(alice.Stop)
	aliceClient := alice.NewTapClientHarness()
	t.Cleanup(aliceClient.Close)

	harness.FundNode(h, alice.LND)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// Mint asset.
	asset := aliceClient.MintAsset(
		"BTCOUT", testAssetAmount, taprpc.AssetType_NORMAL,
	)
	require.NotNil(t, asset)

	var assetID tapasset.ID
	copy(assetID[:], asset.AssetGenesis.AssetId)

	// Send to tapd address.
	receiveAddr, err := aliceClient.NewAddr(ctx, &taprpc.NewAddrRequest{
		AssetId:        assetID[:],
		Amt:            testAssetAmount,
		AssetVersion:   taprpc.AssetVersion_ASSET_VERSION_V1,
		AddressVersion: taprpc.AddrVersion_ADDR_VERSION_V1,
	})
	require.NoError(t, err)
	aliceClient.SendAsset(receiveAddr.Encoded)
	h.GenerateAndWait(1)
	aliceClient.WaitForAssetBalance(assetID[:], testAssetAmount)

	proofResp, err := aliceClient.ExportProof(
		ctx, &taprpc.ExportProofRequest{
			AssetId:   assetID[:],
			ScriptKey: receiveAddr.ScriptKey,
		},
	)
	require.NoError(t, err)

	inputCfg, err := assets.InputConfigFromProof(proofResp.RawProofFile)
	require.NoError(t, err)

	// Build transfer with additional BTC output.
	builder := assets.NewAssetTxBuilder(assetID, &chainParams)
	err = builder.AddAssetInput(inputCfg)
	require.NoError(t, err)

	// Asset output.
	err = builder.AddAssetOutput(assets.OutputConfig{
		Amount: testAssetAmount,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(tapasset.NUMSPubKey),
		},
		Script: assets.OpTrueScript(),
	})
	require.NoError(t, err)

	// Generate a BTC address for the extra output.
	rpcClient, err := h.BitcoinRPCClient()
	require.NoError(t, err)
	t.Cleanup(func() { rpcClient.Shutdown() })

	btcAddr, err := rpcClient.GetNewAddress("")
	require.NoError(t, err)

	btcPkScript, err := txscript.PayToAddrScript(btcAddr)
	require.NoError(t, err)

	// Add BTC-only output (e.g., 10000 sats for a connector).
	btcOutputValue := int64(10000)
	err = builder.AddBtcOutput(assets.BtcOutputSpec{
		Description: "connector",
		ValueSat:    btcOutputValue,
		PkScript:    btcPkScript,
	})
	require.NoError(t, err)

	_, err = builder.Compile(ctx)
	require.NoError(t, err)

	err = builder.Commit(
		ctx, aliceClient.AssetWalletClient,
		assets.CommitOptions{FeeRate: chainfee.SatPerVByte(10)},
	)
	require.NoError(t, err)

	err = builder.SignVirtualPackets(ctx, aliceClient.AssetWalletClient)
	require.NoError(t, err)

	// Verify the BTC output is in the PSBT.
	anchorPsbt := builder.AnchorPsbt()
	require.NotNil(t, anchorPsbt)

	// Find the BTC output.
	foundBtcOutput := false
	for _, txOut := range anchorPsbt.UnsignedTx.TxOut {
		if txOut.Value == btcOutputValue &&
			bytes.Equal(txOut.PkScript, btcPkScript) {

			foundBtcOutput = true
			break
		}
	}
	require.True(t, foundBtcOutput, "BTC output not found in anchor PSBT")
	t.Logf("BTC output (value=%d) found in anchor PSBT", btcOutputValue)

	err = builder.PopulateTapdKeyInfo(
		ctx, aliceClient.TapdClient,
		chaincfg.RegressionNetParams.HDCoinType,
	)
	require.NoError(t, err)

	_, err = builder.FinalizeAnchor(ctx, alice.LND.WalletKit)
	require.NoError(t, err)

	resp, err := builder.Publish(
		ctx, aliceClient.TapdClient, "btc-output-test",
		assets.PublishOptions{},
	)
	require.NoError(t, err)
	t.Logf("Published transfer with BTC output: %x",
		resp.Transfer.AnchorTxHash)

	h.GenerateAndWait(1)
	t.Logf("BTC output test completed")
}

// TestAssetTxBuilderBtcInput tests adding a BTC-only input alongside an asset
// transfer. This is useful for scenarios where external BTC UTXOs need to be
// consumed in the same transaction (e.g., ARK forfeit connectors).
func TestAssetTxBuilderBtcInput(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	opts := harness.DefaultOptions()
	opts.StartTapd = true
	opts.GroupName = "asset-btc-input"

	h := harness.NewHarness(t, &opts)
	h.Start()
	t.Cleanup(h.Stop)

	chainParams := address.ParamsForChain(
		chaincfg.RegressionNetParams.Name,
	)

	alice := h.NewTapdHarness("alice")
	t.Cleanup(alice.Stop)
	aliceClient := alice.NewTapClientHarness()
	t.Cleanup(aliceClient.Close)

	harness.FundNode(h, alice.LND)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// Mint asset.
	asset := aliceClient.MintAsset(
		"BTCIN", testAssetAmount, taprpc.AssetType_NORMAL,
	)
	require.NotNil(t, asset)

	var assetID tapasset.ID
	copy(assetID[:], asset.AssetGenesis.AssetId)

	// Send to tapd address.
	receiveAddr, err := aliceClient.NewAddr(ctx, &taprpc.NewAddrRequest{
		AssetId: assetID[:],
		Amt:     testAssetAmount,
	})
	require.NoError(t, err)
	aliceClient.SendAsset(receiveAddr.Encoded)
	h.GenerateAndWait(1)
	aliceClient.WaitForAssetBalance(assetID[:], testAssetAmount)

	proofResp, err := aliceClient.ExportProof(
		ctx, &taprpc.ExportProofRequest{
			AssetId:   assetID[:],
			ScriptKey: receiveAddr.ScriptKey,
		},
	)
	require.NoError(t, err)

	inputCfg, err := assets.InputConfigFromProof(proofResp.RawProofFile)
	require.NoError(t, err)

	// Create a BTC UTXO to use as input.
	rpcClient, err := h.BitcoinRPCClient()
	require.NoError(t, err)
	t.Cleanup(func() { rpcClient.Shutdown() })

	// Send some BTC to create a UTXO we can use.
	btcAddr, err := rpcClient.GetNewAddress("")
	require.NoError(t, err)

	btcAmount := btcutil.Amount(50000)
	txHash, err := rpcClient.SendToAddress(btcAddr, btcAmount)
	require.NoError(t, err)
	h.GenerateAndWait(1)

	// Get the UTXO details.
	tx, err := rpcClient.GetRawTransaction(txHash)
	require.NoError(t, err)

	var btcOutIndex uint32
	var btcPkScript []byte
	for i, txOut := range tx.MsgTx().TxOut {
		if txOut.Value == int64(btcAmount) {
			btcOutIndex = uint32(i)
			btcPkScript = txOut.PkScript
			break
		}
	}
	require.NotNil(t, btcPkScript, "Could not find BTC output")

	// Build transfer with BTC input.
	builder := assets.NewAssetTxBuilder(assetID, &chainParams)
	err = builder.AddAssetInput(inputCfg)
	require.NoError(t, err)

	// Asset output.
	err = builder.AddAssetOutput(assets.OutputConfig{
		Amount: testAssetAmount,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(tapasset.NUMSPubKey),
		},
		Script: assets.OpTrueScript(),
	})
	require.NoError(t, err)

	// Add BTC input.
	btcInputSpec := assets.BtcInputSpec{
		Outpoint: wire.OutPoint{
			Hash:  *txHash,
			Index: btcOutIndex,
		},
		WitnessUtxo: &wire.TxOut{
			Value:    int64(btcAmount),
			PkScript: btcPkScript,
		},
	}
	err = builder.AddBtcInput(btcInputSpec)
	require.NoError(t, err)

	_, err = builder.Compile(ctx)
	require.NoError(t, err)

	err = builder.Commit(
		ctx, aliceClient.AssetWalletClient,
		assets.CommitOptions{FeeRate: chainfee.SatPerVByte(10)},
	)
	require.NoError(t, err)

	// Verify the BTC input is in the PSBT.
	anchorPsbt := builder.AnchorPsbt()
	require.NotNil(t, anchorPsbt)

	foundBtcInput := false
	for _, txIn := range anchorPsbt.UnsignedTx.TxIn {
		if txIn.PreviousOutPoint.Hash == *txHash &&
			txIn.PreviousOutPoint.Index == btcOutIndex {

			foundBtcInput = true
			break
		}
	}
	require.True(t, foundBtcInput, "BTC input not found in anchor PSBT")
	t.Logf("BTC input (outpoint=%s:%d) found in anchor PSBT",
		txHash, btcOutIndex)

	err = builder.PopulateTapdKeyInfo(
		ctx, aliceClient.TapdClient,
		chaincfg.RegressionNetParams.HDCoinType,
	)
	require.NoError(t, err)

	// Note: FinalizeAnchor won't be able to sign the BTC input since it's
	// from bitcoind, not LND. In a real scenario, you'd need to sign it
	// separately. For this test, we verify the input was added correctly.
	t.Logf("BTC input test completed - input added to PSBT successfully")
}

// TestAssetTxBuilderBtcInputOutput tests adding both BTC input and output in
// the same transaction. This simulates an ARK-style forfeit where a connector
// UTXO is consumed and a new connector is created.
func TestAssetTxBuilderBtcInputOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	opts := harness.DefaultOptions()
	opts.StartTapd = true
	opts.GroupName = "asset-btc-io"

	h := harness.NewHarness(t, &opts)
	h.Start()
	t.Cleanup(h.Stop)

	chainParams := address.ParamsForChain(
		chaincfg.RegressionNetParams.Name,
	)

	alice := h.NewTapdHarness("alice")
	t.Cleanup(alice.Stop)
	aliceClient := alice.NewTapClientHarness()
	t.Cleanup(aliceClient.Close)

	harness.FundNode(h, alice.LND)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// Mint asset.
	asset := aliceClient.MintAsset(
		"BTCIO", testAssetAmount, taprpc.AssetType_NORMAL,
	)
	require.NotNil(t, asset)

	var assetID tapasset.ID
	copy(assetID[:], asset.AssetGenesis.AssetId)

	// Send to tapd address.
	receiveAddr, err := aliceClient.NewAddr(ctx, &taprpc.NewAddrRequest{
		AssetId: assetID[:],
		Amt:     testAssetAmount,
	})
	require.NoError(t, err)
	aliceClient.SendAsset(receiveAddr.Encoded)
	h.GenerateAndWait(1)
	aliceClient.WaitForAssetBalance(assetID[:], testAssetAmount)

	proofResp, err := aliceClient.ExportProof(
		ctx, &taprpc.ExportProofRequest{
			AssetId:   assetID[:],
			ScriptKey: receiveAddr.ScriptKey,
		},
	)
	require.NoError(t, err)

	inputCfg, err := assets.InputConfigFromProof(proofResp.RawProofFile)
	require.NoError(t, err)

	// Create a BTC UTXO.
	rpcClient, err := h.BitcoinRPCClient()
	require.NoError(t, err)
	t.Cleanup(func() { rpcClient.Shutdown() })

	btcAddr, err := rpcClient.GetNewAddress("")
	require.NoError(t, err)

	btcAmount := btcutil.Amount(50000)
	txHash, err := rpcClient.SendToAddress(btcAddr, btcAmount)
	require.NoError(t, err)
	h.GenerateAndWait(1)

	tx, err := rpcClient.GetRawTransaction(txHash)
	require.NoError(t, err)

	var btcOutIndex uint32
	var btcPkScript []byte
	for i, txOut := range tx.MsgTx().TxOut {
		if txOut.Value == int64(btcAmount) {
			btcOutIndex = uint32(i)
			btcPkScript = txOut.PkScript
			break
		}
	}
	require.NotNil(t, btcPkScript)

	// Build transfer with both BTC input and output.
	builder := assets.NewAssetTxBuilder(assetID, &chainParams)
	err = builder.AddAssetInput(inputCfg)
	require.NoError(t, err)

	// Asset output.
	err = builder.AddAssetOutput(assets.OutputConfig{
		Amount: testAssetAmount,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(tapasset.NUMSPubKey),
		},
		Script: assets.OpTrueScript(),
	})
	require.NoError(t, err)

	// Add BTC input (connector being consumed).
	err = builder.AddBtcInput(assets.BtcInputSpec{
		Outpoint: wire.OutPoint{
			Hash:  *txHash,
			Index: btcOutIndex,
		},
		WitnessUtxo: &wire.TxOut{
			Value:    int64(btcAmount),
			PkScript: btcPkScript,
		},
	})
	require.NoError(t, err)

	// Add BTC output (new connector).
	newConnectorAddr, err := rpcClient.GetNewAddress("")
	require.NoError(t, err)

	newConnectorScript, err := txscript.PayToAddrScript(newConnectorAddr)
	require.NoError(t, err)

	newConnectorValue := int64(40000) // Less than input to allow for fees.
	err = builder.AddBtcOutput(assets.BtcOutputSpec{
		Description: "new_connector",
		ValueSat:    newConnectorValue,
		PkScript:    newConnectorScript,
	})
	require.NoError(t, err)

	_, err = builder.Compile(ctx)
	require.NoError(t, err)

	err = builder.Commit(
		ctx, aliceClient.AssetWalletClient,
		assets.CommitOptions{FeeRate: chainfee.SatPerVByte(10)},
	)
	require.NoError(t, err)

	// Verify both BTC input and output are in the PSBT.
	anchorPsbt := builder.AnchorPsbt()
	require.NotNil(t, anchorPsbt)

	// Check input.
	foundBtcInput := false
	for _, txIn := range anchorPsbt.UnsignedTx.TxIn {
		if txIn.PreviousOutPoint.Hash == *txHash &&
			txIn.PreviousOutPoint.Index == btcOutIndex {

			foundBtcInput = true
			break
		}
	}
	require.True(t, foundBtcInput, "BTC input not found")

	// Check output.
	foundBtcOutput := false
	for _, txOut := range anchorPsbt.UnsignedTx.TxOut {
		if txOut.Value == newConnectorValue &&
			bytes.Equal(txOut.PkScript, newConnectorScript) {

			foundBtcOutput = true
			break
		}
	}
	require.True(t, foundBtcOutput, "BTC output not found")

	t.Logf("BTC input+output test completed successfully")
	t.Logf("  Input: %s:%d (%d sats)", txHash, btcOutIndex, btcAmount)
	t.Logf("  Output: %d sats to new connector", newConnectorValue)
}

// ============================================================================
// Split Commitment Tests
// ============================================================================

// TestAssetTxBuilderSplitCommitment tests merge and split operations with
// multiple asset inputs/outputs. The test flow is:
//
// Phase 1 - Setup:
//   - Alice mints 100k units, sends 50k to Bob and 50k to herself via tapd
//   - Both outputs use external OP_TRUE script-path script keys (but wallet-
//     managed anchor internal keys) so later merge/split proof construction
//     does not rely on tapd-managed asset witnesses.
//
// Phase 2 - Merge (2→1):
//   - Combine Alice's 50k + Bob's 50k into a single MuSig2(Alice,Bob) output
//   - Both parties sign the anchor with their LND wallets
//   - Result: Single 100k output locked to MuSig2 key
//
// Phase 3 - Split (1→2):
//   - Split the MuSig2 output back into separate outputs
//   - 50k to Alice's new tapd address, 50k to Bob's new tapd address
//   - MuSig2 signing ceremony between Alice and Bob
//   - Result: Alice has 50k, Bob has 50k (new tapd-managed outputs)
func TestAssetTxBuilderSplitCommitment(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	opts := harness.DefaultOptions()
	opts.StartTapd = true
	opts.GroupName = "asset-split-commitment"

	h := harness.NewHarness(t, &opts)
	h.Start()
	t.Cleanup(h.Stop)

	chainParams := address.ParamsForChain(
		chaincfg.RegressionNetParams.Name,
	)

	// Create Alice and Bob tapd nodes.
	alice := h.NewTapdHarness("alice")
	t.Cleanup(alice.Stop)
	aliceClient := alice.NewTapClientHarness()
	t.Cleanup(aliceClient.Close)

	bob := h.NewTapdHarness("bob")
	t.Cleanup(bob.Stop)
	bobClient := bob.NewTapClientHarness()
	t.Cleanup(bobClient.Close)

	// Fund both nodes.
	harness.FundNode(h, alice.LND)
	harness.FundNode(h, bob.LND)

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	// ================================================================
	// Phase 1: Setup - Alice mints, sends half to Bob
	// ================================================================
	t.Log("Phase 1: Setup - Mint and distribute assets")

	const totalAmount = uint64(100_000)
	const halfAmount = uint64(50_000)

	// Alice mints asset.
	asset := aliceClient.MintAsset(
		"SPLIT", totalAmount, taprpc.AssetType_NORMAL,
	)
	require.NotNil(t, asset)

	var assetID tapasset.ID
	copy(assetID[:], asset.AssetGenesis.AssetId)
	t.Logf("Minted asset ID: %x", assetID[:])

	// Wait for Alice's tapd to have the asset registered in the universe
	// before Bob tries to sync.
	aliceClient.WaitForAssetBalance(assetID[:], totalAmount)

	// Bob needs to sync with Alice's universe to know about the asset.
	// SyncUniverse() syncs with the main harness tapd, but Alice is a
	// separate tapd, so we need to sync with Alice specifically.
	bobClient.SyncUniverseWith(alice.UniverseHost())

	// Bob creates an address to receive half the amount. We deliberately
	// override the asset script key with an external OP_TRUE script key so
	// that later merge/split sweeps can be proven without needing tapd to
	// contribute virtual transaction signature witnesses.
	//
	// IMPORTANT: The *anchor output* internal key remains wallet-managed so
	// the BTC UTXO stays spendable by the corresponding LND wallet.
	_, bobInternalKey, err := bobClient.TapdClient.DeriveNewKeys(ctx)
	require.NoError(t, err)

	bobOpTrueInternalKey := bobInternalKey.PubKey
	bobOpTrueArtifacts, err := assets.BuildOpTrueArtifacts(
		bobOpTrueInternalKey,
	)
	require.NoError(t, err)

	bobSiblingBytes, _, err := tapcommitment.MaybeEncodeTapscriptPreimage(
		bobOpTrueArtifacts.SiblingPreimage,
	)
	require.NoError(t, err)

	//nolint:ll
	scriptPathExternal := taprpc.ScriptKeyType_SCRIPT_KEY_SCRIPT_PATH_EXTERNAL
	bobTapTweak := bobOpTrueArtifacts.ScriptKey.TweakedScriptKey.Tweak
	bobAddr, err := bobClient.NewAddr(ctx, &taprpc.NewAddrRequest{
		AssetId: assetID[:],
		Amt:     halfAmount,
		ScriptKey: &taprpc.ScriptKey{
			PubKey: schnorr.SerializePubKey(
				bobOpTrueArtifacts.OutputKey,
			),
			KeyDesc: &taprpc.KeyDescriptor{
				RawKeyBytes: bobOpTrueInternalKey.
					SerializeCompressed(),
			},
			TapTweak: bobTapTweak,
			Type:     scriptPathExternal,
		},
		InternalKey:      rpcutils.MarshalKeyDescriptor(bobInternalKey),
		TapscriptSibling: bobSiblingBytes,
		AssetVersion:     taprpc.AssetVersion_ASSET_VERSION_V1,
		AddressVersion:   taprpc.AddrVersion_ADDR_VERSION_V1,
	})
	require.NoError(t, err)
	t.Logf("Bob's receive address: %s", bobAddr.Encoded)

	// Alice creates her own local receive address for the other half using
	// the same OP_TRUE pattern.
	_, aliceInternalKey, err := aliceClient.TapdClient.DeriveNewKeys(ctx)
	require.NoError(t, err)

	aliceOpTrueInternalKey := aliceInternalKey.PubKey
	aliceOpTrueArtifacts, err := assets.BuildOpTrueArtifacts(
		aliceOpTrueInternalKey,
	)
	require.NoError(t, err)

	aliceSiblingBytes, _, err := tapcommitment.MaybeEncodeTapscriptPreimage(
		aliceOpTrueArtifacts.SiblingPreimage,
	)
	require.NoError(t, err)

	aliceTapTweak := aliceOpTrueArtifacts.ScriptKey.TweakedScriptKey.Tweak
	aliceAddr, err := aliceClient.NewAddr(ctx, &taprpc.NewAddrRequest{
		AssetId: assetID[:],
		Amt:     halfAmount,
		ScriptKey: &taprpc.ScriptKey{
			PubKey: schnorr.SerializePubKey(
				aliceOpTrueArtifacts.OutputKey,
			),
			KeyDesc: &taprpc.KeyDescriptor{
				RawKeyBytes: aliceOpTrueInternalKey.
					SerializeCompressed(),
			},
			TapTweak: aliceTapTweak,
			Type:     scriptPathExternal,
		},
		InternalKey: rpcutils.MarshalKeyDescriptor(
			aliceInternalKey,
		),
		TapscriptSibling: aliceSiblingBytes,
		AssetVersion:     taprpc.AssetVersion_ASSET_VERSION_V1,
		AddressVersion:   taprpc.AddrVersion_ADDR_VERSION_V1,
	})
	require.NoError(t, err)

	// Alice sends 50k to Bob and 50k to herself.
	_, err = aliceClient.TapdClient.SendAsset(ctx, &taprpc.SendAssetRequest{
		TapAddrs: []string{
			bobAddr.Encoded,
			aliceAddr.Encoded,
		},
	})
	require.NoError(t, err)
	h.GenerateAndWait(1)

	// Export the proofs from Alice's tapd. For script-path keys, the
	// receiver might not immediately register the transfer in its wallet,
	// but the sender is still able to export the full proof chain for each
	// output.
	var (
		aliceProofResp *taprpc.ProofFile
		bobProofResp   *taprpc.ProofFile
	)
	require.Eventually(t, func() bool {
		resp, err := aliceClient.ExportProof(
			ctx, &taprpc.ExportProofRequest{
				AssetId:   assetID[:],
				ScriptKey: aliceAddr.ScriptKey,
			},
		)
		if err != nil {
			return false
		}

		aliceProofResp = resp

		return len(resp.RawProofFile) > 0
	}, 30*time.Second, 500*time.Millisecond)
	t.Logf("Got Alice's proof (len=%d)", len(aliceProofResp.RawProofFile))

	require.Eventually(t, func() bool {
		resp, err := aliceClient.ExportProof(
			ctx, &taprpc.ExportProofRequest{
				AssetId:   assetID[:],
				ScriptKey: bobAddr.ScriptKey,
			},
		)
		if err != nil {
			return false
		}

		bobProofResp = resp

		return len(resp.RawProofFile) > 0
	}, 30*time.Second, 500*time.Millisecond)
	t.Logf("Got Bob's proof (len=%d)", len(bobProofResp.RawProofFile))

	t.Log("Phase 1 complete: proofs exported for both outputs")

	// ================================================================
	// Phase 2: Merge (2→1) - Combine into MuSig2 output
	// ================================================================
	t.Log("Phase 2: Merge - Combine Alice + Bob into MuSig2 output")

	// Create input configs from proofs.
	aliceInputCfg, err := assets.InputConfigFromProof(
		aliceProofResp.RawProofFile,
	)
	require.NoError(t, err)

	bobInputCfg, err := assets.InputConfigFromProof(
		bobProofResp.RawProofFile,
	)
	require.NoError(t, err)

	// Create MuSig2 signers for the joint output. We use LocalMuSig2Signer
	// with deterministic keys for the merged output.
	aliceMuSig2Key := testKeyFromSeed(t, 0x50)
	bobMuSig2Key := testKeyFromSeed(t, 0x51)
	aliceMuSig2PubKey := aliceMuSig2Key.PubKey()
	bobMuSig2PubKey := bobMuSig2Key.PubKey()

	allMuSig2PubKeys := []*btcec.PublicKey{
		aliceMuSig2PubKey, bobMuSig2PubKey,
	}

	// Create MuSig2 combined key for the output anchor.
	aliceMuSig2Signer := assets.NewLocalMuSig2Signer(aliceMuSig2Key)
	bobMuSig2Signer := assets.NewLocalMuSig2Signer(bobMuSig2Key)

	// We need the combined key for the output. Create a temporary session
	// just to get the combined key.
	tempTweaks := &input.MuSig2Tweaks{
		TaprootBIP0086Tweak: false,
	}
	tempSession, err := aliceMuSig2Signer.MuSig2CreateSession(
		input.MuSig2Version100RC2, keychain.KeyLocator{},
		allMuSig2PubKeys, tempTweaks, nil, nil,
	)
	require.NoError(t, err)
	musig2CombinedKey := tempSession.CombinedKey
	_ = aliceMuSig2Signer.MuSig2Cleanup(tempSession.SessionID)
	t.Logf("MuSig2 combined key: %x",
		musig2CombinedKey.SerializeCompressed())

	// Build merge transaction.
	mergeBuilder := assets.NewAssetTxBuilder(assetID, &chainParams)

	err = mergeBuilder.AddAssetInput(aliceInputCfg)
	require.NoError(t, err)

	err = mergeBuilder.AddAssetInput(bobInputCfg)
	require.NoError(t, err)

	// Single output with MuSig2 anchor key and OP_TRUE script. Use
	// OpTrueUniqueScript with the MuSig2 combined key so that the internal
	// key in the proof matches the MuSig2 combined key. This is required
	// for the split phase to correctly sign with the same key.
	err = mergeBuilder.AddAssetOutput(assets.OutputConfig{
		Amount: totalAmount,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(musig2CombinedKey),
		},
		Script: assets.OpTrueUniqueScript(musig2CombinedKey),
	})
	require.NoError(t, err)

	_, err = mergeBuilder.Compile(ctx)
	require.NoError(t, err)

	// Commit using Alice's wallet (she'll pay the fees).
	err = mergeBuilder.Commit(
		ctx, aliceClient.AssetWalletClient,
		assets.CommitOptions{FeeRate: chainfee.SatPerVByte(10)},
	)
	require.NoError(t, err)

	// The inputs in this anchor PSBT are controlled by internal keys that
	// were derived by different nodes (Alice and Bob). LND's PSBT signer
	// requires BIP32 derivation paths per input, so we attach the key
	// descriptors we used when creating the receiver addresses above.
	mergeAnchor := mergeBuilder.AnchorPsbt()
	err = setPsbtInputKeyDesc(
		mergeAnchor, 0, aliceInternalKey,
		chaincfg.RegressionNetParams.HDCoinType,
	)
	require.NoError(t, err)
	err = setPsbtInputKeyDesc(
		mergeAnchor, 1, bobInternalKey,
		chaincfg.RegressionNetParams.HDCoinType,
	)
	require.NoError(t, err)
	mergeBuilder.SetAnchorPsbt(mergeAnchor)

	// Sign the anchor with both LND wallets. For multi-party signing, we
	// need to sign with each wallet separately before finalizing.
	// First Alice signs.
	signedByAlice, err := alice.LND.WalletKit.SignPsbt(
		ctx, mergeBuilder.AnchorPsbt(),
	)
	require.NoError(t, err)
	mergeBuilder.SetAnchorPsbt(signedByAlice)
	t.Log("Alice signed merge anchor")

	// Then Bob signs.
	signedByBoth, err := bob.LND.WalletKit.SignPsbt(
		ctx, mergeBuilder.AnchorPsbt(),
	)
	require.NoError(t, err)
	mergeBuilder.SetAnchorPsbt(signedByBoth)
	t.Log("Bob signed merge anchor")

	// Finalize the PSBT after collecting partial signatures from both
	// wallets. We rely on LND's PSBT finalizer to assemble the Taproot
	// witness data correctly.
	finalPsbt, _, err := alice.LND.WalletKit.FinalizePsbt(
		ctx, signedByBoth, "",
	)
	require.NoError(t, err)
	mergeBuilder.SetAnchorPsbt(finalPsbt)

	// Publish via Alice's tapd.
	mergeResp, err := mergeBuilder.Publish(
		ctx, aliceClient.TapdClient, "merge-to-musig2",
		assets.PublishOptions{},
	)
	require.NoError(t, err)
	t.Logf("Published merge tx: %x", mergeResp.Transfer.AnchorTxHash)

	// Mine and get proof for merged output.
	mergeMinedBlocks := h.GenerateAndWait(1)
	require.Len(t, mergeMinedBlocks, 1)
	mergeMinedBlock := mergeMinedBlocks[0]

	rpcClient, err := h.BitcoinRPCClient()
	require.NoError(t, err)
	t.Cleanup(func() { rpcClient.Shutdown() })

	mergeBlockHash, err := chainhash.NewHashFromStr(
		mergeMinedBlock.Header.Hash,
	)
	require.NoError(t, err)
	mergeRawBlock, err := rpcClient.GetBlock(mergeBlockHash)
	require.NoError(t, err)

	mergeTd, err := mergeBuilder.GetTransferData()
	require.NoError(t, err)

	mergeAnchorTx, err := psbt.Extract(mergeBuilder.AnchorPsbt())
	require.NoError(t, err)
	mergeAnchorTxHash := mergeAnchorTx.TxHash()

	mergeTxIndex := -1
	for i, tx := range mergeRawBlock.Transactions {
		txHash := tx.TxHash()
		if txHash.IsEqual(&mergeAnchorTxHash) {
			mergeTxIndex = i
			break
		}
	}
	require.NotEqual(t, -1, mergeTxIndex)

	mergedProof, err := assets.BuildProofFromTransferData(
		mergeTd,
		[][]byte{
			aliceProofResp.RawProofFile, bobProofResp.RawProofFile,
		},
		0,
		&assets.ProofParams{
			Block:       mergeRawBlock,
			BlockHeight: uint32(mergeMinedBlock.Header.Height),
			TxIndex:     mergeTxIndex,
		},
	)
	require.NoError(t, err)

	t.Logf("Phase 2 complete: Generated merged proof (len=%d)",
		len(mergedProof))

	// Debug: validate the merge witness locally using the same VM rules
	// tapd uses when importing proof files.
	{
		mergedProofFile, err := proof.DecodeFile(mergedProof)
		require.NoError(t, err)
		mergedLastProof, err := mergedProofFile.LastProof()
		require.NoError(t, err)

		aliceInputFile, err := proof.DecodeFile(
			aliceProofResp.RawProofFile,
		)
		require.NoError(t, err)
		aliceInputLast, err := aliceInputFile.LastProof()
		require.NoError(t, err)

		bobInputFile, err := proof.DecodeFile(
			bobProofResp.RawProofFile,
		)
		require.NoError(t, err)
		bobInputLast, err := bobInputFile.LastProof()
		require.NoError(t, err)

		alicePrevID := mergedLastProof.Asset.PrevWitnesses[0].PrevID
		bobPrevID := mergedLastProof.Asset.PrevWitnesses[1].PrevID
		prevAssets := tapcommitment.InputSet{
			*alicePrevID: &aliceInputLast.Asset,
			*bobPrevID:   &bobInputLast.Asset,
		}

		vmEngine, err := vm.New(
			&mergedLastProof.Asset, nil, prevAssets,
			vm.WithSkipTimeLockValidation(),
		)
		require.NoError(t, err)
		err = vmEngine.Execute()
		if err != nil {
			t.Logf("DEBUG: merge VM validation failed: %v", err)
		}
	}

	// Sanity check: importing the merged proof should succeed before we
	// attempt to spend it in Phase 3.
	_, err = aliceClient.TapdClient.ImportProofFile(ctx, mergedProof)
	require.NoError(t, err)

	// ================================================================
	// Phase 3: Split (1→2) - Split MuSig2 output back to Alice and Bob
	// ================================================================
	t.Log("Phase 3: Split - Divide MuSig2 output to Alice and Bob")

	// Derive wallet keys for Alice and Bob's final outputs.
	aliceWalletScriptKey, aliceWalletInternalKey, err := aliceClient.
		TapdClient.DeriveNewKeys(ctx)
	require.NoError(t, err)

	bobWalletScriptKey, bobWalletInternalKey, err := bobClient.
		TapdClient.DeriveNewKeys(ctx)
	require.NoError(t, err)

	// Build split transaction.
	splitBuilder := assets.NewAssetTxBuilder(assetID, &chainParams)

	// Input is the merged MuSig2 output.
	err = splitBuilder.AddAssetInput(assets.InputConfig{
		ProofFile: mergedProof,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(musig2CombinedKey),
		},
	})
	require.NoError(t, err)

	// Output 1: Alice's share to her wallet (split root).
	aliceInternalKeyBytes := aliceWalletInternalKey.PubKey.
		SerializeCompressed()
	err = splitBuilder.AddAssetOutput(assets.OutputConfig{
		Type:   tappsbt.TypeSplitRoot,
		Amount: halfAmount,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeTapdManaged,
			Key:  aliceInternalKeyBytes,
		},
		Script: assets.DirectWalletScript(&aliceWalletScriptKey),
	})
	require.NoError(t, err)

	// Output 2: Bob's share to his wallet.
	bobInternalKeyBytes := bobWalletInternalKey.PubKey.SerializeCompressed()
	err = splitBuilder.AddAssetOutput(assets.OutputConfig{
		Amount: halfAmount,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeTapdManaged,
			Key:  bobInternalKeyBytes,
		},
		Script: assets.DirectWalletScript(&bobWalletScriptKey),
	})
	require.NoError(t, err)

	_, err = splitBuilder.Compile(ctx)
	require.NoError(t, err)

	// For split transactions, we need wallet funding because we're creating
	// more outputs (2) than inputs (1), so we need additional satoshis.
	// Alice's wallet will fund the additional inputs.
	err = splitBuilder.Commit(
		ctx, aliceClient.AssetWalletClient,
		assets.CommitOptions{FeeRate: chainfee.SatPerVByte(10)},
	)
	require.NoError(t, err)

	// Get the split anchor PSBT sighash for MuSig2 signing.
	splitSigHash, err := assets.GetSigHash(splitBuilder.AnchorPsbt(), 0)
	require.NoError(t, err)

	// Get the taproot root from the merged proof for proper tweaking.
	mergedProofFile, err := proof.DecodeFile(mergedProof)
	require.NoError(t, err)
	mergedLastProof, err := mergedProofFile.LastProof()
	require.NoError(t, err)
	taprootRoot, err := assets.GenTaprootRootFromProof(mergedLastProof)
	require.NoError(t, err)

	// Debug: Log key information.
	proofInternalKey := mergedLastProof.InclusionProof.InternalKey
	t.Logf("DEBUG: Proof internal key: %x",
		schnorr.SerializePubKey(proofInternalKey))
	t.Logf("DEBUG: MuSig2 combined key (used for output): %x",
		schnorr.SerializePubKey(musig2CombinedKey))
	t.Logf("DEBUG: Taproot root from proof: %x", taprootRoot)

	// Verify the output key matches what we expect.
	expectedOutputKey := txscript.ComputeTaprootOutputKey(
		proofInternalKey, taprootRoot,
	)
	t.Logf("DEBUG: Expected output key (from proof internal + root): %x",
		schnorr.SerializePubKey(expectedOutputKey))

	// Also compute using MuSig2 combined key.
	expectedOutputKeyFromMuSig2 := txscript.ComputeTaprootOutputKey(
		musig2CombinedKey, taprootRoot,
	)
	t.Logf("DEBUG: Expected output key (from musig2 combined + root): %x",
		schnorr.SerializePubKey(expectedOutputKeyFromMuSig2))

	// Get the actual output key from the PSBT's WitnessUtxo.
	splitPsbt := splitBuilder.AnchorPsbt()
	if len(splitPsbt.Inputs) > 0 && splitPsbt.Inputs[0].WitnessUtxo != nil {
		actualPkScript := splitPsbt.Inputs[0].WitnessUtxo.PkScript
		if len(actualPkScript) == 34 {
			actualOutputKey := actualPkScript[2:]
			t.Logf("DEBUG: Actual output key: %x", actualOutputKey)
		}
	}

	// MuSig2 signing ceremony for the split.
	tweaks := &input.MuSig2Tweaks{
		TaprootTweak: taprootRoot,
	}

	// Create sessions for both signers.
	aliceSplitSession, err := aliceMuSig2Signer.MuSig2CreateSession(
		input.MuSig2Version100RC2, keychain.KeyLocator{},
		allMuSig2PubKeys, tweaks, nil, nil,
	)
	require.NoError(t, err)

	bobSplitSession, err := bobMuSig2Signer.MuSig2CreateSession(
		input.MuSig2Version100RC2, keychain.KeyLocator{},
		allMuSig2PubKeys, tweaks, nil, nil,
	)
	require.NoError(t, err)

	// Exchange nonces.
	_, err = aliceMuSig2Signer.MuSig2RegisterNonces(
		aliceSplitSession.SessionID,
		[][musig2.PubNonceSize]byte{bobSplitSession.PublicNonce},
	)
	require.NoError(t, err)

	_, err = bobMuSig2Signer.MuSig2RegisterNonces(
		bobSplitSession.SessionID,
		[][musig2.PubNonceSize]byte{aliceSplitSession.PublicNonce},
	)
	require.NoError(t, err)

	// Sign with both parties.
	_, err = aliceMuSig2Signer.MuSig2Sign(
		aliceSplitSession.SessionID, splitSigHash, false,
	)
	require.NoError(t, err)

	bobPartialSig, err := bobMuSig2Signer.MuSig2Sign(
		bobSplitSession.SessionID, splitSigHash, false,
	)
	require.NoError(t, err)

	// Combine signatures.
	finalSig, haveAll, err := aliceMuSig2Signer.MuSig2CombineSig(
		aliceSplitSession.SessionID,
		[]*musig2.PartialSignature{bobPartialSig},
	)
	require.NoError(t, err)
	require.True(t, haveAll)

	// Apply the MuSig2 signature using builder helper. This stores the
	// signature in both the PSBT and anchorWitnesses so it gets preserved
	// after wallet finalization.
	err = splitBuilder.ApplyKeySpendSignature(0, finalSig.Serialize())
	require.NoError(t, err)
	t.Logf("MuSig2 signature applied (len=%d)", len(finalSig.Serialize()))

	// Log PSBT state before finalization.
	prePsbt := splitBuilder.AnchorPsbt()
	input0OutPt := prePsbt.UnsignedTx.TxIn[0].PreviousOutPoint
	t.Logf("Pre-finalize: %d inputs, input 0 outpoint: %s:%d",
		len(prePsbt.Inputs), input0OutPt.Hash, input0OutPt.Index)
	t.Logf("Pre-finalize: input 0 TaprootKeySpendSig len=%d",
		len(prePsbt.Inputs[0].TaprootKeySpendSig))

	// FinalizeAnchor will sign the wallet inputs with Alice's LND wallet
	// and then re-apply our MuSig2 witness from anchorWitnesses.
	splitAnchorPsbt, err := splitBuilder.FinalizeAnchor(
		ctx, alice.LND.WalletKit,
	)
	require.NoError(t, err)

	// Log PSBT state after finalization.
	t.Logf("Post-finalize: input 0 FinalScriptWitness len=%d",
		len(splitAnchorPsbt.Inputs[0].FinalScriptWitness))

	splitTx, err := psbt.Extract(splitAnchorPsbt)
	require.NoError(t, err)

	// Log split transaction details for debugging.
	t.Logf("Split tx hash: %s", splitTx.TxHash())
	t.Logf("Split tx inputs: %d", len(splitTx.TxIn))
	for i, in := range splitTx.TxIn {
		t.Logf("  Input %d: %s:%d, witness len=%d",
			i, in.PreviousOutPoint.Hash, in.PreviousOutPoint.Index,
			len(in.Witness))
	}
	t.Logf("Split tx outputs: %d", len(splitTx.TxOut))
	for i, out := range splitTx.TxOut {
		t.Logf("  Output %d: value=%d, script len=%d",
			i, out.Value, len(out.PkScript))
	}

	// Broadcast the split transaction directly (wallet funded, no CPFP).
	broadcastHash, err := rpcClient.SendRawTransaction(splitTx, false)
	require.NoError(t, err)
	t.Logf("Split tx broadcast: %s", broadcastHash)

	// Mine the split transaction.
	splitMinedBlocks := h.GenerateAndWait(1)
	require.Len(t, splitMinedBlocks, 1)
	splitMinedBlock := splitMinedBlocks[0]

	// Build and import proofs so tapd recognizes the new outputs.
	splitTd, err := splitBuilder.GetTransferData()
	require.NoError(t, err)

	require.Len(t, splitTd.V1InputTxWitnesses, 1)
	require.NotEmpty(t, splitTd.V1InputTxWitnesses[0],
		"expected OP_TRUE witness captured at commit time")
	t.Logf("DEBUG: split TransferData v1 witness items=%d",
		len(splitTd.V1InputTxWitnesses[0]))

	splitTxHash := splitTx.TxHash()
	splitBlockHash, err := chainhash.NewHashFromStr(
		splitMinedBlock.Header.Hash,
	)
	require.NoError(t, err)
	splitRawBlock, err := rpcClient.GetBlock(splitBlockHash)
	require.NoError(t, err)

	splitTxIndex := -1
	for i, tx := range splitRawBlock.Transactions {
		h := tx.TxHash()
		if h.IsEqual(&splitTxHash) {
			splitTxIndex = i
			break
		}
	}
	require.NotEqual(t, -1, splitTxIndex)

	// Debug: Check merged proof script key.
	mergedProofFile2, err := proof.DecodeFile(mergedProof)
	require.NoError(t, err)
	mergedProof2, err := mergedProofFile2.LastProof()
	require.NoError(t, err)
	t.Logf("DEBUG: Merged proof script key: %x",
		mergedProof2.Asset.ScriptKey.PubKey.SerializeCompressed())
	mergedInternalKey := mergedProof2.InclusionProof.InternalKey
	t.Logf("DEBUG: Merged proof internal key: %x",
		schnorr.SerializePubKey(mergedInternalKey))

	// Compute what the OP_TRUE script key should be.
	opTrueArtifacts, err := assets.BuildOpTrueArtifacts(musig2CombinedKey)
	require.NoError(t, err)
	t.Logf("DEBUG: Expected OP_TRUE script key (from musig2): %x",
		opTrueArtifacts.OutputKey.SerializeCompressed())

	// Build proofs for both outputs.
	aliceSplitProof, err := assets.BuildProofFromTransferData(
		splitTd, [][]byte{mergedProof}, 0,
		&assets.ProofParams{
			Block:       splitRawBlock,
			BlockHeight: uint32(splitMinedBlock.Header.Height),
			TxIndex:     splitTxIndex,
		},
	)
	require.NoError(t, err)

	bobSplitProof, err := assets.BuildProofFromTransferData(
		splitTd, [][]byte{mergedProof}, 1,
		&assets.ProofParams{
			Block:       splitRawBlock,
			BlockHeight: uint32(splitMinedBlock.Header.Height),
			TxIndex:     splitTxIndex,
		},
	)
	require.NoError(t, err)

	// Verify the witness is actually attached in the proof files before we
	// attempt import. This makes failures actionable and helps distinguish
	// between proof construction and tapd import issues.
	aliceProofFile, err := proof.DecodeFile(aliceSplitProof)
	require.NoError(t, err)
	aliceLastProof, err := aliceProofFile.LastProof()
	require.NoError(t, err)
	aliceAsset := &aliceLastProof.Asset
	t.Logf("DEBUG: Alice proof version=%v prev_wit=%d has_split=%v",
		aliceAsset.Version, len(aliceAsset.PrevWitnesses),
		aliceAsset.HasSplitCommitmentWitness())
	for i := range aliceAsset.PrevWitnesses {
		w := aliceAsset.PrevWitnesses[i]
		prevIDSet := w.PrevID != nil
		splitSet := w.SplitCommitment != nil
		t.Logf("DEBUG: Alice prev_wit[%d] prev_id=%v split=%v wit=%d",
			i, prevIDSet, splitSet, len(w.TxWitness))
	}
	if aliceAsset.HasSplitCommitmentWitness() {
		if aliceAsset.Version == tapasset.V1 {
			splitRoot := aliceAsset.PrevWitnesses[0].
				SplitCommitment.RootAsset
			rootWit := splitRoot.PrevWitnesses[0].TxWitness
			require.NotEmpty(t, rootWit,
				"Alice split proof missing root TxWitness")
			t.Logf("DEBUG: Alice root witness items=%d",
				len(rootWit))
		}
	} else {
		foundNonSplit := false
		for i := range aliceAsset.PrevWitnesses {
			witness := aliceAsset.PrevWitnesses[i]
			if witness.SplitCommitment != nil {
				continue
			}

			foundNonSplit = true
			if aliceAsset.Version == tapasset.V1 {
				require.NotEmpty(t, witness.TxWitness,
					"Alice proof missing TxWitness")
				t.Logf("DEBUG: Alice prev_wit[%d] items=%d",
					i, len(witness.TxWitness))
			}
		}
		require.True(t, foundNonSplit,
			"Alice proof has no non-split witnesses")
	}

	bobProofFile, err := proof.DecodeFile(bobSplitProof)
	require.NoError(t, err)
	bobLastProof, err := bobProofFile.LastProof()
	require.NoError(t, err)
	bobAsset := &bobLastProof.Asset
	if bobAsset.HasSplitCommitmentWitness() {
		if bobAsset.Version == tapasset.V1 {
			bobSplitRoot := bobAsset.PrevWitnesses[0].
				SplitCommitment.RootAsset
			bobRootWit := bobSplitRoot.PrevWitnesses[0].TxWitness
			require.NotEmpty(t, bobRootWit,
				"Bob split proof missing root TxWitness")
			t.Logf("DEBUG: Bob root witness items=%d",
				len(bobRootWit))
		}
	} else {
		foundNonSplit := false
		for i := range bobAsset.PrevWitnesses {
			witness := bobAsset.PrevWitnesses[i]
			if witness.SplitCommitment != nil {
				continue
			}

			foundNonSplit = true
			if bobAsset.Version == tapasset.V1 {
				require.NotEmpty(t, witness.TxWitness,
					"Bob proof missing TxWitness")
				t.Logf("DEBUG: Bob prev_wit[%d] items=%d",
					i, len(witness.TxWitness))
			}
		}
		require.True(t, foundNonSplit,
			"Bob proof has no non-split witnesses")
	}

	// Import proofs.
	//
	// If the import fails, the underlying issue is usually in virtual TX
	// witness construction. To make that easier to diagnose, we also
	// validate the asset witness locally using the same tapscript engine.
	{
		alicePrevID := aliceAsset.PrevWitnesses[0].PrevID
		prevAssets := tapcommitment.InputSet{
			*alicePrevID: &mergedLastProof.Asset,
		}
		virtualTx, _, err := tapscript.VirtualTx(aliceAsset, prevAssets)
		require.NoError(t, err)

		prevOutFetcher, err := tapscript.InputPrevOutFetcher(
			mergedLastProof.Asset,
		)
		require.NoError(t, err)

		prevOut := prevOutFetcher.FetchPrevOutput(wire.OutPoint{})
		virtualTxCopy := tapasset.VirtualTxWithInput(
			virtualTx, aliceLastProof.Asset.LockTime,
			aliceLastProof.Asset.RelativeLockTime, 0,
			aliceLastProof.Asset.PrevWitnesses[0].TxWitness,
		)
		sigHashes := txscript.NewTxSigHashes(
			virtualTxCopy, prevOutFetcher,
		)
		engine, err := txscript.NewEngine(
			prevOut.PkScript, virtualTxCopy, 0,
			txscript.StandardVerifyFlags, nil, sigHashes,
			prevOut.Value, prevOutFetcher,
		)
		require.NoError(t, err)
		err = engine.Execute()
		if err != nil {
			t.Logf("DEBUG: tapscript validation failed: %v", err)
		}
	}

	_, err = aliceClient.TapdClient.ImportProofFile(ctx, aliceSplitProof)
	require.NoError(t, err)

	_, err = bobClient.TapdClient.ImportProofFile(ctx, bobSplitProof)
	require.NoError(t, err)

	// Verify final balances.
	aliceClient.WaitForAssetBalance(assetID[:], halfAmount)
	bobClient.WaitForAssetBalance(assetID[:], halfAmount)

	t.Log("Phase 3 complete: Split MuSig2(100k) → Alice(50k) + Bob(50k)")
	t.Log("Split Commitment Test PASSED")
}
