package txconfirm

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/walletcore"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
)

const (
	// feeInputFanoutTxVersion is the version used for the internal
	// fee-input fanout transaction.
	feeInputFanoutTxVersion int32 = 2

	// minFeeInputFanoutOutputs is the minimum number of fee-input outputs
	// a fanout transaction produces so a single confirmation tops up the
	// supply for several blocked parents at once.
	minFeeInputFanoutOutputs = 3
)

// feeBumpStateIdle is the FSM state in which no fanout transaction is in
// flight. A fresh demand set that cannot be served from current supply moves
// the FSM into the fanout-pending state by building and broadcasting a fanout.
type feeBumpStateIdle struct{}

// String returns a human-readable representation of the idle state.
func (s *feeBumpStateIdle) String() string {
	return "FeeBumpIdle"
}

// IsTerminal returns false because the idle state is the FSM's resting state,
// not a terminal one: the actor keeps a single long-lived FSM and cycles it
// through fanouts for the lifetime of the actor.
func (s *feeBumpStateIdle) IsTerminal() bool {
	return false
}

// feeBumpStateSealed marks feeBumpStateIdle as a fanout state.
func (s *feeBumpStateIdle) feeBumpStateSealed() {}

// idleTransition is the self-loop transition used by the idle state when no
// fanout was started (already covered, no free supply, or a stashed failure).
func (s *feeBumpStateIdle) idleTransition() *feeBumpStateTransition {
	return &feeBumpStateTransition{
		NextState: s,
	}
}

// stayIdle stashes an operational failure on the environment and returns the
// idle self-loop transition with a nil ProcessEvent error. The error is
// deliberately not returned from ProcessEvent: protofsm tears the whole FSM
// down on any transition error, and a transient fanout failure must leave the
// long-lived machine ready for the next demand. The actor reads the stashed
// error back via env.takeLastErr.
func (s *feeBumpStateIdle) stayIdle(env *feeBumpEnvironment, failure error) (
	*feeBumpStateTransition, error) {

	env.lastErr = failure

	return s.idleTransition(), nil
}

// ProcessEvent applies one event to the idle fanout state.
//
// The only event that does work here is a fresh demand observation: it runs the
// full supply decision (is a fanout actually needed?) and, if so, funds and
// broadcasts a fanout transaction, reserving its outputs as predicted fee
// inputs on the blocked parents. On success the FSM advances to fanout-pending
// and asks the actor to register a confirmation watch.
//
// Confirmation and eviction events arriving while idle are late or duplicate
// callbacks for a fanout we are no longer tracking; they self-loop rather than
// erroring so a redundant chainsource event can never tear down the long-lived
// FSM.
func (s *feeBumpStateIdle) ProcessEvent(ctx context.Context, event feeBumpEvent,
	env *feeBumpEnvironment) (*feeBumpStateTransition, error) {

	switch event := event.(type) {
	case *feeBumpDemandsObserved:
		return s.processDemands(ctx, event, env)

	// Nothing is in flight, so a confirmation or eviction is not ours to
	// act on. Self-loop idle.
	case *feeBumpFanoutConfirmedEvent, *feeBumpParentEvicted:
		return &feeBumpStateTransition{
			NextState: s,
		}, nil

	default:
		return nil, fmt.Errorf("unexpected event %T in %s", event, s)
	}
}

