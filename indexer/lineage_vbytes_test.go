package indexer_test

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo/indexer"
	"github.com/stretchr/testify/require"
)

// makeBareTx returns a syntactically valid wire.MsgTx with the given
// number of inputs and outputs. Each input prevout is derived from a
// distinct label so different invocations produce different txids.
// Witness data is omitted, so SerializeSizeStripped == SerializeSize
// (witness scale factor reduces to base*4 / 4 = base).
func makeBareTx(label string, numIn, numOut int) *wire.MsgTx {
	tx := wire.NewMsgTx(2)
	for i := 0; i < numIn; i++ {
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: wire.OutPoint{
				Hash: chainhash.HashH([]byte(
					label,
				)),
				Index: uint32(i),
			},
		})
	}
	for i := 0; i < numOut; i++ {
		tx.AddTxOut(&wire.TxOut{
			Value:    int64(1_000 + i),
			PkScript: []byte{0x51},
		})
	}

	return tx
}

// makeWitnessTx returns a wire.MsgTx with a single input carrying a
// non-empty witness stack. This forces SerializeSizeStripped <
// SerializeSize so the witness-discounted vbytes formula
// `(base*3 + total + 3) / 4` produces a value strictly less than the
// raw serialized size.
func makeWitnessTx(label string) *wire.MsgTx {
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.HashH([]byte(label)),
			Index: 0,
		},
		Witness: wire.TxWitness{
			make([]byte, 64),
			make([]byte, 32),
		},
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    1_000,
		PkScript: []byte{0x51},
	})

	return tx
}

// referenceTxVBytes computes the canonical witness-discounted virtual
// size of a wire.MsgTx using btcd's blockchain.GetTransactionWeight as
// the source of truth: vbytes = ceil(weight / 4) where
// weight = base*3 + total. Used as a regression check for txVBytes —
// any drift between indexer.TxVBytes and this reference would
// indicate a formula bug.
func referenceTxVBytes(tx *wire.MsgTx) int {
	weight := blockchain.GetTransactionWeight(btcutil.NewTx(tx))

	return int((weight + 3) / 4)
}

// TestTxVBytesNilTx verifies the explicit nil-safety contract: the
// helper short-circuits to zero rather than panicking.
func TestTxVBytesNilTx(t *testing.T) {
	t.Parallel()
	require.Equal(t, 0, indexer.TxVBytes(nil))
}

// TestTxVBytesMatchesBlockchainReference verifies the formula matches
// btcd's blockchain.GetTransactionWeight for a representative range of
// tx shapes (no witness, with witness, multi-input, multi-output).
// This is the cross-validation users requested for the cap arithmetic.
func TestTxVBytesMatchesBlockchainReference(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		tx   *wire.MsgTx
	}{
		{
			name: "single in/out no witness",
			tx:   makeBareTx("single", 1, 1),
		},
		{
			name: "multi in/out no witness",
			tx:   makeBareTx("multi", 4, 3),
		},
		{
			name: "single in with witness",
			tx:   makeWitnessTx("witness-1"),
		},
		{
			name: "fan-out 8 outputs",
			tx:   makeBareTx("fanout", 1, 8),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(
				t, referenceTxVBytes(tc.tx),
				indexer.TxVBytes(tc.tx),
				"txVBytes must match blockchain reference "+
					"weight/4 ceiling",
			)
		})
	}
}

// TestTxVBytesWitnessDiscount verifies that a tx with witness data
// has a vbyte size strictly smaller than its raw SerializeSize. The
// witness-discount formula is the whole point of computing in vbytes
// rather than raw bytes: 4x cheaper data should be 4x cheaper to
// publish, and the cap arithmetic must reflect that.
func TestTxVBytesWitnessDiscount(t *testing.T) {
	t.Parallel()

	tx := makeWitnessTx("discount")
	full := tx.SerializeSize()
	vbytes := indexer.TxVBytes(tx)
	require.Less(t, vbytes, full,
		"witness tx vbytes must be strictly less than raw size")
}

// TestEstimateOORLineageVBytesNilStore verifies the explicit nil-store
// guard so callers cannot accidentally invoke the helper without
// configuration.
func TestEstimateOORLineageVBytesNilStore(t *testing.T) {
	t.Parallel()

	_, err := indexer.EstimateOORLineageVBytes(
		t.Context(), nil, nil, nil, nil,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "store must be provided")
}

