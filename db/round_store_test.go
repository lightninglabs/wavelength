package db

import (
	"bytes"
	"database/sql"
	"sort"
	"testing"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/round"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// testRoundIDDB creates a deterministic RoundID from a string seed for tests.
func testRoundIDDB(seed string) round.RoundID {
	h := chainhash.HashH([]byte(seed))
	id, _ := uuid.FromBytes(h[:16])

	return round.RoundID(id)
}

// newRoundStoreForTest creates a new RoundPersistenceStore for testing.
func newRoundStoreForTest(t *testing.T) (*RoundPersistenceStore, *BaseDB) {
	db := NewTestDB(t)

	roundDB := NewTransactionExecutor(
		db.BaseDB,
		func(tx *sql.Tx) RoundStore {
			return db.WithTx(tx)
		},
		btclog.Disabled,
	)

	store := NewRoundPersistenceStore(
		roundDB, &chaincfg.RegressionNetParams, clock.NewDefaultClock(),
	)

	return store, db.BaseDB
}

// createTestRound creates a test round with minimal data.
func createTestRound(t *testing.T, roundID round.RoundID) *round.Round {
	// Create a simple commitment transaction as a PSBT.
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{0x01},
			Index: 0,
		},
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    1000000,
		PkScript: []byte{0x51, 0x20, 0xab, 0xcd},
	})

	commitTx, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)

	// Create a simple tree.
	pubKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	vtxtTree := &tree.Tree{
		BatchOutpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				0x02,
			},
			Index: 0,
		},
		BatchOutput: &wire.TxOut{
			Value: 1000000,
			PkScript: []byte{
				0x51,
				0x20,
			},
		},
		Root: &tree.Node{
			Input: wire.OutPoint{
				Hash: chainhash.Hash{
					0x03,
				},
				Index: 0,
			},
			Outputs: []*wire.TxOut{
				{
					Value: 500000,
					PkScript: []byte{
						0x00,
						0x14,
					},
				},
			},
			CoSigners: []*btcec.PublicKey{
				pubKey.PubKey(),
			},
			Children: make(map[uint32]*tree.Node),
		},
	}

	return &round.Round{
		RoundID:       roundID,
		StartHeight:   100, // Test starting block height.
		CommitmentTx:  fn.Some(commitTx),
		VTXOTreePaths: fn.Some(map[int]*tree.Tree{0: vtxtTree}),
	}
}

// createTestClientVTXO creates a test ClientVTXO.
func createTestClientVTXO(
	t *testing.T, roundID round.RoundID, idx int,
) *round.ClientVTXO {

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	var hash chainhash.Hash
	hash[0] = byte(idx)

	return &round.ClientVTXO{
		Outpoint: wire.OutPoint{
			Hash:  hash,
			Index: uint32(idx),
		},
		Amount: btcutil.Amount(100000 * (idx + 1)),
		PolicyTemplate: func() []byte {
			policy, err := arkscript.EncodeStandardVTXOTemplate(
				privKey.PubKey(), operatorKey.PubKey(), 144,
			)
			require.NoError(t, err)

			return policy
		}(),
		PkScript: []byte{
			0x51,
			0x20,
			byte(idx),
		},
		Expiry: 144,
		OwnerKey: keychain.KeyDescriptor{
			PubKey: privKey.PubKey(),
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamily(0),
				Index:  uint32(idx),
			},
		},
		OperatorKey: operatorKey.PubKey(),
		Ancestry: []types.Ancestry{{
			TreePath: &tree.Tree{
				BatchOutpoint: wire.OutPoint{
					Hash: hash, Index: 0,
				},
				Root: &tree.Node{
					Input: wire.OutPoint{
						Hash: hash, Index: 0,
					},
					Outputs:   []*wire.TxOut{},
					CoSigners: []*btcec.PublicKey{},
					Children:  make(map[uint32]*tree.Node),
				},
			},
		}},
		RoundID: fn.Some[round.RoundID](roundID),
	}
}

// TestRoundStoreCommitAndFetch tests the basic commit and fetch flow.
func TestRoundStoreCommitAndFetch(t *testing.T) {
	t.Parallel()

	store, _ := newRoundStoreForTest(t)
	ctx := t.Context()

	// Create a test round.
	testRound := createTestRound(t, testRoundIDDB("test-round-1"))

	// Create a test FSM state.
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	testRound.CommitmentTx.WhenSome(func(packet *psbt.Packet) {
		state.CommitmentTx = packet
	})
	testRound.VTXOTreePaths.WhenSome(func(treePaths map[int]*tree.Tree) {
		state.VTXOTreePaths = treePaths
	})

	// Commit the state.
	err := store.CommitState(ctx, testRound, state)
	require.NoError(t, err)

	// Fetch the state.
	fetchedRound, fetchedState, err := store.FetchState(
		ctx, testRound.RoundID,
	)
	require.NoError(t, err)
	require.NotNil(t, fetchedRound)
	require.NotNil(t, fetchedState)

	// Verify round fields.
	require.Equal(t, testRound.RoundID, fetchedRound.RoundID)

	// Verify StartHeight is persisted and restored.
	require.Equal(t, testRound.StartHeight, fetchedRound.StartHeight)

	// ConfInfo is None at commit time (not yet confirmed).
	require.True(t, fetchedRound.ConfInfo.IsNone())

	// Verify commitment tx.
	require.True(t, fetchedRound.CommitmentTx.IsSome())
	fetchedRound.CommitmentTx.WhenSome(func(packet *psbt.Packet) {
		testRound.CommitmentTx.WhenSome(
			func(expectedPacket *psbt.Packet) {
				require.Equal(
					t, expectedPacket.UnsignedTx.TxHash(),
					packet.UnsignedTx.TxHash(),
				)
			},
		)
	})

	// Verify FSM state type.
	inputSigState, ok := fetchedState.(*round.InputSigSentState)
	require.True(t, ok)
	require.Equal(t, testRound.RoundID, inputSigState.RoundID)
}

// TestRoundStoreLookupByTxid tests looking up a round by commitment txid.
func TestRoundStoreLookupByTxid(t *testing.T) {
	t.Parallel()

	store, _ := newRoundStoreForTest(t)
	ctx := t.Context()

	// Create and commit a test round.
	testRound := createTestRound(t, testRoundIDDB("test-round-txid"))
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}

	err := store.CommitState(ctx, testRound, state)
	require.NoError(t, err)

	// Get the commitment txid.
	var txid chainhash.Hash
	testRound.CommitmentTx.WhenSome(func(packet *psbt.Packet) {
		txid = packet.UnsignedTx.TxHash()
	})

	// Lookup by txid.
	foundRound, err := store.LookupRoundByCommitmentTx(ctx, txid)
	require.NoError(t, err)
	require.NotNil(t, foundRound)
	require.Equal(t, testRound.RoundID, foundRound.RoundID)
}

