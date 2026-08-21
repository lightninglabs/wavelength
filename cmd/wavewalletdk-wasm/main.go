//go:build js && wasm

// Command wavewalletdk-wasm exposes the embedded wavewalletdk runtime through
// a private browser transport ABI. It is a thin syscall/js adapter over the
// sdk/wavewalletdk/mobile JSON facade: every verb takes a JS request object and
// resolves a JS response object, so the daemon, swap, and OOR machinery all run
// in-process in the one browser VM with no separate gateway. A typed host SDK
// invokes this ABI from its Worker and owns lifecycle serialization and
// cross-tab locking; application code must not call the global directly. The
// facade is the single source of truth shared with the gomobile bindings, so
// this bridge never reaches into the wavewalletdk.Client API directly and
// cannot drift from it.
//
//	Build with: GOOS=js GOARCH=wasm go build \
//		-tags "mobile wavewalletrpc swapruntime" ./cmd/wavewalletdk-wasm
package main

import (
	"errors"
	"io"
	"syscall/js"

	"github.com/lightninglabs/wavelength/internal/wasmhost"
	"github.com/lightninglabs/wavelength/sdk/wavewalletdk/mobile"
)

// main installs the private browser transport entry point and then parks the Go
// runtime so the exported callbacks stay live for the lifetime of the page.
func main() {
	js.Global().Set("wavewalletdkCall", js.FuncOf(walletCall))
	js.Global().Call("dispatchEvent", js.Global().Get("CustomEvent").New(
		"wavewalletdk-ready",
	))

	select {}
}

// walletCall is the private JS transport dispatcher used by a typed host SDK.
// It takes a method name and an optional request object and returns a Promise
// that resolves with the verb's JSON response decoded into a JS value (or
// rejects with an Error). Calling it directly would bypass the host SDK's
// lifecycle queue and runtime lock.
func walletCall(_ js.Value, args []js.Value) any {
	if len(args) == 0 {
		return rejected(errors.New("method is required"))
	}

	method := args[0].String()

	var req js.Value
	if len(args) > 1 {
		req = args[1]
	}

	switch method {
	// Private lifecycle transport verbs. The typed host SDK serializes them
	// with stop and owns the runtime lock.
	case "start":
		cfg, err := startConfig(req)
		if err != nil {
			return rejected(err)
		}

		return promise(func() (any, error) {
			return js.Null(), mobile.Start(cfg)
		})

	case "startExternalSeedWallet":
		body, err := externalSeedWalletStartRequest(req)
		if err != nil {
			return rejected(err)
		}

		return promise(func() (any, error) {
			out, err := mobile.StartExternalSeedWallet(body)
			if err != nil {
				return nil, err
			}

			return parse(out), nil
		})

	case "stop":
		return promise(func() (any, error) {
			return js.Null(), mobile.Stop()
		})

	// No-argument verbs returning JSON.
	case "getInfo":
		return promise(jsonNoArg(mobile.GetInfo))

	case "balance":
		return promise(jsonNoArg(mobile.Balance))

	case "status":
		return promise(jsonNoArg(mobile.Status))

	// Request/response verbs returning JSON.
	case "createWallet":
		return promise(jsonVerb(req, mobile.CreateWallet))

	case "unlockWallet":
		return promise(jsonVerb(req, mobile.UnlockWallet))

	case "openWalletFromPasskey":
		return promise(jsonVerb(req, mobile.OpenWalletFromPasskey))

	case "deposit":
		return promise(jsonVerb(req, mobile.Deposit))

	case "receive":
		return promise(jsonVerb(req, mobile.Receive))

	case "prepareSend":
		return promise(jsonVerb(req, mobile.PrepareSend))

	case "sendPrepared":
		return promise(jsonVerb(req, mobile.SendPrepared))

	case "list":
		return promise(jsonVerb(req, mobile.List))

	case "exit":
		return promise(jsonVerb(req, mobile.Exit))

	case "exitStatus":
		return promise(jsonVerb(req, mobile.ExitStatus))

	case "exitSummary":
		return promise(jsonVerb(req, mobile.ExitSummary))

	case "getExitPlan":
		return promise(jsonVerb(req, mobile.GetExitPlan))

	case "sweepWallet":
		return promise(jsonVerb(req, mobile.SweepWallet))

	// Scalar convenience verbs for the hottest UI paths.
	case "confirmedBalanceSat":
		return promise(func() (any, error) {
			return mobile.ConfirmedBalanceSat()
		})

	case "pendingInboundSat":
		return promise(func() (any, error) {
			return mobile.PendingInboundSat()
		})

	case "walletReady":
		return promise(func() (any, error) {
			return mobile.WalletReady()
		})

	case "isRunning":
		return promise(func() (any, error) {
			return mobile.IsRunning(), nil
		})

	// Streaming verb, exposed as a pull handle.
	case "subscribe":
		return promise(func() (any, error) {
			sub, err := mobile.Subscribe(jsonBytes(req))
			if err != nil {
				return nil, err
			}

			return subscriptionHandle(sub), nil
		})

	default:
		return rejected(errors.New("unknown method: " + method))
	}
}

