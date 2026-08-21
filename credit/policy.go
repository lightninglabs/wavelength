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
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/build"
)

// autoRedeemer runs the wallet-owned auto-redeem policy. Redemption is never
// exposed to the user: the wallet decides when to materialize available credits
// back into a vTXO.
//
// Steady-state auto-redeem is folded into the receive state machine: a settled
// receive that clears the watermark signals the registry directly (see
// awaitingSettlementState). The autoRedeemer therefore no longer runs a
// steady-state periodic sweep; it performs a boot-time reconcile, retrying
// transient evaluation failures until one succeeds, so a balance accumulated
// before this start is still materialized even when no receive will re-evaluate
// it.
type autoRedeemer struct {
	cfg      AutoRedeemConfig
	server   CreditServer
	daemon   CreditDaemon
	registry actor.TellOnlyRef[CreditMsg]
	log      btclog.Logger
	retry    time.Duration

	// earmark is the shared credit-earmark provider, read on the boot
	// reconcile. It is the same atomic pointer the per-operation children
	// consult, so wiring the provider once (after construction) reaches
	// every redeem decision.
	earmark *atomic.Pointer[EarmarkFunc]

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// newAutoRedeemer builds an auto-redeemer from the registry config, sharing the
// registry's earmark pointer so the provider can be wired once after
// construction.
func newAutoRedeemer(cfg RegistryConfig, registry actor.TellOnlyRef[CreditMsg],
	earmark *atomic.Pointer[EarmarkFunc]) *autoRedeemer {

	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}

	return &autoRedeemer{
		cfg:      cfg.AutoRedeem,
		server:   cfg.Server,
		daemon:   cfg.Daemon,
		registry: registry,
		log:      cfg.Log.UnwrapOr(btclog.Disabled),
		retry:    cfg.PollInterval,
		earmark:  earmark,
	}
}

// setEarmark wires (or rewires) the shared credit-earmark provider. The
// per-operation children read the same pointer, so this reaches both the boot
// reconcile and every receive-driven redeem decision.
func (a *autoRedeemer) setEarmark(fn EarmarkFunc) {
	if a == nil || fn == nil {
		return
	}

	a.earmark.Store(&fn)
}

// start runs the boot-time reconcile when the policy is enabled. A failed
// evaluation retries until one complete evaluation succeeds, because no later
// receive necessarily exists to reconsider an already-available balance.
// Steady-state auto-redeem remains receive-driven. The retry loop is anchored
// to ctx, which must be a daemon-lifetime context.
func (a *autoRedeemer) start(ctx context.Context) {
	if a == nil || !a.cfg.Enabled {
		return
	}

	a.mu.Lock()
	if a.cancel != nil {
		a.mu.Unlock()

		return
	}

	reconcileCtx, cancel := context.WithCancel(ctx)
	a.cancel = cancel
	a.wg.Add(1)
	a.mu.Unlock()

	go func() {
		defer a.wg.Done()

		for attempt := 1; ; attempt++ {
			err := a.reconcile(reconcileCtx)
			if err == nil || reconcileCtx.Err() != nil {
				return
			}

			a.logger(reconcileCtx).WarnS(
				reconcileCtx,
				"Boot auto-redeem reconcile failed; retrying",
				err,
				slog.Int("attempt", attempt),
			)

			timer := time.NewTimer(a.retry)
			select {
			case <-reconcileCtx.Done():
				timer.Stop()

				return

			case <-timer.C:
			}
		}
	}()
}

// stop cancels and waits for the boot reconcile goroutine to exit.
func (a *autoRedeemer) stop() {
	if a == nil {
		return
	}

	a.mu.Lock()
	cancel := a.cancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}

	a.wg.Wait()
}

// reconcile evaluates the auto-redeem watermark once and signals the registry
// when an over-watermark balance is already sitting available. The registry
// applies the no-pending-pay/redeem interlock before admitting the redeem, so
// this only has to clear the threshold against the earmark-adjusted balance.
func (a *autoRedeemer) reconcile(ctx context.Context) error {
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
	// this, the reconcile could redeem credits the user is about to spend,
	// forcing the pending send to re-top-up. Fail-safe: an error redeems
	// nothing.
	available := snapshot.AvailableSat
	if a.earmark != nil {
		if earmarkFn := a.earmark.Load(); earmarkFn != nil {
			earmarked, err := (*earmarkFn)(ctx)
			if err != nil {
				return fmt.Errorf("read earmarked credits: %w",
					err)
			}
			if earmarked >= available {
				available = 0
			} else {
				available -= earmarked
			}
		}
	}

	if available < threshold {
		return nil
	}

	a.logger(ctx).InfoS(ctx, "Boot reconcile signaling auto-redeem",
		slog.Uint64("available_sat", available),
		slog.Uint64("threshold_sat", threshold),
	)

	return a.registry.Tell(ctx, &ConsiderRedeemRequest{
		AvailableSat: available,
	})
}

// threshold returns the greater of the configured minimum and the live
// effective operator VTXO floor.
func (a *autoRedeemer) threshold(ctx context.Context) (uint64, error) {
	floor, err := a.daemon.VTXOFloor(ctx)
	if err != nil {
		return 0, fmt.Errorf("get operator VTXO floor: %w", err)
	}
	if floor == 0 {
		return 0, fmt.Errorf("operator VTXO floor is unavailable")
	}

	return max(a.cfg.MinRedeemSat, floor), nil
}

// logger returns the redeemer logger bound to ctx.
func (a *autoRedeemer) logger(ctx context.Context) btclog.Logger {
	if a.log != btclog.Disabled {
		return a.log
	}

	return build.LoggerFromContext(ctx)
}

// redeemOpKey mints a fresh stable idempotency key for one auto-redeem
// operation. A redemption is a one-shot materialization, so each trigger that
// passes the registry's in-flight interlock uses a fresh key.
func redeemOpKey() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate redeem op key: %w", err)
	}

	return "redeem:" + hex.EncodeToString(buf[:]), nil
}
