package arkrpc

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// ---------------------------------------------------------------------------
// Rapid generators
// ---------------------------------------------------------------------------

func genHash(t *rapid.T) chainhash.Hash {
	var h chainhash.Hash
	b := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(t, "hash")
	copy(h[:], b)

	return h
}

func genOutpoint(t *rapid.T) wire.OutPoint {
	return wire.OutPoint{
		Hash:  genHash(t),
		Index: rapid.Uint32().Draw(t, "index"),
	}
}

func genTxOut(t *rapid.T) *wire.TxOut {
	scriptLen := rapid.IntRange(1, 100).Draw(t, "script_len")
	script := rapid.SliceOfN(
		rapid.Byte(), scriptLen, scriptLen,
	).Draw(t, "pk_script")

	maxSats := int64(21_000_000_00000000)
	value := rapid.Int64Range(0, maxSats).Draw(t, "value")

	return &wire.TxOut{
		Value:    value,
		PkScript: script,
	}
}

func genPubKey(t *rapid.T) *btcec.PublicKey {
	privBytes := rapid.SliceOfN(
		rapid.Byte(), 32, 32,
	).Draw(t, "priv_key")
	privKey, _ := btcec.PrivKeyFromBytes(privBytes)
	if privKey == nil {
		privKey, _ = btcec.NewPrivateKey()
	}

	return privKey.PubKey()
}

func genSignature(t *rapid.T) *schnorr.Signature {
	privBytes := rapid.SliceOfN(
		rapid.Byte(), 32, 32,
	).Draw(t, "sig_pk")
	privKey, _ := btcec.PrivKeyFromBytes(privBytes)
	if privKey == nil {
		privKey, _ = btcec.NewPrivateKey()
	}

	msgBytes := rapid.SliceOfN(
		rapid.Byte(), 32, 32,
	).Draw(t, "sig_msg")
	var msg [32]byte
	copy(msg[:], msgBytes)

	sig, err := schnorr.Sign(privKey, msg[:])
	if err != nil {
		privKey, _ = btcec.NewPrivateKey()
		sig, _ = schnorr.Sign(privKey, msg[:])
	}

	return sig
}

// genNode generates a random tree.Node with bounded depth.
func genNode(t *rapid.T, depth int) *tree.Node {
	maxChildren := 0
	if depth > 0 {
		maxChildren = rapid.IntRange(0, 2).Draw(
			t, "max_children",
		)
	}

	numOutputs := rapid.IntRange(1, 4).Draw(t, "num_outputs")
	outputs := make([]*wire.TxOut, numOutputs)
	for i := range outputs {
		outputs[i] = genTxOut(t)
	}

	numCoSigners := rapid.IntRange(0, 3).Draw(t, "num_cosigners")
	coSigners := make([]*btcec.PublicKey, numCoSigners)
	for i := range coSigners {
		coSigners[i] = genPubKey(t)
	}

	var sig *schnorr.Signature
	if rapid.Bool().Draw(t, "has_sig") {
		sig = genSignature(t)
	}

	maxSats := int64(21_000_000_00000000)
	amount := btcutil.Amount(
		rapid.Int64Range(0, maxSats).Draw(t, "amount"),
	)

	children := make(map[uint32]*tree.Node)
	for i := 0; i < maxChildren; i++ {
		idx := rapid.Uint32Range(
			0, uint32(numOutputs-1),
		).Draw(t, "child_idx")
		children[idx] = genNode(t, depth-1)
	}

	return &tree.Node{
		Input:     genOutpoint(t),
		Outputs:   outputs,
		CoSigners: coSigners,
		Children:  children,
		Amount:    amount,
		Signature: sig,
	}
}

// genTree generates a random tree.Tree with a root and up to 3 levels
// of children.
func genTree(t *rapid.T) *tree.Tree {
	var batchOutput *wire.TxOut
	if rapid.Bool().Draw(t, "has_batch_output") {
		batchOutput = genTxOut(t)
	}

	sweepLen := rapid.IntRange(0, 32).Draw(t, "sweep_len")
	var sweepRoot []byte
	if sweepLen > 0 {
		sweepRoot = rapid.SliceOfN(
			rapid.Byte(), sweepLen, sweepLen,
		).Draw(t, "sweep_root")
	}

	return &tree.Tree{
		Root:               genNode(t, 3),
		BatchOutpoint:      genOutpoint(t),
		BatchOutput:        batchOutput,
		SweepTapscriptRoot: sweepRoot,
	}
}

