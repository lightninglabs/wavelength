//go:build !test_postgres

package waved

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/oor"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestGetOORSessionWait verifies durable terminal status wakes a pending read,
// terminal rows survive missed notifications, and every exit releases waiters.
func TestGetOORSessionWait(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{
		"commit",
		"already terminal",
		"timeout",
		"cancel",
		"missing",
	} {
		t.Run(mode, func(t *testing.T) {
			database := db.NewTestDB(t)
			server := &Server{db: database, log: btclog.Disabled}
			rpc := NewRPCServer(server)
			id := chainhash.Hash{5}
			row := sqlc.UpsertOORSessionRegistryParams{
				SessionID: id[:], ActorID: id.String(),
				Direction: 2, Status: 0, Phase: "ReceiveNotified",
				CreatedAt: 100, UpdatedAt: 100,
			}
			if mode == "already terminal" {
				row.Status = 1
				row.Phase = "Completed"
			}
			if mode != "missing" {
				require.NoError(
					t,
					database.UpsertOORSessionRegistry(
						t.Context(), row,
					),
				)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			wait := uint32(30_000)
			if mode == "timeout" {
				wait = 20
			}
			type result struct {
				response *waverpc.GetOORSessionResponse
				err      error
			}
			done := make(chan result, 1)
			start := time.Now()
			go func() {
				response, err := rpc.GetOORSession(
					ctx, &waverpc.GetOORSessionRequest{
						SessionId:         id.String(),
						WaitForTerminalMs: wait,
					},
				)
				done <- result{response, err}
			}()
			if mode == "commit" || mode == "cancel" {
				require.Eventually(t, func() bool {
					server.oorTerminalWaiters.mu.Lock()
					defer server.oorTerminalWaiters.mu.Unlock()

					return len(
						server.oorTerminalWaiters.waiters[oor.SessionID(
							id,
						)],
					) == 1
				}, time.Second, time.Millisecond)
				if mode == "cancel" {
					cancel()
				} else {
					row.Status = 1
					row.Phase = "Completed"
					require.NoError(
						t,
						database.UpsertOORSessionRegistry(
							t.Context(), row,
						),
					)
					server.oorTerminalWaiters.notify(
						oor.SessionID(id),
					)
				}
			}
			select {
			case got := <-done:
				switch mode {
				case "cancel":
					require.Equal(
						t, codes.Canceled,
						status.Code(got.err),
					)

				case "missing":
					require.Equal(
						t, codes.NotFound,
						status.Code(got.err),
					)

				default:
					require.NoError(t, got.err)
					require.True(
						t,
						got.response.TerminalWaitSupported,
					)
					if mode == "timeout" {
						require.GreaterOrEqual(
							t, time.Since(start),
							20*time.Millisecond,
						)
						require.False(
							t, oorStatusTerminal(
								got.response.Session,
							),
						)
					} else {
						require.True(
							t, oorStatusTerminal(
								got.response.Session,
							),
						)
					}
				}

			case <-time.After(3 * time.Second):
				t.Fatal("status read did not wake")
			}
			server.oorTerminalWaiters.mu.Lock()
			require.Empty(t, server.oorTerminalWaiters.waiters)
			server.oorTerminalWaiters.mu.Unlock()
		})
	}
}
