package db

import (
	"database/sql"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/round"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// newVTXOStoreForTest creates a new VTXOPersistenceStore and the underlying
// round store for test setup. Returns both to allow tests to set up rounds
// first (for FK constraints).
func newVTXOStoreForTest(t *testing.T) (
	*VTXOPersistenceStore, *RoundPersistenceStore, *BaseDB,
) {

	db := NewTestDB(t)

	roundDB := NewTransactionExecutor(
		db.BaseDB,
		func(tx *sql.Tx) RoundStore {
			return db.WithTx(tx)
		},
		btclog.Disabled,
	)

	roundStore := NewRoundPersistenceStore(
		roundDB, &chaincfg.RegressionNetParams,
		clock.NewDefaultClock(),
	)

	vtxoStore := NewVTXOPersistenceStore(roundDB, clock.NewDefaultClock())

	return vtxoStore, roundStore, db.BaseDB
}

// createTestVTXODescriptor creates a vtxo.Descriptor for testing. The index
// parameter generates unique outpoints and keys.
func createTestVTXODescriptor(
	t *testing.T, roundID round.RoundID, idx int,
) *vtxo.Descriptor {

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	var hash chainhash.Hash
	hash[0] = byte(idx)
	hash[1] = 0xde
	hash[2] = 0xad

	outpoint := wire.OutPoint{
		Hash:  hash,
		Index: uint32(idx),
	}

	// Create a minimal tree path for testing.
	treePath := &tree.Tree{
		BatchOutpoint: wire.OutPoint{Hash: hash, Index: 0},
		Root: &tree.Node{
			Input:     wire.OutPoint{Hash: hash, Index: 0},
			Outputs:   []*wire.TxOut{},
			CoSigners: []*btcec.PublicKey{},
			Children:  make(map[uint32]*tree.Node),
		},
	}

	// Create the commitment txid.
	var commitmentTxID chainhash.Hash
	commitmentTxID[0] = byte(idx)
	commitmentTxID[1] = 0xc0
	commitmentTxID[2] = 0xff
	commitmentTxID[3] = 0xee

	// Build the tapscript from client and operator keys.
	const exitDelay uint32 = 144
	tapscript, err := arkscript.VTXOTapScript(
		privKey.PubKey(), operatorKey.PubKey(), exitDelay,
	)
	require.NoError(t, err)

	return &vtxo.Descriptor{
		Outpoint: outpoint,
		Amount:   btcutil.Amount(100000 * (idx + 1)),
		PkScript: []byte{0x51, 0x20, byte(idx)},
		ClientKey: keychain.KeyDescriptor{
			PubKey: privKey.PubKey(),
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamily(0),
				Index:  uint32(idx),
			},
		},
		OperatorKey:    operatorKey.PubKey(),
		TapScript:      tapscript,
		TreePath:       treePath,
		RoundID:        roundID.String(),
		CommitmentTxID: commitmentTxID,
		BatchExpiry:    1000 + int32(idx*100),
		RelativeExpiry: exitDelay,
		TreeDepth:      2 + idx,
		CreatedHeight:  500 + int32(idx*10),
		Status:         vtxo.VTXOStatusLive,
	}
}

