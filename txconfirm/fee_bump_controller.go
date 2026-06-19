package txconfirm

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/walletcore"
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

// feeInputDemand records that one anchor parent needs a confirmed wallet fee
// input of at least minAmount before its CPFP child can be built. The actor
// computes the demand set from its tracked txids and hands it to the
// controller; the controller owns the supply decision.
type feeInputDemand struct {
	// parentTxid is the anchor parent that needs a fee input.
	parentTxid chainhash.Hash

	// minAmount is the smallest fee input that would unblock the parent.
	minAmount btcutil.Amount
}

// FeeBumpInputController owns the fee-input fanout lifecycle. When a CPFP
// child for an anchor parent cannot find a confirmed wallet UTXO to pay fees,
// the controller broadcasts a fanout transaction that splits wallet funds into
// several right-sized fee-input outputs, watches for it to confirm, then
// promotes those outputs into the per-parent reservation map so the blocked
// parents can be retried.
//
// The controller is the single owner of all fanout state and bookkeeping. It
// shares the broadcaster's per-parent reservation map (parentStates) by
// holding a reference to the CPFPBroadcaster, so predicted/used fee-input
// accounting stays consistent with the CPFP-child build path. The in-flight
// fanout itself lives as protofsm state, not as a raw field, so the lifecycle
// (broadcast -> awaiting confirmation -> idle) is explicit and follows the
// same protofsm shape as the tracked-tx FSM.
//
// Like CPFPBroadcaster, FeeBumpInputController is not safe for concurrent use:
// the txconfirm actor serializes access.
type FeeBumpInputController struct {
	// broadcaster provides the shared parentStates reservation map plus the
	// wallet/chainsource helpers the fanout build path reuses
	// (deriveChangePkScript, selectReservedFeeInput, releaseWalletLease,
	// parentState).
	broadcaster *CPFPBroadcaster

	// fsm tracks the lifecycle of the at-most-one in-flight fanout.
	fsm *feeBumpStateMachine

	log btclog.Logger
}

// NewFeeBumpInputController creates a fanout controller bound to the supplied
// broadcaster. The controller reuses the broadcaster's wallet, chainsource,
// logger, and shared per-parent reservation map.
func NewFeeBumpInputController(
	broadcaster *CPFPBroadcaster) *FeeBumpInputController {

	log := broadcaster.log

	return &FeeBumpInputController{
		broadcaster: broadcaster,
		fsm:         newFeeBumpStateMachine(log),
		log:         log,
	}
}

// Start spins up the controller's fanout FSM goroutine. It must be called
// before the controller is driven.
func (c *FeeBumpInputController) Start(ctx context.Context) {
	c.fsm.Start(ctx)
}

// ensureStarted lazily starts the fanout FSM the first time the controller is
// driven. The supplied context is the actor's long-lived internal context (the
// same one used to start the per-tracked-tx FSMs), so the fanout FSM goroutine
// outlives any individual request that happens to trigger the first fanout.
func (c *FeeBumpInputController) ensureStarted(ctx context.Context) {
	if c.fsm == nil || c.fsm.IsRunning() {
		return
	}

	c.fsm.Start(ctx)
}

// Stop tears down the controller's fanout FSM goroutine.
func (c *FeeBumpInputController) Stop() {
	if c.fsm != nil {
		c.fsm.Stop()
	}
}

// currentState returns the current fanout FSM state.
func (c *FeeBumpInputController) currentState() (feeBumpState, error) {
	if c.fsm == nil || !c.fsm.IsRunning() {
		return nil, fmt.Errorf("fee bump fsm not running")
	}

	rawState, err := c.fsm.CurrentState()
	if err != nil {
		return nil, err
	}

	state, ok := rawState.(feeBumpState)
	if !ok {
		return nil, fmt.Errorf("unexpected fee bump state %T", rawState)
	}

	return state, nil
}

// PendingFanout returns the in-flight fanout, or nil if none is pending. It is
// the controller's read accessor for the FSM-owned pending state.
func (c *FeeBumpInputController) PendingFanout() *pendingFeeInputFanout {
	state, err := c.currentState()
	if err != nil {
		return nil
	}

	return feeBumpPendingFanout(state)
}

// sendEvent drives the fanout FSM with one event, waiting for the transition to
// complete so the controller's subsequent reads see the updated state.
func (c *FeeBumpInputController) sendEvent(ctx context.Context,
	event feeBumpEvent) error {

	if c.fsm == nil {
		return fmt.Errorf("fee bump fsm not initialized")
	}

	_, err := c.fsm.AskEvent(ctx, event).Await(ctx).Unpack()

	return err
}

