package wallet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/build"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/metrics"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

const (
	// BoardingKeyFamily is the BIP32 key family used for deriving boarding
	// address keys.
	BoardingKeyFamily = 42

	// MinBoardingConfs is the minimum number of confirmations required
	// before notifying about a boarding UTXO.
	MinBoardingConfs = 1

	// MaxConfsForListUnspent is the maximum confirmations parameter for
	// ListUnspent queries.
	MaxConfsForListUnspent = 9999999

	// defaultTipTickInterval is how often the wallet actor's tick loop
	// checks whether the most recently observed chain tip has advanced
	// past the last successfully-processed height. New boarding UTXO
	// detection latency is bounded above by one tick interval plus the
	// chain's own block cadence; a backend whose UTXO reporting lags
	// the block-epoch arrival is caught the next time the chain
	// advances, since each tip advance re-runs ListUnspent.
	// Configurable via WithTipTickInterval; tests pin this short to
	// keep assertions tight.
	defaultTipTickInterval = 1 * time.Second
)

// notifierInfo holds the configuration for a registered confirmation notifier.
type notifierInfo struct {
	// actor is the reference to send BoardingUtxoConfirmedEvent messages
	// to.
	actor actor.TellOnlyRef[BoardingUtxoConfirmedEvent]

	// minConf is the minimum number of confirmations required before
	// notifying this actor about a boarding UTXO.
	minConf uint32
}

// Ark manages boarding addresses and monitors for on-chain boarding UTXOs. It
// provides the primary interface for creating boarding addresses, tracking
// confirmations, and notifying registered actors (like the round actor) when
// new boarding opportunities are detected.
type Ark struct {
	// backend provides LND integration for key derivation, address import,
	// and UTXO enumeration.
	backend BoardingBackend

	// store persists boarding addresses and intents to the database.
	store BoardingStore

	// intentReplayers is the registry of pending-intent replayers,
	// one per PendingIntentKind. handleReplayPendingIntents walks this
	// set on the daemon's startup replay Ask to re-issue user intents
	// persisted before the last shutdown.
	intentReplayers []PendingIntentReplayer

	// vtxoReader provides read-only access to VTXO descriptors. The wallet
	// uses this to load VTXO data when building intent packages for round
	// registration (refresh and leave flows).
	vtxoReader VTXOReader

	// actorSystem is the actor system context for looking up actors by
	// service key. Used to find the round actor when forwarding intent
	// registration messages.
	actorSystem actor.SystemContext

	// chainSource provides block epoch notifications for polling.
	chainSource actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]

	// notifiers is the map of registered actors to notify of new confirmed
	// UTXOs. Each entry contains the actor reference and configuration.
	notifiers map[string]notifierInfo

	// seenUtxos tracks all UTXOs we've already processed to detect new
	// confirmations from ListUnspent.
	seenUtxos fn.Set[UtxoKey]

	// boardingShipped tracks boarding outpoints this session has already
	// handed to the round actor via TriggerBoardMsg and that have not yet
	// left the Confirmed set. handleBoard ships the full confirmed set on
	// every call, so without this guard a second deposit confirming while
	// an earlier round is still in flight would re-ship the earlier
	// outpoint, which the round actor would re-register under a freshly
	// derived owner key — two divergent registrations of one boarding
	// UTXO, surfacing as a quote pkScript-echo mismatch. The set is
	// in-memory on purpose: it is empty after a restart so the board
	// replayer still re-boards an outpoint stranded by a failed round.
	boardingShipped fn.Set[wire.OutPoint]

	// wg tracks background goroutines spawned by the wallet actor.
	wg sync.WaitGroup

	// ctx is the wallet's internal context, cancelled on shutdown. Used for
	// background goroutines that should respect wallet lifecycle.
	//
	//nolint:containedctx
	ctx context.Context

	// cancel cancels the internal context on shutdown.
	cancel context.CancelFunc

	// actorLog is an optional logger for this actor instance. When set, it
	// takes precedence over the context-based logger from
	// build.LoggerFromContext. When None, the actor falls back to the
	// context logger (or btclog.Disabled if none is found).
	actorLog fn.Option[btclog.Logger]

	// ledgerSink is an optional reference to the client-side ledger
	// accounting actor. When set, the wallet emits UTXOCreatedMsg /
	// UTXOSpentMsg events as on-chain wallet UTXOs come and go so
	// the local ledger sees a complete on-chain audit log alongside
	// the off-chain double-entry ledger. When None, audit emission
	// is silently skipped (tests, lightweight harnesses).
	ledgerSink fn.Option[ledger.Sink]

	// metricsSink is an optional reference to the client-side metrics
	// actor. When set, the boarding-sweep watcher emits a
	// BackgroundTaskErrorMsg as managed sweeps fail terminally so the
	// waved_background_task_errors_total counter carries signal for
	// this daemon-owned background task. When None (metrics disabled,
	// or tests), emission is silently skipped.
	metricsSink fn.Option[metrics.Sink]

	// selfRef is the actor's own ref, captured at Start time so the
	// boarding-sweep handlers can hand it to chainsource.MapSpendEvent
	// and txconfirm.MapNotification when registering watches.
	selfRef actor.TellOnlyRef[WalletMsg]

	// sweepStore persists boarding-sweep records for restart recovery
	// and inspection. nil disables the sweep subsystem.
	sweepStore BoardingSweepStore

	// sweepSigner builds witnesses for boarding-timeout aggregate sweep
	// transactions and allocates wallet-managed destination scripts.
	// nil disables the sweep subsystem.
	sweepSigner SweepSigner

	// sweepChainParams is the chain network parameters used to validate
	// caller-supplied sweep destination addresses.
	sweepChainParams *chaincfg.Params

	// walletSweeper is the backend surface used by the general
	// backing-wallet sweep flow (SweepWalletFundsRequest) to list
	// confirmed UTXOs, lease them, sign the aggregate sweep, and finalize
	// the PSBT. nil disables the general wallet-sweep subsystem; the
	// concrete adapter is the same per-backend type wired as sweepSigner,
	// reinterpreted through the broader WalletBackingSweeper interface.
	walletSweeper WalletBackingSweeper

	// walletSweepMaxFeeRate is the operator-configured fee-rate cap, in
	// sats/vByte, applied to general backing-wallet sweeps. Zero means no
	// operator cap is configured; the sweep handler then falls back to
	// txconfirm.DefaultMaxFeeRateSatPerVByte so the cap is never a no-op.
	walletSweepMaxFeeRate int64

	// pendingSweeps tracks in-flight aggregate boarding sweeps the
	// wallet actor is correlating spend / txconfirm notifications
	// against. Keyed by sweep txid. The map is owned by the actor's
	// single-threaded Receive loop — chainsource and txconfirm
	// notifications both arrive via the actor's mailbox, so no
	// additional mutex is required.
	pendingSweeps map[chainhash.Hash]*pendingSweepState

	// pendingSweepInputs is a per-outpoint reverse index that maps each
	// boarding-sweep input outpoint back to the sweep txid that owns it.
	// chainsource.handleRegisterSpend always Spawns a fresh SpendActor
	// per call, so duplicate registrations would leak goroutines. The
	// map is owned by the actor's single-threaded Receive loop and is
	// kept in lockstep with each pendingSweepState.inputs entry.
	pendingSweepInputs map[wire.OutPoint]chainhash.Hash

	// clk is the clock used to stamp persistence timestamps. Tests pass
	// a deterministic clock via WithClock; production wires the
	// server-wide clock instance so all stores share one source of time.
	clk clock.Clock

	// eagerRoundJoin, when true, makes the wallet drive round-joining
	// without waiting for an explicit Board/LeaveVTXOs RPC handshake.
	// Two behaviors change:
	//
	//   - On every freshly confirmed boarding UTXO, the wallet runs the
	//     standard handleBoard path inline so the confirmation directly
	//     produces a TriggerBoardMsg for the round actor.
	//   - Cooperative-leave intents are submitted with
	//     TriggerRegistration=true so the round FSM advances out of
	//     PendingRoundAssembly immediately instead of waiting for a
	//     batched trigger.
	//
	// Wallet-shaped SDK hosts (wavewalletdk) opt in so a "deposit funds"
	// or "leave" interaction joins a round end-to-end without the host
	// having to chase the second RPC.
	eagerRoundJoin bool

	// tipTickInterval is how often runTipTickLoop fires a
	// ProcessTipTickNotification self-Tell to drive per-tip work
	// (ListUnspent + boarding-sweep resume kick). Zero falls back to
	// defaultTipTickInterval at Start time.
	tipTickInterval time.Duration

	// latestKnownTip is the most recent chain tip observed via a
	// BlockEpochNotification Tell. The block-epoch handler stores into
	// this atomic ptr with no other work, so a burst of N notifications
	// completes in microseconds and does not saturate the mailbox.
	// handleProcessTipTick reads the value to decide whether the tip
	// has advanced since processedTipHeight and the heavy per-tip work
	// needs to run.
	latestKnownTip atomic.Pointer[chainsource.BlockEpoch]

	// processedTipHeight is the height the wallet has successfully
	// completed a per-tip work pass for. A tick whose latestKnownTip
	// height equals processedTipHeight short-circuits with no work, so
	// the steady-state idle cost of the tick loop is one atomic load
	// per tick.
	processedTipHeight atomic.Int32

	// tickInflight is set to true when a ProcessTipTickNotification
	// has been enqueued and not yet processed. runTipTickLoop
	// compare-and-swaps this flag to skip firing a duplicate tick
	// when the previous one is still queued, capping ProcessTipTick
	// mailbox depth at 1. Without this cap, a tick handler that
	// runs longer than tipTickInterval (~2 s when ListUnspent races
	// against retries) would let ticker.C accumulate excess ticks
	// in the mailbox, push out room for self-Tells from inside the
	// handler, and degrade the boarding-sweep resume cadence.
	tickInflight atomic.Bool

	// fetchOperatorKey, when set, returns the operator's current
	// long-term public key by issuing a fresh GetInfo round-trip at
	// the moment a refresh intent is composed. The fetched key is
	// used to build the NEW VTXO output's policy template inside
	// handleRefreshVTXOs; the input VTXO's stored key is intentionally
	// not reused for the new output because VTXOs commit to their
	// operator key for life and the new output's key is chosen at
	// join time. Nil leaves the handler falling back to the
	// descriptor's stored bytes for harness paths and for non-standard
	// policy shapes where the rebuild surface is unavailable.
	fetchOperatorKey func(context.Context) (*btcec.PublicKey, error)

	// fetchOperatorTerms, when set, returns the operator's current
	// terms snapshot so boarding can enforce the advertised per-VTXO
	// maximum and total user balance cap. Nil disables limit clamping
	// entirely (harness paths and operators that advertise no caps).
	fetchOperatorTerms func(context.Context) (*types.OperatorTerms, error)

	// fetchLiveBalance, when set, returns the wallet's current live
	// VTXO balance. Boarding uses it (together with adopted boarding
	// intents) to compute the remaining headroom under the operator's
	// maximum user balance. Nil skips the balance term, so only the
	// per-VTXO maximum applies.
	fetchLiveBalance func(context.Context) (btcutil.Amount, error)
}

// NewArk creates a new Ark wallet actor. The logger is optional and falls back
// to the global package logger when nil is passed.
//
// The vtxoReader provides read-only VTXO descriptor access so the wallet can
// compose intent packages for round registration. The actorSystem enables
// round actor lookup via service key. The ledgerSink is required (use fn.None
// to opt out); it is plumbed as a mandatory argument so every call site must
// make an explicit choice about accounting emission rather than silently
// skipping it.
func NewArk(backend BoardingBackend, store BoardingStore, vtxoReader VTXOReader,
	chainSource actor.ActorRef[
		chainsource.ChainSourceMsg,
		chainsource.ChainSourceResp,
	],
	actorSystem actor.SystemContext, ledgerSink fn.Option[ledger.Sink],
	actorLog btclog.Logger, opts ...ArkOption) *Ark {

	// Wrap the provided logger in an Option. A nil logger becomes None,
	// causing the actor to fall back to build.LoggerFromContext at call
	// sites via the logger() helper.
	optLog := fn.None[btclog.Logger]()
	if actorLog != nil {
		optLog = fn.Some(actorLog)
	}

	a := &Ark{
		backend:         backend,
		store:           store,
		vtxoReader:      vtxoReader,
		chainSource:     chainSource,
		actorSystem:     actorSystem,
		ledgerSink:      ledgerSink,
		notifiers:       make(map[string]notifierInfo),
		seenUtxos:       fn.NewSet[UtxoKey](),
		boardingShipped: fn.NewSet[wire.OutPoint](),
		actorLog:        optLog,
		pendingSweeps:   make(map[chainhash.Hash]*pendingSweepState),
		pendingSweepInputs: make(
			map[wire.OutPoint]chainhash.Hash,
		),
		clk: clock.NewDefaultClock(),
	}

	// Register the pending-intent replayers. Each kind persisted in the
	// intent outbox needs exactly one replayer here, or persisted rows
	// of that kind would silently never be replayed after a restart.
	a.intentReplayers = []PendingIntentReplayer{
		&boardIntentReplayer{
			ark: a,
		},
		&sendOnChainIntentReplayer{
			ark: a,
		},
	}

	for _, opt := range opts {
		opt(a)
	}

	return a
}

// ArkOption tunes optional dependencies of the wallet actor. Subsystems that
// are not strictly required to start the actor (such as the boarding-sweep
// flow) are wired through ArkOption-style functional options so test
// harnesses can construct a minimal *Ark without manufacturing every
// downstream collaborator.
type ArkOption func(*Ark)

// WithBoardingSweep wires the boarding-sweep subsystem into the wallet
// actor. When omitted, the corresponding RPC paths return a clear
// "subsystem not initialised" error rather than silently no-oping.
//
// The shared txconfirm broadcaster is resolved lazily through the
// receptionist via txconfirm.LookupRef so callers do not need to
// guarantee actor-init ordering; a sweep request issued before the
// broadcaster has been registered surfaces an explicit error rather
// than silently dropping into the dead-letter queue.
func WithBoardingSweep(store BoardingSweepStore, signer SweepSigner,
	chainParams *chaincfg.Params) ArkOption {

	return func(a *Ark) {
		a.sweepStore = store
		a.sweepSigner = signer
		a.sweepChainParams = chainParams
	}
}

// WithWalletSweep wires the general backing-wallet sweep subsystem into the
// wallet actor. When omitted, SweepWalletFundsRequest returns a clear
// "subsystem not initialised" error rather than silently no-oping.
//
// The backing argument is the same per-backend adapter the daemon wires as
// the boarding-sweep signer; it satisfies WalletBackingSweeper structurally
// because it already implements txconfirm.Wallet. The maxFeeRateSatPerVByte
// is the operator's configured fee-rate cap (zero falls back to
// txconfirm.DefaultMaxFeeRateSatPerVByte at sweep time so the cap is never a
// no-op).
func WithWalletSweep(backing WalletBackingSweeper,
	maxFeeRateSatPerVByte int64) ArkOption {

	return func(a *Ark) {
		a.walletSweeper = backing
		a.walletSweepMaxFeeRate = maxFeeRateSatPerVByte
	}
}

