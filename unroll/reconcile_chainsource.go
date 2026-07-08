package unroll

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/lib/recovery"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// defaultReconcileProbeTimeout bounds how long the chainsource-backed
// reconciler waits for a confirmation or spend event before treating
// the probed anchor as no longer live. lnd's chainntnfs dispatches
// historical confirmation events immediately when the tx is in chain
// and the height-hint is fresh; legitimate "still confirmed" answers
// return well under a second on a healthy node, so a 10-second
// budget leaves comfortable headroom for slow restarts while still
// failing fast on truly absent anchors.
const defaultReconcileProbeTimeout = 10 * time.Second

// ChainSourceReconcilerConfig configures NewChainSourceReconciler.
type ChainSourceReconcilerConfig struct {
	// ChainSource is the actor ref used to issue RegisterConf /
	// RegisterSpend probes.
	ChainSource actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]

	// Proof is the immutable recovery graph the reconciler queries
	// pkScripts from. Required.
	Proof *recovery.Proof

	// CallerID prefix used when registering probes; the reconciler
	// appends per-probe suffixes so chainsource sees a unique key
	// per (caller, txid / outpoint, confs) tuple.
	//
	// Callers that share the underlying ChainSource between multiple
	// reconcilers MUST give each instance a unique prefix (typically
	// by including the per-actor target identity) so concurrent
	// probes of the same shared proof-graph txid do not collide on
	// the chainsource service-key namespace.
	CallerID string

	// ProbeTimeout bounds each individual confirmation / spend
	// probe. Zero falls back to defaultReconcileProbeTimeout.
	// Slow chainsource backends can legitimately need a higher
	// budget here: a probe that times out is treated as "not on
	// chain", which on restart triggers a conservative
	// re-broadcast / planner rollback. Operators running against
	// a slow backend should raise this to avoid spurious
	// rollbacks; see the timeout warn log emitted on miss for the
	// signal to watch for.
	ProbeTimeout time.Duration

	// CleanupTimeout bounds the best-effort unregister send that
	// runs when a probe is interrupted before its future fires.
	// Zero falls back to 5 seconds.
	CleanupTimeout time.Duration

	// Log is an optional logger. Timeout-as-absent answers are
	// surfaced at warn level here so a slow backend masquerading
	// as a reorged-out anchor is visible in production logs
	// instead of silently driving conservative rollbacks. Zero
	// falls back to btclog.Disabled.
	Log fn.Option[btclog.Logger]
}

// chainSourceReconciler is a ChainReconciler that answers queries by
// probing the chainsource actor with short-timeout RegisterConf and
// RegisterSpend requests in future mode.
//
// The implementation is intentionally simple: it does NOT consume a
// dedicated "is this tx in chain right now" API (chainsource does not
// expose one today). Instead it leans on lnd's chainntnfs behavior of
// dispatching a historical confirmation immediately when the watched
// tx is currently on chain. A timed-out probe is treated as "not on
// chain" — conservative for restart reconciliation, which is the only
// caller, because the worst-case false-negative is a re-broadcast of
// an already-confirmed proof node (idempotent thanks to txconfirm's
// txid-keyed dedup).
//
// Probes that complete on their own self-clean (the chainsource
// sub-actor exits after delivering the single positive event in
// future mode); probes that time out enqueue a best-effort
// UnregisterConfRequest on a fresh background context so the
// long-lived chainsource sub-actor does not leak per restart.
type chainSourceReconciler struct {
	chainSource actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]
	proof          *recovery.Proof
	callerID       string
	probeTimeout   time.Duration
	cleanupTimeout time.Duration
	log            btclog.Logger
}