// jsonNoArg adapts a facade verb that takes no request and returns a JSON body.
func jsonNoArg(fn func() ([]byte, error)) func() (any, error) {
	return func() (any, error) {
		out, err := fn()
		if err != nil {
			return nil, err
		}

		return parse(out), nil
	}
}

// jsonVerb adapts a facade verb that takes a JSON request and returns a JSON
// body, marshalling the JS request object on the way in.
func jsonVerb(req js.Value,
	fn func([]byte) ([]byte, error)) func() (any, error) {

	return func() (any, error) {
		out, err := fn(jsonBytes(req))
		if err != nil {
			return nil, err
		}

		return parse(out), nil
	}
}

// subscriptionHandle wraps a pull-based mobile.Subscription as a JS object with
// next() (resolving the next entry, or null at end of stream) and close()
// methods. The handle owns its js.Func callbacks and releases them on close.
func subscriptionHandle(sub *mobile.Subscription) js.Value {
	handle := js.Global().Get("Object").New()

	var nextFn, closeFn js.Func

	nextFn = js.FuncOf(func(_ js.Value, _ []js.Value) any {
		return promise(func() (any, error) {
			entry, err := sub.Next()
			switch {
			case errors.Is(err, io.EOF):
				return js.Null(), nil

			case err != nil:
				return nil, err

			default:
				return parse(entry), nil
			}
		})
	})

	closeFn = js.FuncOf(func(_ js.Value, _ []js.Value) any {
		err := sub.Close()
		nextFn.Release()
		closeFn.Release()
		if err != nil {
			return jsError(err)
		}

		return js.Null()
	})

	handle.Set("next", nextFn)
	handle.Set("close", closeFn)

	return handle
}

// promise runs fn on a fresh goroutine and surfaces its result as a JS Promise.
// A panic in fn is recovered and rejected so it never kills the Go runtime.
func promise(fn func() (any, error)) any {
	var executor js.Func
	executor = js.FuncOf(func(_ js.Value, args []js.Value) any {
		resolve, reject := args[0], args[1]

		go func() {
			defer func() {
				if r := recover(); r != nil {
					reject.Invoke(
						jsError(
							errors.New(
								"wavewalletdk" +
									" panic"),
						),
					)
				}
			}()

			res, err := fn()
			if err != nil {
				reject.Invoke(jsError(err))

				return
			}

			resolve.Invoke(res)
		}()

		return nil
	})

	// The Promise constructor invokes the executor synchronously, so by the
	// time New returns the callback has already run and is never called
	// again. Release it now, otherwise every wallet call leaks a Go
	// callback handle for the lifetime of the page.
	p := js.Global().Get("Promise").New(executor)
	executor.Release()

	return p
}

// rejected returns an immediately-rejected Promise carrying err.
func rejected(err error) any {
	return js.Global().Get("Promise").Call("reject", jsError(err))
}

