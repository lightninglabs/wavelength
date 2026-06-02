package db

import (
	"bytes"
	"database/sql"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/virtualchannel"
	"github.com/stretchr/testify/require"
)

// TestVirtualChannelStoreRoundTrip verifies that the store can reload a
// virtual channel by lnd channel point with all backing material intact.
func TestVirtualChannelStoreRoundTrip(t *testing.T) {
	t.Parallel()

	sqlDB := NewTestDB(t)
	store := NewStore(
		sqlDB.DB, sqlDB.Queries, sqlDB.Backend(), sqlDB.log,
	)
	ctx := t.Context()

	backingVTXOs := []virtualchannel.BackingVTXO{
		{
			OutPoint: wire.OutPoint{
				Hash:  testHash("virtual-channel-a"),
				Index: 1,
			},
			Amount: btcutil.Amount(70000),
		},
		{
			OutPoint: wire.OutPoint{
				Hash:  testHash("virtual-channel-b"),
				Index: 2,
			},
			Amount: btcutil.Amount(30000),
		},
	}

	insertBackingVTXOs(t, sqlDB.Queries, backingVTXOs)

	backingTx := wire.NewMsgTx(2)
	for _, backing := range backingVTXOs {
		backingTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: backing.OutPoint,
		})
	}
	backingTx.AddTxOut(&wire.TxOut{
		Value:    99000,
		PkScript: append([]byte{0x51, 0x20}, make([]byte, 32)...),
	})
	channelPoint := wire.OutPoint{
		Hash:  backingTx.TxHash(),
		Index: 0,
	}

	reg := virtualchannel.Registration{
		ID:               fixedVirtualChannelID(1),
		PendingChannelID: fixedPendingChannelID(2),
		ChannelPoint:     channelPoint,
		RemoteNodePubKey: fixedNodePubKey(3),
		Role:             virtualchannel.RoleClient,
		Status:           virtualchannel.StatusActive,
		Capacity:         btcutil.Amount(99000),
		LocalBalance:     btcutil.Amount(80000),
		RemoteBalance:    btcutil.Amount(19000),
		BackingTx:        backingTx,
		FundingPsbt: []byte{
			0x70,
			0x73,
			0x62,
			0x74,
		},
		BackingVTXOs: backingVTXOs,
	}

	virtualStore := store.NewVirtualChannelStore()
	pending := virtualchannel.PendingOpen{
		PendingChannelID: reg.PendingChannelID,
		RemoteNodePubKey: reg.RemoteNodePubKey,
		Role:             reg.Role,
		Status:           virtualchannel.StatusNegotiating,
		Capacity:         reg.Capacity,
		LocalBalance:     reg.LocalBalance,
		RemoteBalance:    reg.RemoteBalance,
		BackingVTXOs:     backingVTXOs,
	}
	err := virtualStore.InsertVirtualChannelPendingOpen(ctx, pending)
	require.NoError(t, err)

	pendingLoaded, ok, err := virtualStore.FindVirtualChannelPendingOpen(
		ctx, reg.PendingChannelID,
	)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, pending, *pendingLoaded)

	err = virtualStore.InsertVirtualChannel(ctx, reg)
	require.NoError(t, err)

	loaded, err := virtualStore.GetVirtualChannelByChannelPoint(
		ctx, channelPoint,
	)
	require.NoError(t, err)
	require.Equal(t, reg.ID, loaded.ID)
	require.Equal(t, reg.PendingChannelID, loaded.PendingChannelID)
	require.Equal(t, reg.ChannelPoint, loaded.ChannelPoint)
	require.Equal(t, reg.RemoteNodePubKey, loaded.RemoteNodePubKey)
	require.Equal(t, reg.Role, loaded.Role)
	require.Equal(t, reg.Status, loaded.Status)
	require.Equal(t, reg.Capacity, loaded.Capacity)
	require.Equal(t, reg.LocalBalance, loaded.LocalBalance)
	require.Equal(t, reg.RemoteBalance, loaded.RemoteBalance)
	require.Equal(t, reg.FundingPsbt, loaded.FundingPsbt)
	require.Equal(t, backingVTXOs, loaded.BackingVTXOs)

	byPending, ok, err := virtualStore.FindVirtualChannelByPendingChannelID(
		ctx, reg.PendingChannelID,
	)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, reg.ID, byPending.ID)

	pendingLoaded, ok, err = virtualStore.FindVirtualChannelPendingOpen(
		ctx, reg.PendingChannelID,
	)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, reg.RemoteNodePubKey, pendingLoaded.RemoteNodePubKey)
	require.Equal(t, reg.BackingVTXOs, pendingLoaded.BackingVTXOs)

	var expectedTx bytes.Buffer
	require.NoError(t, backingTx.Serialize(&expectedTx))

	var gotTx bytes.Buffer
	require.NoError(t, loaded.BackingTx.Serialize(&gotTx))
	require.Equal(t, expectedTx.Bytes(), gotTx.Bytes())

	byTxID, err := virtualStore.ListVirtualChannelsByFundingTxID(
		ctx, channelPoint.Hash,
	)
	require.NoError(t, err)
	require.Len(t, byTxID, 1)
	require.Equal(t, reg.ID, byTxID[0].ID)

	changed, err := virtualStore.MarkVirtualChannelMaterializing(
		ctx, reg.ID,
	)
	require.NoError(t, err)
	require.True(t, changed)

	loaded, err = virtualStore.GetVirtualChannel(ctx, reg.ID)
	require.NoError(t, err)
	require.Equal(t, virtualchannel.StatusMaterializing, loaded.Status)
	require.False(t, loaded.MaterializedAt.IsZero())
}

