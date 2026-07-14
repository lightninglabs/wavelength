//go:build js && wasm

package waved

// ensureDataDir is a no-op in browser builds. Persistent state uses
// OPFS-backed SQLite and browser storage instead of host filesystem
// directories, and os.MkdirAll is not implemented under js/wasm.
func ensureDataDir(string) error {
	return nil
}