// TestVTXOPersistenceStoreSaveAndGet tests the basic save and get operations.
func TestVTXOPersistenceStoreSaveAndGet(t *testing.T) {
	t.Parallel()

	vtxoStore, roundStore, _ := newVTXOStoreForTest(t)
	ctx := t.Context()

	// Create a round to satisfy FK constraint.
	roundID := testRoundIDDB("test-round-save-get")
	testRound := createTestRound(t, roundID)
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	err := roundStore.CommitState(ctx, testRound, state)
	require.NoError(t, err)

	// Create and save a VTXO.
	desc := createTestVTXODescriptor(t, roundID, 42)
	err = vtxoStore.SaveVTXO(ctx, desc)
	require.NoError(t, err)

	// Retrieve it.
	fetched, err := vtxoStore.GetVTXO(ctx, desc.Outpoint)
	require.NoError(t, err)
	require.NotNil(t, fetched)

	// Verify fields.
	require.Equal(t, desc.Outpoint, fetched.Outpoint)
	require.Equal(t, desc.Amount, fetched.Amount)
	require.Equal(t, desc.PkScript, fetched.PkScript)
	require.Equal(t, desc.RelativeExpiry, fetched.RelativeExpiry)
	require.Equal(t, desc.RoundID, fetched.RoundID)
	require.Equal(t, desc.Status, fetched.Status)

	// Verify keys.
	require.NotNil(t, fetched.ClientKey.PubKey)
	require.NotNil(t, fetched.OperatorKey)
	require.Equal(t, desc.ClientKey.Family, fetched.ClientKey.Family)
	require.Equal(t, desc.ClientKey.Index, fetched.ClientKey.Index)

	// Verify tree path was persisted.
	require.NotNil(t, fetched.TreePath)
	require.Equal(
		t, desc.TreePath.BatchOutpoint, fetched.TreePath.BatchOutpoint,
	)
}

// TestVTXOPersistenceStoreSaveVTXOCreatesMissingRound verifies that SaveVTXO
// creates a minimal local round row for imported OOR VTXOs whose source round
// is otherwise unknown to the client.
func TestVTXOPersistenceStoreSaveVTXOCreatesMissingRound(t *testing.T) {
	t.Parallel()

	vtxoStore, _, db := newVTXOStoreForTest(t)
	ctx := t.Context()

	roundID := testRoundIDDB("test-round-imported-oor")
	desc := createTestVTXODescriptor(t, roundID, 77)

	err := vtxoStore.SaveVTXO(ctx, desc)
	require.NoError(t, err)

	row, err := db.GetRound(ctx, desc.RoundID)
	require.NoError(t, err)
	require.Equal(t, desc.RoundID, row.RoundID)
	require.Equal(t, "confirmed", row.Status)
	require.True(t, row.ConfirmationHeight.Valid)
	require.Equal(t, desc.CreatedHeight, row.ConfirmationHeight.Int32)
	require.Equal(t, desc.CommitmentTxID[:], row.CommitmentTxid)
	require.Nil(t, row.CommitmentTx)
	require.Nil(t, row.VtxtTree)

	fetched, err := vtxoStore.GetVTXO(ctx, desc.Outpoint)
	require.NoError(t, err)
	require.Equal(t, desc.RoundID, fetched.RoundID)
}

// TestVTXOPersistenceStoreSaveVTXOKeepsExistingRound verifies that SaveVTXO
// does not overwrite richer round state when the round row already exists.
func TestVTXOPersistenceStoreSaveVTXOKeepsExistingRound(t *testing.T) {
	t.Parallel()

	vtxoStore, roundStore, db := newVTXOStoreForTest(t)
	ctx := t.Context()

	roundID := testRoundIDDB("test-round-existing-preserved")
	testRound := createTestRound(t, roundID)
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	err := roundStore.CommitState(ctx, testRound, state)
	require.NoError(t, err)

	before, err := db.GetRound(ctx, roundID.String())
	require.NoError(t, err)

	desc := createTestVTXODescriptor(t, roundID, 78)
	err = vtxoStore.SaveVTXO(ctx, desc)
	require.NoError(t, err)

	after, err := db.GetRound(ctx, roundID.String())
	require.NoError(t, err)
	require.Equal(t, before.Status, after.Status)
	require.Equal(t, before.CommitmentTxid, after.CommitmentTxid)
	require.Equal(t, before.CommitmentTx, after.CommitmentTx)
	require.Equal(t, before.VtxtTree, after.VtxtTree)
}

