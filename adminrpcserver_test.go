package darepo

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/lightninglabs/darepo/adminrpc"
	storedb "github.com/lightninglabs/darepo/db"
	"github.com/lightninglabs/darepo/db/sqlc"
	"github.com/stretchr/testify/require"
)

// TestRoundToSummaryIncludesPersistedStats verifies round summaries expose
// participant and value totals derived from persisted admin data.
func TestRoundToSummaryIncludesPersistedStats(t *testing.T) {
	t.Parallel()

	sqlStore := storedb.NewTestDB(t)
	q := sqlStore.Queries
	ctx := t.Context()

	roundID := uuid.New()
	roundIDBytes, err := roundID.MarshalBinary()
	require.NoError(t, err)

	err = q.InsertRound(ctx, sqlc.InsertRoundParams{
		RoundID:        roundIDBytes,
		FinalTx:        []byte{0x01},
		CommitmentTxid: "commitment-txid",
		Status:         "confirmed",
		SweepKey:       []byte{0x02},
		CsvDelay:       144,
		CreatedAt:      123,
		UpdatedAt:      456,
	})
	require.NoError(t, err)

	for _, clientID := range [][]byte{[]byte("alice"), []byte("bob")} {
		err := q.InsertRoundClientRegistration(
			ctx, sqlc.InsertRoundClientRegistrationParams{
				RoundID:          roundIDBytes,
				ClientID:         clientID,
				RegistrationData: []byte{0x03},
			},
		)
		require.NoError(t, err)
	}

	for i, amount := range []int64{1000, 2000} {
		outpointHash := make([]byte, 32)
		outpointHash[0] = byte(i)

		err := q.InsertVTXO(ctx, sqlc.InsertVTXOParams{
			OutpointHash:  outpointHash,
			OutpointIndex: int32(i),
			RoundID:       roundIDBytes,
			BatchOutputIndex: sql.NullInt32{
				Int32: int32(i),
				Valid: true,
			},
			Amount:         amount,
			PkScript:       []byte{0x51, byte(i)},
			PolicyTemplate: []byte{0x52},
			CosignerKey:    []byte{0x02, byte(i)},
			Status:         "live",
		})
		require.NoError(t, err)
	}

	row, err := q.GetRound(ctx, roundIDBytes)
	require.NoError(t, err)

	adminServer := &AdminRPCServer{}
	statsByID, err := adminServer.roundStatsByID(ctx, q, []sqlc.Round{
		row,
	})
	require.NoError(t, err)

	summary, err := adminServer.roundToSummary(
		row, statsByID[string(row.RoundID)],
	)
	require.NoError(t, err)

	require.Equal(t, roundID.String(), summary.Id)
	require.Equal(t, adminrpc.RoundStatus_ROUND_STATUS_CONFIRMED,
		summary.Status)
	require.Equal(t, "commitment-txid", summary.TxId)
	require.Equal(t, uint32(2), summary.NumParticipants)
	require.Equal(t, int64(3000), summary.TotalValueSat)
	require.Equal(t, int64(123), summary.CreatedAtUnixS)
}

// TestMapDBRoundStatusPendingMatchesBroadcast ensures the admin API exposes the
// persisted pre-confirmation round state as broadcast.
func TestMapDBRoundStatusPendingMatchesBroadcast(t *testing.T) {
	t.Parallel()

	status := mapDBRoundStatus("pending")
	require.Equal(
		t, adminrpc.RoundStatus_ROUND_STATUS_BROADCAST, status,
	)
}

// TestMapRoundStatusToDBStrBroadcastMatchesPending ensures broadcast round
// filters resolve to the persisted pre-confirmation database state.
func TestMapRoundStatusToDBStrBroadcastMatchesPending(t *testing.T) {
	t.Parallel()

	status, err := mapRoundStatusToDBStr(
		adminrpc.RoundStatus_ROUND_STATUS_BROADCAST,
	)
	require.NoError(t, err)
	require.Equal(t, "pending", status)
}

// TestMapRoundStatusToDBStrOpenRejectsNonPersistedState ensures callers do not
// accidentally treat the persisted post-finalization state as open.
func TestMapRoundStatusToDBStrOpenRejectsNonPersistedState(t *testing.T) {
	t.Parallel()

	_, err := mapRoundStatusToDBStr(
		adminrpc.RoundStatus_ROUND_STATUS_OPEN,
	)
	require.Error(t, err)
	require.ErrorContains(t, err, "not persisted")
}
