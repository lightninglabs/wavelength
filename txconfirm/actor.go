package txconfirm

import (
	"bytes"
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
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"golang.org/x/time/rate"
)

const (
	// DefaultFeeBumpIntervalBlocks is the default number of new
	// blocks to wait before retrying a still-unconfirmed transaction
	// with a fresh CPFP child.
	DefaultFeeBumpIntervalBlocks int32 = 2

	// DefaultBroadcastFailureAlertThreshold is the default number of
	// consecutive initial-broadcast failures (a tx that reached no mempool
	// at all) after which the actor emits an operator-visible escalation.
	// The tx keeps being re-attempted regardless; this only controls when
	// the actor starts asking for operator attention.
	//
	// Note the effective time to first alert is roughly this threshold
	// times FeeBumpIntervalBlocks blocks, because re-attempts are
	// interval-paced. The default is kept small so a fund-risk tx (e.g. a
	// fraud-response checkpoint) surfaces to operators quickly rather than
	// after a long silent window.
	DefaultBroadcastFailureAlertThreshold = 3

	// broadcastEscalationReminder bounds how often the operator-facing
	// "repeatedly failing" escalation repeats once the alert threshold has
	// been crossed. The first crossing always logs; subsequent reminders
	// are emitted at most once per this interval, decoupling the reminder
	// cadence from the (block-driven) retry interval so a permanently stuck
	// tx does not flood the log.
	broadcastEscalationReminder = 10 * time.Minute
)

var (
	// terminalNotifyTimeout bounds how long txconfirm waits for one
	// subscriber's terminal notification before returning to its actor
	// mailbox. Terminal entries stay cached and retry on later ticks, so
	// waiting longer only risks blocking unrelated confirmation work behind
	// a durable subscriber's DB writer.
	terminalNotifyTimeout = time.Second
)

// ErrEnsureParamsMismatch is returned by EnsureConfirmedReq when a second
// caller asks to confirm a txid that is already being tracked, but with
// different confirmation parameters (TargetConfs or ConfirmationPkScript)
// than the in-flight tracker. Silently reusing the existing entry would
// cause one subscriber to receive a notification that does not match the
// criteria it asked for, so the second request is rejected outright and
// the caller is responsible for reconciling.
var ErrEnsureParamsMismatch = errors.New("ensure params mismatch existing " +
	"tracker")

// Config configures the generic shared tx confirmation actor.
type Config struct {
	// ChainSource provides the blockchain interface for best-height
	// queries,
	// confirmation watches, block subscriptions, fee estimation, and
	// broadcast.
	ChainSource actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]

	// Wallet provides confirmed fee inputs and PSBT finalization for anchor
	// based CPFP children.
	Wallet Wallet

	// Log is an optional logger.
	Log fn.Option[btclog.Logger]

	// FeeBumpIntervalBlocks controls how many new blocks the actor waits
	// before retrying an unconfirmed transaction. Zero falls back to
	// DefaultFeeBumpIntervalBlocks.
	FeeBumpIntervalBlocks int32

	// BroadcastFailureAlertThreshold is the number of consecutive
	// initial-broadcast failures (a tx that reached no mempool at all)
	// after which the actor escalates with an operator-visible warning.
	// The actor never gives up on the tx — escalation is informational so
	// dashboards and alerts can distinguish "cannot broadcast" from a tx
	// that is genuinely awaiting confirmation. Zero falls back to
	// DefaultBroadcastFailureAlertThreshold.
	//
	// Because re-attempts are interval-paced, the effective time to the
	// first alert is roughly BroadcastFailureAlertThreshold *
	// FeeBumpIntervalBlocks blocks. Tune both together when changing how
	// quickly a stuck fund-risk tx should surface to operators.
	BroadcastFailureAlertThreshold int

	// MaxFeeRateSatPerVByte caps fee estimates used by the internal CPFP
	// broadcaster. Zero falls back to DefaultMaxFeeRateSatPerVByte.
	MaxFeeRateSatPerVByte int64

	// IncrementalRelayFeeSatPerVByte is forwarded to the internal CPFP
	// broadcaster to enforce BIP-125 Rule 4 on fee-bump replacements.
	// Zero falls back to DefaultIncrementalRelayFeeSatPerVByte.
	IncrementalRelayFeeSatPerVByte int64

	// PreSubmitTestMempoolAccept is forwarded to the internal CPFP
	// broadcaster. When true, every broadcast attempt is preflighted
	// against ChainSource.TestMempoolAccept and rejected locally if
	// the backend reports a policy violation. Safe to leave enabled on
	// backends that do not implement testmempoolaccept — the
	// unsupported case is downgraded to a soft-miss.
	PreSubmitTestMempoolAccept bool
}

// TxBroadcasterActor is a generic shared actor that deduplicates
// confirmation requests by txid and ensures transactions confirm on-chain.
//
// The actor is intentionally not tied to unrolling. Any subsystem can
// reuse it by providing signed transactions, an optional wallet for
// anchor-backed CPFP, and a subscriber reference for terminal
// notifications.
//
// Invariants upheld by this type (cross-reference the package doc):
//
//   - Receive is single-threaded. All mutation of a.tracked,
//     a.bestHeight, etc. happens from a single goroutine.
//
//   - For every non-terminal entry in a.tracked, exactly one
//     chainsource confirmation watch is registered and exactly one
//     tracked-tx FSM goroutine is alive. Terminal entries hold neither.
//
//   - A.tracked contains terminal entries only while at least one
//     subscriber still needs terminal notification delivery. These
//     entries no longer hold a conf watch and are retried on later
//     actor ticks until every subscriber is notified.
//
//   - The shared block subscription is started lazily on the first
//     ensure request and torn down on OnStop.
type TxBroadcasterActor struct {
	cfg Config
	log btclog.Logger

	// selfRef receives mapped chainsource callbacks.
	selfRef actor.TellOnlyRef[Msg]

	// broadcaster handles direct broadcast and anchor-aware CPFP package
	// submission.
	broadcaster *CPFPBroadcaster

	// feeBumpFSM owns the fee-input fanout lifecycle: when a CPFP child
	// cannot find a confirmed wallet fee input, it broadcasts a fanout
	// transaction that mints right-sized fee inputs and promotes them once
	// it confirms. The FSM does its own wallet/broadcast IO inside
	// transitions and shares the broadcaster's per-parent reservation map;
	// the actor drives it with events and applies the conf-watch / retry
	// effects it emits as outbox events.
	feeBumpFSM *feeBumpStateMachine

	// feeBumpEnv is the fanout FSM's environment, retained so the actor can
	// read back the per-turn operational error a transition stashed (rather
	// than returned, which would tear the long-lived FSM down).
	feeBumpEnv *feeBumpEnvironment

	// tracked maps txid to its shared confirmation state.
	tracked map[chainhash.Hash]*trackedTx

	// terminalNotifyInflight tracks terminal notifications that timed out
	// on the actor path but still have a background Tell in progress.
	terminalNotifyInflight map[string]struct{}

	// bestHeight is the last observed best block height.
	bestHeight int32

	// hasBestHeight reports whether bestHeight has been initialized.
	hasBestHeight bool

	// blockSubscriptionActive reports whether the shared block subscription
	// is active.
	blockSubscriptionActive bool
}

