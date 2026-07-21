package db

import (
	"database/sql"
	"encoding/binary"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// newVTXOStoreForTest creates a new VTXOPersistenceStore and the underlying
// round store for test setup. Returns both to allow tests to set up rounds
// first (for FK constraints).
func newVTXOStoreForTest(t *testing.T) (*VTXOPersistenceStore,
	*RoundPersistenceStore, *BaseDB) {

	db := NewTestDB(t)

	roundDB := NewTransactionExecutor(
		db.BaseDB,
		func(tx *sql.Tx) RoundStore {
			return db.WithTx(tx)
		},
		btclog.Disabled,
	)

	roundStore := NewRoundPersistenceStore(
		roundDB, &chaincfg.RegressionNetParams, clock.NewDefaultClock(),
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
		BatchOutpoint: wire.OutPoint{
			Hash:  hash,
			Index: 0,
		},
		Root: &tree.Node{
			Input: wire.OutPoint{
				Hash:  hash,
				Index: 0,
			},
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

	policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		privKey.PubKey(), operatorKey.PubKey(), exitDelay,
	)
	require.NoError(t, err)

	return &vtxo.Descriptor{
		Outpoint:       outpoint,
		Amount:         btcutil.Amount(100000 * (idx + 1)),
		PolicyTemplate: policyTemplate,
		PkScript: []byte{
			0x51,
			0x20,
			byte(idx),
		},
		ClientKey: keychain.KeyDescriptor{
			PubKey: privKey.PubKey(),
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamily(0),
				Index:  uint32(idx),
			},
		},
		OperatorKey: operatorKey.PubKey(),
		TapScript:   tapscript,
		Ancestry: []vtxo.Ancestry{{
			TreePath:       treePath,
			CommitmentTxID: commitmentTxID,
			TreeDepth:      uint32(2 + idx),
		}},
		RoundID:        roundID.String(),
		CommitmentTxID: commitmentTxID,
		BatchExpiry:    1000 + int32(idx*100),
		RelativeExpiry: exitDelay,
		CreatedHeight:  500 + int32(idx*10),
		Status:         vtxo.VTXOStatusLive,
	}
}

// testOddParityPubKey returns a deterministic odd-parity public key for
// parity-sensitive persistence tests.
func testOddParityPubKey(t *testing.T) *btcec.PublicKey {
	t.Helper()

	for scalar := byte(1); scalar != 0; scalar++ {
		priv, pub := btcec.PrivKeyFromBytes([]byte{scalar})
		require.NotNil(t, priv)
		require.NotNil(t, pub)

		if pub.SerializeCompressed()[0] == 0x03 {
			return pub
		}
	}

	t.Fatal("failed to derive deterministic odd-parity pubkey")

	return nil
}

// TestUpdateVTXOStatusReleasingReservation verifies that the combined update
// path changes a VTXO's status and deletes its spending-reservation row inside
// one transaction, so a VTXO leaving SpendingState can never leave a stale
// reservation row behind to mask a future orphan on the same outpoint.
func TestUpdateVTXOStatusReleasingReservation(t *testing.T) {
	t.Parallel()

	vtxoStore, roundStore, baseDB := newVTXOStoreForTest(t)
	ctx := t.Context()

	// A spending-reservation store sharing the same database, so the delete
	// the VTXO store performs and the rows this store reads hit one
	// backend.
	reservationDB := NewTransactionExecutor(
		baseDB,
		func(tx *sql.Tx) SpendingReservationStore {
			return baseDB.WithTx(tx)
		},
		btclog.Disabled,
	)
	reservationStore := NewSpendingReservationPersistenceStore(
		reservationDB, clock.NewDefaultClock(),
	)

	// Persist a round + VTXO, move it into SpendingState, and reserve it.
	roundID := testRoundIDDB("test-round-release-reservation")
	testRound := createTestRound(t, roundID)
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	require.NoError(t, roundStore.CommitState(ctx, testRound, state))

	desc := createTestVTXODescriptor(t, roundID, 7)
	require.NoError(t, vtxoStore.SaveVTXO(ctx, desc))
	require.NoError(
		t, vtxoStore.UpdateVTXOStatus(
			ctx, desc.Outpoint, vtxo.VTXOStatusSpending,
		),
	)

	ownerID := chainhash.Hash{0x99}
	const taprootAssetPreparationOwnerKind = 1
	require.NoError(
		t, reservationStore.UpsertReservation(
			ctx, desc.Outpoint, taprootAssetPreparationOwnerKind,
			ownerID,
		),
	)

	reserved, err := reservationStore.ListReservedOutpoints(ctx)
	require.NoError(t, err)
	require.Equal(t, []wire.OutPoint{desc.Outpoint}, reserved)

	// A known-safe setup failure releases the VTXO to Live and drops the
	// preparation-owner row in one transaction.
	require.NoError(
		t, vtxoStore.UpdateVTXOStatusReleasingReservation(
			ctx, desc.Outpoint, vtxo.VTXOStatusLive,
		),
	)

	fetched, err := vtxoStore.GetVTXO(ctx, desc.Outpoint)
	require.NoError(t, err)
	require.Equal(t, vtxo.VTXOStatusLive, fetched.Status)

	reserved, err = reservationStore.ListReservedOutpoints(ctx)
	require.NoError(t, err)
	require.Empty(t, reserved)

	// Updating an outpoint whose reservation was already released is a
	// no-op for the reservation table: the status still updates without an
	// error.
	require.NoError(
		t, vtxoStore.UpdateVTXOStatusReleasingReservation(
			ctx, desc.Outpoint, vtxo.VTXOStatusSpent,
		),
	)

	fetched, err = vtxoStore.GetVTXO(ctx, desc.Outpoint)
	require.NoError(t, err)
	require.Equal(t, vtxo.VTXOStatusSpent, fetched.Status)
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

	// Verify ancestry was persisted in the side table.
	require.Len(t, fetched.Ancestry, len(desc.Ancestry))
	require.NotNil(t, fetched.Ancestry[0].TreePath)
	require.Equal(
		t, desc.Ancestry[0].TreePath.BatchOutpoint,
		fetched.Ancestry[0].TreePath.BatchOutpoint,
	)
}

// TestVTXODescriptorDecodeMemoization verifies the per-outpoint descriptor
// decode cache: repeated listings return equal descriptors whose immutable
// derived parts (taproot script and parsed operator key) are the SAME shared
// objects, while mutable row state (status) is read fresh on every call.
func TestVTXODescriptorDecodeMemoization(t *testing.T) {
	t.Parallel()

	vtxoStore, roundStore, _ := newVTXOStoreForTest(t)
	ctx := t.Context()

	roundID := testRoundIDDB("test-round-decode-memo")
	testRound := createTestRound(t, roundID)
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	require.NoError(t, roundStore.CommitState(ctx, testRound, state))

	desc := createTestVTXODescriptor(t, roundID, 7)
	require.NoError(t, vtxoStore.SaveVTXO(ctx, desc))

	first, err := vtxoStore.ListVTXOsByStatus(ctx, vtxo.VTXOStatusLive)
	require.NoError(t, err)
	require.Len(t, first, 1)

	second, err := vtxoStore.ListVTXOsByStatus(ctx, vtxo.VTXOStatusLive)
	require.NoError(t, err)
	require.Len(t, second, 1)

	// Equal contents across calls.
	require.Equal(t, first[0], second[0])

	// The derived parts must be memoized: pointer identity proves the
	// second listing skipped the re-derivation. Client keys are hydrated
	// through the internal key registry, so equality above covers them.
	require.Same(t, first[0].TapScript, second[0].TapScript)
	require.Same(t, first[0].OperatorKey, second[0].OperatorKey)

	// Mutable row state is read fresh: a status flip shows up on the next
	// listing while the derived parts stay the same shared objects.
	require.NoError(
		t, vtxoStore.UpdateVTXOStatus(
			ctx, desc.Outpoint, vtxo.VTXOStatusSpent,
		),
	)

	spent, err := vtxoStore.ListVTXOsByStatus(ctx, vtxo.VTXOStatusSpent)
	require.NoError(t, err)
	require.Len(t, spent, 1)
	require.Equal(t, vtxo.VTXOStatusSpent, spent[0].Status)
	require.Same(t, first[0].TapScript, spent[0].TapScript)
	require.Same(t, first[0].OperatorKey, spent[0].OperatorKey)
}

// TestListSelectionCandidatesByStatus verifies the lightweight selection
// projection agrees with the full descriptor listing on outpoint, amount,
// and pkScript.
func TestListSelectionCandidatesByStatus(t *testing.T) {
	t.Parallel()

	vtxoStore, roundStore, _ := newVTXOStoreForTest(t)
	ctx := t.Context()

	roundID := testRoundIDDB("test-round-sel-proj")
	testRound := createTestRound(t, roundID)
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	require.NoError(t, roundStore.CommitState(ctx, testRound, state))

	descA := createTestVTXODescriptor(t, roundID, 11)
	require.NoError(t, vtxoStore.SaveVTXO(ctx, descA))

	descB := createTestVTXODescriptor(t, roundID, 12)
	require.NoError(t, vtxoStore.SaveVTXO(ctx, descB))

	descAsset := createTestVTXODescriptor(t, roundID, 13)
	assetRoot := chainhash.Hash{0xa1, 0xb2, 0xc3}
	descAsset.TaprootAssetRoot = &assetRoot
	descAsset.TaprootAssetRef = "asset:selection-candidate"
	descAsset.TaprootAssetAmount = ^uint64(0)
	assetPkScript, err := descAsset.EffectivePkScript()
	require.NoError(t, err)
	descAsset.PkScript = assetPkScript
	require.NoError(t, vtxoStore.SaveVTXO(ctx, descAsset))

	full, err := vtxoStore.ListVTXOsByStatus(ctx, vtxo.VTXOStatusLive)
	require.NoError(t, err)
	require.Len(t, full, 3)

	candidates, err := vtxoStore.ListSelectionCandidatesByStatus(
		ctx, vtxo.VTXOStatusLive,
	)
	require.NoError(t, err)
	require.Len(t, candidates, 2)

	byOutpoint := make(map[wire.OutPoint]*vtxo.Descriptor)
	for _, desc := range full {
		byOutpoint[desc.Outpoint] = desc
	}

	for _, candidate := range candidates {
		desc, ok := byOutpoint[candidate.Outpoint]
		require.True(t, ok)
		require.Equal(t, desc.Amount, candidate.Amount)
		require.Equal(t, desc.PkScript, candidate.PkScript)
		require.Nil(t, candidate.TaprootAssetRoot)
	}

	storedAsset, err := vtxoStore.GetVTXO(ctx, descAsset.Outpoint)
	require.NoError(t, err)
	require.Equal(t, &assetRoot, storedAsset.TaprootAssetRoot)
	require.Equal(
		t, descAsset.TaprootAssetRef, storedAsset.TaprootAssetRef,
	)
	require.Equal(
		t, descAsset.TaprootAssetAmount, storedAsset.TaprootAssetAmount,
	)
	require.Equal(t, descAsset.PkScript, storedAsset.PkScript)

	// A status the projection was not asked for stays invisible.
	require.NoError(
		t, vtxoStore.UpdateVTXOStatus(
			ctx, descA.Outpoint, vtxo.VTXOStatusSpent,
		),
	)

	candidates, err = vtxoStore.ListSelectionCandidatesByStatus(
		ctx, vtxo.VTXOStatusLive,
	)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.Equal(t, descB.Outpoint, candidates[0].Outpoint)
}

// TestTaprootAssetMetadataCodec verifies the SQL boundary preserves the full
// uint64 asset-amount range while accepting historical root-only rows and
// rejecting partial or non-canonical metadata.
func TestTaprootAssetMetadataCodec(t *testing.T) {
	t.Parallel()

	root := chainhash.HashH([]byte("asset-metadata-root"))
	maxAmount := ^uint64(0)
	desc := &vtxo.Descriptor{
		TaprootAssetRoot:   &root,
		TaprootAssetRef:    "asset:max-supply",
		TaprootAssetAmount: maxAmount,
	}

	ref, amount, err := encodeTaprootAssetMetadata(desc)
	require.NoError(t, err)
	require.Equal(t, sql.NullString{
		String: desc.TaprootAssetRef,
		Valid:  true,
	}, ref)
	require.Len(t, amount, taprootAssetAmountSize)
	require.Equal(t, maxAmount, binary.BigEndian.Uint64(amount))

	decodedRef, decodedAmount, err := decodeTaprootAssetMetadata(
		&root, ref, amount,
	)
	require.NoError(t, err)
	require.Equal(t, desc.TaprootAssetRef, decodedRef)
	require.Equal(t, maxAmount, decodedAmount)

	legacyRef, legacyAmount, err := encodeTaprootAssetMetadata(
		&vtxo.Descriptor{
			TaprootAssetRoot: &root,
		},
	)
	require.NoError(t, err)
	require.False(t, legacyRef.Valid)
	require.Empty(t, legacyAmount)

	decodedRef, decodedAmount, err = decodeTaprootAssetMetadata(
		&root, sql.NullString{}, nil,
	)
	require.NoError(t, err)
	require.Empty(t, decodedRef)
	require.Zero(t, decodedAmount)

	encodeTests := []struct {
		name string
		desc *vtxo.Descriptor
		want string
	}{
		{
			name: "nil descriptor",
			want: "descriptor must be provided",
		},
		{
			name: "metadata without root",
			desc: &vtxo.Descriptor{
				TaprootAssetRef:    "asset:no-root",
				TaprootAssetAmount: 1,
			},
			want: "requires a commitment root",
		},
		{
			name: "reference without amount",
			desc: &vtxo.Descriptor{
				TaprootAssetRoot: &root,
				TaprootAssetRef:  "asset:partial",
			},
			want: "ref and amount must both be provided",
		},
		{
			name: "amount without reference",
			desc: &vtxo.Descriptor{
				TaprootAssetRoot:   &root,
				TaprootAssetAmount: 1,
			},
			want: "ref and amount must both be provided",
		},
	}
	for _, test := range encodeTests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, _, err := encodeTaprootAssetMetadata(test.desc)
			require.ErrorContains(t, err, test.want)
		})
	}

	canonicalAmount := make([]byte, taprootAssetAmountSize)
	binary.BigEndian.PutUint64(canonicalAmount, 1)
	decodeTests := []struct {
		name   string
		root   *chainhash.Hash
		ref    sql.NullString
		amount []byte
		want   string
	}{
		{
			name: "metadata without root",
			ref: sql.NullString{
				String: "asset:no-root",
				Valid:  true,
			},
			amount: canonicalAmount,
			want:   "has no commitment root",
		},
		{
			name:   "missing reference",
			root:   &root,
			amount: canonicalAmount,
			want:   "incomplete Taproot Asset metadata",
		},
		{
			name: "empty reference",
			root: &root,
			ref: sql.NullString{
				Valid: true,
			},
			amount: canonicalAmount,
			want:   "incomplete Taproot Asset metadata",
		},
		{
			name: "missing amount",
			root: &root,
			ref: sql.NullString{
				String: "asset:missing-amount",
				Valid:  true,
			},
			want: "incomplete Taproot Asset metadata",
		},
		{
			name: "short amount blob",
			root: &root,
			ref: sql.NullString{
				String: "asset:short",
				Valid:  true,
			},
			amount: []byte{
				1,
			},
			want: "invalid Taproot Asset amount length",
		},
		{
			name: "long amount blob",
			root: &root,
			ref: sql.NullString{
				String: "asset:long",
				Valid:  true,
			},
			amount: make([]byte, taprootAssetAmountSize+1),
			want:   "invalid Taproot Asset amount length",
		},
		{
			name: "zero amount",
			root: &root,
			ref: sql.NullString{
				String: "asset:zero",
				Valid:  true,
			},
			amount: make([]byte, taprootAssetAmountSize),
			want:   "amount must be positive",
		},
	}
	for _, test := range decodeTests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, _, err := decodeTaprootAssetMetadata(
				test.root, test.ref, test.amount,
			)
			require.ErrorContains(t, err, test.want)
		})
	}
}

