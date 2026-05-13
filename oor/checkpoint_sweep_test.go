package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	checkpointtx "github.com/lightninglabs/darepo-client/lib/tx/checkpoint"
	"github.com/stretchr/testify/require"
)

// TestSweepInfoFromCheckpointPSBTBindsTapTree verifies the loader accepts a
// canonical finalized checkpoint PSBT — the on-chain pkScript was derived
// from the same tap tree the PSBT advertises in TaprootTapTree, so the
// binding check must pass.
func TestSweepInfoFromCheckpointPSBTBindsTapTree(t *testing.T) {
	t.Parallel()

	pkt, _, _ := buildSignedCheckpointPSBT(t)
	raw, err := serializePSBT(pkt)
	require.NoError(t, err)

	input := pkt.UnsignedTx.TxIn[0].PreviousOutPoint
	info, err := sweepInfoFromCheckpointPSBT(input, raw)
	require.NoError(t, err)
	require.NotNil(t, info)
	require.Equal(t, input, info.InputOutpoint)
	require.NotEmpty(t, info.TapTreeEncoded)
}

// TestSweepInfoFromCheckpointPSBTRejectsPoisonedTapTree is the regression test
// for the unvalidated-tap-tree finding. PSBT output metadata is unsigned: a
// malicious OOR client can swap out the persisted TaprootTapTree to a tree
// containing a different "owner leaf" while leaving the on-chain pkScript and
// signatures intact. Before the binding check, the loader would happily hand
// this poisoned blob to the fraud sweep builder, which would only fail much
// later when the rebuilt control block did not commit to the real output
// script — silently breaking timely fraud recovery. With the binding check,
// the loader rejects the row up front.
func TestSweepInfoFromCheckpointPSBTRejectsPoisonedTapTree(t *testing.T) {
	t.Parallel()

	pkt, _, operatorKey := buildSignedCheckpointPSBT(t)

	// Construct an alternative owner leaf that is NOT the one committed to
	// by the checkpoint output's pkScript. Re-encoding the tree with this
	// alternative leaf preserves the canonical two-leaf shape and the
	// operator CSV timeout leaf, so existing tree-shape and policy checks
	// (e.g. the submit-policy operator-leaf scan) still accept it.
	otherOwner, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	fakeOwnerLeaf, err := (&arkscript.Multisig{
		Keys: []*btcec.PublicKey{
			otherOwner.PubKey(), operatorKey.PubKey(),
		},
	}).Script()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}
	timeoutLeaf, err := arkscript.UnilateralCSVTimeoutTapLeaf(
		policy.OperatorKey, policy.CSVDelay,
	)
	require.NoError(t, err)

	poisonedTree, err := checkpointtx.EncodeTapTree([][]byte{
		timeoutLeaf.Script,
		fakeOwnerLeaf,
	})
	require.NoError(t, err)

	pkt.Outputs[0].TaprootTapTree = poisonedTree

	raw, err := serializePSBT(pkt)
	require.NoError(t, err)

	input := pkt.UnsignedTx.TxIn[0].PreviousOutPoint
	_, err = sweepInfoFromCheckpointPSBT(input, raw)
	require.Error(t, err)
	require.ErrorContains(t, err, "tap tree does not commit to checkpoint")
}

// TestVerifyCheckpointTapTreeRejectsWrongLeafCount covers the shape guard:
// trees with anything other than the canonical [timeout, owner] pair cannot
// have produced the on-chain pkScript via CheckpointTapScript, so the loader
// rejects them before doing any cryptographic work.
func TestVerifyCheckpointTapTreeRejectsWrongLeafCount(t *testing.T) {
	t.Parallel()

	tooFew, err := checkpointtx.EncodeTapTree([][]byte{{0x51}})
	require.NoError(t, err)

	err = verifyCheckpointTapTreeBindsToPkScript(
		tooFew, []byte{0x51, 0x20, 0x00},
	)
	require.ErrorContains(t, err, "tap tree has 1 leaves")

	tooMany, err := checkpointtx.EncodeTapTree([][]byte{
		{0x51}, {0x52}, {0x53},
	})
	require.NoError(t, err)

	err = verifyCheckpointTapTreeBindsToPkScript(
		tooMany, []byte{0x51, 0x20, 0x00},
	)
	require.ErrorContains(t, err, "tap tree has 3 leaves")
}

// TestVerifyCheckpointTapTreeRejectsEmptyPkScript surfaces the trivial
// precondition: the caller must supply a non-empty pkScript to compare
// against, otherwise the binding result would be meaningless.
func TestVerifyCheckpointTapTreeRejectsEmptyPkScript(t *testing.T) {
	t.Parallel()

	encoded, err := checkpointtx.EncodeTapTree([][]byte{{0x51}, {0x52}})
	require.NoError(t, err)

	err = verifyCheckpointTapTreeBindsToPkScript(encoded, nil)
	require.ErrorContains(t, err, "pkScript is empty")
}

// TestSweepInfoFromCheckpointPSBTRoundTripOutpoint sanity-checks that the
// loader echoes back the requested input outpoint regardless of what is
// encoded inside the PSBT — the outpoint is the lookup key, not derived from
// PSBT contents.
func TestSweepInfoFromCheckpointPSBTRoundTripOutpoint(t *testing.T) {
	t.Parallel()

	pkt, _, _ := buildSignedCheckpointPSBT(t)
	raw, err := serializePSBT(pkt)
	require.NoError(t, err)

	custom := wire.OutPoint{Index: 7}
	custom.Hash[0] = 0xff

	info, err := sweepInfoFromCheckpointPSBT(custom, raw)
	require.NoError(t, err)
	require.Equal(t, custom, info.InputOutpoint)
}
