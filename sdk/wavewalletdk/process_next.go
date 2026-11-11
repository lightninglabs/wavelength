package wavewalletdk

import (
	"context"

	"github.com/lightninglabs/wavelength/waved"
)

// IngressStepResult separates remote intake from domain and effect completion.
type IngressStepResult = waved.IngressStepResult

// ProcessNext retains the embedded daemon through one bounded ingress batch.
// The host first pauses foreground ingress and serializes state-creating calls.
// Use a fresh id per invocation; CancelReceiptWindow(id) cancels early expiry.
// Only a successful nonempty result proves recoverable handoff and remote ACK.
// The route policy rejects unproven work before dispatch. This method does not
// run receipt lookup or expiry maintenance and cannot authorize a payment
// alert.
func (c *Client) ProcessNext(ctx context.Context, id string) (IngressStepResult,
	error) {

	if c == nil || c.processNext == nil {
		return IngressStepResult{}, ErrReceiptWindowUnavailable
	}
	callCtx, release, err := c.beginLocalCall(ctx)
	if err != nil {
		return IngressStepResult{}, err
	}
	defer release()

	return c.processNext(callCtx, id)
}