// processDemands runs the "no pending fanout" supply path: it filters the
// demand set down to parents that have no confirmed supply, checks whether the
// wallet can already serve them, and if not funds and broadcasts a fanout that
// mints right-sized fee inputs for the still-blocked parents.
//
// On a build or broadcast failure the predicted-output reservation is rolled
// back, the error is stashed on the environment (see env.lastErr), and the FSM
// stays idle. Staying idle (rather than returning the error from ProcessEvent,
// which would tear the long-lived FSM down) is correct because a transient
// inability to fan out must not wedge the machine; the next demand observation
// simply tries again.
func (s *feeBumpStateIdle) processDemands(ctx context.Context,
	event *feeBumpDemandsObserved, env *feeBumpEnvironment) (
	*feeBumpStateTransition, error) {

	b := env.broadcaster
	if b == nil || b.cfg.Wallet == nil || len(event.demands) == 0 {
		return s.idleTransition(), nil
	}

	demands := event.demands
	feeRate := event.feeRate
	height := event.height

	blocked := env.blockedFeeInputDemands(demands)
	if len(blocked) == 0 {
		return s.idleTransition(), nil
	}

	utxos, err := b.cfg.Wallet.ListUnspent(ctx, 1, 9999999)
	if err != nil {
		return s.stayIdle(
			env, fmt.Errorf("list fanout supply: %w", err),
		)
	}

	available := fanoutAvailableFeeInputs(utxos, env.reservedOutpoints())
	blocked = demandsStillMissingSupply(blocked, available)
	if len(blocked) == 0 {
		return s.idleTransition(), nil
	}

	// Some demands still have no confirmed supply, so a fanout is the only
	// way to unblock them. Record the demand pressure that triggered it
	// before committing wallet funds.
	env.log.DebugS(ctx, "Fee-input fanout needed",
		slog.Int("blocked_demands", len(blocked)),
		slog.Int64("fee_rate_sat_per_vbyte", feeRate),
		slog.Int("height", int(height)),
	)

	fanoutDemands := blocked
	fundedTx, err := env.buildFeeInputFanoutTx(
		ctx, blocked, len(blocked), feeRate,
	)
	if err != nil {
		reservedInputs := env.reservedFeeInputsForFanout(demands)
		if len(reservedInputs) == 0 {
			return s.stayIdle(env, err)
		}

		fundedTx, err = env.buildReservedInputFanoutTx(
			ctx, demands, reservedInputs,
			max(
				len(demands), minFeeInputFanoutOutputs,
			),
			feeRate,
		)
		if err != nil {
			return s.stayIdle(env, err)
		}

		// The wallet had no free confirmed supply, so we fell back to
		// re-spending inputs already reserved by these parents. This is
		// the replacement path, worth a trace as it changes which UTXOs
		// the parents depend on.
		env.log.DebugS(ctx, "Fee-input fanout using reserved-input "+
			"fallback",
			slog.Int("reserved_inputs", len(reservedInputs)),
			slog.Int("demands", len(demands)),
		)

		fanoutDemands = demands
	}

	txid := fundedTx.TxHash()
	assignments, err := env.reservePredictedFanoutOutputs(
		txid, fundedTx, fanoutDemands,
	)
	if err != nil {
		return s.stayIdle(env, err)
	}

	for _, txIn := range fundedTx.TxIn {
		_, err := b.cfg.Wallet.LeaseOutput(
			ctx, txconfirmLockID, txIn.PreviousOutPoint,
			DefaultFeeInputLeaseExpiry,
		)
		if err != nil {
			env.log.WarnS(ctx, "Fanout input lease failed; relying "+
				"on wallet funding lock",
				err, "outpoint", txIn.PreviousOutPoint)
		}
	}

	watchScript := fanoutWatchScript(fundedTx, blocked)
	if len(watchScript) == 0 {
		env.releasePredictedFanoutOutputs(assignments)

		return s.stayIdle(
			env, fmt.Errorf("fanout watch script not found"),
		)
	}

	resp, err := b.cfg.ChainSource.Ask(
		ctx, &chainsource.BroadcastTxRequest{
			Tx:    fundedTx,
			Label: "txconfirm-fee-input-fanout",
		},
	).Await(ctx).Unpack()
	if err != nil {
		env.releasePredictedFanoutOutputs(assignments)

		return s.stayIdle(env, fmt.Errorf("broadcast fanout: %w", err))
	}

	broadcastResp, ok := resp.(*chainsource.BroadcastTxResponse)
	if !ok {
		env.releasePredictedFanoutOutputs(assignments)

		return s.stayIdle(
			env, fmt.Errorf("unexpected fanout response %T", resp),
		)
	}

	pending := &pendingFeeInputFanout{
		txid:                broadcastResp.Txid,
		tx:                  fundedTx.Copy(),
		watchScript:         watchScript,
		assignments:         assignments,
		lastBroadcastHeight: height,
	}

	// The fanout is now on the wire. Log it at info as a lifecycle event:
	// this is the moment several blocked parents gain a supply path.
	env.log.InfoS(ctx, "Fee-input fanout broadcast",
		slog.String("txid", broadcastResp.Txid.String()),
		slog.Int("num_outputs", len(fundedTx.TxOut)),
		slog.Int("num_demands", len(fanoutDemands)),
	)

	env.consumeFanoutInputs(fundedTx.TxIn)

	// Advance to fanout-pending and ask the actor to register a
	// confirmation watch on the fanout's chosen output script. The watch is
	// the only effect that must reach the actor, so it is the lone outbox
	// event.
	return &feeBumpStateTransition{
		NextState: &feeBumpStateFanoutPending{
			pending: pending,
		},
		NewEvents: fn.Some(feeBumpEmittedEvent{
			Outbox: []feeBumpOutboxEvent{
				&feeBumpWatchFanout{
					txid:        pending.txid,
					watchScript: pending.watchScript,
				},
			},
		}),
	}, nil
}