// trackedTx stores the actor-owned handle for one tracked txid.
//
// The struct is the actor's single source of truth about a tracked
// transaction: callers never hold a *trackedTx directly, they interact
// only via actor messages. Mutation happens exclusively from the actor
// goroutine so the fields are not mutex-guarded.
type trackedTx struct {
	data trackedTxData
	fsm  *trackedTxStateMachine

	subscribers map[string]actor.TellOnlyRef[Notification]

	// escalateLog rate-limits the operator-facing escalation that fires
	// once a tx has failed to reach any mempool repeatedly. It
	// edge-triggers on the first crossing of the alert threshold and then
	// emits at most one reminder per broadcastEscalationReminder, so a
	// persistently stuck tx stays visible without flooding the log on every
	// retry interval.
	escalateLog rate.Sometimes

	// confWatchRegistered reports whether a chainsource confirmation
	// watch is currently active for this txid. It is flipped true by
	// registerConfWatch on success and false by unregisterConfWatch on
	// success. Terminal cleanup uses it to avoid redundant unregister
	// round trips for entries whose watch was never registered (e.g.
	// entries that failed during block-subscription setup).
	confWatchRegistered bool

	// pendingTargetFeeRate carries a one-shot operator-supplied fee rate
	// (sat/vB) for the next fee bump, set by a BumpNowReq. It overrides the
	// estimator for exactly one bump and is cleared once consumed, so
	// subsequent interval-paced bumps fall back to the estimator. Zero
	// means "no override pending".
	pendingTargetFeeRate int64
}

// confirmationObservedMsg routes a chainsource confirmation callback back into
// the actor mailbox.
type confirmationObservedMsg struct {
	actor.BaseMessage
	txid        chainhash.Hash
	blockHeight int32
	numConfs    uint32
}

// MessageType returns the stable message type identifier.
func (m *confirmationObservedMsg) MessageType() string {
	return "confirmationObservedMsg"
}

// txConfirmMsgSealed seals confirmationObservedMsg into the package message
// set.
func (m *confirmationObservedMsg) txConfirmMsgSealed() {}

// terminalNotifyResultMsg returns the result of a terminal notification that
// outlived the actor-path wait budget.
type terminalNotifyResultMsg struct {
	actor.BaseMessage

	txid         chainhash.Hash
	subscriberID string
	inflightKey  string
	err          error
}

// MessageType returns the stable message type identifier.
func (m *terminalNotifyResultMsg) MessageType() string {
	return "terminalNotifyResultMsg"
}

// txConfirmMsgSealed seals terminalNotifyResultMsg into the package message
// set.
func (m *terminalNotifyResultMsg) txConfirmMsgSealed() {}

// blockEpochObservedMsg routes a chainsource block callback back into the
// actor mailbox.
type blockEpochObservedMsg struct {
	actor.BaseMessage
	height int32
}

// MessageType returns the stable message type identifier.
func (m *blockEpochObservedMsg) MessageType() string {
	return "blockEpochObservedMsg"
}

// txConfirmMsgSealed seals blockEpochObservedMsg into the package message set.
func (m *blockEpochObservedMsg) txConfirmMsgSealed() {}

// NewTxBroadcasterActor creates a new generic shared tx confirmation actor
// behavior.
func NewTxBroadcasterActor(cfg Config) *TxBroadcasterActor {
	if cfg.FeeBumpIntervalBlocks <= 0 {
		cfg.FeeBumpIntervalBlocks = DefaultFeeBumpIntervalBlocks
	}

	if cfg.BroadcastFailureAlertThreshold <= 0 {
		cfg.BroadcastFailureAlertThreshold =
			DefaultBroadcastFailureAlertThreshold
	}

	// The fanout FSM shares the broadcaster's per-parent reservation map,
	// so it is constructed from the broadcaster rather than alongside it.
	broadcaster := NewCPFPBroadcaster(BroadcasterConfig{
		ChainSource:                    cfg.ChainSource,
		Wallet:                         cfg.Wallet,
		Log:                            cfg.Log,
		MaxFeeRateSatPerVByte:          cfg.MaxFeeRateSatPerVByte,
		IncrementalRelayFeeSatPerVByte: cfg.IncrementalRelayFeeSatPerVByte,
		PreSubmitTestMempoolAccept:     cfg.PreSubmitTestMempoolAccept,
	})

	log := cfg.Log.UnwrapOr(btclog.Disabled)
	feeBumpFSM, feeBumpEnv := newFeeBumpStateMachine(broadcaster, log)

	return &TxBroadcasterActor{
		cfg:                    cfg,
		log:                    log,
		broadcaster:            broadcaster,
		feeBumpFSM:             feeBumpFSM,
		feeBumpEnv:             feeBumpEnv,
		tracked:                make(map[chainhash.Hash]*trackedTx),
		terminalNotifyInflight: make(map[string]struct{}),
	}
}

// SetSelfRef sets the actor's self-reference so chainsource callbacks can be
// mapped back into the actor mailbox.
func (a *TxBroadcasterActor) SetSelfRef(ref actor.TellOnlyRef[Msg]) {
	a.selfRef = ref
}

// Receive processes one tx confirmation actor message.
func (a *TxBroadcasterActor) Receive(ctx context.Context,
	msg Msg) fn.Result[Resp] {

	switch req := msg.(type) {
	case *EnsureConfirmedReq:
		resp, err := a.handleEnsure(ctx, req)
		if err != nil {
			return fn.Err[Resp](err)
		}

		return fn.Ok[Resp](resp)

	case *CancelInterestReq:
		resp, err := a.handleCancel(ctx, req)
		if err != nil {
			return fn.Err[Resp](err)
		}

		return fn.Ok[Resp](resp)

	case *BumpNowReq:
		resp, err := a.handleBumpNow(ctx, req)
		if err != nil {
			return fn.Err[Resp](err)
		}

		return fn.Ok[Resp](resp)

	case *confirmationObservedMsg:
		a.handleConfirmationObserved(ctx, req)

		return fn.Ok[Resp](&EnsureConfirmedResp{
			Txid:  req.txid,
			State: TxStateConfirmed,
		})

	case *blockEpochObservedMsg:
		a.handleBlockObserved(ctx, req)

		return fn.Ok[Resp](&EnsureConfirmedResp{
			State: TxStateAwaitingConfirmation,
		})

	case *terminalNotifyResultMsg:
		a.handleTerminalNotifyResult(ctx, req)

		return fn.Ok[Resp](&EnsureConfirmedResp{
			Txid: req.txid,
		})

	default:
		return fn.Err[Resp](
			fmt.Errorf("unknown txconfirm message: %T", msg),
		)
	}
}

// OnStop cleans up block and confirmation subscriptions held by the actor.
func (a *TxBroadcasterActor) OnStop(ctx context.Context) error {
	var firstErr error

	if a.blockSubscriptionActive && a.selfRef != nil {
		_, err := a.cfg.ChainSource.Ask(
			ctx, &chainsource.UnsubscribeBlocksRequest{
				CallerID: a.blockCallerID(),
			},
		).Await(ctx).Unpack()
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("unsubscribe blocks: %w", err)
		}
	}

	for _, entry := range a.tracked {
		state, err := entry.currentTxState()
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("current tx state %s: %w",
					entry.data.Txid, err)
			}

			continue
		}

		if state == TxStateConfirmed || state == TxStateFailed {
			if entry.fsm != nil {
				entry.fsm.Stop()
			}

			// Still evict here: terminal entries can hold
			// parent state between notifyConfirmed and the
			// tracked-map delete if OnStop races against the
			// tail end of a confirmation, and Evict is a
			// no-op when parentStates has no entry.
			a.broadcaster.Evict(ctx, entry.data.Txid)
			a.driveFeeBump(ctx, &feeBumpParentEvicted{
				parentTxid: entry.data.Txid,
			})

			continue
		}

		if err := a.unregisterConfWatch(ctx, entry); err != nil &&
			firstErr == nil {

			firstErr = err
		}

		if entry.fsm != nil {
			entry.fsm.Stop()
		}

		// Release the broadcaster's per-parent bump state and any
		// wallet-level fee-input lease it holds. Without this, a
		// daemon restart leaves lease rows in backends that persist
		// them across restarts (btcwallet, lndclient WalletKit) until
		// their configured expiry fires, blocking unrelated wallet
		// coin selection after restart.
		a.broadcaster.Evict(ctx, entry.data.Txid)
		a.driveFeeBump(ctx, &feeBumpParentEvicted{
			parentTxid: entry.data.Txid,
		})
	}

	// Tear down the fanout FSM goroutine alongside the per-tracked-tx FSMs.
	if a.feeBumpFSM != nil {
		a.feeBumpFSM.Stop()
	}

	return firstErr
}

