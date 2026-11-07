package waved

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/lightninglabs/wavelength/rpc/oorpb"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	"github.com/lightninglabs/wavelength/serverconn"
)

// ReceiptWindowRequest selects one already-enrolled invoice and one unique
// host invocation. It cannot create an invoice or unlock a cold wallet.
type ReceiptWindowRequest struct {
	WindowID         string                    `json:"window_id"`
	Reference        LightningReceiptReference `json:"reference"`
	CreatedAfterUnix int64                     `json:"created_after_unix"`
}

// ReceiptWindowResult reports local source proof and why a bounded attempt
// yielded. No field asserts global reconciliation or safe suspension.
type ReceiptWindowResult struct {
	Receipts        []LightningReceipt `json:"receipts"`
	Batches         uint32             `json:"batches"`
	NeedsForeground bool               `json:"needs_foreground"`
	Interrupted     bool               `json:"interrupted"`
	BudgetExhausted bool               `json:"budget_exhausted"`
}

// receiptIngress keeps mode ownership independently testable from transport.
type receiptIngress interface {
	// StartIngress starts polling with the supplied lifetime.
	StartIngress(context.Context) error

	// PauseIngress joins the current intake owner.
	PauseIngress()

	// PumpIngressWithPolicy refuses unsupported work before dispatch.
	PumpIngressWithPolicy(context.Context, uint32,
		serverconn.IngressPumpPolicy) (
		serverconn.IngressPumpResult,
		error,
	)
}

// receiptWindowController serializes ingress modes while retaining all daemon
// and domain owners. stateMu admits/cancels a window before modeMu can block.
type receiptWindowController struct {
	modeMu           sync.Mutex
	stateMu          sync.Mutex
	foreground       bool
	transitions      int
	activeID         string
	cancel           context.CancelFunc
	foregroundCancel context.CancelFunc
	lastCanceledID   string
	ingress          receiptIngress
	// connected publishes mode changes while modeMu owns intake ordering.
	connected func(bool)
	//nolint:containedctx // Daemon owns foreground ingress, not the caller.
	root context.Context
	read func(context.Context, LightningReceiptRequest) (
		[]LightningReceipt, error)
	resume func(context.Context, LightningReceiptReference) error
}

