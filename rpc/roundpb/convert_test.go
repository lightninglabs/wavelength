package roundpb

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// ---------------------------------------------------------------------------
// Rapid generators
// ---------------------------------------------------------------------------

// genHash generates a random 32-byte chainhash.Hash.
func genHash(t *rapid.T) chainhash.Hash {
	var h chainhash.Hash
	b := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(t, "hash")
	copy(h[:], b)

	return h
}

// genOutpoint generates a random wire.OutPoint.
func genOutpoint(t *rapid.T) wire.OutPoint {
	return wire.OutPoint{
		Hash:  genHash(t),
		Index: rapid.Uint32().Draw(t, "index"),
	}
}

// genTxOut generates a random wire.TxOut with a reasonable script and value.
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

// genPubKey generates a random secp256k1 public key.
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

// genSignature generates a valid random schnorr signature.
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

// genNode generates a random tree.Node with bounded depth to prevent
// infinite recursion. FinalKey is deliberately omitted since the proto
// representation does not carry it (see TreeFromProto doc comment).
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
// Property tests
// ---------------------------------------------------------------------------

// TestOutpointProtoRoundTrip verifies that any wire.OutPoint survives a
// round-trip through OutpointToProto → OutpointFromProto.
func TestOutpointProtoRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		op := genOutpoint(rt)
		pb := OutpointToProto(op)

		got, err := OutpointFromProto(pb)
		require.NoError(t, err)
		require.Equal(t, op, got)
	})
}

// TestOutpointsProtoRoundTrip verifies slice round-trip through
// OutpointsToProto → OutpointsFromProto.
func TestOutpointsProtoRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(0, 10).Draw(rt, "n")
		ops := make([]wire.OutPoint, n)
		for i := range ops {
			ops[i] = genOutpoint(rt)
		}

		pbs := OutpointsToProto(ops)
		got, err := OutpointsFromProto(pbs)
		require.NoError(t, err)
		require.Equal(t, ops, got)
	})
}

// TestOutpointMapKeyRoundTrip verifies that any wire.OutPoint survives
// a round-trip through OutpointToMapKey → OutpointFromMapKey. This
// exercises the byte-reversed hex hash format used by wire.OutPoint.String().
func TestOutpointMapKeyRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		op := genOutpoint(rt)
		key := OutpointToMapKey(op)

		got, err := OutpointFromMapKey(key)
		require.NoError(t, err)
		require.Equal(t, op, got)
	})
}

// TestTxOutProtoRoundTrip verifies that any wire.TxOut survives a
// round-trip through TxOutToProto → TxOutFromProto.
func TestTxOutProtoRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		out := genTxOut(rt)
		pb := TxOutToProto(out)
		got, err := TxOutFromProto(pb)
		require.NoError(t, err)

		require.Equal(t, out.Value, got.Value)
		require.Equal(t, out.PkScript, got.PkScript)
	})
}

// TestTxOutProtoNil verifies nil handling in TxOut conversion.
func TestTxOutProtoNil(t *testing.T) {
	t.Parallel()

	require.Nil(t, TxOutToProto(nil))
	nilOut, nilErr := TxOutFromProto(nil)
	require.NoError(t, nilErr)
	require.Nil(t, nilOut)
}

// TestSchnorrSigRoundTrip verifies that any schnorr signature survives
// a round-trip through SchnorrSigToBytes → SchnorrSigFromBytes.
func TestSchnorrSigRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		sig := genSignature(rt)
		b := SchnorrSigToBytes(sig)
		got, err := SchnorrSigFromBytes(b)

		require.NoError(t, err)
		require.Equal(t, sig.Serialize(), got.Serialize())
	})
}

// TestSchnorrSigNil verifies nil handling in schnorr sig conversion.
func TestSchnorrSigNil(t *testing.T) {
	t.Parallel()

	require.Nil(t, SchnorrSigToBytes(nil))

	got, err := SchnorrSigFromBytes(nil)
	require.NoError(t, err)
	require.Nil(t, got)
}

// TestTxIDHexRoundTrip verifies that any tree.TxID survives a
// round-trip through TxIDToHex → TxIDFromHex.
func TestTxIDHexRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		var id tree.TxID
		b := rapid.SliceOfN(
			rapid.Byte(), 32, 32,
		).Draw(rt, "txid")
		copy(id[:], b)

		hex := TxIDToHex(id)
		got, err := TxIDFromHex(hex)

		require.NoError(t, err)
		require.Equal(t, id, got)
	})
}

