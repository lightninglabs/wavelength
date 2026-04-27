package indexer_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/indexer"
	"github.com/lightninglabs/darepo/rounds"
	"github.com/stretchr/testify/require"
)

// mockLineageStore is a minimal mock of indexer.Store for lineage
// resolver tests. Only the methods used by the resolver are
// implemented; the rest panic to surface unexpected calls.
type mockLineageStore struct {
	rounds map[rounds.RoundID]indexer.RoundRow
	trees  map[string]*tree.Tree // key: "roundID:batchIdx"
	vtxos  map[string]indexer.VTXORow

	oorSessions    map[string]indexer.OORSession
	oorCheckpoints map[int32][]indexer.OORCheckpoint

	roundCallCount int
	treeCallCount  int
}

// newMockLineageStore creates a mockLineageStore with initialised maps.
func newMockLineageStore() *mockLineageStore {
	return &mockLineageStore{
		rounds: make(map[rounds.RoundID]indexer.RoundRow),
		trees:  make(map[string]*tree.Tree),
		vtxos:  make(map[string]indexer.VTXORow),
		oorSessions: make(
			map[string]indexer.OORSession,
		),
		oorCheckpoints: make(
			map[int32][]indexer.OORCheckpoint,
		),
	}
}

func (m *mockLineageStore) GetRound(_ context.Context,
	roundID rounds.RoundID) (indexer.RoundRow, error) {

	m.roundCallCount++

	row, ok := m.rounds[roundID]
	if !ok {
		return indexer.RoundRow{}, fmt.Errorf(
			"round not found: %w", indexer.ErrNotFound,
		)
	}

	return row, nil
}

func (m *mockLineageStore) LoadVTXOTree(_ context.Context,
	roundID rounds.RoundID,
	batchOutputIndex int) (*tree.Tree, error) {

	m.treeCallCount++

	key := fmt.Sprintf("%x:%d", roundID[:], batchOutputIndex)
	t, ok := m.trees[key]
	if !ok {
		return nil, fmt.Errorf(
			"tree not found: %w", indexer.ErrNotFound,
		)
	}

	return t, nil
}

func (m *mockLineageStore) GetVTXO(_ context.Context,
	outpoint wire.OutPoint) (indexer.VTXORow, error) {

	row, ok := m.vtxos[outpoint.String()]
	if !ok {
		return indexer.VTXORow{}, fmt.Errorf(
			"vtxo not found: %w", indexer.ErrNotFound,
		)
	}

	return row, nil
}

// GetOORSpendingSessionTxidByInput reports no OOR spender in lineage tests.
func (m *mockLineageStore) GetOORSpendingSessionTxidByInput(
	_ context.Context,
	_ wire.OutPoint) ([]byte, error) {

	return nil, indexer.ErrNotFound
}

// OORSessionSpendsScript reports no session-script linkage in lineage tests.
func (m *mockLineageStore) OORSessionSpendsScript(
	_ context.Context, _ []byte, _ []byte) (bool, error) {

	return false, nil
}

func (m *mockLineageStore) ListRoundsByIDs(_ context.Context,
	ids []rounds.RoundID) ([]indexer.RoundRow, error) {

	var rows []indexer.RoundRow
	for _, id := range ids {
		if row, ok := m.rounds[id]; ok {
			rows = append(rows, row)
		}
	}

	return rows, nil
}

// Stub methods that satisfy the Store interface but are unused by
// the lineage resolver.

func (m *mockLineageStore) ListVTXOsByPkScripts(
	_ context.Context,
	_ [][]byte) ([]indexer.VTXORow, error) {

	return nil, nil
}

func (m *mockLineageStore) GetOORRecipientEventBySessionOutput(
	_ context.Context, _, _ []byte,
	_ int32) (indexer.OORRecipientEvent, error) {

	return indexer.OORRecipientEvent{}, indexer.ErrNotFound
}

func (m *mockLineageStore) GetOORSession(
	_ context.Context,
	sessionID []byte) (indexer.OORSession, error) {

	session, ok := m.oorSessions[string(sessionID)]
	if !ok {
		return indexer.OORSession{}, indexer.ErrNotFound
	}

	return session, nil
}

