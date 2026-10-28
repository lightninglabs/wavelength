//go:build !test_postgres

package waved

import (
	"database/sql"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestDispatchIdempotencyKey reads immutable dispatch evidence independently of
// the mutable session registry, including the display-to-storage byte order.
func TestDispatchIdempotencyKey(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	database := db.NewTestDB(t)
	rpc := NewRPCServer(&Server{db: database, log: btclog.Disabled})
	id := chainhash.Hash{1, 2, 3, 4}
	key := "in-swap-funding:" + strings.Repeat("ab", 32)
	require.NoError(
		t,
		database.InsertOORDispatchAttempt(
			ctx, sqlc.InsertOORDispatchAttemptParams{
				IdempotencyKey: key,
				SessionID:      id[:],
				CreatedAt:      100,
			},
		),
	)

	got, err := rpc.DispatchIdempotencyKey(ctx, id.String())
	require.NoError(t, err)
	require.Equal(t, key, got)
	got, err = rpc.DispatchIdempotencyKey(ctx, chainhash.Hash{9}.String())
	require.NoError(t, err)
	require.Empty(t, got)

	for _, invalid := range []string{"", "not-hex"} {
		_, err := rpc.DispatchIdempotencyKey(ctx, invalid)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	}

	withoutDB := NewRPCServer(&Server{log: btclog.Disabled})
	got, err = withoutDB.DispatchIdempotencyKey(ctx, id.String())
	require.NoError(t, err)
	require.Empty(t, got)
}

// TestOORStatusRPCPagination verifies default and explicit pages, terminal
// lookahead, filters after artifact override, and direct normalized lookup.
// Registry status is durable, so it remains visible before actor startup.
func TestOORStatusRPCPagination(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	database := db.NewTestDB(t)
	rpc := NewRPCServer(&Server{db: database, log: btclog.Disabled})
	for i := 1; i <= 103; i++ {
		id, err := chainhash.NewHashFromStr(fmt.Sprintf("%064x", i))
		require.NoError(t, err)
		require.NoError(
			t,
			database.UpsertOORSessionRegistry(
				ctx, sqlc.UpsertOORSessionRegistryParams{
					SessionID: id[:],
					ActorID:   id.String(),
					Direction: 2,
					Status:    0,
					Phase:     "ReceiveNotified",
					CreatedAt: int64(100 + i/5),
					UpdatedAt: int64(150 + i/5),
					LastError: sql.NullString{
						String: "retry",
						Valid:  true,
					},
				},
			),
		)
		if i%2 == 0 {
			_, err = database.UpsertOORPackage(
				ctx, sqlc.UpsertOORPackageParams{
					SessionID: id[:],
					Direction: 1,
					ArkPsbt: []byte(
						"unused PSBT",
					),
					CreatedAt: int64(1000 + i/5),
					UpdatedAt: 200,
				},
			)
			require.NoError(t, err)
		}
	}
	first, err := rpc.ListOORSessions(ctx, nil)
	require.NoError(t, err)
	require.Len(t, first.Sessions, 100)
	require.NotEmpty(t, first.NextPageToken)
	require.Equal(t, fmt.Sprintf("%064x", 103), first.Sessions[0].SessionId)
	require.Equal(t, fmt.Sprintf("%064x", 4), first.Sessions[99].SessionId)
	last, err := rpc.ListOORSessions(ctx, &waverpc.ListOORSessionsRequest{
		PageToken: first.NextPageToken,
	})
	require.NoError(t, err)
	require.Len(t, last.Sessions, 3)
	require.Empty(t, last.NextPageToken)
	for _, filter := range []struct {
		direction waverpc.OORSessionDirection
		state     waverpc.OORSessionStatus
		want      int
	}{
		{
			0,
			0,
			103,
		}, {
			1,
			0,
			51,
		}, {
			2,
			0,
			52,
		}, {
			0,
			1,
			52,
		}, {
			0,
			2,
			51,
		}, {
			0,
			3,
			0,
		}, {
			2,
			2,
			0,
		}, {
			1,
			1,
			0,
		}, {
			99,
			0,
			0,
		}, {
			0,
			99,
			0,
		},
	} {
		var ids []string
		cursor := ""
		for {
			response, err := rpc.ListOORSessions(
				ctx, &waverpc.ListOORSessionsRequest{
					PageSize:        7,
					PageToken:       cursor,
					DirectionFilter: filter.direction,
					StatusFilter:    filter.state,
				},
			)
			require.NoError(t, err)
			for _, session := range response.Sessions {
				ids = append(ids, session.SessionId)
				id, err := chainhash.NewHashFromStr(
					session.SessionId,
				)
				require.NoError(t, err)
				require.EqualValues(
					t, 100+int(id[0])/5, session.CreatedAt,
				)
				// The point endpoint must return the exact same
				// projection.
				got, err := rpc.GetOORSession(
					ctx, &waverpc.GetOORSessionRequest{
						SessionId: strings.ToUpper(
							session.SessionId,
						),
					},
				)
				require.NoError(t, err)
				require.Equal(t, session, got.Session)
				if session.Direction == 1 {
					require.Equal(
						t,
						waverpc.OORSessionStatus_OOR_SESSION_STATUS_COMPLETED,
						session.Status,
					)
					require.Equal(
						t, "completed", session.Phase,
					)
					require.Empty(t, session.FailureReason)

				} else {
					require.Equal(
						t, "retry",
						session.FailureReason,
					)

				}
			}
			if response.NextPageToken == "" {
				break
			}
			require.NotEqual(t, cursor, response.NextPageToken)
			cursor = response.NextPageToken
		}
		require.Len(t, ids, filter.want, "filter %+v", filter)
		require.IsDecreasing(t, ids)
	}
	// Old ID-only cursors cannot safely continue in the new ordering.
	for _, token := range []string{
		fmt.Sprintf("%064x", 50), "not-base64!",
		base64.RawURLEncoding.EncodeToString(
			[]byte("v2:100:" +
				fmt.Sprintf("%064x", 50)),
		),
		base64.RawURLEncoding.EncodeToString(
			[]byte("v1:no-time:" +
				fmt.Sprintf("%064x", 50)),
		),
		base64.RawURLEncoding.EncodeToString(
			[]byte("v1:100:" +
				strings.Repeat("z", 64)),
		),
	} {
		_, err := rpc.ListOORSessions(
			ctx, &waverpc.ListOORSessionsRequest{
				PageToken: token,
			},
		)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
		require.Contains(t, err.Error(), "restart pagination")
	}
	for _, request := range []*waverpc.GetOORSessionRequest{
		nil, {}, {
			SessionId: "not-hex",
		},
	} {
		_, err := rpc.GetOORSession(ctx, request)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	}
	_, err = rpc.GetOORSession(
		ctx, &waverpc.GetOORSessionRequest{
			SessionId: "ffff",
		},
	)
	require.Equal(t, codes.NotFound, status.Code(err))
	// The final full page must not advertise an empty next page.
	exact, err := rpc.ListOORSessions(
		ctx, &waverpc.ListOORSessionsRequest{
			PageSize: 103,
		},
	)
	require.NoError(t, err)
	require.Len(t, exact.Sessions, 103)
	require.Empty(t, exact.NextPageToken)
	// A newer insertion and completion of an already returned session do
	// not shift the next page. Completion uses the earlier registry time.
	before, err := rpc.ListOORSessions(ctx, &waverpc.ListOORSessionsRequest{
		PageSize: 2,
	})
	require.NoError(t, err)
	completedID, err := chainhash.NewHashFromStr(fmt.Sprintf("%064x", 103))
	require.NoError(t, err)
	_, err = database.UpsertOORPackage(ctx, sqlc.UpsertOORPackageParams{
		SessionID: completedID[:], Direction: 1,
		ArkPsbt: []byte("unused"), CreatedAt: 2000, UpdatedAt: 2000,
	})
	require.NoError(t, err)
	newID := chainhash.Hash{104}
	require.NoError(
		t,
		database.UpsertOORSessionRegistry(
			ctx, sqlc.UpsertOORSessionRegistryParams{
				SessionID: newID[:],
				ActorID:   newID.String(),
				Direction: 2,
				CreatedAt: 3000,
			},
		),
	)
	after, err := rpc.ListOORSessions(ctx, &waverpc.ListOORSessionsRequest{
		PageSize: 103, PageToken: before.NextPageToken,
	})
	require.NoError(t, err)
	require.Len(t, after.Sessions, 101)
	for i, session := range after.Sessions {
		require.Equal(t, fmt.Sprintf("%064x", 101-i), session.SessionId)
	}
	require.Empty(t, after.NextPageToken)

}
