package darepod

import (
	"testing"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/stretchr/testify/require"
)

// TestPageOORSessionsUsesSessionCursor asserts OOR pagination resumes after
// the last session id returned to the caller.
func TestPageOORSessionsUsesSessionCursor(t *testing.T) {
	t.Parallel()

	sessions := []*daemonrpc.OORSessionInfo{
		{
			SessionId: "a",
		},
		{
			SessionId: "b",
		},
		{
			SessionId: "c",
		},
	}

	page, nextToken := pageOORSessions(sessions, "", 2)
	require.Len(t, page, 2)
	require.Equal(t, "a", page[0].GetSessionId())
	require.Equal(t, "b", page[1].GetSessionId())
	require.Equal(t, "b", nextToken)

	page, nextToken = pageOORSessions(sessions, nextToken, 2)
	require.Len(t, page, 1)
	require.Equal(t, "c", page[0].GetSessionId())
	require.Empty(t, nextToken)
}

// TestOORSessionMatchesFilters checks direction and status filtering.
func TestOORSessionMatchesFilters(t *testing.T) {
	t.Parallel()

	outgoing := daemonrpc.
		OORSessionDirection_OOR_SESSION_DIRECTION_OUTGOING
	incoming := daemonrpc.
		OORSessionDirection_OOR_SESSION_DIRECTION_INCOMING
	pending := daemonrpc.OORSessionStatus_OOR_SESSION_STATUS_PENDING

	info := &daemonrpc.OORSessionInfo{
		Direction: outgoing,
		Status:    pending,
	}

	require.True(
		t,
		oorSessionMatchesFilters(
			info, &daemonrpc.ListOORSessionsRequest{},
		),
	)

	require.True(
		t,
		oorSessionMatchesFilters(
			info, &daemonrpc.ListOORSessionsRequest{
				DirectionFilter: outgoing,
				StatusFilter:    pending,
			},
		),
	)

	require.False(
		t,
		oorSessionMatchesFilters(
			info, &daemonrpc.ListOORSessionsRequest{
				DirectionFilter: incoming,
			},
		),
	)
}

// TestMergeOORSessionListsPrefersPersistedDirection verifies that completed
// package artifacts are the user-facing authority when a sender later observes
// its own OOR change output as an incoming live actor session.
func TestMergeOORSessionListsPrefersPersistedDirection(t *testing.T) {
	t.Parallel()

	outgoing := daemonrpc.
		OORSessionDirection_OOR_SESSION_DIRECTION_OUTGOING
	incoming := daemonrpc.
		OORSessionDirection_OOR_SESSION_DIRECTION_INCOMING
	pending := daemonrpc.OORSessionStatus_OOR_SESSION_STATUS_PENDING
	completed := daemonrpc.OORSessionStatus_OOR_SESSION_STATUS_COMPLETED

	live := []*daemonrpc.OORSessionInfo{
		{
			SessionId: "session-a",
			Direction: incoming,
			Status:    pending,
		},
	}
	persisted := []*daemonrpc.OORSessionInfo{
		{
			SessionId: "session-a",
			Direction: outgoing,
			Status:    completed,
			Phase:     "completed",
			ConsumedOutpoints: []string{
				"input:0",
			},
		},
	}

	all := mergeOORSessionLists(
		live, persisted, &daemonrpc.ListOORSessionsRequest{},
	)
	require.Len(t, all, 1)
	require.Equal(t, outgoing, all[0].GetDirection())
	require.Equal(t, completed, all[0].GetStatus())
	require.Equal(t, "completed", all[0].GetPhase())
	require.Equal(t, []string{"input:0"}, all[0].GetConsumedOutpoints())

	outgoingOnly := mergeOORSessionLists(
		live, persisted, &daemonrpc.ListOORSessionsRequest{
			DirectionFilter: outgoing,
		},
	)
	require.Len(t, outgoingOnly, 1)

	incomingOnly := mergeOORSessionLists(
		live, persisted, &daemonrpc.ListOORSessionsRequest{
			DirectionFilter: incoming,
		},
	)
	require.Empty(t, incomingOnly)

	pendingOnly := mergeOORSessionLists(
		live, persisted, &daemonrpc.ListOORSessionsRequest{
			StatusFilter: pending,
		},
	)
	require.Empty(t, pendingOnly)
}