// WithMetricsSink wires the client-side metrics actor into the wallet so
// the boarding-sweep watcher can report terminal sweep failures as a
// background-task error. When omitted (the default), the metrics sink
// stays None and emission is a silent no-op, matching the opt-in metrics
// design. Production passes fn.Some(metrics.NewSink(actorSystem)) only
// when metrics are enabled.
func WithMetricsSink(sink fn.Option[metrics.Sink]) ArkOption {
	return func(a *Ark) {
		a.metricsSink = sink
	}
}

// WithClock overrides the wallet's clock with a caller-supplied instance.
// Production wires this with the daemon-wide clock so persist timestamps
// share one source of truth; tests use this to freeze time. When omitted,
// the wallet falls back to clock.NewDefaultClock().
func WithClock(clk clock.Clock) ArkOption {
	return func(a *Ark) {
		a.clk = clk
	}
}

// WithEagerRoundJoin makes the wallet drive round-joining without waiting
// for a follow-up Board or LeaveVTXOs RPC handshake. Freshly confirmed
// boarding UTXOs run the standard handleBoard path inline, and
// cooperative-leave intents are forwarded with TriggerRegistration=true
// so the round FSM advances out of PendingRoundAssembly immediately.
// Opt in from wallet-shaped SDK hosts that want a single user action to
// translate into a full round join; leave off for daemons whose hosts
// drive the second RPC themselves (e.g. wavecli).
func WithEagerRoundJoin(enabled bool) ArkOption {
	return func(a *Ark) {
		a.eagerRoundJoin = enabled
	}
}

// WithTipTickInterval overrides the wallet actor's tip-tick cadence.
// Production typically wants the default (one second is short enough
// that new boarding UTXO detection feels responsive while idle cost
// stays at one atomic load per tick); test harnesses that need a
// tight assertion timeline pin this to 50–100 ms. Zero or negative
// values fall back to defaultTipTickInterval at Start time.
func WithTipTickInterval(d time.Duration) ArkOption {
	return func(a *Ark) {
		a.tipTickInterval = d
	}
}

// WithFetchOperatorKey wires a closure that fetches the operator's
// current long-term public key (via a fresh GetInfo round-trip) into
// the wallet actor. The closure is invoked when composing refresh
// intents so the NEW VTXO output's policy template is built against
// the operator's join-time key, rather than the daemon-startup cache
// (which goes stale across rotations) or the descriptor's stored K1
// (which would defeat the rotation entirely). Tests and harnesses
// can omit the option to keep the legacy fallback to the descriptor's
// stored template.
func WithFetchOperatorKey(
	fetch func(context.Context) (*btcec.PublicKey, error)) ArkOption {

	return func(a *Ark) {
		a.fetchOperatorKey = fetch
	}
}

// WithFetchOperatorTerms wires a closure that fetches the operator's
// current terms snapshot into the wallet actor. Boarding uses the terms
// to clamp board requests to the advertised per-VTXO maximum and total
// user balance cap, clipping any excess back on-chain via a change
// leave output. Tests and harnesses can omit the option to board
// without limits.
func WithFetchOperatorTerms(
	fetch func(context.Context) (*types.OperatorTerms, error)) ArkOption {

	return func(a *Ark) {
		a.fetchOperatorTerms = fetch
	}
}

// WithFetchLiveBalance wires a closure that returns the wallet's
// current live VTXO balance into the wallet actor. Boarding combines
// the balance with adopted boarding intents to compute the remaining
// headroom under the operator's maximum user balance.
func WithFetchLiveBalance(
	fetch func(context.Context) (btcutil.Amount, error)) ArkOption {

	return func(a *Ark) {
		a.fetchLiveBalance = fetch
	}
}

// logger returns the configured logger or falls back to extracting from
// context. If no logger is found in either location, returns btclog.Disabled.
func (a *Ark) logger(ctx context.Context) btclog.Logger {
	return a.actorLog.UnwrapOr(build.LoggerFromContext(ctx))
}

// composeRefreshTemplate returns the policy template that the explicit
// RefreshVTXOs handler should attach to the new VTXO output for a given
// input descriptor, using the operator key already resolved for the
// surrounding batch (see handleRefreshVTXOs).
//
// Mirrors the vtxo-side refreshOutputTemplate helper but takes the
// resolved key as a parameter so the caller can hoist a single GetInfo
// round-trip out of the per-outpoint loop.
//
// A nil currentOperatorKey means the surrounding handler is running
// without the fetchOperatorKey seam (harness path) or with a deliberate
// opt-out: the descriptor's stored bytes are returned verbatim. Non-
// standard policy shapes trigger the same fallback because the rebuild
// surface only covers the standard shape today.
func composeRefreshTemplate(vtxo *VTXODescriptor,
	currentOperatorKey *btcec.PublicKey) ([]byte, error) {

	if currentOperatorKey == nil {
		return vtxo.EffectivePolicyTemplate()
	}

	rebuilt, err := vtxo.RefreshOutputTemplate(currentOperatorKey)
	if err != nil {
		if errors.Is(err, ErrRefreshOperatorKeyUnsupported) {
			return vtxo.EffectivePolicyTemplate()
		}

		return nil, err
	}

	return rebuilt, nil
}

// emitBackgroundTaskError reports a failure in a daemon-owned
// background task to the metrics actor so the
// waved_background_task_errors_total counter advances, labelled by
// task. The boarding-sweep watcher is such a task: it runs
// independently of any RPC, so a terminal sweep failure has no caller
// to surface it. Emission is best-effort and fire-and-forget — a Tell
// failure is logged at debug level and never blocks the path that
// observed the error. A None sink (metrics disabled, tests) is a silent
// no-op.
func (a *Ark) emitBackgroundTaskError(ctx context.Context, task string) {
	a.metricsSink.WhenSome(func(sink metrics.Sink) {
		msg := &metrics.BackgroundTaskErrorMsg{Task: task}
		if err := sink.Tell(ctx, msg); err != nil {
			a.logger(ctx).DebugS(
				ctx,
				"Failed to emit background task error metric",
				err,
				slog.String("task", task),
			)
		}
	})
}

// emitUTXOCreated posts a UTXOCreatedMsg to the client ledger
// actor when the wallet observes a new on-chain UTXO. The ledger
// handler persists an audit row in wallet_utxo_log and, for
// deposit-like classifications or boarding sweep returns, records
// the corresponding double-entry ledger row.
//
// Classification is supplied by the caller because the wallet
// actor alone cannot always tell whether a UTXO is a deposit, a
// change output from a round, or a sweep return -- that context
// lives with whichever subsystem triggered the underlying tx.
// Emission is guarded by the nil-safe fn.Option[ledger.Sink] and
// Tell failures are logged but not propagated so a momentary
// ledger outage never blocks the confirmation path.
func (a *Ark) emitUTXOCreated(ctx context.Context, utxo *Utxo,
	blockHeight int32, classification string) {

	a.ledgerSink.WhenSome(func(sink ledger.Sink) {
		if utxo == nil {
			return
		}

		var height uint32
		if blockHeight > 0 {
			height = uint32(blockHeight)
		}

		msg := &ledger.UTXOCreatedMsg{
			OutpointHash:   utxo.Outpoint.Hash,
			OutpointIndex:  utxo.Outpoint.Index,
			AmountSat:      int64(utxo.Amount),
			BlockHeight:    height,
			Classification: classification,
		}

		if err := sink.Tell(ctx, msg); err != nil {
			a.logger(ctx).WarnS(
				ctx,
				"Failed to emit UTXOCreatedMsg to ledger",
				err,
				btclog.Fmt("outpoint", "%v", utxo.Outpoint),
				slog.Int64("amount_sat", int64(utxo.Amount)),
				slog.String("classification", classification),
			)
		}
	})
}

// allTargetErrors builds a per-outpoint error map for operations that fail
// before the wallet can inspect individual VTXOs.
func allTargetErrors(targets []wire.OutPoint,
	err error) map[wire.OutPoint]error {

	errors := make(map[wire.OutPoint]error, len(targets))
	for _, outpoint := range targets {
		errors[outpoint] = err
	}

	return errors
}

// Start initializes the actor and subscribes to block epochs. The selfRef
// parameter is the actor's own reference, used to receive block epoch
// notifications from the chainsource actor.
func (a *Ark) Start(ctx context.Context,
	selfRef actor.TellOnlyRef[WalletMsg]) error {

	// Capture the self ref so boarding-sweep handlers can register
	// chainsource spend watches and txconfirm subscribers that route
	// notifications back to this actor.
	a.selfRef = selfRef

	// Create an internal context for background goroutines that outlive
	// request contexts but should respect wallet shutdown.
	a.ctx, a.cancel = context.WithCancel(context.Background())

	// Load existing addresses from database to populate seenUtxos for
	// restart recovery.
	addresses, err := a.store.ListAllBoardingAddresses(ctx)
	if err != nil {
		return fmt.Errorf("load existing addresses: %w", err)
	}

	a.logger(ctx).InfoS(ctx, "Loaded boarding addresses from database",
		slog.Int("count", len(addresses)),
	)

	// Re-import each persisted boarding address into the boarding
	// backend. For in-memory backends (lwwallet), this restores
	// tracked scripts lost on restart. For persistent backends
	// (LND), the script may already exist and the import will
	// return a benign "already exists" error which we log and
	// skip since the backend is already tracking the address.
	for _, addr := range addresses {
		_, err := a.backend.ImportTaprootScript(
			ctx, addr.Tapscript,
		)
		if err != nil {
			// The import may fail if the backend already
			// tracks this script (e.g., LND persists
			// imports internally and returns "already
			// exists"). This is expected during restart
			// recovery so we log and continue.
			a.logger(ctx).DebugS(ctx,
				"Boarding address re-import skipped",
				slog.String("address",
					addr.Address.String()),
				slog.String("reason",
					err.Error()))
		}
	}

	// Load just the outpoints of existing intents to populate seenUtxos.
	// This is more efficient than loading full intents since we only need
	// the outpoints to avoid duplicate notifications.
	outpoints, err := a.store.FetchBoardingIntentOutpoints(ctx)
	if err != nil {
		return fmt.Errorf("load existing intent outpoints: %w", err)
	}

	// Add each outpoint to our seen UTXO map to ensure we don't make
	// duplicate notifications.
	for _, outpoint := range outpoints {
		key := NewUtxoKey(outpoint)
		a.seenUtxos.Add(key)
	}

	a.logger(ctx).InfoS(ctx, "Loaded existing boarding intents",
		slog.Int("count", len(outpoints)),
	)

	// Subscribe to block epochs from chainsource using notify pattern. Map
	// BlockEpoch messages to BlockEpochNotification for our actor.
	epochRef := chainsource.MapBlockEpoch(selfRef,
		func(epoch chainsource.BlockEpoch) WalletMsg {
			return BlockEpochNotification{BlockEpoch: epoch}
		},
	)
	req := &chainsource.SubscribeBlocksRequest{
		CallerID:    "boarding-wallet",
		NotifyActor: fn.Some(epochRef),
	}

	future := a.chainSource.Ask(ctx, req)
	result := future.Await(ctx)
	if result.IsErr() {
		return fmt.Errorf("subscribe to block epochs: %w", result.Err())
	}

	// Boarding-sweep resume is intentionally NOT dispatched from
	// Start. The wallet starts before txconfirm registers (step 9 vs
	// step 12 of waved.Server.startWalletDependentActors), so a
	// self-Tell here would race the receptionist registration and a
	// scheduling-unlucky resume would observe txconfirm.LookupRef as
	// "not found", silently orphaning every persisted pending sweep.
	// The daemon explicitly Asks the wallet to resume after step 12.

	// Replay of any persisted pending intent is intentionally NOT
	// dispatched here. The wallet starts before the round-client actor
	// registers with the receptionist, so a self-Tell from Start would
	// land the replayed intent's downstream round dispatch against an
	// unresolved service key and silently drop the replay. The daemon
	// explicitly Asks the wallet to replay (ReplayPendingIntentsRequest)
	// once the round-client actor is up, mirroring the
	// resumeBoardingSweeps startup-ordering fix.

	// Start the tip-tick loop that drives per-tip work off the
	// block-epoch hot path. handleBlockEpoch only records the latest
	// tip into latestKnownTip; this loop is what fires the deferred
	// ListUnspent / processUtxo / sweep-resume work via a self-Tell
	// on a configurable cadence (defaultTipTickInterval, or whatever
	// WithTipTickInterval was set to).
	interval := a.tipTickInterval
	if interval <= 0 {
		interval = defaultTipTickInterval
	}
	a.wg.Add(1)
	go a.runTipTickLoop(interval)

	a.logger(ctx).InfoS(ctx, "Boarding wallet actor started",
		slog.Duration("tip_tick_interval", interval),
	)

	return nil
}

// runTipTickLoop fires a ProcessTipTickNotification self-Tell at the
// supplied cadence until the wallet's internal context is cancelled.
// The tickInflight compare-and-swap caps mailbox depth at one pending
// tick: a tick handler that takes longer than the interval (a few
// seconds is common when ListUnspent races against retries) would
// otherwise let ticker.C events accumulate and drown out self-Tells
// the handler makes for boarding-sweep resume kicks. Transient errors
// from the Tell are debug-logged but never escalated: the next tick
// will retry, and any backlog the actor is processing will catch up on
// its own. A terminated-actor error is the one exception — the mailbox
// is gone for good, so the loop returns instead of spinning, because
// the actor system can terminate the actor without invoking Stop()
// (which is what cancels a.ctx), and a dead ref will never recover.
func (a *Ark) runTipTickLoop(interval time.Duration) {
	defer a.wg.Done()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-a.ctx.Done():
			return

		case <-ticker.C:
			if !a.tickInflight.CompareAndSwap(false, true) {
				// Previous tick still queued or being
				// processed. Skip this tick — the queued
				// one will pick up the latest tip.
				continue
			}

			err := a.selfRef.Tell(
				a.ctx, ProcessTipTickNotification{},
			)
			if err != nil {
				a.tickInflight.Store(false)

				// A terminated actor never recovers, so stop
				// the loop rather than respin and re-log every
				// tick against a dead mailbox.
				if errors.Is(err, actor.ErrActorTerminated) {
					return
				}

				a.logger(a.ctx).DebugS(
					a.ctx,
					"Failed to schedule tip tick",
					slog.String("err", err.Error()),
				)
			}
		}
	}
}

// Stop gracefully shuts down the wallet actor by unsubscribing from block
// notifications, tearing down per-input chainsource spend watches owned by
// pending boarding sweeps, and waiting for any in-flight backlog deliveries
// to complete. The spend-watch cleanup is explicit (rather than implicit via
// actor-system shutdown) so callers that stop the wallet without tearing
// down the whole system leave no dangling chainsource sub-actors.
func (a *Ark) Stop(ctx context.Context) {
	a.logger(ctx).InfoS(ctx, "Stopping boarding wallet actor")

	// Cancel the internal context to signal background goroutines to stop.
	if a.cancel != nil {
		a.cancel()
	}

	for _, pending := range a.pendingSweeps {
		a.cancelSweepSpendWatches(ctx, pending)
	}

	err := a.chainSource.Tell(ctx, &chainsource.UnsubscribeBlocksRequest{
		CallerID: "boarding-wallet",
	})
	if err != nil {
		a.logger(ctx).WarnS(ctx, "Failed to unsubscribe blocks", err)
	}

	a.wg.Wait()

	a.logger(ctx).InfoS(ctx, "Boarding wallet actor stopped")
}