// TestRoundStoreListActiveRounds tests listing active rounds.
func TestRoundStoreListActiveRounds(t *testing.T) {
	t.Parallel()

	store, _ := newRoundStoreForTest(t)
	ctx := t.Context()

	// Create and commit multiple rounds.
	for i := 0; i < 3; i++ {
		roundID := testRoundIDDB("test-round-" + string(rune('a'+i)))
		testRound := createTestRound(t, roundID)
		state := &round.InputSigSentState{
			RoundID:     testRound.RoundID,
			ClientTrees: make(map[round.SignerKey]*tree.Tree),
		}

		err := store.CommitState(ctx, testRound, state)
		require.NoError(t, err)
	}

	// List active rounds.
	activeRounds, err := store.ListActiveRounds(ctx)
	require.NoError(t, err)
	require.Len(t, activeRounds, 3)
}

// TestRoundStoreFinalizeRound tests finalizing a round.
func TestRoundStoreFinalizeRound(t *testing.T) {
	t.Parallel()

	store, _ := newRoundStoreForTest(t)
	ctx := t.Context()

	// Create and commit a test round.
	testRound := createTestRound(t, testRoundIDDB("test-round-finalize"))
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}

	err := store.CommitState(ctx, testRound, state)
	require.NoError(t, err)

	// Finalize the round with confirmation info.
	var txid chainhash.Hash
	testRound.CommitmentTx.WhenSome(func(packet *psbt.Packet) {
		txid = packet.UnsignedTx.TxHash()
	})

	confInfo := round.ConfInfo{
		Height: 12345,
		BlockHash: chainhash.Hash{
			0xab,
			0xcd,
			0xef,
		},
	}
	err = store.FinalizeRound(ctx, testRound.RoundID, txid, confInfo)
	require.NoError(t, err)

	// List active rounds - should be empty now.
	activeRounds, err := store.ListActiveRounds(ctx)
	require.NoError(t, err)
	require.Len(t, activeRounds, 0)

	// Fetch the round and verify ConfInfo was persisted.
	fetchedRound, _, err := store.FetchState(ctx, testRound.RoundID)
	require.NoError(t, err)
	require.NotNil(t, fetchedRound)

	// After finalization, ConfInfo should be populated.
	require.True(t, fetchedRound.ConfInfo.IsSome())
	fetchedRound.ConfInfo.WhenSome(func(ci round.ConfInfo) {
		require.Equal(t, confInfo.Height, ci.Height)
		require.Equal(t, confInfo.BlockHash, ci.BlockHash)
	})
}

// TestRoundStoreListConfirmedRounds tests listing confirmed rounds. This
// verifies that only rounds marked as "confirmed" (via FinalizeRound) are
// returned, while active rounds are excluded.
func TestRoundStoreListConfirmedRounds(t *testing.T) {
	t.Parallel()

	store, _ := newRoundStoreForTest(t)
	ctx := t.Context()

	// Create and commit three rounds.
	rounds := make([]*round.Round, 3)
	for i := 0; i < 3; i++ {
		seed := "confirmed-test-" + string(rune('a'+i))
		roundID := testRoundIDDB(seed)
		rounds[i] = createTestRound(t, roundID)
		state := &round.InputSigSentState{
			RoundID:     rounds[i].RoundID,
			ClientTrees: make(map[round.SignerKey]*tree.Tree),
		}

		err := store.CommitState(ctx, rounds[i], state)
		require.NoError(t, err)
	}

	// Initially, no rounds should be confirmed.
	confirmedRounds, err := store.ListConfirmedRounds(ctx)
	require.NoError(t, err)
	require.Len(t, confirmedRounds, 0)

	// Finalize two of the three rounds.
	for i := 0; i < 2; i++ {
		var txid chainhash.Hash
		rounds[i].CommitmentTx.WhenSome(func(packet *psbt.Packet) {
			txid = packet.UnsignedTx.TxHash()
		})

		confInfo := round.ConfInfo{
			Height: int32(1000 + i),
			BlockHash: chainhash.Hash{
				byte(i),
				0xab,
				0xcd,
			},
		}
		err = store.FinalizeRound(
			ctx, rounds[i].RoundID, txid, confInfo,
		)
		require.NoError(t, err)
	}

	// Now list confirmed rounds - should return exactly 2.
	confirmedRounds, err = store.ListConfirmedRounds(ctx)
	require.NoError(t, err)
	require.Len(t, confirmedRounds, 2)

	// Verify the confirmed rounds have correct ConfInfo populated.
	for _, r := range confirmedRounds {
		require.True(
			t, r.ConfInfo.IsSome(),
			"confirmed round should have ConfInfo",
		)
	}

	// Verify the active (non-confirmed) round is still in active list.
	activeRounds, err := store.ListActiveRounds(ctx)
	require.NoError(t, err)
	require.Len(t, activeRounds, 1)
	require.Equal(t, rounds[2].RoundID, activeRounds[0].RoundID)
}

// TestVTXOStoreSaveAndList tests saving and listing VTXOs.
func TestVTXOStoreSaveAndList(t *testing.T) {
	t.Parallel()

	store, _ := newRoundStoreForTest(t)
	ctx := t.Context()

	// First, create a round to satisfy foreign key constraint.
	roundID := testRoundIDDB("test-round-vtxo")
	testRound := createTestRound(t, roundID)
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	err := store.CommitState(ctx, testRound, state)
	require.NoError(t, err)

	// Create test VTXOs.
	vtxos := make([]*round.ClientVTXO, 3)
	for i := 0; i < 3; i++ {
		vtxos[i] = createTestClientVTXO(t, roundID, i)
	}

	// Save VTXOs.
	err = store.SaveVTXOs(ctx, vtxos)
	require.NoError(t, err)

	// List VTXOs.
	listedVTXOs, err := store.ListVTXOs(ctx)
	require.NoError(t, err)
	require.Len(t, listedVTXOs, 3)

	// Verify amounts.
	for _, vtxo := range listedVTXOs {
		require.NotZero(t, vtxo.Amount)
		require.NotNil(t, vtxo.OwnerKey.PubKey)
		require.NotNil(t, vtxo.OperatorKey)
	}
}

// TestVTXOStoreGetVTXO tests getting a specific VTXO by outpoint.
func TestVTXOStoreGetVTXO(t *testing.T) {
	t.Parallel()

	store, _ := newRoundStoreForTest(t)
	ctx := t.Context()

	// First, create a round to satisfy foreign key constraint.
	roundID := testRoundIDDB("test-round-get")
	testRound := createTestRound(t, roundID)
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	err := store.CommitState(ctx, testRound, state)
	require.NoError(t, err)

	// Create and save a test VTXO.
	vtxo := createTestClientVTXO(t, roundID, 42)
	err = store.SaveVTXOs(ctx, []*round.ClientVTXO{vtxo})
	require.NoError(t, err)

	// Get the VTXO.
	fetchedVTXO, err := store.GetVTXO(ctx, vtxo.Outpoint)
	require.NoError(t, err)
	require.NotNil(t, fetchedVTXO)
	require.Equal(t, vtxo.Outpoint, fetchedVTXO.Outpoint)
	require.Equal(t, vtxo.Amount, fetchedVTXO.Amount)
}

