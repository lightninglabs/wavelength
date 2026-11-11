package coordinator

import (
	"testing"

	"github.com/lightninglabs/wavelength/vhtlcrecovery"
	"github.com/stretchr/testify/require"
)

// TestServiceRequestLookup resolves one arm key without inspecting unrelated
// active jobs, whose unroll probes may be slow or unavailable.
func TestServiceRequestLookup(t *testing.T) {
	t.Parallel()
	job := testRecoveryJob("wanted", vhtlcrecovery.StateArmed)
	other := testRecoveryJob("other", vhtlcrecovery.StateUnrollStarted)
	store := newFakeStore(job, other)
	service := newTestService(t, store, &fakeUnrollRegistry{})
	got, err := service.GetRecoveryStatusByRequestID(
		t.Context(), job.RequestID,
	)
	require.NoError(t, err)
	require.Equal(t, job.ID, got.Job.ID)
	require.Empty(t, store.events)
	_, err = service.GetRecoveryStatusByRequestID(t.Context(), "missing")
	require.ErrorContains(t, err, "missing request")
}