func (m *mockLineageStore) ListOORCheckpoints(
	_ context.Context,
	sessionDBID int32) ([]indexer.OORCheckpoint, error) {

	return m.oorCheckpoints[sessionDBID], nil
}

func (m *mockLineageStore) UpsertReceiveScript(
	_ context.Context, _ string, _ []byte,
	_ time.Time, _ string, _ time.Time, _ []byte, _ []byte,
	_ uint32) error {

	return nil
}

func (m *mockLineageStore) DeleteReceiveScript(
	_ context.Context, _ string, _ []byte) (int64, error) {

	return 0, nil
}

func (m *mockLineageStore) ListActiveReceiveScriptsByPrincipal(
	_ context.Context, _ string,
	_ time.Time) ([]indexer.ReceiveScript, error) {

	return nil, nil
}

func (m *mockLineageStore) ListOORRecipientEventsAfterWithSession(
	_ context.Context, _ []byte, _ int64,
	_ int32) ([]indexer.OORRecipientEventWithSession, error) {

	return nil, nil
}

func (m *mockLineageStore) GetOORSessionCheckpoints(
	_ context.Context,
	_ []byte) ([]indexer.OORSessionCheckpoint, error) {

	return nil, nil
}

func (m *mockLineageStore) ExecReadTx(
	_ context.Context, fn func(indexer.Store) error) error {

	return fn(m)
}

func (m *mockLineageStore) InsertOORRecipientEvent(
	_ context.Context, _ []byte, _ int64,
	_, _ int32, _ int64, _ time.Time) (int64, error) {

	return 0, nil
}

func (m *mockLineageStore) GetMaxOORRecipientEventID(
	_ context.Context, _ []byte) (int64, error) {

	return 0, nil
}

func (m *mockLineageStore) ListActiveReceivePrincipalsByScript(
	_ context.Context, _ []byte,
	_ time.Time) ([]indexer.ReceiveScript, error) {

	return nil, nil
}

func (m *mockLineageStore) ListVTXOEventsAfterByScripts(
	_ context.Context, _ int64, _ [][]byte,
	_ int32) ([]indexer.VTXOEvent, error) {

	return nil, nil
}

func (m *mockLineageStore) InsertVTXOEvent(
	_ context.Context, _ []byte, _ string,
	_ wire.OutPoint, _ string,
	_ time.Time, _ indexer.VTXOEventMetadata) (int64, error) {

	return 0, nil
}

// Compile-time check that mockLineageStore satisfies indexer.Store.
var _ indexer.Store = (*mockLineageStore)(nil)

// buildTestTree constructs a minimal VTXO tree with the given number
// of leaves. It returns the tree and the outpoints for each leaf.
func buildTestTree(t *testing.T,
	numLeaves int) (*tree.Tree, []wire.OutPoint) {

	t.Helper()

	_, operatorKey := newTestKeyPair(t)

	leaves := make([]tree.LeafDescriptor, numLeaves)
	for i := range leaves {
		_, cosignerKey := newTestKeyPair(t)
		leaves[i] = tree.LeafDescriptor{
			PkScript: []byte(
				fmt.Sprintf("vtxo_script_%d", i),
			),
			Amount:      btcutil.Amount(1000 * (i + 1)),
			CoSignerKey: cosignerKey,
		}
	}

	rootOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("batch-tx")),
		Index: 0,
	}

	var totalAmt int64
	for _, leaf := range leaves {
		totalAmt += int64(leaf.Amount)
	}

	rootOutput := &wire.TxOut{Value: totalAmt}
	sweepRoot := make([]byte, 32)

	builtTree, err := tree.NewTree(
		rootOutpoint, rootOutput, leaves, operatorKey,
		sweepRoot, 2,
	)
	require.NoError(t, err)

	// Collect leaf outpoints in iteration order.
	var leafOutpoints []wire.OutPoint
	for leaf := range builtTree.Root.LeavesIter() {
		op, err := leaf.GetNonAnchorOutpoint()
		require.NoError(t, err)
		leafOutpoints = append(leafOutpoints, *op)
	}

	return builtTree, leafOutpoints
}

