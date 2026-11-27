package txconfirm

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// FeeSplitJournal stores the single signed fee split for this wallet. Writes
// must commit independently of the caller's RPC or actor transaction.
type FeeSplitJournal interface {
	// SaveWalletFeeSplit accepts identical bytes or an empty slot. A zero
	// row count means a different candidate owns the slot.
	SaveWalletFeeSplit(context.Context, []byte) (int64, error)

	// ClearWalletFeeSplit deletes only the exact candidate supplied.
	ClearWalletFeeSplit(context.Context, []byte) error
}

// encodeFeeSplit stores a height hint and the full signed transaction. On
// recovery wallet enumeration supplies confirmed outputs to the live parents.
func encodeFeeSplit(pending *pendingFeeInputFanout) ([]byte, error) {
	var buf bytes.Buffer
	if err := binary.Write(
		&buf, binary.BigEndian, pending.heightHint,
	); err != nil {
		return nil, err
	}
	if err := pending.tx.Serialize(&buf); err != nil {
		return nil, err
	}

	// One owner slot per signed output preserves the assignment even if
	// wallet synchronization delivers the split before its watch fires.
	owners := make([]chainhash.Hash, len(pending.tx.TxOut))
	for parent, outputs := range pending.assignments {
		for _, op := range outputs {
			owners[op.Index] = parent
		}
	}
	for _, parent := range owners {
		buf.Write(parent[:])
	}

	return buf.Bytes(), nil
}

// restoreFeeSplit returns a candidate with its original input exclusion and
// watch script. No output becomes eligible based on the journal alone.
func restoreFeeSplit(raw []byte) (*pendingFeeInputFanout, error) {
	reader := bytes.NewReader(raw)
	var height int32
	if err := binary.Read(reader, binary.BigEndian, &height); err != nil {
		return nil, err
	}

	var tx wire.MsgTx
	if err := tx.Deserialize(reader); err != nil {
		return nil, err
	}

	// The record carries one owner slot per output, including change.
	if reader.Len() != len(tx.TxOut)*chainhash.HashSize ||
		height < 0 || len(tx.TxOut) == 0 || len(tx.TxIn) == 0 ||
		len(tx.TxOut[0].PkScript) == 0 {
		return nil, fmt.Errorf("invalid stored fee split")
	}

	// Restore ownership by output index so identical amounts keep their
	// original parents when the transaction is replayed.
	assignments := make(map[chainhash.Hash][]wire.OutPoint)
	id := tx.TxHash()
	for index := range tx.TxOut {
		var owner chainhash.Hash
		if _, err := io.ReadFull(reader, owner[:]); err != nil {
			return nil, err
		}
		if owner != (chainhash.Hash{}) {
			op := wire.OutPoint{Hash: id, Index: uint32(index)}
			assignments[owner] = append(assignments[owner], op)
		}
	}

	return &pendingFeeInputFanout{
		assignments:  assignments,
		txid:         id,
		tx:           &tx,
		watchScript:  tx.TxOut[0].PkScript,
		heightHint:   height,
		journalEntry: raw,
		saved:        true,
	}, nil
}

// RestoreFeeSplit installs the durable candidate before registering the actor.
// The caller sends ResumeFeeSplit once chainsource can deliver callbacks.
func (a *TxBroadcasterActor) RestoreFeeSplit(raw []byte) error {
	pending, err := restoreFeeSplit(raw)
	if err != nil {
		return err
	}

	// Reserve the original inputs before new submissions reach the actor.
	a.feeBumpFSM, a.feeBumpEnv = newFeeBumpStateMachine(
		a.broadcaster, a.log, pending,
	)
	a.feeBumpEnv.journal = a.cfg.FeeSplitJournal
	a.broadcaster.fanoutInputs = pending.tx.TxIn

	// Restored assignments remain predictions until the watch confirms.
	for parent, ops := range pending.assignments {
		state := a.broadcaster.parentState(parent)
		for _, op := range ops {
			out := pending.tx.TxOut[op.Index]
			state.PredictedFeeInputs[op] = &FeeInput{
				Outpoint: op,
				Output:   cloneTxOut(out),
			}
		}
	}

	return nil
}

// ResumeFeeSplit reattaches the chain subscriptions for a restored candidate.
type ResumeFeeSplit struct {
	// BaseMessage supplies the actor envelope for the startup request.
	actor.BaseMessage
}

// MessageType identifies the startup recovery request.
func (*ResumeFeeSplit) MessageType() string {
	return "ResumeFeeSplit"
}

// txConfirmMsgSealed restricts the recovery request to this package.
func (*ResumeFeeSplit) txConfirmMsgSealed() {}

// resumeFeeSplit subscribes before rebroadcast. A later block retries a lost
// receipt or watch registration independently of the restored exit jobs.
func (a *TxBroadcasterActor) resumeFeeSplit(ctx context.Context) error {
	a.ensureFeeBumpStarted(ctx)
	if err := a.ensureBlockSubscription(ctx); err != nil {
		return err
	}
	pending := a.feeBumpPending()
	if pending == nil {
		return nil
	}

	if err := a.watchFeeSplit(ctx, pending); err != nil {
		a.log.WarnS(ctx, "Retrying fee split watch on next block", err)
	}

	return nil
}