// TestEstimateOORLineageVBytesEmptySubmit verifies the trivial
// boundary case: no parent inputs, no Ark, no checkpoints results in
// zero vbytes. This locks in that the helper does not require a real
// session even at the empty-input boundary.
func TestEstimateOORLineageVBytesEmptySubmit(t *testing.T) {
	t.Parallel()

	store := newMockLineageStore()
	got, err := indexer.EstimateOORLineageVBytes(
		t.Context(), store, nil, nil, nil,
	)
	require.NoError(t, err)
	require.Equal(t, uint32(0), got)
}

// TestEstimateOORLineageVBytesArkOnly verifies that a submit with an
// Ark tx and no checkpoints/inputs returns exactly the Ark's vbytes.
// This is the simplest non-empty case and isolates the new-submit
// counting branch.
func TestEstimateOORLineageVBytesArkOnly(t *testing.T) {
	t.Parallel()

	store := newMockLineageStore()
	arkTx := makeBareTx("ark-only", 2, 2)
	pkt, err := psbt.NewFromUnsignedTx(arkTx)
	require.NoError(t, err)

	got, err := indexer.EstimateOORLineageVBytes(
		t.Context(), store, nil, pkt, nil,
	)
	require.NoError(t, err)
	require.Equal(t,
		uint32(indexer.TxVBytes(arkTx)), got,
		"empty inputs + Ark only must equal Ark's vbytes")
}

// TestEstimateOORLineageVBytesArkAndCheckpoints verifies the standard
// new-submit accounting: every checkpoint and the Ark tx contribute
// once, and total = sum of every distinct tx's vbytes.
func TestEstimateOORLineageVBytesArkAndCheckpoints(t *testing.T) {
	t.Parallel()

	store := newMockLineageStore()
	arkTx := makeBareTx("ark", 2, 1)
	cp0 := makeBareTx("cp-0", 1, 1)
	cp1 := makeBareTx("cp-1", 1, 1)

	arkPkt, err := psbt.NewFromUnsignedTx(arkTx)
	require.NoError(t, err)
	cp0Pkt, err := psbt.NewFromUnsignedTx(cp0)
	require.NoError(t, err)
	cp1Pkt, err := psbt.NewFromUnsignedTx(cp1)
	require.NoError(t, err)

	want := uint32(indexer.TxVBytes(arkTx) +
		indexer.TxVBytes(cp0) + indexer.TxVBytes(cp1))

	got, err := indexer.EstimateOORLineageVBytes(
		t.Context(), store, nil, arkPkt,
		[]*psbt.Packet{cp0Pkt, cp1Pkt},
	)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// TestEstimateOORLineageVBytesDeDupCheckpoints verifies that the
// txid de-dup map collapses repeated checkpoints to a single
// contribution. A misbehaving caller listing the same checkpoint
// twice cannot inflate the cap arithmetic.
func TestEstimateOORLineageVBytesDeDupCheckpoints(t *testing.T) {
	t.Parallel()

	store := newMockLineageStore()
	arkTx := makeBareTx("ark", 1, 1)
	cp := makeBareTx("dup-cp", 1, 1)

	arkPkt, err := psbt.NewFromUnsignedTx(arkTx)
	require.NoError(t, err)
	cpPkt, err := psbt.NewFromUnsignedTx(cp)
	require.NoError(t, err)

	// Same checkpoint repeated three times.
	got, err := indexer.EstimateOORLineageVBytes(
		t.Context(), store, nil, arkPkt,
		[]*psbt.Packet{cpPkt, cpPkt, cpPkt},
	)
	require.NoError(t, err)
	want := uint32(indexer.TxVBytes(arkTx) + indexer.TxVBytes(cp))
	require.Equal(t, want, got,
		"repeated checkpoints must contribute exactly once")
}

// TestEstimateOORLineageVBytesDeDupArkAsCheckpoint verifies that an
// Ark tx whose txid happens to also appear in the checkpoint list is
// counted once. This guards against a future caller mistake where the
// Ark tx and a degenerate "self-checkpoint" share txids.
func TestEstimateOORLineageVBytesDeDupArkAsCheckpoint(t *testing.T) {
	t.Parallel()

	store := newMockLineageStore()
	tx := makeBareTx("shared", 1, 1)
	pkt, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)

	got, err := indexer.EstimateOORLineageVBytes(
		t.Context(), store, nil, pkt,
		[]*psbt.Packet{pkt},
	)
	require.NoError(t, err)
	require.Equal(t, uint32(indexer.TxVBytes(tx)), got,
		"shared-txid Ark and checkpoint must count once")
}

