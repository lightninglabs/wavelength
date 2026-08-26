//go:build mobile && wavewalletrpc && swapruntime

package mobile

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"runtime/debug"
	"time"

	"github.com/lightninglabs/wavelength/sdk/wavewalletdk"
)

const (
	// defaultReadTimeout bounds read-only wallet calls whose foreign host
	// cannot supply context.Context through gomobile. Reads are safe to
	// repeat after a timeout and must not inherit the daemon's entire
	// lifetime.
	defaultReadTimeout = 10 * time.Second

	// defaultReceiveTimeout bounds invoice creation when a mobile host
	// omits TimeoutSeconds. A timed-out request has an uncertain outcome:
	// callers must reconcile Activity before deliberately creating another
	// invoice.
	defaultReceiveTimeout = 20 * time.Second

	// maxReceiveTimeout prevents malformed host input from restoring the
	// effectively unbounded behavior this mobile-only request field avoids.
	maxReceiveTimeout = 5 * time.Minute

	// receiveUncertainErrorPrefix is stable binding ABI. It lets a foreign
	// host distinguish a canceled receive with an uncertain outcome from a
	// rejected request without depending on context-specific error text.
	receiveUncertainErrorPrefix = "receive outcome uncertain; reconcile " +
		"Activity before retrying"
)

// readContext derives the bounded context used by safe, repeatable mobile
// reads. The daemon-lifetime parent still cancels it immediately during Stop.
func readContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, defaultReadTimeout)
}

// receiveError preserves ordinary receive errors while marking cancellation by
// either the binding-owned deadline or Stop as an uncertain outcome.
func receiveError(callCtx context.Context, err error) error {
	if err == nil || callCtx.Err() == nil {
		return err
	}

	return fmt.Errorf("%s: %w", receiveUncertainErrorPrefix, err)
}

// mobileReceiveRequest extends the SDK receive DTO with a mobile-only request
// deadline. The extra JSON field is backwards-compatible with older bindings,
// whose encoding/json decoder ignores it.
type mobileReceiveRequest struct {
	AmountSat      uint64
	Memo           string
	TimeoutSeconds int64
}

// decodeReceiveRequest validates the mobile-only deadline and converts the
// wire request into the SDK DTO. Zero selects the bounded default so older
// hosts gain a deadline when they update only the native framework.
func decodeReceiveRequest(reqJSON []byte) (wavewalletdk.ReceiveRequest,
	time.Duration, error) {

	var mobileReq mobileReceiveRequest
	if err := decode(reqJSON, &mobileReq); err != nil {
		return wavewalletdk.ReceiveRequest{}, 0, err
	}

	timeout := defaultReceiveTimeout
	if mobileReq.TimeoutSeconds < 0 {
		return wavewalletdk.ReceiveRequest{}, 0,
			fmt.Errorf("receive timeout seconds must not be " +
				"negative")
	}
	maxTimeoutSeconds := int64(maxReceiveTimeout / time.Second)
	if mobileReq.TimeoutSeconds > maxTimeoutSeconds {
		return wavewalletdk.ReceiveRequest{}, 0, fmt.Errorf("receive "+
			"timeout seconds %d exceeds maximum %d",
			mobileReq.TimeoutSeconds, maxTimeoutSeconds)
	}
	if mobileReq.TimeoutSeconds > 0 {
		timeout = time.Duration(mobileReq.TimeoutSeconds) * time.Second
	}

	return wavewalletdk.ReceiveRequest{
		AmountSat: mobileReq.AmountSat,
		Memo:      mobileReq.Memo,
	}, timeout, nil
}

// GetInfo returns the daemon readiness snapshot as JSON (wavewalletdk.Info).
func GetInfo() ([]byte, error) {
	client, parentCtx, err := activeClient()
	if err != nil {
		return nil, err
	}
	ctx, cancel := readContext(parentCtx)
	defer cancel()

	info, err := client.GetInfo(ctx)
	if err != nil {
		return nil, err
	}

	return marshal(info)
}

