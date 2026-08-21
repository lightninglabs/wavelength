package credit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/build"
	mailboxconn "github.com/lightninglabs/wavelength/mailbox/conn"
)

const (
	// defaultBootReconcileTimeout bounds how long startup keeps probing for
	// a transiently unavailable operator. It matches the durable credit
	// operation backstop closely enough to absorb a short outage without
	// creating an unbounded daemon-lifetime retry loop.
	defaultBootReconcileTimeout = 4 * time.Minute

	// maxBootReconcileBackoff caps exponential retry spacing so a
	// recovering operator is noticed promptly without being polled every
	// two seconds throughout a longer outage.
	maxBootReconcileBackoff = 30 * time.Second
)

// autoRedeemer runs the wallet-owned auto-redeem policy. Redemption is never
// exposed to the user: the wallet decides when to materialize available credits
// back into a vTXO.
//
// Steady-state auto-redeem is folded into the receive state machine: a settled
// receive that clears the watermark signals the registry directly (see
// awaitingSettlementState). The autoRedeemer therefore no longer runs a
// steady-state periodic sweep; it performs a bounded boot-time reconcile,
// retrying transient evaluation failures with backoff so a balance accumulated
// before this start can still be materialized even when no receive will
// re-evaluate it.
type autoRedeemer struct {
	cfg          AutoRedeemConfig
	server       CreditServer
	daemon       CreditDaemon
	registry     actor.TellOnlyRef[CreditMsg]
	log          btclog.Logger
	retry        time.Duration
	maxRetry     time.Duration
	retryTimeout time.Duration

	// earmark is the shared credit-earmark provider, read on the boot
	// reconcile. It is the same atomic pointer the per-operation children
	// consult, so wiring the provider once (after construction) reaches
	// every redeem decision.
	earmark *atomic.Pointer[EarmarkFunc]

	mu           sync.Mutex
	cancel       context.CancelFunc
	signalCancel context.CancelFunc
	wg           sync.WaitGroup
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
		cfg:          cfg.AutoRedeem,
		server:       cfg.Server,
		daemon:       cfg.Daemon,
		registry:     registry,
		log:          cfg.Log.UnwrapOr(btclog.Disabled),
		retry:        cfg.PollInterval,
		maxRetry:     maxBootReconcileBackoff,
		retryTimeout: defaultBootReconcileTimeout,
		earmark:      earmark,
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

// start runs the boot-time reconcile when the policy is enabled. Transient
// evaluation failures retry with exponential backoff until one evaluation
// succeeds or the cumulative retry window expires. Permanent compatibility
// errors stop immediately. Steady-state auto-redeem remains receive-driven.
// The retry loop is anchored to ctx, which must be a daemon-lifetime context.
func (a *autoRedeemer) start(ctx context.Context) {
	if a == nil || !a.cfg.Enabled {
		return
	}

	a.mu.Lock()
	if a.cancel != nil {
		a.mu.Unlock()

		return
	}

	retry := a.retry
	if retry <= 0 {
		retry = DefaultPollInterval
	}
	maxRetry := a.maxRetry
	if maxRetry < retry {
		maxRetry = retry
	}
	retryTimeout := a.retryTimeout
	if retryTimeout <= 0 {
		retryTimeout = defaultBootReconcileTimeout
	}

	reconcileCtx, cancel := context.WithTimeout(ctx, retryTimeout)
	signalCtx, signalCancel := context.WithCancel(ctx)
	a.cancel = cancel
	a.signalCancel = signalCancel
	a.wg.Add(1)
	a.mu.Unlock()

	go func() {
		defer a.wg.Done()
		defer cancel()

		var (
			attempt int
			lastErr error
		)
		for {
			if reconcileCtx.Err() != nil {
				if ctx.Err() == nil && errors.Is(
					reconcileCtx.Err(),
					context.DeadlineExceeded,
				) {

					a.logger(ctx).WarnS(
						ctx,
						"Boot auto-redeem reconcile "+
							"stopped",
						lastErr,
						slog.Int("attempt", attempt),
						slog.String(
							"reason",
							"retry window expired",
						),
					)
				}

				return
			}

			attempt++
			err := a.reconcile(reconcileCtx, signalCtx)
			if err == nil || ctx.Err() != nil {
				return
			}
			lastErr = err
			if mailboxconn.IsPermanentVersionError(err) {
				a.logger(reconcileCtx).WarnS(
					reconcileCtx,
					"Boot auto-redeem reconcile stopped",
					err,
					slog.Int("attempt", attempt),
					slog.String(
						"reason",
						"incompatible operator",
					),
				)

				return
			}
			if reconcileCtx.Err() != nil {
				continue
			}

			a.logger(reconcileCtx).WarnS(
				reconcileCtx,
				"Boot auto-redeem reconcile failed; retrying",
				err,
				slog.Int("attempt", attempt),
				slog.Duration("retry_after", retry),
			)

			timer := time.NewTimer(retry)
			select {
			case <-reconcileCtx.Done():
				timer.Stop()

			case <-timer.C:
			}

			if retry < maxRetry {
				retry = min(retry*2, maxRetry)
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
	signalCancel := a.signalCancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if signalCancel != nil {
		signalCancel()
	}

	a.wg.Wait()
}

// reconcile evaluates the auto-redeem watermark once and signals the registry
// when an over-watermark balance is already sitting available. evalCtx bounds
// the external reads, while signalCtx owns the queued actor work and must live
// until auto-redeemer shutdown. The registry applies the no-pending-pay/redeem
// interlock before admitting the redeem, so this only has to clear the
// threshold against the earmark-adjusted balance.
func (a *autoRedeemer) reconcile(evalCtx, signalCtx context.Context) error {
	acctKey, err := a.daemon.IdentityPubKey(evalCtx)
	if err != nil {
		return fmt.Errorf("get identity pubkey: %w", err)
	}

	snapshot, err := a.server.ListCredits(evalCtx, acctKey)
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
			earmarked, err := (*earmarkFn)(evalCtx)
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

	if available == 0 || available < a.cfg.MinRedeemSat {
		return nil
	}

	threshold, err := a.threshold(evalCtx)
	if err != nil {
		return err
	}
	if available < threshold {
		return nil
	}

	a.logger(evalCtx).InfoS(evalCtx, "Boot reconcile signaling auto-redeem",
		slog.Uint64("available_sat", available),
		slog.Uint64("threshold_sat", threshold),
	)

	return a.registry.Tell(signalCtx, &ConsiderRedeemRequest{
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