// setForeground cancels a reserved window before joining its mode lease.
// A caller deadline may cancel startup, but never the returned foreground run.
//
//nolint:contextcheck // Foreground belongs to the daemon after caller return.
func (c *receiptWindowController) setForeground(ctx context.Context,
	foreground bool) error {

	if err := ctx.Err(); err != nil {
		return err
	}
	c.stateMu.Lock()
	c.transitions++
	if c.cancel != nil {
		c.cancel()
	}
	c.stateMu.Unlock()
	c.modeMu.Lock()
	defer c.modeMu.Unlock()
	c.connected(false)
	if c.foregroundCancel != nil {
		c.foregroundCancel()
		c.foregroundCancel = nil
	}
	c.ingress.PauseIngress()
	started := false
	defer func() {
		c.stateMu.Lock()
		c.foreground = started
		c.transitions--
		c.stateMu.Unlock()
		if !started {
			c.connected(false)
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !foreground {
		return nil
	}
	// Keep caller cancellation linked only until synchronous startup ends.
	// Join an already-running cancellation callback before deciding
	// success.
	runCtx, cancel := context.WithCancel(c.root)
	cancelDone := make(chan struct{})
	unlink := context.AfterFunc(ctx, func() {
		cancel()
		close(cancelDone)
	})
	// Publish before startup so an immediate incompatibility callback
	// can set false without being overwritten after StartIngress returns.
	c.connected(true)
	err := c.ingress.StartIngress(runCtx)
	if !unlink() {
		<-cancelDone
	}
	if err != nil || ctx.Err() != nil || runCtx.Err() != nil {
		cancel()
		c.ingress.PauseIngress()

		return errors.Join(err, ctx.Err(), runCtx.Err())
	}
	c.foregroundCancel = cancel
	started = true

	return nil
}

// cancelWindow remembers the last expiry even when it wins before admission.
// Hosts use unique IDs and serialize their background notification callbacks.
func (c *receiptWindowController) cancelWindow(id string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.lastCanceledID = id
	if c.activeID == id && c.cancel != nil {
		c.cancel()
	}
}

// run reserves cancellation ownership before waiting for the ingress lease.
//
//nolint:contextcheck // Link daemon shutdown as well as caller cancellation.
func (c *receiptWindowController) run(ctx context.Context,
	request ReceiptWindowRequest) (ReceiptWindowResult, error) {

	result := ReceiptWindowResult{Receipts: []LightningReceipt{}}
	windowCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	unlink := context.AfterFunc(c.root, cancel)
	defer unlink()
	c.stateMu.Lock()
	if c.foreground || c.transitions != 0 || c.activeID != "" {
		c.stateMu.Unlock()
		result.NeedsForeground = true

		return result, nil
	}
	if c.lastCanceledID == request.WindowID {
		cancel()
	}
	c.activeID, c.cancel = request.WindowID, cancel
	c.stateMu.Unlock()
	defer func() {
		c.stateMu.Lock()
		c.activeID, c.cancel = "", nil
		c.stateMu.Unlock()
	}()
	c.modeMu.Lock()
	defer c.modeMu.Unlock()
	lookup := LightningReceiptRequest{
		References: []LightningReceiptReference{
			request.Reference,
		},
		CreatedAfterUnix: request.CreatedAfterUnix,
	}
	resumed := false
	for {
		if windowCtx.Err() != nil {
			result.Interrupted = true

			return result, nil
		}
		receipts, err := c.read(windowCtx, lookup)
		if err != nil {
			if windowCtx.Err() != nil {
				result.Interrupted = true

				return result, nil
			}

			return result, err
		}
		result.Receipts = receipts
		if len(receipts) == 0 || receipts[0].State != "pending" {
			return result, nil
		}
		if !resumed {
			if err := c.resume(
				windowCtx, request.Reference,
			); err != nil {

				if windowCtx.Err() != nil {
					result.Interrupted = true

					return result, nil
				}

				return result, err
			}
			resumed = true
		}
		if result.Batches >= 32 {
			result.BudgetExhausted = true

			return result, nil
		}
		pumped, err := c.ingress.PumpIngressWithPolicy(
			windowCtx, 1, receiptIngressPolicy(),
		)
		result.Batches += pumped.Batches
		if errors.Is(err, serverconn.ErrIngressNeedsForeground) {
			result.NeedsForeground = true

			return result, nil
		}
		if err != nil && windowCtx.Err() == nil {
			return result, err
		}
		// An empty mailbox is no completion barrier. Source workers and
		// direct swap transport can commit after this pull returns.
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-windowCtx.Done():
		case <-timer.C:
		}
		timer.Stop()
	}
}

// receiptIngressPolicy lists the native durable destinations in daemon wiring.
// Read-only waiters must also match the request method recorded before Send.
func receiptIngressPolicy() serverconn.IngressPumpPolicy {
	reads := []mailboxrpc.ServiceMethod{
		{
			Service: "arkrpc.IndexerService",
			Method:  "ListVTXOsByScripts",
		},
		{
			Service: "arkrpc.IndexerService",
			Method:  "ListOORRecipientEventsByScript",
		},
	}
	durable := append([]mailboxrpc.ServiceMethod{}, reads...)
	durable = append(
		durable, mailboxrpc.ServiceMethod{
			Service: oorpb.ServiceName,
			Method:  oorpb.MethodSubmitPackage,
		}, mailboxrpc.ServiceMethod{
			Service: oorpb.ServiceName,
			Method:  oorpb.MethodFinalizePackage,
		}, mailboxrpc.ServiceMethod{
			Service: arkServiceName,
			Method:  MethodIncomingOOR,
		}, mailboxrpc.ServiceMethod{
			Service: arkServiceName,
			Method:  MethodIncomingVTXO,
		},
	)
	for _, method := range []string{
		roundpb.MethodJoinAck, roundpb.MethodJoinRoundQuote,
		roundpb.MethodBatchInfo, roundpb.MethodAwaitingInputSigs,
		roundpb.MethodAggNonces, roundpb.MethodAggSigs,
		roundpb.MethodRoundFailed, roundpb.MethodError,
		roundpb.MethodRoundStatusReport,
	} {
		durable = append(durable, mailboxrpc.ServiceMethod{
			Service: roundpb.ServiceName, Method: method,
		})
	}

	return serverconn.IngressPumpPolicy{
		DurableRoutes:     durable,
		ReadOnlyResponses: reads,
	}
}

// receiptController is available only after an unlocked, durable runtime boots.
func (s *Server) receiptController(ctx context.Context) (
	*receiptWindowController, error) {

	if s.cfg == nil || !s.cfg.DurableRoundRuntime {
		return nil, fmt.Errorf("receipt windows require durable " +
			"round runtime")
	}
	if err := s.WaitForWalletServicesReady(ctx); err != nil {
		return nil, err
	}
	s.hostControllerMu.Lock()
	defer s.hostControllerMu.Unlock()
	if s.hostController == nil {
		runtime := s.loadRuntime()
		if runtime == nil || s.runCtx == nil {
			return nil, fmt.Errorf("receipt ingress unavailable")
		}
		s.hostController = &receiptWindowController{
			ingress: runtime, root: s.runCtx,
			connected:  s.setServerConnected,
			foreground: !s.cfg.HostDrivenIngress,
			read:       s.ReadLightningReceipts,
			resume:     s.resumeLightningReceipt,
		}
	}

	return s.hostController, nil
}

// SetHostForeground joins the previous ingress owner before selecting a mode.
// Domain workers remain live; this is not a daemon shutdown or wallet unlock.
func (s *Server) SetHostForeground(ctx context.Context, foreground bool) error {
	controller, err := s.receiptController(ctx)
	if err != nil {
		return err
	}

	return controller.setForeground(ctx, foreground)
}

// RunReceiptWindow attempts one enrolled source under a cooperative 20s cap.
func (s *Server) RunReceiptWindow(ctx context.Context,
	request ReceiptWindowRequest) (ReceiptWindowResult, error) {

	if request.WindowID == "" || len(request.WindowID) > 128 {
		return ReceiptWindowResult{}, fmt.Errorf("invalid receipt " +
			"window ID")
	}
	controller, err := s.receiptController(ctx)
	if err != nil {
		return ReceiptWindowResult{}, err
	}

	return controller.run(ctx, request)
}

// CancelReceiptWindow cancels the matching reserved window without releasing
// its daemon owner. The original invocation still joins its final access.
func (s *Server) CancelReceiptWindow(ctx context.Context, id string) error {
	if id == "" || len(id) > 128 {
		return fmt.Errorf("invalid receipt window ID")
	}
	controller, err := s.receiptController(ctx)
	if err != nil {
		return err
	}
	controller.cancelWindow(id)

	return nil
}
