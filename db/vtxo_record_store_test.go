package db

import (
	"database/sql"
	"math"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo/db/sqlc"
	"github.com/lightninglabs/darepo/vtxo"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

func setupVTXORecordStore(t *testing.T) (*VTXORecordStoreDB, wire.OutPoint) {
	t.Helper()

	sqlStore := NewTestDB(t)
	store := NewStore(
		sqlStore.DB, sqlStore.Queries, sqlStore.Backend(),
		btclog.Disabled, clock.NewDefaultClock(),
	)
	recordStore := store.NewVTXORecordStore()

	outpoint := wire.OutPoint{
		Hash:  testOutpointHash(t, "vtxo-record-store"),
		Index: 0,
	}

	err := recordStore.Create(t.Context(), &vtxo.Record{
		Outpoint: outpoint,
		Value:    1000,
		PkScript: append([]byte{0x51, 0x20}, make([]byte, 32)...),
		Status:   vtxo.StatusLive,
	})
	require.NoError(t, err)

	return recordStore, outpoint
}

// TestVTXORecordStoreRejectsDuplicateOutpoints ensures duplicate outpoints are
// rejected explicitly for lifecycle transitions.
func TestVTXORecordStoreRejectsDuplicateOutpoints(t *testing.T) {
	t.Parallel()

	recordStore, outpoint := setupVTXORecordStore(t)
	ctx := t.Context()

	err := recordStore.MarkInFlight(
		ctx, []wire.OutPoint{outpoint, outpoint},
		vtxo.OORLockOwner("session-1"),
	)
	require.ErrorContains(t, err, "duplicate outpoint")

	rec, err := recordStore.Get(ctx, outpoint)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, vtxo.StatusLive, rec.Status)

	err = recordStore.MarkSpent(ctx, []wire.OutPoint{outpoint, outpoint})
	require.ErrorContains(t, err, "duplicate outpoint")

	rec, err = recordStore.Get(ctx, outpoint)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, vtxo.StatusLive, rec.Status)
}

// TestVTXORecordStoreCreateWithDescriptorMetadata ensures OOR-created VTXOs
// can persist the real collaborative descriptor fields needed by later round
// validation and operator signing.
func TestVTXORecordStoreCreateWithDescriptorMetadata(t *testing.T) {
	t.Parallel()

	sqlStore := NewTestDB(t)
	store := NewStore(
		sqlStore.DB, sqlStore.Queries, sqlStore.Backend(),
		btclog.Disabled, clock.NewDefaultClock(),
	)

	recordStore := store.NewVTXORecordStore()
	vtxoStore := store.NewVTXOStore()

	ownerKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	const exitDelay = uint32(144)

	tapKey, err := scripts.VTXOTapKey(
		ownerKey.PubKey(), operatorKey.PubKey(), exitDelay,
	)
	require.NoError(t, err)

	pkScript, err := txscript.PayToTaprootScript(tapKey)
	require.NoError(t, err)

	outpoint := wire.OutPoint{
		Hash:  testOutpointHash(t, "vtxo-record-metadata"),
		Index: 1,
	}

	err = recordStore.Create(t.Context(), &vtxo.Record{
		Outpoint: outpoint,
		Value:    42_000,
		PkScript: pkScript,
		Status:   vtxo.StatusLive,
		OwnerKey: ownerKey.PubKey(),
		OperatorKeyDesc: &keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: 9,
				Index:  21,
			},
			PubKey: operatorKey.PubKey(),
		},
		ExitDelay: exitDelay,
	})
	require.NoError(t, err)

	persisted, err := vtxoStore.GetVTXO(t.Context(), outpoint)
	require.NoError(t, err)
	require.NotNil(t, persisted)
	require.NotNil(t, persisted.Descriptor)
	require.NotNil(t, persisted.OperatorKeyDesc)
	require.True(
		t,
		persisted.Descriptor.OwnerKey.IsEqual(ownerKey.PubKey()),
	)
	require.True(
		t,
		persisted.Descriptor.OperatorKey.IsEqual(
			operatorKey.PubKey(),
		),
	)
	require.Equal(t, exitDelay, persisted.Descriptor.ExitDelay)
	require.Equal(
		t, keychain.KeyFamily(9),
		persisted.OperatorKeyDesc.Family,
	)
	require.Equal(t, uint32(21), persisted.OperatorKeyDesc.Index)

	record, err := recordStore.Get(t.Context(), outpoint)
	require.NoError(t, err)
	require.NotNil(t, record)
	require.NotNil(t, record.OwnerKey)
	require.NotNil(t, record.OperatorKeyDesc)
	require.True(t, record.OwnerKey.IsEqual(ownerKey.PubKey()))
	require.True(
		t, record.OperatorKeyDesc.PubKey.IsEqual(
			operatorKey.PubKey(),
		),
	)
	require.Equal(t, exitDelay, record.ExitDelay)
	require.Equal(
		t, keychain.KeyFamily(9), record.OperatorKeyDesc.Family,
	)
	require.Equal(t, uint32(21), record.OperatorKeyDesc.Index)
}

