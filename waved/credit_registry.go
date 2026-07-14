package waved

import (
	"context"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/credit"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/timeout"
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

	// The registry actor is daemon-owned: its construction-time restore
	// and durable mailbox lifetime belong to the actor system, not the
	// boot ctx threaded into this init, so it must not inherit that ctx.
	//nolint:contextcheck
	registry, err := credit.NewRegistry(credit.RegistryConfig{
		Log:           fn.Some(s.subLogger(creditSubsystem)),
		Server:        s.cfg.Swap.CreditServer,
		Daemon:        s.cfg.Swap.CreditDaemon,
		Store:         creditStore,
		DeliveryStore: s.deliveryStore,
		TimeoutActor:  creditTimeoutRef,
		CallbackRef:   callbackRef,
		ActorSystem:   s.actorSystem,

		// Bound the awaiting states so a stuck credit-backed send fails
		// fast rather than parking forever (wavelength#880); see
		// MaxAwaitingPollsOrDefault for the zero-coercion rationale.
		MaxAwaitingPolls: s.cfg.Swap.Credit.MaxAwaitingPollsOrDefault(),
		AutoRedeem: credit.AutoRedeemConfig{
			Enabled:      !s.cfg.Swap.Credit.AutoRedeemDisabled,
			MinRedeemSat: s.cfg.Swap.Credit.AutoRedeemMinSat,
		},
	})
	if err != nil {
		return err
	}
	s.creditRegistry = registry

	// Publish the earmark setter so the wavewalletrpc subserver can wire
	// its prepared-send store into the auto-redeem interlock once that
	// store exists (the subserver is registered after this runs). Until
	// then the sweep has no earmark provider and redeems on available
	// credits alone, which is safe because no credit-backed send has been
	// prepared yet.
	s.cfg.Swap.CreditEarmarkSetter = registry.SetEarmarkProvider

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