// ---------------------------------------------------------------------------
// Assertion helpers
// ---------------------------------------------------------------------------

// clearFinalKeys recursively clears the FinalKey field on all nodes in a
// tree, since the proto representation does not carry it.
func clearFinalKeys(n *tree.Node) {
	if n == nil {
		return
	}

	n.FinalKey = nil

	for _, child := range n.Children {
		clearFinalKeys(child)
	}
}

// assertTreeEqual compares two trees structurally, checking all fields
// that survive proto round-trip.
func assertTreeEqual(t testing.TB, a, b *tree.Tree) {
	t.Helper()

	require.Equal(t, a.BatchOutpoint, b.BatchOutpoint)
	require.Equal(t, a.SweepTapscriptRoot, b.SweepTapscriptRoot)

	if a.BatchOutput == nil {
		require.Nil(t, b.BatchOutput)
	} else {
		require.NotNil(t, b.BatchOutput)
		require.Equal(t, a.BatchOutput.Value, b.BatchOutput.Value)
		require.Equal(
			t, a.BatchOutput.PkScript, b.BatchOutput.PkScript,
		)
	}

	assertNodeEqual(t, a.Root, b.Root)
}

// assertNodeEqual recursively compares two tree nodes.
func assertNodeEqual(t testing.TB, a, b *tree.Node) {
	t.Helper()

	require.Equal(t, a.Input, b.Input)
	require.Equal(t, a.Amount, b.Amount)
	require.Equal(t, len(a.Outputs), len(b.Outputs))

	for i := range a.Outputs {
		require.Equal(t, a.Outputs[i].Value, b.Outputs[i].Value)
		require.Equal(
			t, a.Outputs[i].PkScript, b.Outputs[i].PkScript,
		)
	}

	require.Equal(t, len(a.CoSigners), len(b.CoSigners))
	for i := range a.CoSigners {
		require.Equal(
			t, a.CoSigners[i].SerializeCompressed(),
			b.CoSigners[i].SerializeCompressed(),
		)
	}

	if a.Signature == nil {
		require.Nil(t, b.Signature)
	} else {
		require.NotNil(t, b.Signature)
		require.Equal(
			t, a.Signature.Serialize(), b.Signature.Serialize(),
		)
	}

	require.Equal(t, len(a.Children), len(b.Children))
	for idx, childA := range a.Children {
		childB, ok := b.Children[idx]
		require.True(t, ok, "missing child at index %d", idx)
		assertNodeEqual(t, childA, childB)
	}
}

// ---------------------------------------------------------------------------
// Property tests
// ---------------------------------------------------------------------------

// TestTreePathProtoRoundTrip verifies that any tree.Tree survives a
// round-trip through TreePathFromTree → TreePathToTree. FinalKey is
// excluded from comparison since the proto does not carry it.
func TestTreePathProtoRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		original := genTree(rt)
		pb, err := TreePathFromTree(original)
		require.NoError(t, err)

		got, err := TreePathToTree(pb)
		require.NoError(t, err)

		// FinalKey is derived from CoSigners + SweepTapscriptRoot,
		// not carried in the proto. Clear on both sides.
		clearFinalKeys(original.Root)
		clearFinalKeys(got.Root)
		assertTreeEqual(t, original, got)
	})
}

// TestTreePathProtoNil verifies nil handling in tree path conversion.
func TestTreePathProtoNil(t *testing.T) {
	t.Parallel()

	pb, err := TreePathFromTree(nil)
	require.NoError(t, err)
	require.Nil(t, pb)

	got, err := TreePathToTree(nil)
	require.NoError(t, err)
	require.Nil(t, got)
}

// TestTreePathOutpointRoundTrip verifies that outpoint conversion
// between wire.OutPoint and arkrpc.OutPoint is lossless.
func TestTreePathOutpointRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		op := genOutpoint(rt)
		pb := outpointToProto(op)

		got, err := outpointFromProto(pb)
		require.NoError(t, err)
		require.Equal(t, op, got)
	})
}

// TestTreePathTxOutRoundTrip verifies that TxOut conversion is lossless.
func TestTreePathTxOutRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		out := genTxOut(rt)
		pb := txOutToProto(out)
		got, err := txOutFromProto(pb)
		require.NoError(t, err)

		require.Equal(t, out.Value, got.Value)
		require.Equal(t, out.PkScript, got.PkScript)
	})
}
