package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// newOORArtifactStoreForTest creates an artifact store backed by a fresh test
// database and transactional query adapter.
func newOORArtifactStoreForTest(t *testing.T) (*OORArtifactPersistenceStore,
	*RoundPersistenceStore) {

	t.Helper()

	db := NewTestDB(t)

	artifactDB := NewTransactionExecutor(
		db.BaseDB,
		func(tx *sql.Tx) OORArtifactStore {
			return db.WithTx(tx)
		},
		btclog.Disabled,
	)

	clk := clock.NewDefaultClock()

	roundDB := NewTransactionExecutor(
		db.BaseDB,
		func(tx *sql.Tx) RoundStore {
			return db.WithTx(tx)
		},
		btclog.Disabled,
	)
	roundStore := NewRoundPersistenceStore(
		roundDB, &chaincfg.RegressionNetParams, clk,
	)

	return NewOORArtifactPersistenceStore(artifactDB, clk), roundStore
}

// newOORArtifactStoreFromBaseDB creates an artifact store over an existing
// test database handle.
func newOORArtifactStoreFromBaseDB(
	baseDB *BaseDB) *OORArtifactPersistenceStore {

	artifactDB := NewTransactionExecutor(
		baseDB,
		func(tx *sql.Tx) OORArtifactStore {
			return baseDB.WithTx(tx)
		},
		btclog.Disabled,
	)

	return NewOORArtifactPersistenceStore(
		artifactDB, clock.NewDefaultClock(),
	)
}

// newOORArtifactStoreForTestPath opens an artifact store at a stable test
// database path so callers can close and reopen it to model restart.
func newOORArtifactStoreForTestPath(t *testing.T,
	dbPath string) (*OORArtifactPersistenceStore, *BaseDB) {

	t.Helper()

	testDB := NewTestDBHandleFromPath(t, dbPath)

	return newOORArtifactStoreFromBaseDB(testDB.BaseDB), testDB.BaseDB
}

func seedBindingOutpoint(t *testing.T, ctx context.Context,
	roundStore *RoundPersistenceStore, outpoint wire.OutPoint,
	pkScript []byte, valueSat int64) {

	t.Helper()

	roundID := testRoundIDDB("oor-bind-" + outpoint.String())
	testRound := createTestRound(t, roundID)
	state := &round.InputSigSentState{
		RoundID:     testRound.RoundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}
	err := roundStore.CommitState(ctx, testRound, state)
	require.NoError(t, err)

	clientVTXO := createTestClientVTXO(t, roundID, int(outpoint.Index)+1)
	clientVTXO.Outpoint = outpoint
	if len(pkScript) > 0 {
		clientVTXO.PkScript = append([]byte(nil), pkScript...)
	}
	if valueSat > 0 {
		clientVTXO.Amount = btcutil.Amount(valueSat)
	}

	err = roundStore.SaveVTXOs(ctx, []*round.ClientVTXO{clientVTXO})
	require.NoError(t, err)
}

// TestOORArtifactStoreGetPackageForOutpoint verifies that a persisted
// outpoint binding resolves to a full package bundle and preserves the matched
// binding identity.
func TestOORArtifactStoreGetPackageForOutpoint(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, roundStore := newOORArtifactStoreForTest(t)

	sessionID, arkPSBT, checkpoints, recipientOutpoint, recipientPkScript,
		valueSat, _ := buildTestOORPackage(t, 0x11)

	assetTransfer := &oortx.TaprootAssetTransfer{
		Version: oortx.TaprootAssetTransferVersion,
		CheckpointPackages: [][]byte{
			{
				0x01,
				0x02,
			},
		},
		ArkPackage: []byte{
			0x03,
			0x04,
		},
	}
	err := store.UpsertPackageWithAssets(
		ctx, OORPackageDirectionIncoming, sessionID, arkPSBT,
		checkpoints, assetTransfer,
	)
	require.NoError(t, err)

	seedBindingOutpoint(
		t, ctx, roundStore, recipientOutpoint, recipientPkScript,
		valueSat,
	)

	err = store.UpsertBinding(
		ctx, recipientOutpoint, sessionID, 0,
		OORPackageLinkKindCreatedOutput,
	)
	require.NoError(t, err)

	pkg, err := store.GetPackageForOutpoint(ctx, recipientOutpoint)
	require.NoError(t, err)
	require.NotNil(t, pkg)
	require.Equal(t, sessionID, pkg.SessionID)
	require.Equal(t, OORPackageDirectionIncoming, pkg.Direction)
	require.Len(t, pkg.FinalCheckpointPSBTs, 1)
	require.Equal(t, assetTransfer, pkg.TaprootAssetTransfer)
	require.True(t, pkg.MatchedOutpointBinding.IsSome())
	matched := pkg.MatchedOutpointBinding.UnsafeFromSome()
	require.Equal(t, OORPackageLinkKindCreatedOutput,
		matched.LinkKind)
	require.EqualValues(t, recipientOutpoint.Index,
		matched.Outpoint.Index)
	require.Equal(t, recipientOutpoint.Hash,
		matched.Outpoint.Hash)
}

