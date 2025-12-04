package chainresolver

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/internal/testutils"
	"github.com/lightninglabs/darepo-client/lib/tree"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Integration Test Helpers
// ---------------------------------------------------------------------------

// integrationTestConfig holds configuration for integration tests.
type integrationTestConfig struct {
	operatorKey    *btcec.PublicKey
	operatorSigner input.Signer
	sweepKey       *btcec.PublicKey
	sweepSigner    input.Signer
	exitDelay      uint32
}

// newIntegrationTestConfig creates config with deterministic keys.
func newIntegrationTestConfig() *integrationTestConfig {
	operatorPub, operatorSigner := testutils.CreateKey(1)
	sweepPub, sweepSigner := testutils.CreateKey(2)

	return &integrationTestConfig{
		operatorKey:    operatorPub,
		operatorSigner: operatorSigner,
		sweepKey:       sweepPub,
		sweepSigner:    sweepSigner,
		exitDelay:      144,
	}
}

// userKeyPair represents a VTXO owner's keys.
type userKeyPair struct {
	pubKey *btcec.PublicKey
	signer input.Signer
}

// createUserKeyPair creates deterministic user keys at the given index.
func createUserKeyPair(index int32) userKeyPair {
	pub, signer := testutils.CreateKey(index)

	return userKeyPair{
		pubKey: pub,
		signer: signer,
	}
}

// createRealVTXOTree creates a VTXO tree with proper taproot scripts.
func createRealVTXOTree(
	t *testing.T,
	cfg *integrationTestConfig,
	amounts []btcutil.Amount,
) (*tree.Tree, []userKeyPair) {

	t.Helper()

	// Create user keys for each VTXO.
	users := make([]userKeyPair, len(amounts))
	vtxos := make([]tree.VTXODescriptor, len(amounts))

	for i, amount := range amounts {
		// Start at index 10 to avoid conflicts with config keys.
		users[i] = createUserKeyPair(int32(10 + i))

		// Create VTXO descriptor with real taproot script.
		desc, err := tree.NewVTXODescriptor(
			amount, users[i].pubKey, cfg.operatorKey, cfg.exitDelay,
		)
		require.NoError(t, err)
		vtxos[i] = *desc
	}

	// Create batch outpoint.
	batchOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("commitment_tx")),
		Index: 0,
	}

	// Build real batch output with proper taproot script.
	batchOutput, err := tree.BuildBatchOutput(
		vtxos, cfg.operatorKey, cfg.sweepKey, cfg.exitDelay,
	)
	require.NoError(t, err)

	// Build the VTXO tree.
	vtxtTree, err := tree.BuildVTXOTree(
		batchOutpoint, batchOutput, vtxos,
		cfg.operatorKey, cfg.sweepKey, cfg.exitDelay,
		2, // Binary tree.
	)
	require.NoError(t, err)

	return vtxtTree, users
}

// ---------------------------------------------------------------------------
// Integration Tests - Real VTXO Trees
// ---------------------------------------------------------------------------

// TestIntegrationMonitorRealVTXOTree tests monitoring a VTXO with proper
// taproot scripts and tree structure.
func TestIntegrationMonitorRealVTXOTree(t *testing.T) {
	t.Parallel()

	cfg := newIntegrationTestConfig()

	// Create a 3-VTXO tree.
	amounts := []btcutil.Amount{500000, 400000, 300000}
	vtxtTree, users := createRealVTXOTree(t, cfg, amounts)

	// Verify tree structure.
	require.Equal(t, 3, vtxtTree.NumLeaves())
	require.Len(t, users, 3)

	// Setup test system.
	system, chainSourceRef, _ := setupTestSystem(t)
	defer func() { _ = system.Shutdown(t.Context()) }()

	signer := &mockSigner{}
	log := testLogger{}

	resolver := NewClientResolverActor(
		chainSourceRef, system, signer, log,
	)
	resolverRef := ClientResolverKey.Spawn(
		system, "client-resolver", resolver,
	)

	// Get the first leaf VTXO for monitoring.
	leaves := vtxtTree.Root.GetLeafNodes()
	require.NotEmpty(t, leaves)

	firstLeaf := leaves[0]
	leafTxid, err := firstLeaf.TXID()
	require.NoError(t, err)

	// The VTXO outpoint is the first output of the leaf node.
	vtxoOutpoint := wire.OutPoint{
		Hash:  leafTxid,
		Index: 0,
	}

	// Get the VTXO output (first output of leaf, skipping anchor).
	var vtxoOutput *wire.TxOut
	for _, out := range firstLeaf.Outputs {
		if out.Value > 0 {
			vtxoOutput = out

			break
		}
	}
	require.NotNil(t, vtxoOutput)

	// Monitor the VTXO.
	ctx := t.Context()
	req := &MonitorVTXORequest{
		VTXOOutpoint: vtxoOutpoint,
		VTXOOutput:   vtxoOutput,
		TreePath:     vtxtTree,
		ExitDelay:    cfg.exitDelay,
		HeightHint:   100,
		NotifyActor:  fn.None[actor.TellOnlyRef[ClientResolverEvent]](),
	}

	future := resolverRef.Ask(ctx, req)
	result := future.Await(ctx)

	require.True(t, result.IsOk())

	// Verify response.
	resp, err := result.Unpack()
	require.NoError(t, err)

	monitorResp, ok := resp.(*MonitorVTXOResponse)
	require.True(t, ok)
	require.NotEmpty(t, monitorResp.MonitorID)
}