// TestVirtualChannelStoreMarkActiveRequiresWitnesses verifies that active
// registrations only persist conflict-publishable backing parents.
func TestVirtualChannelStoreMarkActiveRequiresWitnesses(t *testing.T) {
	t.Parallel()

	sqlDB := NewTestDB(t)
	store := NewStore(
		sqlDB.DB, sqlDB.Queries, sqlDB.Backend(), sqlDB.log,
	)
	ctx := t.Context()

	backingVTXOs := []virtualchannel.BackingVTXO{
		{
			OutPoint: wire.OutPoint{
				Hash:  testHash("virtual-active-a"),
				Index: 1,
			},
			Amount: btcutil.Amount(70000),
		},
	}
	insertBackingVTXOs(t, sqlDB.Queries, backingVTXOs)

	backingTx := wire.NewMsgTx(2)
	backingTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: backingVTXOs[0].OutPoint,
	})
	backingTx.AddTxOut(&wire.TxOut{
		Value:    69000,
		PkScript: append([]byte{0x51, 0x20}, make([]byte, 32)...),
	})
	reg := virtualchannel.Registration{
		ID:               fixedVirtualChannelID(9),
		PendingChannelID: fixedPendingChannelID(10),
		ChannelPoint: wire.OutPoint{
			Hash:  backingTx.TxHash(),
			Index: 0,
		},
		RemoteNodePubKey: fixedNodePubKey(11),
		Role:             virtualchannel.RoleClient,
		Status:           virtualchannel.StatusNegotiating,
		Capacity:         btcutil.Amount(69000),
		LocalBalance:     btcutil.Amount(68000),
		RemoteBalance:    btcutil.Amount(1000),
		BackingTx:        backingTx,
		FundingPsbt: []byte{
			0x70,
			0x73,
			0x62,
			0x74,
		},
		BackingVTXOs: backingVTXOs,
	}

	virtualStore := store.NewVirtualChannelStore()
	require.NoError(t, virtualStore.InsertVirtualChannel(ctx, reg))

	changed, err := virtualStore.MarkVirtualChannelActive(
		ctx, reg.ID, backingTx,
	)
	require.ErrorContains(t, err, "has no witness")
	require.False(t, changed)

	signedTx := backingTx.Copy()
	signedTx.TxIn[0].Witness = wire.TxWitness{[]byte{0x01}}
	require.Equal(t, backingTx.TxHash(), signedTx.TxHash())

	changed, err = virtualStore.MarkVirtualChannelActive(
		ctx, reg.ID, signedTx,
	)
	require.NoError(t, err)
	require.True(t, changed)

	loaded, err := virtualStore.GetVirtualChannel(ctx, reg.ID)
	require.NoError(t, err)
	require.Equal(t, virtualchannel.StatusActive, loaded.Status)
	require.Equal(
		t, wire.TxWitness{[]byte{0x01}},
		loaded.BackingTx.TxIn[0].Witness,
	)
}

