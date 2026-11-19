package oor

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	clientdb "github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// preparedReplayChainSource keeps the real VTXO actors below their expiry
// threshold without introducing unrelated block notifications.
type preparedReplayChainSource struct{}

// Receive supplies the subscription and initial tip used by recovered actors.
func (*preparedReplayChainSource) Receive(_ context.Context,
	msg chainsource.ChainSourceMsg) fn.Result[chainsource.ChainSourceResp] {

	switch msg.(type) {
	case *chainsource.SubscribeBlocksRequest:
		return fn.Ok[chainsource.ChainSourceResp](
			&chainsource.SubscribeBlocksResponse{},
		)

	case *chainsource.BestHeightRequest:
		return fn.Ok[chainsource.ChainSourceResp](
			&chainsource.BestHeightResponse{
				Height: 100,
			},
		)

	default:
		return fn.Err[chainsource.ChainSourceResp](
			fmt.Errorf("unexpected chain request %T", msg),
		)
	}
}

// TestPreparedReserveEpochs keeps legacy unknown epochs out of guarded
// releases while retaining every known reservation by its exact outpoint.
func TestPreparedReserveEpochs(t *testing.T) {
	t.Parallel()

	first := wire.OutPoint{Hash: [32]byte{0x83}, Index: 1}
	second := wire.OutPoint{Hash: first.Hash, Index: 2}
	legacy := wire.OutPoint{Hash: first.Hash, Index: 3}
	state := &Prepared{TransferInputs: []TransferInput{
		{
			VTXO: &vtxo.Descriptor{
				Outpoint: first,
			},
			ReserveEpoch: 41,
		},
		{
			VTXO: &vtxo.Descriptor{
				Outpoint: second,
			},
			ReserveEpoch: 42,
		},
		{
			VTXO: &vtxo.Descriptor{
				Outpoint: legacy,
			},
		},
		{
			ReserveEpoch: 43,
		},
	}}
	require.Equal(t, map[wire.OutPoint]uint64{
		first: 41, second: 42,
	}, prePONRReserveEpochs(state))
}

// TestPreparedAbortReplayPreservesNewReservation exercises an abort whose
// release succeeds but snapshot commit fails. Replaying the saved Prepared
// state must not release a newer owner's reservation of the same input.
func TestPreparedAbortReplayPreservesNewReservation(t *testing.T) {
	t.Parallel()

	for name, event := range map[string]Event{
		"abort": &AbortPreparedEvent{
			Reason: "negotiation canceled",
		},
		"fail": &FailEvent{
			Reason: "preparation failed",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assertPreparedAbortReplay(t, event)
		})
	}
}

