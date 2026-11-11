package waved

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lightninglabs/wavelength/serverconn"
)

// IngressStepResult reports one bounded intake attempt, not domain completion.
// A successful nonempty step has committed recoverable handoffs and their
// remote ACK. Actor turns and external effects may still be pending.
type IngressStepResult struct {
	Batches         uint32 `json:"batches"`
	MailboxEmpty    bool   `json:"mailbox_empty"`
	NeedsForeground bool   `json:"needs_foreground"`
}

// processNext shares window exclusion and cancellation without receipt lookup,
// maintenance, polling sleeps or a second batch. Its unique ID lets OS expiry
// cancel this invocation through CancelReceiptWindow without stopping workers.
//
//nolint:contextcheck // Link daemon shutdown as well as caller cancellation.
func (c *receiptWindowController) processNext(ctx context.Context, id string) (
	IngressStepResult, error) {

	var result IngressStepResult
	if err := ctx.Err(); err != nil {
		return result, err
	}
	stepCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	unlink := context.AfterFunc(c.root, cancel)
	defer unlink()
	c.stateMu.Lock()
	if c.foreground || c.transitions != 0 || c.activeID != "" {
		c.stateMu.Unlock()
		result.NeedsForeground = true

		return result, nil
	}
	if c.lastCanceledID == id {
		cancel()
	}
	c.activeID, c.cancel = id, cancel
	c.stateMu.Unlock()
	defer func() {
		c.stateMu.Lock()
		c.activeID, c.cancel = "", nil
		c.stateMu.Unlock()
	}()
	c.modeMu.Lock()
	defer c.modeMu.Unlock()

	pumped, err := c.ingress.PumpIngressWithPolicy(
		stepCtx, 1, receiptIngressPolicy(),
	)
	result.Batches = pumped.Batches
	result.MailboxEmpty = pumped.MailboxEmpty
	if errors.Is(err, serverconn.ErrIngressNeedsForeground) {
		result.NeedsForeground = true

		return result, nil
	}

	return result, err
}

// ProcessNext attempts at most one remote batch under a retained host lease.
// The host first pauses ingress with SetHostForeground(false), serializes
// state-creating wallet calls, and uses a unique invocation ID. Cancellation
// does not roll back work already handed to a durable actor. An empty result
// does not prove idle, awaiting-peer, completed payment or safe suspension.
func (s *Server) ProcessNext(ctx context.Context, id string) (IngressStepResult,
	error) {

	if id == "" || len(id) > 128 {
		return IngressStepResult{}, fmt.Errorf("invalid ingress step " +
			"ID")
	}
	controller, err := s.receiptController(ctx)
	if err != nil {
		return IngressStepResult{}, err
	}

	return controller.processNext(ctx, id)
}
