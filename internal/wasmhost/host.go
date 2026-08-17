//go:build js && wasm

// Package wasmhost answers one question for the js/wasm build: which host is
// running this binary?
//
// The wavewalletdk wasm blob targets two very different hosts. A browser page
// has no filesystem and persists everything in OPFS. A Node process has a real
// filesystem, reached both through Go's own os package (once the host wires
// globalThis.fs to node:fs) and through the go-wasmsqlite nodefs VFS. Nothing
// else about the daemon differs, but storage differs completely, so the answer
// is needed in several packages and is worth deriving in exactly one place.
package wasmhost

import "syscall/js"

// UnderNode reports whether this binary is hosted by Node rather than by a
// browser page.
//
// Two signals are checked together because neither is sufficient alone. A
// bundler may define a process shim in a browser, so a node version string is
// not proof on its own; and a non-browser host other than Node would also lack
// a Worker constructor. Requiring both keeps a browser that fakes one of them
// from being mistaken for a filesystem host, which would send the daemon
// looking for storage that is not there.
func UnderNode() bool {
	process := js.Global().Get("process")
	if process.Type() != js.TypeObject {
		return false
	}

	versions := process.Get("versions")
	if versions.Type() != js.TypeObject ||
		versions.Get("node").Type() != js.TypeString {
		return false
	}

	return js.Global().Get("Worker").Type() != js.TypeFunction
}

// SQLiteVFS names the go-wasmsqlite VFS that gives this host durable storage.
// Both names are durable, so an open that asks for one and cannot have it
// fails rather than quietly running against memory.
func SQLiteVFS() string {
	if UnderNode() {
		return "nodefs"
	}

	return "opfs"
}
