package waved

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/lnruntime"
	"github.com/stretchr/testify/require"
)

// quiesceRecovery records an unexpected release of the recovery lifecycle.
type quiesceRecovery struct {
	ArkChannelRecoveryController
	stopped atomic.Bool
}

// Stop records terminal teardown, which quiescing must not perform.
func (r *quiesceRecovery) Stop() {
	r.stopped.Store(true)
}

// TestNativeArkChannelQuiesceRetainsLifecycle verifies controller workers are
// joined while every object required by native shutdown remains available.
func TestNativeArkChannelQuiesceRetainsLifecycle(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	recovery := &quiesceRecovery{}
	node := &lnruntime.NativeNode{}
	service := &arkchannel.Service{}
	coordinator := &arkchannel.Coordinator{}
	controller := &NativeArkChannelController{
		cfg: ArkChannelControllerConfig{
			Recovery: recovery,
		},
		node: node, service: service, coordinator: coordinator,
		lifecycleCtx: ctx, lifecycleCancel: cancel,
	}
	canceled := make(chan struct{})
	release := make(chan struct{})
	controller.lifecycleWG.Add(1)
	go func() {
		defer controller.lifecycleWG.Done()
		<-ctx.Done()
		close(canceled)
		<-release
	}()
	done := make(chan struct{})
	go func() {
		controller.Quiesce()
		close(done)
	}()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("controller worker was not canceled")
	}
	select {
	case <-done:
		t.Fatal("quiesce returned before its worker joined")

	default:
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("quiesce did not join its worker")
	}
	require.Same(t, node, controller.node)
	require.Same(t, service, controller.service)
	require.Same(t, coordinator, controller.coordinator)
	require.Same(t, recovery, controller.cfg.Recovery)
	require.False(t, recovery.stopped.Load())
	require.True(t, controller.stopped)
	controller.Quiesce()
}