// TestOORArtifactStoreListReceivedAndSentPackages verifies that package
// listings are correctly filtered by direction while still returning full
// bundle data.
func TestOORArtifactStoreListReceivedAndSentPackages(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, roundStore := newOORArtifactStoreForTest(t)

	incomingSession, incomingArk, incomingCheckpoints,
		incomingOutpoint, incomingScript, incomingValue, _ :=
		buildTestOORPackage(t, 0x21)

	err := store.UpsertPackage(
		ctx, OORPackageDirectionIncoming, incomingSession, incomingArk,
		incomingCheckpoints,
	)
	require.NoError(t, err)

	seedBindingOutpoint(
		t, ctx, roundStore, incomingOutpoint, incomingScript,
		incomingValue,
	)

	err = store.UpsertBinding(
		ctx, incomingOutpoint, incomingSession, 0,
		OORPackageLinkKindCreatedOutput,
	)
	require.NoError(t, err)

	outgoingSession, outgoingArk, outgoingCheckpoints, _, _, _,
		consumedInput := buildTestOORPackage(t, 0x31)

	err = store.UpsertPackage(
		ctx, OORPackageDirectionOutgoing, outgoingSession, outgoingArk,
		outgoingCheckpoints,
	)
	require.NoError(t, err)

	seedBindingOutpoint(t, ctx, roundStore, consumedInput, nil, 0)

	err = store.UpsertBinding(
		ctx, consumedInput, outgoingSession, 0,
		OORPackageLinkKindConsumedInput,
	)
	require.NoError(t, err)

	all, err := store.ListPackages(ctx, nil)
	require.NoError(t, err)
	require.Len(t, all, 2)

	received, err := store.ListReceivedPackages(ctx)
	require.NoError(t, err)
	require.Len(t, received, 1)
	require.Equal(t, OORPackageDirectionIncoming, received[0].Direction)

	sent, err := store.ListSentPackages(ctx)
	require.NoError(t, err)
	require.Len(t, sent, 1)
	require.Equal(t, OORPackageDirectionOutgoing, sent[0].Direction)
}

// TestOORArtifactStoreUpsertPackageDirectionConflict verifies direction is
// immutable once a session package row has been created.
func TestOORArtifactStoreUpsertPackageDirectionConflict(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _ := newOORArtifactStoreForTest(t)

	sessionID, arkPSBT, checkpoints, _, _, _, _ := buildTestOORPackage(
		t, 0x2f,
	)

	err := store.UpsertPackage(
		ctx, OORPackageDirectionIncoming, sessionID, arkPSBT,
		checkpoints,
	)
	require.NoError(t, err)

	err = store.UpsertPackage(
		ctx, OORPackageDirectionOutgoing, sessionID, arkPSBT,
		checkpoints,
	)
	require.Error(t, err)
	require.ErrorIs(t, err, types.ErrOORPackageDirectionConflict)
	require.ErrorContains(t, err, "package direction conflict")
}

// TestOORArtifactStoreUpsertPackageRejectsPayloadRewrite verifies same-session
// package persistence is retry-idempotent, not a rewrite surface.
func TestOORArtifactStoreUpsertPackageRejectsPayloadRewrite(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _ := newOORArtifactStoreForTest(t)

	sessionID, arkPSBT, checkpoints, _, _, _, _ := buildTestOORPackage(
		t, 0x35,
	)

	err := store.UpsertPackage(
		ctx, OORPackageDirectionIncoming, sessionID, arkPSBT,
		checkpoints,
	)
	require.NoError(t, err)

	err = store.UpsertPackage(
		ctx, OORPackageDirectionIncoming, sessionID, arkPSBT,
		checkpoints,
	)
	require.NoError(t, err)

	checkpoints[0].Inputs[0].FinalScriptWitness = []byte{
		0x01,
		0x51,
	}

	err = store.UpsertPackage(
		ctx, OORPackageDirectionIncoming, sessionID, arkPSBT,
		checkpoints,
	)
	require.Error(t, err)
	require.ErrorContains(t, err, "already exists with different payload")

	pkg, err := store.GetPackage(ctx, sessionID)
	require.NoError(t, err)
	require.Empty(
		t, pkg.FinalCheckpointPSBTs[0].Inputs[0].
			FinalScriptWitness,
	)
}