// Receive processes incoming messages using the actor pattern.
func (a *Ark) Receive(ctx context.Context,
	msg WalletMsg) fn.Result[WalletResp] {

	switch m := msg.(type) {
	case *CreateBoardingAddressRequest:
		return a.handleCreateBoardingAddress(ctx, m)

	case *GetActiveBoardingAddressesRequest:
		return a.handleGetActiveBoardingAddresses(ctx, m)

	case *GetBoardingBalanceRequest:
		return a.handleGetBoardingBalance(ctx, m)

	case *RegisterConfirmationNotifierRequest:
		return a.handleRegisterNotifier(ctx, m)

	case *GetConfirmedBoardingIntentsRequest:
		return a.handleGetConfirmedBoardingIntents(ctx, m)

	case *UnregisterConfirmationNotifierRequest:
		return a.handleUnregisterNotifier(ctx, m)

	case BlockEpochNotification:
		return a.handleBlockEpoch(ctx, m.BlockEpoch)

	case ProcessTipTickNotification:
		return a.handleProcessTipTick(ctx)

	case *RefreshVTXOsRequest:
		return a.handleRefreshVTXOs(ctx, m)

	case *RefreshCustomVTXOsRequest:
		return a.handleRefreshCustomVTXOs(ctx, m)

	case *DropCustomRefreshVTXOsRequest:
		return a.handleDropCustomRefreshVTXOs(ctx, m)

	case *LeaveVTXOsRequest:
		return a.handleLeaveVTXOs(ctx, m)

	case *BoardRequest:
		return a.handleBoard(ctx, m)

	case *SelectAndLockVTXOsRequest:
		return a.handleSelectAndLockVTXOs(ctx, m)

	case *UnlockVTXOsRequest:
		return a.handleUnlockVTXOs(ctx, m)

	case *CompleteSpendVTXOsRequest:
		return a.handleCompleteSpendVTXOs(ctx, m)

	case *SendVTXOsRequest:
		return a.handleSendVTXOs(ctx, m)

	case *SendOnChainRequest:
		return a.handleSendOnChain(ctx, m)

	case *ReplaySendOnChainIntent:
		return a.handleReplaySendOnChainIntent(ctx, m)

	case *SweepBoardingUTXOsRequest:
		return a.handleSweepBoardingUTXOs(ctx, m)

	case *SweepWalletFundsRequest:
		return a.handleSweepWalletFunds(ctx, m)

	case WalletSweepTxNotification:
		return a.handleWalletSweepTxNotification(ctx, m)

	case *ResumeBoardingSweepsRequest:
		return a.handleResumeBoardingSweeps(ctx, m)

	case *ReplayPendingIntentsRequest:
		return a.handleReplayPendingIntents(ctx, m)

	case BoardingSweepSpendNotification:
		return a.handleSweepSpendNotification(ctx, m)

	case BoardingSweepTxNotification:
		return a.handleSweepTxNotification(ctx, m)

	default:
		return fn.Err[WalletResp](
			fmt.Errorf("unknown message type: %T", msg),
		)
	}
}

// handleCreateBoardingAddress derives a new key, constructs a boarding
// tapscript, imports it into LND, and persists the address.
func (a *Ark) handleCreateBoardingAddress(ctx context.Context,
	req *CreateBoardingAddressRequest) fn.Result[WalletResp] {

	// Grab a fresh key from lnd, then create the boarding tapscript given
	// the current operator information.
	keyDesc, err := a.backend.DeriveNextKey(ctx, BoardingKeyFamily)
	if err != nil {
		return fn.Err[WalletResp](fmt.Errorf("derive key: %w", err))
	}
	tapscript, err := buildBoardingTapscript(
		keyDesc.PubKey, req.OperatorKey, req.ExitDelay,
	)
	if err != nil {
		return fn.Err[WalletResp](
			fmt.Errorf("build tapscript: %w", err),
		)
	}

	// We'll now import the address into lnd which will enable us to view
	// the credits to the address using list unspent, etc.
	address, err := a.backend.ImportTaprootScript(ctx, tapscript)
	if err != nil {
		return fn.Err[WalletResp](
			fmt.Errorf("import taproot script: %w", err),
		)
	}

	// With the address created, we'll now write the new boarding address
	// to disk.
	boardingAddr := &BoardingAddress{
		Address:     address,
		Tapscript:   tapscript,
		KeyDesc:     *keyDesc,
		OperatorKey: req.OperatorKey,
		ExitDelay:   req.ExitDelay,
	}
	err = a.store.InsertBoardingAddress(ctx, boardingAddr)
	if err != nil {
		return fn.Err[WalletResp](
			fmt.Errorf("persist address: %w", err),
		)
	}

	a.logger(ctx).InfoS(ctx, "Created new boarding address",
		slog.String("address", address.String()),
		slog.Int("exit_delay", int(req.ExitDelay)),
	)

	resp := &CreateBoardingAddressResponse{
		Address:   address,
		ClientKey: keyDesc.PubKey,
	}

	return fn.Ok[WalletResp](resp)
}

// handleGetActiveBoardingAddresses queries all boarding addresses from the
// database.
func (a *Ark) handleGetActiveBoardingAddresses(ctx context.Context,
	_ *GetActiveBoardingAddressesRequest) fn.Result[WalletResp] {

	addresses, err := a.store.ListAllBoardingAddresses(ctx)
	if err != nil {
		return fn.Err[WalletResp](
			fmt.Errorf("list addresses: %w", err),
		)
	}

	resp := &GetActiveBoardingAddressesResponse{
		Addresses: addresses,
	}

	return fn.Ok[WalletResp](resp)
}

// handleGetBoardingBalance queries boarding intents in their
// monitoring-relevant statuses (confirmed / adopted / sweep_pending / swept)
// and sums them. Adopted totals keep funds visible after a round checkpoint
// spends the boarding UTXO but before the resulting VTXO confirms.
// Sweep-pending and swept totals power the boarding_pending_sweep_sat and
// boarding_swept_sat fields exposed through GetBalance, so dashboards see
// boarding funds in flight even while a sweep tx awaits confirmation.
//
// CONTRACT: this handler must remain a pure read of the boarding
// store. `waved.RPCServer.fetchBoardingBalance` deliberately
// duplicates this logic at the RPC layer to bypass the wallet
// actor's serial mailbox under block-epoch catch-up bursts (see
// BUGS_FOUND.md). If you add in-memory caching, admission gating,
// or any other actor-local state to the computation here, the
// bypass at the RPC layer must be updated in lockstep — otherwise
// GetBalance will silently report a different value than this Ask.
func (a *Ark) handleGetBoardingBalance(ctx context.Context,
	_ *GetBoardingBalanceRequest) fn.Result[WalletResp] {

	confirmed, err := a.store.FetchBoardingIntentsByStatus(
		ctx, BoardingStatusConfirmed,
	)
	if err != nil {
		return fn.Err[WalletResp](
			fmt.Errorf("fetch confirmed intents: %w", err),
		)
	}

	adopted, err := a.store.FetchBoardingIntentsByStatus(
		ctx, BoardingStatusAdopted,
	)
	if err != nil {
		return fn.Err[WalletResp](
			fmt.Errorf("fetch adopted intents: %w", err),
		)
	}

	pendingSweep, err := a.store.FetchBoardingIntentsByStatus(
		ctx, BoardingStatusSweepPending,
	)
	if err != nil {
		return fn.Err[WalletResp](
			fmt.Errorf("fetch sweep-pending intents: %w", err),
		)
	}

	swept, err := a.store.FetchBoardingIntentsByStatus(
		ctx, BoardingStatusSwept,
	)
	if err != nil {
		return fn.Err[WalletResp](
			fmt.Errorf("fetch swept intents: %w", err),
		)
	}

	sumAmounts := func(intents []BoardingIntent) btcutil.Amount {
		var total btcutil.Amount
		for _, intent := range intents {
			total += intent.ChainInfo.Amount
		}

		return total
	}

	unconfirmed, unconfirmedCount, err := a.unconfirmedBoardingBalance(ctx)
	if err != nil {
		return fn.Err[WalletResp](
			fmt.Errorf("fetch unconfirmed boarding balance: %w",
				err),
		)
	}

	resp := &GetBoardingBalanceResponse{
		TotalBalance:         sumAmounts(confirmed),
		UtxoCount:            len(confirmed),
		UnconfirmedBalance:   unconfirmed,
		UnconfirmedUtxoCount: unconfirmedCount,
		AdoptedBalance:       sumAmounts(adopted),
		PendingSweepBalance:  sumAmounts(pendingSweep),
		SweptBalance:         sumAmounts(swept),
	}

	return fn.Ok[WalletResp](resp)
}

// unconfirmedBoardingBalance sums zero-conf backend UTXOs that pay to known
// boarding scripts.
func (a *Ark) unconfirmedBoardingBalance(ctx context.Context) (btcutil.Amount,
	int, error) {

	utxos, err := a.backend.ListUnspent(ctx, 0, MaxConfsForListUnspent)
	if err != nil {
		return 0, 0, fmt.Errorf("list unspent: %w", err)
	}

	var total btcutil.Amount
	var count int
	for _, utxo := range utxos {
		if utxo == nil || utxo.Confirmations != 0 {
			continue
		}

		addr, err := a.store.LookupBoardingAddress(
			ctx, utxo.PkScript,
		)
		if err != nil || addr == nil {
			continue
		}

		total += utxo.Amount
		count++
	}

	return total, count, nil
}

// handleRegisterNotifier adds an actor to the notification list and optionally
// sends backlog events.
func (a *Ark) handleRegisterNotifier(ctx context.Context,
	req *RegisterConfirmationNotifierRequest) fn.Result[WalletResp] {

	// Reject duplicate registrations. Callers must unregister first before
	// re-registering with the same ID.
	if _, exists := a.notifiers[req.NotifierID]; exists {
		return fn.Err[WalletResp](
			fmt.Errorf("notifier already registered: %s",
				req.NotifierID),
		)
	}

	// Use the caller's minConf if specified, otherwise use the default.
	minConf := req.MinConf.UnwrapOr(MinBoardingConfs)

	a.notifiers[req.NotifierID] = notifierInfo{
		actor:   req.NotifyActor,
		minConf: minConf,
	}

	a.logger(ctx).InfoS(ctx, "Registered confirmation notifier",
		slog.String("notifier_id", req.NotifierID),
		slog.Int("min_conf", int(minConf)),
	)

	// If a backlog is needed, send it asynchronously so we don't block
	// the registration response. We use the wallet's internal context since
	// the request context will be cancelled after Ask returns, but we still
	// want the backlog goroutine to respect wallet shutdown.
	req.BacklogHeight.WhenSome(func(height int32) {
		a.wg.Go(func() {
			a.sendBacklog(a.ctx, req.NotifyActor, height)
		})
	})

	resp := &RegisterConfirmationNotifierResponse{
		Success: true,
	}

	return fn.Ok[WalletResp](resp)
}

// handleGetConfirmedBoardingIntents returns the wallet's currently confirmed
// boarding intents. This gives the round actor a restart-safe way to rebuild
// pending boarding input packages from the wallet's persisted state.
//
// Each loaded intent is run through maybeRebuildBoardingProof so legacy rows
// (pre-migration 000010) and rows with a corrupt persisted blob recover a
// usable SPV TxProof before the round actor ships the boarding request to
// the operator. Without this, a synchronous Board RPC issued in the
// post-restart window against an unhealed row would propagate
// `TxProof=None` to the operator and fail with "TxProof is required when
// server has no chain source".
func (a *Ark) handleGetConfirmedBoardingIntents(ctx context.Context,
	_ *GetConfirmedBoardingIntentsRequest) fn.Result[WalletResp] {

	intents, err := a.store.FetchBoardingIntentsByStatus(
		ctx, BoardingStatusConfirmed,
	)
	if err != nil {
		return fn.Err[WalletResp](
			fmt.Errorf("fetch confirmed boarding intents: %w", err),
		)
	}

	for i := range intents {
		a.maybeRebuildBoardingProof(ctx, &intents[i])
	}

	return fn.Ok[WalletResp](&GetConfirmedBoardingIntentsResponse{
		Intents: intents,
	})
}

// handleUnregisterNotifier removes an actor from the notification list.
func (a *Ark) handleUnregisterNotifier(ctx context.Context,
	req *UnregisterConfirmationNotifierRequest) fn.Result[WalletResp] {

	_, existed := a.notifiers[req.NotifierID]
	delete(a.notifiers, req.NotifierID)

	a.logger(ctx).InfoS(ctx, "Unregistered confirmation notifier",
		slog.String("notifier_id", req.NotifierID),
		slog.Bool("existed", existed),
	)

	resp := &UnregisterConfirmationNotifierResponse{
		Success: existed,
	}

	return fn.Ok[WalletResp](resp)
}

// handleBlockEpoch records the latest observed chain tip and returns
// immediately. The heavy per-tip work (ListUnspent, processUtxo,
// boarding-sweep resume kick) runs on the tip-tick handler, so a
// burst of N block notifications completes in microseconds rather
// than queuing N×(ListUnspent + retries) of serial work behind every
// other Ask the actor needs to service. See handleProcessTipTick for
// the deferred work and bug-2 in the shared BUGS_FOUND.md for the
// rationale; the longer-term latency tradeoff is documented on
// defaultTipTickInterval.
func (a *Ark) handleBlockEpoch(ctx context.Context,
	epoch chainsource.BlockEpoch) fn.Result[WalletResp] {

	// Copy the value so the atomic ptr points at a stable backing
	// allocation rather than the loop-scoped argument.
	stored := epoch
	a.latestKnownTip.Store(&stored)

	a.logger(ctx).TraceS(ctx, "Recorded new chain tip",
		slog.Int("height", int(epoch.Height)),
	)

	return fn.Ok[WalletResp](nil)
}