// assertPreparedAbortReplay verifies reservation fencing across abort replay.
func assertPreparedAbortReplay(t *testing.T, event Event) {
	t.Helper()

	ctx := t.Context()
	operator, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	client, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	input := newTestTransferInput(
		t, client, operator.PubKey(), wire.OutPoint{
			Hash: [32]byte{0x84},
		},
		btcutil.Amount(10_000),
	)
	input.VTXO.Status = vtxo.VTXOStatusLive
	input.VTXO.RoundID = "prepared-replay-round"
	input.VTXO.CreatedHeight = 100
	input.VTXO.BatchExpiry = 10_000
	op := input.VTXO.Outpoint

	sqlDB := clientdb.NewTestDB(t)
	roundDB := clientdb.NewTransactionExecutor(
		sqlDB.BaseDB,
		func(tx *sql.Tx) clientdb.RoundStore {
			return sqlDB.WithTx(tx)
		},
		btclog.Disabled,
	)
	coins := clientdb.NewVTXOPersistenceStore(
		roundDB, clock.NewDefaultClock(),
	)
	require.NoError(t, coins.SaveVTXO(ctx, input.VTXO))
	system := actor.NewActorSystem()
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(
			context.Background(), 5*time.Second,
		)
		defer cancel()
		require.NoError(t, system.Shutdown(stopCtx))
	})
	chainKey := actor.NewServiceKey[
		chainsource.ChainSourceMsg,
		chainsource.ChainSourceResp,
	](
		"prepared-replay-chain",
	)
	chainRef := chainKey.Spawn(
		system, "prepared-replay-chain", &preparedReplayChainSource{},
	)
	manager := vtxo.NewManager(&vtxo.ManagerConfig{
		Store: coins, ActorSystem: system,
		ChainSource: chainRef,
		Log: fn.Some[btclog.Logger](
			btclog.Disabled,
		),
	})
	managerRef := actormsg.VTXOManagerServiceKey().Spawn(
		system, actormsg.VTXOManagerServiceKeyName, manager,
	)
	require.NoError(t, manager.Start(ctx, managerRef))
	w := wallet.NewArk(
		nil, nil, nil, nil, system, fn.None[ledger.Sink](),
		btclog.Disabled,
	)
	reserve := func() uint64 {
		response, err := w.Receive(
			ctx, &wallet.SelectAndLockVTXOsRequest{
				TargetAmount: input.VTXO.Amount,
				Outpoints:    []wire.OutPoint{op},
			},
		).Unpack()
		require.NoError(t, err)
		locked, ok := response.(*wallet.SelectAndLockVTXOsResponse)
		require.True(t, ok)
		require.Len(t, locked.SelectedVTXOs, 1)

		// Selection is detached from the durable
		// Spending write.
		require.Eventually(t, func() bool {
			desc, err := coins.GetVTXO(ctx, op)

			return err == nil &&
				desc.Status == vtxo.VTXOStatusSpending
		}, 5*time.Second, time.Millisecond)

		return locked.SelectedVTXOs[0].ReserveEpoch
	}
	epochA := reserve()
	require.NotZero(t, epochA)
	input.ReserveEpoch = epochA
	session, _, err := NewPreparedSessionWithIdempotencyKey(
		ctx, arkscript.CheckpointPolicy{
			OperatorKey: operator.PubKey(), CSVDelay: 10,
		}, []TransferInput{input}, []oortx.RecipientOutput{{
			PkScript: input.VTXO.PkScript,
			Value:    input.VTXO.Amount,
		}}, "", EnvConfig{},
	)
	require.NoError(t, err)
	initial, err := session.FSM.CurrentState()
	require.NoError(t, err)
	prepared, ok := initial.(*Prepared)
	require.True(t, ok)
	record, err := outgoingRegistryRecord(
		session.ID, prepared,
	)
	require.NoError(t, err)
	base := newFakeRegistryStore()
	require.NoError(t, base.UpsertSession(ctx, record))
	store := &failFirstUpsertStore{
		fakeRegistryStore: base, failsLeft: 1,
	}
	var released []int
	var releaseEpochs []uint64
	release := func(ctx context.Context, ops []wire.OutPoint,
		epochs map[wire.OutPoint]uint64) error {

		response, err := managerRef.Ask(
			ctx, &actormsg.ReleaseSpendRequest{
				Outpoints: ops, ReserveEpochs: epochs,
			},
		).Await(ctx).Unpack()
		if err != nil {
			return err
		}
		releasedResp, ok := response.(*actormsg.ReleaseSpendResponse)
		require.True(t, ok)
		released = append(
			released, releasedResp.ReleasedCount,
		)
		releaseEpochs = append(
			releaseEpochs, epochs[op],
		)

		return nil
	}
	b := &sessionBehavior{
		cfg: SessionActorConfig{
			RegistryStore: store,
			ServerConn:    fakeServerConnRef{},
			VTXOStore:     coins,
			SpendReleaser: release,
		},
		actorID:   ActorIDForSession(session.ID),
		log:       btclog.Disabled,
		sessionID: session.ID,
		direction: clientdb.OORSessionDirectionOutgoing,
		fsm:       session.FSM, loaded: true,
	}
	t.Cleanup(b.stopFSM)
	ax := fakeExec{tx: oorTx{
		store: &fakeDeliveryStore{}, actorID: b.actorID,
	}}
	drive := &DriveEventRequest{
		SessionID: session.ID, Event: event,
	}
	result := b.Receive(ctx, drive, ax)
	require.ErrorIs(t, result.Err(), errFilterBroken)
	require.True(t, b.commitFailed)
	require.Equal(t, []int{1}, released)
	desc, err := coins.GetVTXO(ctx, op)
	require.NoError(t, err)
	require.Equal(t, vtxo.VTXOStatusLive, desc.Status)
	saved, err := base.GetSession(
		ctx, chainHashOf(session.ID),
	)
	require.NoError(t, err)
	require.Equal(
		t, record.SnapshotData, saved.SnapshotData,
	)
	snapshot, err := decodeOutgoingSnapshot(
		saved.SnapshotData,
	)
	require.NoError(t, err)
	restored, err := OutgoingStateFromSnapshot(snapshot)
	require.NoError(t, err)
	prepared, ok = restored.(*Prepared)
	require.True(t, ok)
	require.Equal(
		t, epochA,
		prepared.TransferInputs[0].ReserveEpoch,
	)

	epochB := reserve()
	require.Greater(t, epochB, epochA)
	result = b.Receive(ctx, drive, ax)
	require.True(t, result.IsOk(), result.Err())
	require.False(t, b.commitFailed)
	require.Equal(
		t, []int{1, 0}, released,
		"replayed abort must not release the newer reservation",
	)
	require.Equal(
		t, []uint64{epochA, epochA}, releaseEpochs,
	)
	desc, err = coins.GetVTXO(ctx, op)
	require.NoError(t, err)
	require.Equal(t, vtxo.VTXOStatusSpending, desc.Status)
	_, err = w.Receive(ctx, &wallet.SelectAndLockVTXOsRequest{
		TargetAmount: input.VTXO.Amount,
		Outpoints:    []wire.OutPoint{op},
	}).Unpack()
	require.ErrorIs(t, err, vtxo.ErrVTXOLiquidityLocked)
	saved, err = base.GetSession(
		ctx, chainHashOf(session.ID),
	)
	require.NoError(t, err)
	require.Equal(
		t, clientdb.OORSessionStatusFailed,
		saved.Status,
	)

	// The newer owner's matching epoch must still release
	// X.
	require.NoError(
		t,
		release(
			ctx, []wire.OutPoint{op}, map[wire.OutPoint]uint64{
				op: epochB,
			},
		),
	)
	require.Equal(t, []int{1, 0, 1}, released)
}