// TestOORArtifactStoreGetPackageForOutpointPrefersCreatedBinding verifies that
// outpoint lookups return the created-output package when both created and
// consumed bindings exist for the same outpoint.
func TestOORArtifactStoreGetPackageForOutpointPrefersCreatedBinding(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()
	store, roundStore := newOORArtifactStoreForTest(t)

	externalInput := wire.OutPoint{
		Hash: chainhash.Hash{
			0x63,
			0xaa,
		},
		Index: 0,
	}

	parentSession, parentArk, parentCheckpoints, parentOutpoint,
		parentScript, parentValue, _ := buildTestOORPackageWithInput(
		t, 0x63, externalInput,
	)

	err := store.UpsertPackage(
		ctx, OORPackageDirectionIncoming, parentSession, parentArk,
		parentCheckpoints,
	)
	require.NoError(t, err)

	seedBindingOutpoint(
		t, ctx, roundStore, parentOutpoint, parentScript, parentValue,
	)

	err = store.UpsertBinding(
		ctx, parentOutpoint, parentSession, 0,
		OORPackageLinkKindCreatedOutput,
	)
	require.NoError(t, err)

	childSession, childArk, childCheckpoints, _, _, _, _ :=
		buildTestOORPackageWithInput(t, 0x64, parentOutpoint)
	err = store.UpsertPackage(
		ctx, OORPackageDirectionOutgoing, childSession, childArk,
		childCheckpoints,
	)
	require.NoError(t, err)

	err = store.UpsertBinding(
		ctx, parentOutpoint, childSession, 0,
		OORPackageLinkKindConsumedInput,
	)
	require.NoError(t, err)

	pkg, err := store.GetPackageForOutpoint(ctx, parentOutpoint)
	require.NoError(t, err)
	require.NotNil(t, pkg)
	require.Equal(t, parentSession, pkg.SessionID)
	require.True(t, pkg.MatchedOutpointBinding.IsSome())
	matched := pkg.MatchedOutpointBinding.UnsafeFromSome()
	require.Equal(t, OORPackageLinkKindCreatedOutput,
		matched.LinkKind)

	// The proof-source lookup has a stricter contract than the legacy
	// convenience lookup above: it must select by created-output kind in
	// SQL rather than relying on row ordering when the same outpoint is
	// also consumed by another package.
	createdPkg, err := store.GetCreatedPackageForOutpoint(
		ctx, parentOutpoint,
	)
	require.NoError(t, err)
	require.NotNil(t, createdPkg)
	require.Equal(t, parentSession, createdPkg.SessionID)
	require.Equal(t, OORPackageDirectionIncoming, createdPkg.Direction)
	require.True(t, createdPkg.MatchedOutpointBinding.IsSome())
	createdBinding := createdPkg.MatchedOutpointBinding.UnsafeFromSome()
	require.Equal(
		t, OORPackageLinkKindCreatedOutput, createdBinding.LinkKind,
	)
	require.Equal(t, parentOutpoint, createdBinding.Outpoint)
}

// TestOORArtifactStoreBindingSessionConflict verifies a binding outpoint+kind
// cannot be rebound to a different session once persisted.
func TestOORArtifactStoreBindingSessionConflict(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, roundStore := newOORArtifactStoreForTest(t)

	sessionA, arkA, checkpointsA, outpointA, scriptA, valueA, _ :=
		buildTestOORPackage(t, 0x6a)
	sessionB, arkB, checkpointsB, _, _, _, _ :=
		buildTestOORPackage(t, 0x6b)

	err := store.UpsertPackage(
		ctx, OORPackageDirectionIncoming, sessionA, arkA, checkpointsA,
	)
	require.NoError(t, err)

	err = store.UpsertPackage(
		ctx, OORPackageDirectionIncoming, sessionB, arkB, checkpointsB,
	)
	require.NoError(t, err)

	seedBindingOutpoint(t, ctx, roundStore, outpointA, scriptA, valueA)

	err = store.UpsertBinding(
		ctx, outpointA, sessionA, 0, OORPackageLinkKindCreatedOutput,
	)
	require.NoError(t, err)

	err = store.UpsertBinding(
		ctx, outpointA, sessionB, 0, OORPackageLinkKindCreatedOutput,
	)
	require.Error(t, err)
	require.ErrorContains(t, err, "binding conflict")
}

// TestOORArtifactStoreBindingOutputIndexConflict verifies a persisted
// outpoint+kind binding keeps a stable output index.
func TestOORArtifactStoreBindingOutputIndexConflict(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, roundStore := newOORArtifactStoreForTest(t)

	sessionID, arkPSBT, checkpoints, outpoint, script, valueSat, _ :=
		buildTestOORPackage(t, 0x6c)

	err := store.UpsertPackage(
		ctx, OORPackageDirectionIncoming, sessionID, arkPSBT,
		checkpoints,
	)
	require.NoError(t, err)

	seedBindingOutpoint(t, ctx, roundStore, outpoint, script, valueSat)

	err = store.UpsertBinding(
		ctx, outpoint, sessionID, 0, OORPackageLinkKindCreatedOutput,
	)
	require.NoError(t, err)

	err = store.UpsertBinding(
		ctx, outpoint, sessionID, 1, OORPackageLinkKindCreatedOutput,
	)
	require.Error(t, err)
	require.ErrorContains(t, err, "binding output index conflict")
}

// TestOORArtifactStoreBindingRequiresExistingVTXO verifies bindings can only
// be recorded for outpoints that exist in the local VTXO store.
func TestOORArtifactStoreBindingRequiresExistingVTXO(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _ := newOORArtifactStoreForTest(t)

	sessionID, arkPSBT, checkpoints, outpoint, _, _, _ :=
		buildTestOORPackage(t, 0x7b)

	err := store.UpsertPackage(
		ctx, OORPackageDirectionIncoming, sessionID, arkPSBT,
		checkpoints,
	)
	require.NoError(t, err)

	err = store.UpsertBinding(
		ctx, outpoint, sessionID, 0, OORPackageLinkKindCreatedOutput,
	)
	require.Error(t, err)
	require.ErrorIs(t, err, types.ErrOORBindingOutpointNotFound)
	require.ErrorContains(t, err, "not found in local vtxo store")
}

