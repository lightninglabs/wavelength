package types

import (
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/lib/batchschedule"
	"github.com/stretchr/testify/require"
)

// TestJoinAuthBindsBatchSlot pins how a scheduled slot selection enters the
// signed join-authorization message. A request without a selection must keep
// producing the legacy bytes, so existing clients and operators still verify
// each other. A request with a selection must authenticate both the schedule
// identity and the cutoff, so an operator cannot admit the join into a slot
// the client did not choose, and the selection must survive decoding intact.
func TestJoinAuthBindsBatchSlot(t *testing.T) {
	t.Parallel()

	// Record the legacy encoding of a request that selects no slot.
	req := testJoinRoundAuthRequest(t)
	legacy, err := JoinRoundAuthMessage(req)
	require.NoError(t, err)

	// Adding a selection must change the signed bytes, otherwise the
	// signature would not cover the chosen slot.
	req.BatchSlot = &batchschedule.Selection{
		ScheduleID: batchschedule.ID{
			1,
		},
		Cutoff: time.Unix(1800003600, 0).UTC(),
	}
	scheduled, err := JoinRoundAuthMessage(req)
	require.NoError(t, err)
	require.NotEqual(t, legacy, scheduled)

	// The operator decodes the same selection the client signed, which is
	// what it later matches against its collector.
	decoded, err := DecodeJoinRoundAuthMessage(scheduled)
	require.NoError(t, err)
	require.Equal(t, req.BatchSlot, decoded.BatchSlot)

	// Moving the cutoff by a single second must change the message, so a
	// signature cannot be replayed into the neighboring slot.
	req.BatchSlot.Cutoff = req.BatchSlot.Cutoff.Add(time.Second)
	changed, err := JoinRoundAuthMessage(req)
	require.NoError(t, err)
	require.NotEqual(t, scheduled, changed)

	// Changing only the schedule identity must also change the message,
	// so a signature cannot survive an operator policy change.
	req.BatchSlot.ScheduleID[0]++
	changedID, err := JoinRoundAuthMessage(req)
	require.NoError(t, err)
	require.NotEqual(t, changed, changedID)

	// Removing the selection restores the exact legacy bytes, and legacy
	// bytes decode to a request with no selection.
	req.BatchSlot = nil
	again, err := JoinRoundAuthMessage(req)
	require.NoError(t, err)
	require.Equal(t, legacy, again)

	decoded, err = DecodeJoinRoundAuthMessage(legacy)
	require.NoError(t, err)
	require.Nil(t, decoded.BatchSlot)
}