// TestVTXOPersistenceStoreChainDepthRoundTrip verifies that a non-zero
// ChainDepth survives a save/load cycle through the database.
func TestVTXOPersistenceStoreChainDepthRoundTrip(t *testing.T) {
	t.Parallel()

	vtxoStore, roundStore, _ := newVTXOStoreForTest(t)
	ctx := t.Context()

	roundID := testRoundIDDB("test-round-chain-depth")
	testRound := createTestRound(t, roundID)
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	err := roundStore.CommitState(ctx, testRound, state)
	require.NoError(t, err)

	// Create a descriptor with a non-zero chain depth (simulating an
	// OOR VTXO that is 3 hops from the on-chain commitment).
	desc := createTestVTXODescriptor(t, roundID, 99)
	desc.ChainDepth = 3

	err = vtxoStore.SaveVTXO(ctx, desc)
	require.NoError(t, err)

	fetched, err := vtxoStore.GetVTXO(ctx, desc.Outpoint)
	require.NoError(t, err)
	require.Equal(t, 3, fetched.ChainDepth)

	// Also verify via ListLiveVTXOs.
	live, err := vtxoStore.ListLiveVTXOs(ctx)
	require.NoError(t, err)
	require.Len(t, live, 1)
	require.Equal(t, 3, live[0].ChainDepth)
}

// TestVTXOPersistenceStoreListLiveVTXOs tests that ListLiveVTXOs returns only
// VTXOs in non-terminal states.
func TestVTXOPersistenceStoreListLiveVTXOs(t *testing.T) {
	t.Parallel()

	vtxoStore, roundStore, _ := newVTXOStoreForTest(t)
	ctx := t.Context()

	// Create a round to satisfy foreign key constraint.
	roundID := testRoundIDDB("test-round-live-vtxos")
	testRound := createTestRound(t, roundID)
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	err := roundStore.CommitState(ctx, testRound, state)
	require.NoError(t, err)

	// Create and save three test VTXOs.
	vtxo1 := createTestVTXODescriptor(t, roundID, 1)
	vtxo2 := createTestVTXODescriptor(t, roundID, 2)
	vtxo3 := createTestVTXODescriptor(t, roundID, 3)

	err = vtxoStore.SaveVTXO(ctx, vtxo1)
	require.NoError(t, err)
	err = vtxoStore.SaveVTXO(ctx, vtxo2)
	require.NoError(t, err)
	err = vtxoStore.SaveVTXO(ctx, vtxo3)
	require.NoError(t, err)

	// Verify all three VTXOs are returned as live.
	liveVTXOs, err := vtxoStore.ListLiveVTXOs(ctx)
	require.NoError(t, err)
	require.Len(t, liveVTXOs, 3)

	// Mark vtxo2 as Forfeited (terminal state).
	err = vtxoStore.UpdateVTXOStatus(
		ctx, vtxo2.Outpoint, vtxo.VTXOStatusForfeited,
	)
	require.NoError(t, err)

	// Verify vtxo2 is no longer in the live list.
	liveVTXOs, err = vtxoStore.ListLiveVTXOs(ctx)
	require.NoError(t, err)
	require.Len(t, liveVTXOs, 2, "forfeited VTXO should be excluded")

	// Verify the correct VTXOs are returned.
	outpoints := make(map[wire.OutPoint]bool)
	for _, v := range liveVTXOs {
		outpoints[v.Outpoint] = true
	}
	require.True(t, outpoints[vtxo1.Outpoint], "vtxo1 should be live")
	require.False(t, outpoints[vtxo2.Outpoint], "vtxo2 should NOT be live")
	require.True(t, outpoints[vtxo3.Outpoint], "vtxo3 should be live")

	// Mark vtxo3 as RefreshRequested (non-terminal, should still be live).
	err = vtxoStore.UpdateVTXOStatus(
		ctx, vtxo3.Outpoint, vtxo.VTXOStatusPendingForfeit,
	)
	require.NoError(t, err)

	liveVTXOs, err = vtxoStore.ListLiveVTXOs(ctx)
	require.NoError(t, err)
	require.Len(t, liveVTXOs, 2, "RefreshRequested is non-terminal")
}

