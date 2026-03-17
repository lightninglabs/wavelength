package darepo

import (
	"testing"

	"github.com/lightninglabs/darepo/adminrpc"
	"github.com/stretchr/testify/require"
)

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
