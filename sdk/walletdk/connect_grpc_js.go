//go:build js

package walletdk

import (
	"context"
	"fmt"
)

func connectGRPC(context.Context, ConnectConfig) (*Client, error) {
	return nil, fmt.Errorf("walletdk gRPC transport is unavailable in " +
		"js/wasm builds; use TransportREST")
}
