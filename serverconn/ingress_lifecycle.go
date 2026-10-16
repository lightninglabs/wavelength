package serverconn

import (
	"context"
	"errors"
)

var (
	// ErrIngressBusy means another loop or pump owns this connector's
	// ingress. The caller should coalesce the wake or retry after it exits.
	ErrIngressBusy = errors.New("ingress already running")

	// ErrIngressStopped means StopIngress has closed ingress admission.
	ErrIngressStopped = errors.New("ingress stopped")
)

// ingressRun owns checkpoint access and all workers of one ingress invocation.
// Closing done releases the lease only after those workers have exited.
type ingressRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// beginIngress admits one owner before it touches durable cursor state. The
// published cancellation also lets incompatibility interrupt startup or pumps.
func (a *ServerConnectionActor) beginIngress(ctx context.Context,
	cancel context.CancelFunc) (*ingressRun, error) {

	a.ingressMu.Lock()
	defer a.ingressMu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a.ingressStopped {
		return nil, ErrIngressStopped
	}
	if ce := a.compatibilityError(); ce != nil {
		return nil, ce
	}
	if a.ingressRun != nil {
		return nil, ErrIngressBusy
	}

	run := &ingressRun{
		cancel: cancel,
		done:   make(chan struct{}),
	}
	a.ingressCancel.Store(&run.cancel)

	// markIncompatible publishes its error before loading ingressCancel.
	// Rechecking here closes the race between those two publications.
	if ce := a.compatibilityError(); ce != nil {
		cancel()
		a.ingressCancel.Store(nil)

		return nil, ce
	}
	a.ingressRun = run

	return run, nil
}

// finishIngress releases ownership after all work using the run has joined.
func (a *ServerConnectionActor) finishIngress(run *ingressRun) {
	run.cancel()
	a.ingressMu.Lock()
	defer a.ingressMu.Unlock()

	a.ingressCancel.Store(nil)
	a.ingressRun = nil
	close(run.done)
}

// ingressError preserves a permanent incompatibility that canceled the run;
// otherwise local cancellation takes precedence over a transport's error.
func (a *ServerConnectionActor) ingressError(ctx context.Context,
	err error) error {

	if ce := a.compatibilityError(); ce != nil {
		return ce
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}

	return err
}

// stopIngress cancels and joins the owner observed under the admission lock.
// A permanent stop also prevents future invocations from acquiring a lease.
func (a *ServerConnectionActor) stopIngress(permanent bool) {
	a.ingressMu.Lock()
	if permanent {
		a.ingressStopped = true
	}
	run := a.ingressRun
	if run != nil {
		run.cancel()
	}
	a.ingressMu.Unlock()

	if run != nil {
		<-run.done
	}
}
