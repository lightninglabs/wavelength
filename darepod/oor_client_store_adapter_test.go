//nolint:ll
package darepod

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	clientdb "github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
	"github.com/lightninglabs/darepo-client/oor"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

func TestOORClientSQLSessionStorePersistsIncomingAncestors(t *testing.T) {
	t.Parallel()

	sqlDB := clientdb.NewTestDB(t)
	dbStore := clientdb.NewStore(
		sqlDB.DB, sqlDB.Queries, sqlDB.Backend(), btclog.Disabled,
	)
	clientStore := clientdb.NewOORClientStore(
		dbStore, clock.NewDefaultClock(),
	)
	packageStore := dbStore.NewOORArtifactStore(clock.NewDefaultClock())

	sessionStore := &oorClientSQLSessionStore{
		store:        clientStore,
		packageStore: packageStore,
	}

	currentID := testOORSessionID(1)
	ancestorID := testOORSessionID(2)
	currentArk := testSerializedPSBT(t, 3)
	currentCheckpoint := testSerializedPSBT(t, 4)
	ancestorArk := testPSBT(t, 5)
	ancestorCheckpoint := testPSBT(t, 6)

	err := sessionStore.SaveIncomingSession(
		t.Context(), &oor.IncomingSnapshot{
			Version:         1,
			SessionID:       currentID,
			Phase:           oor.IncomingPhaseMaterializePending,
			ArkPSBT:         currentArk,
			CheckpointPSBTs: [][]byte{currentCheckpoint},
			AncestorPackages: []oor.PackageArtifact{
				{
					SessionID:            ancestorID,
					ArkPSBT:              ancestorArk,
					FinalCheckpointPSBTs: []*psbt.Packet{ancestorCheckpoint},
				},
			},
		},
	)
	require.NoError(t, err)

	bundle, err := packageStore.GetPackage(
		t.Context(), chainhash.Hash(ancestorID),
	)
	require.NoError(t, err)
	require.Len(t, bundle.FinalCheckpointPSBTs, 1)
	require.Equal(t, chainhash.Hash(ancestorID), bundle.SessionID)
}

func testOORSessionID(seed byte) oor.SessionID {
	var h chainhash.Hash
	h[0] = seed

	return oor.SessionID(h)
}

func testSerializedPSBT(t *testing.T, seed byte) []byte {
	t.Helper()

	raw, err := psbtutil.Serialize(testPSBT(t, seed))
	require.NoError(t, err)

	return raw
}

func testPSBT(t *testing.T, seed byte) *psbt.Packet {
	t.Helper()

	var prev chainhash.Hash
	prev[0] = seed

	tx := wire.NewMsgTx(3)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  prev,
			Index: 0,
		},
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    1_000,
		PkScript: []byte{0x51, seed},
	})

	pkt, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)

	return pkt
}