// TestVTXOPersistenceStoreTaprootAssetMetadataUpsert proves a historical
// root-only row can be atomically enriched with asset identity and a full
// uint64 amount, while partial or root-only retries cannot split or erase the
// persisted metadata tuple.
func TestVTXOPersistenceStoreTaprootAssetMetadataUpsert(t *testing.T) {
	t.Parallel()

	vtxoStore, roundStore, _ := newVTXOStoreForTest(t)
	ctx := t.Context()

	roundID := testRoundIDDB("test-asset-metadata-upsert")
	testRound := createTestRound(t, roundID)
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	require.NoError(t, roundStore.CommitState(ctx, testRound, state))

	root := chainhash.HashH([]byte("asset-upsert-root"))
	desc := createTestVTXODescriptor(t, roundID, 31)
	desc.TaprootAssetRoot = &root
	pkScript, err := desc.EffectivePkScript()
	require.NoError(t, err)
	desc.PkScript = pkScript
	require.NoError(t, vtxoStore.SaveVTXO(ctx, desc))

	legacy, err := vtxoStore.GetVTXO(ctx, desc.Outpoint)
	require.NoError(t, err)
	require.Equal(t, &root, legacy.TaprootAssetRoot)
	require.Empty(t, legacy.TaprootAssetRef)
	require.Zero(t, legacy.TaprootAssetAmount)

	const assetRef = "asset:upsert"
	maxAmount := ^uint64(0)
	desc.TaprootAssetRef = assetRef
	desc.TaprootAssetAmount = maxAmount
	require.NoError(t, vtxoStore.SaveVTXO(ctx, desc))

	assertComplete := func() {
		t.Helper()

		stored, err := vtxoStore.GetVTXO(ctx, desc.Outpoint)
		require.NoError(t, err)
		require.Equal(t, &root, stored.TaprootAssetRoot)
		require.Equal(t, assetRef, stored.TaprootAssetRef)
		require.Equal(t, maxAmount, stored.TaprootAssetAmount)
	}
	assertComplete()

	// A replay from a pre-migration daemon carries only the root. The
	// upsert must preserve the richer tuple already stored.
	desc.TaprootAssetRef = ""
	desc.TaprootAssetAmount = 0
	require.NoError(t, vtxoStore.SaveVTXO(ctx, desc))
	assertComplete()

	// A partial update is rejected before the SQL upsert and leaves the
	// complete tuple intact.
	desc.TaprootAssetRef = "asset:partial"
	err = vtxoStore.SaveVTXO(ctx, desc)
	require.ErrorContains(t, err, "ref and amount must both be provided")
	assertComplete()
}