// TestVTXORecordStoreCreateRejectsOversizedExitDelay ensures we fail before
// narrowing a malformed descriptor exit delay into the signed DB column.
func TestVTXORecordStoreCreateRejectsOversizedExitDelay(t *testing.T) {
	t.Parallel()

	sqlStore := NewTestDB(t)
	store := NewStore(
		sqlStore.DB, sqlStore.Queries, sqlStore.Backend(),
		btclog.Disabled, clock.NewDefaultClock(),
	)

	recordStore := store.NewVTXORecordStore()

	ownerKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	outpoint := wire.OutPoint{
		Hash:  testOutpointHash(t, "vtxo-record-oversized-exit-delay"),
		Index: 3,
	}

	err = recordStore.Create(t.Context(), &vtxo.Record{
		Outpoint: outpoint,
		Value:    42_000,
		PkScript: append([]byte{0x51, 0x20}, make([]byte, 32)...),
		Status:   vtxo.StatusLive,
		OwnerKey: ownerKey.PubKey(),
		OperatorKeyDesc: &keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: 9,
				Index:  21,
			},
			PubKey: operatorKey.PubKey(),
		},
		ExitDelay: math.MaxInt32 + 1,
	})
	require.ErrorContains(t, err, "exit delay out of range")
}

// TestVTXORecordStoreCreateEnrichesReceiveScriptMetadata ensures a bare live
// record is upgraded to a real Ark VTXO when the receive script was
// registered with standardized descriptor metadata.
func TestVTXORecordStoreCreateEnrichesReceiveScriptMetadata(t *testing.T) {
	t.Parallel()

	sqlStore := NewTestDB(t)
	store := NewStore(
		sqlStore.DB, sqlStore.Queries, sqlStore.Backend(),
		btclog.Disabled, clock.NewDefaultClock(),
	)

	recordStore := store.NewVTXORecordStore()
	vtxoStore := store.NewVTXOStore()

	ownerKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorKeyDesc := keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: 9,
			Index:  21,
		},
		PubKey: operatorKey.PubKey(),
	}
	recordStore.SetOperatorKey(operatorKeyDesc)

	const exitDelay = uint32(144)

	tapKey, err := scripts.VTXOTapKey(
		ownerKey.PubKey(), operatorKey.PubKey(), exitDelay,
	)
	require.NoError(t, err)

	pkScript, err := txscript.PayToTaprootScript(tapKey)
	require.NoError(t, err)

	err = store.Queries.UpsertIndexerReceiveScript(
		t.Context(), sqlc.UpsertIndexerReceiveScriptParams{
			PrincipalMailboxID: "client-bob",
			PkScript:           pkScript,
			ExpiresAtUnixS:     time.Now().Add(time.Hour).Unix(),
			Label:              "test",
			UpdatedAt:          time.Now().Unix(),
			OwnerPubkey: ownerKey.PubKey().
				SerializeCompressed(),
			OperatorPubkey: operatorKey.PubKey().
				SerializeCompressed(),
			ExitDelay: sql.NullInt64{
				Int64: int64(exitDelay),
				Valid: true,
			},
		},
	)
	require.NoError(t, err)

	outpoint := wire.OutPoint{
		Hash:  testOutpointHash(t, "vtxo-record-receive-script"),
		Index: 2,
	}

	err = recordStore.Create(t.Context(), &vtxo.Record{
		Outpoint: outpoint,
		Value:    42_000,
		PkScript: pkScript,
		Status:   vtxo.StatusLive,
	})
	require.NoError(t, err)

	persisted, err := vtxoStore.GetVTXO(t.Context(), outpoint)
	require.NoError(t, err)
	require.NotNil(t, persisted)
	require.NotNil(t, persisted.Descriptor)
	require.NotNil(t, persisted.OperatorKeyDesc)
	require.True(
		t,
		persisted.Descriptor.OwnerKey.IsEqual(ownerKey.PubKey()),
	)
	require.True(
		t,
		persisted.Descriptor.OperatorKey.IsEqual(operatorKey.PubKey()),
	)
	require.Equal(t, exitDelay, persisted.Descriptor.ExitDelay)
	require.Equal(
		t, operatorKeyDesc.KeyLocator,
		persisted.OperatorKeyDesc.KeyLocator,
	)
}