// TestVTXOPersistenceStoreStatusTransitions tests the status update methods.
func TestVTXOPersistenceStoreStatusTransitions(t *testing.T) {
	t.Parallel()

	vtxoStore, roundStore, _ := newVTXOStoreForTest(t)
	ctx := t.Context()

	// Create a round to satisfy FK constraint.
	roundID := testRoundIDDB("test-round-status")
	testRound := createTestRound(t, roundID)
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	err := roundStore.CommitState(ctx, testRound, state)
	require.NoError(t, err)

	// Create and save a VTXO.
	desc := createTestVTXODescriptor(t, roundID, 1)
	err = vtxoStore.SaveVTXO(ctx, desc)
	require.NoError(t, err)

	// Verify initial status is Live.
	fetched, err := vtxoStore.GetVTXO(ctx, desc.Outpoint)
	require.NoError(t, err)
	require.Equal(t, vtxo.VTXOStatusLive, fetched.Status)

	// Transition to RefreshRequested.
	err = vtxoStore.UpdateVTXOStatus(
		ctx, desc.Outpoint, vtxo.VTXOStatusPendingForfeit,
	)
	require.NoError(t, err)

	fetched, err = vtxoStore.GetVTXO(ctx, desc.Outpoint)
	require.NoError(t, err)
	require.Equal(t, vtxo.VTXOStatusPendingForfeit, fetched.Status)

	// Transition to Forfeiting via MarkForfeiting.
	forfeitRoundID := testRoundIDDB("forfeit-round")
	err = vtxoStore.MarkForfeiting(
		ctx, desc.Outpoint, forfeitRoundID.String(), nil,
	)
	require.NoError(t, err)

	fetched, err = vtxoStore.GetVTXO(ctx, desc.Outpoint)
	require.NoError(t, err)
	require.Equal(t, vtxo.VTXOStatusForfeiting, fetched.Status)

	// Transition to Forfeited via MarkForfeited.
	forfeitTxID := chainhash.Hash{0xab, 0xcd}
	err = vtxoStore.MarkForfeited(ctx, desc.Outpoint, forfeitTxID)
	require.NoError(t, err)

	fetched, err = vtxoStore.GetVTXO(ctx, desc.Outpoint)
	require.NoError(t, err)
	require.Equal(t, vtxo.VTXOStatusForfeited, fetched.Status)

	// Verify VTXO is no longer in live list (terminal state).
	liveVTXOs, err := vtxoStore.ListLiveVTXOs(ctx)
	require.NoError(t, err)
	require.Len(t, liveVTXOs, 0)
}

// TestVTXOPersistenceStoreSpentStatusSynchronizesSpentFlag verifies that
// setting status=Spent via UpdateVTXOStatus also marks spent=true and removes
// the VTXO from round unspent listings.
func TestVTXOPersistenceStoreSpentStatusSynchronizesSpentFlag(t *testing.T) {
	t.Parallel()

	vtxoStore, roundStore, db := newVTXOStoreForTest(t)
	ctx := t.Context()

	roundID := testRoundIDDB("test-round-status-spent-sync")
	testRound := createTestRound(t, roundID)
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	err := roundStore.CommitState(ctx, testRound, state)
	require.NoError(t, err)

	desc := createTestVTXODescriptor(t, roundID, 11)
	err = vtxoStore.SaveVTXO(ctx, desc)
	require.NoError(t, err)

	listed, err := roundStore.ListVTXOs(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 1)

	err = vtxoStore.UpdateVTXOStatus(
		ctx, desc.Outpoint, vtxo.VTXOStatusSpent,
	)
	require.NoError(t, err)

	row, err := db.GetVTXO(ctx, sqlc.GetVTXOParams{
		OutpointHash:  desc.Outpoint.Hash[:],
		OutpointIndex: int32(desc.Outpoint.Index),
	})
	require.NoError(t, err)
	require.Equal(t, int32(vtxo.VTXOStatusSpent), row.Status)
	require.True(t, row.Spent)

	listed, err = roundStore.ListVTXOs(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 0)
}