// newTestKeyPair generates a random secp256k1 key pair for test use.
func newTestKeyPair(t *testing.T) (
	*btcec.PrivateKey, *btcec.PublicKey) {

	t.Helper()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return priv, priv.PubKey()
}

func checkpointPSBTForParent(t *testing.T,
	parent wire.OutPoint) *psbt.Packet {

	t.Helper()

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: parent,
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    1,
		PkScript: []byte{0x51},
	})

	pkt, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)

	return pkt
}

// TestLineageResolverRoundBacked verifies that the resolver correctly
// resolves a round-backed VTXO row and returns lineage with the
// expected round metadata and tree path.
func TestLineageResolverRoundBacked(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newMockLineageStore()

	// Build a test tree with 4 leaves.
	testTree, leafOutpoints := buildTestTree(t, 4)
	require.True(t, len(leafOutpoints) >= 1)

	roundID := newTestRoundID(0xAA)
	commitTxID := chainhash.HashH([]byte("commitment-tx"))
	confHeight := int32(500)
	csvDelay := int32(144)

	store.rounds[roundID] = indexer.RoundRow{
		RoundID:            roundID,
		CommitmentTxid:     commitTxID,
		ConfirmationHeight: &confHeight,
		CsvDelay:           csvDelay,
	}

	treeKey := fmt.Sprintf("%x:%d", roundID[:], 0)
	store.trees[treeKey] = testTree

	// Build a VTXO row that points to the first leaf.
	batchIdx := int32(0)
	row := indexer.VTXORow{
		Outpoint:         leafOutpoints[0],
		BatchOutputIndex: &batchIdx,
		Amount:           1000,
		PkScript:         []byte("vtxo_script_0"),
		Status:           "live",
		RoundID:          &roundID,
	}

	resolver := indexer.NewTestLineageResolver(store, nil)
	lineage, err := resolver.Resolve(ctx, row)
	require.NoError(t, err)
	require.NotNil(t, lineage)

	// Verify round metadata.
	require.Equal(t, roundID.String(),
		indexer.LineageRoundID(lineage),
	)
	require.Equal(t, commitTxID,
		indexer.LineageCommitmentTxID(lineage),
	)
	require.Equal(t, uint32(csvDelay),
		indexer.LineageRelativeExpiry(lineage),
	)

	// Batch expiry = confirmation height + csv delay.
	expectedExpiry := confHeight + csvDelay
	require.Equal(t, expectedExpiry,
		indexer.LineageBatchExpiry(lineage),
	)

	// Created height should match the confirmation height.
	require.Equal(t, confHeight,
		indexer.LineageCreatedHeight(lineage),
	)

	// Tree path should be non-nil and have a valid depth.
	require.NotNil(t, indexer.LineageTreePath(lineage))
	require.Greater(t,
		indexer.LineageTreeDepth(lineage), 0,
	)

	// TLV-encoded tree path should be non-empty.
	require.NotEmpty(t, indexer.LineageTreePathTLV(lineage))

	// Chain depth for a direct round-backed VTXO is 0.
	require.Equal(t, 0, indexer.LineageChainDepth(lineage))
}

