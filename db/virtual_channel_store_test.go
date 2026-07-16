package db

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/virtualchannel"
	"github.com/lightninglabs/wavelength/vtxo"
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

	backingVTXOs := []virtualchannel.BackingVTXO{{
		OutPoint: wire.OutPoint{
			Hash: testHash("virtual-channel-a"), Index: 1,
		},
		Amount: btcutil.Amount(100000),
	}}

	insertBackingVTXOs(t, sqlDB.Queries, backingVTXOs)

	candidates, err := sqlDB.Queries.ListVTXOSelectionCandidatesByStatus(
		ctx, int32(vtxo.VTXOStatusLive),
	)
	require.NoError(t, err)
	require.Len(t, candidates, 1)

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
		Kind:             virtualchannel.KindPromoteVTXO,
		PendingChannelID: fixedPendingChannelID(2),
		ChannelPoint:     channelPoint,
		RemoteNodePubKey: fixedNodePubKey(3),
		Role:             virtualchannel.RoleClient,
		Status:           virtualchannel.StatusLNDNegotiating,
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
		Kind:             virtualchannel.KindPromoteVTXO,
		StateVersion:     1,
		PendingChannelID: reg.PendingChannelID,
		RemoteNodePubKey: reg.RemoteNodePubKey,
		Role:             reg.Role,
		Status:           virtualchannel.StatusFundingBound,
		Capacity:         reg.Capacity,
		LocalBalance:     reg.LocalBalance,
		RemoteBalance:    reg.RemoteBalance,
		BackingVTXOs:     backingVTXOs,
	}
	err = virtualStore.InsertVirtualChannelPendingOpen(ctx, pending)
	require.NoError(t, err)
	require.NoError(
		t, virtualStore.InsertVirtualChannelPendingOpen(ctx, pending),
	)
	candidates, err = sqlDB.Queries.ListVTXOSelectionCandidatesByStatus(
		ctx, int32(vtxo.VTXOStatusLive),
	)
	require.NoError(t, err)
	require.Empty(t, candidates)
	conflictingPending := pending
	conflictingPending.RemoteNodePubKey = fixedNodePubKey(99)
	require.ErrorContains(
		t, virtualStore.InsertVirtualChannelPendingOpen(
			ctx, conflictingPending,
		),
		"different virtual channel",
	)
	duplicateBacking := pending
	duplicateBacking.PendingChannelID = fixedPendingChannelID(98)
	require.ErrorContains(
		t, virtualStore.InsertVirtualChannelPendingOpen(
			ctx, duplicateBacking,
		),
		"already backs another virtual channel",
	)

	pendingLoaded, ok, err := virtualStore.FindVirtualChannelPendingOpen(
		ctx, reg.PendingChannelID,
	)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, pending, *pendingLoaded)
	changed, err := virtualStore.MarkVirtualChannelLNDNegotiating(
		ctx, reg.PendingChannelID,
	)
	require.NoError(t, err)
	require.True(t, changed)

	err = virtualStore.InsertVirtualChannel(ctx, reg)
	require.NoError(t, err)
	require.NoError(t, virtualStore.InsertVirtualChannel(ctx, reg))

	// Promoting the pending intent atomically replaces its VTXO reference
	// with the full channel reference. The backing VTXO must remain
	// excluded from ordinary Ark coin selection throughout that handoff.
	candidates, err = sqlDB.Queries.ListVTXOSelectionCandidatesByStatus(
		ctx, int32(vtxo.VTXOStatusLive),
	)
	require.NoError(t, err)
	require.Empty(t, candidates)

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
	require.Equal(t, uint64(2), loaded.StateVersion)
	require.Equal(t, reg.Capacity, loaded.Capacity)
	require.Equal(t, reg.LocalBalance, loaded.LocalBalance)
	require.Equal(t, reg.RemoteBalance, loaded.RemoteBalance)
	require.Equal(t, reg.FundingPsbt, loaded.FundingPsbt)
	require.Equal(t, backingVTXOs, loaded.BackingVTXOs)
	byBacking, found, err := virtualStore.FindVirtualChannelByBackingVTXO(
		ctx, backingVTXOs[0].OutPoint,
	)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, reg.ID, byBacking.ID)
	_, found, err = virtualStore.FindVirtualChannelByBackingVTXO(
		ctx, wire.OutPoint{
			Hash: testHash("not-a-channel"),
		},
	)
	require.NoError(t, err)
	require.False(t, found)

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

	changed, err = virtualStore.MarkVirtualChannelFailed(ctx, reg.ID)
	require.NoError(t, err)
	require.True(t, changed)
	candidates, err = sqlDB.Queries.ListVTXOSelectionCandidatesByStatus(
		ctx, int32(vtxo.VTXOStatusLive),
	)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	replacement := pending
	replacement.PendingChannelID = fixedPendingChannelID(97)
	replacement.StateVersion = 1
	require.NoError(
		t, virtualStore.InsertVirtualChannelPendingOpen(
			ctx, replacement,
		),
	)
}