// feeBumpStateFanoutPending is the FSM state in which a fanout transaction has
// been broadcast and is awaiting confirmation. It carries the full pending
// fanout state: txid, the funded tx, the watch script, the per-parent output
// assignments, and the last broadcast height.
type feeBumpStateFanoutPending struct {
	// pending is the in-flight fanout awaiting confirmation.
	pending *pendingFeeInputFanout
}

// String returns a human-readable representation of the fanout-pending state.
func (s *feeBumpStateFanoutPending) String() string {
	return "FeeBumpFanoutPending"
}

// IsTerminal returns false because a pending fanout is mid-lifecycle.
func (s *feeBumpStateFanoutPending) IsTerminal() bool {
	return false
}

// feeBumpStateSealed marks feeBumpStateFanoutPending as a fanout state.
func (s *feeBumpStateFanoutPending) feeBumpStateSealed() {}

// ProcessEvent applies one event to the fanout-pending state.
//
//   - A fresh demand observation keeps the in-flight fanout alive: if the
//     fee-bump interval has elapsed it rebroadcasts the same transaction and
//     refreshes the height. A hard-rejected rebroadcast releases the fanout and
//     returns to idle so the next observation can rebuild from scratch.
//
//   - A confirmation for our txid promotes the predicted outputs into used fee
//     inputs and returns to idle, asking the actor to tear down the watch and
//     retry the parents that were waiting on supply.
//
//   - A parent eviction drops that parent's assignments; when none remain the
//     fanout is released and the FSM returns to idle.
func (s *feeBumpStateFanoutPending) ProcessEvent(ctx context.Context,
	event feeBumpEvent, env *feeBumpEnvironment) (*feeBumpStateTransition,
	error) {

	switch event := event.(type) {
	case *feeBumpDemandsObserved:
		return s.processDemands(ctx, event, env)

	case *feeBumpFanoutConfirmedEvent:
		return s.processConfirmed(ctx, event, env)

	case *feeBumpParentEvicted:
		return s.processParentEvicted(ctx, event, env)

	default:
		return nil, fmt.Errorf("unexpected event %T in %s", event, s)
	}
}

// processDemands keeps the in-flight fanout fresh. A fanout is already on the
// wire serving these parents, so a fresh observation does not rebuild — it only
// rebroadcasts the existing fanout once the fee-bump interval has elapsed and
// refreshes the recorded height. A rebroadcast that is hard-rejected (not an
// ignorable "already known") releases the fanout and returns to idle so the
// next observation rebuilds from scratch.
func (s *feeBumpStateFanoutPending) processDemands(ctx context.Context,
	event *feeBumpDemandsObserved, env *feeBumpEnvironment) (
	*feeBumpStateTransition, error) {

	pending := s.pending
	height := event.height
	retryInterval := event.retryInterval
	if retryInterval <= 0 {
		retryInterval = DefaultFeeBumpIntervalBlocks
	}

	// Not yet due for a rebroadcast: keep the current fanout as-is.
	if pending.tx == nil || height-pending.lastBroadcastHeight <
		retryInterval {
		return &feeBumpStateTransition{
			NextState: s,
		}, nil
	}

	resp, err := env.broadcaster.cfg.ChainSource.Ask(
		ctx, &chainsource.BroadcastTxRequest{
			Tx:    pending.tx.Copy(),
			Label: "txconfirm-fee-input-fanout",
		},
	).Await(ctx).Unpack()
	if err != nil {
		// An ignorable "already known" rejection means the fanout is
		// still in the mempool; just refresh the height.
		if IsIgnorableBroadcastError(err) {
			return env.refreshRebroadcast(ctx, pending, height), nil
		}

		// A hard rejection means the fanout is no longer valid; release
		// it and return to idle, then immediately rebuild from scratch
		// within this same turn by routing the demand event back into
		// the (now idle) FSM as an internal event. The watch teardown
		// for the abandoned fanout is left to the actor via the unwatch
		// outbox.
		env.log.WarnS(ctx, "Pending fee-input fanout rejected; "+
			"rebuilding", err, "txid", pending.txid)

		return env.clearAndRebuild(ctx, pending, event), nil
	}

	broadcastResp, ok := resp.(*chainsource.BroadcastTxResponse)
	if !ok {
		env.lastErr = fmt.Errorf("unexpected fanout response %T", resp)

		return env.clearPendingFanout(ctx, pending), nil
	}

	if broadcastResp.Txid != pending.txid {
		env.lastErr = fmt.Errorf("fanout rebroadcast txid changed")

		return env.clearPendingFanout(ctx, pending), nil
	}

	return env.refreshRebroadcast(ctx, pending, height), nil
}