// handleEnsure creates or reuses confirmation tracking for one txid.
func (a *TxBroadcasterActor) handleEnsure(ctx context.Context,
	req *EnsureConfirmedReq) (*EnsureConfirmedResp, error) {

	if req == nil {
		return nil, fmt.Errorf("ensure request required")
	}

	if req.Tx == nil {
		return nil, fmt.Errorf("ensure request tx required")
	}

	if req.Subscriber == nil {
		return nil, fmt.Errorf("ensure request subscriber required")
	}

	if a.selfRef == nil {
		return nil, fmt.Errorf("self ref must be set before use")
	}

	txid := req.Tx.TxHash()
	if existing, ok := a.tracked[txid]; ok {
		if err := validateEnsureMatch(req, existing); err != nil {
			return nil, err
		}

		return a.attachExistingSubscriber(
			ctx, existing, req.Subscriber,
		), nil
	}

	if err := a.ensureBestHeight(ctx); err != nil {
		return nil, fmt.Errorf("best height: %w", err)
	}

	entry, err := a.newTrackedTx(ctx, req)
	if err != nil {
		return nil, err
	}
	a.tracked[txid] = entry

	if err := a.ensureBlockSubscription(ctx); err != nil {
		a.failTrackedTx(
			ctx, entry, fmt.Sprintf("subscribe blocks: %v", err),
		)

		return a.ensureResp(entry, true), nil
	}

	if err := a.registerConfWatch(ctx, entry); err != nil {
		a.failTrackedTx(
			ctx, entry, fmt.Sprintf("register conf: %v", err),
		)

		return a.ensureResp(entry, true), nil
	}

	_, err = a.broadcastTrackedTx(ctx, entry, TxStateBroadcasting)
	a.recordInitialBroadcastOutcome(ctx, entry, err)

	return a.ensureResp(entry, true), nil
}

// recordInitialBroadcastOutcome advances the FSM based on the result of an
// initial (or retried) broadcast for a tracked tx that had not yet reached any
// mempool. It is shared by the initial submit in handleEnsure and the
// interval-driven re-attempt in handleBlockObserved so both apply identical
// state semantics.
//
// The outcome is routed by error class:
//
//   - nil: broadcastTrackedTx already advanced the FSM to
//     AwaitingConfirmation, so there is nothing left to do.
//
//   - ErrParentAlreadyBroadcast: a live parent genuinely exists on another
//     path (typically the operator's redundant CPFP racing the client's own
//     broadcast). The conf watch is already registered, so advance to
//     AwaitingConfirmation and let it ride.
//
//   - a permanent/structural error (see isPermanentBroadcastError): the tx can
//     never be accepted as submitted, so retrying is pointless. Fail it
//     terminally and notify subscribers.
//
//   - any other error on an anchor (CPFP) parent: the tx reached no mempool but
//     the failure is plausibly transient — a missing confirmed fee input
//     (ErrCPFPFeeInputUnavailable), a min-relay-fee rejection of the zero-fee
//     anchor parent, a mempool-full condition, or a fee input spent out from
//     under us. These are exactly the conditions CPFP retry exists to overcome,
//     so the tx stays in Broadcasting (it must NOT report
//     AwaitingConfirmation), is re-attempted on the next interval, and
//     escalates to the operator past the configured threshold. The actor never
//     gives up on such a tx: a fraud-response checkpoint must land before the
//     counterparty's CSV-timeout path can win.
//
//   - any other error on a non-anchor parent: a plain direct broadcast with no
//     CPFP retry machinery. There is nothing to fee-bump and no fund-risk
//     retry contract, so fail it terminally as before.
func (a *TxBroadcasterActor) recordInitialBroadcastOutcome(ctx context.Context,
	entry *trackedTx, err error) {

	a.maybeEnsureFeeInputSupply(ctx, err)

	switch {
	case err == nil:
		return

	case errors.Is(err, ErrParentAlreadyBroadcast):
		// A live parent exists on another path, so the conf watch can
		// ride to confirmation. Advance to AwaitingConfirmation.
		a.log.WarnS(ctx,
			"Initial anchor broadcast deferring to existing path",
			err, "txid", entry.data.Txid,
		)

		_ = a.advanceTrackedTxFSM(
			ctx, entry, &trackedTxBroadcastAccepted{
				Progress: trackedTxProgress{
					LastBroadcastHeight: fn.Some(
						a.bestHeight,
					),
				},
			},
		)

	case isPermanentBroadcastError(err):
		// The tx is structurally unacceptable (e.g. a non-TRUC parent);
		// no amount of retrying will land it, so fail terminally.
		a.failTrackedTx(
			ctx, entry, fmt.Sprintf("broadcast: %v", err),
		)

	case findAnchorOutput(entry.data.Tx) >= 0:
		// An anchor (CPFP) parent reached no mempool. The failure is
		// plausibly transient and this is the fund-risk path, so stay
		// in Broadcasting, re-attempt next interval, and escalate
		// rather than give up.
		a.recordBroadcastFailure(ctx, entry, err)

	default:
		// A non-anchor direct broadcast failed and has no CPFP retry
		// contract; fail terminally.
		a.failTrackedTx(
			ctx, entry, fmt.Sprintf("broadcast: %v", err),
		)
	}
}

// isPermanentBroadcastError reports whether a broadcast error is structural and
// can never succeed on retry, so an anchor-bearing tracked tx should fail
// terminally rather than spin in the Broadcasting retry loop. Transient
// conditions on an anchor parent are kept retryable, because on a fund-risk
// path (e.g. a fraud-response checkpoint) it is safer to keep trying and alert
// an operator than to silently abort a tx that must confirm.
func isPermanentBroadcastError(err error) bool {

	// A non-TRUC (non-v3) parent is a caller/programming error: the version
	// gate rejects it identically on every attempt, so retrying only spams.
	return errors.Is(err, ErrNonTRUCParent)
}

// recordBroadcastFailure keeps a never-broadcast tx in the Broadcasting state,
// increments its consecutive-failure counter, and escalates to the operator
// once the counter crosses the configured threshold. It never transitions the
// tx to a terminal state: a fraud-response checkpoint (and similar fund-risk
// txs) must keep retrying until it lands, so the escalation is informational
// rather than a give-up.
func (a *TxBroadcasterActor) recordBroadcastFailure(ctx context.Context,
	entry *trackedTx, cause error) {

	currentState, stateErr := entry.currentFSMState()
	if stateErr != nil {
		a.log.WarnS(ctx, "Failed to read tracked tx state",
			stateErr, "txid", entry.data.Txid)

		return
	}

	failures := trackedTxBroadcastFailures(currentState) + 1

	// The per-attempt record is routine (one line per retry interval), so
	// it logs at debug; the operator-facing signal is the throttled
	// escalation below.
	a.log.DebugS(ctx,
		"Initial broadcast reached no mempool; will retry",
		"txid", entry.data.Txid,
		slog.String("label", entry.data.Label),
		slog.Int("consecutive_failures", failures),
		slog.Any("cause", cause),
	)

	_ = a.advanceTrackedTxFSM(
		ctx, entry, &trackedTxBroadcastFailed{
			Progress: trackedTxProgress{
				LastBroadcastHeight: fn.Some(a.bestHeight),
				BroadcastFailures:   failures,
			},
		},
	)

	// Escalate once the failure count reaches the threshold. escalateLog
	// edge-triggers on the first crossing and then emits at most one
	// reminder per broadcastEscalationReminder, so a persistently stuck tx
	// stays visible without a warning on every retry interval. The message
	// is intentionally static so alerting can key on it.
	if failures >= a.cfg.BroadcastFailureAlertThreshold {
		entry.escalateLog.Do(func() {
			a.log.WarnS(ctx,
				"Tx broadcast repeatedly failing; operator "+
					"intervention may be required",
				cause,
				"txid", entry.data.Txid,
				slog.String("label", entry.data.Label),
				slog.Int("consecutive_failures", failures),
			)
		})
	}
}

