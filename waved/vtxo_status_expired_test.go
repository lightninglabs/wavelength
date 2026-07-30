package waved

import (
	"testing"

	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
)

// TestVTXOStatusExpiredRoundTripsThroughRPC asserts the expired status is
// surfaced by the daemon RPC rather than collapsing to UNSPECIFIED.
//
// Expiry is not terminal — the value is recovered by forfeiting the VTXO in a
// round — so a wallet UI has to be able to tell "expired, recoverable" apart
// from "unknown".
func TestVTXOStatusExpiredRoundTripsThroughRPC(t *testing.T) {
	t.Parallel()

	proto := vtxoStatusToProto(vtxo.VTXOStatusExpired)
	require.Equal(t, waverpc.VTXOStatus_VTXO_STATUS_EXPIRED, proto)
	require.NotEqual(
		t, waverpc.VTXOStatus_VTXO_STATUS_UNSPECIFIED, proto,
		"an expired VTXO must not surface as an unknown status",
	)

	back, err := protoStatusToDomain(proto)
	require.NoError(t, err)
	require.Equal(t, vtxo.VTXOStatusExpired, back)
}