// TestOORArtifactStoreRecipientCursorCRUD verifies recipient cursor upsert,
// lookup, and listing behavior for both insert and update paths.
func TestOORArtifactStoreRecipientCursorCRUD(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _ := newOORArtifactStoreForTest(t)

	scriptA := []byte{0x51, 0x20, 0x01}
	scriptB := []byte{0x51, 0x20, 0x02}
	sessionA := chainhash.Hash{0xaa, 0x01}
	sessionB := chainhash.Hash{0xbb, 0x02}

	err := store.UpsertRecipientCursor(ctx, scriptA, 10, &sessionA)
	require.NoError(t, err)

	cursor, err := store.GetRecipientCursor(ctx, scriptA)
	require.NoError(t, err)
	require.Equal(t, scriptA, cursor.RecipientPkScript)
	require.Equal(t, int64(10), cursor.LastEventID)
	require.Equal(t, sessionA[:], cursor.LastSessionID)

	err = store.UpsertRecipientCursor(ctx, scriptA, 11, &sessionB)
	require.NoError(t, err)

	cursor, err = store.GetRecipientCursor(ctx, scriptA)
	require.NoError(t, err)
	require.Equal(t, int64(11), cursor.LastEventID)
	require.Equal(t, sessionB[:], cursor.LastSessionID)

	err = store.UpsertRecipientCursor(ctx, scriptB, 7, nil)
	require.NoError(t, err)

	rows, err := store.ListRecipientCursors(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	var (
		foundA bool
		foundB bool
	)

	for i := range rows {
		row := rows[i]
		switch {
		case string(row.RecipientPkScript) == string(scriptA):
			foundA = true
			require.Equal(t, int64(11), row.LastEventID)
			require.Equal(t, sessionB[:], row.LastSessionID)

		case string(row.RecipientPkScript) == string(scriptB):
			foundB = true
			require.Equal(t, int64(7), row.LastEventID)
			require.Nil(t, row.LastSessionID)
		}
	}

	require.True(t, foundA)
	require.True(t, foundB)
}

// TestOORArtifactStoreOwnedReceiveScriptCRUD verifies owned receive-script
// insert, update, lookup, and list ordering semantics.
func TestOORArtifactStoreOwnedReceiveScriptCRUD(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _ := newOORArtifactStoreForTest(t)

	clientPrivKeyA, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorPrivKeyA, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recA := OwnedReceiveScriptRecord{
		PkScript: []byte{
			0x51,
			0x30,
			0x01,
		},
		ClientKey: keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: 9,
				Index:  4,
			},
			PubKey: clientPrivKeyA.PubKey(),
		},
		OperatorPubKey: operatorPrivKeyA.PubKey(),
		ExitDelay:      12,
		Source:         OwnedReceiveScriptSourceRPC,
		CreatedAt:      time.Unix(100, 0).UTC(),
	}

	err = store.UpsertOwnedReceiveScript(ctx, recA)
	require.NoError(t, err)

	gotA, err := store.LookupOwnedReceiveScript(ctx, recA.PkScript)
	require.NoError(t, err)
	require.Equal(t, recA.PkScript, gotA.PkScript)
	require.Equal(t, recA.ClientKey.Family, gotA.ClientKey.Family)
	require.Equal(t, recA.ClientKey.Index, gotA.ClientKey.Index)
	require.True(t, recA.ClientKey.PubKey.IsEqual(gotA.ClientKey.PubKey))
	require.True(t, recA.OperatorPubKey.IsEqual(gotA.OperatorPubKey))
	require.Equal(t, recA.ExitDelay, gotA.ExitDelay)
	require.Equal(t, recA.Source, gotA.Source)
	require.Equal(t, recA.CreatedAt, gotA.CreatedAt)
	require.True(t, gotA.LastUsedAt.IsNone())

	clientPrivKeyA2, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorPrivKeyA2, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recAUpdate := OwnedReceiveScriptRecord{
		PkScript: recA.PkScript,
		ClientKey: keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: 11,
				Index:  7,
			},
			PubKey: clientPrivKeyA2.PubKey(),
		},
		OperatorPubKey: operatorPrivKeyA2.PubKey(),
		ExitDelay:      21,
		Source:         OwnedReceiveScriptSourceSync,
		CreatedAt:      time.Unix(999, 0).UTC(),
		LastUsedAt:     fn.Some(time.Unix(1234, 0).UTC()),
	}

	err = store.UpsertOwnedReceiveScript(ctx, recAUpdate)
	require.NoError(t, err)

	gotA, err = store.LookupOwnedReceiveScript(ctx, recA.PkScript)
	require.NoError(t, err)
	require.Equal(t, recAUpdate.ClientKey.Family, gotA.ClientKey.Family)
	require.Equal(t, recAUpdate.ClientKey.Index, gotA.ClientKey.Index)
	require.True(
		t, recAUpdate.ClientKey.PubKey.IsEqual(gotA.ClientKey.PubKey),
	)
	require.True(t, recAUpdate.OperatorPubKey.IsEqual(gotA.OperatorPubKey))
	require.Equal(t, recAUpdate.ExitDelay, gotA.ExitDelay)
	require.Equal(t, recAUpdate.Source, gotA.Source)
	require.Equal(t, recA.CreatedAt, gotA.CreatedAt)
	require.Equal(t, recAUpdate.LastUsedAt, gotA.LastUsedAt)

	clientPrivKeyB, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorPrivKeyB, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recB := OwnedReceiveScriptRecord{
		PkScript: []byte{
			0x51,
			0x30,
			0x02,
		},
		ClientKey: keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: 15,
				Index:  2,
			},
			PubKey: clientPrivKeyB.PubKey(),
		},
		OperatorPubKey: operatorPrivKeyB.PubKey(),
		ExitDelay:      30,
		Source:         OwnedReceiveScriptSourceWallet,
		CreatedAt:      time.Unix(200, 0).UTC(),
	}

	err = store.UpsertOwnedReceiveScript(ctx, recB)
	require.NoError(t, err)

	rows, err := store.ListOwnedReceiveScripts(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	require.Equal(t, recB.PkScript, rows[0].PkScript)
	require.Equal(t, recA.PkScript, rows[1].PkScript)
}