// handleCancel removes one subscriber from one tracked txid.
func (a *TxBroadcasterActor) handleCancel(ctx context.Context,
	req *CancelInterestReq) (*CancelInterestResp, error) {

	if req == nil {
		return nil, fmt.Errorf("cancel request required")
	}

	entry, ok := a.tracked[req.Txid]
	if !ok {
		return &CancelInterestResp{
			Txid: req.Txid,
		}, nil
	}

	_, removed := entry.subscribers[req.SubscriberID]
	delete(entry.subscribers, req.SubscriberID)

	resp := &CancelInterestResp{
		Txid:                 req.Txid,
		Removed:              removed,
		RemainingSubscribers: len(entry.subscribers),
	}

	if len(entry.subscribers) != 0 {
		return resp, nil
	}

	state, err := entry.currentTxState()
	if err != nil {
		return nil, err
	}

	if state == TxStateConfirmed || state == TxStateFailed {
		a.evictTerminal(ctx, entry)

		return resp, nil
	}

	if err := a.unregisterConfWatch(ctx, entry); err != nil {
		a.log.WarnS(ctx, "Failed to unregister confirmation watch",
			err, "txid", entry.data.Txid)
	}

	if entry.fsm != nil {
		entry.fsm.Stop()
	}

	// Release the broadcaster's per-parent state so any wallet-level
	// fee-input lease we took during broadcastWithCPFP is released
	// immediately, rather than lingering until the wallet's auto-expiry.
	// Without this, a caller who cancels before confirmation can starve
	// subsequent broadcasts of the same UTXO for up to an hour.
	a.broadcaster.Evict(ctx, entry.data.Txid)
	a.driveFeeBump(ctx, &feeBumpParentEvicted{
		parentTxid: entry.data.Txid,
	})

	delete(a.tracked, entry.data.Txid)
	resp.StoppedTracking = true

	return resp, nil
}

// handleBumpNow forces an immediate CPFP fee bump of an already-tracked
// transaction at an operator-supplied target rate, rather than waiting for the
// next interval-paced bump. It is a no-op (reported via Bumped=false) when the
// txid is not tracked, has already reached a terminal state, has not yet
// reached a mempool (still broadcasting, so there is nothing to bump), or
// carries no anchor to attach a child to. Otherwise it stamps the one-shot
// target rate onto the entry and runs one fee-bump pass, returning the
// submitted child's txid.
func (a *TxBroadcasterActor) handleBumpNow(ctx context.Context,
	req *BumpNowReq) (*BumpNowResp, error) {

	if req == nil {
		return nil, fmt.Errorf("bump request required")
	}

	entry, ok := a.tracked[req.Txid]
	if !ok {
		return &BumpNowResp{
			Txid:   req.Txid,
			Bumped: false,
			Reason: "transaction not tracked",
		}, nil
	}

	state, err := entry.currentTxState()
	if err != nil {
		return nil, err
	}

	// Locate the anchor up front: it decides both whether a bump is
	// possible at all and whether a never-broadcast parent can be carried
	// into the mempool by a package submit.
	anchorIdx := findAnchorOutput(entry.data.Tx)
	funded := anchorIdx >= 0 && anchorIsFunded(entry.data.Tx, anchorIdx)

	switch {
	case state == TxStateConfirmed:
		return &BumpNowResp{
			Txid:   req.Txid,
			State:  state,
			Reason: "transaction already confirmed",
		}, nil

	case state == TxStateFailed:
		return &BumpNowResp{
			Txid:   req.Txid,
			State:  state,
			Reason: "transaction in terminal failure",
		}, nil

	// An entry still in New never accepted its initial submission event;
	// there is nothing coherent to bump yet.
	case state == TxStateNew:
		return &BumpNowResp{
			Txid:   req.Txid,
			State:  state,
			Reason: "transaction not yet submitted",
		}, nil

	// A concurrent bump pass is already in flight (or the FSM was left
	// mid-transition); stacking a second one would race the replacement
	// accounting.
	case state == TxStateFeeBumping:
		return &BumpNowResp{
			Txid:   req.Txid,
			State:  state,
			Reason: "fee bump already in progress",
		}, nil

	// An ephemeral (zero-fee) parent that has not reached any mempool is
	// already being re-submitted as a full CPFP package by the retry loop
	// on every interval; a forced pass adds nothing. A FUNDED parent in
	// Broadcasting is different — its retries are plain direct broadcasts
	// at its fixed fee, so it falls through to the forced package submit
	// below, which is exactly what carries a never-in-mempool parent in.
	case state == TxStateBroadcasting && !funded:
		return &BumpNowResp{
			Txid:   req.Txid,
			State:  state,
			Reason: "transaction not yet in mempool",
		}, nil
	}

	// Only an anchor-bearing parent can be CPFP-bumped; a plain parent has
	// no child handle, so report the no-op rather than spinning up a doomed
	// fee-bump pass.
	if anchorIdx < 0 {
		return &BumpNowResp{
			Txid:   req.Txid,
			State:  state,
			Reason: "transaction has no anchor to bump",
		}, nil
	}

	// Stamp the one-shot operator target rate; broadcastTrackedTxOpts
	// consumes and clears the override on every outcome.
	entry.pendingTargetFeeRate = req.TargetFeeRateSatPerVByte

	// A funded parent still in Broadcasting never reached a mempool on its
	// own fee, so its bump stays on the Broadcasting state track (the
	// success condition is "parent finally landed") while still submitting
	// as a parent+child package.
	nextState := TxStateFeeBumping
	if state == TxStateBroadcasting {
		nextState = TxStateBroadcasting
	}

	result, bumpErr := a.broadcastTrackedTxOpts(ctx, entry, nextState, true)
	if bumpErr != nil {
		// A never-broadcast parent routes its failure through the
		// shared initial-outcome handler: it stays in Broadcasting
		// (with the escalation counter ticking) or fails terminally on
		// structural errors, identical to an interval retry.
		if nextState == TxStateBroadcasting {
			a.recordInitialBroadcastOutcome(ctx, entry, bumpErr)

			bumpedState, _ := entry.currentTxState()

			return &BumpNowResp{
				Txid:   req.Txid,
				State:  bumpedState,
				Bumped: false,
				Reason: fmt.Sprintf("fee bump failed: %v",
					bumpErr),
			}, nil
		}

		a.maybeEnsureFeeInputSupply(ctx, bumpErr)

		// A failed forced bump is non-terminal, exactly like an
		// interval-paced bump failure: the original broadcast is still
		// live and the confirmation watch remains active. Recover the
		// FSM back to AwaitingConfirmation with an updated broadcast
		// height so the next interval bump waits the full interval.
		a.log.WarnS(ctx, "Forced fee bump failed", bumpErr,
			"txid", entry.data.Txid)

		_ = a.advanceTrackedTxFSM(
			ctx, entry, &trackedTxBroadcastAccepted{
				Progress: trackedTxProgress{
					LastBroadcastHeight: fn.Some(
						a.bestHeight,
					),
				},
			},
		)

		bumpedState, _ := entry.currentTxState()

		return &BumpNowResp{
			Txid:   req.Txid,
			State:  bumpedState,
			Bumped: false,
			Reason: fmt.Sprintf("fee bump failed: %v", bumpErr),
		}, nil
	}

	bumpedState, _ := entry.currentTxState()

	// A submission that produced no CPFP child did not actually bump
	// anything: the ephemeral path's hail-mary fallback re-broadcasts the
	// parent directly when child setup fails, and reporting that as a
	// successful bump would hide the failure from the operator. Report the
	// honest no-op instead; the tracked tx remains live either way.
	if result.ChildTxid == nil {
		return &BumpNowResp{
			Txid:   req.Txid,
			State:  bumpedState,
			Bumped: false,
			Reason: "cpfp child unavailable; parent re-broadcast " +
				"directly without a fee bump",
		}, nil
	}

	// Surface the rate the package actually targets so a silently clamped
	// operator request is visible: Clamped is true when the supplied target
	// exceeded the broadcaster's ceiling and was reduced.
	return &BumpNowResp{
		Txid:      req.Txid,
		State:     bumpedState,
		Bumped:    true,
		ChildTxid: copyHash(result.ChildTxid),

		EffectiveFeeRateSatPerVByte: result.FeeRate,
		Clamped: req.TargetFeeRateSatPerVByte > 0 &&
			result.FeeRate < req.TargetFeeRateSatPerVByte,
	}, nil
}