// NewChainSourceReconciler constructs a chainsource-backed
// ChainReconciler. The proof is required because chainsource probes
// rely on (txid, pkScript) pairs for historical scans.
func NewChainSourceReconciler(cfg ChainSourceReconcilerConfig) ChainReconciler {
	if cfg.Proof == nil {
		return nil
	}
	probeTimeout := cfg.ProbeTimeout
	if probeTimeout <= 0 {
		probeTimeout = defaultReconcileProbeTimeout
	}
	cleanupTimeout := cfg.CleanupTimeout
	if cleanupTimeout <= 0 {
		cleanupTimeout = 5 * time.Second
	}

	return &chainSourceReconciler{
		chainSource:    cfg.ChainSource,
		proof:          cfg.Proof,
		callerID:       cfg.CallerID,
		probeTimeout:   probeTimeout,
		cleanupTimeout: cleanupTimeout,
		log:            cfg.Log.UnwrapOr(btclog.Disabled),
	}
}

// ConfirmedTx probes chainsource for the current confirmation status
// of txid. The probe registers a one-shot conf watch in future mode
// using the txid + first-output pkScript from the proof, then awaits
// the future with a bounded timeout.
func (r *chainSourceReconciler) ConfirmedTx(ctx context.Context,
	txid chainhash.Hash) (fn.Option[ConfirmedAnchor], error) {

	node, ok := r.proof.Node(txid)
	if !ok || node == nil || node.Tx == nil ||
		len(node.Tx.TxOut) == 0 {
		// Without an output script we cannot drive a historical
		// scan; treat as "unknown / not on chain" so reconciliation
		// stays conservative.
		return fn.None[ConfirmedAnchor](), nil
	}
	pkScript := append([]byte(nil), node.Tx.TxOut[0].PkScript...)

	callerID := fmt.Sprintf("%s-conf-%s", r.callerID, txid)
	probeCtx, cancel := context.WithTimeout(ctx, r.probeTimeout)
	defer cancel()

	resp, err := r.chainSource.Ask(
		probeCtx, &chainsource.RegisterConfRequest{
			CallerID:    callerID,
			Txid:        &txid,
			PkScript:    pkScript,
			TargetConfs: 1,
		},
	).Await(probeCtx).Unpack()
	if err != nil {
		return fn.None[ConfirmedAnchor](), fmt.Errorf("register conf "+
			"probe %s: %w", txid, err)
	}

	confResp, ok := resp.(*chainsource.RegisterConfResponse)
	if !ok || confResp.Future == nil {
		return fn.None[ConfirmedAnchor](), fmt.Errorf("register conf "+
			"probe %s: unexpected response %T", txid, resp)
	}

	// Schedule a best-effort cleanup unregister in case the probe
	// times out: the chainsource sub-actor would otherwise stay
	// alive watching for a confirmation that, by assumption, is
	// never coming. The cleanup runs on a fresh background context
	// so it executes even when the probe context was cancelled.
	probeTxid := txid
	probePkScript := pkScript
	//nolint:contextcheck // cleanup intentionally uses its own context
	defer r.cleanupConfWatch(callerID, &probeTxid, probePkScript)

	event, err := confResp.Future.Await(probeCtx).Unpack()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(err, context.Canceled) {
			// Timeout-as-absent is a conservative answer: the
			// reconciler tells the caller "not on chain" and
			// any persisted anchor will be rolled back. That is
			// safe but expensive when the real cause is a slow
			// backend rather than a vanished tx, so surface it
			// at warn level to make slow-backend regressions
			// visible without changing planner behavior.
			r.log.WarnS(ctx, "Reconciler conf probe timed out; "+
				"treating tx as not on chain", err,
				slog.String("txid", txid.String()),
				slog.Duration("timeout", r.probeTimeout),
			)

			return fn.None[ConfirmedAnchor](), nil
		}

		return fn.None[ConfirmedAnchor](), fmt.Errorf("await conf "+
			"probe %s: %w", txid, err)
	}

	return fn.Some(ConfirmedAnchor{
		Txid:   event.Txid,
		Height: event.BlockHeight,
	}), nil
}