func TestVirtualChannelStoreBindsDurableReceiveRequest(t *testing.T) {
	t.Parallel()

	sqlDB := NewTestDB(t)
	store := NewStore(
		sqlDB.DB, sqlDB.Queries, sqlDB.Backend(), sqlDB.log,
	)
	ctx := t.Context()
	virtualStore := store.NewVirtualChannelStore()
	pending := virtualchannel.PendingOpen{
		Kind:             virtualchannel.KindReceiveChannel,
		RequestKey:       "receive-request-1",
		StateVersion:     1,
		PendingChannelID: fixedPendingChannelID(20),
		RemoteNodePubKey: fixedNodePubKey(21),
		Role:             virtualchannel.RoleClient,
		Status:           virtualchannel.StatusRequested,
		Capacity:         50_000,
		LocalBalance:     0,
		RemoteBalance:    50_000,
	}

	require.NoError(
		t, virtualStore.InsertVirtualChannelPendingOpen(
			ctx, pending,
		),
	)
	require.NoError(
		t, virtualStore.InsertVirtualChannelPendingOpen(
			ctx, pending,
		),
	)
	requested, err := virtualStore.ListVirtualChannelPendingOpensByStatus(
		ctx, virtualchannel.StatusRequested,
	)
	require.NoError(t, err)
	require.Len(t, requested, 1)
	require.Equal(
		t, pending.PendingChannelID, requested[0].PendingChannelID,
	)

	changed, err := virtualStore.MarkVirtualChannelRoundRequested(
		ctx, pending.PendingChannelID,
	)
	require.NoError(t, err)
	require.True(t, changed)
	changed, err = virtualStore.MarkVirtualChannelRoundRequested(
		ctx, pending.PendingChannelID,
	)
	require.NoError(t, err)
	require.False(t, changed)

	backing := virtualchannel.BackingVTXO{
		OutPoint: wire.OutPoint{
			Hash: testHash("receive-request-vtxo"), Index: 1,
		},
		Amount: 51_000,
		PkScript: []byte{
			0x51,
		},
		PolicyTemplate: []byte{
			0x01,
		},
	}
	insertBackingVTXOs(
		t, sqlDB.Queries, []virtualchannel.BackingVTXO{backing},
	)
	bound := pending
	bound.RoundID = "receive-round-1"
	bound.StateVersion = 2
	bound.Status = virtualchannel.StatusFundingBound
	bound.BackingVTXOs = []virtualchannel.BackingVTXO{backing}

	changed, err = virtualStore.BindVirtualChannelPendingOpen(ctx, bound)
	require.NoError(t, err)
	require.True(t, changed)
	changed, err = virtualStore.BindVirtualChannelPendingOpen(ctx, bound)
	require.NoError(t, err)
	require.False(t, changed)

	loaded, ok, err := virtualStore.FindVirtualChannelPendingOpen(
		ctx, pending.PendingChannelID,
	)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, virtualchannel.StatusFundingBound, loaded.Status)
	require.Equal(t, uint64(3), loaded.StateVersion)
	require.Equal(t, bound.RoundID, loaded.RoundID)
	require.Equal(t, bound.BackingVTXOs, loaded.BackingVTXOs)

	reserved, err := sqlDB.Queries.ListSpendingReservationOutpoints(ctx)
	require.NoError(t, err)
	require.Len(t, reserved, 1)
	require.Equal(t, backing.OutPoint.Hash[:], reserved[0].OutpointHash)
	require.Equal(
		t, int32(backing.OutPoint.Index), reserved[0].OutpointIndex,
	)

	changed, err = virtualStore.MarkVirtualChannelLNDNegotiating(
		ctx, pending.PendingChannelID,
	)
	require.NoError(t, err)
	require.True(t, changed)
	changed, err = virtualStore.BindVirtualChannelPendingOpen(ctx, bound)
	require.NoError(t, err)
	require.False(t, changed)

	conflict := bound
	conflict.RoundID = "receive-round-2"
	_, err = virtualStore.BindVirtualChannelPendingOpen(ctx, conflict)
	require.ErrorContains(t, err, "another round VTXO")

	failed, err := virtualStore.FailRoundVirtualChannels(ctx, bound.RoundID)
	require.NoError(t, err)
	require.EqualValues(t, 1, failed)
	candidates, err := sqlDB.Queries.ListVTXOSelectionCandidatesByStatus(
		ctx, int32(vtxo.VTXOStatusLive),
	)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	replacement := bound
	replacement.PendingChannelID = fixedPendingChannelID(22)
	replacement.RequestKey = "receive-request-2"
	replacement.RoundID = "receive-round-2"
	replacement.StateVersion = 1
	require.NoError(
		t, virtualStore.InsertVirtualChannelPendingOpen(
			ctx, replacement,
		),
	)
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

	backing, backingTx, signedTx, fundingPSBT := testVirtualChannelProof(
		t, wire.OutPoint{
			Hash: testHash("virtual-active-a"), Index: 1,
		}, 70000, 69000,
	)
	backingVTXOs := []virtualchannel.BackingVTXO{backing}
	insertBackingVTXOs(t, sqlDB.Queries, backingVTXOs)

	reg := virtualchannel.Registration{
		ID:               fixedVirtualChannelID(9),
		Kind:             virtualchannel.KindPromoteVTXO,
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
		FundingPsbt:      fundingPSBT,
		BackingVTXOs:     backingVTXOs,
	}

	virtualStore := store.NewVirtualChannelStore()
	require.NoError(t, virtualStore.InsertVirtualChannel(ctx, reg))
	changed, err := virtualStore.MarkVirtualChannelFundingVerified(
		ctx, reg.ID,
	)
	require.NoError(t, err)
	require.True(t, changed)

	changed, err = virtualStore.ArmVirtualChannelBacking(
		ctx, reg.ID, backingTx,
	)
	require.ErrorContains(t, err, "has no witness")
	require.False(t, changed)

	require.Equal(t, backingTx.TxHash(), signedTx.TxHash())

	changed, err = virtualStore.ArmVirtualChannelBacking(
		ctx, reg.ID, signedTx,
	)
	require.NoError(t, err)
	require.True(t, changed)

	loaded, err := virtualStore.GetVirtualChannel(ctx, reg.ID)
	require.NoError(t, err)
	require.Equal(t, virtualchannel.StatusBackingArmed, loaded.Status)

	changed, err = virtualStore.ActivateVirtualChannel(ctx, reg.ID)
	require.NoError(t, err)
	require.True(t, changed)

	loaded, err = virtualStore.GetVirtualChannel(ctx, reg.ID)
	require.NoError(t, err)
	require.Equal(t, virtualchannel.StatusActive, loaded.Status)
	require.Equal(
		t, signedTx.TxIn[0].Witness, loaded.BackingTx.TxIn[0].Witness,
	)

	changed, err = virtualStore.ArmVirtualChannelBacking(
		ctx, reg.ID, signedTx,
	)
	require.NoError(t, err)
	require.False(t, changed)
}

