package db

import (
	"math/rand"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightninglabs/darepo/rounds"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// testRoundID creates a deterministic RoundID from a string seed for tests.
func testRoundID(seed string) rounds.RoundID {
	h := chainhash.HashH([]byte(seed))
	id, _ := uuid.FromBytes(h[:16])

	return rounds.RoundID(id)
}

// testOutpointHash creates a deterministic hash from a string seed for tests.
func testOutpointHash(t *testing.T, seed string) chainhash.Hash {
	t.Helper()

	return chainhash.HashH([]byte(seed))
}

// createTestFinalTx creates a simple test commitment transaction.
// The seed parameter is used to create unique transactions.
func createTestFinalTx(t *testing.T, seed string) *wire.MsgTx {
	t.Helper()

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  testOutpointHash(t, seed),
			Index: 0,
		},
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    1000000,
		PkScript: []byte{0x51, 0x20, 0xab, 0xcd},
	})

	return tx
}

// createTestVTXOTree creates a multi-level VTXO tree for testing.
func createTestVTXOTree(t *testing.T, batchOutput int) *tree.Tree {
	t.Helper()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	grandchild := &tree.Node{
		Input: wire.OutPoint{
			Hash:  chainhash.Hash{0x05},
			Index: 0,
		},
		Outputs: []*wire.TxOut{
			{Value: 200000, PkScript: []byte{0x51}},
		},
		CoSigners: []*btcec.PublicKey{
			clientKey.PubKey(),
		},
		Children: make(map[uint32]*tree.Node),
		Amount:   btcutil.Amount(200000),
	}

	child0 := &tree.Node{
		Input: wire.OutPoint{
			Hash:  chainhash.Hash{0x04},
			Index: 0,
		},
		Outputs: []*wire.TxOut{
			{Value: 300000, PkScript: []byte{0x51}},
		},
		CoSigners: []*btcec.PublicKey{
			clientKey.PubKey(),
		},
		Children: map[uint32]*tree.Node{
			0: grandchild,
		},
		Amount: btcutil.Amount(300000),
	}

	child1 := &tree.Node{
		Input: wire.OutPoint{
			Hash:  chainhash.Hash{0x06},
			Index: 1,
		},
		Outputs: []*wire.TxOut{
			{Value: 400000, PkScript: []byte{0x52}},
		},
		CoSigners: []*btcec.PublicKey{
			operatorKey.PubKey(),
		},
		Children: make(map[uint32]*tree.Node),
		Amount:   btcutil.Amount(400000),
	}

	return &tree.Tree{
		BatchOutpoint: wire.OutPoint{
			Hash:  chainhash.Hash{byte(batchOutput)},
			Index: uint32(batchOutput),
		},
		BatchOutput: &wire.TxOut{
			Value:    1000000,
			PkScript: []byte{0x51, 0x20, 0xab, 0xcd},
		},
		Root: &tree.Node{
			Input: wire.OutPoint{
				Hash:  chainhash.Hash{0x03},
				Index: 0,
			},
			Outputs: []*wire.TxOut{
				{Value: 500000, PkScript: []byte{0x00, 0x14}},
				{Value: 600000, PkScript: []byte{0x00, 0x15}},
			},
			CoSigners: []*btcec.PublicKey{
				operatorKey.PubKey(),
			},
			Children: map[uint32]*tree.Node{
				0: child0,
				1: child1,
			},
			Amount: btcutil.Amount(500000),
		},
	}
}

// createTestSingleNodeVTXOTree creates a minimal tree with just a root node.
func createTestSingleNodeVTXOTree(t *testing.T,
	batchOutput int) *tree.Tree {

	t.Helper()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return &tree.Tree{
		BatchOutpoint: wire.OutPoint{
			Hash:  chainhash.Hash{byte(batchOutput)},
			Index: uint32(batchOutput),
		},
		BatchOutput: &wire.TxOut{
			Value:    700000,
			PkScript: []byte{0x51, 0x21},
		},
		Root: &tree.Node{
			Input: wire.OutPoint{
				Hash:  chainhash.Hash{0x07},
				Index: 0,
			},
			Outputs: []*wire.TxOut{
				{Value: 700000, PkScript: []byte{0x00, 0x16}},
			},
			CoSigners: []*btcec.PublicKey{
				operatorKey.PubKey(),
			},
			Children: make(map[uint32]*tree.Node),
			Amount:   btcutil.Amount(700000),
		},
	}
}

