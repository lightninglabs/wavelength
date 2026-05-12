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
				StateFilter: daemonrpc.RoundState_ROUND_STATE_FAILED,
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