// TestFailedRoundClosesArmedReceiveChannel verifies that a failed round cannot
// make a fully signed backing VTXO spendable by another flow.
func TestFailedRoundClosesArmedReceiveChannel(t *testing.T) {
	t.Parallel()

	sqlDB := NewTestDB(t)
	store := NewStore(
		sqlDB.DB, sqlDB.Queries, sqlDB.Backend(), sqlDB.log,
	)
	ctx := t.Context()
	backing, backingTx, signedTx, fundingPSBT := testVirtualChannelProof(
		t, wire.OutPoint{
			Hash: testHash("failed-armed-receive"), Index: 0,
		}, 70_000, 69_000,
	)
	insertBackingVTXOs(
		t, sqlDB.Queries, []virtualchannel.BackingVTXO{backing},
	)

	reg := virtualchannel.Registration{
		ID:               fixedVirtualChannelID(70),
		Kind:             virtualchannel.KindReceiveChannel,
		RoundID:          "failed-armed-round",
		PendingChannelID: fixedPendingChannelID(71),
		ChannelPoint: wire.OutPoint{
			Hash: backingTx.TxHash(), Index: 0,
		},
		RemoteNodePubKey: fixedNodePubKey(72),
		Role:             virtualchannel.RoleClient,
		Status:           virtualchannel.StatusLNDNegotiating,
		Capacity:         69_000,
		LocalBalance:     0,
		RemoteBalance:    69_000,
		BackingTx:        backingTx,
		FundingPsbt:      fundingPSBT,
		BackingVTXOs: []virtualchannel.BackingVTXO{
			backing,
		},
	}
	virtualStore := store.NewVirtualChannelStore()
	require.NoError(t, virtualStore.InsertVirtualChannel(ctx, reg))
	changed, err := virtualStore.MarkVirtualChannelFundingVerified(
		ctx, reg.ID,
	)
	require.NoError(t, err)
	require.True(t, changed)
	changed, err = virtualStore.ArmVirtualChannelBacking(
		ctx, reg.ID, signedTx,
	)
	require.NoError(t, err)
	require.True(t, changed)

	failed, err := virtualStore.FailRoundVirtualChannels(ctx, reg.RoundID)
	require.NoError(t, err)
	require.EqualValues(t, 1, failed)
	loaded, err := virtualStore.GetVirtualChannel(ctx, reg.ID)
	require.NoError(t, err)
	require.Equal(t, virtualchannel.StatusClosing, loaded.Status)
	candidates, err := sqlDB.Queries.ListVTXOSelectionCandidatesByStatus(
		ctx, int32(vtxo.VTXOStatusLive),
	)
	require.NoError(t, err)
	require.Empty(t, candidates)
	reserved, err := sqlDB.Queries.ListSpendingReservationOutpoints(ctx)
	require.NoError(t, err)
	require.Len(t, reserved, 1)
	replacement := virtualchannel.PendingOpen{
		Kind:             virtualchannel.KindReceiveChannel,
		RoundID:          "replacement-round",
		StateVersion:     1,
		PendingChannelID: fixedPendingChannelID(73),
		RemoteNodePubKey: reg.RemoteNodePubKey,
		Role:             reg.Role,
		Status:           virtualchannel.StatusFundingBound,
		Capacity:         reg.Capacity,
		LocalBalance:     reg.LocalBalance,
		RemoteBalance:    reg.RemoteBalance,
		BackingVTXOs:     reg.BackingVTXOs,
	}
	require.ErrorContains(
		t, virtualStore.InsertVirtualChannelPendingOpen(
			ctx, replacement,
		),
		"already backs another virtual channel",
	)
}

