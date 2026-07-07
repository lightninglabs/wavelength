package db

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// randHash generates a random chainhash.Hash.
func randHash(t *rapid.T) chainhash.Hash {
	var hash chainhash.Hash
	data := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(t, "hash_bytes")
	copy(hash[:], data)

	return hash
}

// randOutpoint generates a random wire.OutPoint.
func randOutpoint(t *rapid.T) wire.OutPoint {
	return wire.OutPoint{
		Hash:  randHash(t),
		Index: rapid.Uint32().Draw(t, "outpoint_index"),
	}
}

// randTxOut generates a random wire.TxOut.
func randTxOut(t *rapid.T) *wire.TxOut {
	// Generate a reasonable script (1-100 bytes).
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

// randPubKey generates a random btcec public key.
func randPubKey(t *rapid.T) *btcec.PublicKey {
	// Generate 32 bytes for the private key.
	privBytes := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(t, "priv_key")

	// Ensure it's a valid private key.
	privKey, _ := btcec.PrivKeyFromBytes(privBytes)
	if privKey == nil {
		// Fall back to a known valid key if random bytes don't work.
		privKey, _ = btcec.NewPrivateKey()
	}

	return privKey.PubKey()
}

// randSignature generates a random schnorr signature.
func randSignature(t *rapid.T) *schnorr.Signature {
	// Generate 32 bytes for the private key.
	privBytes := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(t, "sig_pk")
	privKey, _ := btcec.PrivKeyFromBytes(privBytes)
	if privKey == nil {
		privKey, _ = btcec.NewPrivateKey()
	}

	// Sign a random message.
	msgBytes := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(t, "sig_msg")
	var msg [32]byte
	copy(msg[:], msgBytes)

	sig, err := schnorr.Sign(privKey, msg[:])
	if err != nil {
		// Fall back to a valid signature if generation fails.
		privKey, _ = btcec.NewPrivateKey()
		sig, _ = schnorr.Sign(privKey, msg[:])
	}

	return sig
}

// randNode generates a random tree.Node with optional children.
func randNode(t *rapid.T, depth int) *tree.Node {
	// Limit depth to prevent infinite recursion.
	maxChildren := 0
	if depth > 0 {
		maxChildren = rapid.IntRange(0, 3).Draw(t, "max_children")
	}

	// Generate outputs (0-5).
	numOutputs := rapid.IntRange(0, 5).Draw(t, "num_outputs")
	outputs := make([]*wire.TxOut, numOutputs)
	for i := 0; i < numOutputs; i++ {
		outputs[i] = randTxOut(t)
	}

	// Generate co-signers (0-3).
	numCoSigners := rapid.IntRange(0, 3).Draw(t, "num_cosigners")
	coSigners := make([]*btcec.PublicKey, numCoSigners)
	for i := 0; i < numCoSigners; i++ {
		coSigners[i] = randPubKey(t)
	}

	// Optionally generate signature.
	var sig *schnorr.Signature
	if rapid.Bool().Draw(t, "has_signature") {
		sig = randSignature(t)
	}

	// Optionally generate final key.
	var finalKey *btcec.PublicKey
	if rapid.Bool().Draw(t, "has_final_key") {
		finalKey = randPubKey(t)
	}

	// Generate children recursively.
	children := make(map[uint32]*tree.Node)
	for i := 0; i < maxChildren; i++ {
		childIdx := rapid.Uint32Range(
			0, uint32(numOutputs),
		).Draw(t, "child_idx")
		children[childIdx] = randNode(t, depth-1)
	}

	return &tree.Node{
		Input:     randOutpoint(t),
		Outputs:   outputs,
		CoSigners: coSigners,
		Signature: sig,
		FinalKey:  finalKey,
		Children:  children,
	}
}

// randTree generates a random tree.Tree.
func randTree(t *rapid.T) *tree.Tree {
	// Optionally generate batch output.
	var batchOutput *wire.TxOut
	if rapid.Bool().Draw(t, "has_batch_output") {
		batchOutput = randTxOut(t)
	}

	// Generate sweep root (0-32 bytes).
	sweepRootLen := rapid.IntRange(0, 32).Draw(t, "sweep_root_len")
	sweepRoot := rapid.SliceOfN(
		rapid.Byte(), sweepRootLen, sweepRootLen,
	).Draw(t, "sweep_root")

	return &tree.Tree{
		BatchOutpoint:      randOutpoint(t),
		BatchOutput:        batchOutput,
		SweepTapscriptRoot: sweepRoot,
		Root:               randNode(t, 3), // Max depth of 3
	}
}

// TestTreeCodecRoundTrip_Property tests that any valid tree can be serialized
// and deserialized without data loss using property-based testing.
func TestTreeCodecRoundTrip_Property(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		original := randTree(t)

		// Serialize.
		serialized, err := SerializeTree(original)
		if err != nil {
			t.Fatalf("failed to serialize: %v", err)
		}

		// Deserialize.
		deserialized, err := DeserializeTree(serialized)
		if err != nil {
			t.Fatalf("failed to deserialize: %v", err)
		}

		// Verify equality.
		assertTreesEqual(t, original, deserialized)
	})
}

