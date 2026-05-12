//go:build !swapruntime

package walletdk

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSwapMethodsRequireSwapRuntime makes default builds fail swap calls before
// they attempt an unregistered RPC.
func TestSwapMethodsRequireSwapRuntime(t *testing.T) {
	client := &Client{}

	_, err := client.Receive(context.Background(), ReceiveRequest{
		AmountSat: 1,
	})
	require.ErrorIs(t, err, ErrSwapRuntimeUnavailable)

	_, err = client.Send(context.Background(), SendRequest{
		Invoice: "invoice",
	})
	require.ErrorIs(t, err, ErrSwapRuntimeUnavailable)

	_, err = client.ListSwaps(context.Background(), ListSwapsRequest{})
	require.ErrorIs(t, err, ErrSwapRuntimeUnavailable)
}