// TestReceiveChannelStoreRequiresRoundConfirmation proves that a fully signed
// receive channel remains unroutable until its exact backing round confirms.
func TestReceiveChannelStoreRequiresRoundConfirmation(t *testing.T) {
	t.Parallel()

	sqlDB := NewTestDB(t)
	store := NewStore(
		sqlDB.DB, sqlDB.Queries, sqlDB.Backend(), sqlDB.log,
	)
	ctx := t.Context()

	backing, backingTx, signedTx, fundingPSBT := testVirtualChannelProof(
		t, wire.OutPoint{
			Hash: testHash("receive-channel"), Index: 0,
		}, 150_000, 149_000,
	)
	insertBackingVTXOs(
		t, sqlDB.Queries, []virtualchannel.BackingVTXO{backing},
	)

	reg := virtualchannel.Registration{
		ID:               fixedVirtualChannelID(30),
		Kind:             virtualchannel.KindReceiveChannel,
		RoundID:          "round-receive-1",
		PendingChannelID: fixedPendingChannelID(31),
		ChannelPoint: wire.OutPoint{
			Hash: backingTx.TxHash(), Index: 0,
		},
		RemoteNodePubKey: fixedNodePubKey(32),
		Role:             virtualchannel.RoleClient,
		Status:           virtualchannel.StatusLNDNegotiating,
		Capacity:         149_000,
		LocalBalance:     0,
		RemoteBalance:    149_000,
		BackingTx:        backingTx,
		FundingPsbt:      fundingPSBT,
		BackingVTXOs: []virtualchannel.BackingVTXO{
			backing,
		},
	}

	virtualStore := store.NewVirtualChannelStore()
	require.NoError(t, virtualStore.InsertVirtualChannel(ctx, reg))
	changed, err := virtualStore.MarkVirtualChannelFundingVerified(
		ctx, reg.ID,
	)
	require.NoError(t, err)
	require.True(t, changed)

	changed, err = virtualStore.ArmVirtualChannelBacking(
		ctx, reg.ID, signedTx,
	)
	require.NoError(t, err)
	require.True(t, changed)

	loaded, err := virtualStore.GetVirtualChannel(ctx, reg.ID)
	require.NoError(t, err)
	require.Equal(t, virtualchannel.StatusBackingArmed, loaded.Status)
	require.Equal(t, uint64(3), loaded.StateVersion)

	changed, err = virtualStore.ActivateVirtualChannel(ctx, reg.ID)
	require.ErrorContains(
		t, err, "invalid receive_channel channel transition",
	)
	require.False(t, changed)

	count, err := virtualStore.ConfirmRoundVirtualChannels(
		ctx, "another-round",
	)
	require.NoError(t, err)
	require.Zero(t, count)

	count, err = virtualStore.MarkRoundVirtualChannelsConfirmed(
		ctx, reg.RoundID,
	)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)

	loaded, err = virtualStore.GetVirtualChannel(ctx, reg.ID)
	require.NoError(t, err)
	require.Equal(t, virtualchannel.StatusRoundConfirmed, loaded.Status)
	require.Equal(t, uint64(4), loaded.StateVersion)

	count, err = virtualStore.RecoverConfirmedVirtualChannels(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)

	loaded, err = virtualStore.GetVirtualChannel(ctx, reg.ID)
	require.NoError(t, err)
	require.Equal(t, virtualchannel.StatusActive, loaded.Status)
	require.Equal(t, uint64(5), loaded.StateVersion)
}

