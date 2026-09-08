package waved

import (
	"database/sql"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newListVTXOsStore returns an empty SQL-backed VTXO store.
func newListVTXOsStore(t *testing.T) *db.VTXOPersistenceStore {
	t.Helper()

	sqlDB := db.NewTestDB(t)
	roundDB := db.NewTransactionExecutor(
		sqlDB.BaseDB,
		func(tx *sql.Tx) db.RoundStore {
			return sqlDB.WithTx(tx)
		},
		btclog.Disabled,
	)

	return db.NewVTXOPersistenceStore(roundDB, clock.NewDefaultClock())
}

// newListVTXOsServer returns a wallet-ready RPC server.
func newListVTXOsServer(store *db.VTXOPersistenceStore,
	system *actor.ActorSystem) *RPCServer {

	walletReady := make(chan struct{})
	close(walletReady)

	return &RPCServer{server: &Server{
		actorSystem: system,
		walletReady: walletReady,
		vtxoStore:   store,
	}}
}

// responseStatuses projects the status of every returned VTXO.
func responseStatuses(resp *waverpc.ListVTXOsResponse) []waverpc.VTXOStatus {
	statuses := make([]waverpc.VTXOStatus, 0, len(resp.Vtxos))
	for _, v := range resp.Vtxos {
		statuses = append(statuses, v.Status)
	}

	return statuses
}

// TestListVTXOsStatusSelection covers the default inventory set, explicit
// status lists, the status_filter union and the UNSPECIFIED rejection.
func TestListVTXOsStatusSelection(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	vtxoStore := newListVTXOsStore(t)

	seeded := map[vtxo.VTXOStatus]*vtxo.Descriptor{
		vtxo.VTXOStatusLive: newRefreshEstimateVTXO(
			t, 0x01, 10_000, 900,
		),
		vtxo.VTXOStatusForfeited: newRefreshEstimateVTXO(
			t, 0x02, 10_000, 900,
		),
		vtxo.VTXOStatusSpent: newRefreshEstimateVTXO(
			t, 0x03, 10_000, 900,
		),
		vtxo.VTXOStatusUnilateralExit: newRefreshEstimateVTXO(
			t, 0x04, 10_000, 900,
		),
		vtxo.VTXOStatusExpired: newRefreshEstimateVTXO(
			t, 0x05, 10_000, 900,
		),
		vtxo.VTXOStatusSpending: newRefreshEstimateVTXO(
			t, 0x06, 10_000, 900,
		),
	}
	for vtxoStatus, desc := range seeded {
		require.NoError(t, vtxoStore.SaveVTXO(ctx, desc))
		require.NoError(
			t, vtxoStore.UpdateVTXOStatus(
				ctx, desc.Outpoint, vtxoStatus,
			),
		)
	}

	srv := newListVTXOsServer(vtxoStore, nil)

	inventory, err := srv.ListVTXOs(ctx, &waverpc.ListVTXOsRequest{})
	require.NoError(t, err)
	require.ElementsMatch(t, []waverpc.VTXOStatus{
		waverpc.VTXOStatus_VTXO_STATUS_LIVE,
		waverpc.VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT,
		waverpc.VTXOStatus_VTXO_STATUS_EXPIRED,
		waverpc.VTXOStatus_VTXO_STATUS_SPENDING,
	}, responseStatuses(inventory))

	consumed, err := srv.ListVTXOs(ctx, &waverpc.ListVTXOsRequest{
		Statuses: []waverpc.VTXOStatus{
			waverpc.VTXOStatus_VTXO_STATUS_FORFEITED,
			waverpc.VTXOStatus_VTXO_STATUS_SPENT,
		},
	})
	require.NoError(t, err)
	require.ElementsMatch(t, []waverpc.VTXOStatus{
		waverpc.VTXOStatus_VTXO_STATUS_FORFEITED,
		waverpc.VTXOStatus_VTXO_STATUS_SPENT,
	}, responseStatuses(consumed))

	union, err := srv.ListVTXOs(ctx, &waverpc.ListVTXOsRequest{
		StatusFilter: waverpc.VTXOStatus_VTXO_STATUS_LIVE,
		Statuses: []waverpc.VTXOStatus{
			waverpc.VTXOStatus_VTXO_STATUS_SPENT,
		},
	})
	require.NoError(t, err)
	require.ElementsMatch(t, []waverpc.VTXOStatus{
		waverpc.VTXOStatus_VTXO_STATUS_LIVE,
		waverpc.VTXOStatus_VTXO_STATUS_SPENT,
	}, responseStatuses(union))

	single, err := srv.ListVTXOs(ctx, &waverpc.ListVTXOsRequest{
		StatusFilter: waverpc.VTXOStatus_VTXO_STATUS_SPENT,
	})
	require.NoError(t, err)
	require.Equal(t, []waverpc.VTXOStatus{
		waverpc.VTXOStatus_VTXO_STATUS_SPENT,
	}, responseStatuses(single))

	_, err = srv.ListVTXOs(ctx, &waverpc.ListVTXOsRequest{
		Statuses: []waverpc.VTXOStatus{
			waverpc.VTXOStatus_VTXO_STATUS_UNSPECIFIED,
		},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestListVTXOsPendingRoundCombinesWithStored verifies that pending-round
// projections precede stored rows when both are requested and are absent
// from the default listing.
func TestListVTXOsPendingRoundCombinesWithStored(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	roundID := round.RoundID(uuid.New())
	state := &round.InputSigSentState{
		RoundID: roundID,
		Intents: round.Intents{
			VTXOs: []types.VTXORequest{{
				Amount:   btcutil.Amount(50_000),
				OwnerKey: localOwnerKey(t),
			}},
		},
	}
	system := newRoundActorWithStates(t,
		map[string]round.FSMStateInfo{
			roundID.String(): {
				State:   state,
				RoundID: roundID,
			},
		},
	)
	defer func() {
		require.NoError(t, system.Shutdown(ctx))
	}()

	vtxoStore := newListVTXOsStore(t)
	live := newRefreshEstimateVTXO(t, 0x01, 10_000, 900)
	require.NoError(t, vtxoStore.SaveVTXO(ctx, live))

	srv := newListVTXOsServer(vtxoStore, system)

	combined, err := srv.ListVTXOs(ctx, &waverpc.ListVTXOsRequest{
		Statuses: []waverpc.VTXOStatus{
			waverpc.VTXOStatus_VTXO_STATUS_LIVE,
			waverpc.VTXOStatus_VTXO_STATUS_PENDING_ROUND,
		},
	})
	require.NoError(t, err)
	require.Len(t, combined.Vtxos, 2)
	require.Equal(
		t, waverpc.VTXOStatus_VTXO_STATUS_PENDING_ROUND,
		combined.Vtxos[0].Status,
	)
	require.Equal(t, roundID.String(), combined.Vtxos[0].RoundId)
	require.Equal(
		t, waverpc.VTXOStatus_VTXO_STATUS_LIVE,
		combined.Vtxos[1].Status,
	)
	require.Equal(t, live.Outpoint.String(), combined.Vtxos[1].Outpoint)

	inventory, err := srv.ListVTXOs(ctx, &waverpc.ListVTXOsRequest{})
	require.NoError(t, err)
	require.Equal(t, []waverpc.VTXOStatus{
		waverpc.VTXOStatus_VTXO_STATUS_LIVE,
	}, responseStatuses(inventory))
}