// handleConfirmationObserved marks a tracked txid as confirmed and fans the
// result out to all subscribers.
func (a *TxBroadcasterActor) handleConfirmationObserved(ctx context.Context,
	msg *confirmationObservedMsg) {

	if a.handleFanoutConfirmed(ctx, msg) {
		return
	}

	entry, ok := a.tracked[msg.txid]
	if !ok {
		return
	}

	state, err := entry.currentTxState()
	if err != nil {
		a.log.WarnS(ctx, "Failed to read tracked tx state",
			err, "txid", entry.data.Txid)

		return
	}

	if state == TxStateConfirmed || state == TxStateFailed {
		if a.retryTerminalNotifications(ctx, entry) {
			a.evictTerminal(ctx, entry)
		}

		return
	}

	if err := a.advanceTrackedTxFSM(ctx, entry, &trackedTxConfirmed{
		BlockHeight: msg.blockHeight,
	}); err != nil {

		a.log.WarnS(ctx, "Failed to confirm tracked tx FSM",
			err, "txid", entry.data.Txid)

		return
	}

	if err := a.unregisterConfWatch(ctx, entry); err != nil {
		a.log.WarnS(ctx, "Failed to unregister confirmation watch",
			err, "txid", entry.data.Txid)
	}

	if a.notifyConfirmed(ctx, entry, msg.blockHeight, msg.numConfs) {
		a.evictTerminal(ctx, entry)
	}
}

// handleBlockObserved records a new best height and drives interval-paced
// retries for eligible pending transactions. Two distinct retry paths exist:
//
//   - A tx still in Broadcasting reached no mempool on its initial attempt.
//     It is re-broadcast from scratch and the result routed through
//     recordInitialBroadcastOutcome, so a continued failure stays in
//     Broadcasting (and escalates) while a success advances to
//     AwaitingConfirmation.
//
//   - A tx in AwaitingConfirmation already reached a mempool and is
//     fee-bumped. Fee-bump failures are intentionally non-terminal: the
//     original broadcast is still live and the confirmation watch remains
//     active, so the tx may still confirm on its own. We recover the FSM back
//     to AwaitingConfirmation with the new height so the next block
//     observation evaluates eligibility freshly — an attempt that failed at
//     height H is not retried until at least FeeBumpIntervalBlocks have
//     elapsed since H.
func (a *TxBroadcasterActor) handleBlockObserved(ctx context.Context,
	msg *blockEpochObservedMsg) {

	if !a.hasBestHeight || msg.height > a.bestHeight {
		a.bestHeight = msg.height
		a.hasBestHeight = true
	}

	for _, entry := range a.tracked {
		state, err := entry.currentTxState()
		if err != nil {
			a.log.WarnS(ctx, "Failed to read tracked tx state",
				err, "txid", entry.data.Txid)
			continue
		}

		if state == TxStateConfirmed || state == TxStateFailed {
			if a.retryTerminalNotifications(ctx, entry) {
				a.evictTerminal(ctx, entry)
			}

			continue
		}

		switch {
		// A tx that never reached any mempool is re-attempted from
		// scratch and routed through the shared outcome handler.
		case a.shouldRetryBroadcast(entry):
			_, err := a.broadcastTrackedTx(
				ctx, entry, TxStateBroadcasting,
			)
			a.recordInitialBroadcastOutcome(ctx, entry, err)

		// A tx already in a mempool is fee-bumped.
		case a.shouldFeeBump(entry):
			if _, err := a.broadcastTrackedTx(
				ctx, entry, TxStateFeeBumping,
			); err != nil {

				a.maybeEnsureFeeInputSupply(ctx, err)

				// Fee-bump failures are non-terminal. The
				// original broadcast is still live and the
				// confirmation watch remains active, so the tx
				// may still confirm without the bump. Recover
				// the FSM back to AwaitingConfirmation with an
				// updated broadcast height so the next bump
				// waits the full interval.
				a.log.WarnS(ctx, "Fee bump failed, will retry",
					err, "txid", entry.data.Txid)

				progress := trackedTxProgress{
					LastBroadcastHeight: fn.Some(
						a.bestHeight,
					),
				}
				_ = a.advanceTrackedTxFSM(
					ctx, entry, &trackedTxBroadcastAccepted{
						Progress: progress,
					},
				)
			}
		}
	}
}

// attachExistingSubscriber attaches a new subscriber to an already-tracked
// txid or immediately replays a terminal result.
func (a *TxBroadcasterActor) attachExistingSubscriber(
	ctx context.Context, entry *trackedTx,
	subscriber actor.TellOnlyRef[Notification],
) *EnsureConfirmedResp {

	state, err := entry.currentFSMState()
	if err != nil {
		a.notifyOneFailed(
			ctx, subscriber, entry.data.Txid,
			fmt.Sprintf("tracked tx state: %v", err),
		)

		return &EnsureConfirmedResp{
			Txid:  entry.data.Txid,
			State: TxStateFailed,
		}
	}

	switch state := state.(type) {
	case *trackedTxStateConfirmed:
		confirmHeight, _ := trackedTxConfirmHeight(state)
		if !a.notifyOneConfirmed(
			ctx, subscriber, entry.data.Txid, confirmHeight,
			entry.data.TargetConfs,
		) {

			entry.subscribers[subscriber.ID()] = subscriber
		}

	case *trackedTxStateFailed:
		reason, _ := trackedTxFailureReason(state)
		if !a.notifyOneFailed(ctx, subscriber, entry.data.Txid,
			reason) {

			entry.subscribers[subscriber.ID()] = subscriber
		}

	default:
		entry.subscribers[subscriber.ID()] = subscriber
	}

	return a.ensureResp(entry, false)
}

// ensureResp constructs one EnsureConfirmedResp from the current entry state.
func (a *TxBroadcasterActor) ensureResp(entry *trackedTx,
	created bool) *EnsureConfirmedResp {

	state, err := entry.currentTxState()
	if err != nil {
		state = TxStateFailed
	}

	return &EnsureConfirmedResp{
		Txid:    entry.data.Txid,
		State:   state,
		Created: created,
	}
}

// newTrackedTx constructs the initial state for a newly-tracked txid.
func (a *TxBroadcasterActor) newTrackedTx(ctx context.Context,
	req *EnsureConfirmedReq) (*trackedTx, error) {

	targetConfs := normalizeTargetConfs(req)

	txCopy := req.Tx.Copy()
	txid := txCopy.TxHash()
	confirmationPkScript, err := confirmationPkScriptForRequest(req, txCopy)
	if err != nil {
		return nil, err
	}
	heightHint := req.HeightHint
	if heightHint == 0 {
		heightHint = defaultHeightHint(a.bestHeight)
	}

	fsmLog := a.log.WithPrefix("trackedtx(" + txid.String() + ")")
	data := trackedTxData{
		Tx:   txCopy,
		Txid: txid,
		ConfirmationPkScript: append(
			[]byte(nil), confirmationPkScript...,
		),
		Label:       req.Label,
		HeightHint:  heightHint,
		TargetConfs: targetConfs,
		ParentFee:   req.ParentFee,
	}
	fsm := newTrackedTxStateMachine(fsmLog, data)
	fsm.Start(ctx)

	return &trackedTx{
		data: data,
		fsm:  fsm,
		subscribers: map[string]actor.TellOnlyRef[Notification]{
			req.Subscriber.ID(): req.Subscriber,
		},
		escalateLog: rate.Sometimes{
			First:    1,
			Interval: broadcastEscalationReminder,
		},
	}, nil
}