// TestVirtualChannelStoreMarkCoopClosed records the cooperative close artifact
// only when the close transaction spends the registered channel point.
func TestVirtualChannelStoreMarkCoopClosed(t *testing.T) {
	t.Parallel()

	sqlDB := NewTestDB(t)
	store := NewStore(
		sqlDB.DB, sqlDB.Queries, sqlDB.Backend(), sqlDB.log,
	)
	ctx := t.Context()

	backing := virtualchannel.BackingVTXO{
		OutPoint: wire.OutPoint{
			Hash:  testHash("virtual-close-a"),
			Index: 1,
		},
		Amount: btcutil.Amount(70000),
	}
	insertBackingVTXOs(t, sqlDB.Queries, []virtualchannel.BackingVTXO{
		backing,
	})

	backingTx := wire.NewMsgTx(2)
	backingTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: backing.OutPoint,
		Witness:          wire.TxWitness{[]byte{0x01}},
	})
	backingTx.AddTxOut(&wire.TxOut{
		Value:    69000,
		PkScript: append([]byte{0x51, 0x20}, make([]byte, 32)...),
	})
	channelPoint := wire.OutPoint{
		Hash:  backingTx.TxHash(),
		Index: 0,
	}
	reg := virtualchannel.Registration{
		ID:               fixedVirtualChannelID(20),
		PendingChannelID: fixedPendingChannelID(21),
		ChannelPoint:     channelPoint,
		RemoteNodePubKey: fixedNodePubKey(22),
		Role:             virtualchannel.RoleClient,
		Status:           virtualchannel.StatusActive,
		Capacity:         btcutil.Amount(69000),
		LocalBalance:     btcutil.Amount(68000),
		RemoteBalance:    btcutil.Amount(1000),
		BackingTx:        backingTx,
		FundingPsbt: []byte{
			0x70,
			0x73,
			0x62,
			0x74,
		},
		BackingVTXOs: []virtualchannel.BackingVTXO{
			backing,
		},
	}

	virtualStore := store.NewVirtualChannelStore()
	require.NoError(t, virtualStore.InsertVirtualChannel(ctx, reg))

	unrelatedClose := wire.NewMsgTx(2)
	unrelatedClose.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  testHash("unrelated-close"),
			Index: 0,
		},
	})
	changed, err := virtualStore.MarkVirtualChannelCoopClosed(
		ctx, reg.ID, unrelatedClose, btcutil.Amount(1000),
		btcutil.Amount(2000),
	)
	require.ErrorContains(t, err, "does not spend channel point")
	require.False(t, changed)

	closeTx := wire.NewMsgTx(2)
	closeTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: channelPoint,
	})
	closeTx.AddTxOut(&wire.TxOut{
		Value:    67000,
		PkScript: []byte{0x51},
	})

	changed, err = virtualStore.MarkVirtualChannelCoopClosed(
		ctx, reg.ID, closeTx, btcutil.Amount(67000), btcutil.Amount(0),
	)
	require.NoError(t, err)
	require.True(t, changed)

	loaded, err := virtualStore.GetVirtualChannel(ctx, reg.ID)
	require.NoError(t, err)
	require.Equal(t, virtualchannel.StatusClosed, loaded.Status)
	require.Equal(t, btcutil.Amount(67000), loaded.LocalBalance)
	require.Equal(t, btcutil.Amount(0), loaded.RemoteBalance)
	require.False(t, loaded.ClosedAt.IsZero())
	require.NotNil(t, loaded.CloseTx)

	var expected bytes.Buffer
	require.NoError(t, closeTx.Serialize(&expected))
	var got bytes.Buffer
	require.NoError(t, loaded.CloseTx.Serialize(&got))
	require.Equal(t, expected.Bytes(), got.Bytes())

	changed, err = virtualStore.MarkVirtualChannelCoopClosed(
		ctx, reg.ID, closeTx, btcutil.Amount(67000), btcutil.Amount(0),
	)
	require.NoError(t, err)
	require.True(t, changed)
}