// TestEstimateOORLineageVBytesNilCheckpoints verifies that nil packets
// in the checkpoint slice are silently tolerated rather than panicking.
// Real callers should never pass nil, but defense-in-depth at the
// boundary keeps a misbehaving driver from crashing the validator.
func TestEstimateOORLineageVBytesNilCheckpoints(t *testing.T) {
	t.Parallel()

	store := newMockLineageStore()
	arkTx := makeBareTx("ark-nil-cp", 1, 1)
	arkPkt, err := psbt.NewFromUnsignedTx(arkTx)
	require.NoError(t, err)

	got, err := indexer.EstimateOORLineageVBytes(
		t.Context(), store, nil, arkPkt,
		[]*psbt.Packet{nil, nil},
	)
	require.NoError(t, err)
	require.Equal(t, uint32(indexer.TxVBytes(arkTx)), got)
}

// TestEstimateOORLineageVBytesNilArk verifies that a submit-time
// vbytes calc with no Ark tx (an unusual path but reachable through
// the API) returns just the checkpoints' contributions. Mirrors
// TestEstimateOORLineageVBytesArkOnly's symmetric case.
func TestEstimateOORLineageVBytesNilArk(t *testing.T) {
	t.Parallel()

	store := newMockLineageStore()
	cp0 := makeBareTx("only-cp", 1, 1)
	cp0Pkt, err := psbt.NewFromUnsignedTx(cp0)
	require.NoError(t, err)

	got, err := indexer.EstimateOORLineageVBytes(
		t.Context(), store, nil, nil,
		[]*psbt.Packet{cp0Pkt},
	)
	require.NoError(t, err)
	require.Equal(t, uint32(indexer.TxVBytes(cp0)), got)
}

// TestEstimateOORLineageVBytesRoundBackedLineageWalk verifies the
// end-to-end resolver path: when a parent is registered as a
// round-direct VTXO, EstimateOORLineageVBytes resolves the lineage,
// walks every tree node, and adds the new submit's bytes — without
// crashing or losing input contributions when tree nodes happen to
// be unsigned (degenerate fixtures skip cleanly per
// LineageVBytes's defense-in-depth contract). The returned vbytes
// is at least the new-submit contribution, so cap arithmetic is
// always >= raw submit bytes.
func TestEstimateOORLineageVBytesRoundBackedLineageWalk(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newMockLineageStore()

	testTree, leafOutpoints := buildTestTree(t, 4)
	require.True(t, len(leafOutpoints) >= 1)
	parentOutpoint := leafOutpoints[0]

	roundID := newTestRoundID(0xA1)
	commitTxID := chainhash.HashH([]byte("commitment-vbytes"))
	confHeight := int32(700)
	csvDelay := int32(144)

	store.rounds[roundID] = indexer.RoundRow{
		RoundID:            roundID,
		CommitmentTxid:     commitTxID,
		ConfirmationHeight: &confHeight,
		CsvDelay:           csvDelay,
	}
	store.trees[fmt.Sprintf("%x:%d", roundID[:], 0)] = testTree

	batchIdx := int32(0)
	store.vtxos[parentOutpoint.String()] = indexer.VTXORow{
		Outpoint:         parentOutpoint,
		BatchOutputIndex: &batchIdx,
		Amount:           1_000,
		PkScript:         []byte("parent-pk"),
		Status:           "spent",
		RoundID:          &roundID,
	}

	// New submit's own tx contributions: one Ark + one checkpoint
	// that consumes the parent. These have predictable txids and
	// vbytes regardless of whether the tree is signed, so the
	// assertion below is non-trivial.
	arkTx := makeBareTx("ark-tree-walk", 1, 1)
	checkpointTx := wire.NewMsgTx(2)
	checkpointTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: parentOutpoint,
	})
	checkpointTx.AddTxOut(&wire.TxOut{
		Value:    100,
		PkScript: []byte{0x51},
	})
	arkPkt, err := psbt.NewFromUnsignedTx(arkTx)
	require.NoError(t, err)
	cpPkt, err := psbt.NewFromUnsignedTx(checkpointTx)
	require.NoError(t, err)

	got, err := indexer.EstimateOORLineageVBytes(
		ctx, store,
		[]wire.OutPoint{parentOutpoint},
		arkPkt, []*psbt.Packet{cpPkt},
	)
	require.NoError(t, err)

	// The new-submit floor: every cap result must include at least
	// the Ark + checkpoint vbytes the recipient would publish.
	floor := uint32(indexer.TxVBytes(arkTx) +
		indexer.TxVBytes(checkpointTx))
	require.GreaterOrEqual(t, got, floor,
		"round-backed lineage walk must yield at least the new "+
			"submit's vbytes (resolver successfully walked tree)")
}