// TestVTXOPersistenceStoreForfeitTxPersistence tests that MarkForfeiting
// correctly persists the forfeit transaction and GetForfeitTx retrieves it.
func TestVTXOPersistenceStoreForfeitTxPersistence(t *testing.T) {
	t.Parallel()

	vtxoStore, roundStore, _ := newVTXOStoreForTest(t)
	ctx := t.Context()

	// Create a round to satisfy FK constraint.
	roundID := testRoundIDDB("test-round-forfeit-tx")
	testRound := createTestRound(t, roundID)
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	err := roundStore.CommitState(ctx, testRound, state)
	require.NoError(t, err)

	// Create and save a VTXO.
	desc := createTestVTXODescriptor(t, roundID, 1)
	err = vtxoStore.SaveVTXO(ctx, desc)
	require.NoError(t, err)

	// Initially, no forfeit tx should be stored.
	forfeitTx, err := vtxoStore.GetForfeitTx(ctx, desc.Outpoint)
	require.NoError(t, err)
	require.Nil(t, forfeitTx, "no forfeit tx should exist initially")

	// Create a test forfeit transaction.
	testForfeitTx := wire.NewMsgTx(2)
	testForfeitTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: desc.Outpoint,
	})
	testForfeitTx.AddTxOut(&wire.TxOut{
		Value:    int64(desc.Amount) - 1000,
		PkScript: []byte{0x00, 0x14, 0xab, 0xcd},
	})

	// Mark forfeiting with the forfeit transaction.
	forfeitRoundID := testRoundIDDB("forfeit-round")
	err = vtxoStore.MarkForfeiting(
		ctx, desc.Outpoint, forfeitRoundID.String(), testForfeitTx,
	)
	require.NoError(t, err)

	// Verify status changed to Forfeiting.
	fetched, err := vtxoStore.GetVTXO(ctx, desc.Outpoint)
	require.NoError(t, err)
	require.Equal(t, vtxo.VTXOStatusForfeiting, fetched.Status)

	// Retrieve the forfeit transaction.
	retrievedForfeitTx, err := vtxoStore.GetForfeitTx(ctx, desc.Outpoint)
	require.NoError(t, err)
	require.NotNil(t, retrievedForfeitTx)

	// Verify the transaction matches.
	require.Equal(t, testForfeitTx.TxHash(), retrievedForfeitTx.TxHash())
	require.Len(t, retrievedForfeitTx.TxIn, 1)
	require.Equal(
		t, desc.Outpoint, retrievedForfeitTx.TxIn[0].PreviousOutPoint,
	)
}

// TestVTXOPersistenceStoreDeleteVTXO tests the DeleteVTXO method.
func TestVTXOPersistenceStoreDeleteVTXO(t *testing.T) {
	t.Parallel()

	vtxoStore, roundStore, _ := newVTXOStoreForTest(t)
	ctx := t.Context()

	// Create a round to satisfy FK constraint.
	roundID := testRoundIDDB("test-round-delete")
	testRound := createTestRound(t, roundID)
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	err := roundStore.CommitState(ctx, testRound, state)
	require.NoError(t, err)

	// Create and save two VTXOs.
	vtxo1 := createTestVTXODescriptor(t, roundID, 1)
	vtxo2 := createTestVTXODescriptor(t, roundID, 2)

	err = vtxoStore.SaveVTXO(ctx, vtxo1)
	require.NoError(t, err)
	err = vtxoStore.SaveVTXO(ctx, vtxo2)
	require.NoError(t, err)

	// Verify both exist.
	liveVTXOs, err := vtxoStore.ListLiveVTXOs(ctx)
	require.NoError(t, err)
	require.Len(t, liveVTXOs, 2)

	// Delete vtxo1.
	err = vtxoStore.DeleteVTXO(ctx, vtxo1.Outpoint)
	require.NoError(t, err)

	// Verify vtxo1 is gone.
	liveVTXOs, err = vtxoStore.ListLiveVTXOs(ctx)
	require.NoError(t, err)
	require.Len(t, liveVTXOs, 1)
	require.Equal(t, vtxo2.Outpoint, liveVTXOs[0].Outpoint)

	// Attempting to get the deleted VTXO should fail.
	_, err = vtxoStore.GetVTXO(ctx, vtxo1.Outpoint)
	require.Error(t, err, "getting deleted VTXO should fail")
}

