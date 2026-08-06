package waved

import (
	"database/sql"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newLeaveAdmissionServer wires only the SQL VTXO store, which is all the
// leave admission filter reads. Nothing wallet-related is needed: the filter
// and the dry-run preview both run before the wallet-ready gate.
func newLeaveAdmissionServer(t *testing.T) (*RPCServer,
	*db.VTXOPersistenceStore) {

	t.Helper()

	sqlDB := db.NewTestDB(t)
	roundDB := db.NewTransactionExecutor(
		sqlDB.BaseDB,
		func(tx *sql.Tx) db.RoundStore {
			return sqlDB.WithTx(tx)
		},
		btclog.Disabled,
	)
	vtxoStore := db.NewVTXOPersistenceStore(
		roundDB, clock.NewDefaultClock(),
	)

	return &RPCServer{server: &Server{
		log:       btclog.Disabled,
		vtxoStore: vtxoStore,
	}}, vtxoStore
}

// saveVTXOWithStatus persists a descriptor in the given lifecycle status so
// the admission filter has a realistic row to read back.
func saveVTXOWithStatus(t *testing.T, store *db.VTXOPersistenceStore,
	hashByte byte, status vtxo.VTXOStatus) *vtxo.Descriptor {

	t.Helper()

	desc := newRefreshEstimateVTXO(t, hashByte, 50_000, 900)
	require.NoError(t, store.SaveVTXO(t.Context(), desc))

	// SaveVTXO inserts at a fixed status and ignores desc.Status, so the
	// lifecycle state under test has to be applied as an update.
	desc.Status = status
	require.NoError(
		t,
		store.UpdateVTXOStatus(
			t.Context(), desc.Outpoint, status,
		),
	)

	return desc
}

// TestAdmitLeaveTargetsStrictRejectsCommitted is the regression guard for
// wavelength#577: a second leave naming a VTXO already committed to a round
// must be refused with an actionable error, not with the VTXO manager's raw
// FSM event-type mismatch.
func TestAdmitLeaveTargetsStrictRejectsCommitted(t *testing.T) {
	t.Parallel()

	for _, committed := range []vtxo.VTXOStatus{
		vtxo.VTXOStatusPendingForfeit, vtxo.VTXOStatusForfeiting,
	} {
		t.Run(committed.String(), func(t *testing.T) {
			t.Parallel()

			r, store := newLeaveAdmissionServer(t)
			desc := saveVTXOWithStatus(t, store, 0x11, committed)

			_, err := r.admitLeaveTargets(
				t.Context(), []wire.OutPoint{desc.Outpoint},
				true,
			)
			require.Error(t, err)
			require.Equal(
				t, codes.FailedPrecondition, status.Code(err),
			)

			// The message must name the coin and say what holds
			// it — that is the whole point of the pre-check.
			require.Contains(t, err.Error(), desc.Outpoint.String())
			require.Contains(
				t, err.Error(),
				"already committed to a round",
			)
		})
	}
}

// TestAdmitLeaveTargetsAllSkipsCommitted asserts selection=all drops the
// VTXOs it cannot leave rather than failing the batch. ListLiveVTXOs returns
// every non-terminal VTXO, so without this filter one in-flight coin would
// sink an otherwise valid "leave everything" request.
func TestAdmitLeaveTargetsAllSkipsCommitted(t *testing.T) {
	t.Parallel()

	r, store := newLeaveAdmissionServer(t)

	live := saveVTXOWithStatus(t, store, 0x21, vtxo.VTXOStatusLive)
	forfeiting := saveVTXOWithStatus(
		t, store, 0x22, vtxo.VTXOStatusForfeiting,
	)

	admitted, err := r.admitLeaveTargets(
		t.Context(),
		[]wire.OutPoint{live.Outpoint, forfeiting.Outpoint}, false,
	)
	require.NoError(t, err)
	require.Equal(t, []wire.OutPoint{live.Outpoint}, admitted)
}

// TestAdmitLeaveTargetsAdmitsExpired asserts an expired VTXO stays leavable.
// Forfeiting it into a round is the only way its value is ever recovered, so
// a filter that refused it would strand the coin permanently.
func TestAdmitLeaveTargetsAdmitsExpired(t *testing.T) {
	t.Parallel()

	r, store := newLeaveAdmissionServer(t)
	desc := saveVTXOWithStatus(t, store, 0x31, vtxo.VTXOStatusExpired)

	admitted, err := r.admitLeaveTargets(
		t.Context(), []wire.OutPoint{desc.Outpoint}, true,
	)
	require.NoError(t, err)
	require.Equal(t, []wire.OutPoint{desc.Outpoint}, admitted)
}

// TestAdmitLeaveTargetsUnknownOutpoint asserts the two selection modes
// disagree on an unknown outpoint in the direction that matters: an explicit
// request is a caller mistake, an "all" sweep simply has nothing to say.
func TestAdmitLeaveTargetsUnknownOutpoint(t *testing.T) {
	t.Parallel()

	r, _ := newLeaveAdmissionServer(t)
	missing := wire.OutPoint{Hash: chainhash.Hash{0x41}, Index: 0}

	_, err := r.admitLeaveTargets(
		t.Context(), []wire.OutPoint{missing}, true,
	)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	admitted, err := r.admitLeaveTargets(
		t.Context(), []wire.OutPoint{missing}, false,
	)
	require.NoError(t, err)
	require.Empty(t, admitted)
}

// TestLeaveVTXOsDryRunRejectsCommitted asserts the refusal reaches the caller
// through the dry-run preview too. dry_run is the validity probe behind the
// CLI's consent prompt, so it must not echo an outpoint the real dispatch is
// guaranteed to refuse.
func TestLeaveVTXOsDryRunRejectsCommitted(t *testing.T) {
	t.Parallel()

	r, store := newLeaveAdmissionServer(t)
	desc := saveVTXOWithStatus(t, store, 0x51, vtxo.VTXOStatusForfeiting)

	_, err := r.LeaveVTXOs(t.Context(), &waverpc.LeaveVTXOsRequest{
		Selection: &waverpc.LeaveVTXOsRequest_Outpoints{
			Outpoints: &waverpc.OutpointSelection{
				Outpoints: []string{outpointStr(desc)},
			},
		},
		DefaultDestination: &waverpc.LeaveDestination{
			Target: &waverpc.LeaveDestination_PkScript{
				PkScript: desc.PkScript,
			},
		},
		DryRun: true,
	})
	require.Error(t, err)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
}