// insertBackingVTXOs creates live VTXOs for FK-backed virtual-channel rows.
func insertBackingVTXOs(t *testing.T, q *sqlc.Queries,
	backingVTXOs []virtualchannel.BackingVTXO) {

	ctx := t.Context()
	now := time.Now().UTC().UnixNano()
	err := q.InsertRound(ctx, sqlc.InsertRoundParams{
		RoundID:               "virtual-channel-test-round",
		ConfirmationHeight:    sql.NullInt32{},
		ConfirmationBlockHash: nil,
		CommitmentTx:          nil,
		CommitmentTxid:        nil,
		VtxtTree:              nil,
		Status:                "confirmed",
		CreationTime:          now,
		LastUpdateTime:        now,
		StartHeight:           1,
	})
	require.NoError(t, err)

	for _, backing := range backingVTXOs {
		err := q.InsertVTXO(
			ctx, sqlc.InsertVTXOParams{
				OutpointHash:  backing.OutPoint.Hash[:],
				OutpointIndex: int32(backing.OutPoint.Index),
				RoundID:       "virtual-channel-test-round",
				Amount:        int64(backing.Amount),
				PkScript: append(
					[]byte{0x51, 0x20}, make([]byte, 32)...,
				),
				Expiry:          144,
				PolicyTemplate:  []byte{0x01},
				ClientKeyFamily: 1,
				ClientKeyIndex:  2,
				ClientPubkey:    fixedPubKey(3),
				OperatorPubkey:  fixedPubKey(4),
				BatchExpiry:     300,
				ChainDepth:      1,
				CreatedHeight:   100,
				CommitmentTxid:  testHashBytes("commitment"),
				Spent:           false,
				CreationTime:    now,
				LastUpdateTime:  now,
			},
		)
		require.NoError(t, err)
	}
}

// fixedVirtualChannelID returns a deterministic virtual channel id for tests.
func fixedVirtualChannelID(fill byte) virtualchannel.ID {
	var id virtualchannel.ID
	for i := range id {
		id[i] = fill
	}

	return id
}

// fixedPendingChannelID returns a deterministic pending channel id for tests.
func fixedPendingChannelID(fill byte) virtualchannel.PendingChannelID {
	var id virtualchannel.PendingChannelID
	for i := range id {
		id[i] = fill
	}

	return id
}

// fixedNodePubKey returns a deterministic compressed node pubkey for tests.
func fixedNodePubKey(fill byte) virtualchannel.NodePubKey {
	var key virtualchannel.NodePubKey
	key[0] = 0x02
	for i := 1; i < len(key); i++ {
		key[i] = fill
	}

	return key
}

// fixedPubKey returns a deterministic compressed public key.
func fixedPubKey(fill byte) []byte {
	key := make([]byte, 33)
	key[0] = 0x02
	for i := 1; i < len(key); i++ {
		key[i] = fill
	}

	return key
}

// testHash returns a deterministic hash for tests.
func testHash(label string) chainhash.Hash {
	return chainhash.DoubleHashH([]byte(label))
}

// testHashBytes returns deterministic hash bytes for tests.
func testHashBytes(label string) []byte {
	hash := testHash(label)

	return hash.CloneBytes()
}