// TestRecoverArmedReceiveChannelWithoutRoundCloses proves that a restart
// preserves the ownership and materialization path of a signed channel whose
// round was never persisted at the client's point of no return.
func TestRecoverArmedReceiveChannelWithoutRoundCloses(t *testing.T) {
	t.Parallel()

	sqlDB := NewTestDB(t)
	store := NewStore(
		sqlDB.DB, sqlDB.Queries, sqlDB.Backend(), sqlDB.log,
	)
	ctx := t.Context()

	backing, backingTx, signedTx, fundingPSBT := testVirtualChannelProof(
		t, wire.OutPoint{
			Hash: testHash("orphaned-receive-channel"), Index: 0,
		}, 80_000, 79_000,
	)
	insertBackingVTXOs(
		t, sqlDB.Queries, []virtualchannel.BackingVTXO{backing},
	)

	reg := virtualchannel.Registration{
		ID:               fixedVirtualChannelID(40),
		Kind:             virtualchannel.KindReceiveChannel,
		RoundID:          "orphaned-receive-round",
		PendingChannelID: fixedPendingChannelID(41),
		ChannelPoint: wire.OutPoint{
			Hash: backingTx.TxHash(), Index: 0,
		},
		RemoteNodePubKey: fixedNodePubKey(42),
		Role:             virtualchannel.RoleClient,
		Status:           virtualchannel.StatusLNDNegotiating,
		Capacity:         79_000,
		RemoteBalance:    79_000,
		BackingTx:        backingTx,
		FundingPsbt:      fundingPSBT,
		BackingVTXOs: []virtualchannel.BackingVTXO{
			backing,
		},
	}
	virtualStore := store.NewVirtualChannelStore()
	require.NoError(t, virtualStore.InsertVirtualChannel(ctx, reg))
	_, err := virtualStore.MarkVirtualChannelFundingVerified(ctx, reg.ID)
	require.NoError(t, err)
	_, err = virtualStore.ArmVirtualChannelBacking(ctx, reg.ID, signedTx)
	require.NoError(t, err)

	changed, err := virtualStore.RecoverRoundVirtualChannels(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), changed)

	loaded, err := virtualStore.GetVirtualChannel(ctx, reg.ID)
	require.NoError(t, err)
	require.Equal(t, virtualchannel.StatusClosing, loaded.Status)
}