// TestVTXOStoreRoundMetadataRoundTrip verifies that CommitmentTxID,
// BatchExpiry, and CreatedHeight persisted by SaveVTXOs are mapped back
// onto ClientVTXO by the round-store readback path. Regression guard for
// the metadata race where the write side populated these fields but the
// conversion back to the domain type dropped them.
func TestVTXOStoreRoundMetadataRoundTrip(t *testing.T) {
	t.Parallel()

	store, _ := newRoundStoreForTest(t)
	ctx := t.Context()

	roundID := testRoundIDDB("test-round-metadata")
	testRound := createTestRound(t, roundID)
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	require.NoError(t, store.CommitState(ctx, testRound, state))

	vtxo := createTestClientVTXO(t, roundID, 13)
	var commitmentTxID chainhash.Hash
	for i := range commitmentTxID {
		commitmentTxID[i] = byte(i + 1)
	}
	vtxo.CommitmentTxID = commitmentTxID
	vtxo.BatchExpiry = 987654
	vtxo.CreatedHeight = 123456

	require.NoError(t, store.SaveVTXOs(ctx, []*round.ClientVTXO{vtxo}))

	fetched, err := store.GetVTXO(ctx, vtxo.Outpoint)
	require.NoError(t, err)
	require.NotNil(t, fetched)
	require.Equal(t, commitmentTxID, fetched.CommitmentTxID)
	require.Equal(t, int32(987654), fetched.BatchExpiry)
	require.Equal(t, int32(123456), fetched.CreatedHeight)
}

// TestVTXOStoreGetVTXOPreservesStoredPubKeyParity ensures that loading a VTXO
// through the round store keeps the exact stored owner pubkey instead of
// replacing it with the even-y x-only lift reconstructed from the policy
// template. Join-auth and other signing flows depend on preserving the
// wallet-owned key descriptor exactly as it was persisted.
func TestVTXOStoreGetVTXOPreservesStoredPubKeyParity(t *testing.T) {
	t.Parallel()

	store, _ := newRoundStoreForTest(t)
	ctx := t.Context()

	roundID := testRoundIDDB("test-round-parity")
	testRound := createTestRound(t, roundID)
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	err := store.CommitState(ctx, testRound, state)
	require.NoError(t, err)

	vtxo := createTestClientVTXO(t, roundID, 77)
	for vtxo.OwnerKey.PubKey.SerializeCompressed()[0] != 0x03 {
		vtxo = createTestClientVTXO(t, roundID, 77)
	}

	err = store.SaveVTXOs(ctx, []*round.ClientVTXO{vtxo})
	require.NoError(t, err)

	fetchedVTXO, err := store.GetVTXO(ctx, vtxo.Outpoint)
	require.NoError(t, err)
	require.NotNil(t, fetchedVTXO)
	require.NotNil(t, fetchedVTXO.OwnerKey.PubKey)

	require.Equal(
		t, vtxo.OwnerKey.PubKey.SerializeCompressed(),
		fetchedVTXO.OwnerKey.PubKey.SerializeCompressed(),
	)
	require.Equal(
		t, vtxo.OperatorKey.SerializeCompressed(),
		fetchedVTXO.OperatorKey.SerializeCompressed(),
	)
}

// TestVTXOStoreSaveVTXOsHealsZeroLocatorOwner ensures that a healed VTXO can
// update a placeholder NULL client_key_id to a valid local 0/0 derivation
// path. Derivation path 0/0 is legitimate and must not be treated as
// "missing" during the conflict update.
func TestVTXOStoreSaveVTXOsHealsZeroLocatorOwner(t *testing.T) {
	t.Parallel()

	store, baseDB := newRoundStoreForTest(t)
	ctx := t.Context()

	roundID := testRoundIDDB("test-round-heal-zero-locator")
	testRound := createTestRound(t, roundID)
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	err := store.CommitState(ctx, testRound, state)
	require.NoError(t, err)

	vtxo := createTestClientVTXO(t, roundID, 123)
	vtxo.OwnerKey.KeyLocator = keychain.KeyLocator{}

	params, err := store.domainVTXOToInsertParams(ctx, baseDB.Queries, vtxo)
	require.NoError(t, err)

	// Simulate a minimal round-created row that has not yet had its owner
	// key registered: the client_key_id FK is NULL until SaveVTXOs heals
	// it.
	placeholder := params
	placeholder.ClientKeyID = sql.NullInt64{}

	err = baseDB.Queries.InsertVTXO(ctx, placeholder)
	require.NoError(t, err)

	err = store.SaveVTXOs(ctx, []*round.ClientVTXO{vtxo})
	require.NoError(t, err)

	fetchedVTXO, err := store.GetVTXO(ctx, vtxo.Outpoint)
	require.NoError(t, err)
	require.NotNil(t, fetchedVTXO)
	require.NotNil(t, fetchedVTXO.OwnerKey.PubKey)
	require.True(
		t, fetchedVTXO.OwnerKey.PubKey.IsEqual(vtxo.OwnerKey.PubKey),
	)
	require.Equal(t, vtxo.OwnerKey.Family, fetchedVTXO.OwnerKey.Family)
	require.Equal(t, vtxo.OwnerKey.Index, fetchedVTXO.OwnerKey.Index)
}

// TestVTXOStoreMarkSpent tests marking a VTXO as spent.
func TestVTXOStoreMarkSpent(t *testing.T) {
	t.Parallel()

	store, _ := newRoundStoreForTest(t)
	ctx := t.Context()

	// First, create a round to satisfy foreign key constraint.
	roundID := testRoundIDDB("test-round-spent")
	testRound := createTestRound(t, roundID)
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	err := store.CommitState(ctx, testRound, state)
	require.NoError(t, err)

	// Create and save a test VTXO.
	vtxo := createTestClientVTXO(t, roundID, 99)
	err = store.SaveVTXOs(ctx, []*round.ClientVTXO{vtxo})
	require.NoError(t, err)

	// Verify it's in the list.
	listedVTXOs, err := store.ListVTXOs(ctx)
	require.NoError(t, err)
	require.Len(t, listedVTXOs, 1)

	// Mark as spent.
	err = store.MarkVTXOSpent(ctx, vtxo.Outpoint)
	require.NoError(t, err)

	// Verify it's no longer in the unspent list.
	listedVTXOs, err = store.ListVTXOs(ctx)
	require.NoError(t, err)
	require.Len(t, listedVTXOs, 0)
}

