//go:build js && wasm

package main

import (
	"errors"
	"syscall/js"
	"testing"

	"github.com/stretchr/testify/require"
)

// callWalletJS invokes the registered function from JS and waits for its
// Promise, retaining both callbacks until the Promise settles.
func callWalletJS(method string, body map[string]any) (js.Value, error) {
	type result struct {
		value js.Value
		err   error
	}
	done := make(chan result, 1)
	resolve := js.FuncOf(func(_ js.Value, args []js.Value) any {
		done <- result{value: args[0]}

		return nil
	})
	defer resolve.Release()
	reject := js.FuncOf(func(_ js.Value, args []js.Value) any {
		done <- result{err: errors.New(args[0].Get("message").String())}

		return nil
	})
	defer reject.Release()
	js.Global().Call("wavewalletdkCall", method, body).Call(
		"then", resolve, reject,
	)
	res := <-done

	return res.value, res.err
}

// TestWASMArkChannelDispatch runs the JS -> mobile -> embedded RPC path on a
// fresh wallet. Specific handler rejections prove that valid request fields
// reached a registered RPC; unknown verbs or generic failures do not suffice.
func TestWASMArkChannelDispatch(t *testing.T) {
	call := installWalletCall()
	defer call.Release()
	_, err := callWalletJS("start", map[string]any{
		"data_dir":              t.TempDir(),
		"network":               "regtest",
		"wallet_type":           "lwwallet",
		"wallet_esplora_url":    "http://127.0.0.1:1",
		"server_address":        "http://127.0.0.1:1",
		"server_transport":      "rest",
		"server_insecure":       true,
		"swap_server_address":   "http://127.0.0.1:1",
		"swap_server_transport": "rest",
		"swap_server_insecure":  true,
		"ark_channels_enabled":  true,
	})
	require.NoError(t, err)
	defer func() {
		_, err := callWalletJS("stop", nil)
		require.NoError(t, err)
	}()

	for _, tc := range []struct {
		method string
		req    map[string]any
		want   string
	}{
		{
			"promoteVTXO",
			map[string]any{
				"AmountSat": 5000,
			},
			"idempotency key is required",
		},
		{
			"promoteVTXO",
			map[string]any{
				"AmountSat":      5000,
				"IdempotencyKey": "one",
			},
			"runtime is not ready",
		},
		{
			"payOnActiveChannel",
			map[string]any{},
			"payment request is required",
		},
		{
			"payOnActiveChannel",
			map[string]any{
				"Invoice":   "ln-pay",
				"MaxFeeSat": 1,
			},
			"runtime is not ready",
		},
		{
			"receiveOnActiveChannel",
			map[string]any{
				"AmountSat": 5000,
			},
			"create active-channel receive invoice:",
		},
		{
			"receive",
			map[string]any{
				"AmountSat": 5000,
			},
			"create receive invoice:",
		},
	} {
		t.Run(tc.method, func(t *testing.T) {
			_, err := callWalletJS(tc.method, tc.req)
			require.ErrorContains(t, err, tc.want)
			if tc.method == "receive" || tc.method == "receiveOnActiveChannel" {
				require.ErrorContains(
					t, err, "wallet is not ready",
				)
			}
		})
	}
}
