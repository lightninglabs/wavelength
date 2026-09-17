package arkrpc

import (
	"math"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/lib/batchschedule"
	"github.com/stretchr/testify/require"
)

// TestBatchScheduleDiscovery rejects malformed or unsupported operator timing.
func TestBatchScheduleDiscovery(t *testing.T) {
	t.Parallel()
	now := time.Unix(1800000000, 0)
	s, err := batchschedule.New(now, time.Hour, time.Minute)
	require.NoError(t, err)
	p := BatchScheduleToProto(s, now, now.Add(time.Hour))
	parsed, err := ParseBatchSchedule(p)
	require.NoError(t, err)
	require.Equal(t, s.ID(), parsed.ID())
	p.Version = 2
	_, err = ParseBatchSchedule(p)
	require.Error(t, err)
	p.Version = 1
	p.IntervalSeconds = math.MaxInt64
	_, err = ParseBatchSchedule(p)
	require.Error(t, err)
	p.IntervalSeconds = 3600
	p.ScheduleId[0]++
	_, err = ParseBatchSchedule(p)
	require.Error(t, err)
	parsed, err = ParseBatchSchedule(nil)
	require.NoError(t, err)
	require.Nil(t, parsed)
}