// handleProcessTipTick runs the per-tip work for the most recently
// observed chain tip when it has advanced past the last successfully-
// processed height. Idempotent and short-circuits cheaply when the
// tip has not moved, which is the steady-state once catch-up is
// complete. See handleBlockEpoch for why the work was hoisted off the
// per-block path.
func (a *Ark) handleProcessTipTick(ctx context.Context) fn.Result[WalletResp] {
	// Clear the inflight latch on every exit path so the next
	// ticker.C event can fire a fresh tick. Pair-bonded with the
	// CompareAndSwap(false, true) gate in runTipTickLoop; together
	// they cap pending-tick mailbox depth at 1.
	defer a.tickInflight.Store(false)

	tip := a.latestKnownTip.Load()
	if tip == nil {

		// No block epochs observed yet — nothing to do.
		return fn.Ok[WalletResp](nil)
	}

	// Pin the tip locally so a concurrent block-epoch handler
	// updating the atomic ptr after we read it does not introduce
	// height/hash skew inside this work pass.
	epoch := *tip

	processed := a.processedTipHeight.Load()
	if epoch.Height <= processed {

		// Already caught up to this tip.
		return fn.Ok[WalletResp](nil)
	}

	a.logger(ctx).InfoS(ctx, "Processing new chain tip",
		slog.Int("height", int(epoch.Height)),
		slog.Int("last_processed", int(processed)),
	)

	utxos, err := a.backend.ListUnspent(
		ctx, MinBoardingConfs, MaxConfsForListUnspent,
	)
	if err != nil {
		a.logger(ctx).WarnS(ctx, "Failed listing UTXOs",
			err,
			slog.Int("height", int(epoch.Height)),
		)

		// Don't advance processedTipHeight — the next tick will
		// retry against whatever the tip is then.
		return fn.Ok[WalletResp](nil)
	}

	// For each UTXO, we'll check if it's new and belongs to a fresh
	// boarding intent, dispatching notifications if needed.
	var foundNew bool
	for _, utxo := range utxos {
		if a.processUtxo(ctx, epoch, utxo) {
			foundNew = true
		}
	}

	a.logger(ctx).InfoS(ctx, "ListUnspent returned UTXOs",
		slog.Int("height", int(epoch.Height)),
		slog.Int("utxo_count", len(utxos)),
	)

	// Eager round-join: when at least one new boarding UTXO confirmed
	// in this tip advance, drive the normal Board path inline so the
	// user does not have to chase a follow-up RPC. We coalesce per
	// tip (not per UTXO) so multiple confirmations in the same tip
	// produce one round join with all of them. handleBoard reads the
	// current confirmed boarding set from the store, so it will pick
	// up every UTXO that processUtxo just persisted above.
	if foundNew && a.eagerRoundJoin {
		boardRes := a.handleBoard(ctx, &BoardRequest{}).Unpack
		if _, err := boardRes(); err != nil {
			a.logger(ctx).WarnS(
				ctx,
				"Eager board on boarding confirmation failed",
				err,
				slog.Int("height", int(epoch.Height)),
			)
		}
	}

	// Kick a boarding-sweep resume retry on each successful tip
	// advance when the subsystem is enabled. The handler is
	// idempotent: fully-recovered sweeps short-circuit on the
	// in-memory pendingSweeps lookup (M-4), so the steady-state cost
	// is one ListPendingBoardingSweeps query per tip advance. Sweeps
	// that failed to fully recover during the initial resume
	// (transient chainsource Ask failure, GetIntent error, txconfirm
	// submit error) get re-attempted here without operator
	// intervention.
	//
	// Safe to block on the self-Tell: the runTipTickLoop inflight
	// latch caps pending-tick mailbox depth at one, so the only
	// other producer is the chain source (one BlockEpoch per actual
	// block, at most ~1 every few hundred ms even under regtest
	// burst). The mailbox has plenty of headroom for one more
	// fire-and-forget Tell during the handler.
	if a.boardingSweepEnabled() {
		err := a.selfRef.Tell(
			ctx, &ResumeBoardingSweepsRequest{},
		)
		if err != nil {
			a.logger(ctx).DebugS(ctx,
				"Failed to schedule boarding sweep resume "+
					"retry", err)
		}
	}

	// Mark the tip processed. We deliberately advance even when
	// foundNew is false: a backend whose UTXO reporting lags past
	// scan time will surface the missing UTXO on the next tip
	// advance (the next block's tick re-runs ListUnspent), which is
	// the same coverage Roasbeef's review preferred over an inline
	// multi-tick retry budget. Tests that need lag tolerance can
	// drive successive tips explicitly.
	a.processedTipHeight.Store(epoch.Height)

	return fn.Ok[WalletResp](nil)
}

// processUtxo checks if a UTXO is new and belongs to a boarding address.
func (a *Ark) processUtxo(ctx context.Context, epoch chainsource.BlockEpoch,
	utxo *Utxo) bool {

	// Make sure we haven't already seen this UTXO.
	key := NewUtxoKey(utxo.Outpoint)
	if a.seenUtxos.Contains(key) {
		return false
	}

	// Check if this UTXO pays to a boarding address.
	addr, err := a.store.LookupBoardingAddress(ctx, utxo.PkScript)
	if err != nil {

		// Not a boarding address, ignore.
		return false
	}

	// New boarding UTXO detected!
	a.logger(ctx).InfoS(ctx, "Detected new boarding UTXO",
		btclog.Fmt("outpoint", "%v", utxo.Outpoint),
		slog.Int("amount", int(utxo.Amount)),
		slog.Int("height", int(epoch.Height)),
	)

	// Fetch the full transaction and its confirmation metadata. The
	// TxInfo block hash and height reflect the UTXO's actual
	// confirmation block, which may differ from the epoch during
	// catch-up after downtime.
	txInfo, err := a.backend.GetTransaction(
		ctx, utxo.Outpoint.Hash,
	)
	if err != nil {
		a.logger(ctx).WarnS(ctx, "Failed fetching boarding tx",
			err,
			btclog.Fmt("txid", "%v", utxo.Outpoint.Hash),
		)

		return false
	}

	// Use the confirmation block hash and height from the
	// transaction if available. Fall back to epoch values for
	// backends that don't provide confirmation metadata (e.g.,
	// unconfirmed or missing details).
	blockHash := epoch.Hash
	blockHeight := epoch.Height
	if txInfo.BlockHash != nil {
		blockHash = *txInfo.BlockHash
		blockHeight = txInfo.BlockHeight
	}

	// Build the SPV TxProof so the server can verify the boarding
	// UTXO without querying its own chain source.
	txProof := a.buildBoardingTxProof(
		ctx, blockHash, blockHeight, txInfo.Tx, utxo.Outpoint, addr,
	)

	intent := BoardingIntent{
		Address:  *addr,
		Outpoint: utxo.Outpoint,
		ChainInfo: BoardingChainInfo{
			ConfHeight: blockHeight,
			ConfHash:   blockHash,
			ConfTx:     txInfo.Tx,
			OutPoint:   utxo.Outpoint,
			Amount:     utxo.Amount,
			TxProof:    txProof,
		},
		Status: BoardingStatusConfirmed,
	}

	// Persist first - only mark seen and notify if DB succeeds. On failure
	// we'll retry on the next block when ListUnspent returns this UTXO
	// again.
	err = a.store.InsertBoardingIntents(ctx, intent)
	if err != nil {
		a.logger(ctx).WarnS(ctx, "Failed persisting boarding intent",
			err,
			btclog.Fmt("outpoint", "%v", utxo.Outpoint),
		)

		return false
	}

	a.seenUtxos.Add(key)

	// Mirror the confirmation into the client ledger so the UTXO
	// audit log has a deposit row alongside the double-entry
	// bookkeeping. Classification is ClassificationDeposit because
	// the detection path above filtered for UTXOs paying to a
	// known boarding address -- other classifications (change,
	// sweep_return) belong to different emission sites and are
	// not applicable here.
	a.emitUTXOCreated(ctx, utxo, blockHeight,
		ledger.ClassificationDeposit)

	// Notify registered actors that meet the confirmation threshold.
	event := BoardingUtxoConfirmedEvent{
		BoardingIntent: &intent,
	}
	for _, notifier := range a.notifiers {
		if uint32(utxo.Confirmations) >= notifier.minConf {
			if err := notifier.actor.Tell(ctx, event); err != nil {
				a.logger(ctx).WarnS(
					ctx,
					"Notify confirmation failed",
					err,
				)
			}
		}
	}

	return true
}

// sendBacklog sends recent confirmations to a newly registered notifier. It
// queries confirmed boarding intents and delivers events for those confirmed
// at or after the specified height, allowing newly registered actors to catch
// up on missed confirmations.
func (a *Ark) sendBacklog(ctx context.Context,
	notifier actor.TellOnlyRef[BoardingUtxoConfirmedEvent],
	fromHeight int32) {

	// Query confirmed intents with height filter applied at the database
	// level for efficiency.
	intents, err := a.store.FetchBoardingIntentsByStatusAndMinHeight(
		ctx, BoardingStatusConfirmed, fromHeight,
	)
	if err != nil {
		a.logger(ctx).WarnS(ctx, "Failed fetching confirmed intents",
			err,
		)

		return
	}

	for i := range intents {
		intent := &intents[i]

		// Rebuild the SPV TxProof when the persisted row pre-dates
		// the tx_proof migration (or carried a corrupt blob that
		// decoded to None). The rebuild also re-persists the proof
		// so future loads hydrate it directly. Best-effort: any
		// failure leaves the proof None and we deliver the event
		// anyway, matching pre-fix behavior.
		a.maybeRebuildBoardingProof(ctx, intent)

		event := BoardingUtxoConfirmedEvent{
			BoardingIntent: intent,
		}

		if err := notifier.Tell(ctx, event); err != nil {
			a.logger(ctx).WarnS(ctx, "Backlog delivery failed",
				err,
			)
		}
	}

	a.logger(ctx).InfoS(ctx, "Backlog delivery completed",
		slog.Int("from_height", int(fromHeight)),
		slog.Int("events_sent", len(intents)),
	)
}

// maybeRebuildBoardingProof reconstructs a missing SPV TxProof on a boarding
// intent loaded from the persisted store. It is a no-op when the intent
// already carries a proof or when the persisted row is missing the data
// needed to rebuild (ConfTx/ConfHash/Tapscript). On a successful rebuild
// the proof is stamped onto the intent in place AND persisted back to the
// row so subsequent reads serve the rebuilt proof directly without paying
// the chain-backend cost again. On failure (rebuild or re-persist) the
// in-memory intent is left as best-effort: the caller still ships whatever
// the rebuild produced, matching pre-fix behavior.
//
// This recovers two failure populations: (a) rows written before migration
// 000010 (no persisted proof), and (b) rows whose persisted blob failed
// TLV decode (corrupted on disk; the read path logs and falls through to
// None). Both classes are healed at the next read that touches a consumer
// invoking this helper (sendBacklog and handleGetConfirmedBoardingIntents
// today).
func (a *Ark) maybeRebuildBoardingProof(ctx context.Context,
	intent *BoardingIntent) {

	if intent == nil || intent.ChainInfo.TxProof.IsSome() {
		return
	}

	if intent.ChainInfo.ConfTx == nil {
		return
	}

	zeroHash := chainhash.Hash{}
	if intent.ChainInfo.ConfHash == zeroHash {
		return
	}

	rebuilt := a.buildBoardingTxProof(
		ctx, intent.ChainInfo.ConfHash, intent.ChainInfo.ConfHeight,
		intent.ChainInfo.ConfTx, intent.Outpoint, &intent.Address,
	)
	if rebuilt.IsNone() {
		return
	}

	intent.ChainInfo.TxProof = rebuilt

	a.logger(ctx).InfoS(ctx, "Rebuilt TxProof for boarding intent",
		btclog.Fmt("outpoint", "%v", intent.Outpoint),
		slog.Int("conf_height", int(intent.ChainInfo.ConfHeight)),
	)

	// Re-persist the intent so subsequent loads hydrate the rebuilt
	// proof directly. The InsertBoardingIntents upsert uses
	// COALESCE-with-NULLIF on tx_proof, so if this write races with a
	// concurrent status update neither side clobbers a non-empty proof.
	// Best-effort: a transient store error (e.g. shutdown mid-flight)
	// is logged but does not fail the caller's event delivery, since
	// the next backlog or board read will retry the rebuild.
	if err := a.store.InsertBoardingIntents(ctx, *intent); err != nil {
		a.logger(ctx).WarnS(ctx, "Failed persisting rebuilt TxProof",
			err,
			btclog.Fmt("outpoint", "%v", intent.Outpoint),
		)
	}
}

