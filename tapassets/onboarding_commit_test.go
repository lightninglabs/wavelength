package tapassets

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
)

// fakeOnboardingCommit composes the requested policy with deterministic asset
// roots while leaving wallet funding to the caller's fixture.
func fakeOnboardingCommit(ctx context.Context,
	request *tapsdk.CustomAnchorRequest,
	verifier tapsdk.ConfirmedProofVerifier) (*commitResult, error) {

	for _, input := range request.Inputs {
		if _, err := verifier.VerifyConfirmedProof(
			ctx, input.ProofFile,
		); err != nil {
			return nil, err
		}
	}
	packet, err := psbtutil.Parse(request.AnchorPSBT)
	if err != nil {
		return nil, err
	}
	result := &commitResult{packageBytes: []byte("onboarding-package")}
	for _, output := range request.Outputs {
		internalKey, err := btcec.ParsePubKey(
			output.Anchor.InternalKey.PubKey[:],
		)
		if err != nil {
			return nil, err
		}
		assetRoot := tapsdk.Hash(sha256.Sum256([]byte(output.ID)))
		merkleRoot := assetRoot
		leaves := output.Anchor.Tapscript.TapLeaves
		if len(leaves) > 0 {
			tapLeaves := make([]txscript.TapLeaf, len(leaves))
			for i := range leaves {
				tapLeaves[i] = txscript.NewBaseTapLeaf(
					leaves[i].Script,
				)
			}
			policyRoot := txscript.AssembleTaprootScriptTree(
				tapLeaves...,
			).RootNode.TapHash()
			merkleRoot = tapsdk.Hash(
				tapBranchHash(policyRoot[:], assetRoot[:]),
			)
		}
		script, err := composedScript(internalKey, merkleRoot)
		if err != nil {
			return nil, err
		}
		txOut := packet.UnsignedTx.TxOut[output.AnchorOutputIndex]
		txOut.PkScript = script
		scriptKey := tapsdk.PubKey{}
		witness := [][]byte{{txscript.OP_TRUE}, {1, 2, 3}}
		if output.Script.External != nil {
			scriptKey = output.Script.External.ScriptKey.PubKey
			witness = nil
		}
		result.outputs = append(result.outputs, commitOutput{
			logicalOutputID:   output.ID,
			anchorOutputIndex: output.AnchorOutputIndex,
			anchorValueSat:    int64(output.AnchorValueSat),
			assetRef:          output.AssetRef,
			amount:            output.Amount,
			taprootAssetRoot:  assetRoot,
			taprootMerkleRoot: merkleRoot,
			scriptMode:        output.Script.Mode,
			scriptKey:         scriptKey,
			opTrueWitness:     witness,
			proofBlob: []byte(
				fmt.Sprintf("%s-proof", output.ID),
			),
		})
	}
	for i := range result.outputs {
		result.outputs[i].anchorOutpoint = tapsdk.Outpoint{
			Txid:  packet.UnsignedTx.TxHash(),
			Index: result.outputs[i].anchorOutputIndex,
		}
	}
	result.anchorPSBT, err = psbtutil.Serialize(packet)

	return result, err
}

// testPrivateKey provides reproducible keys for journal and policy tests.
func testPrivateKey(t *testing.T, value byte) *btcec.PrivateKey {
	t.Helper()
	key, _ := btcec.PrivKeyFromBytes([]byte{value})

	return key
}

// sha256Bytes derives deterministic fixture identities.
func sha256Bytes(value []byte) [32]byte { return sha256.Sum256(value) }