// TestVTXOPersistenceStoreMarkForfeitedRecordsTxID tests that MarkForfeited
// correctly records the forfeit transaction ID.
func TestVTXOPersistenceStoreMarkForfeitedRecordsTxID(t *testing.T) {
	t.Parallel()

	vtxoStore, roundStore, db := newVTXOStoreForTest(t)
	ctx := t.Context()

	// Create a round to satisfy FK constraint.
	roundID := testRoundIDDB("test-round-forfeited")
	testRound := createTestRound(t, roundID)
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	err := roundStore.CommitState(ctx, testRound, state)
	require.NoError(t, err)

	// Create and save a VTXO.
	desc := createTestVTXODescriptor(t, roundID, 1)
	err = vtxoStore.SaveVTXO(ctx, desc)
	require.NoError(t, err)

	// Go through the forfeit flow: first mark forfeiting.
	forfeitRoundID := testRoundIDDB("forfeit-round")
	err = vtxoStore.MarkForfeiting(
		ctx, desc.Outpoint, forfeitRoundID.String(), nil,
	)
	require.NoError(t, err)

	// Now mark as forfeited with a txid.
	forfeitTxID := chainhash.Hash{0xde, 0xad, 0xbe, 0xef}
	err = vtxoStore.MarkForfeited(ctx, desc.Outpoint, forfeitTxID)
	require.NoError(t, err)

	// Verify via raw db query that the forfeit_txid was stored.
	row, err := db.GetVTXO(ctx, sqlc.GetVTXOParams{
		OutpointHash:  desc.Outpoint.Hash[:],
		OutpointIndex: int32(desc.Outpoint.Index),
	})
	require.NoError(t, err)
	require.Equal(t, int32(vtxo.VTXOStatusForfeited), row.Status)
	require.Equal(t, forfeitTxID[:], row.ForfeitTxid)
}