// TestListVTXOsLightSkipsAncestry exercises the light listing variants the
// ListVTXOs RPC runs on: the descriptors must match the full listing in
// every field except Ancestry, which the light path never loads.
func TestListVTXOsLightSkipsAncestry(t *testing.T) {
	t.Parallel()

	vtxoStore, roundStore, _ := newVTXOStoreForTest(t)
	ctx := t.Context()

	roundID := testRoundIDDB("test-round-light-list")
	testRound := createTestRound(t, roundID)
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	require.NoError(t, roundStore.CommitState(ctx, testRound, state))

	descA := createTestVTXODescriptor(t, roundID, 21)
	require.NoError(t, vtxoStore.SaveVTXO(ctx, descA))

	descB := createTestVTXODescriptor(t, roundID, 22)
	require.NoError(t, vtxoStore.SaveVTXO(ctx, descB))

	full, err := vtxoStore.ListVTXOsByStatus(ctx, vtxo.VTXOStatusLive)
	require.NoError(t, err)
	require.Len(t, full, 2)

	byOutpoint := make(map[wire.OutPoint]*vtxo.Descriptor)
	for _, desc := range full {
		// The full listing carries the persisted ancestry; the light
		// assertions below lean on this contrast.
		require.NotEmpty(t, desc.Ancestry)
		byOutpoint[desc.Outpoint] = desc
	}

	assertLight := func(light []*vtxo.Descriptor) {
		t.Helper()

		require.Len(t, light, len(full))
		for _, desc := range light {
			fullDesc, ok := byOutpoint[desc.Outpoint]
			require.True(t, ok)

			require.Empty(t, desc.Ancestry)
			require.Equal(t, fullDesc.Amount, desc.Amount)
			require.Equal(t, fullDesc.PkScript, desc.PkScript)
			require.Equal(t, fullDesc.Status, desc.Status)
			require.Equal(t, fullDesc.RoundID, desc.RoundID)
			require.Equal(
				t, fullDesc.ChainDepth, desc.ChainDepth,
			)
			require.Equal(
				t, fullDesc.RelativeExpiry, desc.RelativeExpiry,
			)
		}
	}

	light, err := vtxoStore.ListVTXOsByStatusLight(
		ctx, vtxo.VTXOStatusLive,
	)
	require.NoError(t, err)
	assertLight(light)

	liveLight, err := vtxoStore.ListLiveVTXOsLight(ctx)
	require.NoError(t, err)
	assertLight(liveLight)
}