// createRandomVTXOTree builds a deterministic random tree for test coverage.
func createRandomVTXOTree(t *testing.T, rng *rand.Rand,
	batchOutput int) *tree.Tree {

	t.Helper()

	if rng == nil {
		rng = rand.New(rand.NewSource(1))
	}

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	root := &tree.Node{
		Input: wire.OutPoint{
			Hash:  chainhash.Hash{0x11},
			Index: 0,
		},
		Outputs: []*wire.TxOut{
			{Value: 1000, PkScript: []byte{0x51}},
			{Value: 2000, PkScript: []byte{0x52}},
		},
		CoSigners: []*btcec.PublicKey{
			operatorKey.PubKey(),
		},
		Children: make(map[uint32]*tree.Node),
		Amount:   btcutil.Amount(1000),
	}

	maxDepth := 3
	buildRandomChildren(t, rng, root, 0, maxDepth)

	return &tree.Tree{
		BatchOutpoint: wire.OutPoint{
			Hash:  chainhash.Hash{byte(batchOutput)},
			Index: uint32(batchOutput),
		},
		BatchOutput: &wire.TxOut{
			Value:    1000000,
			PkScript: []byte{0x51, 0x20, 0xab, 0xcd},
		},
		Root: root,
	}
}

// buildRandomChildren recursively creates children with consistent indices.
func buildRandomChildren(t *testing.T, rng *rand.Rand,
	parent *tree.Node, depth int, maxDepth int) {

	t.Helper()

	if depth >= maxDepth {
		return
	}

	numChildren := rng.Intn(3)
	for i := 0; i < numChildren; i++ {
		childKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		child := &tree.Node{
			Input: wire.OutPoint{
				Hash:  chainhash.Hash{byte(0x20 + depth)},
				Index: uint32(i),
			},
			Outputs: []*wire.TxOut{
				{
					Value: int64(1000 + depth*100 + i),
					PkScript: []byte{
						0x51,
					},
				},
			},
			CoSigners: []*btcec.PublicKey{
				childKey.PubKey(),
			},
			Children: make(map[uint32]*tree.Node),
			Amount:   btcutil.Amount(1000 + depth*100 + i),
		}

		parent.Children[uint32(i)] = child
		buildRandomChildren(t, rng, child, depth+1, maxDepth)
	}
}

// createTestConnectorDescriptor creates a test connector descriptor.
func createTestConnectorDescriptor(
	outputIdx int, numLeaves int) *rounds.ConnectorTreeDescriptor {

	return &rounds.ConnectorTreeDescriptor{
		OutputIndex:   outputIdx,
		NumLeaves:     numLeaves,
		ForfeitScript: []byte{0x51, 0x20, byte(outputIdx)},
	}
}

// createTestClientRegistration creates a test client registration.
func createTestClientRegistration(t *testing.T,
	clientID clientconn.ClientID) *rounds.ClientRegistration {

	t.Helper()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// Create a boarding input without Tapscript (complex serialization).
	boardingInput := &rounds.BoardingInput{
		Outpoint: &wire.OutPoint{
			Hash:  testOutpointHash(t, "boarding"),
			Index: 0,
		},
		Tapscript: nil, // Skip Tapscript to avoid gob PublicKey issues
		Value:     btcutil.Amount(100000),
		PkScript:  []byte{0x51, 0x20},
		ClientKey: clientKey.PubKey(),
		OperatorKeyDesc: &keychain.KeyDescriptor{
			PubKey: operatorKey.PubKey(),
		},
	}

	// Create a leave output.
	leaveOutput := &wire.TxOut{
		Value:    50000,
		PkScript: []byte{0x00, 0x14, 0x01, 0x02},
	}

	// Create VTXO descriptors.
	vtxoKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	vtxoDescriptor := &tree.VTXODescriptor{
		Amount:      btcutil.Amount(40000),
		PkScript:    []byte{0x51, 0x20, 0x03, 0x04},
		ExitDelay:   144,
		OwnerKey:    clientKey.PubKey(),
		OperatorKey: operatorKey.PubKey(),
		SigningKey:  vtxoKey.PubKey(),
	}

	return &rounds.ClientRegistration{
		ClientID:       clientID,
		BoardingInputs: []*rounds.BoardingInput{boardingInput},
		LeaveOutputs:   []*wire.TxOut{leaveOutput},
		VTXODescriptors: map[rounds.SigningKeyHex]*tree.VTXODescriptor{
			{0x01}: vtxoDescriptor,
		},
		ForfeitInputs: []*rounds.ForfeitInput{},
	}
}

