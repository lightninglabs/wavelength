//go:build swapruntime && js && wasm

package swapclientserver

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// ensureSwapDBDir creates the directory holding the swap database when the
// js/wasm host has a filesystem, and does nothing when it does not.
//
// See waved.ensureDataDir for why ENOSYS is the browser's way of saying it has
// no host filesystem, while any other error is a real failure a Node host
// should hear about before it tries to open the database.
func ensureSwapDBDir(dbPath string) error {
	if dbPath == "" {
		return nil
	}

	err := os.MkdirAll(filepath.Dir(dbPath), 0o700)
	if err == nil || errors.Is(err, syscall.ENOSYS) {
		return nil
	}

	return err
}
