package types

import (
	"testing"

	"github.com/lightninglabs/wavelength/lib/batchschedule"
	"github.com/stretchr/testify/require"
)

// TestJoinAuthBindsBatchSlot preserves legacy bytes and authenticates every
// part of explicit scheduled admission, including the absence of a selection.
func TestJoinAuthBindsBatchSlot(t *testing.T) {
	t.Parallel()
	req := testJoinRoundAuthRequest(t)
	legacy, err := JoinRoundAuthMessage(req)
	require.NoError(t, err)
	req.BatchSlot = &batchschedule.Selection{
		ScheduleID: [32]byte{
			1,
		},
		CutoffUnix: 1800003600,
	}
	scheduled, err := JoinRoundAuthMessage(req)
	require.NoError(t, err)
	require.NotEqual(t, legacy, scheduled)
	decoded, err := DecodeJoinRoundAuthMessage(scheduled)
	require.NoError(t, err)
	require.Equal(t, req.BatchSlot, decoded.BatchSlot)
	req.BatchSlot.CutoffUnix++
	changed, err := JoinRoundAuthMessage(req)
	require.NoError(t, err)
	require.NotEqual(t, scheduled, changed)
	req.BatchSlot.ScheduleID[0]++
	changedID, err := JoinRoundAuthMessage(req)
	require.NoError(t, err)
	require.NotEqual(t, changed, changedID)
	req.BatchSlot = nil
	again, err := JoinRoundAuthMessage(req)
	require.NoError(t, err)
	require.Equal(t, legacy, again)
	decoded, err = DecodeJoinRoundAuthMessage(legacy)
	require.NoError(t, err)
	require.Nil(t, decoded.BatchSlot)
}
