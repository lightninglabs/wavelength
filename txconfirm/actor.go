package txconfirm

import (
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

const (
	// DefaultFeeBumpIntervalBlocks is the default number of new
	// blocks to wait before retrying a still-unconfirmed transaction
	// with a fresh CPFP child.
	DefaultFeeBumpIntervalBlocks int32 = 2
)

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

	// MaxFeeRateSatPerVByte caps fee estimates used by the internal CPFP
	// broadcaster. Zero falls back to DefaultMaxFeeRateSatPerVByte.
	MaxFeeRateSatPerVByte int64
}

// TxBroadcasterActor is a generic shared actor that deduplicates confirmation
// requests by txid and ensures transactions confirm on-chain.
//
// The actor is intentionally not tied to unrolling. Any subsystem can reuse
// it by providing signed transactions, an optional wallet for anchor-backed
// CPFP, and a subscriber reference for terminal notifications.
type TxBroadcasterActor struct {
	cfg Config
	log btclog.Logger

	// selfRef receives mapped chainsource callbacks.
	selfRef actor.TellOnlyRef[Msg]

	// broadcaster handles direct broadcast and anchor-aware CPFP package
	// submission.
	broadcaster *CPFPBroadcaster

	// tracked maps txid to its shared confirmation state.
	tracked map[chainhash.Hash]*trackedTx

	// bestHeight is the last observed best block height.
	bestHeight int32

	// hasBestHeight reports whether bestHeight has been initialized.
	hasBestHeight bool

	// blockSubscriptionActive reports whether the shared block subscription
	// is active.
	blockSubscriptionActive bool
}

// trackedTx stores the actor-owned handle for one tracked txid.
type trackedTx struct {
	data trackedTxData
	fsm  *trackedTxStateMachine

	subscribers map[string]actor.TellOnlyRef[Notification]

	// confWatchRegistered reports whether a chainsource confirmation
	// watch is currently active for this txid. It is flipped true by
	// registerConfWatch on success and false by unregisterConfWatch on
	// success. Terminal cleanup uses it to avoid redundant unregister
	// round trips for entries whose watch was never registered (e.g.
	// entries that failed during block-subscription setup).
	confWatchRegistered bool
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

	return &TxBroadcasterActor{
		cfg: cfg,
		log: cfg.Log.UnwrapOr(btclog.Disabled),
		broadcaster: NewCPFPBroadcaster(BroadcasterConfig{
			ChainSource:           cfg.ChainSource,
			Wallet:                cfg.Wallet,
			Log:                   cfg.Log,
			MaxFeeRateSatPerVByte: cfg.MaxFeeRateSatPerVByte,
		}),
		tracked: make(map[chainhash.Hash]*trackedTx),
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

	default:
		return fn.Err[Resp](fmt.Errorf("unknown txconfirm message: %T",
			msg))
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
				firstErr = fmt.Errorf(
					"current tx state %s: %w",
					entry.data.Txid, err,
				)
			}

			continue
		}

		if state == TxStateConfirmed || state == TxStateFailed {
			if entry.fsm != nil {
				entry.fsm.Stop()
			}

			continue
		}

		if err := a.unregisterConfWatch(ctx, entry); err != nil &&
			firstErr == nil {

			firstErr = err
		}

		if entry.fsm != nil {
			entry.fsm.Stop()
		}
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
		a.failTrackedTx(ctx, entry, fmt.Sprintf(
			"subscribe blocks: %v", err,
		))

		return a.ensureResp(entry, true), nil
	}

	if err := a.registerConfWatch(ctx, entry); err != nil {
		a.failTrackedTx(ctx, entry, fmt.Sprintf(
			"register conf: %v", err,
		))

		return a.ensureResp(entry, true), nil
	}

	if err := a.broadcastTrackedTx(
		ctx, entry, TxStateBroadcasting,
	); err != nil {
		if errors.Is(err, ErrCPFPFeeInputUnavailable) {
			a.log.WarnS(ctx,
				"Initial anchor broadcast waiting for CPFP fee input",
				err, "txid", entry.data.Txid,
			)

			progress := trackedTxProgress{
				LastBroadcastHeight: a.bestHeight,
			}
			_ = a.advanceTrackedTxFSM(
				ctx, entry, &trackedTxBroadcastAccepted{
					Progress: progress,
				},
			)

			return a.ensureResp(entry, true), nil
		}

		a.failTrackedTx(ctx, entry, fmt.Sprintf(
			"broadcast: %v", err,
		))
	}

	return a.ensureResp(entry, true), nil
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
		return resp, nil
	}

	if err := a.unregisterConfWatch(ctx, entry); err != nil {
		a.log.WarnS(ctx, "Failed to unregister confirmation watch",
			err, "txid", entry.data.Txid)
	}

	if entry.fsm != nil {
		entry.fsm.Stop()
	}

	delete(a.tracked, entry.data.Txid)
	resp.StoppedTracking = true

	return resp, nil
}

