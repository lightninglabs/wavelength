package darepod

import (
	"context"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/credit"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/timeout"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// creditSubsystem is the subsystem name for credit-registry logs.
const creditSubsystem = "CRDT"

// initCreditRegistry constructs and starts the credit durable-actor subsystem
// when the swap runtime has published its credit bridges (cfg.Swap.CreditServer
// / cfg.Swap.CreditDaemon). In builds without the swap runtime those handles
// are nil and the subsystem is skipped. The registry is anchored to the daemon
// root context so a CLI disconnect never cancels an in-flight credit operation.
func (s *Server) initCreditRegistry(ctx context.Context) error {
	if s.cfg.Swap == nil || s.cfg.Swap.CreditServer == nil ||
		s.cfg.Swap.CreditDaemon == nil {
		return nil
	}

	// Build a dedicated timeout actor for credit poll timers, mirroring the
	// OOR timeout actor. When a per-operation poll timer fires, the
	// callback ref maps the expiry into a ResumeCreditOpRequest told to the
	// registry.
	creditTimeoutBehavior := timeout.NewActor()
	creditTimeoutKey := actor.NewServiceKey[timeout.Msg, timeout.Resp](
		"credit-timeout",
	)
	creditTimeoutRef := actor.RegisterWithSystem(
		s.actorSystem, "credit-timeout", creditTimeoutKey,
		creditTimeoutBehavior,
	)
	creditTimeoutBehavior.Start(creditTimeoutRef)

	// The callback resolves the credit service key (owned by the registry)
	// lazily at Tell time, so it is safe to build before the registry
	// registers under that key inside NewRegistry.
	callbackRef := credit.NewRetryCallbackRef(
		credit.NewServiceKey().Ref(s.actorSystem),
	)

	dbStore := db.NewStore(
		s.db.DB, s.db.Queries, s.db.Backend(),
		s.subLogger(db.Subsystem),
	)
	creditStore := db.NewCreditOperationStore(dbStore, s.clk)

	registry, err := credit.NewRegistry(credit.RegistryConfig{
		Log:           fn.Some(s.subLogger(creditSubsystem)),
		Server:        s.cfg.Swap.CreditServer,
		Daemon:        s.cfg.Swap.CreditDaemon,
		Store:         creditStore,
		DeliveryStore: s.deliveryStore,
		TimeoutActor:  creditTimeoutRef,
		CallbackRef:   callbackRef,
		ActorSystem:   s.actorSystem,
		AutoRedeem: credit.AutoRedeemConfig{
			Enabled: true,
		},
	})
	if err != nil {
		return err
	}
	s.creditRegistry = registry

	// Restore any in-flight credit operations interrupted by a restart,
	// then start the wallet-owned auto-redeem sweep on the daemon root
	// context.
	if err := registry.RestoreNonTerminal(ctx); err != nil {
		return err
	}
	registry.StartAutoRedeem(ctx)

	s.log.InfoS(ctx, "Credit registry started")

	return nil
}
