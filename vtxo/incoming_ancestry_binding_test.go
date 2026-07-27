package vtxo

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/batchcanon"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/stretchr/testify/require"
)

func bindingKey(t *testing.T) *btcec.PublicKey {
	t.Helper()
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return priv.PubKey()
}

// taprootScript returns PayToTaproot(ComputeFinalKey([keys...], sweepRoot)) — the
// output script a genuine tree node commits its children to.
func taprootScript(t *testing.T, sweepRoot []byte,
	keys ...*btcec.PublicKey) []byte {

	t.Helper()
	finalKey, err := tree.ComputeFinalKey(keys, sweepRoot)
	require.NoError(t, err)
	script, err := txscript.PayToTaprootScript(finalKey)
	require.NoError(t, err)

	return script
}

// oneLeafTree builds a structurally-valid single-leaf VTXO tree whose root
// spends `batchOutput` (the authenticated commitment output) and whose leaf pays
// vtxoScript, cosigned by operator+owner. It is UNSIGNED (NewTree does not sign).
func oneLeafTree(t *testing.T, commitment chainhash.Hash, sweepRoot []byte,
	operator, owner *btcec.PublicKey, batchOutput *wire.TxOut,
	vtxoScript []byte) *tree.Tree {

	t.Helper()
	leaves := []tree.LeafDescriptor{{
		PkScript:    vtxoScript,
		Amount:      1000,
		CoSignerKey: owner,
	}}
	tr, err := tree.NewTree(
		wire.OutPoint{Hash: commitment}, batchOutput, leaves,
		operator, sweepRoot, 2,
	)
	require.NoError(t, err)

	return tr
}