// assertTreesEqual checks that two trees are equal.
func assertTreesEqual(t *rapid.T, a, b *tree.Tree) {
	if a.BatchOutpoint != b.BatchOutpoint {
		t.Fatalf("batch outpoint mismatch: %v != %v", a.BatchOutpoint,
			b.BatchOutpoint)
	}

	assertTxOutEqual(t, a.BatchOutput, b.BatchOutput)

	if string(a.SweepTapscriptRoot) != string(b.SweepTapscriptRoot) {
		t.Fatalf("sweep root mismatch")
	}

	assertNodesEqual(t, a.Root, b.Root)
}

// assertTxOutEqual checks that two TxOuts are equal.
func assertTxOutEqual(t *rapid.T, a, b *wire.TxOut) {
	if a == nil && b == nil {
		return
	}

	if (a == nil) != (b == nil) {
		t.Fatalf("txout nil mismatch: %v vs %v", a, b)
	}

	if a.Value != b.Value {
		t.Fatalf("txout value mismatch: %d != %d", a.Value, b.Value)
	}

	if string(a.PkScript) != string(b.PkScript) {
		t.Fatalf("txout script mismatch")
	}
}

// assertNodesEqual checks that two nodes are equal.
func assertNodesEqual(t *rapid.T, a, b *tree.Node) {
	if a == nil && b == nil {
		return
	}

	if (a == nil) != (b == nil) {
		t.Fatalf("node nil mismatch")
	}

	if a.Input != b.Input {
		t.Fatalf("input mismatch: %v != %v", a.Input, b.Input)
	}

	if len(a.Outputs) != len(b.Outputs) {
		t.Fatalf("outputs length mismatch: %d != %d", len(a.Outputs),
			len(b.Outputs))
	}

	for i := range a.Outputs {
		assertTxOutEqual(t, a.Outputs[i], b.Outputs[i])
	}

	if len(a.CoSigners) != len(b.CoSigners) {
		t.Fatalf("cosigners length mismatch: %d != %d",
			len(a.CoSigners), len(b.CoSigners))
	}

	for i := range a.CoSigners {
		if !a.CoSigners[i].IsEqual(b.CoSigners[i]) {
			t.Fatalf("cosigner %d mismatch", i)
		}
	}

	assertSignatureEqual(t, a.Signature, b.Signature)
	assertPubKeyEqual(t, a.FinalKey, b.FinalKey)

	if len(a.Children) != len(b.Children) {
		t.Fatalf("children length mismatch: %d != %d", len(a.Children),
			len(b.Children))
	}

	for idx, aChild := range a.Children {
		bChild, ok := b.Children[idx]
		if !ok {
			t.Fatalf("child %d missing in b", idx)
		}

		assertNodesEqual(t, aChild, bChild)
	}
}

// assertSignatureEqual checks that two signatures are equal.
func assertSignatureEqual(t *rapid.T, a, b *schnorr.Signature) {
	if a == nil && b == nil {
		return
	}

	if (a == nil) != (b == nil) {
		t.Fatalf("signature nil mismatch: a=%v, b=%v", a, b)
	}

	if string(a.Serialize()) != string(b.Serialize()) {
		t.Fatalf("signature mismatch")
	}
}

// assertPubKeyEqual checks that two public keys are equal.
func assertPubKeyEqual(t *rapid.T, a, b *btcec.PublicKey) {
	if a == nil && b == nil {
		return
	}

	if (a == nil) != (b == nil) {
		t.Fatalf("pubkey nil mismatch: a=%v, b=%v", a, b)
	}

	if !a.IsEqual(b) {
		t.Fatalf("pubkey mismatch")
	}
}

// TestTreeCodecNilTree tests that nil tree returns an error.
func TestTreeCodecNilTree(t *testing.T) {
	t.Parallel()

	_, err := SerializeTree(nil)
	require.Error(t, err)
}

// TestTreeCodecEmptyData tests that empty data returns an error.
func TestTreeCodecEmptyData(t *testing.T) {
	t.Parallel()

	_, err := DeserializeTree(nil)
	require.Error(t, err)

	_, err = DeserializeTree([]byte{})
	require.Error(t, err)
}