// boardingIntentFixture holds all the data needed for a single boarding
// intent in tests, including the wallet-layer objects needed to satisfy
// FK constraints.
type boardingIntentFixture struct {
	clientPubKey  *btcec.PublicKey
	operatorKey   *btcec.PublicKey
	boardingAddr  *wallet.BoardingAddress
	walletIntent  wallet.BoardingIntent
	roundIntent   round.BoardingIntent
	vtxoTemplates []types.VTXORequest
	outpoint      wire.OutPoint
	pkScript      []byte
	inputSig      *types.BoardingInputSignature
	clientTreeKey round.SignerKey
	clientTree    *tree.Tree
}

// createBoardingIntentFixture creates a complete boarding intent fixture with
// all wallet-layer dependencies. The index parameter is used to generate unique
// keys and outpoints for each fixture.
func createBoardingIntentFixture(
	t *testing.T, roundID round.RoundID, idx int,
) *boardingIntentFixture {

	// Create unique keys for this intent.
	clientPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	clientPubKey := clientPrivKey.PubKey()

	operatorPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorPubKey := operatorPrivKey.PubKey()

	exitDelay := uint32(144)

	// Build the tapscript.
	tapscript, err := arkscript.VTXOTapScript(
		clientPubKey, operatorPubKey, exitDelay,
	)
	require.NoError(t, err)

	// Create the taproot address.
	taprootKey := txscript.ComputeTaprootOutputKey(
		&arkscript.ARKNUMSKey, tapscript.RootHash,
	)
	address, err := btcaddr.NewAddressTaproot(
		taprootKey.SerializeCompressed()[1:],
		&chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	pkScript, err := txscript.PayToAddrScript(address)
	require.NoError(t, err)

	keyDesc := keychain.KeyDescriptor{
		PubKey: clientPubKey,
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(42),
			Index:  uint32(idx),
		},
	}

	boardingAddr := &wallet.BoardingAddress{
		Address:     address,
		Tapscript:   tapscript,
		KeyDesc:     keyDesc,
		OperatorKey: operatorPubKey,
		ExitDelay:   exitDelay,
	}

	// Create unique outpoint for this intent.
	intentOutpoint := wire.OutPoint{
		Hash: chainhash.Hash{
			0xaa,
			0xbb,
			byte(idx),
		},
		Index: uint32(idx),
	}

	// Create confirmation tx.
	confHash := chainhash.Hash{0xdd, 0xee, byte(idx)}
	confTx := wire.NewMsgTx(2)
	confTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{0x11, byte(idx)},
			Index: 0,
		},
	})
	confTx.AddTxOut(&wire.TxOut{
		Value:    int64(100000 * (idx + 1)),
		PkScript: pkScript,
	})

	walletIntent := wallet.BoardingIntent{
		Address:  *boardingAddr,
		Outpoint: intentOutpoint,
		ChainInfo: wallet.BoardingChainInfo{
			ConfHeight: int32(100 + idx),
			ConfHash:   confHash,
			ConfTx:     confTx,
			OutPoint:   intentOutpoint,
			Amount:     btcutil.Amount(100000 * (idx + 1)),
		},
		Status: wallet.BoardingStatusConfirmed,
	}

	// Create VTXO template for this intent (1:1 coupling at DB level for
	// now). TODO: Support multiple VTXOs per intent when we decouple
	// storage.
	signingKey := keychain.KeyDescriptor{
		PubKey: clientPubKey,
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(43),
			Index:  uint32(idx * 10),
		},
	}
	ownerKey := keychain.KeyDescriptor{
		PubKey: clientPubKey,
		KeyLocator: keychain.KeyLocator{
			Family: types.VTXOOwnerKeyFamily,
			Index:  uint32(idx*10 + 1),
		},
	}
	vtxoTemplates := []types.VTXORequest{{
		Amount: btcutil.Amount(90000),
		PolicyTemplate: func() []byte {
			policy, err := arkscript.EncodeStandardVTXOTemplate(
				clientPubKey, operatorPubKey, exitDelay,
			)
			require.NoError(t, err)

			return policy
		}(),
		PkScript:    pkScript,
		Expiry:      exitDelay,
		ClientKey:   clientPubKey,
		OwnerKey:    ownerKey,
		OperatorKey: operatorPubKey,
		SigningKey:  signingKey,
	}}

	boardingRequest := types.BoardingRequest{
		Outpoint: &intentOutpoint,
		PolicyTemplate: func() []byte {
			policy, err := arkscript.EncodeStandardVTXOTemplate(
				clientPubKey, operatorPubKey, exitDelay,
			)
			require.NoError(t, err)

			return policy
		}(),
	}

	roundIntent := round.BoardingIntent{
		BoardingIntent: walletIntent,
		Request:        boardingRequest,
	}

	// Create input signature as a BoardingInputSignature.
	var sigBytes [64]byte
	copy(sigBytes[:], bytes.Repeat([]byte{byte(0xab + idx)}, 64))
	sig, err := schnorr.ParseSignature(sigBytes[:])
	require.NoError(t, err)

	inputSig := &types.BoardingInputSignature{
		InputIndex:      idx,
		Outpoint:        intentOutpoint,
		ClientSignature: sig,
	}

	// Create client tree using the actual tree builder (1 leaf per intent
	// to match 1:1 VTXO coupling).
	clientTreeKey := round.NewSignerKey(clientPubKey)
	rootOutpoint := wire.OutPoint{
		Hash: chainhash.Hash{
			0xc1,
			0x1e,
			byte(idx),
		},
		Index: uint32(idx),
	}
	rootOutput := &wire.TxOut{
		Value:    int64(90000 * (idx + 1)),
		PkScript: pkScript,
	}
	leaves := []tree.LeafDescriptor{
		{
			PkScript:    pkScript,
			Amount:      btcutil.Amount(90000 * (idx + 1)),
			CoSignerKey: clientPubKey,
		},
	}
	clientTree, err := tree.NewTree(
		rootOutpoint, rootOutput, leaves, operatorPubKey, nil, 2,
	)
	require.NoError(t, err)

	return &boardingIntentFixture{
		clientPubKey:  clientPubKey,
		operatorKey:   operatorPubKey,
		boardingAddr:  boardingAddr,
		walletIntent:  walletIntent,
		roundIntent:   roundIntent,
		vtxoTemplates: vtxoTemplates,
		outpoint:      intentOutpoint,
		pkScript:      pkScript,
		inputSig:      inputSig,
		clientTreeKey: clientTreeKey,
		clientTree:    clientTree,
	}
}

