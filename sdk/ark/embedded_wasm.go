//go:build js && wasm

package ark

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
)

// EmbeddedConfig configures an in-process daemon runtime managed by the SDK.
type EmbeddedConfig struct {
	// DaemonConfig is intentionally untyped in browser builds so packages
	// that only need remote Ark SDK types do not import the native daemon.
	DaemonConfig any

	// BufferSize is ignored in browser builds.
	BufferSize int

	// DialOptions is ignored in browser builds.
	DialOptions []grpc.DialOption
}

// StartEmbedded is not available in browser builds until the daemon runtime
// has a WASM-safe construction path.
func StartEmbedded(context.Context, EmbeddedConfig) (*Client, error) {
	return nil, fmt.Errorf("embedded ark runtime is not available in wasm")
}