// TestVTXOPersistenceStoreMultipleVTXOsLifecycle tests a realistic scenario
// with multiple VTXOs going through different lifecycle paths.
func TestVTXOPersistenceStoreMultipleVTXOsLifecycle(t *testing.T) {
	t.Parallel()

	vtxoStore, roundStore, _ := newVTXOStoreForTest(t)
	ctx := t.Context()

	// Create a round to satisfy FK constraint.
	roundID := testRoundIDDB("test-round-lifecycle")
	testRound := createTestRound(t, roundID)
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	err := roundStore.CommitState(ctx, testRound, state)
	require.NoError(t, err)

	// Create 5 VTXOs simulating different scenarios.
	vtxos := make([]*vtxo.Descriptor, 5)
	for i := 0; i < 5; i++ {
		vtxos[i] = createTestVTXODescriptor(t, roundID, i)
		err = vtxoStore.SaveVTXO(ctx, vtxos[i])
		require.NoError(t, err)
	}

	// All 5 should be live initially.
	liveVTXOs, err := vtxoStore.ListLiveVTXOs(ctx)
	require.NoError(t, err)
	require.Len(t, liveVTXOs, 5)

	// VTXO 0: stays live (no changes).
	// VTXO 1: goes to RefreshRequested (still live).
	err = vtxoStore.UpdateVTXOStatus(
		ctx, vtxos[1].Outpoint, vtxo.VTXOStatusPendingForfeit,
	)
	require.NoError(t, err)

	// VTXO 2: goes through full forfeit flow.
	forfeitRoundID := testRoundIDDB("forfeit-round-2")
	err = vtxoStore.MarkForfeiting(
		ctx, vtxos[2].Outpoint, forfeitRoundID.String(), nil,
	)
	require.NoError(t, err)
	err = vtxoStore.MarkForfeited(
		ctx, vtxos[2].Outpoint, chainhash.Hash{0x02},
	)
	require.NoError(t, err)

	// VTXO 3: goes to Forfeiting (still counted as live for recovery).
	err = vtxoStore.MarkForfeiting(
		ctx, vtxos[3].Outpoint, forfeitRoundID.String(), nil,
	)
	require.NoError(t, err)

	// VTXO 4: gets deleted.
	err = vtxoStore.DeleteVTXO(ctx, vtxos[4].Outpoint)
	require.NoError(t, err)

	// Check live VTXOs: should be 0, 1, 3 (not 2 which is Forfeited, not 4
	// which is deleted).
	liveVTXOs, err = vtxoStore.ListLiveVTXOs(ctx)
	require.NoError(t, err)
	require.Len(t, liveVTXOs, 3)

	// Verify the expected outpoints.
	outpoints := make(map[wire.OutPoint]bool)
	for _, v := range liveVTXOs {
		outpoints[v.Outpoint] = true
	}
	require.True(t, outpoints[vtxos[0].Outpoint], "vtxo 0 live")
	require.True(t, outpoints[vtxos[1].Outpoint], "vtxo 1 live")
	require.False(t, outpoints[vtxos[2].Outpoint], "vtxo 2 NOT live")
	require.True(t, outpoints[vtxos[3].Outpoint], "vtxo 3 live")
	require.False(t, outpoints[vtxos[4].Outpoint], "vtxo 4 NOT live")

	// Verify statuses.
	fetched0, err := vtxoStore.GetVTXO(ctx, vtxos[0].Outpoint)
	require.NoError(t, err)
	require.Equal(t, vtxo.VTXOStatusLive, fetched0.Status)

	fetched1, err := vtxoStore.GetVTXO(ctx, vtxos[1].Outpoint)
	require.NoError(t, err)
	require.Equal(t, vtxo.VTXOStatusPendingForfeit, fetched1.Status)

	fetched2, err := vtxoStore.GetVTXO(ctx, vtxos[2].Outpoint)
	require.NoError(t, err)
	require.Equal(t, vtxo.VTXOStatusForfeited, fetched2.Status)

	fetched3, err := vtxoStore.GetVTXO(ctx, vtxos[3].Outpoint)
	require.NoError(t, err)
	require.Equal(t, vtxo.VTXOStatusForfeiting, fetched3.Status)
}

// TestVTXOPersistenceStoreMetadataPersistence tests that the new metadata
// fields (BatchExpiry, TreeDepth, CreatedHeight, CommitmentTxID) are correctly
// persisted and retrieved, and that TapScript is correctly reconstructed.
func TestVTXOPersistenceStoreMetadataPersistence(t *testing.T) {
	t.Parallel()

	vtxoStore, roundStore, _ := newVTXOStoreForTest(t)
	ctx := t.Context()

	// Create a round to satisfy FK constraint.
	roundID := testRoundIDDB("test-round-metadata")
	testRound := createTestRound(t, roundID)
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	err := roundStore.CommitState(ctx, testRound, state)
	require.NoError(t, err)

	// Create and save a VTXO with full metadata.
	desc := createTestVTXODescriptor(t, roundID, 42)
	err = vtxoStore.SaveVTXO(ctx, desc)
	require.NoError(t, err)

	// Retrieve it.
	fetched, err := vtxoStore.GetVTXO(ctx, desc.Outpoint)
	require.NoError(t, err)
	require.NotNil(t, fetched)

	// Verify the new metadata fields are persisted correctly.
	require.Equal(
		t, desc.BatchExpiry, fetched.BatchExpiry,
		"BatchExpiry should be persisted",
	)
	require.Equal(
		t, desc.TreeDepth, fetched.TreeDepth,
		"TreeDepth should be persisted",
	)
	require.Equal(
		t, desc.CreatedHeight, fetched.CreatedHeight,
		"CreatedHeight should be persisted",
	)
	require.Equal(
		t, desc.CommitmentTxID, fetched.CommitmentTxID,
		"CommitmentTxID should be persisted",
	)

	// Verify TapScript is reconstructed correctly. The tapscript should be
	// derived from the client/operator keys and exit delay on retrieval.
	require.NotNil(
		t, fetched.TapScript, "TapScript should be reconstructed",
	)
	require.NotNil(t, desc.TapScript, "original TapScript should exist")

	// Verify that the reconstructed TapScript has the same structure by
	// checking it has leaves.
	require.NotEmpty(
		t, fetched.TapScript.Leaves,
		"reconstructed TapScript should have leaves",
	)
	require.Equal(
		t, len(desc.TapScript.Leaves), len(fetched.TapScript.Leaves),
		"reconstructed TapScript should have same number of leaves",
	)
}

