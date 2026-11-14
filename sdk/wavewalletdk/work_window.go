package wavewalletdk

import (
	"context"

	"github.com/lightninglabs/wavelength/waved"
)

// WorkWindowRequest names a unique host invocation and at most 64 existing
// local receive sources. It carries no payment-presentation consent.
type WorkWindowRequest = waved.WorkWindowRequest

// RunWorkWindow re-arms existing receive work alongside restricted mailbox
// intake. It returns no receipt proof. The host must own the selected wallet,
// pause foreground ingress and retain this call through final native access.
func (c *Client) RunWorkWindow(ctx context.Context, request WorkWindowRequest) (
	ReceiptWindowResult, error) {

	if c == nil || c.runWorkWindow == nil {
		return ReceiptWindowResult{}, ErrReceiptWindowUnavailable
	}
	callCtx, release, err := c.beginLocalCall(ctx)
	if err != nil {
		return ReceiptWindowResult{}, err
	}
	defer release()

	return c.runWorkWindow(callCtx, request)
}