// processConfirmed promotes a confirmed fanout's predicted outputs into used
// fee inputs on each assigned parent and returns to idle. A confirmation for
// some other txid is a self-loop. On a match the actor is asked to tear down
// the watch and retry the parents that were stuck waiting on supply.
func (s *feeBumpStateFanoutPending) processConfirmed(ctx context.Context,
	event *feeBumpFanoutConfirmedEvent, env *feeBumpEnvironment) (
	*feeBumpStateTransition, error) {

	pending := s.pending
	if pending.txid != event.txid {
		return &feeBumpStateTransition{
			NextState: s,
		}, nil
	}

	env.promoteConfirmedFanout(ctx, pending)

	// Returning to idle: tell the actor to tear down the confirmation watch
	// and retry every parent that was stuck waiting on the now-available
	// supply.
	return &feeBumpStateTransition{
		NextState: &feeBumpStateIdle{},
		NewEvents: fn.Some(feeBumpEmittedEvent{
			Outbox: []feeBumpOutboxEvent{
				&feeBumpUnwatchFanout{
					txid:        pending.txid,
					watchScript: pending.watchScript,
				},
				&feeBumpRetryParents{},
			},
		}),
	}, nil
}

// processParentEvicted drops the evicted parent from the in-flight fanout's
// assignments. When no parents remain, the whole fanout is released (predicted
// outputs + wallet leases) and the FSM returns to idle, asking the actor to
// tear down the watch.
func (s *feeBumpStateFanoutPending) processParentEvicted(ctx context.Context,
	event *feeBumpParentEvicted, env *feeBumpEnvironment) (
	*feeBumpStateTransition, error) {

	pending := s.pending
	if _, ok := pending.assignments[event.parentTxid]; !ok {

		// The evicted parent was not served by this fanout; nothing to
		// do.
		return &feeBumpStateTransition{
			NextState: s,
		}, nil
	}

	// Build a fresh pending value (transitions do not mutate the carried
	// state in place) with the evicted parent dropped.
	next := *pending
	next.assignments = make(map[chainhash.Hash][]wire.OutPoint)
	for parent, outpoints := range pending.assignments {
		if parent == event.parentTxid {
			continue
		}

		next.assignments[parent] = outpoints
	}

	if len(next.assignments) > 0 {
		return &feeBumpStateTransition{
			NextState: &feeBumpStateFanoutPending{
				pending: &next,
			},
		}, nil
	}

	// No parents remain, so the fanout serves nobody: release it and return
	// to idle. clearPendingFanout releases the predicted outputs and leases
	// and emits the unwatch outbox for the actor.
	return env.clearPendingFanout(ctx, pending), nil
}

// refreshRebroadcast builds the fanout-pending transition that refreshes the
// in-flight fanout's last-broadcast height after a successful rebroadcast.
// Transitions are pure with respect to the carried state, so a fresh pending
// value is built rather than mutating the one in the current state.
func (env *feeBumpEnvironment) refreshRebroadcast(ctx context.Context,
	pending *pendingFeeInputFanout, height int32) *feeBumpStateTransition {

	// A rebroadcast keeps an in-flight fanout fresh across the fee-bump
	// interval; trace it so the cadence is visible without being noisy.
	env.log.DebugS(ctx, "Rebroadcasting pending fee-input fanout",
		slog.String("txid", pending.txid.String()),
		slog.Int("height", int(height)),
	)

	next := *pending
	next.lastBroadcastHeight = height

	return &feeBumpStateTransition{
		NextState: &feeBumpStateFanoutPending{
			pending: &next,
		},
	}
}

// releasePendingFanout releases an abandoned fanout's predicted outputs and the
// wallet leases on its inputs. It is the shared cleanup used by every path that
// drops a pending fanout.
func (env *feeBumpEnvironment) releasePendingFanout(ctx context.Context,
	pending *pendingFeeInputFanout) {

	// Abandoning the fanout releases its predicted outputs and leases; log
	// it so a cleared fanout can be correlated with the rebuild that
	// follows.
	env.log.DebugS(ctx, "Clearing pending fee-input fanout",
		slog.String("txid", pending.txid.String()),
	)

	env.releasePredictedFanoutOutputs(pending.assignments)
	if pending.tx != nil {
		for _, txIn := range pending.tx.TxIn {
			env.broadcaster.releaseWalletLease(
				ctx, txIn.PreviousOutPoint,
			)
		}
	}
}