// handleRefreshVTXOs processes a request to refresh VTXOs. The wallet loads
// each VTXO descriptor, builds a forfeit + VTXO request pair, and sends the
// composed intent package to the round actor via RegisterIntentMsg. The round
// actor validates, registers with the FSM, and notifies VTXO actors.
//
// If some VTXOs fail to load but at least one succeeds, the successful
// forfeits are still submitted to the round (partial participation). The
// caller should check Errors in the response to detect partial failures.
func (a *Ark) handleRefreshVTXOs(ctx context.Context,
	req *RefreshVTXOsRequest) fn.Result[WalletResp] {

	a.logger(ctx).InfoS(ctx, "Received VTXO refresh request",
		slog.Int("target_count", len(req.TargetOutpoints)),
		slog.Bool("force_refresh", req.ForceRefresh),
	)

	if a.actorSystem == nil {
		a.logger(ctx).WarnS(
			ctx, "No actor system for refresh", nil,
		)

		return fn.Ok[WalletResp](&RefreshVTXOsResponse{
			Errors: make(map[wire.OutPoint]error),
		})
	}

	if a.vtxoReader == nil {
		err := fmt.Errorf("VTXO reader not configured")
		a.logger(ctx).WarnS(ctx, "No VTXO reader for refresh", err)

		return fn.Ok[WalletResp](&RefreshVTXOsResponse{
			Errors: allTargetErrors(req.TargetOutpoints, err),
		})
	}

	// Resolve the operator key once for the whole batch. Every new VTXO
	// minted in this round commits to the same key, so a single fresh
	// GetInfo round-trip is enough — per-outpoint fetches would just
	// fan the same answer out N times and could disagree under a
	// concurrent rotation. A fetch error fails the whole RPC: refreshing
	// against an unknown key would just queue a doomed intent. When the
	// seam is unset (harness paths) resolvedOperatorKey stays nil and
	// composeRefreshTemplate falls back to the descriptor's stored bytes.
	var resolvedOperatorKey *btcec.PublicKey
	if a.fetchOperatorKey != nil {
		key, err := a.fetchOperatorKey(ctx)
		if err != nil {
			a.logger(ctx).WarnS(
				ctx,
				"Failed to fetch current operator key for "+
					"refresh",
				err,
			)

			return fn.Ok[WalletResp](&RefreshVTXOsResponse{
				Errors: allTargetErrors(
					req.TargetOutpoints,
					fmt.Errorf("fetch current "+
						"operator key: %w", err),
				),
			})
		}

		if key == nil {
			err := fmt.Errorf("operator key fetch returned nil")
			a.logger(ctx).WarnS(
				ctx,
				"Refresh aborted: nil operator key",
				err,
			)

			return fn.Ok[WalletResp](&RefreshVTXOsResponse{
				Errors: allTargetErrors(
					req.TargetOutpoints, err,
				),
			})
		}

		resolvedOperatorKey = key
	}

	// Build intent package by loading each VTXO descriptor and
	// constructing the corresponding forfeit + VTXO request pair.
	var (
		forfeits []types.ForfeitRequest
		vtxos    []types.VTXORequest
		errors   = make(map[wire.OutPoint]error)
	)

	for _, outpoint := range req.TargetOutpoints {
		vtxo, err := a.vtxoReader.GetVTXO(ctx, outpoint)
		if err != nil {
			a.logger(ctx).WarnS(ctx,
				"Failed to load VTXO for refresh",
				err,
				slog.String("outpoint",
					outpoint.String()))

			errors[outpoint] = err

			continue
		}

		// Validate the per-input operator fee up front so a
		// rejected outpoint never lands in the forfeits slice
		// without a matching VTXO request. A mixed-outcome
		// request with one valid and one invalid outpoint would
		// otherwise ship a Forfeits/VTXOs mismatch to
		// RegisterIntentMsg and reserve a VTXO into
		// PendingForfeitState with no replacement.
		//
		// composeRefreshTemplate uses the join-time operator key
		// resolved above to rebuild the new output's template.
		// The resolved key is nil when the fetchOperatorKey seam
		// is unwired, in which case the helper falls back to the
		// descriptor's stored bytes (harness paths and non-standard
		// policy shapes).
		policyTemplate, err := composeRefreshTemplate(
			vtxo, resolvedOperatorKey,
		)
		if err != nil {
			a.logger(ctx).WarnS(ctx,
				"Failed to load refresh policy",
				err,
				slog.String("outpoint",
					outpoint.String()))

			errors[outpoint] = err

			continue
		}

		// Under the #270 seal-time fee handshake the client no
		// longer subtracts an operator fee from the new VTXO at
		// intent-compose time: the target amount carries the
		// pre-fee value, and exactly one output across the FULL
		// composed intent (boarding + refresh + leave + directed
		// send) is marked IsChange=true to absorb the residual
		// (Σin − Σ(fixed) − fee) at seal time. The marker is
		// stamped centrally by the FSM's IntentRequested handler
		// (designateChangeMarker) over the fully-accumulated
		// intent — stamping here using `len(vtxos) == 0` would
		// only see this RPC's batch and produce two markers when
		// two RefreshVTXOs RPCs land back-to-back during the
		// same PendingRoundAssembly window.
		op := vtxo.Outpoint
		forfeits = append(forfeits, types.ForfeitRequest{
			VTXOOutpoint: &op,
			Amount:       vtxo.Amount,
		})
		vtxos = append(vtxos, types.VTXORequest{
			PolicyTemplate: policyTemplate,
			Amount:         vtxo.Amount,
			OwnerKey:       vtxo.ClientKey,
			SigningKey:     vtxo.ClientKey,
			// Refresh output: the new VTXO is funded by the
			// client's own forfeited VTXO (not by wallet or
			// an external party). Origin drives the ledger
			// emission to SourceRoundRefresh so the
			// VTXOReceived credit cancels the paired VTXOSent
			// debit on transfers_out, leaving only the
			// operator fee as the net vtxo_balance change.
			Origin: types.VTXOOriginRoundRefresh,
		})
	}

	// Reserve the forfeit inputs through the VTXO manager before
	// sending the intent to the round actor. This ensures the VTXO
	// actors are in PendingForfeitState before round registration,
	// preventing split-brain where the round has an intent for a
	// VTXO that is still Live or claimed for OOR spend.
	if len(forfeits) > 0 {
		reserveOutpoints := make(
			[]wire.OutPoint, 0, len(forfeits),
		)
		for _, f := range forfeits {
			if f.VTXOOutpoint != nil {
				reserveOutpoints = append(
					reserveOutpoints, *f.VTXOOutpoint,
				)
			}
		}

		_, err := a.askManager(
			ctx, &actormsg.ReserveForfeitRequest{
				Outpoints: reserveOutpoints,
			},
		)
		if err != nil {
			a.logger(ctx).WarnS(
				ctx,
				"Manager rejected refresh reservation",
				err,
			)

			return fn.Err[WalletResp](
				fmt.Errorf("reserve refresh inputs: %w", err),
			)
		}

		// Send the intent to the round actor. If registration
		// fails, release the forfeit reservation so VTXOs
		// return to LiveState.
		serviceKey := actormsg.RoundActorServiceKey()
		roundRef := serviceKey.Ref(a.actorSystem)

		future := roundRef.Ask(ctx, &actormsg.RegisterIntentMsg{
			Forfeits: forfeits,
			VTXOs:    vtxos,
		})
		result := future.Await(ctx)
		if result.IsErr() {
			a.logger(ctx).WarnS(
				ctx,
				"Round rejected refresh intent",
				result.Err(),
			)

			a.releaseManagerForfeit(
				ctx, reserveOutpoints,
			)

			return fn.Err[WalletResp](
				fmt.Errorf(
					"round rejected refresh intent: %w",
					result.Err(),
				),
			)
		}
	}

	a.logger(ctx).InfoS(ctx, "Registered refresh intent package",
		slog.Int("forfeits", len(forfeits)),
		slog.Int("vtxos", len(vtxos)),
		slog.Int("errors", len(errors)),
	)

	resp := &RefreshVTXOsResponse{
		RefreshingCount: len(forfeits),
		Errors:          errors,
	}

	return fn.Ok[WalletResp](resp)
}

// handleRefreshCustomVTXOs registers a caller-composed custom refresh package
// without selecting wallet-managed live VTXOs. This path exists for swap
// vHTLCs and similar custom-policy VTXOs that are tracked by their owning
// protocol state machine rather than by the wallet balance manager. The wallet
// still activates temporary PendingForfeit VTXO actors so the round can deliver
// the connector-bound forfeit signing request to a local signer.
func (a *Ark) handleRefreshCustomVTXOs(ctx context.Context,
	req *RefreshCustomVTXOsRequest) fn.Result[WalletResp] {

	a.logger(ctx).InfoS(ctx, "Received custom VTXO refresh request",
		slog.Int("input_count", len(req.Inputs)),
		slog.Int("output_count", len(req.Outputs)),
	)

	if a.actorSystem == nil {
		err := fmt.Errorf("actor system not configured")
		a.logger(ctx).WarnS(ctx, "No actor system for custom refresh",
			err,
		)

		return fn.Err[WalletResp](err)
	}

	if len(req.Inputs) == 0 {
		return fn.Err[WalletResp](
			fmt.Errorf("custom refresh inputs are empty"),
		)
	}
	if len(req.Inputs) != len(req.Outputs) {
		return fn.Err[WalletResp](
			fmt.Errorf(
				"custom refresh inputs/output count "+
					"mismatch: %d inputs, %d outputs",
				len(req.Inputs), len(req.Outputs),
			),
		)
	}

	forfeits := make([]types.ForfeitRequest, 0, len(req.Inputs))
	vtxos := make([]types.VTXORequest, 0, len(req.Outputs))
	customInputs := make(
		[]actormsg.CustomForfeitInput, 0, len(req.Inputs),
	)
	customOutpoints := make([]wire.OutPoint, 0, len(req.Inputs))

	for i := range req.Inputs {
		input := req.Inputs[i]
		output := req.Outputs[i]

		customInputs = append(customInputs, actormsg.CustomForfeitInput{
			Outpoint: input.Outpoint,
			Amount:   input.Amount,
			PkScript: append([]byte(nil), input.PkScript...),
			PolicyTemplate: append(
				[]byte(nil), input.PolicyTemplate...,
			),
			ClientKey:      input.ClientKey,
			OperatorKey:    input.OperatorKey,
			RelativeExpiry: input.RelativeExpiry,
			RoundID:        input.RoundID,
			CommitmentTxID: input.CommitmentTxID,
			BatchExpiry:    input.BatchExpiry,
			ChainDepth:     input.ChainDepth,
			CreatedHeight:  input.CreatedHeight,
			Ancestry:       input.Ancestry,
		})
		customOutpoints = append(customOutpoints, input.Outpoint)

		op := input.Outpoint
		forfeits = append(forfeits, types.ForfeitRequest{
			VTXOOutpoint: &op,
			Amount:       input.Amount,
			AuthSpend:    input.AuthSpend,
			ForfeitSpend: input.ForfeitSpend,
		})
		vtxos = append(vtxos, types.VTXORequest{
			PolicyTemplate: append(
				[]byte(nil), output.PolicyTemplate...,
			),
			PkScript:    append([]byte(nil), output.PkScript...),
			Amount:      output.Amount,
			FixedAmount: output.FixedAmount,
			Origin:      types.VTXOOriginRoundRefresh,
		})
	}

	_, err := a.askManager(
		ctx, &actormsg.ActivateCustomForfeitInputsRequest{
			Inputs: customInputs,
		},
	)
	if err != nil {
		a.logger(ctx).WarnS(
			ctx,
			"Manager rejected custom forfeit activation",
			err,
		)

		return fn.Err[WalletResp](
			fmt.Errorf("activate custom refresh inputs: %w", err),
		)
	}

	serviceKey := actormsg.RoundActorServiceKey()
	roundRef := serviceKey.Ref(a.actorSystem)

	future := roundRef.Ask(ctx, &actormsg.RegisterIntentMsg{
		Forfeits: forfeits,
		VTXOs:    vtxos,
	})
	result := future.Await(ctx)
	if result.IsErr() {
		a.logger(ctx).WarnS(ctx, "Round rejected custom refresh intent",
			result.Err(),
		)

		a.dropManagerCustomForfeits(ctx, customOutpoints)

		return fn.Err[WalletResp](
			fmt.Errorf(
				"round rejected custom refresh intent: %w",
				result.Err(),
			),
		)
	}

	a.logger(ctx).InfoS(ctx, "Registered custom refresh intent package",
		slog.Int("forfeits", len(forfeits)),
		slog.Int("vtxos", len(vtxos)),
	)

	return fn.Ok[WalletResp](&RefreshCustomVTXOsResponse{
		RefreshingCount: len(forfeits),
	})
}

func (a *Ark) dropManagerCustomForfeits(ctx context.Context,
	outpoints []wire.OutPoint) int {

	if len(outpoints) == 0 {
		return 0
	}

	resp, err := a.askManager(
		ctx, &actormsg.DropCustomForfeitInputsRequest{
			Outpoints: outpoints,
		},
	)
	if err != nil {
		a.logger(ctx).WarnS(ctx, "Failed to drop custom forfeit inputs",
			err,
			slog.Int("count", len(outpoints)),
		)

		return 0
	}

	dropResp, ok := resp.(*actormsg.DropCustomForfeitInputsResponse)
	if !ok {
		a.logger(ctx).WarnS(ctx, "Unexpected custom forfeit drop "+
			"response type", fmt.Errorf("got %T", resp))

		return 0
	}

	return dropResp.DroppedCount
}

func (a *Ark) handleDropCustomRefreshVTXOs(ctx context.Context,
	req *DropCustomRefreshVTXOsRequest) fn.Result[WalletResp] {

	dropped := a.dropManagerCustomForfeits(ctx, req.Outpoints)

	return fn.Ok[WalletResp](&DropCustomRefreshVTXOsResponse{
		DroppedCount: dropped,
	})
}

// handleLeaveVTXOs processes a leave (offboard) request. The wallet loads each
// VTXO descriptor, builds a forfeit + leave request pair, and sends the
// composed intent package to the round actor via RegisterIntentMsg. The round
// actor validates, registers with the FSM, and notifies VTXO actors.
//
// If some VTXOs fail to load but at least one succeeds, the successful
// forfeits are still submitted (partial participation). The caller should
// check Errors in the response to detect partial failures.
//
// Each target produces its own LeaveRequest whose pkScript is taken
// from req.DestOutputs[outpoint] (per-outpoint override) when the
// entry is set, falling back to the singular req.DestOutput when it
// is not. A target with no destination on either side surfaces a
// per-outpoint error instead of panicking — the RPC layer is
// responsible for guaranteeing coverage before dispatch. The server
// creates a separate on-chain output for each leave; it does not
// aggregate them.
func (a *Ark) handleLeaveVTXOs(ctx context.Context,
	req *LeaveVTXOsRequest) fn.Result[WalletResp] {

	a.logger(ctx).InfoS(ctx, "Received VTXO leave request",
		slog.Int("target_count", len(req.TargetOutpoints)),
	)

	if a.actorSystem == nil {
		a.logger(ctx).WarnS(
			ctx, "No actor system for leave", nil,
		)

		return fn.Ok[WalletResp](&LeaveVTXOsResponse{
			Errors: make(map[wire.OutPoint]error),
		})
	}

	if a.vtxoReader == nil {
		err := fmt.Errorf("VTXO reader not configured")
		a.logger(ctx).WarnS(ctx, "No VTXO reader for leave", err)

		return fn.Ok[WalletResp](&LeaveVTXOsResponse{
			Errors: allTargetErrors(req.TargetOutpoints, err),
		})
	}

	// Build intent package by loading each VTXO descriptor and
	// constructing the corresponding forfeit + leave request pair.
	var (
		forfeits []types.ForfeitRequest
		leaves   []*types.LeaveRequest
		errors   = make(map[wire.OutPoint]error)
	)

	for _, outpoint := range req.TargetOutpoints {
		vtxo, err := a.vtxoReader.GetVTXO(ctx, outpoint)
		if err != nil {
			a.logger(ctx).WarnS(ctx,
				"Failed to load VTXO for leave",
				err,
				slog.String("outpoint",
					outpoint.String()))

			errors[outpoint] = err

			continue
		}

		// Under the #270 seal-time fee handshake the leave
		// output carries the forfeited VTXO's full target
		// amount; exactly one output across the FULL composed
		// intent (boarding + refresh + leave + directed send)
		// is marked IsChange=true to absorb the residual
		// (Σin − Σ(fixed) − fee) at seal time. The marker is
		// stamped centrally by the FSM's IntentRequested handler
		// (designateChangeMarker) over the fully-accumulated
		// intent — stamping here using `len(leaves) == 0` would
		// only see this RPC's batch and produce two markers when
		// two LeaveVTXOs RPCs land back-to-back during the same
		// PendingRoundAssembly window.
		op := outpoint

		// Pick the destination: per-outpoint override from
		// DestOutputs takes precedence so a single batch can
		// offboard to distinct on-chain targets; otherwise the
		// caller's singular DestOutput applies. A missing entry
		// on both sides is a misuse by the RPC layer (which is
		// responsible for guaranteeing every target has a
		// destination before dispatch), so surface a clean
		// per-outpoint error rather than panicking.
		leaveOutput := req.DestOutputs[op]
		if leaveOutput == nil {
			leaveOutput = req.DestOutput
		}
		if leaveOutput == nil {
			errors[outpoint] = fmt.Errorf("no destination for "+
				"outpoint %s", outpoint)

			continue
		}

		forfeits = append(forfeits, types.ForfeitRequest{
			VTXOOutpoint: &op,
			Amount:       vtxo.Amount,
		})
		leaves = append(leaves, &types.LeaveRequest{
			Output: &wire.TxOut{
				PkScript: leaveOutput.PkScript,
				Value:    int64(vtxo.Amount),
			},
		})
	}

	// Reserve the forfeit inputs through the VTXO manager before
	// sending the intent to the round actor. This ensures the VTXO
	// actors are in PendingForfeitState before round registration.
	if len(forfeits) > 0 {
		reserveOutpoints := make(
			[]wire.OutPoint, 0, len(forfeits),
		)
		for _, f := range forfeits {
			if f.VTXOOutpoint != nil {
				reserveOutpoints = append(
					reserveOutpoints, *f.VTXOOutpoint,
				)
			}
		}

		_, err := a.askManager(
			ctx, &actormsg.ReserveForfeitRequest{
				Outpoints: reserveOutpoints,
			},
		)
		if err != nil {
			a.logger(ctx).WarnS(
				ctx,
				"Manager rejected leave reservation",
				err,
			)

			return fn.Err[WalletResp](
				fmt.Errorf("reserve leave inputs: %w", err),
			)
		}

		// Send the intent to the round actor. If registration
		// fails, release the forfeit reservation so VTXOs
		// return to LiveState.
		serviceKey := actormsg.RoundActorServiceKey()
		roundRef := serviceKey.Ref(a.actorSystem)

		// Under eager round-join the leave is treated as an
		// interactive operation: trigger registration immediately
		// so the round FSM leaves PendingRoundAssembly without
		// waiting for an external nudge. The default
		// (TriggerRegistration=false) preserves the batched-leave
		// semantics that operator-driven hosts rely on.
		future := roundRef.Ask(ctx, &actormsg.RegisterIntentMsg{
			Forfeits:            forfeits,
			Leaves:              leaves,
			TriggerRegistration: a.eagerRoundJoin,
		})
		result := future.Await(ctx)
		if result.IsErr() {
			a.logger(ctx).WarnS(ctx, "Round rejected leave intent",
				result.Err(),
			)

			a.releaseManagerForfeit(
				ctx, reserveOutpoints,
			)

			return fn.Err[WalletResp](
				fmt.Errorf(
					"round rejected leave intent: %w",
					result.Err(),
				),
			)
		}
	}

	a.logger(ctx).InfoS(ctx, "Registered leave intent package",
		slog.Int("forfeits", len(forfeits)),
		slog.Int("leaves", len(leaves)),
		slog.Int("errors", len(errors)),
	)

	resp := &LeaveVTXOsResponse{
		LeavingCount: len(forfeits),
		Errors:       errors,
	}

	return fn.Ok[WalletResp](resp)
}