// testIdempotentOwnedReceiveScript builds deterministic ownership metadata for
// one retry-safe receive-script admission.
func testIdempotentOwnedReceiveScript(t *testing.T, seed byte, idempotencyKey,
	label string) OwnedReceiveScriptRecord {

	t.Helper()

	clientKey, _ := btcec.PrivKeyFromBytes([]byte{seed, 0x01})
	operatorKey, _ := btcec.PrivKeyFromBytes([]byte{seed, 0x02})
	createdAt := time.Unix(1_700_000_000+int64(seed), 0).UTC()
	lastUsedAt := time.Unix(1_700_001_000+int64(seed), 0).UTC()

	return OwnedReceiveScriptRecord{
		PkScript: []byte{
			0x51, 0x20, seed,
		},
		ClientKey: keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamily(seed),
				Index:  uint32(seed) + 10,
			},
			PubKey: clientKey.PubKey(),
		},
		OperatorPubKey:    operatorKey.PubKey(),
		ExitDelay:         int64(seed) + 20,
		Source:            OwnedReceiveScriptSourceRPC,
		CreatedAt:         createdAt,
		LastUsedAt:        fn.Some(lastUsedAt),
		IdempotencyKey:    idempotencyKey,
		RegistrationLabel: label,
		RegistrationExpiresAt: time.Now().UTC().Add(
			4 * time.Hour,
		).Truncate(time.Second),
		RegistrationRPCKey: "register-" + idempotencyKey,
	}
}

// requireOwnedReceiveScriptEqual compares every persisted receive-script
// ownership and retry field without relying on public-key pointer identity.
func requireOwnedReceiveScriptEqual(t *testing.T, want,
	got OwnedReceiveScriptRecord) {

	t.Helper()

	require.Equal(t, want.PkScript, got.PkScript)
	require.Equal(t, want.ClientKey.Family, got.ClientKey.Family)
	require.Equal(t, want.ClientKey.Index, got.ClientKey.Index)
	require.True(t, want.ClientKey.PubKey.IsEqual(got.ClientKey.PubKey))
	require.True(t, want.OperatorPubKey.IsEqual(got.OperatorPubKey))
	require.Equal(t, want.ExitDelay, got.ExitDelay)
	require.Equal(t, want.Source, got.Source)
	require.Equal(t, want.CreatedAt, got.CreatedAt)
	require.Equal(t, want.LastUsedAt, got.LastUsedAt)
	require.Equal(t, want.IdempotencyKey, got.IdempotencyKey)
	require.Equal(t, want.RegistrationLabel, got.RegistrationLabel)
	require.Equal(
		t, want.RegistrationExpiresAt, got.RegistrationExpiresAt,
	)
	require.Equal(t, want.RegistrationRPCKey, got.RegistrationRPCKey)
	require.Equal(
		t, want.RegistrationCompletedAt, got.RegistrationCompletedAt,
	)
}