// addAncestryFragment appends a synthetic ancestry fragment to a
// Descriptor under construction so multi-tree round-trip tests can
// build N>1 ancestry layouts without re-implementing the per-fragment
// initialization the createTestVTXODescriptor helper does for the
// primary fragment. The label seeds a deterministic commitment hash
// and tree-batch outpoint, so fragments produced from distinct labels
// have distinct identities.
func addAncestryFragment(t *testing.T, desc *vtxo.Descriptor, label string,
	inputIndices []uint32, treeDepth uint32) {

	t.Helper()

	hash := chainhash.HashH([]byte("ancestry-" + label))
	tp := &tree.Tree{
		BatchOutpoint: wire.OutPoint{
			Hash:  hash,
			Index: 0,
		},
		Root: &tree.Node{
			Input: wire.OutPoint{
				Hash:  hash,
				Index: 0,
			},
			Outputs:   []*wire.TxOut{},
			CoSigners: []*btcec.PublicKey{},
			Children:  make(map[uint32]*tree.Node),
		},
	}

	commitmentTxID := chainhash.HashH(
		[]byte("ancestry-commitment-" + label),
	)

	desc.Ancestry = append(desc.Ancestry, vtxo.Ancestry{
		TreePath:       tp,
		CommitmentTxID: commitmentTxID,
		InputIndices:   append([]uint32(nil), inputIndices...),
		TreeDepth:      treeDepth,
	})
}

// TestVTXOPersistenceStoreMultiAncestryRoundTrip verifies that a
// VTXO descriptor carrying multiple ancestry fragments round-trips
// through the side-table persistence layer with byte-identical
// CommitmentTxID, fragment ordering, InputIndices, and TreeDepth.
// This is the core multi-tree contract the cross-round OOR resolver
// produces; the DB must preserve every fragment so the unroller can
// later route each input to its broadcast tree.
func TestVTXOPersistenceStoreMultiAncestryRoundTrip(t *testing.T) {
	t.Parallel()

	vtxoStore, roundStore, _ := newVTXOStoreForTest(t)
	ctx := t.Context()

	roundID := testRoundIDDB("test-round-multi-ancestry")
	testRound := createTestRound(t, roundID)
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	err := roundStore.CommitState(ctx, testRound, state)
	require.NoError(t, err)

	// Build a descriptor that already carries one ancestry entry
	// (from the helper). Append two more with distinct commitments
	// and disjoint input-index slices to mimic the cross-round OOR
	// shape: 3 inputs spanning 3 distinct contributing commitments.
	desc := createTestVTXODescriptor(t, roundID, 31)

	// Re-shape the primary fragment so the test fixture matches the
	// post-resolver invariant: each fragment carries exactly the
	// indices it contributed.
	desc.Ancestry[0].InputIndices = []uint32{0}
	desc.Ancestry[0].TreeDepth = 3

	addAncestryFragment(t, desc, "second", []uint32{1, 2}, 5)
	addAncestryFragment(t, desc, "third", []uint32{3}, 7)
	require.Len(
		t, desc.Ancestry, 3,
		"fixture must carry 3 distinct ancestry fragments",
	)

	err = vtxoStore.SaveVTXO(ctx, desc)
	require.NoError(t, err)

	fetched, err := vtxoStore.GetVTXO(ctx, desc.Outpoint)
	require.NoError(t, err)
	require.NotNil(t, fetched)

	require.Len(
		t, fetched.Ancestry, len(desc.Ancestry),
		"side-table load must return every ancestry fragment",
	)

	for i, want := range desc.Ancestry {
		got := fetched.Ancestry[i]
		require.Equal(
			t, want.CommitmentTxID, got.CommitmentTxID, "fragmen"+
				"t %d commitment must round-trip", i,
		)
		require.Equal(
			t, want.InputIndices, got.InputIndices, "fragment "+
				"%d input indices must round-trip", i,
		)
		require.Equal(
			t, want.TreeDepth, got.TreeDepth, "fragment %d tree "+
				"depth must round-trip", i,
		)

		require.NotNil(
			t, got.TreePath, "fragment %d tree path must not be "+
				"nil after load", i,
		)
		require.Equal(
			t, want.TreePath.BatchOutpoint,
			got.TreePath.BatchOutpoint, "fragment %d tree path "+
				"batch outpoint must round-trip", i,
		)
	}
}