// TestEstimateOORLineageVBytesSharedTreeNodesDeDuped verifies the
// txid-based de-dup invariant for the multi-input case: when two
// parents come from the same tree, sharing internal tree nodes,
// adding the second parent never *decreases* the cap result and
// the structural de-dup machinery does not crash. The shared-nodes
// invariant is enforced by the txid-keyed `seen` map regardless of
// whether the tree is signed.
func TestEstimateOORLineageVBytesSharedTreeNodesDeDuped(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newMockLineageStore()

	testTree, leafOutpoints := buildTestTree(t, 4)
	require.GreaterOrEqual(t, len(leafOutpoints), 2)
	parentA := leafOutpoints[0]
	parentB := leafOutpoints[1]

	roundID := newTestRoundID(0xA2)
	commitTxID := chainhash.HashH([]byte("commitment-shared"))
	confHeight := int32(700)
	csvDelay := int32(144)

	store.rounds[roundID] = indexer.RoundRow{
		RoundID:            roundID,
		CommitmentTxid:     commitTxID,
		ConfirmationHeight: &confHeight,
		CsvDelay:           csvDelay,
	}
	store.trees[fmt.Sprintf("%x:%d", roundID[:], 0)] = testTree

	batchIdx := int32(0)
	store.vtxos[parentA.String()] = indexer.VTXORow{
		Outpoint:         parentA,
		BatchOutputIndex: &batchIdx,
		Amount:           1_000,
		PkScript:         []byte("parent-A-pk"),
		Status:           "spent",
		RoundID:          &roundID,
	}
	store.vtxos[parentB.String()] = indexer.VTXORow{
		Outpoint:         parentB,
		BatchOutputIndex: &batchIdx,
		Amount:           1_000,
		PkScript:         []byte("parent-B-pk"),
		Status:           "spent",
		RoundID:          &roundID,
	}

	gotSingle, err := indexer.EstimateOORLineageVBytes(
		ctx, store,
		[]wire.OutPoint{parentA},
		nil, nil,
	)
	require.NoError(t, err)

	gotDouble, err := indexer.EstimateOORLineageVBytes(
		ctx, store,
		[]wire.OutPoint{parentA, parentB},
		nil, nil,
	)
	require.NoError(t, err)

	require.GreaterOrEqual(t, gotDouble, gotSingle,
		"adding a parent cannot decrease total vbytes")

	// Compute the union-of-paths tree contribution as a reference,
	// matching LineageVBytes's internal accounting (skip nodes that
	// fail to produce a signed tx; de-dup by txid).
	resolver := indexer.NewTestLineageResolver(store, nil)
	rowA, err := store.GetVTXO(ctx, parentA)
	require.NoError(t, err)
	linA, err := resolver.Resolve(ctx, rowA)
	require.NoError(t, err)
	rowB, err := store.GetVTXO(ctx, parentB)
	require.NoError(t, err)
	linB, err := resolver.Resolve(ctx, rowB)
	require.NoError(t, err)

	seen := make(map[chainhash.Hash]struct{})
	var unionVBytes uint64
	for _, lin := range []*indexer.TestVTXOLineage{linA, linB} {
		fragment := indexer.LineageAncestryFragmentTreePath(lin, 0)
		require.NotNil(t, fragment)
		for node := range fragment.Root.NodesIter() {
			signedTx, err := node.ToSignedTx()
			if err != nil {
				continue
			}
			txid := signedTx.TxHash()
			if _, dup := seen[txid]; dup {
				continue
			}
			seen[txid] = struct{}{}
			unionVBytes += uint64(indexer.TxVBytes(signedTx))
		}
	}

	require.Equal(t, uint32(unionVBytes), gotDouble,
		"shared tree nodes must contribute exactly once across "+
			"the multi-input lineage; reference union and "+
			"LineageVBytes must agree")
}
