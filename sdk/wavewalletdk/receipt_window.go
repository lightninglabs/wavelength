package wavewalletdk

import (
	"context"
	"errors"

	"github.com/lightninglabs/wavelength/waved"
)

// ErrReceiptWindowUnavailable rejects host mode control on remote handles.
var ErrReceiptWindowUnavailable = errors.New("receipt windows require an " +
	"embedded durable runtime")

// ReceiptWindowRequest names a unique invocation and one enrolled invoice.
type ReceiptWindowRequest = waved.ReceiptWindowRequest

// ReceiptWindowResult contains local source proof, never a safe-to-suspend
// flag.
type ReceiptWindowResult = waved.ReceiptWindowResult

// SetHostForeground cancels and joins an active window before resuming normal
// ingress. False pauses ingress; domain and swap workers remain daemon-owned.
func (c *Client) SetHostForeground(ctx context.Context, foreground bool) error {
	if c == nil || c.setHostForeground == nil {
		return ErrReceiptWindowUnavailable
	}
	callCtx, release, err := c.beginLocalCall(ctx)
	if err != nil {
		return err
	}
	defer release()

	return c.setHostForeground(callCtx, foreground)
}

// RunReceiptWindow retains the daemon through a cooperative bounded attempt.
// The host must first pause ingress and must never issue state-creating wallet
// calls concurrently. Only a matched authorized payment hint permits an alert.
func (c *Client) RunReceiptWindow(ctx context.Context,
	request ReceiptWindowRequest) (ReceiptWindowResult, error) {

	if c == nil || c.runReceiptWindow == nil {
		return ReceiptWindowResult{}, ErrReceiptWindowUnavailable
	}
	callCtx, release, err := c.beginLocalCall(ctx)
	if err != nil {
		return ReceiptWindowResult{}, err
	}
	defer release()

	return c.runReceiptWindow(callCtx, request)
}

// CancelReceiptWindow cancels one unique invocation, including early expiry.
// It does not release the original call's retained daemon ownership.
func (c *Client) CancelReceiptWindow(ctx context.Context, id string) error {
	if c == nil || c.cancelReceiptWindow == nil {
		return ErrReceiptWindowUnavailable
	}
	callCtx, release, err := c.beginLocalCall(ctx)
	if err != nil {
		return err
	}
	defer release()

	return c.cancelReceiptWindow(callCtx, id)
}

// RunMaintenanceWindow retains the daemon through a bounded current-tip and
// native mailbox attempt. It shares receipt-window cancellation by unique ID.
// This returns no payment proof and cannot authorize a payment notification.
func (c *Client) RunMaintenanceWindow(ctx context.Context, id string) (
	ReceiptWindowResult, error) {

	if c == nil || c.runMaintenanceWindow == nil {
		return ReceiptWindowResult{}, ErrReceiptWindowUnavailable
	}
	callCtx, release, err := c.beginLocalCall(ctx)
	if err != nil {
		return ReceiptWindowResult{}, err
	}
	defer release()

	return c.runMaintenanceWindow(callCtx, id)
}