// CreateWallet creates or imports the embedded wallet. reqJSON decodes to
// wavewalletdk.CreateWalletRequest; the response is
// wavewalletdk.CreateWalletResult.
func CreateWallet(reqJSON []byte) ([]byte, error) {
	client, ctx, err := activeClient()
	if err != nil {
		return nil, err
	}

	var req wavewalletdk.CreateWalletRequest
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
// wavewalletdk.UnlockWalletRequest; the response is
// wavewalletdk.UnlockWalletResult.
func UnlockWallet(reqJSON []byte) ([]byte, error) {
	client, ctx, err := activeClient()
	if err != nil {
		return nil, err
	}

	var req wavewalletdk.UnlockWalletRequest
	if err := decode(reqJSON, &req); err != nil {
		return nil, err
	}

	res, err := client.UnlockWallet(ctx, req)
	if err != nil {
		return nil, err
	}

	return marshal(res)
}

// Balance returns the wallet balance summary as JSON (wavewalletdk.Balance).
func Balance() ([]byte, error) {
	client, parentCtx, err := activeClient()
	if err != nil {
		return nil, err
	}
	ctx, cancel := readContext(parentCtx)
	defer cancel()

	bal, err := client.Balance(ctx)
	if err != nil {
		return nil, err
	}

	return marshal(bal)
}

// Deposit allocates a fresh boarding address. reqJSON decodes to
// wavewalletdk.DepositRequest; the response is wavewalletdk.DepositResult.
func Deposit(reqJSON []byte) ([]byte, error) {
	client, ctx, err := activeClient()
	if err != nil {
		return nil, err
	}

	var req wavewalletdk.DepositRequest
	if err := decode(reqJSON, &req); err != nil {
		return nil, err
	}

	res, err := client.Deposit(ctx, req)
	if err != nil {
		return nil, err
	}

	return marshal(res)
}

// Receive opens a Lightning invoice receive. reqJSON accepts AmountSat, Memo,
// and an optional mobile-only TimeoutSeconds; the response is
// wavewalletdk.ReceiveResult. When the deadline expires, the outcome may be
// uncertain, so the host must reconcile Activity before retrying deliberately.
func Receive(reqJSON []byte) ([]byte, error) {
	client, parentCtx, err := activeClient()
	if err != nil {
		return nil, err
	}

	req, timeout, err := decodeReceiveRequest(reqJSON)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	res, err := client.Receive(ctx, req)
	if err != nil {
		return nil, receiveError(ctx, err)
	}

	return marshal(res)
}

// PrepareSend validates and quotes an outbound payment, returning a single-use
// SendIntentID. reqJSON decodes to wavewalletdk.PrepareSendRequest; the
// response is wavewalletdk.PrepareSendResult.
func PrepareSend(reqJSON []byte) ([]byte, error) {
	client, ctx, err := activeClient()
	if err != nil {
		return nil, err
	}

	var req wavewalletdk.PrepareSendRequest
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
// wavewalletdk.SendPreparedRequest; the response is wavewalletdk.SendResult.
func SendPrepared(reqJSON []byte) ([]byte, error) {
	client, ctx, err := activeClient()
	if err != nil {
		return nil, err
	}

	var req wavewalletdk.SendPreparedRequest
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
// decodes to wavewalletdk.ListRequest; the response is the tagged-union
// wavewalletdk.ListResult.
func List(reqJSON []byte) ([]byte, error) {
	client, parentCtx, err := activeClient()
	if err != nil {
		return nil, err
	}
	ctx, cancel := readContext(parentCtx)
	defer cancel()

	var req wavewalletdk.ListRequest
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
// decodes to wavewalletdk.ExitRequest; the response is wavewalletdk.ExitResult.
func Exit(reqJSON []byte) ([]byte, error) {
	client, ctx, err := activeClient()
	if err != nil {
		return nil, err
	}

	var req wavewalletdk.ExitRequest
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
// wavewalletdk.ExitStatusRequest; the response is
// wavewalletdk.ExitStatusResult.
func ExitStatus(reqJSON []byte) ([]byte, error) {
	client, parentCtx, err := activeClient()
	if err != nil {
		return nil, err
	}
	ctx, cancel := readContext(parentCtx)
	defer cancel()

	var req wavewalletdk.ExitStatusRequest
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
// decodes to wavewalletdk.ExitSummaryRequest (an empty object is fine); the
// response is wavewalletdk.ExitSummaryResult.
func ExitSummary(reqJSON []byte) ([]byte, error) {
	client, parentCtx, err := activeClient()
	if err != nil {
		return nil, err
	}
	ctx, cancel := readContext(parentCtx)
	defer cancel()

	var req wavewalletdk.ExitSummaryRequest
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
// decodes to wavewalletdk.GetExitPlanRequest; the response is
// wavewalletdk.GetExitPlanResult.
func GetExitPlan(reqJSON []byte) ([]byte, error) {
	client, parentCtx, err := activeClient()
	if err != nil {
		return nil, err
	}
	ctx, cancel := readContext(parentCtx)
	defer cancel()

	var req wavewalletdk.GetExitPlanRequest
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
// wavewalletdk.SweepWalletRequest; the response is
// wavewalletdk.SweepWalletResult.
func SweepWallet(reqJSON []byte) ([]byte, error) {
	client, ctx, err := activeClient()
	if err != nil {
		return nil, err
	}

	var req wavewalletdk.SweepWalletRequest
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
// (wavewalletdk.Status).
func Status() ([]byte, error) {
	client, parentCtx, err := activeClient()
	if err != nil {
		return nil, err
	}
	ctx, cancel := readContext(parentCtx)
	defer cancel()

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
	updates <-chan wavewalletdk.Entry
	errs    <-chan error
	ctx     context.Context
	cancel  context.CancelFunc
}

// Subscribe opens a wallet activity stream and returns a pull handle. reqJSON
// decodes to wavewalletdk.SubscribeRequest (empty is allowed). The subscription
// is cancelled by Close, or by Stop when the daemon shuts down.
func Subscribe(reqJSON []byte) (*Subscription, error) {
	client, parentCtx, err := activeClient()
	if err != nil {
		return nil, err
	}

	var req wavewalletdk.SubscribeRequest
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
			err = fmt.Errorf("wavewalletdk mobile panic: %v\n%s", r,
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