// handleConfirmationObserved marks a tracked txid as confirmed and fans the
// result out to all subscribers.
func (a *TxBroadcasterActor) handleConfirmationObserved(ctx context.Context,
	msg *confirmationObservedMsg) {

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

	a.notifyConfirmed(ctx, entry, msg.blockHeight, msg.numConfs)
	a.evictTerminal(ctx, entry)
}

// handleBlockObserved records a new best height and fee-bumps any eligible
// pending transactions.
func (a *TxBroadcasterActor) handleBlockObserved(ctx context.Context,
	msg *blockEpochObservedMsg) {

	if !a.hasBestHeight || msg.height > a.bestHeight {
		a.bestHeight = msg.height
		a.hasBestHeight = true
	}

	for _, entry := range a.tracked {
		if !a.shouldFeeBump(entry) {
			continue
		}

		if err := a.broadcastTrackedTx(
			ctx, entry, TxStateFeeBumping,
		); err != nil {
			// Fee-bump failures are non-terminal. The original
			// broadcast is still live and the confirmation watch
			// remains active, so the tx may still confirm without
			// the bump. Recover the FSM back to
			// AwaitingConfirmation with an updated broadcast
			// height so the next bump waits the full interval.
			a.log.WarnS(ctx, "Fee bump failed, will retry",
				err, "txid", entry.data.Txid)

			progress := trackedTxProgress{
				LastBroadcastHeight: a.bestHeight,
			}
			_ = a.advanceTrackedTxFSM(
				ctx, entry, &trackedTxBroadcastAccepted{
					Progress: progress,
				},
			)
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
		a.notifyOneFailed(ctx, subscriber, entry.data.Txid,
			fmt.Sprintf("tracked tx state: %v", err))
		return &EnsureConfirmedResp{
			Txid:  entry.data.Txid,
			State: TxStateFailed,
		}
	}

	switch state := state.(type) {
	case *trackedTxStateConfirmed:
		confirmHeight, _ := trackedTxConfirmHeight(state)
		a.notifyOneConfirmed(ctx, subscriber, entry.data.Txid,
			confirmHeight, entry.data.TargetConfs)

	case *trackedTxStateFailed:
		reason, _ := trackedTxFailureReason(state)
		a.notifyOneFailed(ctx, subscriber, entry.data.Txid, reason)

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

	targetConfs := req.TargetConfs
	if targetConfs == 0 {
		targetConfs = 1
	}

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
	}
	fsm := newTrackedTxStateMachine(fsmLog, data)
	fsm.Start(ctx)

	return &trackedTx{
		data: data,
		fsm:  fsm,
		subscribers: map[string]actor.TellOnlyRef[Notification]{
			req.Subscriber.ID(): req.Subscriber,
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
		return fmt.Errorf("unregister conf %s: %w",
			entry.data.Txid, err)
	}

	entry.confWatchRegistered = false

	return nil
}

// broadcastTrackedTx submits one tracked transaction and records the latest
// broadcast metadata.
func (a *TxBroadcasterActor) broadcastTrackedTx(ctx context.Context,
	entry *trackedTx, nextState TxState) error {

	var startEvent trackedTxEvent
	switch nextState {
	case TxStateBroadcasting:
		startEvent = &trackedTxBroadcastStarted{}

	case TxStateFeeBumping:
		startEvent = &trackedTxFeeBumpStarted{}

	default:
		return fmt.Errorf("unexpected broadcast state %v", nextState)
	}

	if err := a.advanceTrackedTxFSM(ctx, entry, startEvent); err != nil {
		return err
	}

	result, err := a.broadcaster.Submit(
		ctx, a.bestHeight, &BroadcastRequest{
			Tx:    entry.data.Tx,
			Label: entry.data.Label,
		},
	)
	if err != nil {
		return err
	}

	if err := a.advanceTrackedTxFSM(
		ctx, entry, &trackedTxBroadcastAccepted{
			Progress: trackedTxProgress{
				LastBroadcastHeight: a.bestHeight,
				CurrentFeeRate:      result.FeeRate,
				ChildTxid:           copyHash(result.ChildTxid),
			},
		},
	); err != nil {
		return err
	}

	return nil
}

// shouldFeeBump reports whether a tracked transaction is eligible for another
// broadcast attempt at the current height.
func (a *TxBroadcasterActor) shouldFeeBump(entry *trackedTx) bool {
	state, err := entry.currentTxState()
	if err != nil {
		return false
	}

	if state != TxStateAwaitingConfirmation {
		return false
	}

	currentState, err := entry.currentFSMState()
	if err != nil {
		return false
	}

	lastBroadcastHeight := trackedTxLastBroadcastHeight(currentState)
	if lastBroadcastHeight == 0 {
		return false
	}

	return a.bestHeight-lastBroadcastHeight >=
		a.cfg.FeeBumpIntervalBlocks
}

// failTrackedTx moves one tracked txid into terminal failure, notifies all
// current subscribers, and evicts the entry from the tracking map so the
// actor does not retain per-tx FSM goroutines and cached tx bytes for the
// rest of its lifetime.
func (a *TxBroadcasterActor) failTrackedTx(ctx context.Context,
	entry *trackedTx, reason string) {

	if err := a.advanceTrackedTxFSM(ctx, entry, &trackedTxFailed{
		Reason: reason,
	}); err != nil {
		a.log.WarnS(ctx, "Failed to move tracked tx into terminal state",
			err, "txid", entry.data.Txid)
	}
	a.notifyFailed(ctx, entry, reason)
	a.evictTerminal(ctx, entry)
}

// evictTerminal releases all resources held for one tracked tx that has
// reached a terminal state.
//
// Callers must have already moved the FSM into Confirmed/Failed and
// delivered all terminal notifications before calling evictTerminal.
//
// We unregister any still-held confirmation watch (the confirmation
// path already unregisters eagerly, but failure paths do not and the
// watch may still be outstanding), stop the per-tx FSM goroutine, and
// drop the entry from the tracking map. Without this step, a
// long-lived daemon accumulates one live FSM goroutine and one cached
// *wire.MsgTx per terminal txid — an O(total_txs_ever) leak even when
// the actor is otherwise idle.
//
// Eviction is unconditional in terminal paths, which means a late
// EnsureConfirmedReq for the same txid will start fresh tracking
// rather than replaying a cached result. That fresh tracking
// re-registers a conf watch with chainsource; if the tx is already
// confirmed on-chain chainsource fires the confirmation notification
// immediately, so the late subscriber still receives TxConfirmed at
// the cost of one extra chainsource round trip per late ensure.
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

	delete(a.tracked, entry.data.Txid)
}

// notifyConfirmed fans a confirmation result out to all current subscribers.
func (a *TxBroadcasterActor) notifyConfirmed(ctx context.Context,
	entry *trackedTx, blockHeight int32, numConfs uint32) {

	for id, subscriber := range entry.subscribers {
		a.notifyOneConfirmed(
			ctx, subscriber, entry.data.Txid, blockHeight, numConfs,
		)
		delete(entry.subscribers, id)
	}
}

// notifyFailed fans a terminal failure result out to all current subscribers.
func (a *TxBroadcasterActor) notifyFailed(ctx context.Context,
	entry *trackedTx, reason string) {

	for id, subscriber := range entry.subscribers {
		a.notifyOneFailed(ctx, subscriber, entry.data.Txid, reason)
		delete(entry.subscribers, id)
	}
}

// notifyOneConfirmed delivers one confirmation notification.
func (a *TxBroadcasterActor) notifyOneConfirmed(ctx context.Context,
	subscriber actor.TellOnlyRef[Notification], txid chainhash.Hash,
	blockHeight int32, numConfs uint32) {

	if err := subscriber.Tell(ctx, &TxConfirmed{
		Txid:        txid,
		BlockHeight: blockHeight,
		NumConfs:    numConfs,
	}); err != nil {
		a.log.WarnS(ctx, "Failed to deliver tx confirmation",
			err, "txid", txid, "subscriber_id", subscriber.ID())
	}
}

// notifyOneFailed delivers one terminal failure notification.
func (a *TxBroadcasterActor) notifyOneFailed(ctx context.Context,
	subscriber actor.TellOnlyRef[Notification], txid chainhash.Hash,
	reason string) {

	if err := subscriber.Tell(ctx, &TxFailed{
		Txid:   txid,
		Reason: reason,
	}); err != nil {
		a.log.WarnS(ctx, "Failed to deliver tx failure",
			err, "txid", txid, "subscriber_id", subscriber.ID())
	}
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
