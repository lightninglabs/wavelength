//go:build mobile && walletdkrpc && swapruntime

package mobile

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"runtime/debug"

	"github.com/lightninglabs/wavelength/sdk/walletdk"
)

// GetInfo returns the daemon readiness snapshot as JSON (walletdk.Info).
func GetInfo() ([]byte, error) {
	client, ctx, err := activeClient()
	if err != nil {
		return nil, err
	}

	info, err := client.GetInfo(ctx)
	if err != nil {
		return nil, err
	}

	return marshal(info)
}

// CreateWallet creates or imports the embedded wallet. reqJSON decodes to
// walletdk.CreateWalletRequest; the response is walletdk.CreateWalletResult.
func CreateWallet(reqJSON []byte) ([]byte, error) {
	client, ctx, err := activeClient()
	if err != nil {
		return nil, err
	}

	var req walletdk.CreateWalletRequest
	if err := decode(reqJSON, &req); err != nil {
		return nil, err
	}

	res, err := client.CreateWallet(ctx, req)
	if err != nil {
		return nil, err
	}

	return marshal(res)
}

// UnlockWallet unlocks an existing wallet. reqJSON decodes to
// walletdk.UnlockWalletRequest; the response is walletdk.UnlockWalletResult.
func UnlockWallet(reqJSON []byte) ([]byte, error) {
	client, ctx, err := activeClient()
	if err != nil {
		return nil, err
	}

	var req walletdk.UnlockWalletRequest
	if err := decode(reqJSON, &req); err != nil {
		return nil, err
	}

	res, err := client.UnlockWallet(ctx, req)
	if err != nil {
		return nil, err
	}

	return marshal(res)
}

// Balance returns the wallet balance summary as JSON (walletdk.Balance).
func Balance() ([]byte, error) {
	client, ctx, err := activeClient()
	if err != nil {
		return nil, err
	}

	bal, err := client.Balance(ctx)
	if err != nil {
		return nil, err
	}

	return marshal(bal)
}

// Deposit allocates a fresh boarding address. reqJSON decodes to
// walletdk.DepositRequest; the response is walletdk.DepositResult.
func Deposit(reqJSON []byte) ([]byte, error) {
	client, ctx, err := activeClient()
	if err != nil {
		return nil, err
	}

	var req walletdk.DepositRequest
	if err := decode(reqJSON, &req); err != nil {
		return nil, err
	}

	res, err := client.Deposit(ctx, req)
	if err != nil {
		return nil, err
	}

	return marshal(res)
}

// Receive opens a Lightning invoice receive. reqJSON decodes to
// walletdk.ReceiveRequest; the response is walletdk.ReceiveResult.
func Receive(reqJSON []byte) ([]byte, error) {
	client, ctx, err := activeClient()
	if err != nil {
		return nil, err
	}

	var req walletdk.ReceiveRequest
	if err := decode(reqJSON, &req); err != nil {
		return nil, err
	}

	res, err := client.Receive(ctx, req)
	if err != nil {
		return nil, err
	}

	return marshal(res)
}

// PrepareSend validates and quotes an outbound payment, returning a single-use
// SendIntentID. reqJSON decodes to walletdk.PrepareSendRequest; the response is
// walletdk.PrepareSendResult.
func PrepareSend(reqJSON []byte) ([]byte, error) {
	client, ctx, err := activeClient()
	if err != nil {
		return nil, err
	}

	var req walletdk.PrepareSendRequest
	if err := decode(reqJSON, &req); err != nil {
		return nil, err
	}

	res, err := client.PrepareSend(ctx, req)
	if err != nil {
		return nil, err
	}

	return marshal(res)
}

// SendPrepared dispatches a previously prepared send. reqJSON decodes to
// walletdk.SendPreparedRequest; the response is walletdk.SendResult.
func SendPrepared(reqJSON []byte) ([]byte, error) {
	client, ctx, err := activeClient()
	if err != nil {
		return nil, err
	}

	var req walletdk.SendPreparedRequest
	if err := decode(reqJSON, &req); err != nil {
		return nil, err
	}

	res, err := client.SendPrepared(ctx, req)
	if err != nil {
		return nil, err
	}

	return marshal(res)
}

// List returns the unified wallet view (activity / vtxos / onchain). reqJSON
// decodes to walletdk.ListRequest; the response is the tagged-union
// walletdk.ListResult.
func List(reqJSON []byte) ([]byte, error) {
	client, ctx, err := activeClient()
	if err != nil {
		return nil, err
	}

	var req walletdk.ListRequest
	if err := decode(reqJSON, &req); err != nil {
		return nil, err
	}

	res, err := client.List(ctx, req)
	if err != nil {
		return nil, err
	}

	return marshal(res)
}

// Exit triggers cooperative leave or unilateral unroll for a VTXO. reqJSON
// decodes to walletdk.ExitRequest; the response is walletdk.ExitResult.
func Exit(reqJSON []byte) ([]byte, error) {
	client, ctx, err := activeClient()
	if err != nil {
		return nil, err
	}

	var req walletdk.ExitRequest
	if err := decode(reqJSON, &req); err != nil {
		return nil, err
	}

	res, err := client.Exit(ctx, req)
	if err != nil {
		return nil, err
	}

	return marshal(res)
}

