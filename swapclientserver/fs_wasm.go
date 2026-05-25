//go:build swapruntime && js && wasm

package swapclientserver

// ensureSwapDBDir is a no-op in browser builds. The SQLite driver maps the
// configured database filename to OPFS, where host directories do not exist.
func ensureSwapDBDir(string) error {
	return nil
}