// handleBoard processes a boarding request by checking the confirmed balance,
// computing the requested VTXO output target amounts, and forwarding a
// TriggerBoardMsg to the round actor. The round actor handles the actual
// registration and FSM transitions asynchronously.
func (a *Ark) handleBoard(ctx context.Context,
	req *BoardRequest) fn.Result[WalletResp] {

	// Fetch confirmed boarding balance from the store.
	status := BoardingStatusConfirmed
	intents, err := a.store.FetchBoardingIntentsByStatus(ctx, status)
	if err != nil {
		return fn.Err[WalletResp](
			fmt.Errorf("fetch boarding intents: %w", err),
		)
	}

	var confirmedBalance btcutil.Amount
	confirmedSet := fn.NewSet[wire.OutPoint]()
	for _, intent := range intents {
		confirmedBalance += intent.ChainInfo.Amount
		confirmedSet.Add(intent.Outpoint)
	}

	if confirmedBalance == 0 {
		return fn.Err[WalletResp](
			fmt.Errorf("no confirmed boarding balance"),
		)
	}

	// Drop boarding outpoints this session already shipped into an
	// in-flight round, so a trigger fired when a second deposit confirms
	// does not re-register an already-in-flight outpoint under a freshly
	// derived owner key. handleBoard re-fetches the FULL confirmed set on
	// every call, but the round actor mints a new owner key per
	// registration, so re-shipping an in-flight outpoint produces two
	// divergent registrations of one boarding UTXO — the server quotes
	// one, the client validates the other, and the round fails with a
	// quote pkScript-echo mismatch. First prune the in-flight set against
	// the live confirmed set so outpoints that have since been adopted or
	// swept (no longer confirmed) free up, then exclude what remains.
	for _, op := range a.boardingShipped.ToSlice() {
		if !confirmedSet.Contains(op) {
			a.boardingShipped.Remove(op)
		}
	}

	boardable := make([]BoardingIntent, 0, len(intents))
	boardOutpoints := make([]wire.OutPoint, 0, len(intents))
	for _, intent := range intents {
		if a.boardingShipped.Contains(intent.Outpoint) {
			continue
		}

		boardable = append(boardable, intent)
		boardOutpoints = append(boardOutpoints, intent.Outpoint)
	}

	// Every confirmed boarding outpoint is already in flight in a round
	// this session shipped: the trigger is redundant. Report the confirmed
	// balance but do not re-dispatch, so we never mint a divergent second
	// registration. The outpoints free up here once their round adopts the
	// deposit (the intent leaves the confirmed set) or a restart clears
	// the in-memory set and the board replayer re-boards a failed round.
	if len(boardable) == 0 {
		a.logger(ctx).InfoS(ctx, "Board trigger redundant; all "+
			"confirmed boarding outpoints already in flight",
			slog.Int("confirmed_count", len(intents)),
			slog.Int64("confirmed_balance", int64(confirmedBalance)),
		)

		return fn.Ok[WalletResp](&BoardResponse{
			BoardingBalance: confirmedBalance,
		})
	}

	intents = boardable

	var totalBalance btcutil.Amount
	for _, intent := range intents {
		totalBalance += intent.ChainInfo.Amount
	}

	// Apply the operator's advertised limits before composing the
	// VTXO targets: the per-VTXO maximum bounds each output, and the
	// total user balance cap bounds how much of the confirmed balance
	// may board at all. Any clipped excess returns on-chain through a
	// change leave output paying a fresh boarding script, so the
	// remainder re-confirms as a new boarding intent that can board
	// later once headroom frees up.
	boardAmount := totalBalance
	targetCount := req.TargetVTXOCount
	var (
		changeLeave  *types.LeaveRequest
		changeAmount btcutil.Amount
		dustToFee    btcutil.Amount
	)

	terms, err := a.boardingTerms(ctx)
	if err != nil {
		return fn.Err[WalletResp](
			fmt.Errorf("fetch operator terms: %w", err),
		)
	}
	if terms != nil {
		clamp, leave, err := a.applyBoardingLimits(
			ctx, totalBalance, targetCount, terms,
		)
		if err != nil {
			return fn.Err[WalletResp](err)
		}

		boardAmount = clamp.BoardAmount
		targetCount = clamp.VTXOCount
		changeLeave = leave
		changeAmount = clamp.Change
		dustToFee = clamp.DustToFee
	}

	// Under the #270 seal-time fee handshake the server decides
	// the operator fee when the round seals, not at submit time.
	// The wallet therefore ships the full boardable balance as
	// one or more VTXO intent targets. For multi-output boarding,
	// the common change-marker logic marks one output as the
	// residual output the server can stamp at seal time. We skip
	// the pre-#270 `vtxoAmount <= DustLimit` gate because it was
	// driven by an advisory submit-time fee estimate and would
	// spuriously reject boards that the seal-time quote would
	// have accepted.
	vtxoAmounts, err := splitBoardingAmount(boardAmount, targetCount)
	if err != nil {
		return fn.Err[WalletResp](err)
	}
	vtxoAmount := sumBoardingAmounts(vtxoAmounts)

	a.logger(ctx).InfoS(ctx, "Boarding request accepted",
		slog.Int64("boarding_balance",
			int64(totalBalance)),
		slog.Int64("vtxo_amount", int64(vtxoAmount)),
		slog.Int("vtxo_count", len(vtxoAmounts)),
		slog.Int64("change_amount", int64(changeAmount)),
		slog.Int64("dust_to_fee", int64(dustToFee)))

	// Persist the user's explicit Board intent BEFORE handing the
	// request to the round actor. The ordering matters for restart
	// recovery: a crash between Tell and persist would leave the round
	// actor holding the intent in memory with no on-disk marker, so the
	// next daemon start would silently drop the user's request. With
	// persist-first, every crash window is either:
	//
	//   - pre-persist: no row, no Tell — the user sees an error and
	//     retries (idempotent).
	//   - post-persist, pre-Tell: rows exist, no Tell. On restart the
	//     wallet's replay hook re-issues TriggerBoardMsg via a
	//     self-Tell of the same BoardRequest.
	//   - post-Tell: rows exist, round actor has the request. On
	//     restart the round actor is empty but the wallet re-issues
	//     and we converge on the same state.
	//
	// The intent is anchored to every confirmed boarding outpoint the
	// call admitted. Anchors are cleared in the same SQL transaction as
	// the round-state checkpoint that flips each intent to Adopted (see
	// db.RoundPersistenceStore.CommitState), so the row can never
	// outlive the intent it was admitted against.
	if !req.NoPersist {
		anchors := make([]wire.OutPoint, 0, len(intents))
		for _, intent := range intents {
			anchors = append(anchors, intent.Outpoint)
		}

		payload := &BoardIntentPayload{
			TargetVTXOCount: req.TargetVTXOCount,
		}

		pendingIntent := PendingIntent{
			ID:          NewPendingIntentID(payload, anchors),
			Payload:     payload,
			RequestedAt: a.clk.Now().Unix(),
			Anchors:     anchors,
		}
		if err := a.store.UpsertPendingIntent(
			ctx, pendingIntent,
		); err != nil {
			return fn.Err[WalletResp](
				fmt.Errorf("persist pending board intent: %w",
					err),
			)
		}
	}

	// Forward to round actor via service key lookup. The round actor
	// registers the VTXO output requests and triggers the round join.
	if a.actorSystem == nil {
		return fn.Err[WalletResp](
			fmt.Errorf("no actor system available for board"),
		)
	}

	serviceKey := actormsg.RoundActorServiceKey()
	roundRef := serviceKey.Ref(a.actorSystem)

	if err := roundRef.Tell(
		ctx, &actormsg.TriggerBoardMsg{
			Amounts:   vtxoAmounts,
			Outpoints: boardOutpoints,
			Change:    changeLeave,
		},
	); err != nil {
		// The persisted row stays in place so the next daemon
		// start (or a fresh Board RPC) will retry. Returning the
		// error here lets the caller surface the Tell failure
		// without leaving the user thinking Board succeeded.
		return fn.Err[WalletResp](
			fmt.Errorf("forward board to round actor: %w", err),
		)
	}

	// Record the outpoints we just handed to the round actor so a later
	// trigger fired before this round adopts them does not re-ship them
	// under a freshly derived owner key. Pruned back out above once they
	// leave the confirmed set.
	for _, op := range boardOutpoints {
		a.boardingShipped.Add(op)
	}

	resp := &BoardResponse{
		BoardingBalance: totalBalance,
		VTXOAmount:      vtxoAmount,
		VTXOAmounts:     vtxoAmounts,
	}

	return fn.Ok[WalletResp](resp)
}

// handleReplayPendingIntents is the Ask handler for
// ReplayPendingIntentsRequest. The daemon issues this Ask once every
// dependent actor (round-client, vtxo-manager, txconfirm, etc.) is
// registered, which is the earliest moment a replayed intent's downstream
// round-actor dispatch can be delivered through the actor receptionist.
//
// The handler walks the registered PendingIntentReplayer set: for each
// kind it lists the persisted intents and hands them to the kind's
// replayer, which reconciles them against live state and either re-issues
// the original command via self-Tell or clears the stale rows. Running
// from a Receive-time handler rather than Start closes the
// round-actor-registration race, and the self-Tell pattern preserves FIFO
// ordering against user RPCs admitted after the replay Ask returns.
func (a *Ark) handleReplayPendingIntents(ctx context.Context,
	_ *ReplayPendingIntentsRequest) fn.Result[WalletResp] {

	var replayedAny bool
	for _, replayer := range a.intentReplayers {
		kind := replayer.Kind()

		intents, err := a.store.ListPendingIntents(ctx, kind)
		if err != nil {
			return fn.Err[WalletResp](
				fmt.Errorf("list pending intents for kind "+
					"%v: %w", kind, err),
			)
		}

		if len(intents) == 0 {
			continue
		}

		replayed, err := replayer.Replay(ctx, intents)
		if err != nil {
			return fn.Err[WalletResp](
				fmt.Errorf("replay pending intents for kind "+
					"%v: %w", kind, err),
			)
		}

		replayedAny = replayedAny || replayed
	}

	return fn.Ok[WalletResp](&ReplayPendingIntentsResponse{
		Replayed: replayedAny,
	})
}

// splitBoardingAmount fans a confirmed boarding balance into count VTXO
// target amounts. A zero count preserves the legacy single-output behavior.
func splitBoardingAmount(total btcutil.Amount,
	count uint32) ([]btcutil.Amount, error) {

	if count == 0 {
		count = 1
	}
	if total <= 0 {
		return nil, fmt.Errorf("boarding balance must be positive")
	}

	base := int64(total) / int64(count)
	remainder := int64(total) % int64(count)
	if base <= 0 {
		return nil, fmt.Errorf("boarding balance %v too small for "+
			"%d VTXOs", total, count)
	}

	amounts := make([]btcutil.Amount, count)
	for i := range amounts {
		amount := base
		if int64(i) < remainder {
			amount++
		}

		amounts[i] = btcutil.Amount(amount)
	}

	return amounts, nil
}

// sumBoardingAmounts returns the total of boarding target amounts.
func sumBoardingAmounts(amounts []btcutil.Amount) btcutil.Amount {
	var total btcutil.Amount
	for _, amount := range amounts {
		total += amount
	}

	return total
}

// buildBoardingTxProof fetches the confirmation block and computes a merkle
// inclusion proof for the boarding transaction. The blockHash parameter should
// be the UTXO's actual confirmation block (from GetTransaction), not the
// current epoch block, so proofs are correct even during catch-up. If anything
// fails, it returns None — the intent will still be persisted, but without a
// proof the server will need its own chain source to validate.
func (a *Ark) buildBoardingTxProof(ctx context.Context,
	blockHash chainhash.Hash, blockHeight int32, confTx *wire.MsgTx,
	outpoint wire.OutPoint,
	addr *BoardingAddress) fn.Option[proof.TxProof] {

	// Fetch the full block to compute the merkle proof.
	block, err := a.backend.GetBlock(ctx, blockHash)
	if err != nil {
		a.logger(ctx).WarnS(ctx, "Failed fetching block for TxProof",
			err,
			btclog.Fmt("block_hash", "%v", blockHash),
		)

		return fn.None[proof.TxProof]()
	}

	// Find the transaction index within the block.
	txHash := confTx.TxHash()
	txIdx := -1
	for i, blockTx := range block.Transactions {
		if blockTx.TxHash() == txHash {
			txIdx = i
			break
		}
	}
	if txIdx < 0 {
		a.logger(ctx).WarnS(ctx, "Boarding tx not found in block",
			nil,
			btclog.Fmt("txid", "%v", txHash),
			btclog.Fmt("block_hash", "%v", blockHash),
		)

		return fn.None[proof.TxProof]()
	}

	// Compute the merkle inclusion proof.
	merkleProof, err := proof.NewTxMerkleProof(
		block.Transactions, txIdx,
	)
	if err != nil {
		a.logger(ctx).WarnS(ctx, "Failed computing merkle proof",
			err,
			btclog.Fmt("txid", "%v", txHash),
		)

		return fn.None[proof.TxProof]()
	}

	// Extract the internal key and tapscript root hash from the boarding
	// address. VTXOTapScript populates both ControlBlock.InternalKey and
	// RootHash when constructing the tapscript.
	if addr.Tapscript == nil || addr.Tapscript.ControlBlock == nil ||
		addr.Tapscript.ControlBlock.InternalKey == nil {

		a.logger(ctx).WarnS(
			ctx,
			"Boarding address missing tapscript data",
			nil,
		)

		return fn.None[proof.TxProof]()
	}
	internalKey := addr.Tapscript.ControlBlock.InternalKey
	merkleRoot := addr.Tapscript.RootHash

	a.logger(ctx).InfoS(ctx, "Built TxProof for boarding UTXO",
		btclog.Fmt("outpoint", "%v", outpoint),
		slog.Int("block_height", int(blockHeight)),
	)

	return fn.Some(proof.TxProof{
		MsgTx:           *confTx,
		BlockHeader:     block.Header,
		BlockHeight:     uint32(blockHeight),
		MerkleProof:     *merkleProof,
		ClaimedOutPoint: outpoint,
		InternalKey:     *internalKey,
		MerkleRoot:      merkleRoot,
	})
}