// TestOORArtifactStoreUpsertPreservesIdempotentReplayMetadata verifies a
// wallet or recovery upsert can refresh script ownership without clearing the
// retry identity and remote-registration evidence owned by admission.
func TestOORArtifactStoreUpsertPreservesIdempotentReplayMetadata(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _ := newOORArtifactStoreForTest(t)
	winner := testIdempotentOwnedReceiveScript(
		t, 0x39, "receive-recovery-upsert", "recovery-label",
	)

	_, created, err := store.AdmitIdempotentOwnedReceiveScript(ctx, winner)
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(
		t, store.MarkOwnedReceiveScriptRegistered(
			ctx, winner.IdempotencyKey, winner.RegistrationRPCKey,
		),
	)

	before, err := store.LookupOwnedReceiveScriptByIdempotencyKey(
		ctx, winner.IdempotencyKey,
	)
	require.NoError(t, err)
	require.True(t, before.RegistrationCompletedAt.IsSome())

	recovery := winner
	recovery.Source = OwnedReceiveScriptSourceSync
	recovery.IdempotencyKey = ""
	recovery.RegistrationLabel = ""
	recovery.RegistrationExpiresAt = time.Time{}
	recovery.RegistrationRPCKey = ""
	recovery.RegistrationCompletedAt = fn.None[time.Time]()
	require.NoError(t, store.UpsertOwnedReceiveScript(ctx, recovery))

	after, err := store.LookupOwnedReceiveScriptByIdempotencyKey(
		ctx, winner.IdempotencyKey,
	)
	require.NoError(t, err)
	require.Equal(t, OwnedReceiveScriptSourceSync, after.Source)
	require.Equal(t, before.IdempotencyKey, after.IdempotencyKey)
	require.Equal(t, before.RegistrationLabel, after.RegistrationLabel)
	require.Equal(
		t, before.RegistrationExpiresAt, after.RegistrationExpiresAt,
	)
	require.Equal(
		t, before.RegistrationRPCKey, after.RegistrationRPCKey,
	)
	require.Equal(
		t, before.RegistrationCompletedAt,
		after.RegistrationCompletedAt,
	)
}

// TestOORArtifactStoreAdmitIdempotentOwnedReceiveScriptExactReplay verifies a
// repeated admission returns the first durable allocation and round-trips all
// ownership and registration metadata.
func TestOORArtifactStoreAdmitIdempotentOwnedReceiveScriptExactReplay(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()
	store, _ := newOORArtifactStoreForTest(t)
	winner := testIdempotentOwnedReceiveScript(
		t, 0x41, "receive-replay", "credit-topup",
	)

	admitted, created, err := store.AdmitIdempotentOwnedReceiveScript(
		ctx, winner,
	)
	require.NoError(t, err)
	require.True(t, created)
	requireOwnedReceiveScriptEqual(t, winner, *admitted)

	replay := testIdempotentOwnedReceiveScript(
		t, 0x42, winner.IdempotencyKey, winner.RegistrationLabel,
	)
	replayed, created, err := store.AdmitIdempotentOwnedReceiveScript(
		ctx, replay,
	)
	require.NoError(t, err)
	require.False(t, created)
	requireOwnedReceiveScriptEqual(t, winner, *replayed)

	lookedUp, err := store.LookupOwnedReceiveScriptByIdempotencyKey(
		ctx, winner.IdempotencyKey,
	)
	require.NoError(t, err)
	requireOwnedReceiveScriptEqual(t, winner, *lookedUp)
}

// TestOORArtifactStoreAdmitIdempotentOwnedReceiveScriptChangedLabel verifies
// reusing an idempotency key for a different caller-controlled label is
// rejected without rewriting the durable winner.
func TestOORArtifactStoreAdmitIdempotentOwnedReceiveScriptChangedLabel(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()
	store, _ := newOORArtifactStoreForTest(t)
	winner := testIdempotentOwnedReceiveScript(
		t, 0x51, "receive-label", "first-label",
	)

	_, created, err := store.AdmitIdempotentOwnedReceiveScript(ctx, winner)
	require.NoError(t, err)
	require.True(t, created)

	mismatch := testIdempotentOwnedReceiveScript(
		t, 0x52, winner.IdempotencyKey, "second-label",
	)
	_, created, err = store.AdmitIdempotentOwnedReceiveScript(
		ctx, mismatch,
	)
	require.ErrorIs(t, err, ErrOwnedReceiveScriptReplayMismatch)
	require.False(t, created)

	lookedUp, err := store.LookupOwnedReceiveScriptByIdempotencyKey(
		ctx, winner.IdempotencyKey,
	)
	require.NoError(t, err)
	requireOwnedReceiveScriptEqual(t, winner, *lookedUp)
}

// TestOORArtifactStoreIdempotentAdmissionSurfacesScriptCollision verifies the
// idempotency conflict clause does not hide a distinct pk_script owner.
func TestOORArtifactStoreIdempotentAdmissionSurfacesScriptCollision(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()
	store, _ := newOORArtifactStoreForTest(t)
	candidate := testIdempotentOwnedReceiveScript(
		t, 0x59, "receive-script-collision", "collision-label",
	)
	require.NoError(t, store.UpsertOwnedReceiveScript(ctx, candidate))

	_, created, err := store.AdmitIdempotentOwnedReceiveScript(
		ctx, candidate,
	)
	require.Error(t, err)
	require.False(t, created)
	require.NotErrorIs(t, err, errOwnedReceiveScriptAdmissionRace)

	_, err = store.LookupOwnedReceiveScriptByIdempotencyKey(
		ctx, candidate.IdempotencyKey,
	)
	require.ErrorIs(t, err, sql.ErrNoRows)
}

