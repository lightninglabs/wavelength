//go:build systest

package systest

import (
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/db/sqlc"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	"github.com/lightninglabs/wavelength/waved"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
)

// queueAdmissionRefresh reserves the fixture's live VTXO through the daemon
// RPC. Repeating it after timeout proves the reservation is spendable again.
func queueAdmissionRefresh(t *testing.T, f *directedSendFixture) {
	t.Helper()

	outpoint := outpointString(f.seededOutpoint)
	resp, err := f.client.RefreshVTXOs(
		t.Context(), &waverpc.RefreshVTXOsRequest{
			Selection: &waverpc.RefreshVTXOsRequest_Outpoints{
				Outpoints: &waverpc.OutpointSelection{
					Outpoints: []string{
						outpoint,
					},
				},
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, "queued", resp.Status)
	_, err = f.client.JoinNextRound(
		t.Context(), &waverpc.JoinNextRoundRequest{},
	)
	require.NoError(t, err)
}

// admitRefresh pushes the real admission watermark through the mailbox after
// observing registration. The fake operator then sends no further event.
func admitRefresh(t *testing.T, f *directedSendFixture) uuid.UUID {
	t.Helper()

	require.Eventually(t, func() bool {
		return len(f.mailboxServer.joinRoundRequests()) > 0
	}, 15*time.Second, 50*time.Millisecond)

	f.mailboxServer.mu.Lock()
	joinEnv := f.mailboxServer.joinRoundEnvs[0]
	f.mailboxServer.mu.Unlock()
	id := uuid.New()
	body, err := anypb.New(&roundpb.ClientSuccessResp{
		RoundId: id[:],
		AcceptedVtxoOutpoints: []*roundpb.Outpoint{
			{
				TxHash:      f.seededOutpoint.Hash[:],
				OutputIndex: f.seededOutpoint.Index,
			},
		},
	})
	require.NoError(t, err)
	f.mailboxServer.enqueueEnvelope(&mailboxpb.Envelope{
		ProtocolVersion:    joinEnv.ProtocolVersion,
		ArkProtocolVersion: joinEnv.ArkProtocolVersion,
		Sender:             f.mailboxServer.operatorMailbox,
		Recipient:          joinEnv.Sender,
		CreatedAtUnixMs:    time.Now().UnixMilli(),
		Body:               body,
		Rpc: &mailboxpb.RpcMeta{
			Kind:    mailboxpb.RpcMeta_KIND_EVENT,
			Service: roundpb.ServiceName,
			Method:  roundpb.MethodJoinAck,
			ReplyTo: f.mailboxServer.operatorMailbox,
		},
	})

	require.Eventually(t, func() bool {
		for _, r := range listRounds(t, f.client) {
			if r.RoundId == id.String() && !r.IsTemp {
				return true
			}
		}

		return false
	}, 15*time.Second, 50*time.Millisecond)

	return id
}

// readAdmissionDeadline inspects the daemon's real SQLite record using the
// generated read query, without modifying the deadline under test.
func readAdmissionDeadline(t *testing.T, f *directedSendFixture,
	id uuid.UUID) sqlc.GetRoundAdmissionDeadlineRow {

	t.Helper()

	cfg := db.DefaultSqliteConfig(f.cfg.NetworkDir())
	cfg.SkipMigrations = true
	store, err := db.NewSqliteStore(cfg, btclog.Disabled)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, store.Close())
	}()
	row, err := store.Queries.GetRoundAdmissionDeadline(
		t.Context(), id.String(),
	)
	require.NoError(t, err)

	return row
}

// TestAcceptedRoundSilenceReleasesReservation proves operator silence after
// acceptance returns a real daemon reservation to live, persists closure, and
// permits the same input to be reserved by a fresh attempt.
func TestAcceptedRoundSilenceReleasesReservation(t *testing.T) {
	ParallelN(t)

	f := newDirectedSendFixture(t, func(c *waved.Config) {
		c.EagerRoundJoin = true
		c.AdmissionTimeout = 4 * time.Second
	})
	queueAdmissionRefresh(t, f)
	id := admitRefresh(t, f)
	initial := readAdmissionDeadline(t, f, id)
	require.False(t, initial.Closed)
	requireVTXOStatusEventually(
		t, f.client, f.seededOutpoint,
		waverpc.VTXOStatus_VTXO_STATUS_LIVE, 15*time.Second,
	)
	closed := readAdmissionDeadline(t, f, id)
	require.True(t, closed.Closed)
	require.Equal(t, initial.ExpiresAt, closed.ExpiresAt)
	require.Equal(t, testSeededAmountSat, vtxoBalanceSat(t, f.client))
	queueAdmissionRefresh(t, f)
}

// TestAcceptedRoundRestartBeforeExpiry proves startup safely abandons a lost
// signing attempt even while its original authorization budget is still valid.
func TestAcceptedRoundRestartBeforeExpiry(t *testing.T) {
	ParallelN(t)
	runAcceptedRoundRestart(t, false)
}

// TestAcceptedRoundRestartAfterExpiry proves expiry while offline cannot renew
// the attempt on restart or leave its reservation stranded.
func TestAcceptedRoundRestartAfterExpiry(t *testing.T) {
	ParallelN(t)
	runAcceptedRoundRestart(t, true)
}

// runAcceptedRoundRestart checks the real database fence and input reuse on
// both sides of expiry. Startup uses fresh signing state and the old deadline
// is retained solely as a closed-attempt fence.
func runAcceptedRoundRestart(t *testing.T, overdue bool) {
	t.Helper()

	f := newDirectedSendFixture(t, func(c *waved.Config) {
		c.EagerRoundJoin = true
		c.AdmissionTimeout = 5 * time.Minute
		if overdue {
			c.AdmissionTimeout = 4 * time.Second
		}
	})
	queueAdmissionRefresh(t, f)
	id := admitRefresh(t, f)
	initial := readAdmissionDeadline(t, f, id)
	require.False(t, initial.Closed)
	f.shutdown()
	if overdue {
		require.Eventually(t, func() bool {
			return time.Now().UnixNano() >= initial.ExpiresAt
		}, 6*time.Second, 50*time.Millisecond)
	}
	f.launch()
	requireVTXOStatusEventually(
		t, f.client, f.seededOutpoint,
		waverpc.VTXOStatus_VTXO_STATUS_LIVE, 15*time.Second,
	)
	closed := readAdmissionDeadline(t, f, id)
	require.True(t, closed.Closed)
	require.Equal(t, initial.ExpiresAt, closed.ExpiresAt)
	queueAdmissionRefresh(t, f)
}
