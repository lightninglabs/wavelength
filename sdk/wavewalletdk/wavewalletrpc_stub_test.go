//go:build (!wavewalletrpc || !swapruntime) && !js

package wavewalletdk

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestWalletMethodsRequireWalletRPC makes default embedded builds fail wallet
// calls before they attempt an unregistered RPC.
func TestWalletMethodsRequireWalletRPC(t *testing.T) {
	client := &Client{}

	_, err := client.Receive(context.Background(), ReceiveRequest{
		AmountSat: 1,
	})
	require.ErrorIs(t, err, ErrWalletRPCUnavailable)

	_, err = client.PrepareSend(context.Background(), PrepareSendRequest{
		Invoice: "invoice",
	})
	require.ErrorIs(t, err, ErrWalletRPCUnavailable)

	_, err = client.List(context.Background(), ListRequest{})
	require.ErrorIs(t, err, ErrWalletRPCUnavailable)
}

// TestStartRequiresWalletRPC makes embedded wavewalletdk fail fast instead of
// starting a daemon that cannot serve wallet RPCs.
func TestStartRequiresWalletRPC(t *testing.T) {
	client, err := Start(context.Background(), Config{})
	require.Nil(t, client)
	require.ErrorIs(t, err, ErrWalletRPCUnavailable)
}
