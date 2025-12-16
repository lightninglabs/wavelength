package assets_test

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/darepo-client/assets"
	tapasset "github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Unit Test Helpers
// ============================================================================

func csvClosureScript(pub *btcec.PublicKey, delay uint32) assets.ScriptClosure {
	return (&assets.CSVClosure{
		Key:   pub,
		Delay: delay,
	}).ScriptClosure()
}

func checkSigAddScriptClosure(userKey *btcec.PublicKey,
	operatorKey *btcec.PublicKey) assets.ScriptClosure {

	return (&assets.CheckSigAddClosure{
		Key1: userKey,
		Key2: operatorKey,
	}).ScriptClosure()
}

// unitTestKeyFromSeed generates a deterministic private key from a seed byte.
func unitTestKeyFromSeed(t *testing.T, seed byte) *btcec.PrivateKey {
	t.Helper()

	var privKeyBytes [32]byte
	for i := range privKeyBytes {
		privKeyBytes[i] = seed
	}

	privKey, _ := btcec.PrivKeyFromBytes(privKeyBytes[:])

	return privKey
}

// closuresToSiblingPreimage builds a tapscript sibling preimage from closures.
func closuresToSiblingPreimage(closures []assets.ScriptClosure) (
	*commitment.TapscriptPreimage, error) {

	if len(closures) == 0 {
		return nil, nil
	}

	// Single closure: create a leaf preimage.
	if len(closures) == 1 {
		tapLeaf, err := closures[0].TapLeaf()
		if err != nil {
			return nil, err
		}

		preimage, err := commitment.NewPreimageFromLeaf(tapLeaf)
		if err != nil {
			return nil, err
		}

		return preimage, nil
	}

	// Multiple closures: build tree from leaves.
	leaves := make([]txscript.TapLeaf, 0, len(closures))
	for _, c := range closures {
		tapLeaf, err := c.TapLeaf()
		if err != nil {
			return nil, err
		}

		leaves = append(leaves, tapLeaf)
	}

	tapTree := txscript.AssembleTaprootScriptTree(leaves...)
	branch, ok := tapTree.RootNode.(txscript.TapBranch)
	if !ok {
		return nil, nil
	}

	preimage := commitment.NewPreimageFromBranch(branch)

	return &preimage, nil
}

// ============================================================================
// Unit Tests
// ============================================================================

// TestAssetScriptClosures tests the script closure functionality with the
// builder.
func TestAssetScriptClosures(t *testing.T) {
	// Test that closures generate valid scripts and can be combined into
	// tapscript trees.

	userPriv := unitTestKeyFromSeed(t, 0x03)
	operatorPriv := unitTestKeyFromSeed(t, 0x04)
	userPubKey := userPriv.PubKey()
	operatorPubKey := operatorPriv.PubKey()

	// Create closures.
	csvClosure := csvClosureScript(userPubKey, 144)
	coopClosure := checkSigAddScriptClosure(userPubKey, operatorPubKey)

	// Verify CSV closure script.
	csvLeaf, err := csvClosure.TapLeaf()
	require.NoError(t, err)
	disasm, err := txscript.DisasmString(csvLeaf.Script)
	require.NoError(t, err)
	t.Logf("CSV script: %s", disasm)
	require.Contains(t, disasm, "OP_CHECKSIGVERIFY")
	require.Contains(t, disasm, "OP_CHECKSEQUENCEVERIFY")

	// Verify cooperative closure script.
	coopLeaf, err := coopClosure.TapLeaf()
	require.NoError(t, err)
	disasm, err = txscript.DisasmString(coopLeaf.Script)
	require.NoError(t, err)
	t.Logf("Coop script: %s", disasm)
	require.Contains(t, disasm, "OP_CHECKSIGADD")
	require.Contains(t, disasm, "OP_EQUAL")

	// Build sibling preimage from closures.
	closures := []assets.ScriptClosure{coopClosure, csvClosure}
	sibling, err := closuresToSiblingPreimage(closures)
	require.NoError(t, err)
	require.NotNil(t, sibling)

	// Single closure should produce leaf preimage.
	singleSibling, err := closuresToSiblingPreimage(
		[]assets.ScriptClosure{csvClosure},
	)
	require.NoError(t, err)
	require.NotNil(t, singleSibling)
}

