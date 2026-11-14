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

// WorkWindowRequest re-arms a bounded set of existing local receive sources.
// References come from trusted host enrollment, never from push payloads.
// Unlike a payment window, work yields no receipt or presentation permission.
type WorkWindowRequest struct {
	WindowID   string                      `json:"window_id"`
	References []LightningReceiptReference `json:"references"`
}

// ReceiptWindowResult reports local source proof and why a bounded attempt
// yielded. No field asserts global reconciliation or safe suspension.
type ReceiptWindowResult struct {
	Receipts            []LightningReceipt `json:"receipts"`
	Batches             uint32             `json:"batches"`
	NeedsForeground     bool               `json:"needs_foreground"`
	Interrupted         bool               `json:"interrupted"`
	BudgetExhausted     bool               `json:"budget_exhausted"`
	MaintenanceChecked  uint32             `json:"maintenance_checked"`
	MaintenanceDeferred bool               `json:"maintenance_deferred"`
	MaintenanceMore     bool               `json:"maintenance_more"`
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
	// afterForeground runs authenticated bootstrap under mode ownership so
	// a concurrent pause/restart cannot cancel its response transport.
	afterForeground func(context.Context) error
	//nolint:containedctx // Daemon owns foreground ingress, not the caller.
	root context.Context
	read func(context.Context, LightningReceiptRequest) (
		[]LightningReceipt, error)
	resume   func(context.Context, LightningReceiptReference) error
	maintain func(context.Context) (uint32, bool, bool)
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
	if c.afterForeground != nil {
		return c.afterForeground(ctx)
	}

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
func (c *receiptWindowController) run(ctx context.Context,
	request ReceiptWindowRequest) (ReceiptWindowResult, error) {

	return c.runWindow(ctx, request, true)
}

// runWindow shares the same lease and cancellation fence for payment and
// maintenance work. Only payment windows read or resume enrolled sources.
func (c *receiptWindowController) runWindow(ctx context.Context,
	request ReceiptWindowRequest, payment bool) (ReceiptWindowResult,
	error) {

	return c.runWindowWithWork(ctx, request, payment, nil)
}

// runWindowWithWork shares intake ownership with receipt windows. Sources are
// resumed only once, after the first pump, even when intake needs foreground.
//
//nolint:contextcheck // Link daemon shutdown as well as caller cancellation.
func (c *receiptWindowController) runWindowWithWork(ctx context.Context,
	request ReceiptWindowRequest, payment bool,
	references []LightningReceiptReference) (ReceiptWindowResult, error) {

	result := ReceiptWindowResult{Receipts: []LightningReceipt{}}
	// The host's expiry cancellation and deadline take precedence. This
	// ceiling only bounds a caller that fails to cancel its opportunity;
	// it must not truncate the host's usable mobile execution window.
	windowCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
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
	// Give ready signing/receive messages the first turn. Routine expiry
	// scanning must not consume the beginning of a short host opportunity.
	maintenancePending := !payment
	result.MaintenanceDeferred = maintenancePending
	resumed := false
	workPending := len(references) > 0
	for {
		if windowCtx.Err() != nil {
			result.Interrupted = true

			return result, nil
		}
		if payment {
			receipts, err := c.advanceReceipt(
				windowCtx, lookup, &resumed,
			)
			if err != nil {
				if windowCtx.Err() != nil {
					result.Interrupted = true

					return result, nil
				}

				return result, err
			}
			result.Receipts = receipts
			if len(receipts) == 0 ||
				receipts[0].State != "pending" {
				return result, nil
			}
		}

		if result.Batches >= 32 {
			result.BudgetExhausted = true

			return result, nil
		}
		pumped, err := c.ingress.PumpIngressWithPolicy(
			windowCtx, 1, receiptIngressPolicy(),
		)
		result.Batches += pumped.Batches
		if workPending && windowCtx.Err() == nil {
			seen := make(map[LightningReceiptReference]struct{})
			for _, ref := range references {
				if windowCtx.Err() != nil {
					break
				}
				if _, duplicate := seen[ref]; duplicate {
					continue
				}
				seen[ref] = struct{}{}
				// Existing-source validation and duplicate
				// worker ownership belong to the targeted
				// resumer. A failed source stays pending; it
				// cannot block other sources.
				_ = c.resume(windowCtx, ref)
			}
			workPending = false
		}
		if errors.Is(err, serverconn.ErrIngressNeedsForeground) {
			result.NeedsForeground = true

			return result, nil
		}
		if err != nil && windowCtx.Err() == nil {
			return result, err
		}
		if maintenancePending && windowCtx.Err() == nil {
			stepCtx, stop := context.WithTimeout(
				windowCtx, 4*time.Second,
			)
			result.MaintenanceChecked, result.MaintenanceDeferred,
				result.MaintenanceMore = c.maintain(stepCtx)
			stop()
			maintenancePending = false
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

// advanceReceipt re-arms one existing source once, retaining committed reads
// as the completion barrier. Maintenance windows never enter this path.
func (c *receiptWindowController) advanceReceipt(ctx context.Context,
	lookup LightningReceiptRequest, resumed *bool) ([]LightningReceipt,
	error) {

	receipts, err := c.read(ctx, lookup)
	if err != nil || len(receipts) == 0 || receipts[0].State != "pending" ||
		*resumed {
		return receipts, err
	}
	if err := c.resume(ctx, lookup.References[0]); err != nil {
		return nil, err
	}
	*resumed = true

	return receipts, nil
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
		maintenance := newExpiryMaintenance(s)
		var foregroundHook func(context.Context) error
		if s.cfg.HostDrivenIngress {
			foregroundHook = s.finishWalletOnlineStartup
		}
		s.hostController = &receiptWindowController{
			ingress: runtime, root: s.runCtx,
			connected:       s.setServerConnected,
			foreground:      !s.cfg.HostDrivenIngress,
			read:            s.ReadLightningReceipts,
			resume:          s.resumeLightningReceipt,
			maintain:        maintenance.run,
			afterForeground: foregroundHook,
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

// RunReceiptWindow attempts one enrolled source until host cancellation or a
// cooperative 30s ceiling.
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

// RunMaintenanceWindow pumps ready protocol work and samples current expiry.
// It re-arms no invoice sources and cannot authorize payment presentation.
func (s *Server) RunMaintenanceWindow(ctx context.Context, id string) (
	ReceiptWindowResult, error) {

	return s.RunWorkWindow(ctx, WorkWindowRequest{WindowID: id})
}

// RunWorkWindow re-arms only existing local receive sources within the same
// host lease as bounded maintenance. It never reads receipts or starts pays.
// Hosts must pause foreground ingress and retain the handle until final access.
func (s *Server) RunWorkWindow(ctx context.Context, request WorkWindowRequest) (
	ReceiptWindowResult, error) {

	if err := validateWorkWindow(request); err != nil {
		return ReceiptWindowResult{}, err
	}
	request.References = append(
		[]LightningReceiptReference(nil), request.References...,
	)
	controller, err := s.receiptController(ctx)
	if err != nil {
		return ReceiptWindowResult{}, err
	}

	return controller.runWindowWithWork(ctx, ReceiptWindowRequest{
		WindowID: request.WindowID,
	}, false, request.References)
}

// validateWorkWindow rejects the full request before any native lease or source
// access. An empty source set is the ordinary maintenance contract.
func validateWorkWindow(request WorkWindowRequest) error {
	if request.WindowID == "" || len(request.WindowID) > 128 ||
		len(request.References) > 64 {
		return fmt.Errorf("invalid work window request")
	}
	for _, ref := range request.References {
		if (ref.Source != LightningReceiptSwap &&
			ref.Source != LightningReceiptCredit) ||
			len(ref.ID) == 0 || len(ref.ID) > 128 {
			return fmt.Errorf("invalid local work source")
		}
	}

	return nil
}