// defaultHeightHint derives a nonzero confirmation height hint from the
// actor's latest observed best height.
func defaultHeightHint(bestHeight int32) uint32 {
	if bestHeight <= 0 {
		return 1
	}

	return uint32(bestHeight)
}

// normalizeTargetConfs returns the effective TargetConfs the actor will
// track for a request, applying the zero-value default (1) consistently
// with newTrackedTx.
func normalizeTargetConfs(req *EnsureConfirmedReq) uint32 {
	if req.TargetConfs == 0 {
		return 1
	}

	return req.TargetConfs
}

// validateEnsureMatch checks that an incoming EnsureConfirmedReq is
// compatible with the already-tracked entry for the same txid. Two
// callers that share a txid must also agree on TargetConfs and
// ConfirmationPkScript, otherwise the confirmation notification one of
// them receives would not match the criteria it asked for.
func validateEnsureMatch(req *EnsureConfirmedReq, existing *trackedTx) error {
	reqConfs := normalizeTargetConfs(req)
	if reqConfs != existing.data.TargetConfs {
		return fmt.Errorf("%w: txid=%s existing=%d incoming=%d",
			ErrEnsureParamsMismatch, existing.data.Txid,
			existing.data.TargetConfs, reqConfs)
	}

	reqScript, err := confirmationPkScriptForRequest(req, req.Tx)
	if err != nil {
		return err
	}

	if !bytes.Equal(reqScript, existing.data.ConfirmationPkScript) {
		return fmt.Errorf("%w: txid=%s pkscript mismatch",
			ErrEnsureParamsMismatch, existing.data.Txid)
	}

	return nil
}

// confirmationPkScriptForRequest returns the script txconfirm should watch for
// confirmations of the tracked transaction.
func confirmationPkScriptForRequest(req *EnsureConfirmedReq,
	tx *wire.MsgTx) ([]byte, error) {

	if len(req.ConfirmationPkScript) != 0 {
		return append([]byte(nil), req.ConfirmationPkScript...), nil
	}

	if tx == nil {
		return nil, fmt.Errorf("ensure request tx required")
	}

	if len(tx.TxOut) == 0 {
		return nil, fmt.Errorf("confirmation pkscript required")
	}

	if len(tx.TxOut[0].PkScript) == 0 {
		return nil, fmt.Errorf("confirmation pkscript required")
	}

	return append([]byte(nil), tx.TxOut[0].PkScript...), nil
}

// ensureBestHeight loads the current best block height on first use.
func (a *TxBroadcasterActor) ensureBestHeight(ctx context.Context) error {
	if a.hasBestHeight {
		return nil
	}

	resp, err := a.cfg.ChainSource.Ask(
		ctx, &chainsource.BestHeightRequest{},
	).Await(ctx).Unpack()
	if err != nil {
		return err
	}

	bestResp, ok := resp.(*chainsource.BestHeightResponse)
	if !ok {
		return fmt.Errorf("unexpected best height response %T", resp)
	}

	a.bestHeight = bestResp.Height
	a.hasBestHeight = true

	return nil
}

// ensureBlockSubscription starts the shared block epoch subscription on first
// use.
func (a *TxBroadcasterActor) ensureBlockSubscription(
	ctx context.Context) error {

	if a.blockSubscriptionActive {
		return nil
	}

	notifyRef := chainsource.MapBlockEpoch(
		a.selfRef,
		func(epoch chainsource.BlockEpoch) Msg {
			return &blockEpochObservedMsg{
				height: epoch.Height,
			}
		},
	)

	_, err := a.cfg.ChainSource.Ask(
		ctx, &chainsource.SubscribeBlocksRequest{
			CallerID:    a.blockCallerID(),
			NotifyActor: fn.Some(notifyRef),
		},
	).Await(ctx).Unpack()
	if err != nil {
		return err
	}

	a.blockSubscriptionActive = true

	return nil
}

// registerConfWatch registers a confirmation watch for one tracked txid.
func (a *TxBroadcasterActor) registerConfWatch(ctx context.Context,
	entry *trackedTx) error {

	txid := entry.data.Txid
	notifyRef := chainsource.MapConfirmationEvent(
		a.selfRef,
		func(event chainsource.ConfirmationEvent) Msg {
			return &confirmationObservedMsg{
				txid:        event.Txid,
				blockHeight: event.BlockHeight,
				numConfs:    event.NumConfs,
			}
		},
	)

	_, err := a.cfg.ChainSource.Ask(
		ctx, &chainsource.RegisterConfRequest{
			CallerID: a.confCallerID(entry.data.Txid),
			Txid:     &txid,
			PkScript: append(
				[]byte(nil), entry.data.ConfirmationPkScript...,
			),
			TargetConfs: entry.data.TargetConfs,
			HeightHint:  entry.data.HeightHint,
			NotifyActor: fn.Some(notifyRef),
		},
	).Await(ctx).Unpack()
	if err != nil {
		return err
	}

	entry.confWatchRegistered = true

	return nil
}

// unregisterConfWatch unregisters the confirmation watch for one tracked
// txid.
//
// The unregister request must supply the same fields that were used at
// registration time — CallerID, Txid, PkScript, and TargetConfs — because
// chainsource derives the sub-actor's service key by hashing all four
// together. Omitting PkScript here (as an earlier revision of this file
// did) produces a different service key and silently leaks the conf
// sub-actor for every tracked txid.
func (a *TxBroadcasterActor) unregisterConfWatch(ctx context.Context,
	entry *trackedTx) error {

	txid := entry.data.Txid
	_, err := a.cfg.ChainSource.Ask(
		ctx, &chainsource.UnregisterConfRequest{
			CallerID: a.confCallerID(entry.data.Txid),
			Txid:     &txid,
			PkScript: append(
				[]byte(nil), entry.data.ConfirmationPkScript...,
			),
			TargetConfs: entry.data.TargetConfs,
		},
	).Await(ctx).Unpack()
	if err != nil {
		return fmt.Errorf("unregister conf %s: %w", entry.data.Txid,
			err)
	}

	entry.confWatchRegistered = false

	return nil
}

// broadcastTrackedTx submits one tracked transaction and records the latest
// broadcast metadata. The fee-bump intent is derived from the FSM state being
// entered: a FeeBumping pass builds a CPFP child, a Broadcasting pass is an
// initial (or retried) submission.
func (a *TxBroadcasterActor) broadcastTrackedTx(ctx context.Context,
	entry *trackedTx, nextState TxState) (*BroadcastResult, error) {

	return a.broadcastTrackedTxOpts(
		ctx, entry, nextState, nextState == TxStateFeeBumping,
	)
}

