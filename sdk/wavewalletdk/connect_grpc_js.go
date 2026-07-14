//go:build js

package wavewalletdk

import (
	"context"
	"fmt"
)

func connectGRPC(context.Context, ConnectConfig) (*Client, error) {
	return nil, fmt.Errorf("wavewalletdk gRPC transport is unavailable " +
		"in js/wasm builds; use TransportREST")
}
