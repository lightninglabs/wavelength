package waved

import (
	"testing"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
)

// TestPageOORSessionsUsesSessionCursor asserts OOR pagination resumes after
// the last session id returned to the caller.
func TestPageOORSessionsUsesSessionCursor(t *testing.T) {
	t.Parallel()

	sessions := []*waverpc.OORSessionInfo{
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

	outgoing := waverpc.
		OORSessionDirection_OOR_SESSION_DIRECTION_OUTGOING
	incoming := waverpc.
		OORSessionDirection_OOR_SESSION_DIRECTION_INCOMING
	pending := waverpc.OORSessionStatus_OOR_SESSION_STATUS_PENDING

	info := &waverpc.OORSessionInfo{
		Direction: outgoing,
		Status:    pending,
	}

	require.True(
		t,
		oorSessionMatchesFilters(
			info, &waverpc.ListOORSessionsRequest{},
		),
	)

	require.True(
		t,
		oorSessionMatchesFilters(
			info, &waverpc.ListOORSessionsRequest{
				DirectionFilter: outgoing,
				StatusFilter:    pending,
			},
		),
	)

	require.False(
		t,
		oorSessionMatchesFilters(
			info, &waverpc.ListOORSessionsRequest{
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

	outgoing := waverpc.
		OORSessionDirection_OOR_SESSION_DIRECTION_OUTGOING
	incoming := waverpc.
		OORSessionDirection_OOR_SESSION_DIRECTION_INCOMING
	pending := waverpc.OORSessionStatus_OOR_SESSION_STATUS_PENDING
	completed := waverpc.OORSessionStatus_OOR_SESSION_STATUS_COMPLETED

	live := []*waverpc.OORSessionInfo{
		{
			SessionId: "session-a",
			Direction: incoming,
			Status:    pending,
		},
	}
	persisted := []*waverpc.OORSessionInfo{
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
		live, persisted, &waverpc.ListOORSessionsRequest{},
	)
	require.Len(t, all, 1)
	require.Equal(t, outgoing, all[0].GetDirection())
	require.Equal(t, completed, all[0].GetStatus())
	require.Equal(t, "completed", all[0].GetPhase())
	require.Equal(t, []string{"input:0"}, all[0].GetConsumedOutpoints())

	outgoingOnly := mergeOORSessionLists(
		live, persisted, &waverpc.ListOORSessionsRequest{
			DirectionFilter: outgoing,
		},
	)
	require.Len(t, outgoingOnly, 1)

	incomingOnly := mergeOORSessionLists(
		live, persisted, &waverpc.ListOORSessionsRequest{
			DirectionFilter: incoming,
		},
	)
	require.Empty(t, incomingOnly)

	pendingOnly := mergeOORSessionLists(
		live, persisted, &waverpc.ListOORSessionsRequest{
			StatusFilter: pending,
		},
	)
	require.Empty(t, pendingOnly)
}

// TestPersistedOORPackageQueryPlanning verifies filtered OOR list requests
// only perform package-store scans when completed artifacts can be returned.
func TestPersistedOORPackageQueryPlanning(t *testing.T) {
	t.Parallel()

	pending := waverpc.OORSessionStatus_OOR_SESSION_STATUS_PENDING
	failed := waverpc.OORSessionStatus_OOR_SESSION_STATUS_FAILED
	completed := waverpc.OORSessionStatus_OOR_SESSION_STATUS_COMPLETED
	incoming := waverpc.
		OORSessionDirection_OOR_SESSION_DIRECTION_INCOMING

	tests := []struct {
		name        string
		req         *waverpc.ListOORSessionsRequest
		wantList    bool
		wantOverlay bool
	}{
		{
			name:     "unfiltered",
			req:      &waverpc.ListOORSessionsRequest{},
			wantList: true,
		},
		{
			name: "completed",
			req: &waverpc.ListOORSessionsRequest{
				StatusFilter: completed,
			},
			wantList: true,
		},
		{
			name: "pending",
			req: &waverpc.ListOORSessionsRequest{
				StatusFilter: pending,
			},
			wantOverlay: true,
		},
		{
			name: "failed",
			req: &waverpc.ListOORSessionsRequest{
				StatusFilter: failed,
			},
			wantOverlay: true,
		},
		{
			name: "direction",
			req: &waverpc.ListOORSessionsRequest{
				DirectionFilter: incoming,
			},
			wantList:    true,
			wantOverlay: true,
		},
		{
			name: "completed direction",
			req: &waverpc.ListOORSessionsRequest{
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

	info := &waverpc.RoundInfo{
		State:        waverpc.RoundState_ROUND_STATE_CONFIRMED,
		CreationTime: 100,
	}

	require.True(
		t,
		roundInfoMatchesFilters(
			info, &waverpc.ListRoundsRequest{},
		),
	)

	require.True(
		t,
		roundInfoMatchesFilters(
			info, &waverpc.ListRoundsRequest{
				StateFilter: waverpc.
					RoundState_ROUND_STATE_CONFIRMED,
				CreatedAfter: 50,
			},
		),
	)

	require.False(
		t,
		roundInfoMatchesFilters(
			info, &waverpc.ListRoundsRequest{
				StateFilter: waverpc.
					RoundState_ROUND_STATE_FAILED,
			},
		),
	)

	require.False(
		t,
		roundInfoMatchesFilters(
			info, &waverpc.ListRoundsRequest{
				CreatedBefore: 50,
			},
		),
	)
}