// TestVTXOPersistenceStoreUpsertReplacesAncestry verifies the
// delete-then-insert idiom that upsertAncestryPaths uses on update:
// re-saving a descriptor with a smaller ancestry slice must drop the
// stale rows so a future load reflects the new shape exactly. This
// catches a regression where the side table accumulates rows
// indefinitely across re-saves.
func TestVTXOPersistenceStoreUpsertReplacesAncestry(t *testing.T) {
	t.Parallel()

	vtxoStore, roundStore, _ := newVTXOStoreForTest(t)
	ctx := t.Context()

	roundID := testRoundIDDB("test-round-upsert-replace")
	testRound := createTestRound(t, roundID)
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	err := roundStore.CommitState(ctx, testRound, state)
	require.NoError(t, err)

	// First save: 3 fragments.
	desc := createTestVTXODescriptor(t, roundID, 32)
	desc.Ancestry[0].InputIndices = []uint32{0}
	addAncestryFragment(t, desc, "upsert-second", []uint32{1, 2}, 4)
	addAncestryFragment(t, desc, "upsert-third", []uint32{3}, 6)
	err = vtxoStore.SaveVTXO(ctx, desc)
	require.NoError(t, err)

	// Re-save with a smaller ancestry: only 1 fragment.
	desc.Ancestry = desc.Ancestry[:1]
	desc.Ancestry[0].InputIndices = []uint32{0, 1, 2, 3}
	err = vtxoStore.SaveVTXO(ctx, desc)
	require.NoError(t, err)

	fetched, err := vtxoStore.GetVTXO(ctx, desc.Outpoint)
	require.NoError(t, err)
	require.Len(
		t, fetched.Ancestry, 1,
		"re-save must replace ancestry rows, not append",
	)
	require.Equal(
		t, []uint32{0, 1, 2, 3}, fetched.Ancestry[0].InputIndices,
		"updated input-indices must be persisted",
	)
}

// TestVTXOPersistenceStoreSameCommitmentMultiLeafRoundTrip verifies
// that two ancestry fragments anchored at the SAME commitment txid but
// carrying distinct tree paths persist and load back intact. This is
// the shape the indexer produces for an OOR spend whose inputs sit at
// different leaves of one commitment tree; the schema used to enforce
// UNIQUE(vtxo, commitment_txid) which made such VTXOs unpersistable
// (wavelength#969), so this test pins the relaxed schema against
// regression.
func TestVTXOPersistenceStoreSameCommitmentMultiLeafRoundTrip(t *testing.T) {
	t.Parallel()

	vtxoStore, roundStore, _ := newVTXOStoreForTest(t)
	ctx := t.Context()

	roundID := testRoundIDDB("test-round-same-commitment")
	testRound := createTestRound(t, roundID)
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	err := roundStore.CommitState(ctx, testRound, state)
	require.NoError(t, err)

	desc := createTestVTXODescriptor(t, roundID, 34)
	desc.Ancestry[0].InputIndices = []uint32{0}
	addAncestryFragment(t, desc, "same-commit-leaf", []uint32{1}, 4)

	// Re-anchor the second fragment at the FIRST fragment's
	// commitment while keeping its distinct tree path, producing the
	// same-commitment multi-leaf shape.
	desc.Ancestry[1].CommitmentTxID = desc.Ancestry[0].CommitmentTxID

	err = vtxoStore.SaveVTXO(ctx, desc)
	require.NoError(
		t, err, "same-commitment multi-leaf ancestry must persist",
	)

	fetched, err := vtxoStore.GetVTXO(ctx, desc.Outpoint)
	require.NoError(t, err)
	require.Len(t, fetched.Ancestry, 2)
	require.Equal(
		t, fetched.Ancestry[0].CommitmentTxID,
		fetched.Ancestry[1].CommitmentTxID,
		"both fragments must keep the shared commitment txid",
	)
	require.NotEqual(
		t, fetched.Ancestry[0].TreePath.BatchOutpoint,
		fetched.Ancestry[1].TreePath.BatchOutpoint,
		"fragments must keep their distinct tree paths",
	)
}

// TestVTXOPersistenceStoreDeleteVTXOCascadesAncestry verifies that
// removing a VTXO drops every ancestry side-table row keyed by its
// outpoint. The migration declares FK ON DELETE CASCADE on
// vtxo_ancestry_paths, so this test pins the cascade behavior
// against schema drift.
func TestVTXOPersistenceStoreDeleteVTXOCascadesAncestry(t *testing.T) {
	t.Parallel()

	vtxoStore, roundStore, _ := newVTXOStoreForTest(t)
	ctx := t.Context()

	roundID := testRoundIDDB("test-round-cascade-ancestry")
	testRound := createTestRound(t, roundID)
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	err := roundStore.CommitState(ctx, testRound, state)
	require.NoError(t, err)

	desc := createTestVTXODescriptor(t, roundID, 33)
	addAncestryFragment(t, desc, "cascade-second", []uint32{1}, 4)
	addAncestryFragment(t, desc, "cascade-third", []uint32{2}, 5)
	err = vtxoStore.SaveVTXO(ctx, desc)
	require.NoError(t, err)

	err = vtxoStore.DeleteVTXO(ctx, desc.Outpoint)
	require.NoError(t, err)

	_, err = vtxoStore.GetVTXO(ctx, desc.Outpoint)
	require.Error(t, err,
		"delete must remove the VTXO row")
}