// =============================================================================
// VTXO admission forwarding handlers
// =============================================================================
//
// These handlers forward admission requests to the VTXO manager actor via
// service key lookup. The manager owns the actual coin selection, reservation,
// and rollback logic; the wallet is a thin forwarding layer that translates
// between wallet messages and actormsg admission types.

// askManager sends a VTXOManagerMsg to the VTXO manager via service key and
// returns the response. This is a convenience wrapper around the Ask/Await
// pattern that reduces boilerplate at each call site.
func (a *Ark) askManager(ctx context.Context, msg actormsg.VTXOManagerMsg) (
	actormsg.VTXOManagerResp, error) {

	if a.actorSystem == nil {
		return nil, fmt.Errorf("actor system not configured")
	}

	serviceKey := actormsg.VTXOManagerServiceKey()
	managerRef := serviceKey.Ref(a.actorSystem)

	future := managerRef.Ask(ctx, msg)
	result := future.Await(ctx)

	return result.Unpack()
}

// releaseManagerForfeit is a best-effort helper that releases forfeit
// reservations when round registration fails after successful admission.
// Errors are logged but not propagated because the primary error (round
// rejection) has already been captured.
func (a *Ark) releaseManagerForfeit(ctx context.Context,
	outpoints []wire.OutPoint) {

	_, err := a.askManager(
		ctx, &actormsg.ReleaseForfeitRequest{
			Outpoints: outpoints,
		},
	)
	if err != nil {
		a.logger(ctx).WarnS(
			ctx,
			"Failed to release forfeit reservation",
			err,
		)
	}
}

// handleSelectAndLockVTXOs forwards a spend selection request to the VTXO
// manager. The manager runs largest-first coin selection and atomically
// reserves VTXOs for OOR spending by transitioning them to SpendingState.
func (a *Ark) handleSelectAndLockVTXOs(ctx context.Context,
	req *SelectAndLockVTXOsRequest) fn.Result[WalletResp] {

	a.logger(ctx).TraceS(ctx, "Selecting and locking VTXOs for spend",
		slog.Int64("target", int64(req.TargetAmount)),
	)

	resp, err := a.askManager(
		ctx, &actormsg.SelectAndReserveSpendRequest{
			TargetAmount:    req.TargetAmount,
			MinChangeAmount: req.MinChangeAmount,
		},
	)
	if err != nil {
		return fn.Err[WalletResp](
			fmt.Errorf("select and reserve: %w", err),
		)
	}

	//nolint:forcetypeassert
	mgrResp := resp.(*actormsg.SelectAndReserveSpendResponse)

	// Translate manager response to wallet response.
	selected := make([]SelectedVTXO, len(mgrResp.SelectedVTXOs))
	for i, v := range mgrResp.SelectedVTXOs {
		selected[i] = SelectedVTXO{
			Outpoint: v.Outpoint,
			Amount:   v.Amount,
			PkScript: v.PkScript,
		}
	}

	a.logger(ctx).InfoS(ctx, "VTXOs selected and locked",
		slog.Int("count", len(selected)),
		slog.Int64("total", int64(mgrResp.TotalSelected)),
	)

	return fn.Ok[WalletResp](&SelectAndLockVTXOsResponse{
		SelectedVTXOs: selected,
		TotalSelected: mgrResp.TotalSelected,
	})
}

// handleUnlockVTXOs forwards a spend release request to the VTXO manager.
// This transitions VTXOs from SpendingState back to LiveState when an OOR
// transfer fails or is cancelled.
func (a *Ark) handleUnlockVTXOs(ctx context.Context,
	req *UnlockVTXOsRequest) fn.Result[WalletResp] {

	a.logger(ctx).InfoS(ctx, "Unlocking VTXOs from spend reservation",
		slog.Int("count", len(req.Outpoints)),
	)

	resp, err := a.askManager(
		ctx, &actormsg.ReleaseSpendRequest{
			Outpoints: req.Outpoints,
		},
	)
	if err != nil {
		return fn.Err[WalletResp](
			fmt.Errorf("release spend: %w", err),
		)
	}

	//nolint:forcetypeassert
	mgrResp := resp.(*actormsg.ReleaseSpendResponse)

	return fn.Ok[WalletResp](&UnlockVTXOsResponse{
		UnlockedCount: mgrResp.ReleasedCount,
	})
}

// handleCompleteSpendVTXOs forwards a spend completion request to the VTXO
// manager. This transitions VTXOs from SpendingState to terminal SpentState
// after an OOR transfer succeeds.
func (a *Ark) handleCompleteSpendVTXOs(ctx context.Context,
	req *CompleteSpendVTXOsRequest) fn.Result[WalletResp] {

	a.logger(ctx).InfoS(ctx, "Completing spend for VTXOs",
		slog.Int("count", len(req.Outpoints)),
	)

	resp, err := a.askManager(
		ctx, &actormsg.CompleteSpendRequest{
			Outpoints: req.Outpoints,
		},
	)
	if err != nil {
		return fn.Err[WalletResp](
			fmt.Errorf("complete spend: %w", err),
		)
	}

	//nolint:forcetypeassert
	mgrResp := resp.(*actormsg.CompleteSpendResponse)

	a.logger(ctx).InfoS(ctx, "Spend completion confirmed",
		slog.Int("completed", mgrResp.CompletedCount),
	)

	return fn.Ok[WalletResp](&CompleteSpendVTXOsResponse{
		CompletedCount: mgrResp.CompletedCount,
	})
}

// handleSendVTXOs processes an in-round directed send. It atomically
// selects and reserves VTXOs for cooperative consumption, builds an
// IntentPackage with forfeits + recipient VTXOs + change, and
// registers it with the round actor. On failure, all reservations are
// released. For dry-run, the reservation is immediately released
// after validation.
func (a *Ark) handleSendVTXOs(ctx context.Context,
	req *SendVTXOsRequest) fn.Result[WalletResp] {

	// Validate recipients.
	if len(req.Recipients) == 0 {
		return fn.Err[WalletResp](
			fmt.Errorf("no recipients provided"),
		)
	}

	var totalRecipientAmount btcutil.Amount
	for i, r := range req.Recipients {
		if len(r.PkScript) == 0 {
			return fn.Err[WalletResp](
				fmt.Errorf("recipient %d: empty pk_script", i),
			)
		}

		if r.Amount <= 0 || r.Amount > btcutil.MaxSatoshi {
			return fn.Err[WalletResp](
				fmt.Errorf(
					"recipient %d: amount must be "+
						"between 1 and %d", i,
					int64(btcutil.MaxSatoshi),
				),
			)
		}

		if totalRecipientAmount+r.Amount < 0 {
			return fn.Err[WalletResp](
				fmt.Errorf("total recipient amount overflows"),
			)
		}

		totalRecipientAmount += r.Amount
	}

	totalNeeded := totalRecipientAmount + req.OperatorFee

	a.logger(ctx).InfoS(ctx, "Processing directed send",
		slog.Int("recipients", len(req.Recipients)),
		slog.Int64("total_amount",
			int64(totalRecipientAmount)),
		slog.Int64("operator_fee",
			int64(req.OperatorFee)),
		slog.Bool("dry_run", req.DryRun))

	// Atomic select-and-reserve for cooperative consumption.
	resp, err := a.askManager(
		ctx,
		&actormsg.SelectAndReserveForfeitRequest{
			TargetAmount: totalNeeded,
		},
	)
	if err != nil {
		return fn.Err[WalletResp](
			fmt.Errorf("select and reserve forfeit: %w", err),
		)
	}

	//nolint:forcetypeassert
	mgrResp := resp.(*actormsg.SelectAndReserveForfeitResponse)

	// Collect reserved outpoints for potential rollback.
	reservedOutpoints := make(
		[]wire.OutPoint, 0, len(mgrResp.SelectedVTXOs),
	)
	for _, v := range mgrResp.SelectedVTXOs {
		reservedOutpoints = append(
			reservedOutpoints, v.Outpoint,
		)
	}

	// Ensure reserved VTXOs are released if we don't reach the
	// successful registration at the end. Use a background
	// context so cleanup survives client disconnection.
	committed := false
	defer func() {
		if committed {
			return
		}

		releaseCtx := context.WithoutCancel(ctx)
		releaseErr := a.releaseManagerForfeitStrict(
			releaseCtx, reservedOutpoints,
		)
		if releaseErr != nil {
			a.logger(releaseCtx).WarnS(
				releaseCtx,
				"Failed to release reserved "+
					"VTXOs", releaseErr,
			)
		}
	}()

	// Compute change.
	change := mgrResp.TotalSelected - totalNeeded
	if change < 0 {

		// Should not happen since coin selection covers the
		// target, but be defensive.
		return fn.Err[WalletResp](
			fmt.Errorf("selection shortfall: selected %d, need %d",
				mgrResp.TotalSelected, totalNeeded),
		)
	}

	if change > 0 && change < req.DustLimit {
		return fn.Err[WalletResp](
			fmt.Errorf("change %d is below VTXO minimum %d; "+
				"adjust send amount", change, req.DustLimit),
		)
	}

	// Under the #270 fixed-output contract a multi-output intent
	// must carry exactly one IsChange=true marker so the server
	// knows which output absorbs the seal-time residual. When
	// coin selection covers the target exactly (change == 0) for
	// a multi-recipient send, no self-change output exists and the
	// intent would ship with zero markers — the server admission
	// path rejects that with INVALID_CHANGE_DESIGNATION, burning
	// a round slot for a deterministic failure. Surface the
	// mismatch locally instead; the operator can retry with a
	// value that allows change, or split the send.
	if change == 0 && len(req.Recipients) > 1 {
		return fn.Err[WalletResp](
			fmt.Errorf("multi-recipient send must leave change " +
				"for the seal-time fee marker: coin " +
				"selection covered the target exactly"),
		)
	}

	// Dry-run: validate coin selection then release immediately.
	if req.DryRun {

		// The deferred cleanup releases the reservation.
		return fn.Ok[WalletResp](&SendVTXOsResponse{
			Status:        "preview",
			SelectedCount: len(mgrResp.SelectedVTXOs),
			TotalSelected: mgrResp.TotalSelected,
			ChangeAmount:  change,
		})
	}

	// Build the intent package.
	forfeits := make(
		[]types.ForfeitRequest, 0, len(mgrResp.SelectedVTXOs),
	)
	for _, v := range mgrResp.SelectedVTXOs {
		op := v.Outpoint
		forfeits = append(forfeits, types.ForfeitRequest{
			VTXOOutpoint: &op,
			Amount:       v.Amount,
		})
	}

	// Build recipient + change VTXOs with fresh signing keys.
	vtxoRequests, buildErr := a.buildSendVTXORequests(
		ctx, req, change,
	)
	if buildErr != nil {
		return fn.Err[WalletResp](buildErr)
	}

	// Register the intent with the round actor.
	serviceKey := actormsg.RoundActorServiceKey()
	roundRef := serviceKey.Ref(a.actorSystem)

	future := roundRef.Ask(ctx, &actormsg.RegisterIntentMsg{
		Forfeits:            forfeits,
		VTXOs:               vtxoRequests,
		TriggerRegistration: true,
	})
	result := future.Await(ctx)
	if result.IsErr() {
		a.logger(ctx).WarnS(ctx, "Round rejected send intent",
			result.Err(),
		)

		return fn.Err[WalletResp](
			fmt.Errorf(
				"round rejected send intent: %w", result.Err(),
			),
		)
	}

	committed = true

	a.logger(ctx).InfoS(ctx, "Directed send intent registered",
		slog.Int("forfeits", len(forfeits)),
		slog.Int("recipient_vtxos", len(req.Recipients)),
		slog.Int64("change", int64(change)),
	)

	return fn.Ok[WalletResp](&SendVTXOsResponse{
		Status:        "submitted",
		SelectedCount: len(mgrResp.SelectedVTXOs),
		TotalSelected: mgrResp.TotalSelected,
		ChangeAmount:  change,
	})
}

// buildSendVTXORequests assembles VTXORequest entries for each recipient plus
// an optional change output. Recipient requests carry only the semantic
// policy and public owner key, while locally owned change also retains the
// owner descriptor so confirmation can persist it correctly.
func (a *Ark) buildSendVTXORequests(ctx context.Context, req *SendVTXOsRequest,
	change btcutil.Amount) ([]types.VTXORequest, error) {

	vtxoRequests := make(
		[]types.VTXORequest, 0,
		len(req.Recipients)+1,
	)
	for i, r := range req.Recipients {
		// Derive the VTXO policy template and pkScript from
		// (ownerKey, operatorKey, exitDelay). Signing keys are
		// NOT derived here — the round FSM derives them during
		// the RegistrationSent transition per #210.
		policyTemplate, pkScript, err := arkscript.
			EncodeStandardVTXOArtifacts(
				r.ClientKey, req.OperatorKey, req.VTXOExitDelay,
			)
		if err != nil {
			return nil, fmt.Errorf("build recipient %d "+
				"descriptor: %w", i, err)
		}

		vtxoRequests = append(vtxoRequests, types.VTXORequest{
			Amount:         r.Amount,
			PolicyTemplate: policyTemplate,
			PkScript:       pkScript,
			Expiry:         req.VTXOExitDelay,
			ClientKey:      r.ClientKey,
			OperatorKey:    req.OperatorKey,
		})
	}

	// Add change VTXO if needed. The sender owns the change, so
	// keep the long-lived owner descriptor locally even though
	// only the pubkey goes on the wire.
	if change > 0 {
		changeClientKey, keyErr := a.backend.DeriveNextKey(
			ctx, types.VTXOOwnerKeyFamily,
		)
		if keyErr != nil {
			return nil, fmt.Errorf("derive change client key: %w",
				keyErr)
		}

		policyTemplate, pkScript, err := arkscript.
			EncodeStandardVTXOArtifacts(
				changeClientKey.PubKey, req.OperatorKey,
				req.VTXOExitDelay,
			)
		if err != nil {
			return nil, fmt.Errorf("build change descriptor: %w",
				err)
		}

		vtxoRequests = append(
			vtxoRequests, types.VTXORequest{
				Amount:         change,
				PolicyTemplate: policyTemplate,
				PkScript:       pkScript,
				Expiry:         req.VTXOExitDelay,
				ClientKey:      changeClientKey.PubKey,
				OwnerKey:       *changeClientKey,
				OperatorKey:    req.OperatorKey,
				// Self-change on a directed send: the
				// client forfeits one or more VTXOs and
				// receives part of the value back as
				// change. Same ledger semantics as a
				// refresh output — the change cancels a
				// portion of the forfeit on transfers_out
				// rather than counting as a new
				// counterparty receipt. Under #270 the
				// change output is the residual sink for
				// the seal-time quote: IsChange=true tells
				// the server which output to stamp with
				// Σin − Σ(fixed) − fee.
				Origin:   types.VTXOOriginRoundRefresh,
				IsChange: true,
			},
		)
	}

	return vtxoRequests, nil
}