// TestIntegrationVTXOTreeTaprootOutputs verifies that all outputs in a
// created tree are valid taproot outputs.
func TestIntegrationVTXOTreeTaprootOutputs(t *testing.T) {
	t.Parallel()

	cfg := newIntegrationTestConfig()

	// Create different sized trees.
	testCases := []struct {
		name    string
		amounts []btcutil.Amount
	}{
		{
			name:    "single VTXO",
			amounts: []btcutil.Amount{100000},
		},
		{
			name:    "two VTXOs",
			amounts: []btcutil.Amount{50000, 50000},
		},
		{
			name:    "four VTXOs",
			amounts: []btcutil.Amount{40000, 30000, 20000, 10000},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			vtxtTree, _ := createRealVTXOTree(t, cfg, tc.amounts)

			// Verify batch output is taproot.
			isTaproot := txscript.IsPayToTaproot(
				vtxtTree.BatchOutput.PkScript,
			)
			require.True(t, isTaproot)

			// Verify all nodes have taproot outputs.
			for node := range vtxtTree.Root.NodesIter() {
				for _, output := range node.Outputs {
					// Skip anchor outputs.
					if output.Value == 0 {
						continue
					}

					isTR := txscript.IsPayToTaproot(
						output.PkScript,
					)
					require.True(t, isTR)
				}
			}
		})
	}
}

// TestIntegrationVTXOTreeTransactionStructure tests that tree transactions
// are properly linked.
func TestIntegrationVTXOTreeTransactionStructure(t *testing.T) {
	t.Parallel()

	cfg := newIntegrationTestConfig()

	// Create a 4-VTXO tree (multiple levels).
	amounts := []btcutil.Amount{400000, 300000, 200000, 100000}
	vtxtTree, _ := createRealVTXOTree(t, cfg, amounts)

	// Root should spend the batch outpoint.
	require.Equal(t, vtxtTree.BatchOutpoint, vtxtTree.Root.Input)

	// Get root txid.
	rootTxid, err := vtxtTree.Root.TXID()
	require.NoError(t, err)
	require.NotEqual(t, chainhash.Hash{}, rootTxid)

	// Verify children reference parent.
	for idx, child := range vtxtTree.Root.Children {
		require.Equal(t, rootTxid, child.Input.Hash)
		require.Equal(t, idx, child.Input.Index)

		// Verify child has valid txid.
		childTxid, err := child.TXID()
		require.NoError(t, err)
		require.NotEqual(t, chainhash.Hash{}, childTxid)

		// Check grandchildren if present.
		if !child.IsLeaf() {
			for gIdx, grandchild := range child.Children {
				require.Equal(t,
					childTxid, grandchild.Input.Hash,
				)
				require.Equal(t,
					gIdx, grandchild.Input.Index,
				)
			}
		}
	}
}

