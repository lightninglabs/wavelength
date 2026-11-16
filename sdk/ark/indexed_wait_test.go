package ark

import (
	"context"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// indexedWaitRPC captures the additive fields without requiring an operator.
type indexedWaitRPC struct {
	waverpc.DaemonServiceClient
	request  *waverpc.GetIndexedVTXOByPkScriptRequest
	response *waverpc.GetIndexedVTXOByPkScriptResponse
}

// GetIndexedVTXOByPkScript records the wait request and returns scripted state.
func (d *indexedWaitRPC) GetIndexedVTXOByPkScript(_ context.Context,
	req *waverpc.GetIndexedVTXOByPkScriptRequest, _ ...grpc.CallOption) (
	*waverpc.GetIndexedVTXOByPkScriptResponse, error) {

	d.request = req

	return d.response, nil
}

// TestWaitIndexedVTXOCompatibility proves older responses cannot suppress
// fallback or bypass policy authorization, including on empty results.
func TestWaitIndexedVTXOCompatibility(t *testing.T) {
	t.Parallel()
	for _, supported := range []bool{false, true} {
		for _, wait := range []time.Duration{
			0, time.Nanosecond, 2 * time.Second, time.Hour,
		} {
			response := &waverpc.GetIndexedVTXOByPkScriptResponse{}
			response.PolicyAuthorized = true
			response.WaitSupported = supported
			d := &indexedWaitRPC{response: response}
			c := &Client{daemon: d}
			info, waited, err := c.WaitIndexedVTXO(
				t.Context(), []byte{1}, []byte{2},
				waverpc.VTXOStatus_VTXO_STATUS_SPENT, wait,
			)
			require.NoError(t, err)
			require.Nil(t, info)
			require.Equal(t, supported && wait > 0, waited)
			require.Equal(t, []byte{2}, d.request.PolicyTemplate)
			require.LessOrEqual(
				t, d.request.WaitForMatchMs, uint32(30000),
			)
			if wait > 0 {
				require.Positive(t, d.request.WaitForMatchMs)
			}
			d.response.PolicyAuthorized = false
			_, _, err = c.WaitIndexedVTXO(
				t.Context(), []byte{1}, []byte{2},
				waverpc.VTXOStatus_VTXO_STATUS_SPENT, wait,
			)
			require.ErrorContains(
				t, err, "did not authorize indexed policy",
			)
			require.Equal(t, codes.Unimplemented, status.Code(err))
		}
	}
}
