package waved

import (
	"testing"

	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/stretchr/testify/require"
)

// TestOperatorClientPublicationIsSynchronized publishes the operator client
// and runtime while another goroutine reads them, the way an RPC can overlap
// bootstrap, so the race detector can see the accesses. Readers observe
// either nothing or the published values.
func TestOperatorClientPublicationIsSynchronized(t *testing.T) {
	t.Parallel()

	srv := &Server{}
	client := &stubArkServiceClient{}
	rt := new(serverconn.Runtime)

	done := make(chan struct{})
	go func() {
		defer close(done)

		srv.publishOperatorClients(&operatorClients{ark: client})
		srv.publishRuntime(rt)
	}()

	for {
		select {
		case <-done:
			require.Same(t, client, srv.operatorArkClient())
			require.Same(t, rt, srv.loadRuntime())

			return

		default:
		}

		if got := srv.operatorArkClient(); got != nil {
			require.Same(t, client, got)
		}
		if got := srv.loadRuntime(); got != nil {
			require.Same(t, rt, got)
		}
	}
}