// TestOORArtifactStoreAdmitIdempotentOwnedReceiveScriptConcurrentWinner
// verifies concurrent first admissions converge on one durable allocation.
func TestOORArtifactStoreAdmitIdempotentOwnedReceiveScriptConcurrentWinner(
	t *testing.T) {

	t.Parallel()

	type admissionResult struct {
		record  *OwnedReceiveScriptRecord
		created bool
		err     error
	}

	ctx := t.Context()
	store, _ := newOORArtifactStoreForTest(t)
	candidates := []OwnedReceiveScriptRecord{
		testIdempotentOwnedReceiveScript(
			t, 0x61, "receive-concurrent", "shared-label",
		),
		testIdempotentOwnedReceiveScript(
			t, 0x62, "receive-concurrent", "shared-label",
		),
	}

	start := make(chan struct{})
	results := make(chan admissionResult, len(candidates))
	var workers sync.WaitGroup
	for i := range candidates {
		candidate := candidates[i]
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start

			record, created, err :=
				store.AdmitIdempotentOwnedReceiveScript(
					ctx, candidate,
				)
			results <- admissionResult{
				record:  record,
				created: created,
				err:     err,
			}
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	var admissions []admissionResult
	for result := range results {
		require.NoError(t, result.err)
		require.NotNil(t, result.record)
		admissions = append(admissions, result)
	}
	require.Len(t, admissions, 2)
	require.NotEqual(t, admissions[0].created, admissions[1].created)
	requireOwnedReceiveScriptEqual(
		t, *admissions[0].record, *admissions[1].record,
	)

	winner := *admissions[0].record
	require.True(
		t, string(winner.PkScript) == string(candidates[0].PkScript) ||
			string(winner.PkScript) == string(
				candidates[1].PkScript,
			),
	)
}

// TestOORArtifactStoreIdempotentOwnedReceiveScriptPendingAcrossRestart
// verifies an admitted but unregistered allocation survives a database-handle
// restart and remains the exact replay winner.
func TestOORArtifactStoreIdempotentOwnedReceiveScriptPendingAcrossRestart(
	t *testing.T) {

	dbPath := filepath.Join(t.TempDir(), "receive-script.db")
	store, baseDB := newOORArtifactStoreForTestPath(t, dbPath)
	winner := testIdempotentOwnedReceiveScript(
		t, 0x71, "receive-restart", "restart-label",
	)

	_, created, err := store.AdmitIdempotentOwnedReceiveScript(
		t.Context(), winner,
	)
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, baseDB.DB.Close())

	restarted, _ := newOORArtifactStoreForTestPath(t, dbPath)
	pending, err := restarted.LookupOwnedReceiveScriptByIdempotencyKey(
		t.Context(), winner.IdempotencyKey,
	)
	require.NoError(t, err)
	require.True(t, pending.RegistrationCompletedAt.IsNone())
	requireOwnedReceiveScriptEqual(t, winner, *pending)

	replay := testIdempotentOwnedReceiveScript(
		t, 0x72, winner.IdempotencyKey, winner.RegistrationLabel,
	)
	replayed, created, err :=
		restarted.AdmitIdempotentOwnedReceiveScript(t.Context(), replay)
	require.NoError(t, err)
	require.False(t, created)
	requireOwnedReceiveScriptEqual(t, winner, *replayed)
}

// TestOORArtifactStoreRenewExpiredOwnedReceiveScriptRegistration verifies an
// expired completed allocation keeps its ownership identity while one new
// durable registration window wins across stale concurrent retries.
func TestOORArtifactStoreRenewExpiredOwnedReceiveScriptRegistration(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)
	clk := clock.NewTestClock(now)
	testDB := NewTestDB(t)
	artifactDB := NewTransactionExecutor(
		testDB.BaseDB,
		func(tx *sql.Tx) OORArtifactStore {
			return testDB.WithTx(tx)
		},
		btclog.Disabled,
	)
	store := NewOORArtifactPersistenceStore(artifactDB, clk)
	winner := testIdempotentOwnedReceiveScript(
		t, 0x79, "receive-renew", "renew-label",
	)
	winner.RegistrationExpiresAt = now.Add(time.Hour)

	_, created, err := store.AdmitIdempotentOwnedReceiveScript(ctx, winner)
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(
		t, store.MarkOwnedReceiveScriptRegistered(
			ctx, winner.IdempotencyKey, winner.RegistrationRPCKey,
		),
	)

	clk.SetTime(now.Add(2 * time.Hour))
	nextExpiry := clk.Now().Add(4 * time.Hour)
	renewed, err :=
		store.RenewOwnedReceiveScriptRegistration(
			ctx, winner, nextExpiry, "renewed-registration-key",
		)
	require.NoError(t, err)
	require.Equal(t, winner.PkScript, renewed.PkScript)
	require.Equal(t, winner.ClientKey, renewed.ClientKey)
	require.True(t, winner.OperatorPubKey.IsEqual(renewed.OperatorPubKey))
	require.Equal(t, winner.ExitDelay, renewed.ExitDelay)
	require.Equal(t, winner.RegistrationLabel, renewed.RegistrationLabel)
	require.Equal(t, nextExpiry, renewed.RegistrationExpiresAt)
	require.Equal(t, "renewed-registration-key", renewed.RegistrationRPCKey)
	require.True(t, renewed.RegistrationCompletedAt.IsNone())

	staleReplay, err :=
		store.RenewOwnedReceiveScriptRegistration(
			ctx, winner, clk.Now().Add(5*time.Hour),
			"losing-registration-key",
		)
	require.NoError(t, err)
	require.Equal(
		t, renewed.RegistrationExpiresAt,
		staleReplay.RegistrationExpiresAt,
	)
	require.Equal(
		t, renewed.RegistrationRPCKey, staleReplay.RegistrationRPCKey,
	)
}

