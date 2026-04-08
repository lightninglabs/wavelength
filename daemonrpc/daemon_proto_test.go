package daemonrpc

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestSendVTXOResponseProtoRoundTrip guards against stale generated
// descriptors dropping newly added response fields on the wire.
func TestSendVTXOResponseProtoRoundTrip(t *testing.T) {
	t.Parallel()

	original := &SendVTXOResponse{
		Status:          "submitted",
		TotalAmountSat:  40_000,
		ChangeAmountSat: 59_000,
		SelectedCount:   1,
	}

	payload, err := proto.Marshal(original)
	require.NoError(t, err)

	var decoded SendVTXOResponse
	require.NoError(t, proto.Unmarshal(payload, &decoded))
	require.Equal(t, original.Status, decoded.Status)
	require.Equal(t, original.TotalAmountSat, decoded.TotalAmountSat)
	require.Equal(t, original.ChangeAmountSat, decoded.ChangeAmountSat)
	require.Equal(t, original.SelectedCount, decoded.SelectedCount)
}
