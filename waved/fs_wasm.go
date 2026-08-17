//go:build js && wasm

package waved

import (
	"errors"
	"os"
	"syscall"
)

// ensureDataDir creates the daemon's data directory when the js/wasm host has
// a filesystem, and does nothing when it does not.
//
// The two hosts genuinely differ here. Go's wasm_exec.js installs a stub fs
// whose calls all fail with ENOSYS unless the host wires a real one in, so in
// a browser there is no directory to create and persistent state lives in OPFS
// instead. A Node host assigns globalThis.fs from node:fs before instantiating
// the module, and os.MkdirAll then reaches the host filesystem exactly as it
// would natively, which is where the node:fs SQLite VFS expects to find this
// directory.
//
// Rather than sniff the host, this asks for the directory and reads ENOSYS as
// the browser answering that it has no filesystem. Every other failure is
// real and is returned, so a Node host given an unwritable path fails at
// startup rather than at the first database open.
func ensureDataDir(dir string) error {
	if dir == "" {
		return nil
	}

	err := os.MkdirAll(dir, 0o700)
	if err == nil || errors.Is(err, syscall.ENOSYS) {
		return nil
	}

	return err
}
