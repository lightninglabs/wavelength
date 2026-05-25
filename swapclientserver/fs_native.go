//go:build swapruntime && (!js || !wasm)

package swapclientserver

import (
	"os"
	"path/filepath"
)

// ensureSwapDBDir creates the host directory that contains the swap database.
func ensureSwapDBDir(dbPath string) error {
	return os.MkdirAll(filepath.Dir(dbPath), 0o700)
}
