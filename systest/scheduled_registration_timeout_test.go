//go:build systest

package systest

import (
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/lib/batchschedule"
	"github.com/lightninglabs/wavelength/waved"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
)

// TestScheduledRefreshWaitsForAdmission delays the real mailbox admission
// beyond the ordinary registration timeout. The daemon must retain the input
// and accept the acknowledgement while its selected batch is still reachable.
func TestScheduledRefreshWaitsForAdmission(t *testing.T) {
	ParallelN(t)

	const grace = 500 * time.Millisecond
	const pending = waverpc.VTXOStatus_VTXO_STATUS_PENDING_FORFEIT
	f := newDirectedSendFixtureWithTerms(
		t, func(info *arkrpc.GetInfoResponse) {
			now := time.Now()
			cutoff := now.Add(2 * time.Minute).Truncate(time.Second)
			schedule, err := batchschedule.New(
				cutoff, 20*time.Minute, 5*time.Minute,
			)
			require.NoError(t, err)
			info.BatchSchedule = arkrpc.BatchScheduleToProto(
				schedule, now, cutoff,
			)
		}, func(c *waved.Config) {
			c.RegistrationTimeout = grace
		},
	)
	queueAdmissionRefresh(t, f)
	require.Eventually(t, func() bool {
		return len(f.mailboxServer.joinRoundRequests()) == 1
	}, 15*time.Second, 25*time.Millisecond)

	// Observe the actual persisted reservation for longer than the old
	// timeout. A dropped attempt must not be concealed by a new join.
	require.Never(t, func() bool {
		for _, v := range listAllVTXOs(t, f.client) {
			if v.Outpoint == outpointString(f.seededOutpoint) {
				return v.Status != pending
			}
		}

		return true
	}, 3*grace, 25*time.Millisecond)

	id := admitRefresh(t, f)
	deadline := readAdmissionDeadline(t, f, id)
	require.False(t, deadline.Closed)
	require.Len(t, f.mailboxServer.joinRoundRequests(), 1)
	requireVTXOStatusEventually(
		t, f.client, f.seededOutpoint, pending, 5*time.Second,
	)
}
