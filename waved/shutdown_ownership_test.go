package waved

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/oor"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// TestSubmittedOORCleanupJoinIncludesFinalCallback proves the RPC cleanup owner
// cannot finish while a detached continuation still accesses wallet state.
func TestSubmittedOORCleanupJoinIncludesFinalCallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rpc := &RPCServer{}
		promise := actor.NewPromise[oor.ActorResp]()
		promise.Complete(
			fn.Ok[oor.ActorResp](
				&oor.StartTransferResponse{},
			),
		)
		entered, release := make(chan struct{}), make(chan struct{})
		defer func() {
			select {
			case <-release:
			default:
				close(release)
			}
		}()
		rpc.cleanupSubmittedOORStartWithTimeout(
			t.Context(), promise.Future(), nil, func() {
				close(entered)
				<-release
			}, time.Minute,
		)
		<-entered
		joined := make(chan struct{})
		go func() {
			rpc.cleanupWorkers.Wait()
			close(joined)
		}()
		synctest.Wait()
		select {
		case <-joined:
			t.Fatal(
				"RPC cleanup returned before its final " +
					"callback",
			)

		default:
		}
		close(release)
		<-joined
	})
}