// TestRoundStoreDecoupledVTXOStorage verifies that VTXO requests are stored
// independently from boarding intents. This allows fan-out (1 input creating
// multiple outputs) and fan-in (multiple inputs funding 1 output) scenarios.
// The test creates 2 boarding intents but 3 VTXO requests to verify the counts
// are independent.
func TestRoundStoreDecoupledVTXOStorage(t *testing.T) {
	t.Parallel()

	const numIntents = 2
	const numVTXORequests = 3

	roundID := testRoundIDDB("test-round-decoupled-vtxo")

	ctx := t.Context()
	roundStore, db := newRoundStoreForTest(t)

	// Set up boarding store for FK constraints.
	boardingDB := NewTransactionExecutor(
		db,
		func(tx *sql.Tx) BoardingStore {
			return db.WithTx(tx)
		},
		btclog.Disabled,
	)
	intentDB := NewTransactionExecutor(
		db,
		func(tx *sql.Tx) PendingIntentStore {
			return db.WithTx(tx)
		},
		btclog.Disabled,
	)
	boardingStore := NewBoardingWalletStore(
		boardingDB, intentDB, &chaincfg.RegressionNetParams,
		clock.NewDefaultClock(),
	)

	// Create 2 boarding intent fixtures.
	fixtures := make([]*boardingIntentFixture, numIntents)
	for i := 0; i < numIntents; i++ {
		fixtures[i] = createBoardingIntentFixture(t, roundID, i)

		err := boardingStore.InsertBoardingAddress(
			ctx, fixtures[i].boardingAddr,
		)
		require.NoError(t, err)

		err = boardingStore.InsertBoardingIntents(
			ctx, fixtures[i].walletIntent,
		)
		require.NoError(t, err)
	}

	// Build boarding intents from fixtures.
	roundIntents := make([]round.BoardingIntent, numIntents)
	inputSigs := make([]*types.BoardingInputSignature, numIntents)
	clientTrees := make(map[round.SignerKey]*tree.Tree)
	for i, f := range fixtures {
		roundIntents[i] = f.roundIntent
		inputSigs[i] = f.inputSig
		clientTrees[f.clientTreeKey] = f.clientTree
	}

	// Create 3 VTXO requests (more than boarding intents) to test
	// decoupled storage.
	allVtxos := make([]types.VTXORequest, numVTXORequests)
	for i := 0; i < numVTXORequests; i++ {
		privKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		operatorKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		signingKey := keychain.KeyDescriptor{
			PubKey: privKey.PubKey(),
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamily(50),
				Index:  uint32(i),
			},
		}
		ownerKey := keychain.KeyDescriptor{
			PubKey: privKey.PubKey(),
			KeyLocator: keychain.KeyLocator{
				Family: types.VTXOOwnerKeyFamily,
				Index:  uint32(100 + i),
			},
		}

		allVtxos[i] = types.VTXORequest{
			Amount: btcutil.Amount(30000 * (i + 1)),
			PolicyTemplate: func() []byte {
				policy, err := arkscript.
					EncodeStandardVTXOTemplate(
						privKey.PubKey(),
						operatorKey.PubKey(), 144,
					)
				require.NoError(t, err)

				return policy
			}(),
			PkScript:    fixtures[0].pkScript,
			Expiry:      144,
			ClientKey:   privKey.PubKey(),
			OwnerKey:    ownerKey,
			OperatorKey: operatorKey.PubKey(),
			SigningKey:  signingKey,
		}
	}

	// Create commitment tx with inputs from all boarding intents.
	tx := wire.NewMsgTx(2)
	for _, f := range fixtures {
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: f.outpoint,
		})
	}
	tx.AddTxOut(&wire.TxOut{
		Value:    180000,
		PkScript: fixtures[0].pkScript,
	})
	commitTx, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)

	// Create VTXT tree. The output value must equal sum of leaf amounts
	// (30000 + 60000 + 90000 = 180000).
	vtxtTreeOutpoint := wire.OutPoint{
		Hash: chainhash.Hash{
			0x02,
		},
		Index: 0,
	}
	vtxtTreeOutput := &wire.TxOut{
		Value:    180000,
		PkScript: fixtures[0].pkScript,
	}
	vtxtLeaves := make([]tree.LeafDescriptor, numVTXORequests)
	for i := 0; i < numVTXORequests; i++ {
		vtxtLeaves[i] = tree.LeafDescriptor{
			PkScript:    fixtures[0].pkScript,
			Amount:      btcutil.Amount(30000 * (i + 1)),
			CoSignerKey: allVtxos[i].ClientKey,
		}
	}
	vtxtTree, err := tree.NewTree(
		vtxtTreeOutpoint, vtxtTreeOutput, vtxtLeaves,
		fixtures[0].operatorKey, nil, 2,
	)
	require.NoError(t, err)

	testRound := &round.Round{
		RoundID:       roundID,
		CommitmentTx:  fn.Some(commitTx),
		VTXOTreePaths: fn.Some(map[int]*tree.Tree{0: vtxtTree}),
		Intents: round.Intents{
			Boarding: roundIntents,
			VTXOs:    allVtxos,
		},
	}

	state := &round.InputSigSentState{
		RoundID:      roundID,
		CommitmentTx: commitTx,
		VTXOTreePaths: map[int]*tree.Tree{
			0: vtxtTree,
		},
		Intents: round.Intents{
			Boarding: roundIntents,
			VTXOs:    allVtxos,
		},
		ClientTrees: clientTrees,
		InputSigs:   inputSigs,
	}

	// Commit state.
	err = roundStore.CommitState(ctx, testRound, state)
	require.NoError(t, err)

	// Fetch and verify.
	fetchedRound, fetchedState, err := roundStore.FetchState(ctx, roundID)
	require.NoError(t, err)

	// Verify boarding intents count.
	require.Len(
		t, fetchedRound.Intents.Boarding, numIntents, "expected %d "+
			"boarding intents", numIntents,
	)

	// Verify VTXO requests count is different from intents.
	inputSigState, ok := fetchedState.(*round.InputSigSentState)
	require.True(t, ok)
	require.Len(
		t, inputSigState.Intents.VTXOs, numVTXORequests, "expected "+
			"%d VTXO requests (decoupled from %d intents)",
		numVTXORequests, numIntents,
	)

	// Verify VTXO amounts were preserved correctly.
	for i, vtxo := range inputSigState.Intents.VTXOs {
		expectedAmount := btcutil.Amount(30000 * (i + 1))
		require.Equal(
			t, expectedAmount, vtxo.Amount, "VTXO %d amount "+
				"mismatch", i,
		)
		require.NotNil(t, vtxo.OwnerKey.PubKey)
		require.True(t, vtxo.OwnerKey.PubKey.IsEqual(vtxo.ClientKey))
		require.Equal(t, types.VTXOOwnerKeyFamily, vtxo.OwnerKey.Family)
		require.Equal(t, uint32(100+i), vtxo.OwnerKey.Index)
	}
}

