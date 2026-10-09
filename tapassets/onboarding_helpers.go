package tapassets

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
)

// validateOutputCommitment binds the returned root to the policy and P2TR
// output.
func validateOutputCommitment(txOut *wire.TxOut, internalKey *btcec.PublicKey,
	policyRoot []byte, output commitOutput) error {

	if txOut == nil {
		return fmt.Errorf("transaction output is required")
	}
	combined := tapBranchHash(policyRoot, output.taprootAssetRoot[:])
	if tapsdk.Hash(combined) != output.taprootMerkleRoot {
		return fmt.Errorf("taproot merkle root mismatch")
	}
	outputKey := txscript.ComputeTaprootOutputKey(internalKey, combined[:])
	wantScript, err := txscript.PayToTaprootScript(outputKey)
	if err != nil {
		return err
	}
	if !bytes.Equal(txOut.PkScript, wantScript) {
		return fmt.Errorf("P2TR output key mismatch")
	}

	return nil
}

// writeDigestBytes length-prefixes bytes in the request identity.
func writeDigestBytes(buffer *bytes.Buffer, value []byte) {
	_ = binary.Write(buffer, binary.BigEndian, uint64(len(value)))
	_, _ = buffer.Write(value)
}

// committedOutput selects an output from the sealed transition.
func committedOutput(committed *commitResult,
	logicalID string) (commitOutput, error) {

	for i := range committed.outputs {
		if committed.outputs[i].logicalOutputID == logicalID {
			return committed.outputs[i], nil
		}
	}

	return commitOutput{}, fmt.Errorf("batch anchor commit misses "+
		"output %q", logicalID)
}

// commitOutcomeKnown distinguishes pre-commit failures from uncertain
// mutations.
func commitOutcomeKnown(err error) bool {
	var attemptErr *tapsdk.CustomAnchorCommitAttemptError
	if errors.As(err, &attemptErr) {
		return !attemptErr.OutcomeUnknown
	}
	var responseErr *tapsdk.CustomAnchorCommitResponseError
	if errors.As(err, &responseErr) {
		return false
	}
	var localResponseErr *commitResponseError
	if errors.As(err, &localResponseErr) {
		return false
	}

	return true
}

// tapBranchHash orders sibling nodes as required by BIP-341.
func tapBranchHash(left, right []byte) chainhash.Hash {
	if bytes.Compare(left, right) > 0 {
		left, right = right, left
	}

	return *chainhash.TaggedHash(chainhash.TagTapBranch, left, right)
}