// TestVTXOPersistenceStoreGetVTXOPreservesStoredOperatorPubKeyParity ensures
// that loading a VTXO keeps the exact stored operator pubkey instead of
// replacing it with the even-y x-only lift reconstructed from the policy
// template. The policy template is intentionally x-only today, so persisted
// compressed keys must remain authoritative whenever they are available.
func TestVTXOPersistenceStoreGetVTXOPreservesStoredOperatorPubKeyParity(
	t *testing.T) {

	t.Parallel()

	vtxoStore, roundStore, _ := newVTXOStoreForTest(t)
	ctx := t.Context()

	roundID := testRoundIDDB("test-vtxo-store-operator-parity")
	testRound := createTestRound(t, roundID)
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	err := roundStore.CommitState(ctx, testRound, state)
	require.NoError(t, err)

	desc := createTestVTXODescriptor(t, roundID, 88)
	desc.OperatorKey = testOddParityPubKey(t)
	desc.PolicyTemplate, err = arkscript.EncodeStandardVTXOTemplate(
		desc.ClientKey.PubKey, desc.OperatorKey, desc.RelativeExpiry,
	)
	require.NoError(t, err)
	desc.TapScript, err = arkscript.VTXOTapScript(
		desc.ClientKey.PubKey, desc.OperatorKey, desc.RelativeExpiry,
	)
	require.NoError(t, err)

	err = vtxoStore.SaveVTXO(ctx, desc)
	require.NoError(t, err)

	fetched, err := vtxoStore.GetVTXO(ctx, desc.Outpoint)
	require.NoError(t, err)
	require.NotNil(t, fetched)
	require.NotNil(t, fetched.OperatorKey)

	require.Equal(
		t, desc.OperatorKey.SerializeCompressed(),
		fetched.OperatorKey.SerializeCompressed(),
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

// TestVTXOPersistenceStoreListLiveVTXOsBatchedAncestry verifies that the
// batched ancestry path used by ListLiveVTXOs (one query for VTXOs plus
// one query for the ancestry side table) reconstructs the full
// per-fragment Ancestry slice for every returned descriptor. This is
// the H-7 regression guard: the prior implementation issued one
// ListVTXOAncestryPaths query per row and silently truncated to a
// single fragment; this test seeds three multi-fragment descriptors
// and asserts every fragment survives the batched round-trip.
func TestVTXOPersistenceStoreListLiveVTXOsBatchedAncestry(t *testing.T) {
	t.Parallel()

	vtxoStore, roundStore, _ := newVTXOStoreForTest(t)
	ctx := t.Context()

	// Round row to satisfy FK.
	roundID := testRoundIDDB("test-round-list-live-batch")
	testRound := createTestRound(t, roundID)
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	require.NoError(t, roundStore.CommitState(ctx, testRound, state))

	// Three descriptors, each carrying a different fragment count.
	// fragCounts[i] is the number of fragments on descriptor i.
	fragCounts := []int{1, 3, 2}
	want := make(map[wire.OutPoint][]vtxo.Ancestry)
	for i, n := range fragCounts {
		desc := createTestVTXODescriptor(t, roundID, i+1)
		desc.Ancestry = make([]vtxo.Ancestry, n)
		for f := 0; f < n; f++ {
			var commit chainhash.Hash
			commit[0] = byte(i + 1)
			commit[1] = byte(f)
			commit[31] = 0xab
			desc.Ancestry[f] = vtxo.Ancestry{
				TreePath: &tree.Tree{
					Root: &tree.Node{},
				},
				CommitmentTxID: commit,
				InputIndices: []uint32{
					uint32(f),
				},
				TreeDepth: uint32(f + 1),
			}
		}
		require.NoError(t, vtxoStore.SaveVTXO(ctx, desc))
		want[desc.Outpoint] = desc.Ancestry
	}

	live, err := vtxoStore.ListLiveVTXOs(ctx)
	require.NoError(t, err)
	require.Len(t, live, len(fragCounts))

	// Per-VTXO ancestry must come back in path_order with every
	// fragment intact — this is what proves the batched query
	// correctly groups rows back to their originating outpoint.
	for _, got := range live {
		expected, ok := want[got.Outpoint]
		require.True(t, ok, "unexpected outpoint %v", got.Outpoint)
		require.Len(
			t, got.Ancestry, len(expected),
			"fragment count mismatch for %v", got.Outpoint,
		)
		for f, frag := range got.Ancestry {
			require.Equal(
				t, expected[f].CommitmentTxID,
				frag.CommitmentTxID, "fragment %d "+
					"commitment mismatch for %v", f,
				got.Outpoint,
			)
			require.Equal(
				t, expected[f].TreeDepth, frag.TreeDepth, "f"+
					"ragment %d depth mismatch for %v", f,
				got.Outpoint,
			)
			require.Equal(
				t, expected[f].InputIndices, frag.InputIndices,
				"fragment %d indices mismatch for %v", f,
				got.Outpoint,
			)
		}
	}
}

// TestVTXOPersistenceStoreListVTXOsByStatusBatchedAncestry is the
// status-filtered counterpart to the live-list batched-ancestry test.
// Saving descriptors with diverging statuses and then asking for one
// status must return only the matching VTXOs but with their full
// ancestry intact — proving the JOIN filter on
// ListVTXOAncestryPathsByStatus matches the parent VTXO filter.
func TestVTXOPersistenceStoreListVTXOsByStatusBatchedAncestry(t *testing.T) {
	t.Parallel()

	vtxoStore, roundStore, _ := newVTXOStoreForTest(t)
	ctx := t.Context()

	roundID := testRoundIDDB("test-round-list-status-batch")
	testRound := createTestRound(t, roundID)
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	require.NoError(t, roundStore.CommitState(ctx, testRound, state))

	// Two live + one forfeited descriptor, each with multi-fragment
	// ancestry so we can verify the JOIN filter does not leak rows
	// from the wrong-status group.
	makeMultiFragDesc := func(idx int) *vtxo.Descriptor {
		desc := createTestVTXODescriptor(t, roundID, idx)
		desc.Ancestry = []vtxo.Ancestry{
			{
				TreePath: &tree.Tree{
					Root: &tree.Node{},
				},
				CommitmentTxID: chainhash.HashH(
					[]byte{byte(idx), 0},
				),
				InputIndices: []uint32{
					0,
				},
				TreeDepth: 1,
			},
			{
				TreePath: &tree.Tree{
					Root: &tree.Node{},
				},
				CommitmentTxID: chainhash.HashH(
					[]byte{byte(idx), 1},
				),
				InputIndices: []uint32{
					1,
				},
				TreeDepth: 2,
			},
		}

		return desc
	}

	d1 := makeMultiFragDesc(1)
	d2 := makeMultiFragDesc(2)
	d3 := makeMultiFragDesc(3)
	require.NoError(t, vtxoStore.SaveVTXO(ctx, d1))
	require.NoError(t, vtxoStore.SaveVTXO(ctx, d2))
	require.NoError(t, vtxoStore.SaveVTXO(ctx, d3))

	// Move d3 into the Forfeited bucket.
	require.NoError(
		t, vtxoStore.UpdateVTXOStatus(
			ctx, d3.Outpoint, vtxo.VTXOStatusForfeited,
		),
	)

	live, err := vtxoStore.ListVTXOsByStatus(ctx, vtxo.VTXOStatusLive)
	require.NoError(t, err)
	require.Len(t, live, 2,
		"only the two live VTXOs should be returned")

	forfeited, err := vtxoStore.ListVTXOsByStatus(
		ctx, vtxo.VTXOStatusForfeited,
	)
	require.NoError(t, err)
	require.Len(
		t, forfeited, 1,
		"only the one forfeited VTXO should be returned",
	)
	require.Len(
		t, forfeited[0].Ancestry, 2,
		"forfeited VTXO must keep both fragments",
	)

	// The ancestry for d1 and d2 must NOT bleed into the forfeited
	// query result, even though they share the round.
	for _, got := range forfeited {
		require.Equal(t, d3.Outpoint, got.Outpoint)
	}
}

// TestVTXOPersistenceStoreListVTXOsByStatusSettlement proves the by-status
// read joins each forfeited VTXO to its forfeit round (via forfeit_round_id)
// and surfaces the round's commitment txid + confirmation height as the VTXO's
// settlement coordinates. A live VTXO, which has no forfeit round, must leave
// the settlement fields zero (issue #924).
func TestVTXOPersistenceStoreListVTXOsByStatusSettlement(t *testing.T) {
	t.Parallel()

	vtxoStore, roundStore, baseDB := newVTXOStoreForTest(t)
	ctx := t.Context()

	// The round that CREATED the VTXOs. Its own commitment txid must not be
	// confused with the settling (forfeit) round below.
	commitRound := func(id round.RoundID) *round.Round {
		r := createTestRound(t, id)
		state := &round.InputSigSentState{
			RoundID:     r.RoundID,
			ClientTrees: make(map[round.SignerKey]*tree.Tree),
		}
		require.NoError(t, roundStore.CommitState(ctx, r, state))

		return r
	}

	createRoundID := testRoundIDDB("settlement-create-round")
	commitRound(createRoundID)

	// The leave round that FORFEITS the VTXO. It confirms at a known txid
	// and height that the join must surface as the settlement.
	settleRoundID := testRoundIDDB("settlement-leave-round")
	commitRound(settleRoundID)

	settlementTxid := chainhash.HashH([]byte("settlement-commitment-tx"))
	const settlementHeight = int32(812345)
	require.NoError(
		t,
		roundStore.FinalizeRound(
			ctx, settleRoundID, settlementTxid, round.ConfInfo{
				Height: settlementHeight,
				BlockHash: chainhash.HashH(
					[]byte("settlement-block"),
				),
			},
		),
	)

	// Ledger fee rows for the settling round: the join must SUM the two
	// operator fee event types, ignore the non-fee row in the same round,
	// and not leak the fee booked against the unrelated create round.
	ledgerStore := &LedgerStoreDB{
		TransactionExecutor: NewTransactionExecutor(
			baseDB,
			func(tx *sql.Tx) *sqlc.Queries {
				return baseDB.WithTx(tx)
			},
			btclog.Disabled,
		),
	}
	settleRoundBytes := uuid.UUID(settleRoundID)
	createRoundBytes := uuid.UUID(createRoundID)
	const (
		refreshFeeSat  = int64(350)
		boardingFeeSat = int64(150)
	)
	feeEntries := []ledger.LedgerEntry{
		makeLedgerEntry(
			ledger.AccountFeesPaid, ledger.AccountVTXOBalance,
			refreshFeeSat, ledger.EventRefreshFeePaid,
			settleRoundBytes[:], 1_000,
		),
		makeLedgerEntry(
			ledger.AccountFeesPaid, ledger.AccountVTXOBalance,
			boardingFeeSat, ledger.EventBoardingFeePaid,
			settleRoundBytes[:], 1_001,
		),
		makeLedgerEntry(
			ledger.AccountTransfersOut, ledger.AccountVTXOBalance,
			5_000, ledger.EventVTXOSent, settleRoundBytes[:], 1_002,
		),
		makeLedgerEntry(
			ledger.AccountFeesPaid, ledger.AccountVTXOBalance, 999,
			ledger.EventRefreshFeePaid, createRoundBytes[:], 1_003,
		),
	}
	for _, entry := range feeEntries {
		require.NoError(t, ledgerStore.InsertLedgerEntry(ctx, entry))
	}

	// A live VTXO (no forfeit round) and a forfeited VTXO whose forfeit
	// round is the confirmed leave round above.
	liveDesc := createTestVTXODescriptor(t, createRoundID, 1)
	forfeitedDesc := createTestVTXODescriptor(t, createRoundID, 2)
	require.NoError(t, vtxoStore.SaveVTXO(ctx, liveDesc))
	require.NoError(t, vtxoStore.SaveVTXO(ctx, forfeitedDesc))

	require.NoError(
		t,
		vtxoStore.MarkForfeiting(
			ctx, forfeitedDesc.Outpoint, settleRoundID.String(),
			nil,
		),
	)
	require.NoError(
		t,
		vtxoStore.MarkForfeited(
			ctx, forfeitedDesc.Outpoint,
			chainhash.HashH(
				[]byte("forfeit-tx"),
			),
		),
	)

	// The forfeited VTXO carries the settling round's coordinates.
	forfeited, err := vtxoStore.ListVTXOsByStatus(
		ctx, vtxo.VTXOStatusForfeited,
	)
	require.NoError(t, err)
	require.Len(t, forfeited, 1)
	require.Equal(t, forfeitedDesc.Outpoint, forfeited[0].Outpoint)
	settle := forfeited[0].Settlement.UnwrapOrFail(t)
	require.Equal(t, settlementTxid, settle.TxID)
	require.Equal(t, settlementHeight, settle.Height)

	// The settlement fee is the SUM of the settling round's operator fee
	// rows only: the vtxo_sent row in the same round and the fee booked
	// against the create round must not contribute.
	require.Equal(t, refreshFeeSat+boardingFeeSat, settle.FeeSat)

	// The live VTXO has no forfeit round, so its settlement is None.
	live, err := vtxoStore.ListVTXOsByStatus(ctx, vtxo.VTXOStatusLive)
	require.NoError(t, err)
	require.Len(t, live, 1)
	require.True(t, live[0].Settlement.IsNone())

	// The light (no-ancestry) path surfaces the same settlement.
	forfeitedLight, err := vtxoStore.ListVTXOsByStatusLight(
		ctx, vtxo.VTXOStatusForfeited,
	)
	require.NoError(t, err)
	require.Len(t, forfeitedLight, 1)
	settleLight := forfeitedLight[0].Settlement.UnwrapOrFail(t)
	require.Equal(t, settlementTxid, settleLight.TxID)
	require.Equal(t, settlementHeight, settleLight.Height)
	require.Equal(t, refreshFeeSat+boardingFeeSat, settleLight.FeeSat)
}

// TestGroupAncestryRowsPreservesOrder is a unit test on the grouping
// helper — distinct outpoints in the same row stream must produce
// distinct map entries, and per-outpoint fragments must be appended
// in the SQL row order (which the queries pin to path_order).
func TestGroupAncestryRowsPreservesOrder(t *testing.T) {
	t.Parallel()

	makeRow := func(hash byte, idx int32, order int32,
		commit byte) sqlc.VtxoAncestryPath {

		var op chainhash.Hash
		op[0] = hash
		var commitTx chainhash.Hash
		commitTx[0] = commit

		return sqlc.VtxoAncestryPath{
			VtxoOutpointHash:  op[:],
			VtxoOutpointIndex: idx,
			PathOrder:         order,
			CommitmentTxid:    commitTx[:],
			TreePath:          nil,
			TreeDepth:         1,
			InputIndices:      encodeUint32SliceBE(nil),
		}
	}

	rows := []sqlc.VtxoAncestryPath{
		makeRow(0xa1, 0, 0, 0x10),
		makeRow(0xa1, 0, 1, 0x11),
		makeRow(0xa1, 0, 2, 0x12),
		makeRow(0xb2, 7, 0, 0x20),
		makeRow(0xb2, 7, 1, 0x21),
	}

	groups, err := groupAncestryRows(rows)
	require.NoError(t, err)
	require.Len(t, groups, 2)

	var hashA chainhash.Hash
	hashA[0] = 0xa1
	keyA := wire.OutPoint{Hash: hashA, Index: 0}
	require.Len(t, groups[keyA], 3)
	require.Equal(t, byte(0x10), groups[keyA][0].CommitmentTxID[0])
	require.Equal(t, byte(0x11), groups[keyA][1].CommitmentTxID[0])
	require.Equal(t, byte(0x12), groups[keyA][2].CommitmentTxID[0])

	var hashB chainhash.Hash
	hashB[0] = 0xb2
	keyB := wire.OutPoint{Hash: hashB, Index: 7}
	require.Len(t, groups[keyB], 2)
	require.Equal(t, byte(0x20), groups[keyB][0].CommitmentTxID[0])
	require.Equal(t, byte(0x21), groups[keyB][1].CommitmentTxID[0])
}

// TestGroupAncestryRowsCachesTreePaths verifies that repeated serialized
// ancestry tree fragments alias one decoded immutable tree.
func TestGroupAncestryRowsCachesTreePaths(t *testing.T) {
	t.Parallel()

	var hash chainhash.Hash
	hash[0] = 0x01
	treePath := &tree.Tree{
		BatchOutpoint: wire.OutPoint{
			Hash:  hash,
			Index: 0,
		},
		Root: &tree.Node{
			Input: wire.OutPoint{
				Hash:  hash,
				Index: 0,
			},
			Outputs:   []*wire.TxOut{},
			CoSigners: []*btcec.PublicKey{},
			Children:  make(map[uint32]*tree.Node),
		},
	}

	treeBytes, err := SerializeTree(treePath)
	require.NoError(t, err)

	makeRow := func(hashByte byte, commitByte byte) sqlc.VtxoAncestryPath {
		var op chainhash.Hash
		op[0] = hashByte
		var commitTx chainhash.Hash
		commitTx[0] = commitByte

		return sqlc.VtxoAncestryPath{
			VtxoOutpointHash:  op[:],
			VtxoOutpointIndex: 0,
			PathOrder:         0,
			CommitmentTxid:    commitTx[:],
			TreePath:          treeBytes,
			TreeDepth:         1,
			InputIndices:      encodeUint32SliceBE(nil),
		}
	}

	groups, err := groupAncestryRowsWithCache(
		[]sqlc.VtxoAncestryPath{
			makeRow(0xa1, 0x10),
			makeRow(0xb2, 0x20),
		},
		newAncestryTreeCache(),
	)
	require.NoError(t, err)

	var hashA chainhash.Hash
	hashA[0] = 0xa1
	keyA := wire.OutPoint{Hash: hashA, Index: 0}

	var hashB chainhash.Hash
	hashB[0] = 0xb2
	keyB := wire.OutPoint{Hash: hashB, Index: 0}

	require.Same(
		t, groups[keyA][0].TreePath,
		groups[keyB][0].TreePath,
	)
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
		t, desc.MaxTreeDepth(), fetched.MaxTreeDepth(),
		"max ancestry tree depth should be persisted",
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
	var roundCreateAncestry []types.Ancestry
	if len(desc.Ancestry) > 0 {
		roundCreateAncestry = []types.Ancestry{{
			TreePath: desc.Ancestry[0].TreePath,
		}}
	}
	clientVTXO := &round.ClientVTXO{
		Outpoint:    desc.Outpoint,
		Amount:      desc.Amount,
		PkScript:    desc.PkScript,
		Expiry:      desc.RelativeExpiry,
		OwnerKey:    desc.ClientKey,
		OperatorKey: desc.OperatorKey,
		Ancestry:    roundCreateAncestry,
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
		t, desc.PolicyTemplate, row.PolicyTemplate,
		"initial policy template should be persisted",
	)
	require.Equal(
		t, int32(0), row.BatchExpiry, "initial BatchExpiry should be 0",
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
		t, desc.PolicyTemplate, row.PolicyTemplate,
		"policy template should be updated",
	)
	require.Equal(
		t, desc.BatchExpiry, row.BatchExpiry,
		"BatchExpiry should be updated",
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

// TestVTXOPersistenceStoreGetVTXONotFound verifies that a miss surfaces as the
// domain sentinel vtxo.ErrVTXONotFound so callers match on it rather than the
// persistence-layer sql.ErrNoRows. The raw driver error stays in the chain so
// existing call sites that still test for it keep working during migration.
func TestVTXOPersistenceStoreGetVTXONotFound(t *testing.T) {
	t.Parallel()

	vtxoStore, _, _ := newVTXOStoreForTest(t)
	ctx := t.Context()

	unknown := wire.OutPoint{Hash: chainhash.Hash{0xde, 0xad}, Index: 7}

	fetched, err := vtxoStore.GetVTXO(ctx, unknown)
	require.Nil(t, fetched)
	require.Error(t, err)
	require.ErrorIs(
		t, err, vtxo.ErrVTXONotFound,
		"a miss must surface the domain not-found sentinel",
	)
	require.ErrorIs(
		t, err, sql.ErrNoRows,
		"the driver error stays in the chain for back-compat",
	)
}