// clearPendingFanout abandons the in-flight fanout and returns the idle
// transition with an unwatch outbox so the actor tears down the confirmation
// watch.
func (env *feeBumpEnvironment) clearPendingFanout(ctx context.Context,
	pending *pendingFeeInputFanout) *feeBumpStateTransition {

	env.releasePendingFanout(ctx, pending)

	return &feeBumpStateTransition{
		NextState: &feeBumpStateIdle{},
		NewEvents: fn.Some(feeBumpEmittedEvent{
			Outbox: []feeBumpOutboxEvent{
				&feeBumpUnwatchFanout{
					txid:        pending.txid,
					watchScript: pending.watchScript,
				},
			},
		}),
	}
}

// clearAndRebuild abandons the in-flight fanout (as clearPendingFanout does)
// and additionally routes the triggering demand event back into the now-idle
// FSM as an internal event, so a fresh fanout is rebuilt from scratch within
// the same turn. This preserves the single-call "reject -> rebuild" behavior
// the actor relies on after a hard-rejected rebroadcast.
func (env *feeBumpEnvironment) clearAndRebuild(ctx context.Context,
	pending *pendingFeeInputFanout,
	rebuild *feeBumpDemandsObserved) *feeBumpStateTransition {

	env.releasePendingFanout(ctx, pending)

	return &feeBumpStateTransition{
		NextState: &feeBumpStateIdle{},
		NewEvents: fn.Some(feeBumpEmittedEvent{
			InternalEvent: []feeBumpEvent{rebuild},
			Outbox: []feeBumpOutboxEvent{
				&feeBumpUnwatchFanout{
					txid:        pending.txid,
					watchScript: pending.watchScript,
				},
			},
		}),
	}
}

// promoteConfirmedFanout promotes a confirmed fanout's predicted outputs into
// used fee inputs on each assigned parent. Evicted parents (no state in the
// map) are skipped.
func (env *feeBumpEnvironment) promoteConfirmedFanout(ctx context.Context,
	pending *pendingFeeInputFanout) {

	// The fanout confirmed, so its predicted outputs become spendable
	// confirmed fee inputs for the assigned parents. This is the event that
	// unblocks the CPFP children, so it logs at info.
	env.log.InfoS(ctx, "Fee-input fanout confirmed; promoting predicted "+
		"inputs",
		slog.String("txid", pending.txid.String()),
		slog.Int("num_parents", len(pending.assignments)),
	)

	for parent, outpoints := range pending.assignments {
		state := env.broadcaster.parentStates[parent]
		if state == nil {
			continue
		}

		for _, op := range outpoints {
			feeInput := state.PredictedFeeInputs[op]
			if feeInput == nil {
				continue
			}

			feeInput = cloneFeeInput(feeInput)
			feeInput.Confirmed = true
			state.UsedFeeOutpoints[op] = struct{}{}
			state.UsedFeeInputs[op] = feeInput
			delete(state.PredictedFeeInputs, op)
		}
	}
}

// fanoutWatchScript returns the output script the actor should register a
// confirmation watch on: the first fanout output whose value matches one of the
// blocked demands.
func fanoutWatchScript(fundedTx *wire.MsgTx, blocked []feeInputDemand) []byte {
	for _, txOut := range fundedTx.TxOut {
		for _, demand := range blocked {
			if txOut.Value == int64(demand.minAmount) {
				return append([]byte(nil), txOut.PkScript...)
			}
		}
	}

	return nil
}

// blockedFeeInputDemands filters the demand set down to parents that cannot be
// served from their own already-reserved confirmed fee inputs, sorted by txid
// for deterministic fanout output assignment.
func (env *feeBumpEnvironment) blockedFeeInputDemands(
	demands []feeInputDemand) []feeInputDemand {

	var blocked []feeInputDemand
	for _, demand := range demands {
		if demand.minAmount <= 0 {
			continue
		}

		if env.broadcaster.selectReservedFeeInput(
			demand.parentTxid, demand.minAmount,
		) != nil {

			continue
		}

		blocked = append(blocked, demand)
	}

	sort.Slice(blocked, func(i, j int) bool {
		return blocked[i].parentTxid.String() <
			blocked[j].parentTxid.String()
	})

	return blocked
}

// reservedOutpoints returns the set of wallet outpoints already committed to
// any parent, whether as used or predicted fee inputs, so the fanout does not
// double-spend supply another parent is relying on.
func (env *feeBumpEnvironment) reservedOutpoints() map[wire.OutPoint]struct{} {
	reserved := make(map[wire.OutPoint]struct{})
	for _, state := range env.broadcaster.parentStates {
		for op := range state.UsedFeeOutpoints {
			reserved[op] = struct{}{}
		}
		for op := range state.PredictedFeeInputs {
			reserved[op] = struct{}{}
		}
	}

	return reserved
}