// broadcastTrackedTxOpts is broadcastTrackedTx with an explicit fee-bump
// intent, decoupled from the FSM state. A forced bump of a funded-anchor
// parent that never reached a mempool stays on the Broadcasting state track
// (its success path is "parent finally landed", not "replacement landed") but
// must still submit as a CPFP package, which is what isFeeBump controls.
func (a *TxBroadcasterActor) broadcastTrackedTxOpts(ctx context.Context,
	entry *trackedTx, nextState TxState, isFeeBump bool) (*BroadcastResult,
	error) {

	var startEvent trackedTxEvent
	switch nextState {
	case TxStateBroadcasting:
		startEvent = &trackedTxBroadcastStarted{}

	case TxStateFeeBumping:
		startEvent = &trackedTxFeeBumpStarted{}

	default:
		return nil, fmt.Errorf("unexpected broadcast state %v",
			nextState)
	}

	// The one-shot target rate applies to exactly one bump attempt
	// regardless of its outcome (including an early FSM-advance failure
	// below), so clear it on every exit path: a failed forced bump must not
	// silently pin the operator's rate onto later interval-paced bumps.
	targetFeeRate := int64(0)
	if isFeeBump {
		targetFeeRate = entry.pendingTargetFeeRate
		defer func() {
			entry.pendingTargetFeeRate = 0
		}()
	}

	if err := a.advanceTrackedTxFSM(ctx, entry, startEvent); err != nil {
		return nil, err
	}

	// A fee-bump pass is what builds a CPFP child for a funded-anchor
	// parent (the initial pass broadcasts the parent directly). Carry the
	// parent's own fee so the child only pays the package shortfall.
	result, err := a.broadcaster.Submit(
		ctx, a.bestHeight, &BroadcastRequest{
			Tx:                       entry.data.Tx,
			Label:                    entry.data.Label,
			IsFeeBump:                isFeeBump,
			ParentFee:                entry.data.ParentFee,
			TargetFeeRateSatPerVByte: targetFeeRate,
		},
	)
	if err != nil {
		return nil, err
	}

	if err := a.advanceTrackedTxFSM(
		ctx, entry, &trackedTxBroadcastAccepted{
			Progress: trackedTxProgress{
				LastBroadcastHeight: fn.Some(a.bestHeight),
				CurrentFeeRate:      result.FeeRate,
				ChildTxid:           copyHash(result.ChildTxid),
			},
		},
	); err != nil {
		return nil, err
	}

	return result, nil
}

// shouldFeeBump reports whether a tracked transaction that already reached a
// mempool is eligible for another fee-bump attempt at the current height.
func (a *TxBroadcasterActor) shouldFeeBump(entry *trackedTx) bool {
	return a.broadcastIntervalElapsed(entry, TxStateAwaitingConfirmation)
}

// shouldRetryBroadcast reports whether a tracked transaction that has not yet
// reached any mempool (still in Broadcasting after a total broadcast failure)
// is due for another initial-broadcast attempt at the current height.
func (a *TxBroadcasterActor) shouldRetryBroadcast(entry *trackedTx) bool {
	return a.broadcastIntervalElapsed(entry, TxStateBroadcasting)
}

// broadcastIntervalElapsed reports whether the tracked tx is in wantState and
// at least FeeBumpIntervalBlocks have elapsed since its last broadcast attempt.
func (a *TxBroadcasterActor) broadcastIntervalElapsed(entry *trackedTx,
	wantState TxState) bool {

	state, err := entry.currentTxState()
	if err != nil {
		return false
	}

	if state != wantState {
		return false
	}

	currentState, err := entry.currentFSMState()
	if err != nil {
		return false
	}

	// A tx with no recorded broadcast height has not been submitted yet, so
	// it is not due for a re-attempt. MapOptionZ yields the bool zero value
	// (false) for the None case, which is exactly that. Keying on the
	// Option presence rather than a zero height means a genuine attempt at
	// height zero (fresh chain / early sync) is still paced correctly
	// instead of being wedged forever.
	return fn.MapOptionZ(
		trackedTxLastBroadcastHeight(currentState),
		func(lastBroadcastHeight int32) bool {
			return a.bestHeight-lastBroadcastHeight >=
				a.cfg.FeeBumpIntervalBlocks
		},
	)
}

// failTrackedTx moves one tracked txid into terminal failure and notifies all
// current subscribers. The entry is retained if any terminal notification
// cannot be delivered, so a later actor tick can retry the failed delivery.
func (a *TxBroadcasterActor) failTrackedTx(ctx context.Context,
	entry *trackedTx, reason string) {

	if err := a.advanceTrackedTxFSM(ctx, entry, &trackedTxFailed{
		Reason: reason,
	}); err != nil {

		a.log.WarnS(ctx, "Failed to move tracked tx into terminal state",
			err, "txid", entry.data.Txid)
	}
	if a.notifyFailed(ctx, entry, reason) {
		a.evictTerminal(ctx, entry)
	}
}

// evictTerminal releases all resources held for one tracked tx that has
// reached a terminal state.
//
// Callers must have already moved the FSM into Confirmed/Failed and either
// delivered all terminal notifications or removed the remaining subscribers
// before calling evictTerminal.
//
// We unregister any still-held confirmation watch (the confirmation
// path already unregisters eagerly, but failure paths do not and the
// watch may still be outstanding), stop the per-tx FSM goroutine, and
// drop the entry from the tracking map. Without this step, a
// long-lived daemon accumulates one live FSM goroutine and one cached
// *wire.MsgTx per terminal txid — an O(total_txs_ever) leak even when
// the actor is otherwise idle.
//
// Once all terminal notifications have been delivered, a late
// EnsureConfirmedReq for the same txid will start fresh tracking rather than
// replaying a cached result. That fresh tracking re-registers a conf watch
// with chainsource; if the tx is already confirmed on-chain chainsource fires
// the confirmation notification immediately, so the late subscriber still
// receives TxConfirmed at the cost of one extra chainsource round trip per
// late ensure.
func (a *TxBroadcasterActor) evictTerminal(ctx context.Context,
	entry *trackedTx) {

	if entry.confWatchRegistered {
		if err := a.unregisterConfWatch(ctx, entry); err != nil {
			a.log.WarnS(ctx, "Failed to unregister confirmation "+
				"watch during terminal eviction",
				err, "txid", entry.data.Txid)
		}
	}

	if entry.fsm != nil {
		entry.fsm.Stop()
	}

	// Release the broadcaster's per-parent bump state (fee-bump history
	// used for BIP-125 Rule 3/4 enforcement) so it doesn't accumulate
	// alongside the actor's own leak fix. The broadcaster also drops
	// any wallet-level leases held on the parent's fee UTXOs so they
	// become immediately available to other subsystems.
	a.broadcaster.Evict(ctx, entry.data.Txid)
	a.driveFeeBump(ctx, &feeBumpParentEvicted{
		parentTxid: entry.data.Txid,
	})

	delete(a.tracked, entry.data.Txid)
}

// retryTerminalNotifications retries pending terminal notifications for a
// tracked transaction that reached a terminal FSM state but could not notify
// every subscriber on the first attempt.
func (a *TxBroadcasterActor) retryTerminalNotifications(ctx context.Context,
	entry *trackedTx) bool {

	state, err := entry.currentFSMState()
	if err != nil {
		a.log.WarnS(ctx, "Failed to read terminal tracked tx state",
			err, "txid", entry.data.Txid)

		return false
	}

	switch state := state.(type) {
	case *trackedTxStateConfirmed:
		confirmHeight, _ := trackedTxConfirmHeight(state)

		return a.notifyConfirmed(
			ctx, entry, confirmHeight, entry.data.TargetConfs,
		)

	case *trackedTxStateFailed:
		reason, _ := trackedTxFailureReason(state)

		return a.notifyFailed(ctx, entry, reason)

	default:
		return false
	}
}

// handleTerminalNotifyResult applies the result of a terminal subscriber
// notification that continued after txconfirm returned to its actor mailbox.
func (a *TxBroadcasterActor) handleTerminalNotifyResult(ctx context.Context,
	msg *terminalNotifyResultMsg) {

	delete(a.terminalNotifyInflight, msg.inflightKey)

	if msg.err != nil {
		a.log.WarnS(ctx, "Terminal notification failed after "+
			"actor-path timeout", msg.err, "txid", msg.txid,
			"subscriber_id", msg.subscriberID)

		return
	}

	entry, ok := a.tracked[msg.txid]
	if !ok {
		return
	}

	delete(entry.subscribers, msg.subscriberID)
	if len(entry.subscribers) != 0 {
		return
	}

	state, err := entry.currentTxState()
	if err != nil {
		a.log.WarnS(ctx, "Failed to read terminal tracked tx state",
			err, "txid", entry.data.Txid)

		return
	}

	if state == TxStateConfirmed || state == TxStateFailed {
		a.evictTerminal(ctx, entry)
	}
}