// handleSendOnChain plans and submits an atomic onchain payment from VTXOs.
// The handler has two modes selected by the request:
//
//   - Bounded (TargetAmountSat > 0): the wallet asks the VTXO manager to
//     select and reserve VTXOs covering TargetAmountSat plus an
//     OperatorFee + DustLimit headroom, then builds an intent of
//     {forfeit inputs, one fixed LeaveRequest of exactly TargetAmountSat,
//     one change VTXORequest with IsChange=true} so the server's seal-time
//     quote builder stamps the residual onto the change VTXO.
//
//   - SweepAll: the wallet reserves the outpoint set enumerated by the
//     RPC layer (no in-wallet enumeration) and builds an intent of
//     {forfeit inputs, one LeaveRequest with IsChange=true and zero
//     target}, so the server stamps the full residual (Σinputs − fee)
//     onto the single on-chain output.
//
// Both modes register with TriggerRegistration=true: the onchain send is
// atomic by design and never composes with other queued intents.
//
//nolint:gocyclo,funlen
func (a *Ark) handleSendOnChain(ctx context.Context,
	req *SendOnChainRequest) fn.Result[WalletResp] {

	// Pure input validation. The RPC layer already enforced these
	// invariants; the duplicate is defense-in-depth so the wallet
	// actor never builds a malformed intent. Sweep-all mode is
	// implied by a non-empty SweepOutpoints set.
	if len(req.DestinationPkScript) == 0 {
		return fn.Err[WalletResp](
			fmt.Errorf("destination pkScript required"),
		)
	}

	sweepAll := req.IsSweepAll()

	switch {
	case sweepAll && req.TargetAmountSat != 0:
		return fn.Err[WalletResp](
			fmt.Errorf("sweep-all requires target_amount_sat == 0"),
		)

	case !sweepAll && req.TargetAmountSat <= 0:
		return fn.Err[WalletResp](
			fmt.Errorf("target_amount_sat must be positive for " +
				"a bounded send"),
		)

	case !sweepAll && req.OperatorKey == nil:
		return fn.Err[WalletResp](
			fmt.Errorf("operator_key required to build the " +
				"change VTXO"),
		)
	}

	a.logger(ctx).InfoS(ctx, "Processing onchain send",
		slog.Bool("sweep_all", sweepAll),
		slog.Int64("target_amount_sat",
			int64(req.TargetAmountSat)),
		slog.Int("sweep_outpoints", len(req.SweepOutpoints)),
		slog.Int64("operator_fee_hint",
			int64(req.OperatorFee)),
		slog.Bool("dry_run", req.DryRun),
	)

	// Reserve forfeit inputs. Bounded mode delegates selection to
	// the VTXO manager (largest-first); sweep-all takes the outpoint
	// list the RPC layer enumerated and reserves it explicitly. Both
	// paths fill in selectedOutpoints / selectedAmounts / totalSelected.
	var (
		selectedOutpoints []wire.OutPoint
		selectedAmounts   []btcutil.Amount
		totalSelected     btcutil.Amount
	)

	if sweepAll {
		_, err := a.askManager(
			ctx, &actormsg.ReserveForfeitRequest{
				Outpoints: req.SweepOutpoints,
			},
		)
		if err != nil {
			return fn.Err[WalletResp](
				fmt.Errorf("reserve sweep outpoints: %w", err),
			)
		}

		// Materialize amounts so the intent's ForfeitRequest set is
		// complete. The reservation already gates concurrent
		// consumers, so reading amounts via the descriptor here is
		// race-free.
		for _, op := range req.SweepOutpoints {
			vtxo, err := a.vtxoReader.GetVTXO(ctx, op)
			if err != nil {
				// Release the full reserved set up front
				// and clear selectedOutpoints so the
				// deferred cleanup below does not attempt
				// to release the (sub)set a second time.
				releaseCtx := context.WithoutCancel(ctx)
				_ = a.releaseManagerForfeitStrict(
					releaseCtx, req.SweepOutpoints,
				)
				selectedOutpoints = nil

				return fn.Err[WalletResp](
					fmt.Errorf("load sweep vtxo %s: %w",
						op, err),
				)
			}
			selectedOutpoints = append(selectedOutpoints, op)
			selectedAmounts = append(
				selectedAmounts, vtxo.Amount,
			)
			totalSelected += vtxo.Amount
		}
	} else {
		// Bounded: select via the manager with enough headroom so
		// the residual change VTXO can clear dust under the
		// seal-time fee handshake. The advisory OperatorFee hint
		// over-selects under stale schedules; excess simply lands
		// as a larger change VTXO.
		target := req.TargetAmountSat + req.OperatorFee + req.DustLimit
		resp, err := a.askManager(
			ctx, &actormsg.SelectAndReserveForfeitRequest{
				TargetAmount: target,
			},
		)
		if err != nil {
			return fn.Err[WalletResp](
				fmt.Errorf("select and reserve forfeit: %w",
					err),
			)
		}

		//nolint:forcetypeassert
		mgrResp := resp.(*actormsg.SelectAndReserveForfeitResponse)
		for _, v := range mgrResp.SelectedVTXOs {
			selectedOutpoints = append(
				selectedOutpoints, v.Outpoint,
			)
			selectedAmounts = append(selectedAmounts, v.Amount)
		}
		totalSelected = mgrResp.TotalSelected
	}

	// Release reserved outpoints if we bail before successfully
	// registering the intent. Same pattern as handleSendVTXOs:
	// context.WithoutCancel survives client disconnect.
	committed := false
	defer func() {
		if committed {
			return
		}

		releaseCtx := context.WithoutCancel(ctx)
		releaseErr := a.releaseManagerForfeitStrict(
			releaseCtx, selectedOutpoints,
		)
		if releaseErr != nil {
			a.logger(releaseCtx).WarnS(
				releaseCtx,
				"Failed to release reserved sweep outpoints",
				releaseErr,
			)
		}
	}()

	// Defensive shortfall check in bounded mode. The manager's
	// SelectAndReserveForfeit fails closed on insufficient funds,
	// but we re-check here so a buggy selector or stale-cache race
	// surfaces as a wallet error rather than as a server-side
	// admission failure on a fee-deficient intent. The dust floor
	// is enforced locally so a below-dust change projection fails
	// fast here instead of late at operator admission with
	// ErrVTXOAmountBelowMinimum.
	var change btcutil.Amount
	if !sweepAll {
		if totalSelected <= req.TargetAmountSat {
			return fn.Err[WalletResp](
				fmt.Errorf("selection shortfall: selected "+
					"%d, need >%d", totalSelected,
					req.TargetAmountSat),
			)
		}
		change = totalSelected - req.TargetAmountSat
		if change < req.DustLimit {
			return fn.Err[WalletResp](
				fmt.Errorf("change amount %d is below VTXO "+
					"minimum %d", change, req.DustLimit),
			)
		}
	}

	if req.DryRun {

		// Deferred release returns the reservation.
		return fn.Ok[WalletResp](&SendOnChainResponse{
			Status: SendOnChainStatusPreview,
			ActualAmountSat: sendOnChainActualAmount(
				req, totalSelected,
			),
			SelectedOutpoints: selectedOutpoints,
			TotalSelected:     totalSelected,
			ChangeAmount:      change,
		})
	}

	// Build the round intent. Forfeit inputs come from the
	// reservation; the leave output is the user's onchain payment;
	// the change VTXO (bounded mode only) absorbs the seal-time fee
	// residual. The package builder is shared with the restart replay
	// path so both produce the identical intent shape.
	intentPayload := &SendOnChainIntentPayload{
		DestinationPkScript: req.DestinationPkScript,
		TargetAmountSat:     req.TargetAmountSat,
		SweepAll:            sweepAll,
		OperatorKey:         req.OperatorKey,
		VTXOExitDelay:       req.VTXOExitDelay,
		DustLimit:           req.DustLimit,
	}

	forfeits, leaves, vtxos, err := a.buildSendOnChainIntentPackage(
		ctx, *intentPayload, selectedOutpoints, selectedAmounts, change,
	)
	if err != nil {
		return fn.Err[WalletResp](
			fmt.Errorf("build onchain send intent: %w", err),
		)
	}

	// Persist the send intent to the pending-intents outbox BEFORE
	// publishing it to the round actor (persist-before-publish). The
	// anchors are the reserved forfeit outpoints — the round consumes
	// exactly these when it adopts the intent, and the round-state
	// checkpoint clears them in the same transaction (see
	// db.RoundPersistenceStore.CommitState), so a replay after adoption
	// is structurally impossible. A crash in any window before the user
	// sees "submitted" either leaves no row (clean retry) or a row whose
	// startup replay re-reserves these exact outpoints and re-registers.
	pendingIntent := PendingIntent{
		ID: NewPendingIntentID(
			intentPayload, selectedOutpoints,
		),
		Payload:     intentPayload,
		RequestedAt: a.clk.Now().Unix(),
		Anchors:     selectedOutpoints,
	}
	if err := a.store.UpsertPendingIntent(ctx, pendingIntent); err != nil {
		return fn.Err[WalletResp](
			fmt.Errorf("persist pending onchain send intent: %w",
				err),
		)
	}

	// If registration fails below, delete the outbox row alongside the
	// deferred reservation release: the caller receives an error, so a
	// silent resurrection of the "failed" send on the next start would
	// contradict what the user was told. This mirrors the !committed
	// release defer above and shares its crash semantics — a crash
	// between persist and the round Ask leaves the row in place, which
	// is exactly the window replay exists to cover.
	defer func() {
		if committed {
			return
		}

		deleteCtx := context.WithoutCancel(ctx)
		if delErr := a.store.DeletePendingIntent(
			deleteCtx, pendingIntent.ID,
		); delErr != nil {
			a.logger(deleteCtx).WarnS(
				deleteCtx,
				"Failed to delete pending onchain send "+
					"intent after registration failure",
				delErr,
			)
		}
	}()

	// Register the intent with the round actor. TriggerRegistration is
	// left false here so the wallet handler stops at the "intent
	// queued" boundary; the RPC handler fires the IntentRequested
	// step separately (via waved.TriggerRoundRegistration), which
	// mirrors the OLD LeaveVTXOs+JoinNextRound split that arktest
	// exercises today. The eager-mode RegisterIntentMsg.TriggerRegistration
	// path collapses both steps into a single Ask and has a latent ctx
	// issue in arktest with EagerRoundJoin=false: the FSM transition
	// inside the same Ask interleaves with the outbound publish in a
	// way that leaves the operator's forfeit-VTXO lookup with a
	// cancelled context. Splitting matches the proven-working pattern.
	//
	// Ask context is stripped of caller cancellation as defense in
	// depth in case any caller invokes the wallet handler directly
	// with a request-scoped ctx; the Await keeps the original ctx.
	serviceKey := actormsg.RoundActorServiceKey()
	roundRef := serviceKey.Ref(a.actorSystem)

	askCtx := context.WithoutCancel(ctx)
	future := roundRef.Ask(askCtx, &actormsg.RegisterIntentMsg{
		Forfeits:            forfeits,
		VTXOs:               vtxos,
		Leaves:              leaves,
		TriggerRegistration: false,
	})
	result := future.Await(ctx)
	if result.IsErr() {
		// Distinguish a caller-await cancellation from a genuine round
		// rejection. The Ask runs on askCtx (WithoutCancel), so it
		// keeps being delivered and processed regardless of the
		// caller; Await(ctx) returns ctx.Err() the moment the caller's
		// ctx ends, independently of the round outcome. On
		// caller-cancel the round may still accept the intent RAM-only,
		// so deleting the outbox row + releasing the reservation here
		// would reopen the #660 window (no row to replay, yet the
		// round may adopt). Retain both — set committed so neither
		// defer fires — and let startup replay (or the round
		// checkpoint) reconcile the in-flight outcome.
		if ctx.Err() != nil {
			committed = true

			a.logger(ctx).WarnS(ctx, "Caller await canceled "+
				"before onchain send registered; keeping "+
				"outbox intent for replay", ctx.Err())

			return fn.Err[WalletResp](
				fmt.Errorf(
					"onchain send await canceled: %w",
					ctx.Err(),
				),
			)
		}

		// Genuine synchronous round rejection: the caller is told the
		// send failed, so the deferred cleanup drops the row and
		// releases the reservation (committed stays false).
		a.logger(ctx).WarnS(ctx, "Round rejected onchain send intent",
			result.Err(),
		)

		return fn.Err[WalletResp](
			fmt.Errorf(
				"round rejected onchain send intent: %w",
				result.Err(),
			),
		)
	}

	committed = true

	a.logger(ctx).InfoS(ctx, "Onchain send intent registered",
		slog.Int("forfeits", len(forfeits)),
		slog.Int("leaves", len(leaves)),
		slog.Int("change_vtxos", len(vtxos)),
		slog.Int64("total_selected", int64(totalSelected)),
		slog.Int64("change_projection", int64(change)),
	)

	return fn.Ok[WalletResp](&SendOnChainResponse{
		Status:            SendOnChainStatusSubmitted,
		IntentID:          pendingIntent.ID,
		ActualAmountSat:   sendOnChainActualAmount(req, totalSelected),
		SelectedOutpoints: selectedOutpoints,
		TotalSelected:     totalSelected,
		ChangeAmount:      change,
	})
}

// sendOnChainActualAmount returns the on-chain amount that will land at
// the destination. In bounded mode this is the exact TargetAmountSat;
// in sweep-all it is the pre-fee Σ(inputs), which the server reduces by
// the seal-time operator fee.
func sendOnChainActualAmount(req *SendOnChainRequest,
	totalSelected btcutil.Amount) btcutil.Amount {

	if req.IsSweepAll() {
		return totalSelected
	}

	return req.TargetAmountSat
}

// releaseManagerForfeitStrict releases forfeit reservations and returns
// the error rather than swallowing it. Used by dry-run where release
// failure must be surfaced to the caller.
func (a *Ark) releaseManagerForfeitStrict(ctx context.Context,
	outpoints []wire.OutPoint) error {

	_, err := a.askManager(
		ctx, &actormsg.ReleaseForfeitRequest{
			Outpoints: outpoints,
		},
	)

	return err
}

// buildBoardingTapscript constructs a 2-of-2 tapscript with CSV timeout for
// boarding. The tapscript has two spending paths:
//   - Collaborative: Requires both client and operator signatures (spendable
//     anytime)
//   - Timeout: Requires only client signature after CSV delay (unilateral
//     recovery)
//
// The internal key is the ARK NUMS key (nothing up my sleeve) which has no
// known discrete log, ensuring the key path is unspendable.
func buildBoardingTapscript(clientKey, operatorKey *btcec.PublicKey,
	exitDelay uint32) (*waddrmgr.Tapscript, error) {

	// Use the standard VTXO tapscript construction. Boarding outputs and
	// VTXOs use the same script structure. The client is the "owner" who
	// can recover funds after the CSV delay, and the operator is the
	// "cosigner" who collaborates with the client for immediate spends.
	tapscript, err := arkscript.VTXOTapScript(
		clientKey, operatorKey, exitDelay,
	)
	if err != nil {
		return nil, fmt.Errorf("build VTXO tapscript: %w", err)
	}

	return tapscript, nil
}
