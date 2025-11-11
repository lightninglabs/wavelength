package chainsource

import (
	"context"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// ConfActor is a single-subscription actor that monitors transaction
// confirmations and delivers an event when the transaction reaches its target
// confirmation count. Each instance serves exactly one subscription.
//
// The actor supports dual-mode operation: Future mode for blocking await, and
// Actor mode for asynchronous event delivery. The actor exits after delivering
// the confirmation event.
type ConfActor struct {
	// backend is the blockchain backend used to monitor confirmations.
	backend ChainBackend

	// txid is the transaction ID being monitored.
	txid *chainhash.Hash

	// pkScript is the public key script being monitored.
	pkScript []byte

	// targetConfs is the number of confirmations to wait for.
	targetConfs uint32

	// heightHint is the earliest block that could contain the transaction.
	heightHint uint32

	// promise is used in Future mode to complete the future when the
	// confirmation is detected.
	promise fn.Option[actor.Promise[ConfirmationEvent]]

	// notifyActor is used in Actor mode to send events. None in Future
	// mode.
	notifyActor fn.Option[actor.TellOnlyRef[ConfirmationEvent]]

	// registration is the backend registration for this watch.
	registration *ConfRegistration

	// ctx is the actor's context, cancelled when the actor stops.
	//nolint:containedctx
	ctx context.Context

	// cancel cancels the actor's context.
	cancel context.CancelFunc

	// wg tracks background goroutines for graceful shutdown.
	wg sync.WaitGroup
}

// NewConfActor creates a new ConfActor instance with the given backend and
// parent context. The actor waits for a RegisterConfRequest message to begin
// monitoring.
func NewConfActor(backend ChainBackend, parentCtx context.Context) *ConfActor {
	ctx, cancel := context.WithCancel(parentCtx)

	return &ConfActor{
		backend: backend,
		ctx:     ctx,
		cancel:  cancel,
	}
}

// Receive processes incoming messages for the ConfActor.
func (a *ConfActor) Receive(actorCtx context.Context,
	msg ConfMsg) fn.Result[ConfResp] {

	switch m := msg.(type) {
	case *RegisterConfRequest:
		return a.handleRegisterConf(actorCtx, m)

	default:
		return fn.Err[ConfResp](fmt.Errorf("unknown message type: %T",
			msg))
	}
}

// handleRegisterConf processes a confirmation registration request. It
// configures the actor and starts monitoring.
func (a *ConfActor) handleRegisterConf(actorCtx context.Context,
	req *RegisterConfRequest) fn.Result[ConfResp] {

	// Do some basic validation of the request parameters.
	if req.Txid == nil && len(req.PkScript) == 0 {
		return fn.Err[ConfResp](fmt.Errorf(
			"either txid or pkScript must be provided"))
	}
	if req.TargetConfs == 0 {
		return fn.Err[ConfResp](fmt.Errorf(
			"target confirmations must be greater than zero"))
	}

	a.txid = req.Txid
	a.pkScript = req.PkScript
	a.targetConfs = req.TargetConfs
	a.heightHint = req.HeightHint
	a.notifyActor = req.NotifyActor

	// We're either in future or iterator mode, set the promise
	// accordingly.
	var promise fn.Option[actor.Promise[ConfirmationEvent]]
	if req.NotifyActor.IsNone() {
		promise = fn.Some(actor.NewPromise[ConfirmationEvent]())
	} else {
		promise = fn.None[actor.Promise[ConfirmationEvent]]()
	}
	a.promise = promise

	// Register with the backend to receive confirmation notifications. We
	// do this before starting the goroutine so we can return an error to
	// the caller if registration fails.
	registration, err := a.backend.RegisterConf(
		a.ctx, a.txid, a.pkScript, a.targetConfs, a.heightHint,
	)
	if err != nil {
		return fn.Err[ConfResp](fmt.Errorf(
			"failed to register for confirmations: %w", err))
	}
	a.registration = registration

	// Now we'll kick off our monitoring goroutine which handles the
	// iteration.
	a.wg.Add(1)
	go a.monitorConfirmation()

	resp := &RegisterConfResponse{}

	// If we're in blocking/future mode, then add the Future to the
	// response.
	promise.WhenSome(func(p actor.Promise[ConfirmationEvent]) {
		resp.Future = p.Future()
	})

	return fn.Ok[ConfResp](resp)
}

// monitorConfirmation runs in a background goroutine and waits for the target
// confirmation count to be reached. When reached, it delivers the event and
// exits.
func (a *ConfActor) monitorConfirmation() {
	defer a.wg.Done()
	defer a.cancel()

	// Clean up registration when done.
	defer func() {
		if a.registration != nil {
			a.registration.Cancel()
		}
	}()

	select {
	case confDetails, ok := <-a.registration.Confirmed:
		if !ok || confDetails == nil {
			a.failConfirmation(fmt.Errorf(
				"confirmation subscription closed",
			))

			return
		}

		event, err := buildConfirmationEvent(confDetails, a)
		if err != nil {
			a.failConfirmation(err)
			return
		}

		a.deliverConfirmation(event)

	case <-a.ctx.Done():

		a.failConfirmation(a.ctx.Err())
	}
}

// deliverConfirmation sends the confirmation event to the subscriber and
// completes the promise (Future mode) or sends to the actor (Actor mode).
func (a *ConfActor) deliverConfirmation(event ConfirmationEvent) {
	a.promise.WhenSome(func(p actor.Promise[ConfirmationEvent]) {
		p.Complete(fn.Ok(event))
	})

	a.notifyActor.WhenSome(func(ref actor.TellOnlyRef[ConfirmationEvent]) {
		ref.Tell(a.ctx, event)
	})
}

// failConfirmation completes the promise with an error (Future mode) or does
// nothing (Actor mode - errors are not delivered in async mode).
func (a *ConfActor) failConfirmation(err error) {
	a.promise.WhenSome(func(p actor.Promise[ConfirmationEvent]) {
		p.Complete(fn.Err[ConfirmationEvent](err))
	})
}

// Stop gracefully shuts down the ConfActor. It cancels the context and waits
// for the monitoring goroutine to complete.
func (a *ConfActor) Stop() {
	// Cancel the context to signal shutdown.
	a.cancel()

	// Wait for the monitoring goroutine to complete.
	a.wg.Wait()
}

// buildConfirmationEvent converts the backend TxConfirmation into a
// ConfirmationEvent, filling in missing fields where possible.
func buildConfirmationEvent(details *TxConfirmation,
	watch *ConfActor) (ConfirmationEvent, error) {

	txHash, err := confTxHash(details, watch)
	if err != nil {
		return ConfirmationEvent{}, err
	}

	if details.BlockHash == nil {
		return ConfirmationEvent{}, fmt.Errorf("confirmation event " +
			"missing block hash")
	}

	event := ConfirmationEvent{
		Txid:        txHash,
		BlockHeight: int32(details.BlockHeight),
		BlockHash:   *details.BlockHash,
		NumConfs:    watch.targetConfs,
	}

	return event, nil
}

// confTxHash determines the transaction hash, falling back to deriving it
// from the txid if needed.
func confTxHash(details *TxConfirmation,
	watch *ConfActor) (chainhash.Hash, error) {

	switch {
	case details.Tx != nil:
		return details.Tx.TxHash(), nil

	case watch.txid != nil:
		return *watch.txid, nil

	default:
		return chainhash.Hash{}, fmt.Errorf(
			"confirmation event missing transaction hash",
		)
	}
}
