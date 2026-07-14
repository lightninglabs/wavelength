package waveclicommands

import (
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// mapSwapRuntimeRPCError turns the gRPC unknown-service response from a
// default daemon into actionable guidance. Other service errors are returned
// unchanged so daemon-side validation remains visible.
func mapSwapRuntimeRPCError(err error) error {
	if err == nil {
		return nil
	}

	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Unimplemented {
		return err
	}

	msg := st.Message()
	if !strings.Contains(msg, "SwapClientService") &&
		!strings.Contains(msg, "swapclientrpc") {
		return err
	}

	return fmt.Errorf("daemon was built without swapruntime support; " +
		"rebuild waved with tags=\"swapruntime\"")
}