// EnsureSupply provisions confirmed wallet fee inputs for the supplied blocked
// parents. If a fanout is already in flight it is rebroadcast on the fee-bump
// interval and returned as-is. Otherwise, demands that cannot be served from
// the current wallet supply trigger a fresh fanout broadcast: wallet funds are
// split into right-sized outputs, reserved as predicted fee inputs for each
// parent, and broadcast. The returned pending fanout (if any) is what the
// actor registers a confirmation watch on.
func (c *FeeBumpInputController) EnsureSupply(ctx context.Context,
	demands []feeInputDemand, feeRate int64, height, retryInterval int32) (
	*pendingFeeInputFanout, error) {

	b := c.broadcaster
	if b == nil || b.cfg.Wallet == nil || len(demands) == 0 {
		return nil, nil
	}

	c.ensureStarted(ctx)

	if c.PendingFanout() != nil {
		pending, err := c.ensurePendingFanoutLive(
			ctx, height, retryInterval,
		)
		if err != nil {
			return nil, err
		}
		if pending != nil {
			return pending, nil
		}
	}

	blocked := c.blockedFeeInputDemands(demands)
	if len(blocked) == 0 {
		return nil, nil
	}

	utxos, err := b.cfg.Wallet.ListUnspent(ctx, 1, 9999999)
	if err != nil {
		return nil, fmt.Errorf("list fanout supply: %w", err)
	}

	available := fanoutAvailableFeeInputs(utxos, c.reservedOutpoints())
	blocked = demandsStillMissingSupply(blocked, available)
	if len(blocked) == 0 {
		return nil, nil
	}

	fanoutDemands := blocked
	fundedTx, err := c.buildFeeInputFanoutTx(
		ctx, blocked, len(blocked), feeRate,
	)
	if err != nil {
		reservedInputs := c.reservedFeeInputsForFanout(demands)
		if len(reservedInputs) == 0 {
			return nil, err
		}

		fundedTx, err = c.buildReservedInputFanoutTx(
			ctx, demands, reservedInputs,
			max(
				len(demands), minFeeInputFanoutOutputs,
			),
			feeRate,
		)
		if err != nil {
			return nil, err
		}
		fanoutDemands = demands
	}

	txid := fundedTx.TxHash()
	assignments, err := c.reservePredictedFanoutOutputs(
		txid, fundedTx, fanoutDemands,
	)
	if err != nil {
		return nil, err
	}

	for _, txIn := range fundedTx.TxIn {
		_, err := b.cfg.Wallet.LeaseOutput(
			ctx, txconfirmLockID, txIn.PreviousOutPoint,
			DefaultFeeInputLeaseExpiry,
		)
		if err != nil {
			c.log.WarnS(ctx, "Fanout input lease failed; relying "+
				"on wallet funding lock",
				err, "outpoint", txIn.PreviousOutPoint)
		}
	}

	var watchScript []byte
	for _, txOut := range fundedTx.TxOut {
		for _, demand := range blocked {
			if txOut.Value == int64(demand.minAmount) {
				watchScript = append(
					[]byte(nil), txOut.PkScript...,
				)

				break
			}
		}
		if len(watchScript) > 0 {
			break
		}
	}
	if len(watchScript) == 0 {
		return nil, fmt.Errorf("fanout watch script not found")
	}

	resp, err := b.cfg.ChainSource.Ask(
		ctx, &chainsource.BroadcastTxRequest{
			Tx:    fundedTx,
			Label: "txconfirm-fee-input-fanout",
		},
	).Await(ctx).Unpack()
	if err != nil {
		c.releasePredictedFanoutOutputs(assignments)

		return nil, fmt.Errorf("broadcast fanout: %w", err)
	}

	broadcastResp, ok := resp.(*chainsource.BroadcastTxResponse)
	if !ok {
		c.releasePredictedFanoutOutputs(assignments)

		return nil, fmt.Errorf("unexpected fanout response %T", resp)
	}

	pending := &pendingFeeInputFanout{
		txid:                broadcastResp.Txid,
		tx:                  fundedTx.Copy(),
		watchScript:         watchScript,
		assignments:         assignments,
		lastBroadcastHeight: height,
	}
	c.consumeFanoutInputs(fundedTx.TxIn)

	if err := c.sendEvent(ctx, &feeBumpFanoutBroadcast{
		pending: pending,
	}); err != nil {

		c.releasePredictedFanoutOutputs(assignments)

		return nil, fmt.Errorf("record fanout broadcast: %w", err)
	}

	return pending, nil
}