// TestIntegrationExtractUserPath tests extracting a single user's path
// from a full VTXO tree (what a client would store).
func TestIntegrationExtractUserPath(t *testing.T) {
	t.Parallel()

	cfg := newIntegrationTestConfig()

	// Create a 4-VTXO tree.
	amounts := []btcutil.Amount{400000, 300000, 200000, 100000}
	vtxtTree, _ := createRealVTXOTree(t, cfg, amounts)

	// Get leaves (where user VTXOs are).
	leaves := vtxtTree.Root.GetLeafNodes()
	require.Len(t, leaves, 4)

	// Each leaf represents one user's VTXO.
	for i, leaf := range leaves {
		// Verify leaf has VTXO output.
		var vtxoOutput *wire.TxOut
		for _, out := range leaf.Outputs {
			if out.Value > 0 {
				vtxoOutput = out

				break
			}
		}
		require.NotNil(t, vtxoOutput, "leaf %d should have VTXO", i)

		// Verify leaf is taproot.
		isTaproot := txscript.IsPayToTaproot(vtxoOutput.PkScript)
		require.True(t, isTaproot, "VTXO %d should be taproot", i)

		// Verify leaf has valid txid.
		leafTxid, err := leaf.TXID()
		require.NoError(t, err)
		require.NotEqual(t, chainhash.Hash{}, leafTxid)
	}
}

// TestIntegrationMultipleVTXOMonitoring tests monitoring multiple VTXOs
// concurrently.
func TestIntegrationMultipleVTXOMonitoring(t *testing.T) {
	t.Parallel()

	cfg := newIntegrationTestConfig()

	// Create a 5-VTXO tree.
	amounts := []btcutil.Amount{500000, 400000, 300000, 200000, 100000}
	vtxtTree, _ := createRealVTXOTree(t, cfg, amounts)

	// Setup test system.
	system, chainSourceRef, _ := setupTestSystem(t)
	defer func() { _ = system.Shutdown(t.Context()) }()

	signer := &mockSigner{}
	log := testLogger{}

	resolver := NewClientResolverActor(
		chainSourceRef, system, signer, log,
	)
	resolverRef := ClientResolverKey.Spawn(
		system, "client-resolver", resolver,
	)

	ctx := t.Context()
	leaves := vtxtTree.Root.GetLeafNodes()

	// Monitor all VTXOs.
	for i, leaf := range leaves {
		leafTxid, err := leaf.TXID()
		require.NoError(t, err)

		// Find VTXO output.
		var vtxoOutput *wire.TxOut
		var vtxoIndex uint32
		for idx, out := range leaf.Outputs {
			if out.Value > 0 {
				vtxoOutput = out
				vtxoIndex = uint32(idx)

				break
			}
		}
		require.NotNil(t, vtxoOutput)

		vtxoOutpoint := wire.OutPoint{
			Hash:  leafTxid,
			Index: vtxoIndex,
		}

		noNotify := fn.None[actor.TellOnlyRef[ClientResolverEvent]]()
		req := &MonitorVTXORequest{
			VTXOOutpoint: vtxoOutpoint,
			VTXOOutput:   vtxoOutput,
			TreePath:     vtxtTree,
			ExitDelay:    cfg.exitDelay,
			HeightHint:   100,
			NotifyActor:  noNotify,
		}

		future := resolverRef.Ask(ctx, req)
		result := future.Await(ctx)

		require.True(t, result.IsOk(), "VTXO %d should register", i)
	}
}

// TestIntegrationVTXOTreeCoSigners tests that cosigners are properly tracked
// at each tree level.
func TestIntegrationVTXOTreeCoSigners(t *testing.T) {
	t.Parallel()

	cfg := newIntegrationTestConfig()

	// Create a 4-VTXO tree.
	amounts := []btcutil.Amount{400000, 300000, 200000, 100000}
	vtxtTree, users := createRealVTXOTree(t, cfg, amounts)

	// Root should have all cosigners (operator + all users).
	rootCoSigners := vtxtTree.Root.CoSigners
	require.Len(t, rootCoSigners, len(users)+1)

	// Verify operator is in root cosigners.
	hasOperator := false
	for _, cs := range rootCoSigners {
		if cs.IsEqual(cfg.operatorKey) {
			hasOperator = true

			break
		}
	}
	require.True(t, hasOperator, "operator should be in root")

	// Verify all users are in root cosigners.
	for _, user := range users {
		found := false
		for _, cs := range rootCoSigners {
			if cs.IsEqual(user.pubKey) {
				found = true

				break
			}
		}
		require.True(t, found, "user should be in root")
	}

	// Leaf nodes have fewer cosigners (operator + that leaf's user).
	leaves := vtxtTree.Root.GetLeafNodes()
	for _, leaf := range leaves {
		require.Len(t, leaf.CoSigners, 2)
	}
}