// jsError builds a JS Error from a Go error.
func jsError(err error) js.Value {
	return js.Global().Get("Error").New(err.Error())
}

// browserDataDir is a WASM-safe default data directory for a browser host. A
// browser has no $HOME for the daemon's `~` expansion to resolve against, and
// persistent state lives in OPFS-backed SQLite keyed by hashed file names
// rather than host directories, so any fixed in-origin path is all the
// embedded daemon needs.
//
// A Node host inverts both halves of that reasoning, which is why this default
// is not for it. Node has a real filesystem, so waved.ensureDataDir creates
// this path rather than ignoring it, and the node:fs SQLite VFS writes to the
// configured path rather than to a hashed OPFS name. Injecting it there asks
// the host to mkdir at the filesystem root, which fails with EROFS or EACCES
// for any process not running as root.
const browserDataDir = "/wavelength"

// errNodeDataDirRequired reports that a Node host started the daemon without
// saying where its databases should live. There is no default worth guessing
// here: the daemon's databases are the only copy of the wallet's state, and a
// path picked from the process's working directory or environment would put
// them somewhere that changes with how the host happened to be launched.
var errNodeDataDirRequired = errors.New("data_dir must be set when the " +
	"js/wasm host is Node, which stores databases on the host filesystem")

// startConfig renders the start request as a config JSON string, injecting the
// browser-safe data dir when the caller didn't set one. Without it the embedded
// daemon's config validation expands the default `~/.waved` via
// os.UserHomeDir, which fails with "$HOME is not defined" under wasm_exec.js
// and aborts start before the wallet boots.
//
// A Node caller that omits the path is told so instead, because the browser
// default cannot be created there and the alternative is an EROFS from mkdir
// well after start was accepted.
func startConfig(req js.Value) (string, error) {
	if req.IsUndefined() || req.IsNull() {
		req = js.Global().Get("Object").New()
	}

	if err := ensureDataDir(req); err != nil {
		return "", err
	}

	return stringify(req), nil
}

// externalSeedWalletStartRequest renders the typed SDK's private external-seed
// transport request. Its nested config must already select the final wallet
// data directory; unlike legacy Start, this path cannot safely guess one.
func externalSeedWalletStartRequest(req js.Value) ([]byte, error) {
	if req.IsUndefined() || req.IsNull() || req.Type() != js.TypeObject {
		req = js.Global().Get("Object").New()
	}

	cfg := req.Get("config")
	if cfg.IsUndefined() || cfg.IsNull() || cfg.Type() != js.TypeObject {
		return nil, errors.New("config must be an object")
	}
	dataDir := cfg.Get("data_dir")
	if dataDir.IsUndefined() || dataDir.IsNull() || dataDir.String() == "" {
		return nil, errors.New("config.data_dir must select the " +
			"final wallet directory")
	}

	return jsonBytes(req), nil
}

// ensureDataDir fills the browser-safe data directory or requires an explicit
// host-filesystem path under Node for legacy Start.
func ensureDataDir(cfg js.Value) error {
	if v := cfg.Get("data_dir"); !v.IsUndefined() && !v.IsNull() &&
		v.String() != "" {
		return nil
	}

	if wasmhost.UnderNode() {
		return errNodeDataDirRequired
	}

	cfg.Set("data_dir", browserDataDir)

	return nil
}

// stringify renders a JS request value as a JSON string, or "" when absent.
func stringify(v js.Value) string {
	if v.IsUndefined() || v.IsNull() {
		return ""
	}

	return js.Global().Get("JSON").Call("stringify", v).String()
}

// jsonBytes renders a JS request value as JSON request bytes, or nil when
// absent (the facade treats a nil body as the zero request).
func jsonBytes(v js.Value) []byte {
	s := stringify(v)
	if s == "" {
		return nil
	}

	return []byte(s)
}

// parse decodes a JSON response body into a JS value, mapping an empty body to
// null.
func parse(b []byte) js.Value {
	if len(b) == 0 {
		return js.Null()
	}

	return js.Global().Get("JSON").Call("parse", string(b))
}
