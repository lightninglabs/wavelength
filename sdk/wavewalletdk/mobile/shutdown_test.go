//go:build mobile && wavewalletrpc && swapruntime

package mobile

import (
	"context"
	"testing"

	"github.com/lightninglabs/wavelength/sdk/wavewalletdk"
	"github.com/stretchr/testify/require"
)

// TestStopRetainsStartingGeneration blocks startup after cancellation to prove
// a host timeout cannot release the singleton or allow an overlapping Start.
func TestStopRetainsStartingGeneration(t *testing.T) {
	resetMobileState(t)
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{})
	returned := make(chan error, 1)
	go func() {
		returned <- startEmbedded(
			func(startCtx, _ context.Context) (*wavewalletdk.Client,
				error) {

				close(entered)
				<-startCtx.Done()
				close(cancelled)
				<-release

				return &wavewalletdk.Client{}, nil
			},
		)
	}()
	<-entered
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, stopWithContext(ctx), context.Canceled)
	<-cancelled
	state.mu.Lock()
	status := state.status
	state.mu.Unlock()
	require.Equal(t, statusStopping, status)
	require.ErrorContains(t, Start("{}"), "already started")
	require.ErrorIs(t, stopWithContext(ctx), context.Canceled)
	_, _, err := activeClient()
	require.Error(t, err)
	close(release)
	require.NoError(t, stopWithContext(t.Context()))
	require.ErrorContains(t, <-returned, "cancelled by Stop")
	require.NoError(
		t, startEmbedded(
			func(context.Context, context.Context) (
				*wavewalletdk.Client, error) {

				return &wavewalletdk.Client{}, nil
			},
		),
	)
	require.NoError(t, Stop())
}