// TestTreeProtoRoundTrip verifies that any tree.Tree survives a
// round-trip through TreeToProto → TreeFromProto. FinalKey is
// excluded from comparison since the proto does not carry it.
func TestTreeProtoRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		original := genTree(rt)
		pb, err := TreeToProto(original)
		require.NoError(t, err)

		got, err := TreeFromProto(pb)
		require.NoError(t, err)

		// Compare structurally. FinalKey is a derived field
		// computed from CoSigners + SweepTapscriptRoot, not
		// carried in the proto. Clear it on both sides so we
		// only compare serialized fields.
		clearFinalKeys(original.Root)
		clearFinalKeys(got.Root)
		assertTreeEqual(t, original, got)
	})
}

// TestTreeProtoNil verifies nil handling in tree conversion.
func TestTreeProtoNil(t *testing.T) {
	t.Parallel()

	pb, err := TreeToProto(nil)
	require.NoError(t, err)
	require.Nil(t, pb)

	got, err := TreeFromProto(nil)
	require.NoError(t, err)
	require.Nil(t, got)
}

// TestMsgTxBytesRoundTrip verifies that any wire.MsgTx survives a
// round-trip through MsgTxToBytes → MsgTxFromBytes.
func TestMsgTxBytesRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		tx := wire.NewMsgTx(2)

		// Add 1-3 inputs.
		numInputs := rapid.IntRange(1, 3).Draw(
			rt, "num_inputs",
		)
		for range numInputs {
			tx.AddTxIn(&wire.TxIn{
				PreviousOutPoint: genOutpoint(rt),
				Sequence: rapid.Uint32().Draw(
					rt, "sequence",
				),
			})
		}

		// Add 1-3 outputs.
		numOutputs := rapid.IntRange(1, 3).Draw(
			rt, "num_outputs",
		)
		for range numOutputs {
			tx.AddTxOut(genTxOut(rt))
		}

		b, err := MsgTxToBytes(tx)
		require.NoError(t, err)

		got, err := MsgTxFromBytes(b)
		require.NoError(t, err)
		require.Equal(t, tx.TxHash(), got.TxHash())
		require.Equal(t, len(tx.TxIn), len(got.TxIn))
		require.Equal(t, len(tx.TxOut), len(got.TxOut))
	})
}

// TestMsgTxBytesNil verifies nil handling in MsgTx conversion.
func TestMsgTxBytesNil(t *testing.T) {
	t.Parallel()

	b, err := MsgTxToBytes(nil)
	require.NoError(t, err)
	require.Nil(t, b)

	got, err := MsgTxFromBytes(nil)
	require.NoError(t, err)
	require.Nil(t, got)
}

// TestPSBTBytesRoundTrip verifies that a valid PSBT survives a
// round-trip through PSBTToBytes → PSBTFromBytes.
func TestPSBTBytesRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		tx := wire.NewMsgTx(2)
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: genOutpoint(rt),
		})
		tx.AddTxOut(genTxOut(rt))

		pkt, err := psbt.NewFromUnsignedTx(tx)
		require.NoError(t, err)

		b, err := PSBTToBytes(pkt)
		require.NoError(t, err)

		got, err := PSBTFromBytes(b)
		require.NoError(t, err)
		require.Equal(
			t, pkt.UnsignedTx.TxHash(), got.UnsignedTx.TxHash(),
		)
	})
}

// TestPSBTBytesNil verifies nil handling in PSBT conversion.
func TestPSBTBytesNil(t *testing.T) {
	t.Parallel()

	b, err := PSBTToBytes(nil)
	require.NoError(t, err)
	require.Nil(t, b)

	got, err := PSBTFromBytes(nil)
	require.NoError(t, err)
	require.Nil(t, got)
}

// TestOutpointFromMapKeyInvalid verifies error handling for malformed
// map keys.
func TestOutpointFromMapKeyInvalid(t *testing.T) {
	t.Parallel()

	cases := []string{
		"",
		"not-an-outpoint",
		"abc:def",
		"abc:",
		// Too long (65 hex chars = 32.5 bytes).
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" +
			"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:0",
	}

	for _, tc := range cases {
		_, err := OutpointFromMapKey(tc)
		require.Error(t, err, "expected error for key %q", tc)
	}
}

// TestTxIDFromHexInvalid verifies error handling for malformed hex.
func TestTxIDFromHexInvalid(t *testing.T) {
	t.Parallel()

	cases := []string{
		"",
		"not-hex",
		// Valid hex but wrong length (31 bytes).
		"abcdef0123456789abcdef01234567890" +
			"abcdef0123456789abcdef1234567",
	}

	for _, tc := range cases {
		_, err := TxIDFromHex(tc)
		require.Error(t, err, "expected error for hex %q", tc)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// clearFinalKeys recursively sets FinalKey to nil on all nodes in the
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