// fanoutAvailableFeeInputs returns the confirmed wallet UTXO amounts that are
// not already reserved by any parent, sorted ascending.
func fanoutAvailableFeeInputs(utxos []*walletcore.Utxo,
	reserved map[wire.OutPoint]struct{}) []btcutil.Amount {

	available := make([]btcutil.Amount, 0, len(utxos))
	for _, utxo := range utxos {
		if utxo == nil {
			continue
		}

		if _, ok := reserved[utxo.Outpoint]; ok {
			continue
		}

		available = append(available, utxo.Amount)
	}

	sort.Slice(available, func(i, j int) bool {
		return available[i] < available[j]
	})

	return available
}

// demandsStillMissingSupply returns the demands that cannot be matched
// one-to-one against the available wallet amounts, using a smallest-first
// greedy assignment so each demand consumes the tightest fitting amount.
func demandsStillMissingSupply(demands []feeInputDemand,
	available []btcutil.Amount) []feeInputDemand {

	demands = append([]feeInputDemand(nil), demands...)
	sort.Slice(demands, func(i, j int) bool {
		return demands[i].minAmount < demands[j].minAmount
	})

	used := make([]bool, len(available))
	var missing []feeInputDemand
	for _, demand := range demands {
		found := false
		for idx, amount := range available {
			if used[idx] || amount < demand.minAmount {
				continue
			}

			used[idx] = true
			found = true

			break
		}

		if !found {
			missing = append(missing, demand)
		}
	}

	return missing
}

// buildFeeInputFanoutTx funds a fanout transaction whose outputs cover the
// supplied demands (padded to minOutputs) from confirmed wallet supply via
// FundPsbt.
func (env *feeBumpEnvironment) buildFeeInputFanoutTx(ctx context.Context,
	demands []feeInputDemand, minOutputs int, feeRate int64) (*wire.MsgTx,
	error) {

	outputs, err := env.fanoutOutputs(ctx, demands, minOutputs)
	if err != nil {
		return nil, err
	}

	fanoutTx := wire.NewMsgTx(feeInputFanoutTxVersion)
	for _, output := range outputs {
		fanoutTx.AddTxOut(output)
	}

	packet, err := psbt.NewFromUnsignedTx(fanoutTx)
	if err != nil {
		return nil, fmt.Errorf("create fanout PSBT: %w", err)
	}

	var buf bytes.Buffer
	if err := packet.Serialize(&buf); err != nil {
		return nil, fmt.Errorf("serialize fanout PSBT: %w", err)
	}

	fundedTx, err := env.broadcaster.cfg.Wallet.FundPsbt(
		ctx, buf.Bytes(), feeRate, txconfirmLockID,
		DefaultFeeInputLeaseExpiry,
	)
	if err != nil {
		return nil, fmt.Errorf("fund fanout PSBT: %w", err)
	}

	if err := verifyFanoutOutputs(outputs, fundedTx); err != nil {
		return nil, err
	}

	return fundedTx, nil
}

// fanoutOutputs builds the fanout output set: one output sized to each
// demand's minAmount, padded out to minOutputs with extra outputs sized to the
// largest demand so a single confirmation refills several blocked parents.
func (env *feeBumpEnvironment) fanoutOutputs(ctx context.Context,
	demands []feeInputDemand, minOutputs int) ([]*wire.TxOut, error) {

	outputCount := max(len(demands), minOutputs)
	outputs := make([]*wire.TxOut, 0, outputCount)
	var extraAmount btcutil.Amount
	for _, demand := range demands {
		if demand.minAmount > extraAmount {
			extraAmount = demand.minAmount
		}

		pkScript, err := env.broadcaster.deriveChangePkScript(ctx)
		if err != nil {
			return nil, fmt.Errorf("fanout pkscript: %w", err)
		}

		outputs = append(outputs, &wire.TxOut{
			Value:    int64(demand.minAmount),
			PkScript: pkScript,
		})
	}
	for len(outputs) < outputCount {
		pkScript, err := env.broadcaster.deriveChangePkScript(ctx)
		if err != nil {
			return nil, fmt.Errorf("fanout pkscript: %w", err)
		}

		outputs = append(outputs, &wire.TxOut{
			Value:    int64(extraAmount),
			PkScript: pkScript,
		})
	}

	return outputs, nil
}