// TestLineageResolverVirtualMultiRoundParents verifies multi-input OOR VTXOs
// that merge parents from different commitment rounds remain queryable. The
// current RPC shape carries singular lineage fields, so the resolver inherits
// the earliest-expiring parent and omits the non-singular tree path.
func TestLineageResolverVirtualMultiRoundParents(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newMockLineageStore()

	treeA, leafOutpointsA := buildTestTree(t, 1)
	require.Len(t, leafOutpointsA, 1)

	treeB, leafOutpointsB := buildTestTree(t, 2)
	require.Len(t, leafOutpointsB, 2)

	roundA := newTestRoundID(0xA1)
	roundB := newTestRoundID(0xB2)

	commitA := chainhash.HashH([]byte("commitment-a"))
	commitB := chainhash.HashH([]byte("commitment-b"))
	confA := int32(500)
	confB := int32(700)
	csvDelay := int32(144)

	store.rounds[roundA] = indexer.RoundRow{
		RoundID:            roundA,
		CommitmentTxid:     commitA,
		ConfirmationHeight: &confA,
		CsvDelay:           csvDelay,
	}
	store.rounds[roundB] = indexer.RoundRow{
		RoundID:            roundB,
		CommitmentTxid:     commitB,
		ConfirmationHeight: &confB,
		CsvDelay:           csvDelay,
	}

	store.trees[fmt.Sprintf("%x:%d", roundA[:], 0)] = treeA
	store.trees[fmt.Sprintf("%x:%d", roundB[:], 0)] = treeB

	batchIdx := int32(0)
	parentA := indexer.VTXORow{
		Outpoint:         leafOutpointsA[0],
		BatchOutputIndex: &batchIdx,
		Amount:           1000,
		PkScript:         []byte("parent_a"),
		Status:           "spent",
		RoundID:          &roundA,
	}
	parentB := indexer.VTXORow{
		Outpoint:         leafOutpointsB[1],
		BatchOutputIndex: &batchIdx,
		Amount:           1000,
		PkScript:         []byte("parent_b"),
		Status:           "spent",
		RoundID:          &roundB,
	}
	store.vtxos[parentA.Outpoint.String()] = parentA
	store.vtxos[parentB.Outpoint.String()] = parentB

	sessionID := chainhash.HashH([]byte("mixed-round-oor-session"))
	sessionDBID := int32(77)
	store.oorSessions[string(sessionID[:])] = indexer.OORSession{
		ID: int64(sessionDBID),
	}
	store.oorCheckpoints[sessionDBID] = []indexer.OORCheckpoint{
		{Psbt: checkpointPSBTForParent(t, parentA.Outpoint)},
		{Psbt: checkpointPSBTForParent(t, parentB.Outpoint)},
	}

	virtualRow := indexer.VTXORow{
		Outpoint: wire.OutPoint{
			Hash:  sessionID,
			Index: 1,
		},
		Amount:   1500,
		PkScript: []byte("recipient"),
		Status:   "live",
	}

	resolver := indexer.NewTestLineageResolver(store, nil)
	lineage, err := resolver.Resolve(ctx, virtualRow)
	require.NoError(t, err)
	require.NotNil(t, lineage)

	require.Equal(t, roundA.String(), indexer.LineageRoundID(lineage))
	require.Equal(t, commitA, indexer.LineageCommitmentTxID(lineage))
	require.Equal(t, confA+csvDelay,
		indexer.LineageBatchExpiry(lineage))
	require.Equal(t, confA, indexer.LineageCreatedHeight(lineage))
	require.Equal(t, uint32(csvDelay),
		indexer.LineageRelativeExpiry(lineage))
	require.Equal(t, 1, indexer.LineageChainDepth(lineage))
	require.Zero(t, indexer.LineageTreeDepth(lineage))
	require.Nil(t, indexer.LineageTreePath(lineage))
	require.Empty(t, indexer.LineageTreePathTLV(lineage))
}

// TestLineageResolverCaching verifies that the resolver caches
// lineage results and does not re-query the store on subsequent
// calls for the same VTXO.
func TestLineageResolverCaching(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newMockLineageStore()

	testTree, leafOutpoints := buildTestTree(t, 2)
	require.True(t, len(leafOutpoints) >= 1)

	roundID := newTestRoundID(0xBB)
	commitTxID := chainhash.HashH([]byte("commitment-cache"))
	confHeight := int32(600)

	store.rounds[roundID] = indexer.RoundRow{
		RoundID:            roundID,
		CommitmentTxid:     commitTxID,
		ConfirmationHeight: &confHeight,
		CsvDelay:           144,
	}

	treeKey := fmt.Sprintf("%x:%d", roundID[:], 0)
	store.trees[treeKey] = testTree

	batchIdx := int32(0)
	row := indexer.VTXORow{
		Outpoint:         leafOutpoints[0],
		BatchOutputIndex: &batchIdx,
		Amount:           1000,
		PkScript:         []byte("vtxo_script_0"),
		Status:           "live",
		RoundID:          &roundID,
	}

	resolver := indexer.NewTestLineageResolver(store, nil)

	// First call: should hit the store.
	lineage1, err := resolver.Resolve(ctx, row)
	require.NoError(t, err)
	require.NotNil(t, lineage1)

	callsAfterFirst := store.treeCallCount
	require.Equal(t, 1, callsAfterFirst)

	// Second call: should return cached result without
	// additional store queries.
	lineage2, err := resolver.Resolve(ctx, row)
	require.NoError(t, err)
	require.NotNil(t, lineage2)

	require.Equal(t, callsAfterFirst, store.treeCallCount,
		"second Resolve should not hit the store",
	)

	// Both calls should return the same lineage pointer from
	// the cache.
	cache := indexer.LineageByOutpoint(resolver)
	key := row.Outpoint.String()
	require.Contains(t, cache, key)

	// Verify the returned values are identical.
	require.Equal(t,
		indexer.LineageRoundID(lineage1),
		indexer.LineageRoundID(lineage2),
	)
}