// TestIntegrationVTXOAmountsPreserved tests that VTXO amounts are correctly
// reflected in the tree outputs.
func TestIntegrationVTXOAmountsPreserved(t *testing.T) {
	t.Parallel()

	cfg := newIntegrationTestConfig()

	// Create tree with specific amounts.
	amounts := []btcutil.Amount{500000, 400000, 300000}
	vtxtTree, _ := createRealVTXOTree(t, cfg, amounts)

	// Get leaves and verify amounts.
	leaves := vtxtTree.Root.GetLeafNodes()
	require.Len(t, leaves, 3)

	// Collect all leaf amounts.
	var foundAmounts []btcutil.Amount
	for _, leaf := range leaves {
		for _, out := range leaf.Outputs {
			if out.Value > 0 {
				foundAmounts = append(
					foundAmounts,
					btcutil.Amount(out.Value),
				)
			}
		}
	}

	// Verify all expected amounts are present.
	require.Len(t, foundAmounts, 3)
	for _, expectedAmt := range amounts {
		found := false
		for _, foundAmt := range foundAmounts {
			if foundAmt == expectedAmt {
				found = true

				break
			}
		}
		require.True(t, found, "expected amount %d", expectedAmt)
	}
}

// ---------------------------------------------------------------------------
// Integration Tests - Unroll and Recovery
// ---------------------------------------------------------------------------

// createTestSignature creates a test schnorr signature for testing.
func createTestSignature(t *testing.T) *schnorr.Signature {
	t.Helper()
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	msg := chainhash.HashH([]byte("test message"))
	sig, err := schnorr.Sign(privKey, msg[:])
	require.NoError(t, err)

	return sig
}

// signAllNodes adds test signatures to all nodes in a tree.
func signAllNodes(t *testing.T, vtxtTree *tree.Tree) {
	t.Helper()

	sig := createTestSignature(t)
	for node := range vtxtTree.Root.NodesIter() {
		node.AddSignature(sig)
	}
}

// TestIntegrationInitiateUnroll tests broadcasting unroll transactions
// for a signed VTXO tree.
func TestIntegrationInitiateUnroll(t *testing.T) {
	t.Parallel()

	cfg := newIntegrationTestConfig()

	// Create a 3-VTXO tree and sign all nodes.
	amounts := []btcutil.Amount{500000, 400000, 300000}
	vtxtTree, users := createRealVTXOTree(t, cfg, amounts)

	// Sign all nodes so they can be broadcast.
	signAllNodes(t, vtxtTree)

	// Setup test system.
	system, chainSourceRef, backend := setupTestSystem(t)
	defer func() { _ = system.Shutdown(t.Context()) }()

	signer := &mockSigner{}
	log := testLogger{}

	resolver := NewClientResolverActor(
		chainSourceRef, system, signer, log,
	)
	resolverRef := ClientResolverKey.Spawn(
		system, "client-resolver", resolver,
	)

	ctx := t.Context()

	// Initiate unroll for first user.
	req := &InitiateUnrollRequest{
		TreePath:    vtxtTree,
		CoSignerKey: users[0].pubKey,
	}

	future := resolverRef.Ask(ctx, req)
	result := future.Await(ctx)

	require.True(t, result.IsOk(), "unroll should succeed")

	// Verify response.
	resp, err := result.Unpack()
	require.NoError(t, err)

	unrollResp, ok := resp.(*InitiateUnrollResponse)
	require.True(t, ok)

	// Should have broadcast transactions from root to leaf.
	require.NotEmpty(t, unrollResp.BroadcastTxids)
	require.NotEqual(t, wire.OutPoint{}, unrollResp.LeafOutpoint)

	// Verify transactions were actually broadcast.
	require.GreaterOrEqual(t, len(backend.broadcastTxs), 1)
}