// reservedFeeInputsForFanout collects the confirmed fee inputs already
// reserved by the supplied parents, to fund a fanout when the wallet has no
// free confirmed supply.
//
// This fallback intentionally replaces a parent's prior CPFP child when the
// only usable wallet value is already reserved in this ledger. The fanout
// immediately consumes that reservation and replaces it with predicted
// outputs assigned through the same parentStates map, so the cross-parent
// ownership boundary stays inside txconfirm.
func (env *feeBumpEnvironment) reservedFeeInputsForFanout(
	demands []feeInputDemand) []*FeeInput {

	var inputs []*FeeInput
	seen := make(map[wire.OutPoint]struct{})
	for _, demand := range demands {
		state := env.broadcaster.parentStates[demand.parentTxid]
		if state == nil {
			continue
		}

		for op, input := range state.UsedFeeInputs {
			if input == nil || input.Output == nil ||
				!input.Confirmed {

				continue
			}
			if _, ok := seen[op]; ok {
				continue
			}

			inputs = append(inputs, cloneFeeInput(input))
			seen[op] = struct{}{}
		}
	}

	sort.Slice(inputs, func(i, j int) bool {
		return inputs[i].Outpoint.String() < inputs[j].Outpoint.String()
	})

	return inputs
}

// buildReservedInputFanoutTx funds a fanout transaction from a set of already
// reserved confirmed fee inputs (the no-free-supply fallback), sizing the fee
// to at least the sum of the demands so the replacement clears relay policy.
func (env *feeBumpEnvironment) buildReservedInputFanoutTx(ctx context.Context,
	demands []feeInputDemand, inputs []*FeeInput, minOutputs int,
	feeRate int64) (*wire.MsgTx, error) {

	outputs, err := env.fanoutOutputs(ctx, demands, minOutputs)
	if err != nil {
		return nil, err
	}

	changePkScript, err := env.broadcaster.deriveChangePkScript(ctx)
	if err != nil {
		return nil, fmt.Errorf("fanout change pkscript: %w", err)
	}

	inputAmount := btcutil.Amount(0)
	walletInputs := make([]*walletcore.Utxo, 0, len(inputs))
	outpoints := make([]*wire.OutPoint, 0, len(inputs))
	sequences := make([]uint32, 0, len(inputs))
	for _, input := range inputs {
		inputAmount += btcutil.Amount(input.Output.Value)
		walletInputs = append(walletInputs, &walletcore.Utxo{
			Outpoint: input.Outpoint,
			Amount:   btcutil.Amount(input.Output.Value),
			PkScript: append([]byte(nil), input.Output.PkScript...),
		})

		op := input.Outpoint
		outpoints = append(outpoints, &op)
		sequences = append(sequences, wire.MaxTxInSequenceNum-2)
	}

	outputAmount := btcutil.Amount(0)
	for _, output := range outputs {
		outputAmount += btcutil.Amount(output.Value)
	}

	fee := btcutil.Amount(feeRate) * btcutil.Amount(
		estimateFanoutVSize(
			walletInputs, len(outputs), changePkScript,
		),
	)
	minReplacementFee := btcutil.Amount(0)
	for _, demand := range demands {
		minReplacementFee += demand.minAmount
	}
	if fee < minReplacementFee {
		fee = minReplacementFee
	}

	change := inputAmount - outputAmount - fee
	if change < 0 {
		return nil, fmt.Errorf("reserved fanout input amount %d "+
			"insufficient for outputs %d and fee %d", inputAmount,
			outputAmount, fee)
	}
	if change > DustLimit {
		outputs = append(outputs, &wire.TxOut{
			Value:    int64(change),
			PkScript: changePkScript,
		})
	}

	packet, err := psbt.New(
		outpoints, outputs, feeInputFanoutTxVersion, 0, sequences,
	)
	if err != nil {
		return nil, fmt.Errorf("create reserved fanout PSBT: %w", err)
	}
	for idx, input := range inputs {
		packet.Inputs[idx].WitnessUtxo = cloneTxOut(input.Output)
	}

	var buf bytes.Buffer
	if err := packet.Serialize(&buf); err != nil {
		return nil, fmt.Errorf("serialize reserved fanout PSBT: %w",
			err)
	}

	fundedTx, err := env.broadcaster.cfg.Wallet.FinalizePsbt(
		ctx, buf.Bytes(),
	)
	if err != nil {
		return nil, fmt.Errorf("finalize reserved fanout PSBT: %w", err)
	}
	if err := verifyFanoutOutputs(outputs, fundedTx); err != nil {
		return nil, err
	}

	return fundedTx, nil
}

// consumeFanoutInputs drops the fanout's own spent inputs from every parent's
// reservation maps: those UTXOs are now committed to the fanout transaction,
// not to any CPFP child.
func (env *feeBumpEnvironment) consumeFanoutInputs(inputs []*wire.TxIn) {
	for _, txIn := range inputs {
		op := txIn.PreviousOutPoint
		for _, state := range env.broadcaster.parentStates {
			delete(state.UsedFeeOutpoints, op)
			delete(state.UsedFeeInputs, op)
		}
	}
}