// TestLineageResolverMissingRound verifies that resolving a VTXO row
// referencing a non-existent round returns an error.
func TestLineageResolverMissingRound(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newMockLineageStore()

	// Do NOT insert any round data into the mock store.
	missingRoundID := newTestRoundID(0xCC)
	batchIdx := int32(0)
	row := indexer.VTXORow{
		Outpoint: wire.OutPoint{
			Hash:  chainhash.HashH([]byte("missing-vtxo")),
			Index: 0,
		},
		BatchOutputIndex: &batchIdx,
		Amount:           500,
		PkScript:         []byte("pk_script"),
		Status:           "live",
		RoundID:          &missingRoundID,
	}

	resolver := indexer.NewTestLineageResolver(store, nil)
	lineage, err := resolver.Resolve(ctx, row)
	require.Error(t, err)
	require.Nil(t, lineage)
	require.ErrorContains(t, err, "get round")
}

// TestLineageResolverMissingTree verifies that resolving a VTXO row
// when the tree cannot be loaded returns an error.
func TestLineageResolverMissingTree(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newMockLineageStore()

	// Insert a round but do NOT insert the tree.
	roundID := newTestRoundID(0xDD)
	confHeight := int32(700)
	store.rounds[roundID] = indexer.RoundRow{
		RoundID:            roundID,
		CommitmentTxid:     chainhash.HashH([]byte("commit")),
		ConfirmationHeight: &confHeight,
		CsvDelay:           144,
	}

	batchIdx := int32(0)
	row := indexer.VTXORow{
		Outpoint: wire.OutPoint{
			Hash:  chainhash.HashH([]byte("no-tree-vtxo")),
			Index: 0,
		},
		BatchOutputIndex: &batchIdx,
		Amount:           500,
		PkScript:         []byte("pk_script"),
		Status:           "live",
		RoundID:          &roundID,
	}

	resolver := indexer.NewTestLineageResolver(store, nil)
	lineage, err := resolver.Resolve(ctx, row)
	require.Error(t, err)
	require.Nil(t, lineage)
	require.ErrorContains(t, err, "load vtxo tree")
}

// TestApplyLineageMetadata verifies that applyLineageMetadata
// correctly populates all fields on the RPC VTXO output, including
// the TreePath conversion.
func TestApplyLineageMetadata(t *testing.T) {
	t.Parallel()

	// Build a small tree to get a valid tree path.
	testTree, _ := buildTestTree(t, 2)
	require.NotNil(t, testTree)

	// Extract a subtree for the first leaf to get a valid path.
	var firstLeaf *tree.Node
	for leaf := range testTree.Root.LeavesIter() {
		firstLeaf = leaf
		break
	}
	require.NotNil(t, firstLeaf)

	firstOp, err := firstLeaf.GetNonAnchorOutpoint()
	require.NoError(t, err)

	extracted, err := testTree.ExtractPathForIndices(0)
	require.NoError(t, err)

	commitTxID := chainhash.HashH([]byte("apply-commit"))
	lineage := indexer.NewTestVTXOLineage(
		"test-round-id",
		commitTxID,
		644,          // batchExpiry
		144,          // relativeExpiry
		2,            // treeDepth
		1,            // chainDepth
		500,          // createdHeight
		extracted,    // treePath
		[]byte{0x01}, // treePathTLV (non-empty placeholder)
	)

	out := &arkrpc.VTXO{
		Outpoint: &arkrpc.OutPoint{
			Txid: firstOp.Hash[:],
			Vout: firstOp.Index,
		},
		ValueSat: 1000,
	}

	err = indexer.ApplyLineageMetadata(out, lineage)
	require.NoError(t, err)

	// Verify all scalar fields were populated.
	require.Equal(t, "test-round-id", out.RoundId)
	require.Equal(t, commitTxID[:], out.CommitmentTxid)
	require.Equal(t, int32(644), out.BatchExpiryHeight)
	require.Equal(t, uint32(144), out.RelativeExpiry)
	require.Equal(t, uint32(2), out.TreeDepth)
	require.Equal(t, uint32(1), out.ChainDepth)
	require.Equal(t, int32(500), out.CreatedHeight)

	// TreePath should be non-nil with at least one node.
	require.NotNil(t, out.TreePath)
	require.NotEmpty(t, out.TreePath.Nodes)
}

