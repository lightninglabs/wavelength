package round

import (
	"context"
	"fmt"
	"sync"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// DurableRoundClientActor runs all client turns through the native persistent
// mailbox. Its owner must register Ref and OutboxRef before Start and include
// the client codec in the shared outbox publisher.
type DurableRoundClientActor struct {
	cfg      *RoundClientConfig
	runtime  *actor.DurableActor[actor.TLVMessage, actormsg.RoundActorResp]
	behavior *durableClientBehavior
	store    actor.DeliveryStore
	ref      actor.ActorRef[
		actormsg.RoundReceivable, actormsg.RoundActorResp,
	]
	start    sync.Once
	stop     sync.Once
	startErr error
	stopErr  error
	cancel   context.CancelFunc
}

// NewDurableRoundClientActor builds an inert runtime. The delivery store and
// domain stores must share the same database and honor its transaction context.
func NewDurableRoundClientActor(cfg *RoundClientConfig,
	store actor.DeliveryStore) (*DurableRoundClientActor, error) {

	if cfg == nil || cfg.Name == "" || store == nil {
		return nil, fmt.Errorf("client config, name, and store are " +
			"required")
	}
	if cfg.WalletActor == nil || cfg.ServerConn == nil ||
		cfg.ChainSource == nil || cfg.OperatorTerms == nil ||
		cfg.RoundStore == nil || cfg.VTXOStore == nil {
		return nil, fmt.Errorf("client runtime dependencies are " +
			"required")
	}
	config := *cfg
	owner, err := NewRoundClientActor(&config).Unpack()
	if err != nil {
		return nil, err
	}
	behavior := &durableClientBehavior{
		owner: owner, actorID: config.Name,
		codec: newDurableClientCodec(),
		ready: actor.NewPromise[struct{}](),
	}
	runtimeConfig := actor.DefaultDurableTxActorConfig(
		config.Name, behavior,
		func(_ context.Context,
			delivery actor.DeliveryStore) clientDurableTx {

			return clientDurableTx{delivery: delivery}
		},
		store, behavior.codec,
	)
	runtimeConfig.Log = fn.Some(owner.log)
	runtime, err := actor.NewDurableActor(runtimeConfig).Unpack()
	if err != nil {
		return nil, err
	}
	ref := actor.NewMapRef(
		runtime.Ref(), durableClientIngress,
		func(response actormsg.RoundActorResp) actormsg.RoundActorResp {
			return response
		},
	)
	config.SelfRef = ref
	lifecycle, cancel := context.WithCancel(context.Background())
	owner.runCtx = lifecycle

	return &DurableRoundClientActor{
		runtime: runtime, behavior: behavior, store: store, ref: ref,
		cfg:    &config,
		cancel: cancel,
	}, nil
}

// Ref exposes the existing typed client service through durable ingress.
func (a *DurableRoundClientActor) Ref() actor.ActorRef[
	actormsg.RoundReceivable, actormsg.RoundActorResp] {

	return a.ref
}

// OutboxRef exposes native effect delivery under the durable mailbox identity.
func (a *DurableRoundClientActor) OutboxRef() actor.ActorRef[
	actor.Message,
	any,
] {

	return actor.TypeAssertingRef[
		actor.Message, actor.TLVMessage, actormsg.RoundActorResp,
	](
		a.runtime.Ref(),
	)
}

// Start enqueues native restart before delivery and waits for its commit before
// requesting wallet backlog. It does not scan domain rows to reconstruct FSMs.
func (a *DurableRoundClientActor) Start(ctx context.Context) error {
	a.start.Do(func() {
		a.startErr = a.startRuntime(ctx)
		if a.startErr != nil {
			a.cancel()
			a.runtime.Stop()
		}
	})

	return a.startErr
}

// startRuntime establishes checkpoint recovery before callbacks can arrive.
func (a *DurableRoundClientActor) startRuntime(ctx context.Context) error {
	b := a.behavior
	checkpoint, err := a.store.LoadCheckpoint(ctx, b.actorID)
	if err != nil {
		return err
	}
	if err := actor.PrependRestartMessage(
		ctx, a.store, b.codec, b.actorID, checkpoint,
	); err != nil {
		return err
	}
	a.runtime.Start()
	budget := a.cfg.WalletAskTimeout
	if budget <= 0 {
		budget = defaultWalletAskTimeout
	}
	readyCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	if _, err := b.ready.Future().Await(readyCtx).Unpack(); err != nil {
		return err
	}
	if err := b.owner.registerWalletNotifier(ctx, a.cfg); err != nil {
		return err
	}

	return nil
}

// StopAndWait joins message delivery before cleaning process-local sessions.
func (a *DurableRoundClientActor) StopAndWait(ctx context.Context) error {
	a.cancel()
	if err := a.runtime.StopAndWait(ctx); err != nil {
		return err
	}
	a.stop.Do(func() {
		a.stopErr = a.behavior.owner.OnStop(ctx)
	})

	return a.stopErr
}
