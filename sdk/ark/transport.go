package ark

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
)

const (
	// defaultBufConnSize is the size of the in-memory listener buffer used
	// by embedded clients that do not provide their own transport.
	defaultBufConnSize = 1 << 20
)

// waitForReady forces a new client connection to attempt dialing and waits
// until it reaches READY, the embedded daemon exits, or the caller's context
// expires.
func waitForReady(ctx context.Context, conn *grpc.ClientConn,
	runDoneChan <-chan error) error {

	conn.Connect()

	for {
		state := conn.GetState()
		if state == connectivity.Ready {
			return nil
		}

		if state == connectivity.Shutdown {
			return fmt.Errorf("grpc connection shut down before " +
				"readiness")
		}

		waitCtx, waitCancel := context.WithCancel(ctx)
		runExitErr := make(chan error, 1)
		go func() {
			select {
			case runErr := <-runDoneChan:
				if runErr != nil {
					runExitErr <- fmt.Errorf("embedded "+
						"daemon exited before "+
						"readiness: %w", runErr)
				} else {
					runExitErr <- fmt.Errorf("embedded " +
						"daemon exited before " +
						"readiness")
				}

				waitCancel()

			case <-waitCtx.Done():
			}
		}()

		if !conn.WaitForStateChange(waitCtx, state) {
			waitCancel()

			select {
			case err := <-runExitErr:
				return err

			default:
			}

			return fmt.Errorf("wait for grpc readiness: %w",
				ctx.Err())
		}

		waitCancel()
	}
}
