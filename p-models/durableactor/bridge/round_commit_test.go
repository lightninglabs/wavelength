package bridge

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// TestRoundCommitBoundary replays the atomic commit contract against the real
// delivery store, reopening SQLite after each injected rollback. It verifies
// the runtime storage primitive; it does not exercise either round behavior.
func TestRoundCommitBoundary(t *testing.T) {
	for failure := 0; failure <= 3; failure++ {
		t.Run(fmt.Sprintf("failure%d", failure), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "round.db")
			clk := clock.NewTestClock(traceTime(0))
			store, closeStore := openStoreAt(t, path, clk, true)
			require.NoError(
				t,
				store.EnqueueMessage(
					t.Context(), actor.EnqueueParams{
						ID:          "delivery",
						MailboxID:   "round",
						MessageType: "model.RoundTurn",
						Payload:     []byte{1},
						AvailableAt: clk.Now(),
						MaxAttempts: 3,
					},
				),
			)
			effect := actor.OutboxParams{
				ID: "effect", SourceActorID: "round",
				TargetActorID: "connection",
				MessageType:   "model.RoundResponse",
				Payload: []byte{
					2,
				},
			}
			errInjected := errors.New("round commit interrupted")
			err := store.ExecTx(t.Context(), false,
				func(ctx context.Context,
					tx actor.DeliveryStore) error {

					err := tx.SaveCheckpoint(
						ctx, actor.CheckpointParams{
							ActorID:   "round",
							StateType: "accepted",
							StateData: []byte{1},
							Version:   1,
						},
					)
					if err != nil {
						return err
					}
					if failure == 1 {
						return errInjected
					}
					err = tx.EnqueueOutbox(ctx, effect)
					if err != nil {
						return err
					}
					if failure == 2 {
						return errInjected
					}
					rows, err := tx.AckMessageByID(
						ctx, "delivery",
					)
					if err != nil {
						return err
					}
					if rows != 1 {
						return actor.ErrLeaseLost
					}
					if failure == 3 {
						return errInjected
					}

					return nil
				},
			)
			if failure == 0 {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, errInjected)
			}
			closeStore()

			store, closeStore = openStoreAt(t, path, clk, false)
			defer closeStore()
			checkpoint, err := store.LoadCheckpoint(
				t.Context(),
				"round",
			)
			require.NoError(t, err)
			pending, err := store.PeekNextMessage(
				t.Context(),
				"round",
			)
			require.NoError(t, err)
			outbox := claimOutbox(t, store, "restart")
			if failure != 0 {
				require.Nil(t, checkpoint)
				require.NotNil(t, pending)
				require.Equal(t, "delivery", pending.ID)
				require.Empty(t, outbox)

				return
			}
			require.NotNil(t, checkpoint)
			require.Equal(t, []byte{1}, checkpoint.StateData)
			require.Nil(t, pending)
			require.Len(t, outbox, 1)
			require.Equal(t, "effect", outbox[0].ID)
			require.Equal(t, []byte{2}, outbox[0].Payload)
		})
	}
}
