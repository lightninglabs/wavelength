package swaps

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
)

// TestValidateRouteHintRejectsTruncatedFields verifies route-hint fields are
// range checked before they are cast into the narrower zpay32 hop hint shape.
func TestValidateRouteHintRejectsTruncatedFields(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	nodeID := privKey.PubKey().SerializeCompressed()

	routeHint := &RouteHint{
		NodeID:      nodeID,
		FeeBaseMsat: uint64(^uint32(0)) + 1,
	}
	err = validateRouteHint(routeHint)
	require.ErrorContains(t, err, "fee base msat")

	routeHint = &RouteHint{
		NodeID:     nodeID,
		FeePropPpm: uint64(^uint32(0)) + 1,
	}
	err = validateRouteHint(routeHint)
	require.ErrorContains(t, err, "fee proportional ppm")

	routeHint = &RouteHint{
		NodeID:          nodeID,
		CltvExpiryDelta: uint32(^uint16(0)) + 1,
	}
	err = validateRouteHint(routeHint)
	require.ErrorContains(t, err, "CLTV expiry delta")
}
