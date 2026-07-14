//go:build !js || !wasm

package waved

import "os"

// ensureDataDir creates a host filesystem directory for daemon state.
func ensureDataDir(path string) error {
	return os.MkdirAll(path, 0700)
}
