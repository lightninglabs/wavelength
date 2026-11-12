package wavewalletdk

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/waved"
	"github.com/stretchr/testify/require"
)

// TestVTXOAnchorReadRetainsDaemonUntilFinalAccess pins cancellation, shutdown
// joining and closed-handle admission for the direct ancestry callback.
func TestVTXOAnchorReadRetainsDaemonUntilFinalAccess(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	var closed atomic.Bool
	client := &Client{
		readVTXOAnchors: func(ctx context.Context, _ string) (
			[]waved.VTXOAnchor, error) {

			close(started)
			<-ctx.Done()
			<-release

			return nil, ctx.Err()
		},
		closeFn: func(context.Context) error {
			closed.Store(true)

			return nil
		},
	}
	go func() {
		_, err := client.VTXOAnchors(context.Background(), "selected:7")
		done <- err
	}()
	<-started
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, client.CloseContext(ctx), context.DeadlineExceeded)
	require.False(t, closed.Load())
	close(release)
	require.ErrorIs(t, <-done, context.Canceled)
	require.NoError(t, client.CloseContext(t.Context()))
	require.True(t, closed.Load())
	_, err := client.VTXOAnchors(t.Context(), "selected:7")
	require.ErrorIs(t, err, ErrClientClosed)
}