// createTestRound creates a complete test round.
func createTestRound(t *testing.T, roundID rounds.RoundID) *rounds.Round {
	t.Helper()

	// Use roundID as seed to create unique FinalTx for each round.
	finalTx := createTestFinalTx(t, roundID.String())
	vtxoTree := createTestVTXOTree(t, 0)
	applyBatchOutpointToTree(vtxoTree, finalTx, 0)

	// Create test sweep key.
	sweepKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	applySweepRootToTree(t, vtxoTree, sweepKey.PubKey(), 144)

	return &rounds.Round{
		RoundID: roundID,
		FinalTx: finalTx,
		VTXOTrees: map[int]*tree.Tree{
			0: vtxoTree,
		},
		ConnectorDescriptors: []*rounds.ConnectorTreeDescriptor{
			createTestConnectorDescriptor(1, 4),
		},
		ForfeitInfos: make(map[wire.OutPoint]*rounds.ForfeitInfo),
		ClientRegistrations: map[clientconn.ClientID]*rounds.
			ClientRegistration{
			"client1": createTestClientRegistration(t, "client1"),
		},
		SweepKey: sweepKey.PubKey(),
		CSVDelay: 144,
	}
}

// applyBatchOutpointToTree sets the batch outpoint and root input to match the
// commitment transaction output.
func applyBatchOutpointToTree(vtxoTree *tree.Tree, finalTx *wire.MsgTx,
	batchOutputIndex int) {

	if vtxoTree == nil || finalTx == nil {
		return
	}

	commitmentTxid := finalTx.TxHash()
	batchOutpoint := wire.OutPoint{
		Hash:  commitmentTxid,
		Index: uint32(batchOutputIndex),
	}
	vtxoTree.BatchOutpoint = batchOutpoint
	if vtxoTree.Root != nil {
		vtxoTree.Root.Input = batchOutpoint
	}
}

// applySweepRootToTree computes and sets the sweep tapscript root for a tree.
func applySweepRootToTree(t *testing.T, vtxoTree *tree.Tree,
	sweepKey *btcec.PublicKey, csvDelay uint32) {

	t.Helper()

	if vtxoTree == nil || sweepKey == nil {
		return
	}

	sweepTapLeaf, err := scripts.UnilateralCSVTimeoutTapLeaf(
		sweepKey, csvDelay,
	)
	require.NoError(t, err)

	sweepTapRoot := sweepTapLeaf.TapHash()
	vtxoTree.SweepTapscriptRoot = sweepTapRoot[:]
}

// createTestVTXO creates a test VTXO.
func createTestVTXO(t *testing.T, roundID rounds.RoundID,
	idx int) *rounds.VTXO {

	t.Helper()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return &rounds.VTXO{
		Outpoint: wire.OutPoint{
			Hash:  testOutpointHash(t, "vtxo"),
			Index: uint32(idx),
		},
		RoundID:          roundID,
		BatchOutputIndex: 0,
		Descriptor: &tree.VTXODescriptor{
			OwnerKey:    clientKey.PubKey(),
			OperatorKey: operatorKey.PubKey(),
			Amount:      btcutil.Amount(100000 * (idx + 1)),
			ExitDelay:   144,
			PkScript:    []byte{0x51, 0x20, byte(idx)},
		},
		OperatorKeyDesc: &keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: 1,
				Index:  uint32(idx),
			},
			PubKey: operatorKey.PubKey(),
		},
		Status: rounds.VTXOStatusPending,
	}
}