// notifyConfirmed fans a confirmation result out to all current subscribers.
// It returns true only after every subscriber accepted the terminal
// notification. Failed deliveries are left in the subscriber map so a later
// actor tick can retry instead of permanently losing the confirmation.
func (a *TxBroadcasterActor) notifyConfirmed(ctx context.Context,
	entry *trackedTx, blockHeight int32, numConfs uint32) bool {

	for id, subscriber := range entry.subscribers {
		ok := a.notifyOneConfirmed(
			ctx, subscriber, entry.data.Txid, blockHeight, numConfs,
		)
		if !ok {
			continue
		}

		delete(entry.subscribers, id)
	}

	return len(entry.subscribers) == 0
}

// notifyFailed fans a terminal failure result out to all current subscribers.
// It returns true only after every subscriber accepted the terminal
// notification. Failed deliveries are left in the subscriber map so a later
// actor tick can retry instead of permanently losing the failure.
func (a *TxBroadcasterActor) notifyFailed(ctx context.Context, entry *trackedTx,
	reason string) bool {

	for id, subscriber := range entry.subscribers {
		ok := a.notifyOneFailed(
			ctx, subscriber, entry.data.Txid, reason,
		)
		if !ok {
			continue
		}

		delete(entry.subscribers, id)
	}

	return len(entry.subscribers) == 0
}

// notifyOneConfirmed delivers one confirmation notification.
func (a *TxBroadcasterActor) notifyOneConfirmed(ctx context.Context,
	subscriber actor.TellOnlyRef[Notification], txid chainhash.Hash,
	blockHeight int32, numConfs uint32) bool {

	return a.notifyOneTerminal(
		ctx, subscriber, txid, "confirmed",
		func(notifyCtx context.Context) error {
			return subscriber.Tell(notifyCtx, &TxConfirmed{
				Txid:        txid,
				BlockHeight: blockHeight,
				NumConfs:    numConfs,
			})
		},
	)
}

// notifyOneFailed delivers one terminal failure notification.
func (a *TxBroadcasterActor) notifyOneFailed(ctx context.Context,
	subscriber actor.TellOnlyRef[Notification], txid chainhash.Hash,
	reason string) bool {

	return a.notifyOneTerminal(
		ctx, subscriber, txid, "failed",
		func(notifyCtx context.Context) error {
			return subscriber.Tell(notifyCtx, &TxFailed{
				Txid:   txid,
				Reason: reason,
			})
		},
	)
}

// notifyOneTerminal delivers one terminal notification without letting a slow
// durable subscriber block txconfirm's actor loop indefinitely.
func (a *TxBroadcasterActor) notifyOneTerminal(ctx context.Context,
	subscriber actor.TellOnlyRef[Notification], txid chainhash.Hash,
	kind string, deliver func(context.Context) error) bool {

	subscriberID := subscriber.ID()
	inflightKey := terminalNotifyKey(txid, subscriberID, kind)
	if _, ok := a.terminalNotifyInflight[inflightKey]; ok {
		return false
	}

	notifyCtx, cancel := terminalNotifyContext(ctx, inflightKey)
	errChan := make(chan error, 1)
	go func() {
		errChan <- deliver(notifyCtx)
	}()

	select {
	case err := <-errChan:
		cancel()
		if err != nil {
			a.log.WarnS(ctx, "Failed to deliver terminal tx "+
				"notification", err, "txid", txid,
				"subscriber_id", subscriberID,
				"notification_kind", kind)

			return false
		}

		return true

	case <-notifyCtx.Done():
		a.terminalNotifyInflight[inflightKey] = struct{}{}
		//nolint:contextcheck // async result outlives ctx
		a.completeTerminalNotifyAsync(
			inflightKey, txid, subscriberID, errChan, cancel,
		)

		a.log.DebugS(ctx, "Terminal tx notification deferred",
			"txid", txid,
			"subscriber_id", subscriberID,
			"notification_kind", kind,
		)

		return false
	}
}

// completeTerminalNotifyAsync reports a timed-out terminal delivery back to the
// txconfirm actor once the underlying Tell returns.
func (a *TxBroadcasterActor) completeTerminalNotifyAsync(inflightKey string,
	txid chainhash.Hash, subscriberID string, errChan <-chan error,
	cancel context.CancelFunc) {

	if a.selfRef == nil {
		cancel()

		return
	}

	go func() {
		err := <-errChan
		cancel()

		msg := &terminalNotifyResultMsg{
			txid:         txid,
			subscriberID: subscriberID,
			inflightKey:  inflightKey,
			err:          err,
		}
		bgCtx := context.Background()
		if sendErr := a.selfRef.Tell(bgCtx, msg); sendErr != nil {
			a.log.WarnS(bgCtx, "Failed to enqueue "+
				"terminal notification result", sendErr,
				"txid", txid, "subscriber_id", subscriberID)
		}
	}()
}

// terminalNotifyKey returns the stable idempotency key for one terminal
// subscriber notification.
func terminalNotifyKey(txid chainhash.Hash, subscriberID string,
	kind string) string {

	return fmt.Sprintf("txconfirm-terminal-%s-%s-%s", kind, txid,
		subscriberID)
}

// terminalNotifyContext isolates subscriber notification from txconfirm's actor
// transaction.
func terminalNotifyContext(ctx context.Context,
	dedupKey string) (context.Context, context.CancelFunc) {

	// Terminal delivery crosses from txconfirm into an arbitrary
	// subscriber actor. The tracked-tx FSM has already committed its
	// terminal state, and failed delivery is retried from a later actor
	// tick, so borrowing txconfirm's actor transaction cannot make the two
	// actors atomic. It can only hand the subscriber a tx handle that is
	// invalid outside this handler, or force two actor mailboxes through
	// the same SQLite writer and deadlock under block-heavy itests.
	notifyCtx := actor.WithoutTx(context.WithoutCancel(ctx))

	// A timed-out delivery may still complete after txconfirm retries the
	// same subscriber. Durable mailboxes consume OutboxID as their inbox
	// message id, so a stable key keeps those late/duplicate deliveries
	// idempotent.
	notifyCtx = actor.WithoutOutboxID(notifyCtx)
	notifyCtx = actor.WithOutboxID(notifyCtx, dedupKey)

	return context.WithTimeout(notifyCtx, terminalNotifyTimeout)
}

// advanceTrackedTxFSM applies one event to the tracked-tx protofsm.
func (a *TxBroadcasterActor) advanceTrackedTxFSM(ctx context.Context,
	entry *trackedTx, event trackedTxEvent) error {

	if entry.fsm == nil {
		return fmt.Errorf("tracked tx fsm not initialized")
	}

	_, err := entry.fsm.AskEvent(ctx, event).Await(ctx).Unpack()

	return err
}

// confCallerID returns the deterministic chainsource caller ID for one txid
// confirmation watch.
func (a *TxBroadcasterActor) confCallerID(txid chainhash.Hash) string {
	return a.selfRef.ID() + "-conf-" + txid.String()
}

// blockCallerID returns the deterministic chainsource caller ID for the shared
// block subscription.
func (a *TxBroadcasterActor) blockCallerID() string {
	return a.selfRef.ID() + "-blocks"
}

// copyHash returns a heap-independent copy of an optional hash.
func copyHash(hash *chainhash.Hash) *chainhash.Hash {
	if hash == nil {
		return nil
	}

	hashCopy := *hash

	return &hashCopy
}