// TestApplyLineageMetadataNilInputs verifies that
// applyLineageMetadata handles nil inputs gracefully.
func TestApplyLineageMetadataNilInputs(t *testing.T) {
	t.Parallel()

	// Nil VTXO — should be a no-op.
	err := indexer.ApplyLineageMetadata(nil, &indexer.TestVTXOLineage{})
	require.NoError(t, err)

	// Nil lineage — should be a no-op.
	err = indexer.ApplyLineageMetadata(&arkrpc.VTXO{}, nil)
	require.NoError(t, err)

	// Both nil — should be a no-op.
	err = indexer.ApplyLineageMetadata(nil, nil)
	require.NoError(t, err)
}

// TestApplyLineageMetadataZeroCommitment verifies that a zero-value
// commitment txid does not populate the CommitmentTxid field on the
// RPC output.
func TestApplyLineageMetadataZeroCommitment(t *testing.T) {
	t.Parallel()

	lineage := indexer.NewTestVTXOLineage(
		"round-zero",
		chainhash.Hash{}, // zero hash
		0,                // batchExpiry
		0,                // relativeExpiry
		0,                // treeDepth
		0,                // chainDepth
		0,                // createdHeight
		nil,              // no tree path
		nil,              // no TLV
	)

	out := &arkrpc.VTXO{}
	err := indexer.ApplyLineageMetadata(out, lineage)
	require.NoError(t, err)

	// Zero commitment hash should not set CommitmentTxid.
	require.Nil(t, out.CommitmentTxid)

	// RoundID should still be set.
	require.Equal(t, "round-zero", out.RoundId)
}

// TestLineageResolverUnconfirmedRound verifies that a round without
// a confirmation height produces zero batch expiry and created
// height.
func TestLineageResolverUnconfirmedRound(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newMockLineageStore()

	testTree, leafOutpoints := buildTestTree(t, 2)
	require.True(t, len(leafOutpoints) >= 1)

	roundID := newTestRoundID(0xEE)
	commitTxID := chainhash.HashH([]byte("unconfirmed"))

	// No ConfirmationHeight set.
	store.rounds[roundID] = indexer.RoundRow{
		RoundID:        roundID,
		CommitmentTxid: commitTxID,
		CsvDelay:       144,
	}

	treeKey := fmt.Sprintf("%x:%d", roundID[:], 0)
	store.trees[treeKey] = testTree

	batchIdx := int32(0)
	row := indexer.VTXORow{
		Outpoint:         leafOutpoints[0],
		BatchOutputIndex: &batchIdx,
		Amount:           1000,
		PkScript:         []byte("vtxo_script_0"),
		Status:           "live",
		RoundID:          &roundID,
	}

	resolver := indexer.NewTestLineageResolver(store, nil)
	lineage, err := resolver.Resolve(ctx, row)
	require.NoError(t, err)
	require.NotNil(t, lineage)

	// Unconfirmed round: batch expiry and created height are 0.
	require.Equal(t, int32(0),
		indexer.LineageBatchExpiry(lineage),
	)
	require.Equal(t, int32(0),
		indexer.LineageCreatedHeight(lineage),
	)
}
