//go:build js && wasm

package darepod

import (
	"errors"
	"fmt"
	"strings"
	"syscall/js"
)

// The encrypted seed is persisted as a single file in the origin-private file
// system (OPFS). OPFS is reachable from both the window and worker globals,
// unlike localStorage which is window-only, so the daemon can run inside a Web
// Worker. SeedFilePath produces a "/dir/dir/wallet_seed.enc" path; it is split
// on "/" and walked as OPFS directory handles, persisting the seed at that
// nested path in the OPFS origin (the daemon's SQLite data is stored
// separately, under hashed flat names at the OPFS root). These functions block
// the calling goroutine on the async OPFS promises, so they keep their
// synchronous signatures; they must run off the JS call stack (the verb
// goroutines spawned by promise() satisfy this).

// errOPFSUnavailable is returned when the OPFS API is absent from the current
// JS context. This is a deterministic capability gap rather than a transient
// failure, so callers can safely treat it as "no seed has ever been persisted".
var errOPFSUnavailable = errors.New("OPFS (navigator.storage.getDirectory) " +
	"is unavailable")

// opfsError wraps a rejected OPFS promise, preserving the DOMException name
// (e.g. "NotFoundError") alongside its string form so callers can tell a
// genuinely-absent file apart from an ambiguous runtime failure.
type opfsError struct {
	name string
	msg  string
}

// Error returns the string form of the underlying OPFS rejection.
func (e *opfsError) Error() string {
	return e.msg
}

// isNotFound reports whether err is an OPFS rejection caused by a missing file
// or directory (a DOMException with name "NotFoundError").
func isNotFound(err error) bool {
	var oerr *opfsError

	return errors.As(err, &oerr) && oerr.name == "NotFoundError"
}

// awaitJSPromise blocks the calling goroutine until the JS promise settles and
// returns its resolved value, or an error built from the rejection reason. The
// channel is buffered so the settle callback never blocks the JS event loop. p
// must be a thenable (a real Promise); every OPFS caller here passes one.
func awaitJSPromise(p js.Value) (js.Value, error) {
	type settled struct {
		value js.Value
		err   error
	}
	ch := make(chan settled, 1)

	onResolve := js.FuncOf(func(_ js.Value, args []js.Value) any {
		var v js.Value
		if len(args) > 0 {
			v = args[0]
		}
		ch <- settled{value: v}

		return nil
	})
	defer onResolve.Release()

	onReject := js.FuncOf(func(_ js.Value, args []js.Value) any {
		oerr := &opfsError{msg: "promise rejected"}
		if len(args) > 0 && args[0].Truthy() {
			reason := args[0]
			oerr.msg = reason.Call("toString").String()

			// DOMExceptions carry the failure kind on .name
			// (e.g. "NotFoundError"); preserve it so callers can
			// distinguish a missing file from other failures.
			if n := reason.Get("name"); n.Type() == js.TypeString {
				oerr.name = n.String()
			}
		}
		ch <- settled{err: oerr}

		return nil
	})
	defer onReject.Release()

	p.Call("then", onResolve, onReject)

	res := <-ch

	return res.value, res.err
}

// opfsRootDir resolves the OPFS root directory handle, returning an error when
// OPFS is unavailable in the current context.
func opfsRootDir() (js.Value, error) {
	storage := js.Global().Get("navigator").Get("storage")
	if !storage.Truthy() || storage.Get("getDirectory").IsUndefined() {
		return js.Value{}, errOPFSUnavailable
	}

	return awaitJSPromise(storage.Call("getDirectory"))
}

// splitSeedPath splits a "/a/b/file" seed path into its directory segments and
// the final file name, skipping empty segments.
func splitSeedPath(path string) (dirs []string, file string) {
	var parts []string
	for _, p := range strings.Split(path, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}

	if len(parts) == 0 {
		return nil, ""
	}

	return parts[:len(parts)-1], parts[len(parts)-1]
}

