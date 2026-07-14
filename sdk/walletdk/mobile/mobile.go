//go:build mobile && walletdkrpc && swapruntime

package mobile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/lightninglabs/wavelength/sdk/walletdk"
)

// startTimeout bounds how long Start will wait for the embedded daemon's gRPC
// transport to report readiness before giving up. The daemon lifetime itself
// is owned by walletdk's internal runCtx, so this deadline only cancels
// dialing, never the running daemon.
const startTimeout = 90 * time.Second

// lifecycleStatus is the explicit state of the embedded daemon. A plain CAS on
// a started/stopped bit cannot express the in-between "starting" and "stopping"
// states, and that gap is exactly where a Stop racing an in-progress Start
// would strand a live daemon under a guard that reads "stopped". The four-state
// machine plus the stored startCancel closes that race.
type lifecycleStatus int32

const (
	statusStopped lifecycleStatus = iota
	statusStarting
	statusStarted
	statusStopping
)

// state holds the package-level singleton. gomobile exposes free functions, so
// the client and its lifecycle live here rather than on a handle the host would
// carry across the boundary. All fields are guarded by mu.
var state = struct {
	mu     sync.Mutex
	status lifecycleStatus

	// gen is bumped on every status transition that begins or abandons a
	// boot, so a Start that finishes after a racing Stop (and a possible
	// subsequent Start) can tell it no longer owns the starting state and
	// must not publish its client or reset the guard.
	gen uint64

	// client is the live wallet handle, non-nil only in statusStarted.
	client *walletdk.Client

	// callCtx is cancelled by Stop so in-flight RPCs and subscriptions
	// unwind promptly. It is the wrapper-owned context the mobile API uses
	// in place of a per-call context.Context.
	callCtx    context.Context
	callCancel context.CancelFunc

	// startCancel cancels an in-progress walletdk.Start so a racing Stop
	// can abort a boot rather than orphan the daemon it would produce.
	startCancel context.CancelFunc
}{}

// Start boots the embedded waved wallet daemon from a JSON config and blocks
// until the private gRPC transport is serving, or returns an error if the boot
// fails. It is the gomobile-safe equivalent of walletdk.Start.
//
// Start may block for several seconds, so the host must call it off the main
// thread (e.g. Kotlin withContext(Dispatchers.IO) or a Swift background Task);
// gomobile maps the returned error to a thrown exception. It is singleton
// guarded: a second Start before Stop returns an error rather than booting a
// second daemon. A Stop that races an in-progress Start cancels the boot, and
// Start then tears down any client it produced instead of publishing it. A
// panic in the boot path is recovered into the returned error so it never
// crosses the gomobile boundary as a process kill.
func Start(cfgJSON string) (err error) {
	state.mu.Lock()
	if state.status != statusStopped {
		state.mu.Unlock()

		return errors.New("walletdk mobile already started")
	}
	state.status = statusStarting
	state.gen++
	gen := state.gen

	startCtx, startCancel := context.WithTimeout(
		context.Background(), startTimeout,
	)
	state.startCancel = startCancel
	state.mu.Unlock()

	// On any failure or panic, cancel the boot context and return to
	// stopped, but only if this Start still owns the starting state: a
	// racing Stop (and a later Start) may have moved on, and clobbering
	// their state would defeat the guard.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("walletdk mobile panic: %v\n%s", r,
				debug.Stack())
		}
		if err == nil {
			return
		}

		startCancel()

		state.mu.Lock()
		if state.status == statusStarting && state.gen == gen {
			state.status = statusStopped
			state.startCancel = nil
		}
		state.mu.Unlock()
	}()

	cfg, err := parseConfig(cfgJSON)
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	client, err := walletdk.Start(startCtx, cfg)
	if err != nil {
		return fmt.Errorf("start embedded wallet: %w", err)
	}

	callCtx, callCancel := context.WithCancel(context.Background())

	state.mu.Lock()
	// A Stop raced this boot: it cancelled startCtx and moved us out of the
	// starting state. Tear the just-booted client down rather than publish
	// it under a reset guard.
	if state.status != statusStarting || state.gen != gen {
		state.mu.Unlock()
		_ = client.Stop()

		return errors.New("walletdk mobile start cancelled by Stop")
	}
	state.client = client
	state.callCtx = callCtx
	state.callCancel = callCancel
	state.startCancel = nil
	state.status = statusStarted
	state.mu.Unlock()

	return nil
}

// Stop tears down the embedded daemon and releases the private transport. It is
// idempotent: calling it when stopped is a no-op that returns nil. If a Start
// is still in progress, Stop cancels the boot and returns; that Start then
// stops any client it produced. Otherwise Stop fully tears down the live client
// (cancelling the wrapper context unblocks any in-flight Subscription.Next)
// before resetting to stopped, so a Start that immediately follows cannot race
// a half-finished shutdown. After Stop the singleton resets so a host can Start
// again, e.g. after the OS suspends and resumes the app.
func Stop() error {
	state.mu.Lock()
	switch state.status {
	case statusStopped, statusStopping:
		// Already stopped, or another Stop owns the teardown.
		state.mu.Unlock()

		return nil

	case statusStarting:
		// Cancel the in-progress boot; that Start observes the status /
		// gen change, stops any client it produced, and returns.
		if state.startCancel != nil {
			state.startCancel()
			state.startCancel = nil
		}
		state.status = statusStopped
		state.gen++
		state.mu.Unlock()

		return nil
	}

	// statusStarted: take ownership of the live client and tear it down.
	state.status = statusStopping
	state.gen++
	client := state.client
	cancel := state.callCancel
	state.client = nil
	state.callCtx = nil
	state.callCancel = nil
	state.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	var err error
	if client != nil {
		err = client.Stop()
	}

	state.mu.Lock()
	state.status = statusStopped
	state.mu.Unlock()

	return err
}

// activeClient returns the live client and the wrapper-owned call context, or
// an error when the daemon is not running. Callers must not retain the context
// past the call; it is cancelled by Stop.
func activeClient() (*walletdk.Client, context.Context, error) {
	state.mu.Lock()
	client := state.client
	ctx := state.callCtx
	state.mu.Unlock()

	if client == nil || ctx == nil {
		return nil, nil, errors.New("walletdk mobile not started")
	}

	return client, ctx, nil
}

// lifecycleActive reports whether a daemon lifecycle is in progress, i.e. a
// Start has begun (including the up-to-startTimeout boot window) and Stop has
// not yet completed. It backs IsRunning so a host sees "running" across the
// whole boot, not only once gRPC is serving.
func lifecycleActive() bool {
	state.mu.Lock()
	defer state.mu.Unlock()

	return state.status == statusStarting || state.status == statusStarted
}

// marshal is a small helper so every verb returns JSON bytes with a uniform
// error wrap.
func marshal(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode response: %w", err)
	}

	return b, nil
}
