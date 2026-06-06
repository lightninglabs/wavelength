package unroll

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFeeInputFanoutCoordinatorAggregatesChildDemand(t *testing.T) {
	t.Parallel()

	coordinator := &FeeInputFanoutCoordinator{}
	coordinator.recordDemand("child-a", 1)
	coordinator.recordDemand("child-b", 2)
	coordinator.recordDemand("child-c", 3)

	require.Equal(t, 6, coordinator.aggregateDemand())

	coordinator.recordDemand("child-b", 4)
	require.Equal(t, 8, coordinator.aggregateDemand())
}