// TestPersistedOORPackageQueryPlanning verifies filtered OOR list requests
// only perform package-store scans when completed artifacts can be returned.
func TestPersistedOORPackageQueryPlanning(t *testing.T) {
	t.Parallel()

	pending := daemonrpc.OORSessionStatus_OOR_SESSION_STATUS_PENDING
	failed := daemonrpc.OORSessionStatus_OOR_SESSION_STATUS_FAILED
	completed := daemonrpc.OORSessionStatus_OOR_SESSION_STATUS_COMPLETED
	incoming := daemonrpc.
		OORSessionDirection_OOR_SESSION_DIRECTION_INCOMING

	tests := []struct {
		name        string
		req         *daemonrpc.ListOORSessionsRequest
		wantList    bool
		wantOverlay bool
	}{
		{
			name:     "unfiltered",
			req:      &daemonrpc.ListOORSessionsRequest{},
			wantList: true,
		},
		{
			name: "completed",
			req: &daemonrpc.ListOORSessionsRequest{
				StatusFilter: completed,
			},
			wantList: true,
		},
		{
			name: "pending",
			req: &daemonrpc.ListOORSessionsRequest{
				StatusFilter: pending,
			},
			wantOverlay: true,
		},
		{
			name: "failed",
			req: &daemonrpc.ListOORSessionsRequest{
				StatusFilter: failed,
			},
			wantOverlay: true,
		},
		{
			name: "direction",
			req: &daemonrpc.ListOORSessionsRequest{
				DirectionFilter: incoming,
			},
			wantList:    true,
			wantOverlay: true,
		},
		{
			name: "completed direction",
			req: &daemonrpc.ListOORSessionsRequest{
				DirectionFilter: incoming,
				StatusFilter:    completed,
			},
			wantList:    true,
			wantOverlay: true,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(
				t, test.wantList,
				shouldListPersistedOORPackages(test.req),
			)
			require.Equal(
				t, test.wantOverlay,
				shouldQueryPersistedOORLiveOverlay(test.req),
			)
		})
	}
}

// TestParseOORSessionIDNormalizesCase verifies uppercase session ids are
// accepted and normalized to the canonical chainhash string form.
func TestParseOORSessionIDNormalizesCase(t *testing.T) {
	t.Parallel()

	const sessionID = "ABCDEF0123456789ABCDEF0123456789" +
		"ABCDEF0123456789ABCDEF0123456789"
	const normalizedID = "abcdef0123456789abcdef0123456789" +
		"abcdef0123456789abcdef0123456789"

	hash, err := parseOORSessionID(sessionID)
	require.NoError(t, err)
	require.Equal(t, normalizedID, hash.String())
}

// TestRoundInfoMatchesFilters checks state and creation-time filtering.
func TestRoundInfoMatchesFilters(t *testing.T) {
	t.Parallel()

	info := &daemonrpc.RoundInfo{
		State:        daemonrpc.RoundState_ROUND_STATE_CONFIRMED,
		CreationTime: 100,
	}

	require.True(
		t,
		roundInfoMatchesFilters(
			info, &daemonrpc.ListRoundsRequest{},
		),
	)

	require.True(
		t,
		roundInfoMatchesFilters(
			info, &daemonrpc.ListRoundsRequest{
				StateFilter: daemonrpc.
					RoundState_ROUND_STATE_CONFIRMED,
				CreatedAfter: 50,
			},
		),
	)

	require.False(
		t,
		roundInfoMatchesFilters(
			info, &daemonrpc.ListRoundsRequest{
				StateFilter: daemonrpc.
					RoundState_ROUND_STATE_FAILED,
			},
		),
	)

	require.False(
		t,
		roundInfoMatchesFilters(
			info, &daemonrpc.ListRoundsRequest{
				CreatedBefore: 50,
			},
		),
	)
}