// SpentOutpoint probes chainsource for the current spend status of
// outpoint. Symmetric to ConfirmedTx: registers a future-mode spend
// watch with the outpoint's pkScript and awaits the future with a
// bounded timeout, scheduling an UnregisterSpendRequest cleanup on
// the way out.
func (r *chainSourceReconciler) SpentOutpoint(ctx context.Context,
	outpoint wire.OutPoint) (fn.Option[SpendAnchor], error) {

	node, ok := r.proof.Node(outpoint.Hash)
	if !ok || node == nil || node.Tx == nil {
		return fn.None[SpendAnchor](), nil
	}
	if int(outpoint.Index) >= len(node.Tx.TxOut) {
		return fn.None[SpendAnchor](), nil
	}
	pkScript := append(
		[]byte(nil), node.Tx.TxOut[outpoint.Index].PkScript...,
	)

	callerID := fmt.Sprintf("%s-spend-%s", r.callerID, outpoint)
	probeCtx, cancel := context.WithTimeout(ctx, r.probeTimeout)
	defer cancel()

	probeOutpoint := outpoint
	resp, err := r.chainSource.Ask(
		probeCtx, &chainsource.RegisterSpendRequest{
			CallerID: callerID,
			Outpoint: &probeOutpoint,
			PkScript: pkScript,
		},
	).Await(probeCtx).Unpack()
	if err != nil {
		return fn.None[SpendAnchor](), fmt.Errorf("register spend "+
			"probe %s: %w", outpoint, err)
	}

	spendResp, ok := resp.(*chainsource.RegisterSpendResponse)
	if !ok || spendResp.Future == nil {
		return fn.None[SpendAnchor](), fmt.Errorf("register spend "+
			"probe %s: unexpected response %T", outpoint, resp)
	}

	//nolint:contextcheck // cleanup intentionally uses its own context
	defer r.cleanupSpendWatch(callerID, &probeOutpoint)

	event, err := spendResp.Future.Await(probeCtx).Unpack()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(err, context.Canceled) {
			r.log.WarnS(ctx, "Reconciler spend probe timed "+
				"out; treating outpoint as unspent", err,
				slog.String("outpoint", outpoint.String()),
				slog.Duration("timeout", r.probeTimeout),
			)

			return fn.None[SpendAnchor](), nil
		}

		return fn.None[SpendAnchor](), fmt.Errorf("await spend probe "+
			"%s: %w", outpoint, err)
	}

	return fn.Some(SpendAnchor{
		Outpoint:       event.Outpoint,
		SpendingTxid:   event.SpendingTxid,
		SpendingHeight: event.SpendingHeight,
	}), nil
}

// cleanupConfWatch sends a best-effort UnregisterConfRequest on a
// fresh background context so a timed-out probe does not leak the
// underlying chainsource sub-actor. Errors are intentionally ignored
// because the cleanup is purely a hygiene operation.
func (r *chainSourceReconciler) cleanupConfWatch(callerID string,
	txid *chainhash.Hash, pkScript []byte) {

	cleanupCtx, cancel := context.WithTimeout(
		context.Background(), r.cleanupTimeout,
	)
	defer cancel()

	_, _ = r.chainSource.Ask(
		cleanupCtx, &chainsource.UnregisterConfRequest{
			CallerID:    callerID,
			Txid:        txid,
			PkScript:    pkScript,
			TargetConfs: 1,
		},
	).Await(cleanupCtx).Unpack()
}

// cleanupSpendWatch is the spend-side analogue of cleanupConfWatch.
func (r *chainSourceReconciler) cleanupSpendWatch(callerID string,
	outpoint *wire.OutPoint) {

	cleanupCtx, cancel := context.WithTimeout(
		context.Background(), r.cleanupTimeout,
	)
	defer cancel()

	_, _ = r.chainSource.Ask(
		cleanupCtx, &chainsource.UnregisterSpendRequest{
			CallerID: callerID,
			Outpoint: outpoint,
		},
	).Await(cleanupCtx).Unpack()
}
