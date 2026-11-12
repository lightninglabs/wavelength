//go:build mobile && wavewalletrpc && swapruntime

package mobile

import (
	"testing"

	"github.com/lightninglabs/wavelength/sdk/wavewalletdk"
	"github.com/stretchr/testify/require"
)

// TestExitBatchJSONContract pins the mobile binding's field casing and textual
// outcomes, which differ from protobuf JSON used by the daemon REST endpoint.
func TestExitBatchJSONContract(t *testing.T) {
	t.Parallel()
	var request wavewalletdk.ExitBatchRequest
	require.NoError(t, decode([]byte(`{
        "Outpoints":["a:0","b:1"],"Destination":"common-address"
    }`), &request))
	require.Equal(t, []string{"a:0", "b:1"}, request.Outpoints)
	require.Equal(t, "common-address", request.Destination)
	raw, err := marshal(&wavewalletdk.ExitBatchResult{
		Destination: "common-address",
		Results: []wavewalletdk.ExitBatchInputResult{
			{
				Outpoint: "a:0",
				Outcome:  wavewalletdk.ExitBatchAccepted,
			},
			{
				Outpoint: "b:1",
				Outcome:  wavewalletdk.ExitBatchUnknown,
				Error:    "response lost",
			},
		},
	})
	require.NoError(t, err)
	require.JSONEq(t, `{
        "Destination":"common-address",
        "Results":[
            {"Outpoint":"a:0","Outcome":"accepted","Error":""},
            {"Outpoint":"b:1","Outcome":"unknown","Error":"response lost"}
        ]
    }`, string(raw))
}