// TestIntegrationRecoverVTXO tests CSV timeout recovery for a VTXO.
func TestIntegrationRecoverVTXO(t *testing.T) {
	t.Parallel()

	cfg := newIntegrationTestConfig()

	// Create a single-VTXO tree.
	amounts := []btcutil.Amount{500000}
	vtxtTree, _ := createRealVTXOTree(t, cfg, amounts)

	// Get the VTXO outpoint and output.
	leaves := vtxtTree.Root.GetLeafNodes()
	require.Len(t, leaves, 1)

	leaf := leaves[0]
	leafTxid, err := leaf.TXID()
	require.NoError(t, err)

	// Find VTXO output.
	var vtxoOutput *wire.TxOut
	var vtxoIndex uint32
	for idx, out := range leaf.Outputs {
		if out.Value > 0 {
			vtxoOutput = out
			vtxoIndex = uint32(idx)

			break
		}
	}
	require.NotNil(t, vtxoOutput)

	vtxoOutpoint := wire.OutPoint{
		Hash:  leafTxid,
		Index: vtxoIndex,
	}

	// Setup test system.
	system, chainSourceRef, backend := setupTestSystem(t)
	defer func() { _ = system.Shutdown(t.Context()) }()

	// Create destination address.
	destPub, _ := testutils.CreateKey(100)
	destAddr, err := btcutil.NewAddressTaproot(
		destPub.SerializeCompressed()[1:],
		&chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	signer := &mockSigner{}
	log := testLogger{}

	resolver := NewClientResolverActor(
		chainSourceRef, system, signer, log,
	)
	resolverRef := ClientResolverKey.Spawn(
		system, "client-resolver", resolver,
	)

	ctx := t.Context()

	// Request CSV timeout recovery.
	req := &RecoverVTXORequest{
		VTXOOutpoint: vtxoOutpoint,
		VTXOOutput:   vtxoOutput,
		CSVTimeout:   cfg.exitDelay,
		Destination:  destAddr,
	}

	future := resolverRef.Ask(ctx, req)
	result := future.Await(ctx)

	if result.IsErr() {
		t.Logf("Recovery failed with error: %v", result.Err())
	}
	require.True(t, result.IsOk(), "recovery should succeed")

	// Verify response.
	resp, err := result.Unpack()
	require.NoError(t, err)

	recoverResp, ok := resp.(*RecoverVTXOResponse)
	require.True(t, ok)

	// Should have a recovery txid.
	require.NotEqual(t, chainhash.Hash{}, recoverResp.RecoveryTxid)

	// Verify tx was broadcast.
	require.Len(t, backend.broadcastTxs, 1)

	// Verify recovery tx spends the VTXO.
	recoveryTx := backend.broadcastTxs[0]
	require.Len(t, recoveryTx.TxIn, 1)
	require.Equal(t, vtxoOutpoint, recoveryTx.TxIn[0].PreviousOutPoint)

	// Verify CSV sequence.
	require.Equal(t, cfg.exitDelay, recoveryTx.TxIn[0].Sequence)
}

// TestIntegrationUnrollWithExtractedPath tests unrolling with an extracted
// path for a specific user.
func TestIntegrationUnrollWithExtractedPath(t *testing.T) {
	t.Parallel()

	cfg := newIntegrationTestConfig()

	// Create a 4-VTXO tree.
	amounts := []btcutil.Amount{400000, 300000, 200000, 100000}
	vtxtTree, users := createRealVTXOTree(t, cfg, amounts)

	// Sign all nodes.
	signAllNodes(t, vtxtTree)

	// Extract path for second user.
	extractedPath, err := vtxtTree.ExtractPathForCoSigner(users[1].pubKey)
	require.NoError(t, err)
	require.NotNil(t, extractedPath)

	// The extracted path should be smaller than the full tree.
	fullNodeCount := 0
	for range vtxtTree.Root.NodesIter() {
		fullNodeCount++
	}

	extractedNodeCount := 0
	for range extractedPath.Root.NodesIter() {
		extractedNodeCount++
	}

	// Extracted path should have fewer nodes (just the path to leaf).
	require.Less(t, extractedNodeCount, fullNodeCount)
}

// TestIntegrationSignedTreeStructure verifies that a signed tree maintains
// correct transaction structure.
func TestIntegrationSignedTreeStructure(t *testing.T) {
	t.Parallel()

	cfg := newIntegrationTestConfig()

	// Create and sign a tree.
	amounts := []btcutil.Amount{300000, 200000, 100000}
	vtxtTree, _ := createRealVTXOTree(t, cfg, amounts)
	signAllNodes(t, vtxtTree)

	// Verify all nodes can produce signed transactions.
	for node := range vtxtTree.Root.NodesIter() {
		signedTx, err := node.ToSignedTx()
		require.NoError(t, err)
		require.NotNil(t, signedTx)

		// Verify witness is present.
		require.Len(t, signedTx.TxIn, 1)
		require.NotEmpty(t, signedTx.TxIn[0].Witness)

		// Verify outputs are preserved.
		require.Equal(t, len(node.Outputs), len(signedTx.TxOut))
	}
}