// TestVTXOPersistenceStoreMetadataUpdate tests that metadata fields can be
// updated via the ON CONFLICT DO UPDATE clause when a VTXO is inserted twice
// (first with defaults from round store, then with full metadata).
func TestVTXOPersistenceStoreMetadataUpdate(t *testing.T) {
	t.Parallel()

	vtxoStore, roundStore, db := newVTXOStoreForTest(t)
	ctx := t.Context()

	// Create a round to satisfy FK constraint.
	roundID := testRoundIDDB("test-round-metadata-update")
	testRound := createTestRound(t, roundID)
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	err := roundStore.CommitState(ctx, testRound, state)
	require.NoError(t, err)

	// Create a VTXO with full metadata.
	desc := createTestVTXODescriptor(t, roundID, 99)

	// Simulate the round store inserting first with default/zero metadata.
	// This mimics what happens when SaveVTXOs is called from round
	// transitions before the VTXO manager creates full Descriptors.
	clientVTXO := &round.ClientVTXO{
		Outpoint:    desc.Outpoint,
		Amount:      desc.Amount,
		PkScript:    desc.PkScript,
		Expiry:      desc.RelativeExpiry,
		ClientKey:   desc.ClientKey,
		OperatorKey: desc.OperatorKey,
		TreePath:    desc.TreePath,
		RoundID:     fn.Some(roundID),
	}
	err = roundStore.SaveVTXOs(ctx, []*round.ClientVTXO{clientVTXO})
	require.NoError(t, err)

	// Verify the VTXO was inserted with zero metadata.
	row, err := db.GetVTXO(ctx, sqlc.GetVTXOParams{
		OutpointHash:  desc.Outpoint.Hash[:],
		OutpointIndex: int32(desc.Outpoint.Index),
	})
	require.NoError(t, err)
	require.Equal(
		t, int32(0), row.BatchExpiry, "initial BatchExpiry should be 0",
	)
	require.Equal(
		t, int32(0), row.TreeDepth, "initial TreeDepth should be 0",
	)
	require.Equal(
		t, int32(0), row.CreatedHeight,
		"initial CreatedHeight should be 0",
	)

	// Now insert again with full metadata (simulates VTXO manager saving).
	err = vtxoStore.SaveVTXO(ctx, desc)
	require.NoError(t, err)

	// Verify the metadata was updated.
	row, err = db.GetVTXO(ctx, sqlc.GetVTXOParams{
		OutpointHash:  desc.Outpoint.Hash[:],
		OutpointIndex: int32(desc.Outpoint.Index),
	})
	require.NoError(t, err)
	require.Equal(
		t, desc.BatchExpiry, row.BatchExpiry,
		"BatchExpiry should be updated",
	)
	require.Equal(
		t, int32(desc.TreeDepth), row.TreeDepth,
		"TreeDepth should be updated",
	)
	require.Equal(
		t, desc.CreatedHeight, row.CreatedHeight,
		"CreatedHeight should be updated",
	)
	require.Equal(
		t, desc.CommitmentTxID[:], row.CommitmentTxid,
		"CommitmentTxid should be updated",
	)
}