// ExitStatus reports the phase of an exit job. reqJSON decodes to
// walletdk.ExitStatusRequest; the response is walletdk.ExitStatusResult.
func ExitStatus(reqJSON []byte) ([]byte, error) {
	client, ctx, err := activeClient()
	if err != nil {
		return nil, err
	}

	var req walletdk.ExitStatusRequest
	if err := decode(reqJSON, &req); err != nil {
		return nil, err
	}

	res, err := client.ExitStatus(ctx, req)
	if err != nil {
		return nil, err
	}

	return marshal(res)
}

// ExitSummary reports the wallet-wide portfolio of in-progress exits. reqJSON
// decodes to walletdk.ExitSummaryRequest (an empty object is fine); the
// response is walletdk.ExitSummaryResult.
func ExitSummary(reqJSON []byte) ([]byte, error) {
	client, ctx, err := activeClient()
	if err != nil {
		return nil, err
	}

	var req walletdk.ExitSummaryRequest
	if err := decode(reqJSON, &req); err != nil {
		return nil, err
	}

	res, err := client.ExitSummary(ctx, req)
	if err != nil {
		return nil, err
	}

	return marshal(res)
}

// GetExitPlan previews unilateral-exit readiness for a set of VTXOs. reqJSON
// decodes to walletdk.GetExitPlanRequest; the response is
// walletdk.GetExitPlanResult.
func GetExitPlan(reqJSON []byte) ([]byte, error) {
	client, ctx, err := activeClient()
	if err != nil {
		return nil, err
	}

	var req walletdk.GetExitPlanRequest
	if err := decode(reqJSON, &req); err != nil {
		return nil, err
	}

	res, err := client.GetExitPlan(ctx, req)
	if err != nil {
		return nil, err
	}

	return marshal(res)
}

// SweepWallet previews or broadcasts a backing-wallet sweep. reqJSON decodes to
// walletdk.SweepWalletRequest; the response is walletdk.SweepWalletResult.
func SweepWallet(reqJSON []byte) ([]byte, error) {
	client, ctx, err := activeClient()
	if err != nil {
		return nil, err
	}

	var req walletdk.SweepWalletRequest
	if err := decode(reqJSON, &req); err != nil {
		return nil, err
	}

	res, err := client.SweepWallet(ctx, req)
	if err != nil {
		return nil, err
	}

	return marshal(res)
}

// Status returns wallet readiness, balance, and pending counts as JSON
// (walletdk.Status).
func Status() ([]byte, error) {
	client, ctx, err := activeClient()
	if err != nil {
		return nil, err
	}

	status, err := client.Status(ctx)
	if err != nil {
		return nil, err
	}

	return marshal(status)
}

// Subscription is a gomobile-safe, pull-based handle over a wallet activity
// stream. The host calls Next in a loop on a background thread (mapping cleanly
// to a Kotlin Flow or Swift AsyncStream) and Close to stop early; no
// host-implemented callback interface is required.
type Subscription struct {
	updates <-chan walletdk.Entry
	errs    <-chan error
	ctx     context.Context
	cancel  context.CancelFunc
}

// Subscribe opens a wallet activity stream and returns a pull handle. reqJSON
// decodes to walletdk.SubscribeRequest (empty is allowed). The subscription is
// cancelled by Close, or by Stop when the daemon shuts down.
func Subscribe(reqJSON []byte) (*Subscription, error) {
	client, parentCtx, err := activeClient()
	if err != nil {
		return nil, err
	}

	var req walletdk.SubscribeRequest
	if err := decode(reqJSON, &req); err != nil {
		return nil, err
	}

	// Derive a cancellable context from the wrapper call context so both
	// Close and Stop terminate a blocked Next.
	ctx, cancel := context.WithCancel(parentCtx)

	updates, errs, err := client.Subscribe(ctx, req)
	if err != nil {
		cancel()

		return nil, err
	}

	return &Subscription{
		updates: updates,
		errs:    errs,
		ctx:     ctx,
		cancel:  cancel,
	}, nil
}

// Next blocks until the next activity entry is available and returns it as
// JSON. It returns io.EOF when the stream ends cleanly, or the underlying
// error otherwise; either is terminal. A panic is recovered into the returned
// error so it never crosses the gomobile boundary.
func (s *Subscription) Next() (b []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("walletdk mobile panic: %v\n%s", r,
				debug.Stack())
		}
	}()

	entry, ok := <-s.updates
	if !ok {
		// The updates channel has closed. A terminal error (if any) is
		// buffered on errs; a closed errs reads as nil, which we report
		// as a clean EOF so the host can tell a normal end from a
		// failure.
		streamErr := <-s.errs

		// A self-initiated Close or Stop cancels s.ctx, which surfaces
		// upstream as a wrapped context.Canceled on errs. Report that
		// as a clean EOF too, so a host that tears down its own stream
		// (the app-suspend path) ends its loop without a spurious
		// error, as the doc promises.
		if streamErr != nil && s.ctx.Err() == nil {
			return nil, streamErr
		}

		return nil, io.EOF
	}

	return marshal(entry)
}

// Close cancels the subscription and unblocks any in-flight Next. It is
// idempotent and safe to call from any thread.
func (s *Subscription) Close() error {
	s.cancel()

	return nil
}

// decode unmarshals a JSON request body with a uniform error wrap. A nil or
// empty body decodes to the zero request.
func decode(b []byte, v any) error {
	if len(b) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}

	return nil
}