// TestOpTrueArtifacts tests the OP_TRUE artifact generation.
func TestOpTrueArtifacts(t *testing.T) {
	// Test with NUMS key.
	numsArtifacts, err := assets.BuildOpTrueArtifacts(tapasset.NUMSPubKey)
	require.NoError(t, err)
	require.NotNil(t, numsArtifacts.ScriptKey)
	require.NotNil(t, numsArtifacts.OutputKey)
	require.NotEmpty(t, numsArtifacts.ControlBlock)
	require.NotEmpty(t, numsArtifacts.Witness)

	t.Logf("NUMS script key: %x",
		numsArtifacts.ScriptKey.PubKey.SerializeCompressed())
	t.Logf("NUMS output key: %x",
		schnorr.SerializePubKey(numsArtifacts.OutputKey))

	// Test with custom key.
	customPriv := unitTestKeyFromSeed(t, 0x05)
	customKey := customPriv.PubKey()

	customArtifacts, err := assets.BuildOpTrueArtifacts(customKey)
	require.NoError(t, err)
	require.NotNil(t, customArtifacts.ScriptKey)

	// Custom key should produce different script key than NUMS.
	require.NotEqual(t,
		numsArtifacts.ScriptKey.PubKey.SerializeCompressed(),
		customArtifacts.ScriptKey.PubKey.SerializeCompressed(),
	)

	t.Logf("Custom script key: %x",
		customArtifacts.ScriptKey.PubKey.SerializeCompressed())
}

// TestAnchorKeyValidation tests the anchor key spec validation.
func TestAnchorKeyValidation(t *testing.T) {
	userPriv := unitTestKeyFromSeed(t, 0x06)
	userPubKey := userPriv.PubKey()

	// Valid MuSig2 spec.
	userPubKeyBytes := userPubKey.SerializeCompressed()
	validMuSig := assets.AnchorKeySpec{
		Mode: assets.AnchorKeyModeMuSig2,
		MuSig2: &assets.MuSig2Spec{
			Participants: []assets.MuSig2Participant{{
				Role:   "user",
				PubKey: userPubKeyBytes,
			}},
		},
	}
	t.Logf("Valid MuSig2 spec: %+v", validMuSig)

	// Valid static spec.
	validStatic := assets.AnchorKeySpec{
		Mode: assets.AnchorKeyModeStatic,
		Key:  schnorr.SerializePubKey(userPubKey),
	}
	t.Logf("Valid static spec key length: %d", len(validStatic.Key))
	require.Equal(t, 32, len(validStatic.Key))
}

// TestScriptSpendWitness tests script path witness construction.
func TestScriptSpendWitness(t *testing.T) {
	userPriv := unitTestKeyFromSeed(t, 0x07)
	operatorPriv := unitTestKeyFromSeed(t, 0x08)
	userPubKey := userPriv.PubKey()
	operatorPubKey := operatorPriv.PubKey()

	// Create cooperative closure.
	coopClosure := &assets.CheckSigAddClosure{
		Key1: userPubKey,
		Key2: operatorPubKey,
	}

	sc := coopClosure.ScriptClosure()
	require.Equal(t, "coop_multisig", sc.ID)

	// Create mock signatures.
	mockSig1 := make([]byte, 64)
	mockSig2 := make([]byte, 64)
	copy(mockSig1, "sig1_placeholder_64bytes_xxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
	copy(mockSig2, "sig2_placeholder_64bytes_xxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

	controlBlock := []byte("control_block_placeholder")

	userKeyHex := hex.EncodeToString(schnorr.SerializePubKey(userPubKey))
	operatorKeyHex := hex.EncodeToString(
		schnorr.SerializePubKey(operatorPubKey),
	)
	sigs := map[string][]byte{
		userKeyHex:     mockSig1,
		operatorKeyHex: mockSig2,
	}

	witness, err := sc.Witness(controlBlock, sigs)
	require.NoError(t, err)
	require.Len(t, witness, 4) // [sig2, sig1, script, control_block]

	t.Logf("Witness stack length: %d", len(witness))
}