func TestRoundVTXORequestParamsAcceptCustomPolicy(t *testing.T) {
	t.Parallel()

	sender, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	receiver, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	server, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy, err := arkscript.NewVHTLCPolicy(arkscript.VHTLCOpts{
		Sender:                               sender.PubKey(),
		Receiver:                             receiver.PubKey(),
		Server:                               server.PubKey(),
		PreimageHash:                         lntypes.Hash{1, 2, 3},
		RefundLocktime:                       144,
		UnilateralClaimDelay:                 12,
		UnilateralRefundDelay:                18,
		UnilateralRefundWithoutReceiverDelay: 24,
	})
	require.NoError(t, err)

	policyTemplate, err := policy.Template.Encode()
	require.NoError(t, err)
	pkScript, err := policy.PkScript()
	require.NoError(t, err)

	req := &types.VTXORequest{
		Amount:         42_000,
		PolicyTemplate: policyTemplate,
		PkScript:       pkScript,
		Expiry:         24,
	}
	_, err = req.DecodeStandardPolicyTemplate()
	require.Error(t, err)

	db := NewTestDB(t)
	params, err := vtxoRequestToRoundParams(
		t.Context(), db, 1, "round-id", 0, req,
	)
	require.NoError(t, err)
	require.Equal(t, int64(42_000), params.Amount)
	require.Equal(t, pkScript, params.PkScript)
	require.Equal(t, int32(24), params.Expiry)
	require.Equal(t, policyTemplate, params.PolicyTemplate)
	require.Empty(t, params.ClientPubkey)
	require.Empty(t, params.OperatorPubkey)
	require.False(t, params.OwnerKeyID.Valid)
	require.False(t, params.SigningKeyID.Valid)
}

// TestListRoundsPaginated verifies cursor-based pagination of persisted
// rounds, including VTXO details returned per round.
func TestListRoundsPaginated(t *testing.T) {
	t.Parallel()

	store, _ := newRoundStoreForTest(t)
	ctx := t.Context()

	// Empty database returns empty results.
	summaries, err := store.ListRoundsPaginated(ctx, ListRoundsQuery{
		Limit: 10,
	})
	require.NoError(t, err)
	require.Empty(t, summaries)

	// Insert 5 rounds with deterministic IDs so we know their
	// lexicographic order.
	const numRounds = 5
	roundIDs := make([]round.RoundID, numRounds)
	for i := 0; i < numRounds; i++ {
		seed := "paginated-test-" + string(rune('a'+i))
		roundIDs[i] = testRoundIDDB(seed)

		testRound := createTestRound(t, roundIDs[i])
		state := &round.InputSigSentState{
			RoundID:     testRound.RoundID,
			ClientTrees: make(map[round.SignerKey]*tree.Tree),
		}

		err := store.CommitState(ctx, testRound, state)
		require.NoError(t, err)
	}

	// Save VTXOs for the first two rounds to test VTXO inclusion.
	for i := 0; i < 2; i++ {
		vtxos := make([]*round.ClientVTXO, 2)
		for j := 0; j < 2; j++ {
			vtxos[j] = createTestClientVTXO(
				t, roundIDs[i], i*10+j,
			)
		}
		err := store.SaveVTXOs(ctx, vtxos)
		require.NoError(t, err)
	}

	// Fetch all rounds (limit > numRounds).
	all, err := store.ListRoundsPaginated(ctx, ListRoundsQuery{
		Limit: 100,
	})
	require.NoError(t, err)
	require.Len(t, all, numRounds)

	// All should be input_sig_sent status.
	for _, s := range all {
		require.Equal(t, "input_sig_sent", s.Status)
	}

	// Verify ascending round_id order.
	for i := 1; i < len(all); i++ {
		require.True(
			t, all[i-1].RoundID.String() < all[i].RoundID.String(),
			"rounds not in ascending order",
		)
	}

	// Verify VTXOs are attached to the right rounds. The first
	// two rounds should each have 2 VTXOs; the rest have none.
	vtxoRoundIDs := make(map[string]int)
	for _, s := range all {
		vtxoRoundIDs[s.RoundID.String()] = len(s.VTXOs)
	}
	for i := 0; i < 2; i++ {
		cnt, ok := vtxoRoundIDs[roundIDs[i].String()]
		require.True(t, ok)
		require.Equal(t, 2, cnt,
			"round %d should have 2 VTXOs", i)
	}

	// Test page_size limiting: request first 2 rounds.
	page1, err := store.ListRoundsPaginated(ctx, ListRoundsQuery{
		Limit: 2,
	})
	require.NoError(t, err)
	require.Len(t, page1, 2)

	// Use last round_id as cursor for next page.
	cursor := page1[len(page1)-1].RoundID.String()
	page2, err := store.ListRoundsPaginated(ctx, ListRoundsQuery{
		Cursor: cursor,
		Limit:  2,
	})
	require.NoError(t, err)
	require.Len(t, page2, 2)

	// Third page should have 1 remaining round.
	cursor = page2[len(page2)-1].RoundID.String()
	page3, err := store.ListRoundsPaginated(ctx, ListRoundsQuery{
		Cursor: cursor,
		Limit:  2,
	})
	require.NoError(t, err)
	require.Len(t, page3, 1)

	// Fourth page should be empty.
	cursor = page3[0].RoundID.String()
	page4, err := store.ListRoundsPaginated(ctx, ListRoundsQuery{
		Cursor: cursor,
		Limit:  2,
	})
	require.NoError(t, err)
	require.Empty(t, page4)

	// Finalize one round and verify status changes.
	var txid chainhash.Hash
	txid[0] = 0xff
	confInfo := round.ConfInfo{
		Height: 999,
		BlockHash: chainhash.Hash{
			0xab,
		},
	}
	err = store.FinalizeRound(ctx, roundIDs[0], txid, confInfo)
	require.NoError(t, err)

	// Re-fetch all — the finalized round should show "confirmed".
	all, err = store.ListRoundsPaginated(ctx, ListRoundsQuery{
		Limit: 100,
	})
	require.NoError(t, err)
	for _, s := range all {
		if s.RoundID == roundIDs[0] {
			require.Equal(t, "confirmed", s.Status)
		} else {
			require.Equal(t, "input_sig_sent", s.Status)
		}
	}
}

