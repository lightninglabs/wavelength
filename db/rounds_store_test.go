package db

import (
	"bytes"
	"math/rand"
	"strconv"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightninglabs/darepo/rounds"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// TestRoundStorePersist tests basic round persistence and loading.
func TestRoundStorePersist(t *testing.T) {
	t.Parallel()

	sqlStore := NewTestDB(t)
	store := NewStore(
		sqlStore.DB, sqlStore.Queries, sqlStore.Backend(),
		btclog.Disabled, clock.NewDefaultClock(),
	)
	roundStore := store.NewRoundStore()
	ctx := t.Context()

	// Create a test round.
	roundID := testRoundID("test-round-1")
	testRound := createTestRound(t, roundID)

	// Persist the round.
	err := roundStore.PersistRound(ctx, testRound)
	require.NoError(t, err)

	// Load pending rounds.
	pending, err := roundStore.LoadPendingRounds(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1)

	// Verify round fields.
	loaded := pending[0]
	require.Equal(t, testRound.RoundID, loaded.RoundID)
	require.NotNil(t, loaded.FinalTx)
	require.Equal(
		t, testRound.FinalTx.TxHash(), loaded.FinalTx.TxHash(),
	)

	// Verify VTXO trees.
	require.Len(t, loaded.VTXOTrees, 1)
	require.Contains(t, loaded.VTXOTrees, 0)
	loadedTree := loaded.VTXOTrees[0]
	require.Equal(t, loaded.FinalTx.TxHash(), loadedTree.BatchOutpoint.Hash)
	require.NotNil(t, loadedTree.Root)
	require.Equal(t, loadedTree.BatchOutpoint, loadedTree.Root.Input)
	assertTreeInputIndices(t, loadedTree.Root)
	assertTreeEqual(t, testRound.VTXOTrees[0], loadedTree)

	// Verify connector descriptors.
	require.Len(t, loaded.ConnectorDescriptors, 1)
	require.Equal(
		t, testRound.ConnectorDescriptors[0].OutputIndex,
		loaded.ConnectorDescriptors[0].OutputIndex,
	)
	require.Equal(
		t, testRound.ConnectorDescriptors[0].NumLeaves,
		loaded.ConnectorDescriptors[0].NumLeaves,
	)

	// Verify client registrations.
	require.Len(t, loaded.ClientRegistrations, 1)

	reg, ok := loaded.ClientRegistrations["client1"]
	require.True(t, ok, "should contain client1")
	require.NotNil(t, reg)
	require.Equal(t, clientconn.ClientID("client1"), reg.ClientID)
	require.Len(t, reg.BoardingInputs, 1)
	require.Len(t, reg.LeaveOutputs, 1)
}

// TestRoundStoreTreeRandomized persists a round with randomized trees and
// validates round-trip reconstruction for a variety of shapes.
func TestRoundStoreTreeRandomized(t *testing.T) {
	t.Parallel()

	sqlStore := NewTestDB(t)
	store := NewStore(
		sqlStore.DB, sqlStore.Queries, sqlStore.Backend(),
		btclog.Disabled, clock.NewDefaultClock(),
	)
	roundStore := store.NewRoundStore()
	ctx := t.Context()

	rng := rand.New(rand.NewSource(424242))

	for i := 0; i < 5; i++ {
		roundID := testRoundID(
			"random-round-" + strconv.Itoa(i),
		)
		testRound := createTestRound(t, roundID)

		tree := createRandomVTXOTree(t, rng, 0)
		applyBatchOutpointToTree(tree, testRound.FinalTx, 0)
		applySweepRootToTree(t, tree, testRound.SweepKey,
			testRound.CSVDelay)
		testRound.VTXOTrees[0] = tree

		err := roundStore.PersistRound(ctx, testRound)
		require.NoError(t, err)

		pending, err := roundStore.LoadPendingRounds(ctx)
		require.NoError(t, err)
		require.Len(t, pending, i+1)

		var loaded *rounds.Round
		for _, round := range pending {
			if round.RoundID == roundID {
				loaded = round
				break
			}
		}
		require.NotNil(t, loaded)
		require.Len(t, loaded.VTXOTrees, 1)
		assertTreeEqual(t, testRound.VTXOTrees[0],
			loaded.VTXOTrees[0])
	}
}

// assertTreeInputIndices checks that child input indices match their position
// in the parent output list.
func assertTreeInputIndices(t *testing.T, node *tree.Node) {
	t.Helper()

	for idx, child := range node.Children {
		require.Equal(t, idx, child.Input.Index)
		assertTreeInputIndices(t, child)
	}
}

// assertTreeEqual verifies that two trees match across structure and node
// contents.
func assertTreeEqual(t *testing.T, want *tree.Tree, got *tree.Tree) {
	t.Helper()

	require.NotNil(t, want)
	require.NotNil(t, got)
	require.Equal(t, want.BatchOutpoint, got.BatchOutpoint)
	require.NotNil(t, want.BatchOutput)
	require.NotNil(t, got.BatchOutput)
	require.Equal(t, want.BatchOutput.Value, got.BatchOutput.Value)
	require.Equal(t, want.BatchOutput.PkScript, got.BatchOutput.PkScript)
	require.Equal(t, want.SweepTapscriptRoot, got.SweepTapscriptRoot)
	assertNodeEqual(t, want.Root, got.Root)
}

// assertNodeEqual checks node fields, outputs, cosigners, and children.
func assertNodeEqual(t *testing.T, want *tree.Node, got *tree.Node) {
	t.Helper()

	require.NotNil(t, want)
	require.NotNil(t, got)
	require.Equal(t, want.Input, got.Input)
	require.Equal(t, want.Amount, got.Amount)
	require.Len(t, got.Outputs, len(want.Outputs))
	for i := range want.Outputs {
		require.Equal(t, want.Outputs[i].Value, got.Outputs[i].Value)
		require.Equal(t, want.Outputs[i].PkScript,
			got.Outputs[i].PkScript)
	}

	require.Len(t, got.CoSigners, len(want.CoSigners))
	for i := range want.CoSigners {
		require.Equal(t, want.CoSigners[i].SerializeCompressed(),
			got.CoSigners[i].SerializeCompressed(),
		)
	}

	if want.Signature == nil {
		require.Nil(t, got.Signature)
	} else {
		require.NotNil(t, got.Signature)
		require.Equal(t, want.Signature.Serialize(),
			got.Signature.Serialize(),
		)
	}

	if want.FinalKey == nil {
		require.Nil(t, got.FinalKey)
	} else {
		require.NotNil(t, got.FinalKey)
		require.Equal(t, want.FinalKey.SerializeCompressed(),
			got.FinalKey.SerializeCompressed(),
		)
	}

	require.Len(t, got.Children, len(want.Children))
	for idx, wantChild := range want.Children {
		gotChild, ok := got.Children[idx]
		require.True(t, ok, "missing child %d", idx)
		assertNodeEqual(t, wantChild, gotChild)
	}
}

// TestRoundStoreMarkConfirmed tests marking a round as confirmed.
func TestRoundStoreMarkConfirmed(t *testing.T) {
	t.Parallel()

	sqlStore := NewTestDB(t)
	store := NewStore(
		sqlStore.DB, sqlStore.Queries, sqlStore.Backend(),
		btclog.Disabled, clock.NewDefaultClock(),
	)
	roundStore := store.NewRoundStore()
	ctx := t.Context()

	// Create and persist a round.
	roundID := testRoundID("test-round-2")
	testRound := createTestRound(t, roundID)
	err := roundStore.PersistRound(ctx, testRound)
	require.NoError(t, err)

	// Initially should be in pending rounds.
	pending, err := roundStore.LoadPendingRounds(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1)

	// Mark as confirmed.
	blockHeight := int32(100)
	blockHash := chainhash.Hash{0x02}
	err = roundStore.MarkRoundConfirmed(
		ctx, roundID, blockHeight, blockHash,
	)
	require.NoError(t, err)

	// Should no longer be in pending rounds.
	pending, err = roundStore.LoadPendingRounds(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 0)
}

// TestRoundStoreMultipleRounds tests persisting and loading multiple rounds.
func TestRoundStoreMultipleRounds(t *testing.T) {
	t.Parallel()

	sqlStore := NewTestDB(t)
	store := NewStore(
		sqlStore.DB, sqlStore.Queries, sqlStore.Backend(),
		btclog.Disabled, clock.NewDefaultClock(),
	)
	roundStore := store.NewRoundStore()
	ctx := t.Context()

	// Create and persist multiple rounds.
	round1 := createTestRound(t, testRoundID("round-1"))
	round2 := createTestRound(t, testRoundID("round-2"))
	round3 := createTestRound(t, testRoundID("round-3"))
	round2.VTXOTrees[0] = createTestSingleNodeVTXOTree(t, 0)
	applyBatchOutpointToTree(round2.VTXOTrees[0], round2.FinalTx, 0)

	err := roundStore.PersistRound(ctx, round1)
	require.NoError(t, err)

	err = roundStore.PersistRound(ctx, round2)
	require.NoError(t, err)

	err = roundStore.PersistRound(ctx, round3)
	require.NoError(t, err)

	// Load all pending rounds.
	pending, err := roundStore.LoadPendingRounds(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 3)

	// Verify all rounds are present.
	roundIDs := make(map[rounds.RoundID]bool)
	for _, r := range pending {
		roundIDs[r.RoundID] = true
	}
	require.True(t, roundIDs[round1.RoundID])
	require.True(t, roundIDs[round2.RoundID])
	require.True(t, roundIDs[round3.RoundID])

	// Confirm one round.
	err = roundStore.MarkRoundConfirmed(
		ctx, round2.RoundID, 100, chainhash.Hash{0x02},
	)
	require.NoError(t, err)

	// Should now have only 2 pending rounds.
	pending, err = roundStore.LoadPendingRounds(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 2)
}

// TestRoundStoreTransactionAtomicity tests that round persistence is atomic.
func TestRoundStoreTransactionAtomicity(t *testing.T) {
	t.Parallel()

	sqlStore := NewTestDB(t)
	store := NewStore(
		sqlStore.DB, sqlStore.Queries, sqlStore.Backend(),
		btclog.Disabled, clock.NewDefaultClock(),
	)
	roundStore := store.NewRoundStore()
	ctx := t.Context()

	// Create a round with invalid client registration data that will fail
	// serialization. We'll create one with a BoardingInput that has a nil
	// Outpoint, which will cause serialization to fail.
	roundID := testRoundID("test-round-atomic")
	testRound := createTestRound(t, roundID)

	// Make a client registration with a BoardingInput that has a nil
	// Outpoint, which will fail serialization.
	testRound.ClientRegistrations["bad-client"] =
		&rounds.ClientRegistration{
			ClientID: "bad-client",
			BoardingInputs: []*rounds.BoardingInput{
				{
					// Invalid: nil outpoint
					Outpoint: nil,
				},
			},
			VTXODescriptors: make(
				map[rounds.SigningKeyHex]*tree.VTXODescriptor,
			),
		}

	// Attempt to persist - should fail.
	err := roundStore.PersistRound(ctx, testRound)
	require.Error(t, err)

	// Verify no partial data was persisted.
	pending, err := roundStore.LoadPendingRounds(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 0, "no rounds should be persisted on error")
}

// TestRoundStoreEmptyCollections tests persisting rounds with empty
// collections.
func TestRoundStoreEmptyCollections(t *testing.T) {
	t.Parallel()

	sqlStore := NewTestDB(t)
	store := NewStore(
		sqlStore.DB, sqlStore.Queries, sqlStore.Backend(),
		btclog.Disabled, clock.NewDefaultClock(),
	)
	roundStore := store.NewRoundStore()
	ctx := t.Context()

	// Create a minimal round with empty collections.
	roundID := testRoundID("test-round-empty")
	finalTx := createTestFinalTx(t, "empty-round")

	// Create test sweep key.
	sweepKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	testRound := &rounds.Round{
		RoundID:              roundID,
		FinalTx:              finalTx,
		VTXOTrees:            make(map[int]*tree.Tree),
		ConnectorDescriptors: []*rounds.ConnectorTreeDescriptor{},
		ForfeitInfos: make(
			map[wire.OutPoint]*rounds.ForfeitInfo,
		),
		ClientRegistrations: make(
			map[rounds.ClientID]*rounds.ClientRegistration,
		),
		SweepKey: sweepKey.PubKey(),
		CSVDelay: 144,
	}

	// Persist the round.
	err = roundStore.PersistRound(ctx, testRound)
	require.NoError(t, err)

	// Load and verify.
	pending, err := roundStore.LoadPendingRounds(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1)

	loaded := pending[0]
	require.Equal(t, roundID, loaded.RoundID)
	require.Len(t, loaded.VTXOTrees, 0)
	require.Len(t, loaded.ConnectorDescriptors, 0)
	require.Len(t, loaded.ForfeitInfos, 0)
	require.Len(t, loaded.ClientRegistrations, 0)
}
