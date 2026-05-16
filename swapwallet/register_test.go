//go:build walletrpc && swapruntime

package swapwallet

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/lightninglabs/darepo-client/darepod"
	"github.com/stretchr/testify/require"
)

// fakeResumeOnlyBackend implements darepod.SwapBackend but deliberately
// does NOT implement swapclientrpc.SwapClientServiceServer so it forces
// the type-assertion error path inside Register. Used to assert the
// SuppressResume-failover hook fires.
type fakeResumeOnlyBackend struct {
	resumeCalls atomic.Int32
}

func (f *fakeResumeOnlyBackend) ResumePending(_ context.Context) {
	f.resumeCalls.Add(1)
}

// TestRegisterRecoversResumeOnTypeAssertionFailure asserts that when
// Register bails out because the swap backend does not satisfy
// SwapClientServiceServer, it compensates for the SuppressResume
// handshake by calling Backend.ResumePending. Without this, the swap
// subserver would have skipped its own resume sweep (the walletrpc
// build sets cfg.Swap.SuppressResume = true unconditionally before
// this registrar runs) and no actor would ever drive pending workers.
func TestRegisterRecoversResumeOnTypeAssertionFailure(t *testing.T) {
	t.Parallel()

	backend := &fakeResumeOnlyBackend{}
	cfg := &darepod.Config{
		Swap: &darepod.SwapConfig{
			Backend:        backend,
			SuppressResume: true,
		},
	}

	cleanup, err := Register(t.Context(), nil, nil, cfg)
	require.Error(
		t, err, "a backend that does not implement "+
			"SwapClientServiceServer must surface a "+
			"registration error",
	)
	require.Nil(
		t, cleanup,
		"a failed Register must NOT return a cleanup function",
	)
	require.Equal(
		t, int32(1), backend.resumeCalls.Load(),
		"failover must invoke ResumePending exactly once so the "+
			"daemon does not leak SuppressResume into a "+
			"never-resumed swap subsystem",
	)
}

// TestRegisterRejectsNilBackend asserts the missing-handle error path is
// stable and does not panic. No failover is expected here because there
// is nothing to resume: the swap subserver itself never published its
// handle.
func TestRegisterRejectsNilBackend(t *testing.T) {
	t.Parallel()

	cfg := &darepod.Config{
		Swap: &darepod.SwapConfig{
			Backend: nil,
		},
	}
	cleanup, err := Register(t.Context(), nil, nil, cfg)
	require.ErrorIs(t, err, ErrSwapBackendUnavailable)
	require.Nil(t, cleanup)
}
