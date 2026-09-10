package actordelivery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/db"
	mailboxconn "github.com/lightninglabs/wavelength/mailbox/conn"
	"github.com/stretchr/testify/require"
)

// quarantineEvidence names the store capability exercised by these tests.
type quarantineEvidence = mailboxconn.IngressQuarantineStore

// TestIngressQuarantineAtomicAndBounded exercises count/byte caps, immutable
// evidence, lane isolation and rollback using the selected SQL backend.
func TestIngressQuarantineAtomicAndBounded(t *testing.T) {
	for _, mode := range []string{"count", "bytes"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			testDB := db.NewTestDB(t)
			store, err := NewTxAwareDeliveryStoreFromDB(
				testDB.DB, testDB.Backend(), nil, nil,
			)
			require.NoError(t, err)
			evidence, ok := store.(quarantineEvidence)
			require.True(t, ok)
			entry := mailboxconn.IngressQuarantine{
				ID:       "original",
				Lane:     "lane-a",
				Envelope: []byte("raw signed message"),
				Reason:   "cannot adapt",
			}
			err = store.ExecTx(
				ctx, false,
				func(ctx context.Context,
					tx actor.DeliveryStore) error {

					evidence, ok := tx.(quarantineEvidence)
					require.True(t, ok)
					require.NoError(
						t, evidence.QuarantineIngress(
							ctx, entry,
						),
					)

					return errors.New("crash before " +
						"cursor commit")
				},
			)
			require.Error(t, err)
			rows, err := evidence.ListIngressQuarantine(
				ctx, entry.Lane,
			)
			require.NoError(t, err)
			require.Empty(t, rows)
			if mode == "count" {
				require.NoError(
					t,
					evidence.QuarantineIngress(ctx, entry),
				)
				changed := entry
				changed.Envelope = []byte(
					"replacement must not overwrite " +
						"original",
				)
				require.NoError(
					t, evidence.QuarantineIngress(
						ctx, changed,
					),
				)
				rows, err = evidence.ListIngressQuarantine(
					ctx, entry.Lane,
				)
				require.NoError(t, err)
				require.Len(t, rows, 1)
				require.Equal(
					t, entry.Envelope, rows[0].Envelope,
				)
				for i := 0; i < 128; i++ {
					item := entry
					item.ID = fmt.Sprintf("lane-b-%d", i)
					item.Lane = "lane-b"
					require.NoError(
						t, evidence.QuarantineIngress(
							ctx, item,
						),
					)
				}
				rows, err = evidence.ListIngressQuarantine(
					ctx, "lane-b",
				)
				require.NoError(t, err)
				require.Len(t, rows, 128)
				overflow := entry
				overflow.ID = "lane-b-overflow"
				overflow.Lane = "lane-b"
				require.ErrorContains(
					t, evidence.QuarantineIngress(
						ctx, overflow,
					),
					"capacity reached",
				)
				// A full peer lane cannot consume the
				// reservation available to another connection.
				entry.ID = "lane-a-second"
				require.NoError(
					t,
					evidence.QuarantineIngress(ctx, entry),
				)
				for i := 0; i < 126; i++ {
					item := entry
					item.ID = fmt.Sprintf("lane-c-%d", i)
					item.Lane = "lane-c"
					require.NoError(
						t, evidence.QuarantineIngress(
							ctx, item,
						),
					)
				}
				entry.ID = "global-overflow"
				entry.Lane = "lane-d"
			} else {
				entry.Envelope = bytes.Repeat(
					[]byte{1}, 8*1024*1024,
				)
				require.NoError(
					t,
					evidence.QuarantineIngress(ctx, entry),
				)
				peerOverflow := entry
				peerOverflow.ID = "lane-a-overflow"
				peerOverflow.Envelope = []byte{1}
				require.ErrorContains(
					t, evidence.QuarantineIngress(
						ctx, peerOverflow,
					),
					"capacity reached",
				)
				entry.ID = "lane-b-capacity"
				entry.Lane = "lane-b"
				require.NoError(
					t,
					evidence.QuarantineIngress(ctx, entry),
				)
				entry.ID = "global-overflow"
				entry.Lane = "lane-c"
			}
			entry.Envelope = []byte{1}
			require.ErrorContains(
				t, evidence.QuarantineIngress(ctx, entry),
				"capacity reached",
			)
			// Filling quarantine never prevents a normal consumer
			// inbox insert.
			require.NoError(
				t,
				store.EnqueueMessage(
					ctx, actor.EnqueueParams{
						ID:          "healthy",
						MailboxID:   "healthy",
						MessageType: "test",
						Payload:     []byte{1},
						MaxAttempts: 5,
					},
				),
			)
		})
	}
}
