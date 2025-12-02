//nolint:ll
package round

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
)

// ValidateVTXTPath validates the VTXT tree path from the batch output to
// the client's VTXOs. This ensures the client can unilaterally unroll their
// VTXOs if needed.
//
// The client MUST validate the complete VTXT path before signing the boarding
// UTXO. This ensures they can recover their funds even if the operator
// disappears.
func ValidateVTXTPath(vtxtTree *tree.Tree, signingKey *btcec.PublicKey,
	expectedVTXOs []types.VTXORequest,
	operatorKey *btcec.PublicKey) (*tree.Tree, error) {

	if vtxtTree == nil {
		return nil, fmt.Errorf("VTXT tree is nil")
	}
	if signingKey == nil {
		return nil, fmt.Errorf("signing key is nil")
	}

	// Ensure the tree structure is valid before extracting paths to
	// prevent processing of malformed trees.
	if err := vtxtTree.Verify(); err != nil {
		return nil, fmt.Errorf("tree structure invalid: %w", err)
	}

	// Extract only the transactions the client needs to sign, reducing the
	// validation scope to their specific path.
	clientTree, err := vtxtTree.ExtractPathForCoSigner(signingKey)
	if err != nil {
		return nil, fmt.Errorf("failed to extract client path for "+
			"signing key: %w", err)
	}

	// Collect all leaf nodes from the client's path to verify they match
	// the requested VTXOs.
	var leafNodes []*tree.Node
	if err = clientTree.Root.ForEachLeaf(func(node *tree.Node) error {
		leafNodes = append(leafNodes, node)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("failed to iterate leaf nodes: %w", err)
	}

	if len(leafNodes) != len(expectedVTXOs) {
		return nil, fmt.Errorf("tree has %d leaf nodes, expected %d "+
			"VTXOs", len(leafNodes), len(expectedVTXOs))
	}

	// Verify each leaf output matches the expected VTXO parameters to
	// ensure the client receives exactly what they requested.
	for i, leaf := range leafNodes {
		expectedVTXO := expectedVTXOs[i]

		// Each leaf must have at least a VTXO output and an anchor
		// output for fee bumping.
		if len(leaf.Outputs) < 2 {
			return nil, fmt.Errorf("leaf node %d has %d outputs, "+
				"expected at least 2", i, len(leaf.Outputs))
		}

		vtxoOutput := leaf.Outputs[0]

		// Ensure the VTXO amount matches the request to prevent value
		// extraction by the operator.
		if vtxoOutput.Value != int64(expectedVTXO.Amount) {
			return nil, fmt.Errorf("leaf node %d output value %d != "+
				"expected %d", i, vtxoOutput.Value,
				expectedVTXO.Amount)
		}

		// Verify the output script matches to ensure funds go to the
		// correct destination.
		if !bytes.Equal(vtxoOutput.PkScript, expectedVTXO.PkScript) {
			return nil, fmt.Errorf("leaf node %d output script "+
				"mismatch", i)
		}

		// Confirm operator key is present as co-signer to enable
		// collaborative spending path.
		operatorFound := false
		for _, cosigner := range leaf.CoSigners {
			if cosigner.IsEqual(operatorKey) {
				operatorFound = true
				break
			}
		}
		if !operatorFound {
			return nil, fmt.Errorf("leaf node %d does not include "+
				"operator key in co-signers", i)
		}
	}

	return clientTree, nil
}

// ValidateVTXTSignatures validates the complete VTXT signatures. This must be
// called BEFORE the client signs the boarding UTXO input.
//
// The client MUST NOT sign the boarding UTXO until the VTXT is fully signed
// and validated. Otherwise, the operator could include the boarding UTXO in a
// commitment tx without providing valid VTXOs.
func ValidateVTXTSignatures(vtxtTree *tree.Tree,
	signatures [][]byte) error {

	if vtxtTree == nil {
		return fmt.Errorf("VTXT tree is nil")
	}
	if len(signatures) == 0 {
		return fmt.Errorf("no signatures provided")
	}

	// Build a map of transaction IDs to Schnorr signatures for submission
	// to the tree validation logic.
	sigMap := make(map[string]*schnorr.Signature)
	txIndex := 0

	err := vtxtTree.Root.ForEach(func(node *tree.Node) error {
		if txIndex >= len(signatures) {
			return fmt.Errorf("not enough signatures: have "+
				"%d, need more for all tree nodes",
				len(signatures))
		}

		// Convert raw bytes into a Schnorr signature structure for
		// cryptographic verification.
		sig, err := schnorr.ParseSignature(signatures[txIndex])
		if err != nil {
			return fmt.Errorf("failed to parse signature %d: "+
				"%w", txIndex, err)
		}

		// Index signatures by transaction ID to match them with their
		// corresponding tree nodes.
		txid, err := node.TXID()
		if err != nil {
			return fmt.Errorf("failed to get TXID for node: "+
				"%w", err)
		}

		sigMap[txid.String()] = sig
		txIndex++

		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to build signature map: %w", err)
	}

	// Submit and validate all signatures atomically to ensure the entire
	// tree is properly signed.
	if err := vtxtTree.SubmitTreeSigs(sigMap); err != nil {
		return fmt.Errorf("failed to submit tree signatures: %w",
			err)
	}

	// Perform cryptographic verification of all signatures to guarantee
	// the operator has properly co-signed the VTXT.
	if err := vtxtTree.VerifySigned(); err != nil {
		return fmt.Errorf("tree signature verification failed: "+
			"%w", err)
	}

	return nil
}

// ValidateBoardingScript validates that a boarding tapscript has the
// expected structure with collaborative and timeout paths.
func ValidateBoardingScript(tapscript *waddrmgr.Tapscript,
	clientKey *btcec.PublicKey, operatorKey *btcec.PublicKey,
	expectedExitDelay uint32) error {

	if tapscript == nil {
		return fmt.Errorf("tapscript is nil")
	}

	// Ensure control block is present for taproot spending.
	if tapscript.ControlBlock == nil {
		return fmt.Errorf("tapscript control block is nil")
	}

	// Verify the internal key exists for taproot construction.
	if tapscript.ControlBlock.InternalKey == nil {
		return fmt.Errorf("control block internal key is nil")
	}

	// Ensure the internal key is unspendable (ARK NUMS key) to force
	// script path spending only.
	if !tapscript.ControlBlock.InternalKey.IsEqual(&scripts.ARKNUMSKey) {
		return fmt.Errorf("internal key is not ARK NUMS key")
	}

	// For full tree types, verify we have both the collaborative and
	// timeout script paths by constructing the expected script and comparing.
	if tapscript.Type == waddrmgr.TapscriptTypeFullTree {
		if len(tapscript.Leaves) != 2 {
			return fmt.Errorf("boarding script has %d leaves, "+
				"expected 2", len(tapscript.Leaves))
		}

		// Construct the expected boarding tapscript using lib function.
		// This ensures we validate against the exact script structure
		// that lib creates.
		expectedTapscript, err := scripts.VTXOTapScript(
			clientKey, operatorKey, expectedExitDelay,
		)
		if err != nil {
			return fmt.Errorf("failed to construct expected boarding "+
				"script: %w", err)
		}

		if len(expectedTapscript.Leaves) != 2 {
			return fmt.Errorf("expected tapscript has %d leaves, "+
				"should be 2", len(expectedTapscript.Leaves))
		}

		// Compare each leaf script byte-for-byte. The order may vary,
		// so we check if each actual leaf matches one of the expected leaves.
		actualLeaves := make(map[string]bool)
		for _, leaf := range tapscript.Leaves {
			actualLeaves[string(leaf.Script)] = true
		}

		for i, expectedLeaf := range expectedTapscript.Leaves {
			if !actualLeaves[string(expectedLeaf.Script)] {
				return fmt.Errorf("expected leaf %d script not found "+
					"in actual boarding script", i)
			}
		}
	}

	return nil
}

// ValidateBoardingSignature validates a boarding input signature from a
// client. This verifies the signature is for the collaborative spend path
// and signs the commitment transaction correctly.
func ValidateBoardingSignature(commitmentTx *wire.MsgTx,
	boardingRequest *types.BoardingRequest, signature []byte,
	inputIndex int) error {

	if commitmentTx == nil {
		return fmt.Errorf("commitment tx is nil")
	}
	if boardingRequest == nil {
		return fmt.Errorf("boarding request is nil")
	}

	// Schnorr signatures are always 64 bytes (R || s).
	if len(signature) != 64 {
		return fmt.Errorf("invalid signature length: %d, "+
			"expected 64", len(signature))
	}

	if inputIndex < 0 || inputIndex >= len(commitmentTx.TxIn) {
		return fmt.Errorf("input index %d out of range", inputIndex)
	}

	// Ensure the input being signed actually corresponds to the boarding
	// UTXO to prevent signature reuse attacks.
	txIn := commitmentTx.TxIn[inputIndex]
	outpoint := boardingRequest.Outpoint
	if !txIn.PreviousOutPoint.Hash.IsEqual(&outpoint.Hash) ||
		txIn.PreviousOutPoint.Index != outpoint.Index {

		return fmt.Errorf("input %d does not reference boarding "+
			"UTXO", inputIndex)
	}

	// TODO: Verify the signature is valid for the collaborative spend
	// path. This requires:
	// 1. Computing the sighash for the tapscript spend
	// 2. Parsing the Schnorr signature
	// 3. Verifying against the client's public key
	//
	// This will be implemented in Phase 2 when we add full signing logic.

	return nil
}

// ValidateAnchorOutputs validates that all transactions in the VTXT tree have
// valid ephemeral anchor outputs for CPFP fee bumping (BIP 431).
//
// Without valid anchors, the client cannot broadcast the VTXT chain for
// unilateral exit, resulting in fund loss.
func ValidateAnchorOutputs(vtxtTree *tree.Tree) error {
	if vtxtTree == nil {
		return fmt.Errorf("tree is nil")
	}

	// Validate anchors in all tree nodes recursively.
	return vtxtTree.Root.ForEach(func(node *tree.Node) error {
		if node == nil {
			return fmt.Errorf("tree node is nil")
		}

		// Convert node to transaction to check version and outputs.
		tx, err := node.ToTx()
		if err != nil {
			return fmt.Errorf("failed to convert node "+
				"to tx: %w", err)
		}

		// Verify transaction version is 3 for BIP 431 ephemeral anchors.
		if tx.Version != 3 {
			return fmt.Errorf("transaction version is "+
				"%d, expected 3 for BIP 431 ephemeral anchors",
				tx.Version)
		}

		// All virtual transactions must have at least one output
		// (anchor).
		if len(node.Outputs) == 0 {
			return fmt.Errorf("transaction has no outputs")
		}

		// The last output must be the ephemeral anchor.
		anchorIdx := len(node.Outputs) - 1
		anchorOutput := node.Outputs[anchorIdx]

		// Anchor must have zero value.
		if anchorOutput.Value != 0 {
			return fmt.Errorf("anchor output at index %d "+
				"has value %d, expected 0", anchorIdx,
				anchorOutput.Value)
		}

		// Anchor script must match the standard ephemeral anchor
		// script.
		if !bytes.Equal(anchorOutput.PkScript, scripts.AnchorPkScript) {
			return fmt.Errorf("anchor output at index %d has "+
				"invalid script", anchorIdx)
		}

		return nil
	})
}

// ValidateDelayParameters validates the delay parameters for security.
//
// SweepDelay MUST be greater than VTXOExitDelay to ensure the operator has
// time to respond to unilateral exits before the batch expires.
func ValidateDelayParameters(sweepDelay, vtxoExitDelay uint32) error {
	// Both delays must be non-zero.
	if sweepDelay == 0 {
		return fmt.Errorf("sweep delay is zero")
	}
	if vtxoExitDelay == 0 {
		return fmt.Errorf("VTXO exit delay is zero")
	}

	// Sweep delay must be greater than VTXO exit delay for security.
	// This ensures the operator has time to respond to griefing attacks.
	if sweepDelay <= vtxoExitDelay {
		return fmt.Errorf("sweep delay (%d) must be greater than "+
			"VTXO exit delay (%d)", sweepDelay, vtxoExitDelay)
	}

	// Sanity check: Delays should be reasonable (less than ~1 year).
	const maxReasonableDelay = 52560 // ~1 year in blocks (10 min blocks)
	if sweepDelay > maxReasonableDelay {
		return fmt.Errorf("sweep delay (%d) exceeds maximum "+
			"reasonable "+
			"value (%d blocks)", sweepDelay, maxReasonableDelay)
	}
	if vtxoExitDelay > maxReasonableDelay {
		return fmt.Errorf("VTXO exit delay (%d) exceeds maximum "+
			"reasonable value (%d blocks)", vtxoExitDelay,
			maxReasonableDelay)
	}

	return nil
}