// TestListRoundsPaginatedFiltersBeforeLimit verifies persisted round filters
// are applied before cursor pagination and LIMIT.
func TestListRoundsPaginatedFiltersBeforeLimit(t *testing.T) {
	t.Parallel()

	store, _ := newRoundStoreForTest(t)
	ctx := t.Context()

	const numRounds = 4
	roundIDs := make([]round.RoundID, 0, numRounds)
	for i := 0; i < numRounds; i++ {
		roundID := testRoundIDDB(
			"filtered-paginated-test-" + string(rune('a'+i)),
		)
		testRound := createTestRound(t, roundID)
		state := &round.InputSigSentState{
			RoundID:     testRound.RoundID,
			ClientTrees: make(map[round.SignerKey]*tree.Tree),
		}

		err := store.CommitState(ctx, testRound, state)
		require.NoError(t, err)

		roundIDs = append(roundIDs, roundID)
	}

	sort.Slice(roundIDs, func(i, j int) bool {
		return roundIDs[i].String() < roundIDs[j].String()
	})

	var txid chainhash.Hash
	txid[0] = 0xaa
	err := store.FinalizeRound(ctx, roundIDs[len(roundIDs)-1], txid,
		round.ConfInfo{
			Height:    999,
			BlockHash: chainhash.Hash{0xbb},
		},
	)
	require.NoError(t, err)

	confirmed, err := store.ListRoundsPaginated(ctx, ListRoundsQuery{
		Limit:  1,
		Status: "confirmed",
	})
	require.NoError(t, err)
	require.Len(t, confirmed, 1)
	require.Equal(t, roundIDs[len(roundIDs)-1], confirmed[0].RoundID)
	require.Equal(t, "confirmed", confirmed[0].Status)

	inputSigSent, err := store.ListRoundsPaginated(ctx, ListRoundsQuery{
		Limit:  2,
		Status: "input_sig_sent",
	})
	require.NoError(t, err)
	require.Len(t, inputSigSent, 2)
	for _, summary := range inputSigSent {
		require.Equal(t, "input_sig_sent", summary.Status)
	}
}

// TestRoundStoreWithBoardingGroup verifies that rounds with boarding intents
// can be persisted and recovered correctly. This is critical because boarding
// intents link on-chain UTXOs to virtual transaction outputs - losing this
// mapping after a checkpoint would leave the client unable to prove ownership
// of VTXOs created from their boarding funds.
func TestRoundStoreWithBoardingGroup(t *testing.T) {
	t.Parallel()

	const numIntents = 2
	roundID := testRoundIDDB("test-round-boarding")

	ctx := t.Context()
	roundStore, db := newRoundStoreForTest(t)

	// The round store queries the base boarding tables (boarding_addresses,
	// boarding_intents) when reconstructing rounds, so we need to populate
	// those first to satisfy foreign key constraints.
	boardingDB := NewTransactionExecutor(
		db,
		func(tx *sql.Tx) BoardingStore {
			return db.WithTx(tx)
		},
		btclog.Disabled,
	)
	intentDB := NewTransactionExecutor(
		db,
		func(tx *sql.Tx) PendingIntentStore {
			return db.WithTx(tx)
		},
		btclog.Disabled,
	)
	boardingStore := NewBoardingWalletStore(
		boardingDB, intentDB, &chaincfg.RegressionNetParams,
		clock.NewDefaultClock(),
	)

	// Create multiple boarding intent fixtures with their wallet-layer
	// dependencies.
	fixtures := make([]*boardingIntentFixture, numIntents)
	for i := 0; i < numIntents; i++ {
		fixtures[i] = createBoardingIntentFixture(t, roundID, i)

		// Insert wallet-layer objects to satisfy FK constraints.
		err := boardingStore.InsertBoardingAddress(
			ctx, fixtures[i].boardingAddr,
		)
		require.NoError(t, err)

		err = boardingStore.InsertBoardingIntents(
			ctx, fixtures[i].walletIntent,
		)
		require.NoError(t, err)
	}
	// The commitment transaction can order inputs differently from the
	// client's boarding intent list. Recovery must preserve the actual
	// commitment input index stored in the signature, not the intent's
	// position in the local slice.
	fixtures[0].inputSig.InputIndex = 1
	fixtures[1].inputSig.InputIndex = 0

	// Build the round's boarding group from the fixtures.
	roundIntents := make([]round.BoardingIntent, numIntents)
	allVtxos := make([]types.VTXORequest, 0, numIntents)
	inputSigs := make([]*types.BoardingInputSignature, numIntents)
	clientTrees := make(map[round.SignerKey]*tree.Tree)
	for i, f := range fixtures {
		roundIntents[i] = f.roundIntent
		allVtxos = append(allVtxos, f.vtxoTemplates...)
		inputSigs[i] = f.inputSig
		clientTrees[f.clientTreeKey] = f.clientTree
	}

	// Create the commitment tx with inputs from all boarding intents.
	tx := wire.NewMsgTx(2)
	for i := len(fixtures) - 1; i >= 0; i-- {
		f := fixtures[i]
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: f.outpoint,
		})
	}
	tx.AddTxOut(&wire.TxOut{
		Value:    190000,
		PkScript: fixtures[0].pkScript,
	})
	commitTx, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)

	// Create the VTXT tree using the actual tree builder.
	vtxtTreeOutpoint := wire.OutPoint{
		Hash: chainhash.Hash{
			0x02,
		},
		Index: 0,
	}
	vtxtTreeOutput := &wire.TxOut{
		Value:    90000,
		PkScript: fixtures[0].pkScript,
	}
	vtxtLeaves := make([]tree.LeafDescriptor, 0, numIntents)
	for _, f := range fixtures {
		for range f.vtxoTemplates {
			vtxtLeaves = append(vtxtLeaves, tree.LeafDescriptor{
				PkScript:    f.pkScript,
				Amount:      45000,
				CoSignerKey: f.clientPubKey,
			})
		}
	}
	vtxtTree, err := tree.NewTree(
		vtxtTreeOutpoint, vtxtTreeOutput, vtxtLeaves,
		fixtures[0].operatorKey, nil, 2,
	)
	require.NoError(t, err)

	testRound := &round.Round{
		RoundID:       roundID,
		CommitmentTx:  fn.Some(commitTx),
		VTXOTreePaths: fn.Some(map[int]*tree.Tree{0: vtxtTree}),
		Intents: round.Intents{
			Boarding: roundIntents,
			VTXOs:    allVtxos,
		},
	}

	// Create the FSM state with all intents, signatures, and client trees.
	state := &round.InputSigSentState{
		RoundID:      roundID,
		CommitmentTx: commitTx,
		VTXOTreePaths: map[int]*tree.Tree{
			0: vtxtTree,
		},
		Intents: round.Intents{
			Boarding: roundIntents,
			VTXOs:    allVtxos,
		},
		ClientTrees: clientTrees,
		InputSigs:   inputSigs,
	}

	// Commit the state.
	err = roundStore.CommitState(ctx, testRound, state)
	require.NoError(t, err)

	// Fetch the state back.
	fetchedRound, fetchedState, err := roundStore.FetchState(ctx, roundID)
	require.NoError(t, err)
	require.NotNil(t, fetchedRound)
	require.NotNil(t, fetchedState)

	// Verify round fields.
	require.Equal(t, roundID, fetchedRound.RoundID)

	// ConfInfo is None at commit time (not yet confirmed).
	require.True(t, fetchedRound.ConfInfo.IsNone())

	// Verify boarding intents were persisted with all intents.
	require.Len(t, fetchedRound.Intents.Boarding, numIntents)
	for i, f := range fixtures {
		fetchedIntent := fetchedRound.Intents.Boarding[i]
		require.Equal(t, f.outpoint, fetchedIntent.Outpoint)
	}

	// Verify FSM state type.
	inputSigState, ok := fetchedState.(*round.InputSigSentState)
	require.True(t, ok)
	require.Equal(t, roundID, inputSigState.RoundID)

	// Verify VTXO templates were persisted (1 per intent for now).
	require.Len(t, inputSigState.Intents.VTXOs, numIntents)

	// Verify all input signatures were persisted and recovered.
	require.Len(t, inputSigState.InputSigs, numIntents)
	for i, f := range fixtures {
		require.Equal(t, f.inputSig, inputSigState.InputSigs[i])
	}

	// Verify all client trees were persisted and recovered.
	require.Len(t, inputSigState.ClientTrees, numIntents)
	for _, f := range fixtures {
		fetchedTree, ok := inputSigState.ClientTrees[f.clientTreeKey]
		require.True(t, ok, "client tree not found for key")
		require.Equal(
			t, f.clientTree.BatchOutpoint,
			fetchedTree.BatchOutpoint,
		)
		require.Equal(
			t, f.clientTree.Root.Input, fetchedTree.Root.Input,
		)

		// Verify tree structure was preserved. The number of outputs
		// should match: one per leaf group + 1 anchor output (P2A).
		// Each fixture creates 1 leaf (1:1 coupling).
		numLeaves := 1
		expectedOutputs := numLeaves + 1
		require.Len(t, fetchedTree.Root.Outputs, expectedOutputs)
	}

	// Verify client tree txids were populated in the index table.
	for _, f := range fixtures {
		// Extract expected txids from the original client tree.
		expectedTxids, err := f.clientTree.ExtractTxids()
		require.NoError(t, err)
		require.NotEmpty(t, expectedTxids)

		// Query the stored txids from the database.
		storedTxids, err := db.GetClientTreeTxids(
			ctx, sqlc.GetClientTreeTxidsParams{
				RoundID:   roundID.String(),
				ClientKey: f.clientTreeKey[:],
			},
		)
		require.NoError(t, err)

		// Verify the count matches.
		require.Len(
			t, storedTxids, len(expectedTxids),
			"txid count mismatch for client tree",
		)

		// Sort both lists by txid for deterministic comparison. The
		// order within a level may differ due to map iteration order.
		sort.Slice(expectedTxids, func(i, j int) bool {
			return bytes.Compare(
				expectedTxids[i].Txid[:],
				expectedTxids[j].Txid[:],
			) < 0
		})
		sort.Slice(storedTxids, func(i, j int) bool {
			return bytes.Compare(
				storedTxids[i].Txid, storedTxids[j].Txid,
			) < 0
		})

		// Verify each txid was stored with correct level.
		for i, expected := range expectedTxids {
			stored := storedTxids[i]
			require.Equal(
				t, expected.Txid[:], stored.Txid, "txid "+
					"mismatch at index %d", i,
			)

			expLevel := int32(expected.TreeLevel)
			require.Equal(
				t, expLevel, stored.TreeLevel,
				"tree level mismatch for txid",
			)
		}

		// Verify we can lookup the client tree by any of its txids.
		for _, expected := range expectedTxids {
			treeByTxid, err := db.GetClientTreeByTxid(
				ctx, expected.Txid[:],
			)
			require.NoError(t, err)
			require.Equal(t, roundID.String(), treeByTxid.RoundID)
			require.Equal(
				t, f.clientTreeKey[:], treeByTxid.ClientKey,
			)
		}
	}
}