// ensurePendingFanoutLive keeps the in-flight fanout fresh: if the fee-bump
// interval has elapsed since the last broadcast, it rebroadcasts the same
// fanout transaction and refreshes the recorded height. A rejected
// rebroadcast clears the fanout so EnsureSupply can rebuild from scratch.
func (c *FeeBumpInputController) ensurePendingFanoutLive(ctx context.Context,
	height, retryInterval int32) (*pendingFeeInputFanout, error) {

	pending := c.PendingFanout()
	if pending == nil {
		return nil, nil
	}
	if retryInterval <= 0 {
		retryInterval = DefaultFeeBumpIntervalBlocks
	}

	if pending.tx == nil || height-pending.lastBroadcastHeight <
		retryInterval {
		return pending, nil
	}

	resp, err := c.broadcaster.cfg.ChainSource.Ask(
		ctx, &chainsource.BroadcastTxRequest{
			Tx:    pending.tx.Copy(),
			Label: "txconfirm-fee-input-fanout",
		},
	).Await(ctx).Unpack()
	if err != nil {
		if IsIgnorableBroadcastError(err) {
			return c.recordRebroadcast(ctx, height)
		}

		c.log.WarnS(ctx, "Pending fee-input fanout rejected; "+
			"rebuilding", err, "txid", pending.txid)
		c.clearPendingFanout(ctx)

		return nil, nil
	}

	broadcastResp, ok := resp.(*chainsource.BroadcastTxResponse)
	if !ok {
		err := fmt.Errorf("unexpected fanout response %T", resp)
		c.clearPendingFanout(ctx)

		return nil, err
	}

	if broadcastResp.Txid != pending.txid {
		err := fmt.Errorf("fanout rebroadcast txid changed")
		c.clearPendingFanout(ctx)

		return nil, err
	}

	return c.recordRebroadcast(ctx, height)
}

// recordRebroadcast refreshes the in-flight fanout's last-broadcast height in
// the FSM and returns the updated pending state.
func (c *FeeBumpInputController) recordRebroadcast(ctx context.Context,
	height int32) (*pendingFeeInputFanout, error) {

	if err := c.sendEvent(ctx, &feeBumpFanoutRebroadcast{
		height: height,
	}); err != nil {
		return nil, fmt.Errorf("record fanout rebroadcast: %w", err)
	}

	return c.PendingFanout(), nil
}