// TestRecoverPendingReceiveChannelWithoutRoundFails covers the pre-channel
// half of the same FSM, before lnd has produced a channel point.
func TestRecoverPendingReceiveChannelWithoutRoundFails(t *testing.T) {
	t.Parallel()

	sqlDB := NewTestDB(t)
	store := NewStore(
		sqlDB.DB, sqlDB.Queries, sqlDB.Backend(), sqlDB.log,
	)
	backing := virtualchannel.BackingVTXO{
		OutPoint: wire.OutPoint{
			Hash: testHash("orphaned-pending-receive"), Index: 0,
		},
		Amount: 80_000,
	}
	insertBackingVTXOs(
		t, sqlDB.Queries, []virtualchannel.BackingVTXO{backing},
	)

	pending := virtualchannel.PendingOpen{
		Kind:             virtualchannel.KindReceiveChannel,
		RequestKey:       "orphaned-pending-receive",
		RoundID:          "missing-round",
		PendingChannelID: fixedPendingChannelID(51),
		RemoteNodePubKey: fixedNodePubKey(52),
		Role:             virtualchannel.RoleClient,
		Status:           virtualchannel.StatusFundingBound,
		Capacity:         79_000,
		RemoteBalance:    79_000,
		BackingVTXOs: []virtualchannel.BackingVTXO{
			backing,
		},
	}
	virtualStore := store.NewVirtualChannelStore()
	require.NoError(
		t,
		virtualStore.InsertVirtualChannelPendingOpen(
			t.Context(), pending,
		),
	)

	changed, err := virtualStore.RecoverRoundVirtualChannels(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(1), changed)
	loaded, found, err := virtualStore.FindVirtualChannelPendingOpen(
		t.Context(), pending.PendingChannelID,
	)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, virtualchannel.StatusFailed, loaded.Status)
}

func testVirtualChannelProof(t *testing.T, outpoint wire.OutPoint,
	backingAmount, capacity int64) (virtualchannel.BackingVTXO, *wire.MsgTx,
	*wire.MsgTx, []byte) {

	t.Helper()

	witnessScript := []byte{txscript.OP_TRUE}
	witnessHash := sha256.Sum256(witnessScript)
	pkScript, err := txscript.NewScriptBuilder().AddOp(txscript.OP_0).
		AddData(witnessHash[:]).Script()
	require.NoError(t, err)

	fundingScript := append(
		[]byte{txscript.OP_1, 0x20}, make([]byte, 32)...,
	)
	backing := virtualchannel.BackingVTXO{
		OutPoint: outpoint, Amount: btcutil.Amount(backingAmount),
		PkScript: pkScript,
	}
	unsigned := wire.NewMsgTx(2)
	unsigned.AddTxIn(&wire.TxIn{
		PreviousOutPoint: outpoint, Sequence: wire.MaxTxInSequenceNum,
	})
	unsigned.AddTxOut(&wire.TxOut{Value: capacity, PkScript: fundingScript})
	signed := unsigned.Copy()
	signed.TxIn[0].Witness = wire.TxWitness{witnessScript}

	base := wire.NewMsgTx(2)
	base.AddTxOut(&wire.TxOut{Value: capacity, PkScript: fundingScript})
	packet, err := psbt.NewFromUnsignedTx(base)
	require.NoError(t, err)
	var encoded bytes.Buffer
	require.NoError(t, packet.Serialize(&encoded))

	return backing, unsigned, signed, encoded.Bytes()
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
		clientKeyID, err := q.UpsertInternalKey(
			ctx, sqlc.UpsertInternalKeyParams{
				Pubkey: fixedPubKey(3), KeyFamily: 1,
				KeyIndex: 2, CreatedAt: now,
			},
		)
		require.NoError(t, err)

		err = q.InsertVTXO(
			ctx, sqlc.InsertVTXOParams{
				OutpointHash:  backing.OutPoint.Hash[:],
				OutpointIndex: int32(backing.OutPoint.Index),
				RoundID:       "virtual-channel-test-round",
				Amount:        int64(backing.Amount),
				PkScript: append(
					[]byte{0x51, 0x20}, make([]byte, 32)...,
				),
				Expiry:         144,
				PolicyTemplate: []byte{0x01},
				ClientKeyID: sql.NullInt64{
					Int64: clientKeyID, Valid: true,
				},
				OperatorPubkey: fixedPubKey(4),
				BatchExpiry:    300,
				ChainDepth:     1,
				CreatedHeight:  100,
				CommitmentTxid: testHashBytes("commitment"),
				Spent:          false,
				CreationTime:   now,
				LastUpdateTime: now,
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