// TestCommitStateClearsSendIntentAnchors verifies the adoption-side half of
// the pending-intents outbox contract for onchain sends: committing a round
// whose forfeit set covers a persisted send_onchain intent's anchors deletes
// those anchors — and the orphaned parent row — inside the same transaction,
// while an unrelated intent's anchors survive untouched.
func TestCommitStateClearsSendIntentAnchors(t *testing.T) {
	t.Parallel()

	store, baseDB := newRoundStoreForTest(t)
	ctx := t.Context()

	// Build a pending-intent store over the same database so the test
	// observes exactly what the wallet's replay pass would see.
	intentDB := NewTransactionExecutor(
		baseDB,
		func(tx *sql.Tx) PendingIntentStore {
			return baseDB.WithTx(tx)
		},
		btclog.Disabled,
	)
	intentStore := NewPendingIntentPersistenceStore(intentDB)

	opA := wire.OutPoint{Hash: chainhash.Hash{0xa1}, Index: 0}
	opB := wire.OutPoint{Hash: chainhash.Hash{0xa2}, Index: 1}
	opOther := wire.OutPoint{Hash: chainhash.Hash{0xa3}, Index: 0}

	adoptedPayload := &wallet.SendOnChainIntentPayload{
		DestinationPkScript: append(
			[]byte{0x51, 0x20}, make([]byte, 32)...,
		),
		TargetAmountSat: 30_000,
	}
	adopted := wallet.PendingIntent{
		ID: wallet.NewPendingIntentID(
			adoptedPayload, []wire.OutPoint{opA, opB},
		),
		Payload:     adoptedPayload,
		RequestedAt: 1_700_000_000,
		Anchors: []wire.OutPoint{
			opA,
			opB,
		},
	}
	survivorPayload := &wallet.SendOnChainIntentPayload{
		DestinationPkScript: append(
			[]byte{0x51, 0x20}, make([]byte, 32)...,
		),
		TargetAmountSat: 45_000,
	}
	survivor := wallet.PendingIntent{
		ID: wallet.NewPendingIntentID(
			survivorPayload, []wire.OutPoint{opOther},
		),
		Payload:     survivorPayload,
		RequestedAt: 1_700_000_001,
		Anchors: []wire.OutPoint{
			opOther,
		},
	}
	require.NoError(t, intentStore.UpsertPendingIntent(ctx, adopted))
	require.NoError(t, intentStore.UpsertPendingIntent(ctx, survivor))

	// Commit a round whose forfeit set consumes exactly the adopted
	// intent's anchors.
	testRound := createTestRound(
		t, testRoundIDDB("send-anchor-clear-round"),
	)
	testRound.Intents.Forfeits = []types.ForfeitRequest{
		{
			VTXOOutpoint: &opA,
		},
		{
			VTXOOutpoint: &opB,
		},
	}

	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	testRound.CommitmentTx.WhenSome(func(packet *psbt.Packet) {
		state.CommitmentTx = packet
	})

	require.NoError(t, store.CommitState(ctx, testRound, state))

	// The adopted intent must be gone (anchors cleared, orphaned parent
	// swept); the unrelated intent must survive with its anchor intact.
	remaining, err := intentStore.ListPendingIntents(
		ctx, wallet.PendingIntentKindSendOnChain,
	)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	require.Equal(t, survivor.ID, remaining[0].ID)
	require.ElementsMatch(
		t, []wire.OutPoint{opOther}, remaining[0].Anchors,
	)
}