// blockedFeeInputDemands filters the demand set down to parents that cannot be
// served from their own already-reserved confirmed fee inputs, sorted by txid
// for deterministic fanout output assignment.
func (c *FeeBumpInputController) blockedFeeInputDemands(
	demands []feeInputDemand) []feeInputDemand {

	var blocked []feeInputDemand
	for _, demand := range demands {
		if demand.minAmount <= 0 {
			continue
		}

		if c.broadcaster.selectReservedFeeInput(
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
//
//nolint:ll
func (c *FeeBumpInputController) reservedOutpoints() map[wire.OutPoint]struct{} {
	reserved := make(map[wire.OutPoint]struct{})
	for _, state := range c.broadcaster.parentStates {
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
func (c *FeeBumpInputController) buildFeeInputFanoutTx(ctx context.Context,
	demands []feeInputDemand, minOutputs int, feeRate int64) (*wire.MsgTx,
	error) {

	outputs, err := c.fanoutOutputs(ctx, demands, minOutputs)
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

	fundedTx, err := c.broadcaster.cfg.Wallet.FundPsbt(
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
func (c *FeeBumpInputController) fanoutOutputs(ctx context.Context,
	demands []feeInputDemand, minOutputs int) ([]*wire.TxOut, error) {

	outputCount := max(len(demands), minOutputs)
	outputs := make([]*wire.TxOut, 0, outputCount)
	var extraAmount btcutil.Amount
	for _, demand := range demands {
		if demand.minAmount > extraAmount {
			extraAmount = demand.minAmount
		}

		pkScript, err := c.broadcaster.deriveChangePkScript(ctx)
		if err != nil {
			return nil, fmt.Errorf("fanout pkscript: %w", err)
		}

		outputs = append(outputs, &wire.TxOut{
			Value:    int64(demand.minAmount),
			PkScript: pkScript,
		})
	}
	for len(outputs) < outputCount {
		pkScript, err := c.broadcaster.deriveChangePkScript(ctx)
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
func (c *FeeBumpInputController) reservedFeeInputsForFanout(
	demands []feeInputDemand) []*FeeInput {

	var inputs []*FeeInput
	seen := make(map[wire.OutPoint]struct{})
	for _, demand := range demands {
		state := c.broadcaster.parentStates[demand.parentTxid]
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
func (c *FeeBumpInputController) buildReservedInputFanoutTx(ctx context.Context,
	demands []feeInputDemand, inputs []*FeeInput, minOutputs int,
	feeRate int64) (*wire.MsgTx, error) {

	outputs, err := c.fanoutOutputs(ctx, demands, minOutputs)
	if err != nil {
		return nil, err
	}

	changePkScript, err := c.broadcaster.deriveChangePkScript(ctx)
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

	fundedTx, err := c.broadcaster.cfg.Wallet.FinalizePsbt(ctx, buf.Bytes())
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
func (c *FeeBumpInputController) consumeFanoutInputs(inputs []*wire.TxIn) {
	for _, txIn := range inputs {
		op := txIn.PreviousOutPoint
		for _, state := range c.broadcaster.parentStates {
			delete(state.UsedFeeOutpoints, op)
			delete(state.UsedFeeInputs, op)
		}
	}
}

// PruneParent drops the supplied parent from the in-flight fanout's
// assignments. When no parents remain, the whole fanout is cleared. The actor
// calls this whenever a parent is evicted (terminal confirm/fail) so a fanout
// is never left serving a parent that no longer exists.
func (c *FeeBumpInputController) PruneParent(ctx context.Context,
	parent chainhash.Hash) {

	pending := c.PendingFanout()
	if pending == nil {
		return
	}

	delete(pending.assignments, parent)
	if len(pending.assignments) == 0 {
		c.clearPendingFanout(ctx)
	}
}

// reservePredictedFanoutOutputs assigns each demand a matching fanout output
// (by value) as a predicted fee input on that parent's state, returning the
// per-parent outpoint assignments. A demand with no matching output rolls the
// whole reservation back.
func (c *FeeBumpInputController) reservePredictedFanoutOutputs(
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

			state := c.broadcaster.parentState(demand.parentTxid)
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
			c.releasePredictedFanoutOutputs(assignments)

			return nil, fmt.Errorf("fanout missing output for %s",
				demand.parentTxid)
		}
	}

	return assignments, nil
}

// releasePredictedFanoutOutputs removes the supplied predicted-output
// assignments from their parents' states, used to roll back a partial or
// abandoned fanout reservation.
func (c *FeeBumpInputController) releasePredictedFanoutOutputs(
	assignments map[chainhash.Hash][]wire.OutPoint) {

	for parent, outpoints := range assignments {
		state := c.broadcaster.parentStates[parent]
		if state == nil {
			continue
		}

		for _, op := range outpoints {
			delete(state.PredictedFeeInputs, op)
		}
	}
}

// clearPendingFanout abandons the in-flight fanout: it releases the predicted
// outputs, releases the wallet leases on the fanout's inputs, and returns the
// FSM to idle.
func (c *FeeBumpInputController) clearPendingFanout(ctx context.Context) {
	pending := c.PendingFanout()
	if pending == nil {
		return
	}

	c.releasePredictedFanoutOutputs(pending.assignments)
	if pending.tx != nil {
		for _, txIn := range pending.tx.TxIn {
			c.broadcaster.releaseWalletLease(
				ctx, txIn.PreviousOutPoint,
			)
		}
	}

	if err := c.sendEvent(ctx, &feeBumpFanoutCleared{}); err != nil {
		c.log.WarnS(ctx, "Failed to clear pending fanout", err)
	}
}

// OnFanoutConfirmed promotes a confirmed fanout's predicted outputs into used
// fee inputs on each assigned parent and returns the FSM to idle. It returns
// true when the supplied txid matched the in-flight fanout, so the actor knows
// the confirmation was a fanout confirmation rather than a tracked-tx one.
func (c *FeeBumpInputController) OnFanoutConfirmed(ctx context.Context,
	txid chainhash.Hash) bool {

	pending := c.PendingFanout()
	if pending == nil || pending.txid != txid {
		return false
	}

	for parent, outpoints := range pending.assignments {
		state := c.broadcaster.parentStates[parent]
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

	if err := c.sendEvent(ctx, &feeBumpFanoutConfirmed{
		txid: txid,
	}); err != nil {

		c.log.WarnS(ctx, "Failed to record fanout confirmation",
			err, "txid", txid)
	}

	return true
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