// opfsSeedDir walks the OPFS directory chain for the seed path, optionally
// creating it, and returns the parent directory handle and the file name.
func opfsSeedDir(path string, create bool) (js.Value, string, error) {
	dirs, file := splitSeedPath(path)
	if file == "" {
		return js.Value{}, "", fmt.Errorf("invalid seed path %q", path)
	}

	dir, err := opfsRootDir()
	if err != nil {
		return js.Value{}, "", err
	}

	opts := map[string]any{"create": create}
	for _, name := range dirs {
		dir, err = awaitJSPromise(
			dir.Call("getDirectoryHandle", name, opts),
		)
		if err != nil {
			return js.Value{}, "", err
		}
	}

	return dir, file, nil
}

// openSeedFile resolves the handle for an existing seed file, returning the
// underlying OPFS error (e.g. a NotFoundError) when the file or one of its
// parent directories is absent.
func openSeedFile(path string) (js.Value, error) {
	dir, file, err := opfsSeedDir(path, false)
	if err != nil {
		return js.Value{}, err
	}

	return awaitJSPromise(
		dir.Call(
			"getFileHandle", file, map[string]any{
				"create": false,
			},
		),
	)
}

// SaveEncryptedSeed writes the encrypted seed ciphertext to its OPFS file. The
// payload is already password-encrypted by EncryptSeed before this boundary.
func SaveEncryptedSeed(path string, ciphertext []byte) error {
	dir, file, err := opfsSeedDir(path, true)
	if err != nil {
		return fmt.Errorf("opening seed directory for %q: %w", path,
			err)
	}

	handle, err := awaitJSPromise(
		dir.Call(
			"getFileHandle", file, map[string]any{
				"create": true,
			},
		),
	)
	if err != nil {
		return fmt.Errorf("creating seed file %q: %w", path, err)
	}

	writable, err := awaitJSPromise(handle.Call("createWritable"))
	if err != nil {
		return fmt.Errorf("opening seed file %q for write: %w", path,
			err)
	}

	buf := js.Global().Get("Uint8Array").New(len(ciphertext))
	js.CopyBytesToJS(buf, ciphertext)

	// A writable stream holds an exclusive lock on the file until it is
	// closed, so close it unconditionally even when the write fails;
	// otherwise the leaked lock blocks later reads and writes until the
	// stream is garbage-collected. The write error takes precedence.
	_, writeErr := awaitJSPromise(writable.Call("write", buf))
	_, closeErr := awaitJSPromise(writable.Call("close"))

	if writeErr != nil {
		return fmt.Errorf("writing seed file %q: %w", path, writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing seed file %q: %w", path, closeErr)
	}

	return nil
}

// LoadEncryptedSeed reads the encrypted seed ciphertext from its OPFS file.
func LoadEncryptedSeed(path string) ([]byte, error) {
	handle, err := openSeedFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading seed file %q: %w", path, err)
	}

	fileObj, err := awaitJSPromise(handle.Call("getFile"))
	if err != nil {
		return nil, fmt.Errorf("opening seed file %q: %w", path, err)
	}

	bufVal, err := awaitJSPromise(fileObj.Call("arrayBuffer"))
	if err != nil {
		return nil, fmt.Errorf("reading seed file %q: %w", path, err)
	}

	arr := js.Global().Get("Uint8Array").New(bufVal)
	data := make([]byte, arr.Get("length").Int())
	js.CopyBytesToGo(data, arr)

	return data, nil
}

// SeedFileExists reports whether an encrypted seed file exists in OPFS for the
// network data directory. It returns false only when absence is confirmed: a
// NotFoundError for the file or one of its parent directories, or OPFS being
// unavailable (so nothing could ever have been persisted). Any other error is
// ambiguous (transient I/O, quota, a handle locked by a concurrent writer), so
// the seed is reported as present to keep the daemon out of the "no wallet"
// state, where a later InitWallet would overwrite a seed that may exist.
func SeedFileExists(networkDir string) bool {
	_, err := openSeedFile(SeedFilePath(networkDir))
	switch {
	case err == nil:
		return true

	case isNotFound(err), errors.Is(err, errOPFSUnavailable):
		return false

	default:
		return true
	}
}