// TestBindingRejectsForeignKeys is THE bypass-defense assertion. An attacker
// fabricates an internally-consistent tree signed with THEIR OWN keys (so a bare
// signature check would pass) whose leaf carries the victim's public script. But
// the batch output is authenticated to the REAL commitment output = a taproot
// key the attacker does not control. The key-to-output binding rejects it: the
// root's recomputed aggregate key does not match the authenticated output it
// spends. This is the single check that closes the "signs with own keys" hole.
func TestBindingRejectsForeignKeys(t *testing.T) {
	t.Parallel()

	sweepRoot := make([]byte, 32)
	var commitment chainhash.Hash
	commitment[0] = 0xab
	vtxoScript := []byte("victim_vtxo_script")

	// The authenticated commitment output belongs to the REAL round key.
	realOperator := bindingKey(t)
	realOwner := bindingKey(t)
	authedOutput := &wire.TxOut{
		Value:    1000,
		PkScript: taprootScript(t, sweepRoot, realOperator, realOwner),
	}

	// The attacker's tree uses keys THEY control, but its root spends the
	// authenticated (real-key) output.
	attackerOperator := bindingKey(t)
	attackerOwner := bindingKey(t)
	tr := oneLeafTree(
		t, commitment, sweepRoot, attackerOperator, attackerOwner,
		authedOutput, vtxoScript,
	)

	err := VerifyReceivedVTXOBinding(
		[]Ancestry{{TreePath: tr, CommitmentTxID: commitment}},
		vtxoScript,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not match the output it spends")
}

// TestBindingUnsignedGenuineTreeFailsOnlyAtSignatures proves the key-to-output
// binding does NOT false-reject a genuine tree: a structurally-genuine tree
// whose batch output IS PayToTaproot(its own root key) passes structure and the
// key binding, and is rejected only because it is unsigned. This is the negative
// control for the check in TestBindingRejectsForeignKeys and shows genuine trees
// clear steps 1-3 (so the accept path — validated end-to-end against real signed
// trees in itests — is not blocked by this check).
func TestBindingUnsignedGenuineTreeFailsOnlyAtSignatures(t *testing.T) {
	t.Parallel()

	sweepRoot := make([]byte, 32)
	var commitment chainhash.Hash
	commitment[0] = 0xcd
	vtxoScript := []byte("genuine_vtxo_script")

	operator := bindingKey(t)
	owner := bindingKey(t)
	// Genuine: the batch output the root spends IS PayToTaproot(root key).
	genuineOutput := &wire.TxOut{
		Value:    1000,
		PkScript: taprootScript(t, sweepRoot, operator, owner),
	}
	tr := oneLeafTree(
		t, commitment, sweepRoot, operator, owner, genuineOutput,
		vtxoScript,
	)

	err := VerifyReceivedVTXOBinding(
		[]Ancestry{{TreePath: tr, CommitmentTxID: commitment}},
		vtxoScript,
	)
	require.Error(t, err)
	require.Contains(
		t, err.Error(), "signatures",
		"a genuine-structured tree must clear structure + key binding "+
			"and fail only on the missing signatures",
	)
	require.NotContains(t, err.Error(), "does not match the output")
}

// TestOORLineageRejectsForeignKeys is the OOR bypass-defense. The attacker's
// tree spends the authenticated commitment output (so the evidence anchor
// passes) but its nodes are signed with the attacker's own keys: the
// key-to-output binding rejects it, exactly as on the in-round path.
func TestOORLineageRejectsForeignKeys(t *testing.T) {
	t.Parallel()

	sweepRoot := make([]byte, 32)
	var commitment chainhash.Hash
	commitment[0] = 0xab

	realOperator := bindingKey(t)
	realOwner := bindingKey(t)
	authedScript := taprootScript(t, sweepRoot, realOperator, realOwner)
	evidence := []batchcanon.BatchEvidence{{
		BatchTxID:            commitment,
		BatchOutputIndex:     0,
		ConfirmationPkScript: authedScript,
	}}

	// Root spends the authenticated (real-key) output, but the tree is the
	// attacker's, cosigned with keys THEY hold.
	attackerOperator := bindingKey(t)
	attackerOwner := bindingKey(t)
	tr := oneLeafTree(
		t, commitment, sweepRoot, attackerOperator, attackerOwner,
		&wire.TxOut{Value: 1000, PkScript: authedScript},
		[]byte("input_vtxo_script"),
	)

	err := VerifyOORAncestryLineage(
		[]Ancestry{{TreePath: tr, CommitmentTxID: commitment}},
		evidence, 0, nil,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not match the output it spends")
}

// TestOORLineageRejectsEvidenceMismatch proves the evidence anchor: an
// internally-consistent attacker tree rooted at an output whose script the
// attacker controls is rejected because it does not equal the authenticated
// commitment output the client will watch.
func TestOORLineageRejectsEvidenceMismatch(t *testing.T) {
	t.Parallel()

	sweepRoot := make([]byte, 32)
	var commitment chainhash.Hash
	commitment[0] = 0xab

	// The authenticated commitment output belongs to the real round key.
	realScript := taprootScript(t, sweepRoot, bindingKey(t), bindingKey(t))
	evidence := []batchcanon.BatchEvidence{{
		BatchTxID:            commitment,
		BatchOutputIndex:     0,
		ConfirmationPkScript: realScript,
	}}

	// The attacker's fully-self-consistent tree is rooted at THEIR key's
	// output (so its own key-to-output binding would hold), but that is not
	// the authenticated commitment output.
	attackerOperator := bindingKey(t)
	attackerOwner := bindingKey(t)
	attackerScript := taprootScript(
		t, sweepRoot, attackerOperator, attackerOwner,
	)
	tr := oneLeafTree(
		t, commitment, sweepRoot, attackerOperator, attackerOwner,
		&wire.TxOut{Value: 1000, PkScript: attackerScript},
		[]byte("input_vtxo_script"),
	)

	err := VerifyOORAncestryLineage(
		[]Ancestry{{TreePath: tr, CommitmentTxID: commitment}},
		evidence, 0, nil,
	)
	require.Error(t, err)
	require.Contains(
		t, err.Error(), "does not match the authenticated commitment",
	)
}

// TestOORLineageRejectsMissingEvidence proves fail-closed when a watched
// commitment has no authenticated evidence.
func TestOORLineageRejectsMissingEvidence(t *testing.T) {
	t.Parallel()

	sweepRoot := make([]byte, 32)
	var commitment, other chainhash.Hash
	commitment[0] = 0xab
	other[0] = 0xcd

	tr := oneLeafTree(
		t, commitment, sweepRoot, bindingKey(t), bindingKey(t),
		&wire.TxOut{Value: 1000, PkScript: []byte{0x51}},
		[]byte("x"),
	)
	evidence := []batchcanon.BatchEvidence{{BatchTxID: other}}

	err := VerifyOORAncestryLineage(
		[]Ancestry{{TreePath: tr, CommitmentTxID: commitment}},
		evidence, 0, nil,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no authenticated evidence")
}

// TestBindingRejectsEmptyScript and no-tree guard the input contract.
func TestBindingRejectsEmptyScript(t *testing.T) {
	t.Parallel()

	require.Error(t, VerifyReceivedVTXOBinding(nil, nil))

	var commitment chainhash.Hash
	require.Error(t, VerifyReceivedVTXOBinding(
		[]Ancestry{{TreePath: nil, CommitmentTxID: commitment}},
		[]byte("s"),
	))
}

// TestBindCoinInputsToLeaves covers the single-hop OOR terminator's matching
// logic directly (the surrounding tree verification needs signed fixtures,
// exercised end-to-end by the OOR receive itests).
func TestBindCoinInputsToLeaves(t *testing.T) {
	t.Parallel()

	var a, b chainhash.Hash
	a[0] = 0xa1
	b[0] = 0xb2
	leaves := map[chainhash.Hash]struct{}{a: {}, b: {}}

	op := func(h chainhash.Hash, i uint32) wire.OutPoint {
		return wire.OutPoint{Hash: h, Index: i}
	}

	// Single-hop: every coin input produced by a leaf -> accept.
	require.NoError(t, bindCoinInputsToLeaves(
		leaves, []wire.OutPoint{op(a, 0), op(b, 1)},
	))

	// Single-hop: a coin input NOT produced by any leaf (decoy lineage) ->
	// reject. This is the fail-open the terminator closes.
	var decoy chainhash.Hash
	decoy[0] = 0xcc
	err := bindCoinInputsToLeaves(
		leaves, []wire.OutPoint{op(a, 0), op(decoy, 0)},
	)
	require.ErrorContains(t, err, "not produced by any authenticated")

	// No coin inputs -> fail closed (an OOR receive always has checkpoint
	// inputs; their absence means the coin's lineage cannot be bound).
	require.Error(t, bindCoinInputsToLeaves(leaves, nil))
}