// watchFeeSplit uses the original funding height to catch confirmation while
// the daemon was offline. An unsuccessful registration is retried on a block.
func (a *TxBroadcasterActor) watchFeeSplit(ctx context.Context,
	pending *pendingFeeInputFanout) error {

	var registrationErr error
	if !a.fanoutWatchActive {
		registrationErr = a.registerFanoutConfWatch(
			ctx, pending.txid, pending.watchScript,
			uint32(pending.heightHint),
		)
		a.fanoutWatchActive = registrationErr == nil
	}

	if a.cfg.FeeSplitInputScript == nil {
		return registrationErr
	}

	// Each input can invalidate the split independently. Keep successful
	// watches while retrying individual lookup or registration failures.
	if a.splitSpendWatches == nil {
		a.splitSpendWatches = make(map[wire.OutPoint][]byte)
	}
	for _, input := range pending.tx.TxIn {
		op := input.PreviousOutPoint
		if _, ok := a.splitSpendWatches[op]; ok {
			continue
		}

		script, err := a.cfg.FeeSplitInputScript(ctx, op)
		if err != nil {
			registrationErr = errors.Join(registrationErr, err)

			continue
		}

		a.splitSpendWatches[op] = script
		a.registerSplitSpend(ctx, pending, op, script)
	}

	return registrationErr
}

// splitSpendObserved carries a confirmed spender from a watch registered for
// a particular candidate. Messages for a replaced candidate are ignored.
type splitSpendObserved struct {
	// BaseMessage supplies the actor envelope.
	actor.BaseMessage

	candidate chainhash.Hash
	spender   chainhash.Hash
	height    int32
}

// MessageType identifies the spend callback to the actor runtime.
func (*splitSpendObserved) MessageType() string { return "splitSpendObserved" }

// txConfirmMsgSealed restricts the callback to the broadcaster.
func (*splitSpendObserved) txConfirmMsgSealed() {}

// splitWatchResult carries the registration receipt for one input. A failed
// registration becomes eligible for retry on the next block.
type splitWatchResult struct {
	// BaseMessage supplies the actor envelope.
	actor.BaseMessage

	candidate chainhash.Hash
	input     wire.OutPoint
	err       error
}

// MessageType identifies the registration receipt.
func (*splitWatchResult) MessageType() string { return "splitWatchResult" }

// txConfirmMsgSealed restricts the receipt to the broadcaster.
func (*splitWatchResult) txConfirmMsgSealed() {}

// splitWatchTimeout bounds the asynchronous chainsource registration.
const splitWatchTimeout = 30 * time.Second

// registerSplitSpend watches a candidate input starting at the original
// funding height so a spend mined during downtime is replayed.
func (a *TxBroadcasterActor) registerSplitSpend(ctx context.Context,
	pending *pendingFeeInputFanout, op wire.OutPoint, script []byte) {

	id := pending.txid
	ref := chainsource.MapSpendEvent(
		a.selfRef,
		func(event chainsource.SpendEvent) Msg {
			return &splitSpendObserved{
				candidate: id,
				spender:   event.SpendingTxid,
				height:    event.SpendingHeight,
			}
		},
	)

	var req chainsource.ChainSourceMsg = &chainsource.RegisterSpendRequest{
		CallerID:    a.fanoutConfCallerID(id),
		Outpoint:    &op,
		PkScript:    script,
		HeightHint:  uint32(pending.heightHint),
		NotifyActor: fn.Some(ref),
	}
	actor.AskThen(
		ctx, a.cfg.ChainSource, req, a.selfRef, splitWatchTimeout,
		func(result fn.Result[chainsource.ChainSourceResp]) Msg {
			return &splitWatchResult{
				candidate: id,
				input:     op,
				err:       result.Err(),
			}
		},
	)
}

// stopSplitSpendWatches releases the input subscriptions for the resolved
// split.
func (a *TxBroadcasterActor) stopSplitSpendWatches(ctx context.Context,
	id chainhash.Hash) {

	for op, script := range a.splitSpendWatches {
		req := &chainsource.UnregisterSpendRequest{
			CallerID: a.fanoutConfCallerID(id),
			Outpoint: &op,
			PkScript: script,
		}
		if err := a.cfg.ChainSource.Tell(ctx, req); err != nil {
			a.log.WarnS(ctx, "Unregister split input spend", err,
				"outpoint", op)
		}
	}
	clear(a.splitSpendWatches)
}

// saveFeeSplit commits a candidate before its first publish. Retrying the
// exact record also reconciles a committed write whose receipt was lost.
func (env *feeBumpEnvironment) saveFeeSplit(ctx context.Context,
	pending *pendingFeeInputFanout) error {

	if env.journal == nil || pending.saved {
		return nil
	}

	count, err := env.journal.SaveWalletFeeSplit(ctx, pending.journalEntry)
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("another signed split owns the wallet " +
			"journal")
	}

	pending.saved = true

	return nil
}

// clearFeeSplit forgets only the candidate that has resolved.
func (env *feeBumpEnvironment) clearFeeSplit(ctx context.Context,
	pending *pendingFeeInputFanout) error {

	if env.journal == nil {
		return nil
	}

	return env.journal.ClearWalletFeeSplit(ctx, pending.journalEntry)
}
