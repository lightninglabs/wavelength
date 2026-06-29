package credit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightninglabs/darepo-client/db"
)

// autoRedeemer runs the wallet-owned auto-redeem policy. Redemption is never
// exposed to the user: this loop decides when to materialize available credits
// back into a vTXO. It periodically sweeps the account and, when the available
// balance clears the configured threshold and no credit-consuming or in-flight
// redemption operation is pending, asks the registry to redeem.
type autoRedeemer struct {
	cfg      AutoRedeemConfig
	store    Store
	server   CreditServer
	daemon   CreditDaemon
	registry actor.TellOnlyRef[CreditMsg]
	log      btclog.Logger

	// earmark is the credit-earmark provider, read on every sweep. It is an
	// atomic pointer so it can be wired after construction (the wallet's
	// prepared-send store is built after the registry), without locking the
	// sweep against the setter.
	earmark atomic.Pointer[EarmarkFunc]

	wg   sync.WaitGroup
	quit chan struct{}
	once sync.Once
}

// newAutoRedeemer builds an auto-redeemer from the registry config.
func newAutoRedeemer(cfg RegistryConfig,
	registry actor.TellOnlyRef[CreditMsg]) *autoRedeemer {

	policy := cfg.AutoRedeem
	if policy.Interval <= 0 {
		policy.Interval = DefaultAutoRedeemInterval
	}

	a := &autoRedeemer{
		cfg:      policy,
		store:    cfg.Store,
		server:   cfg.Server,
		daemon:   cfg.Daemon,
		registry: registry,
		log:      cfg.Log.UnwrapOr(btclog.Disabled),
		quit:     make(chan struct{}),
	}
	if policy.EarmarkedSat != nil {
		a.setEarmark(policy.EarmarkedSat)
	}

	return a
}

// setEarmark wires (or rewires) the credit-earmark provider read on each sweep.
func (a *autoRedeemer) setEarmark(fn EarmarkFunc) {
	if a == nil || fn == nil {
		return
	}

	a.earmark.Store(&fn)
}

// start launches the sweep loop when the policy is enabled.
func (a *autoRedeemer) start(ctx context.Context) {
	if a == nil || !a.cfg.Enabled {
		return
	}

	a.wg.Add(1)
	go a.run(ctx)
}

// stop signals the sweep loop to exit and waits for it.
func (a *autoRedeemer) stop() {
	if a == nil {
		return
	}

	a.once.Do(func() {
		close(a.quit)
	})
	a.wg.Wait()
}

// run is the periodic sweep loop.
func (a *autoRedeemer) run(ctx context.Context) {
	defer a.wg.Done()

	ticker := time.NewTicker(a.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-a.quit:
			return

		case <-ctx.Done():
			return

		case <-ticker.C:
			if err := a.sweep(ctx); err != nil {
				a.logger(ctx).DebugS(ctx, "Auto-redeem sweep "+
					"failed", slog.String(
					"err", err.Error(),
				))
			}
		}
	}
}

// sweep performs one auto-redeem evaluation and, when warranted, asks the
// registry to redeem the available balance.
func (a *autoRedeemer) sweep(ctx context.Context) error {
	ops, err := a.store.ListNonTerminal(ctx)
	if err != nil {
		return fmt.Errorf("list non-terminal credit ops: %w", err)
	}

	threshold, err := a.threshold(ctx)
	if err != nil {
		return err
	}

	acctKey, err := a.daemon.IdentityPubKey(ctx)
	if err != nil {
		return fmt.Errorf("get identity pubkey: %w", err)
	}

	snapshot, err := a.server.ListCredits(ctx, acctKey)
	if err != nil {
		return fmt.Errorf("list credits: %w", err)
	}

	// Subtract any credits earmarked by an in-flight wallet operation that
	// has not yet written a durable credit_operations row — chiefly a
	// credit-backed PrepareSend, whose row is created only at Send. Without
	// this, the sweep could redeem credits the user is about to spend,
	// forcing the pending send to re-top-up. The interlock fails safe: an
	// error or a missing provider redeems nothing it should not, but does
	// not block a legitimate sweep.
	available := snapshot.AvailableSat
	if fn := a.earmark.Load(); fn != nil {
		earmarked, err := (*fn)(ctx)
		if err != nil {
			return fmt.Errorf("read earmarked credits: %w", err)
		}
		if earmarked >= available {
			available = 0
		} else {
			available -= earmarked
		}
	}

	amount, ok := redeemDecision(available, threshold, ops)
	if !ok {
		return nil
	}

	opKey, err := redeemOpKey()
	if err != nil {
		return err
	}

	a.logger(ctx).InfoS(ctx, "Auto-redeeming available credits",
		slog.Uint64("amount_sat", amount),
		slog.Uint64("threshold_sat", threshold),
	)

	return a.registry.Tell(ctx, &RedeemRequest{
		OpKey:     opKey,
		AmountSat: amount,
	})
}

// threshold returns the configured minimum, defaulting to the operator dust
// limit (the smallest amount that can legally become a vTXO).
func (a *autoRedeemer) threshold(ctx context.Context) (uint64, error) {
	if a.cfg.MinRedeemSat > 0 {
		return a.cfg.MinRedeemSat, nil
	}

	dust, err := a.daemon.DustLimit(ctx)
	if err != nil {
		return 0, fmt.Errorf("get dust limit: %w", err)
	}

	return dust, nil
}

// logger returns the redeemer logger bound to ctx.
func (a *autoRedeemer) logger(ctx context.Context) btclog.Logger {
	if a.log != btclog.Disabled {
		return a.log
	}

	return build.LoggerFromContext(ctx)
}

// redeemDecision decides whether to auto-redeem. It returns the amount to
// redeem and true only when the available balance strictly exceeds the
// threshold and no operation is pending that would consume credits (an
// in-flight pay) or that is itself an in-flight redemption. A pending receive
// does not block: it only adds credits.
func redeemDecision(availableSat, thresholdSat uint64,
	nonTerminal []db.CreditOperationRecord) (uint64, bool) {

	if availableSat <= thresholdSat {
		return 0, false
	}

	for _, op := range nonTerminal {
		switch op.Kind {
		case db.CreditOpKindPay, db.CreditOpKindRedeem:
			return 0, false

		case db.CreditOpKindReceive:
			// A pending receive only adds credits, so it does not
			// block an auto-redeem.
		}
	}

	return availableSat, true
}

// redeemOpKey mints a fresh stable idempotency key for one auto-redeem
// operation. A redemption is a one-shot materialization, so each sweep that
// passes the in-flight interlock uses a fresh key.
func redeemOpKey() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate redeem op key: %w", err)
	}

	return "redeem:" + hex.EncodeToString(buf[:]), nil
}