// reservePredictedFanoutOutputs assigns each demand a matching fanout output
// (by value) as a predicted fee input on that parent's state, returning the
// per-parent outpoint assignments. A demand with no matching output rolls the
// whole reservation back.
func (env *feeBumpEnvironment) reservePredictedFanoutOutputs(
	txid chainhash.Hash, tx *wire.MsgTx, demands []feeInputDemand) (
	map[chainhash.Hash][]wire.OutPoint, error) {

	assignments := make(map[chainhash.Hash][]wire.OutPoint)
	nextOutput := 0
	for _, demand := range demands {
		found := false
		for nextOutput < len(tx.TxOut) {
			txOut := tx.TxOut[nextOutput]
			op := wire.OutPoint{
				Hash:  txid,
				Index: uint32(nextOutput),
			}
			nextOutput++

			if txOut.Value != int64(demand.minAmount) {
				continue
			}

			state := env.broadcaster.parentState(demand.parentTxid)
			state.PredictedFeeInputs[op] = &FeeInput{
				Outpoint: op,
				Output: &wire.TxOut{
					Value: txOut.Value,
					PkScript: append(
						[]byte(nil), txOut.PkScript...,
					),
				},
				Confirmed: false,
			}
			assignments[demand.parentTxid] = append(
				assignments[demand.parentTxid], op,
			)
			found = true

			break
		}

		if !found {
			env.releasePredictedFanoutOutputs(assignments)

			return nil, fmt.Errorf("fanout missing output for %s",
				demand.parentTxid)
		}
	}

	return assignments, nil
}

// releasePredictedFanoutOutputs removes the supplied predicted-output
// assignments from their parents' states, used to roll back a partial or
// abandoned fanout reservation.
func (env *feeBumpEnvironment) releasePredictedFanoutOutputs(
	assignments map[chainhash.Hash][]wire.OutPoint) {

	for parent, outpoints := range assignments {
		state := env.broadcaster.parentStates[parent]
		if state == nil {
			continue
		}

		for _, op := range outpoints {
			delete(state.PredictedFeeInputs, op)
		}
	}
}

// verifyFanoutOutputs confirms the wallet returned a funded transaction whose
// leading outputs match (value and script) the outputs we asked it to fund, so
// the predicted-output assignment stays valid.
func verifyFanoutOutputs(expected []*wire.TxOut, actual *wire.MsgTx) error {
	if actual == nil {
		return fmt.Errorf("fanout tx missing")
	}

	if len(actual.TxOut) < len(expected) {
		return fmt.Errorf("fanout output count changed")
	}

	for idx, want := range expected {
		got := actual.TxOut[idx]
		if got.Value != want.Value ||
			!bytes.Equal(got.PkScript, want.PkScript) {
			return fmt.Errorf("fanout output %d changed", idx)
		}
	}

	return nil
}

// cloneTxOut returns a deep copy of the supplied output.
func cloneTxOut(txOut *wire.TxOut) *wire.TxOut {
	if txOut == nil {
		return nil
	}

	return &wire.TxOut{
		Value:    txOut.Value,
		PkScript: append([]byte(nil), txOut.PkScript...),
	}
}

// estimateFanoutVSize estimates the vbyte size of a reserved-input fanout
// transaction from its inputs, output count, and change script.
func estimateFanoutVSize(inputs []*walletcore.Utxo, outputs int,
	changeScript []byte) int64 {

	var est input.TxWeightEstimator
	for _, utxo := range inputs {
		if utxo == nil {
			continue
		}

		addFanoutInput(&est, utxo.PkScript)
	}
	for i := 0; i < outputs; i++ {
		est.AddP2TROutput()
	}
	if len(changeScript) > 0 {
		est.AddOutput(changeScript)
	}

	return int64(est.VSize())
}

// addFanoutInput adds an input of the appropriate witness/script class to the
// estimator based on the pkScript being spent, defaulting to P2WKH for
// unrecognised scripts so the estimate never under-counts.
func addFanoutInput(est *input.TxWeightEstimator, pkScript []byte) {
	switch txscript.GetScriptClass(pkScript) {
	case txscript.WitnessV0PubKeyHashTy:
		est.AddP2WKHInput()

	case txscript.WitnessV1TaprootTy:
		est.AddTaprootKeySpendInput(txscript.SigHashDefault)

	case txscript.ScriptHashTy:
		est.AddNestedP2WKHInput()

	case txscript.PubKeyHashTy:
		est.AddP2PKHInput()

	default:
		est.AddP2WKHInput()
	}
}