// TestTreeCodecDeterministic tests that serialization is deterministic
// (same input always produces same output).
func TestTreeCodecDeterministic(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		original := randTree(t)

		// Serialize twice.
		serialized1, err := SerializeTree(original)
		if err != nil {
			t.Fatalf("first serialize failed: %v", err)
		}

		serialized2, err := SerializeTree(original)
		if err != nil {
			t.Fatalf("second serialize failed: %v", err)
		}

		// Both serializations should be identical.
		if string(serialized1) != string(serialized2) {
			t.Fatalf("serialization is not deterministic")
		}
	})
}

// TestTreeCodecMinimalTree tests a minimal tree with just required fields.
func TestTreeCodecMinimalTree(t *testing.T) {
	t.Parallel()

	minimalTree := &tree.Tree{
		BatchOutpoint: wire.OutPoint{
			Hash:  chainhash.Hash{},
			Index: 0,
		},
		Root: &tree.Node{
			Input: wire.OutPoint{
				Hash:  chainhash.Hash{},
				Index: 0,
			},
			Outputs:   []*wire.TxOut{},
			CoSigners: []*btcec.PublicKey{},
			Children:  make(map[uint32]*tree.Node),
		},
	}

	serialized, err := SerializeTree(minimalTree)
	require.NoError(t, err)

	deserialized, err := DeserializeTree(serialized)
	require.NoError(t, err)

	require.NotNil(t, deserialized)
	require.NotNil(t, deserialized.Root)
}

// makeLinearChain builds a tree that is a single chain of inner nodes
// reaching the requested depth. depth=1 produces a single-node tree.
// Used to exercise the recursion-depth bound.
func makeLinearChain(depth int) *tree.Tree {
	if depth < 1 {
		depth = 1
	}

	leaf := &tree.Node{
		Input: wire.OutPoint{
			Hash:  chainhash.Hash{},
			Index: 0,
		},
		Outputs:   []*wire.TxOut{},
		CoSigners: []*btcec.PublicKey{},
		Children:  make(map[uint32]*tree.Node),
	}

	cur := leaf
	for i := 1; i < depth; i++ {
		parent := &tree.Node{
			Input: wire.OutPoint{
				Hash:  chainhash.Hash{},
				Index: uint32(i),
			},
			Outputs:   []*wire.TxOut{},
			CoSigners: []*btcec.PublicKey{},
			Children: map[uint32]*tree.Node{
				0: cur,
			},
		}
		cur = parent
	}

	return &tree.Tree{
		BatchOutpoint: wire.OutPoint{
			Hash:  chainhash.Hash{},
			Index: 0,
		},
		Root: cur,
	}
}

// TestTreeCodecDepthBoundAcceptsAtCap verifies that a chain exactly at
// MaxTreeDeserializeDepth round-trips successfully. This guards
// against the depth bound being so tight that it rejects legitimate
// (worst-case-but-valid) trees.
func TestTreeCodecDepthBoundAcceptsAtCap(t *testing.T) {
	t.Parallel()

	tr := makeLinearChain(MaxTreeDeserializeDepth)

	serialized, err := SerializeTree(tr)
	require.NoError(t, err)

	got, err := DeserializeTree(serialized)
	require.NoError(t, err)
	require.NotNil(t, got)
}

// TestTreeCodecDepthBoundRejectsOverCap verifies that a chain one
// level over MaxTreeDeserializeDepth is rejected at decode time
// rather than blowing the goroutine stack via runaway recursion.
func TestTreeCodecDepthBoundRejectsOverCap(t *testing.T) {
	t.Parallel()

	tr := makeLinearChain(MaxTreeDeserializeDepth + 1)

	serialized, err := SerializeTree(tr)
	require.NoError(t, err)

	_, err = DeserializeTree(serialized)
	require.Error(t, err)
	require.Contains(t, err.Error(), "tree depth exceeds max")
}

// TestTreeCodecRejectsHugeNumChildren crafts a children blob whose
// numChildren varint claims uint64-max children. The decoder must
// reject this before reaching the make() call so a corrupted durable
// blob cannot OOM the actor on replay.
func TestTreeCodecRejectsHugeNumChildren(t *testing.T) {
	t.Parallel()

	// Hand-roll a deserializeChildren payload: a single varint
	// holding a huge count and no follow-up data.
	payload := []byte{
		// 0xFF prefix tells tlv.ReadVarInt that an 8-byte count
		// follows in big-endian form.
		0xff,
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	}

	_, err := deserializeChildren(payload, 2)
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds max")
}
