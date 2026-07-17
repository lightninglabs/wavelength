package waveclicommands

import (
	"testing"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestSummarizeRefreshFeeEstimateNil verifies a response without an
// estimate (real refresh, empty selection) renders no stderr lines.
func TestSummarizeRefreshFeeEstimateNil(t *testing.T) {
	t.Parallel()

	require.Empty(t, summarizeRefreshFeeEstimate(nil))
}

// TestSummarizeRefreshFeeEstimatePaid verifies the ordinary paid
// preview renders the headline total, the selection size, and the
// advisory caveat.
func TestSummarizeRefreshFeeEstimatePaid(t *testing.T) {
	t.Parallel()

	lines := summarizeRefreshFeeEstimate(&waverpc.RefreshFeeEstimate{
		EstimatedTotalFeeSat: proto.Int64(1_234),
		Outpoints: []*waverpc.OutpointFeeEstimate{
			{Outpoint: "aa:0"}, {Outpoint: "bb:1"},
		},
	})
	require.Len(t, lines, 1)
	require.Contains(t, lines[0], "1234 sat")
	require.Contains(t, lines[0], "2 VTXO(s)")
	require.Contains(t, lines[0], "advisory")
	require.Contains(t, lines[0], "seal time")
}

// TestSummarizeRefreshFeeEstimateFree verifies a waiver-eligible
// selection leads with the zero fee and names the free-refresh window
// as the reason.
func TestSummarizeRefreshFeeEstimateFree(t *testing.T) {
	t.Parallel()

	lines := summarizeRefreshFeeEstimate(&waverpc.RefreshFeeEstimate{
		FreeRefreshEligible: true,
		Outpoints: []*waverpc.OutpointFeeEstimate{
			{Outpoint: "aa:0", TotalFeeSat: 500},
		},
	})
	require.Len(t, lines, 1)
	require.Contains(t, lines[0], "0 sat")
	require.Contains(t, lines[0], "free-refresh window")
	require.Contains(t, lines[0], "advisory")
}

// TestSummarizeRefreshFeeEstimateError verifies a degraded estimate
// warns that the numbers are absent — and still says a fee applies —
// rather than printing a zero a user could misread as free.
func TestSummarizeRefreshFeeEstimateError(t *testing.T) {
	t.Parallel()

	lines := summarizeRefreshFeeEstimate(&waverpc.RefreshFeeEstimate{
		EstimateError: "operator fee estimate unavailable",
		Outpoints: []*waverpc.OutpointFeeEstimate{
			{Outpoint: "aa:0"},
		},
	})
	require.Len(t, lines, 1)
	require.Contains(t, lines[0], "warning")
	require.Contains(t, lines[0], "operator fee estimate unavailable")
	require.Contains(t, lines[0], "still charged")
	require.NotContains(t, lines[0], "0 sat")
}

// TestSummarizeRefreshFeeEstimateErrorFreeWindow verifies the locally
// computed waiver still surfaces when the operator quote failed: the
// user learns the refresh is expected free even in degraded mode.
func TestSummarizeRefreshFeeEstimateErrorFreeWindow(t *testing.T) {
	t.Parallel()

	lines := summarizeRefreshFeeEstimate(&waverpc.RefreshFeeEstimate{
		EstimateError:       "operator fee estimate unavailable",
		FreeRefreshEligible: true,
		Outpoints: []*waverpc.OutpointFeeEstimate{
			{Outpoint: "aa:0"},
		},
	})
	require.Len(t, lines, 2)
	require.Contains(t, lines[0], "warning")
	require.Contains(t, lines[1], "expected free")
}

// TestSummarizeRefreshFeeEstimateBelowDust verifies uneconomic VTXOs in
// the selection add a count-level warning on top of the fee headline.
func TestSummarizeRefreshFeeEstimateBelowDust(t *testing.T) {
	t.Parallel()

	lines := summarizeRefreshFeeEstimate(&waverpc.RefreshFeeEstimate{
		EstimatedTotalFeeSat: proto.Int64(400),
		Outpoints: []*waverpc.OutpointFeeEstimate{
			{Outpoint: "aa:0", BelowDustWarning: true},
			{Outpoint: "bb:1"},
			{Outpoint: "cc:2", BelowDustWarning: true},
		},
	})
	require.Len(t, lines, 2)
	require.Contains(t, lines[1], "2 selected VTXO(s)")
	require.Contains(t, lines[1], "minimum viable")
}