// TestOORArtifactStoreOwnedReceiveScriptCompletionIdempotency verifies the
// registration completion transition is retry-safe for the exact remote key
// and rejects a different remote request identity.
func TestOORArtifactStoreOwnedReceiveScriptCompletionIdempotency(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _ := newOORArtifactStoreForTest(t)
	winner := testIdempotentOwnedReceiveScript(
		t, 0x81, "receive-complete", "completion-label",
	)

	_, created, err := store.AdmitIdempotentOwnedReceiveScript(ctx, winner)
	require.NoError(t, err)
	require.True(t, created)

	err = store.MarkOwnedReceiveScriptRegistered(
		ctx, winner.IdempotencyKey, winner.RegistrationRPCKey,
	)
	require.NoError(t, err)
	completed, err := store.LookupOwnedReceiveScriptByIdempotencyKey(
		ctx, winner.IdempotencyKey,
	)
	require.NoError(t, err)
	require.True(t, completed.RegistrationCompletedAt.IsSome())
	completedAt := completed.RegistrationCompletedAt

	err = store.MarkOwnedReceiveScriptRegistered(
		ctx, winner.IdempotencyKey, winner.RegistrationRPCKey,
	)
	require.NoError(t, err)
	completed, err = store.LookupOwnedReceiveScriptByIdempotencyKey(
		ctx, winner.IdempotencyKey,
	)
	require.NoError(t, err)
	require.Equal(t, completedAt, completed.RegistrationCompletedAt)

	err = store.MarkOwnedReceiveScriptRegistered(
		ctx, winner.IdempotencyKey, "different-registration-key",
	)
	require.ErrorContains(
		t, err, "registration completion invariant failed",
	)
}

// buildTestOORPackage constructs a deterministic incoming-style OOR package
// fixture and returns the primary outpoints used by storage tests.
func buildTestOORPackage(t *testing.T, seed byte) (chainhash.Hash, *psbt.Packet,
	[]*psbt.Packet, wire.OutPoint, []byte, int64, wire.OutPoint) {

	inputOutpoint := wire.OutPoint{
		Hash: chainhash.Hash{
			seed,
			0xaa,
		},
		Index: 0,
	}

	return buildTestOORPackageWithInput(t, seed, inputOutpoint)
}

// buildTestOORPackageWithInput builds a fixture package that spends the
// provided input outpoint in its checkpoint transaction.
func buildTestOORPackageWithInput(t *testing.T, seed byte,
	inputOutpoint wire.OutPoint) (chainhash.Hash, *psbt.Packet,
	[]*psbt.Packet, wire.OutPoint, []byte, int64, wire.OutPoint) {

	t.Helper()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recipientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	spentPkScript, err := txscript.PayToTaprootScript(operatorKey.PubKey())
	require.NoError(t, err)

	inputValue := btcutil.Amount(10_000)
	checkpointInput := oortx.CheckpointInput{
		SpentVTXO: oortx.SpentVTXORef{
			Outpoint: inputOutpoint,
			Output: &wire.TxOut{
				Value:    int64(inputValue),
				PkScript: spentPkScript,
			},
		},
		OwnerLeafScript: []byte{
			0x51,
		},
	}

	checkpointRes, err := oortx.BuildCheckpointPSBT(policy, checkpointInput)
	require.NoError(t, err)

	recipientTapKey, err := arkscript.VTXOTapKey(
		recipientKey.PubKey(), operatorKey.PubKey(), 10,
	)
	require.NoError(t, err)

	recipientPkScript, err := txscript.PayToTaprootScript(recipientTapKey)
	require.NoError(t, err)

	checkpointTxID := checkpointRes.PSBT.UnsignedTx.TxHash()
	checkpointOutput := checkpointRes.PSBT.UnsignedTx.TxOut[0]

	arkPSBT, err := oortx.BuildArkPSBT(
		[]oortx.CheckpointOutput{
			{
				Txid:           checkpointTxID,
				Output:         checkpointOutput,
				TapTreeEncoded: checkpointRes.TapTreeEncoded,
			},
		},
		[]oortx.RecipientOutput{
			{
				PkScript: recipientPkScript,
				Value:    inputValue,
			},
		},
	)
	require.NoError(t, err)

	sessionID := arkPSBT.UnsignedTx.TxHash()
	recipientOutpoint := wire.OutPoint{
		Hash:  sessionID,
		Index: 0,
	}

	return sessionID, arkPSBT, []*psbt.Packet{checkpointRes.PSBT},
		recipientOutpoint, recipientPkScript, int64(inputValue),
		inputOutpoint
}
