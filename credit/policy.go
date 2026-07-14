package credit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

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
// periodic sweep; it performs a single boot-time reconcile so a balance that
// accumulated over the watermark before this start — which no receive trigger
// will re-evaluate — is still materialized.
type autoRedeemer struct {
	cfg      AutoRedeemConfig
	server   CreditServer
	daemon   CreditDaemon
	registry actor.TellOnlyRef[CreditMsg]
	log      btclog.Logger

	// earmark is the shared credit-earmark provider, read on the boot
	// reconcile. It is the same atomic pointer the per-operation children
	// consult, so wiring the provider once (after construction) reaches
	// every redeem decision.
	earmark *atomic.Pointer[EarmarkFunc]

	wg sync.WaitGroup
}

// newAutoRedeemer builds an auto-redeemer from the registry config, sharing the
// registry's earmark pointer so the provider can be wired once after
// construction.
func newAutoRedeemer(cfg RegistryConfig, registry actor.TellOnlyRef[CreditMsg],
	earmark *atomic.Pointer[EarmarkFunc]) *autoRedeemer {

	return &autoRedeemer{
		cfg:      cfg.AutoRedeem,
		server:   cfg.Server,
		daemon:   cfg.Daemon,
		registry: registry,
		log:      cfg.Log.UnwrapOr(btclog.Disabled),
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

// start runs the single boot-time reconcile when the policy is enabled. There
// is no periodic loop: the receive state machine drives steady-state
// auto-redeem. It is anchored to ctx, which must be a daemon-lifetime context.
func (a *autoRedeemer) start(ctx context.Context) {
	if a == nil || !a.cfg.Enabled {
		return
	}

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()

		if err := a.reconcile(ctx); err != nil {
			a.logger(ctx).DebugS(ctx, "Boot auto-redeem reconcile "+
				"failed", slog.String("err", err.Error()))
		}
	}()
}

// stop waits for the boot reconcile goroutine to exit.
func (a *autoRedeemer) stop() {
	if a == nil {
		return
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

	if available <= threshold {
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
