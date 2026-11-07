//go:build mobile && wavewalletrpc && swapruntime

package mobile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/lightninglabs/wavelength/sdk/wavewalletdk"
)

// startTimeout bounds how long either startup path waits for the embedded
// daemon's gRPC transport to report readiness. Wallet opening and recovery use
// a separate lifecycle context, while the running daemon is owned by
// wavewalletdk's internal runCtx.
const startTimeout = 90 * time.Second

// stopTimeout bounds a host's wait while the singleton retains cleanup.
const stopTimeout = 5 * time.Second

// mobileShutdown belongs to one daemon generation. Closing done publishes err
// and proves the singleton has released that generation's startup or cleanup.
type mobileShutdown struct {
	done chan struct{}
	err  error
}

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

	// gen identifies each admitted boot. Stop retains that generation until
	// startup or cleanup has returned, so a later Start cannot overlap it.
	gen uint64

	// client retains the wallet handle through statusStopping.
	client *wavewalletdk.Client

	// callCtx is cancelled by Stop so in-flight RPCs and subscriptions
	// unwind promptly. It is the wrapper-owned context the mobile API uses
	// in place of a per-call context.Context.
	callCtx    context.Context
	callCancel context.CancelFunc

	// startCancel cancels an in-progress wavewalletdk.Start so a racing
	// Stop can abort a boot rather than orphan the daemon it would produce.
	startCancel context.CancelFunc
	shutdown    *mobileShutdown
}{}

// Start boots the embedded waved wallet daemon from a JSON config and blocks
// until the private gRPC transport is serving, or returns an error if the boot
// fails. It is the gomobile-safe equivalent of wavewalletdk.Start.
//
// Start may block for several seconds, so the host must call it off the main
// thread (e.g. Kotlin withContext(Dispatchers.IO) or a Swift background Task);
// gomobile maps the returned error to a thrown exception. It is singleton
// guarded: a second Start before Stop returns an error rather than booting a
// second daemon. A Stop that races an in-progress Start cancels the boot, and
// Start then tears down any client it produced instead of publishing it. A
// panic in the boot path is recovered into the returned error so it never
// crosses the gomobile boundary as a process kill.
func Start(cfgJSON string) error {
	return startEmbedded(func(startCtx, _ context.Context) (
		*wavewalletdk.Client, error) {

		cfg, err := parseConfig(cfgJSON)
		if err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}

		client, err := wavewalletdk.Start(
			startCtx, cfg, mobileStartOptions(cfg)...,
		)
		if err != nil {
			return nil, fmt.Errorf("start embedded wallet: %w", err)
		}

		return client, nil
	})
}

// startEmbedded owns the singleton lifecycle shared by Start and the additive
// account-aware startup paths. startCtx has the bounded daemon-readiness
// deadline. operationCtx has no deadline and remains valid for wallet opening,
// recovery, and later mobile calls until Stop cancels it. The supplied function
// must either return a fully initialized client or clean up any client it
// started before returning an error.
func startEmbedded(
	start func(context.Context, context.Context) (*wavewalletdk.Client, error)) (
	err error) {

	state.mu.Lock()
	if state.status != statusStopped {
		state.mu.Unlock()

		return errors.New("wavewalletdk mobile already started")
	}
	state.status = statusStarting
	state.gen++
	gen := state.gen
	shutdown := &mobileShutdown{done: make(chan struct{})}
	state.shutdown = shutdown

	operationCtx, operationCancel := context.WithCancel(
		context.Background(),
	)
	startCtx, bootCancel := context.WithTimeout(operationCtx, startTimeout)
	startCancel := func() {
		bootCancel()
		operationCancel()
	}
	state.startCancel = startCancel
	state.mu.Unlock()

	// Failed startup retains its generation until the starter has joined
	// every unpublished resource. Only then may Stop or a new Start
	// proceed.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("wavewalletdk mobile panic: %v\n%s", r,
				debug.Stack())
		}
		if err == nil {
			return
		}

		startCancel()

		state.mu.Lock()
		if state.gen == gen {
			state.status = statusStopped
			state.startCancel = nil
			close(shutdown.done)
		}
		state.mu.Unlock()
	}()

	client, err := start(startCtx, operationCtx)
	if err != nil {
		return err
	}

	state.mu.Lock()
	// A Stop raced this boot: it cancelled startCtx and moved us out of the
	// starting state. Tear the just-booted client down rather than publish
	// it under a reset guard.
	if state.status != statusStarting || state.gen != gen {
		state.mu.Unlock()
		startCancel()
		shutdown.err = client.CloseContext(context.Background())

		return errors.New("wavewalletdk mobile start cancelled by Stop")
	}
	state.client = client
	// The lifecycle context intentionally spans later RPC calls.
	//nolint:fatcontext
	state.callCtx = operationCtx
	state.callCancel = operationCancel
	state.startCancel = nil
	state.status = statusStarted
	state.mu.Unlock()

	// The readiness deadline is no longer needed. Keep only the Stop-owned
	// operation context for wallet calls and subscriptions.
	bootCancel()

	return nil
}

// Stop cancels startup or the running daemon and waits up to five seconds.
// A timeout retains statusStopping and the client until cleanup actually ends.
// Start rejects during that interval. Repeated Stop calls re-wait the same
// cleanup; successful completion releases the singleton for a new Start.
func Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()

	return stopWithContext(ctx)
}

// stopWithContext bounds only the caller wait, never the cleanup owner. Startup
// retains its own generation until its unpublished client has also been joined.
func stopWithContext(ctx context.Context) error {
	state.mu.Lock()
	switch state.status {
	case statusStopped:
		state.mu.Unlock()

		return nil

	case statusStarting:
		state.status = statusStopping
		state.startCancel()

	case statusStarted:
		state.status = statusStopping
		client := state.client
		shutdown := state.shutdown
		if state.callCancel != nil {
			state.callCancel()
		}
		//nolint:gosec,contextcheck // The singleton owns cleanup.
		go func() {
			shutdown.err = client.CloseContext(context.Background())
			state.mu.Lock()
			state.client = nil
			//nolint:fatcontext // Release the joined owner.
			state.callCtx = nil
			state.callCancel = nil
			state.status = statusStopped
			close(shutdown.done)
			state.mu.Unlock()
		}()
	}
	shutdown := state.shutdown
	state.mu.Unlock()

	select {
	case <-shutdown.done:
		return shutdown.err

	default:
	}
	select {
	case <-shutdown.done:
		return shutdown.err

	case <-ctx.Done():
		return ctx.Err()
	}
}

// activeClient returns the live client and the wrapper-owned call context, or
// an error when the daemon is not running. Callers must not retain the context
// past the call; it is cancelled by Stop.
func activeClient() (*wavewalletdk.Client, context.Context, error) {
	state.mu.Lock()
	client := state.client
	ctx := state.callCtx
	running := state.status == statusStarted
	state.mu.Unlock()

	if !running || client == nil || ctx == nil {
		return nil, nil, errors.New("wavewalletdk mobile not started")
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
