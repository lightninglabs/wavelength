package unroll

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/txconfirm"
	"github.com/lightninglabs/darepo-client/unrollplan"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightninglabs/darepo-client/walletcore"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
)

const (
	feeInputFanoutTxVersion int32 = 2

	// feeInputFanoutDemandCoalesceDelay gives sibling child actors started
	// by the same user action a brief window to record their demand before
	// the first child decides the wallet's only UTXO is enough.
	feeInputFanoutDemandCoalesceDelay = 500 * time.Millisecond

	// feeInputFanoutMinPreemptiveOutputs is the minimum split size used
	// when a child sees exactly one usable wallet UTXO. Without this, the
	// first concurrent child can lease that UTXO before sibling demand is
	// visible, causing later children to fail instead of sharing a fanout.
	feeInputFanoutMinPreemptiveOutputs = 3
)

// FeeInputFanoutCoordinator serializes backing-wallet fanout for all child
// unroll actors owned by a registry.
type FeeInputFanoutCoordinator struct {
	mu sync.Mutex

	wallet      txconfirm.Wallet
	chainSource actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]
	maxFeeRate int64

	pending    fn.Option[pendingFeeInputFanout]
	demandByID map[string]int
}

type pendingFeeInputFanout struct {
	txid     chainhash.Hash
	pkScript []byte
}

// NewFeeInputFanoutCoordinator creates a registry-scoped wallet fanout
// coordinator.
func NewFeeInputFanoutCoordinator(wallet txconfirm.Wallet,
	chainSource actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	], maxFeeRate int64) *FeeInputFanoutCoordinator {

	return &FeeInputFanoutCoordinator{
		wallet:      wallet,
		chainSource: chainSource,
		maxFeeRate:  maxFeeRate,
	}
}

// PlanFeeInputs returns the wallet fee-input demand for a ready unroll
// frontier.
func (c *FeeInputFanoutCoordinator) PlanFeeInputs(ctx context.Context,
	ready []unrollplan.TxFrontier) (FeeInputPlan, error) {

	return c.PlanFeeInputsForChild(ctx, "", ready)
}

// PlanFeeInputsForChild records one child actor's current ready-frontier demand
// and returns the aggregate registry-level fanout plan.
func (c *FeeInputFanoutCoordinator) PlanFeeInputsForChild(ctx context.Context,
	childID string, ready []unrollplan.TxFrontier) (FeeInputPlan, error) {

	required := len(ready)
	plan := FeeInputPlan{
		RequiredFeeInputsNow: required,
	}
	if c == nil || required == 0 {
		return plan, nil
	}

	c.mu.Lock()
	if c.wallet == nil {
		c.mu.Unlock()

		return plan, nil
	}

	c.recordDemand(childID, required)
	c.mu.Unlock()

	if childID != "" {
		timer := time.NewTimer(feeInputFanoutDemandCoalesceDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}

			return plan, ctx.Err()

		case <-timer.C:
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	aggregateRequired := c.aggregateDemand()

	feeRate, err := estimateSweepFeeRate(
		ctx, c.chainSource, c.maxFeeRate,
	)
	if err != nil {
		return plan, fmt.Errorf("estimate fanout fee rate: %w", err)
	}

	outputAmount, err := recommendedFanoutOutputAmount(ready, feeRate)
	if err != nil {
		return plan, err
	}
	plan.RecommendedFanoutOutputAmountSat = outputAmount

	utxos, err := c.wallet.ListUnspent(
		ctx, int32(RequiredFeeInputConfirmations),
		wallet.MaxConfsForListUnspent,
	)
	if err != nil {
		return plan, fmt.Errorf("list wallet fee inputs: %w", err)
	}

	if pending := c.pending; pending.IsSome() {
		value := pending.UnsafeFromSome()
		if fanoutOutputConfirmed(utxos, value.txid) {
			c.pending = fn.None[pendingFeeInputFanout]()
		}
	}

	usable, total := feeInputSupply(utxos, outputAmount)
	plan.UsableFeeInputs = usable
	preemptiveFanout := needsPreemptiveSingleUTXOFanout(
		childID, usable, aggregateRequired,
	)
	if usable >= aggregateRequired && !preemptiveFanout {
		c.pending = fn.None[pendingFeeInputFanout]()
		c.clearDemand(childID)

		return plan, nil
	}

	if pending := c.pending; pending.IsSome() {
		value := pending.UnsafeFromSome()
		plan.PendingFanoutTxid = fn.Some(value.txid)
		plan.PendingFanoutPkScript = append(
			[]byte(nil), value.pkScript...,
		)

		return plan, nil
	}

	plan.FanoutOutputsNeeded = fanoutOutputsNeeded(
		aggregateRequired, preemptiveFanout,
	)
	fundingNeeded := btcutil.Amount(plan.FanoutOutputsNeeded) * outputAmount
	fundingNeeded += btcutil.Amount(
		estimateFeeInputFanoutVSize(nil, plan.FanoutOutputsNeeded, nil),
	) * btcutil.Amount(feeRate)
	if total < fundingNeeded {
		plan.FanoutFundingShortfallSat = fundingNeeded - total
		plan.FanoutOutputsNeeded = 0
	}

	return plan, nil
}

// fanoutOutputConfirmed reports whether the backing wallet now sees at least
// one confirmed output created by the pending fanout transaction.
func fanoutOutputConfirmed(utxos []*walletcore.Utxo, txid chainhash.Hash) bool {
	for _, utxo := range utxos {
		if utxo == nil {
			continue
		}

		if utxo.Outpoint.Hash == txid {
			return true
		}
	}

	return false
}

// needsPreemptiveSingleUTXOFanout reports whether a child should split the
// wallet before txconfirm can lease the only currently usable fee input.
func needsPreemptiveSingleUTXOFanout(childID string, usable,
	required int) bool {

	return childID != "" && usable == 1 && required == 1
}

// fanoutOutputsNeeded returns the number of ordinary wallet outputs to create
// for the current registry demand.
func fanoutOutputsNeeded(required int, preemptive bool) int {
	if preemptive && required < feeInputFanoutMinPreemptiveOutputs {
		return feeInputFanoutMinPreemptiveOutputs
	}

	return required
}

// recordDemand stores the current ready-frontier width for one child actor.
func (c *FeeInputFanoutCoordinator) recordDemand(childID string, required int) {
	if childID == "" {
		childID = "default"
	}

	if c.demandByID == nil {
		c.demandByID = make(map[string]int)
	}
	c.demandByID[childID] = required
}

// aggregateDemand sums all child ready-frontier demands known to the
// coordinator.
func (c *FeeInputFanoutCoordinator) aggregateDemand() int {
	var total int
	for _, demand := range c.demandByID {
		total += demand
	}

	return total
}

// clearDemand removes one child actor from the current fanout demand set.
func (c *FeeInputFanoutCoordinator) clearDemand(childID string) {
	if childID == "" {
		childID = "default"
	}

	delete(c.demandByID, childID)
}

// EnsureFanout creates a wallet fanout transaction, or returns the existing
// in-flight fanout another child actor already created.
func (c *FeeInputFanoutCoordinator) EnsureFanout(ctx context.Context,
	plan FeeInputPlan) (chainhash.Hash, []byte, error) {

	if c == nil {
		return chainhash.Hash{}, nil, fmt.Errorf("fanout coordinator " +
			"nil")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if pending := c.pending; pending.IsSome() {
		value := pending.UnsafeFromSome()

		return value.txid, append([]byte(nil), value.pkScript...), nil
	}

	if plan.FanoutOutputsNeeded <= 0 {
		return chainhash.Hash{}, nil, fmt.Errorf("fanout not needed")
	}

	feeRate, err := estimateSweepFeeRate(
		ctx, c.chainSource, c.maxFeeRate,
	)
	if err != nil {
		return chainhash.Hash{}, nil, fmt.Errorf("estimate "+
			"fanout fee: %w", err)
	}

	tx, watchScript, err := buildFeeInputFanoutTx(
		ctx, c.wallet, plan.FanoutOutputsNeeded,
		plan.RecommendedFanoutOutputAmountSat, feeRate,
	)
	if err != nil {
		return chainhash.Hash{}, nil, err
	}

	resp, err := c.chainSource.Ask(
		ctx, &chainsource.BroadcastTxRequest{
			Tx:    tx,
			Label: "unroll-fee-input-fanout",
		},
	).Await(ctx).Unpack()
	if err != nil {
		return chainhash.Hash{}, nil, fmt.Errorf("broadcast fanout: %w",
			err)
	}

	broadcastResp, ok := resp.(*chainsource.BroadcastTxResponse)
	if !ok {
		return chainhash.Hash{}, nil, fmt.Errorf("unexpected fanout "+
			"broadcast response %T", resp)
	}

	c.pending = fn.Some(pendingFeeInputFanout{
		txid:     broadcastResp.Txid,
		pkScript: append([]byte(nil), watchScript...),
	})

	return broadcastResp.Txid, watchScript, nil
}

// MarkConfirmed clears the in-flight fanout marker after a confirmation.
func (c *FeeInputFanoutCoordinator) MarkConfirmed(txid chainhash.Hash) {
	if c == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if pending := c.pending; pending.IsSome() {
		if pending.UnsafeFromSome().txid == txid {
			c.pending = fn.None[pendingFeeInputFanout]()
			c.demandByID = nil
		}
	}
}

// recommendedFanoutOutputAmount computes the largest per-parent CPFP funding
// output needed by the ready frontier.
func recommendedFanoutOutputAmount(ready []unrollplan.TxFrontier,
	feeRate int64) (btcutil.Amount, error) {

	recommended := DefaultFeeInputMinAmountSat
	for _, frontier := range ready {
		if frontier.Node == nil || frontier.Node.Tx == nil {
			return 0, fmt.Errorf("ready frontier %s missing tx",
				frontier.Txid)
		}

		fee, err := txconfirm.EstimatePackageFee(
			frontier.Node.Tx, btcutil.Amount(feeRate),
		)
		if err != nil {
			return 0, err
		}

		amount := fee + txconfirm.DustLimit
		if amount > recommended {
			recommended = amount
		}
	}

	return recommended, nil
}

// feeInputSupply counts wallet UTXOs large enough to be independent CPFP fee
// inputs and sums the confirmed wallet balance available for fanout.
func feeInputSupply(utxos []*wallet.Utxo,
	minAmount btcutil.Amount) (int, btcutil.Amount) {

	var usable int
	var total btcutil.Amount
	for _, utxo := range utxos {
		if utxo == nil {
			continue
		}

		total += utxo.Amount
		if utxo.Amount >= minAmount {
			usable++
		}
	}

	return usable, total
}

// buildFeeInputFanoutTx builds and signs a normal wallet transaction that
// splits wallet balance into distinct fee-input outputs.
func buildFeeInputFanoutTx(ctx context.Context, signer txconfirm.Wallet,
	outputs int, outputAmount btcutil.Amount, feeRate int64) (*wire.MsgTx,
	[]byte, error) {

	if signer == nil {
		return nil, nil, fmt.Errorf("wallet must be provided")
	}

	if outputs <= 0 {
		return nil, nil, fmt.Errorf("fanout output count must be " +
			"positive")
	}

	if outputAmount <= txconfirm.DustLimit {
		return nil, nil, fmt.Errorf("fanout output amount %d is dust",
			outputAmount)
	}

	utxos, err := signer.ListUnspent(
		ctx, int32(RequiredFeeInputConfirmations),
		wallet.MaxConfsForListUnspent,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("list fanout inputs: %w", err)
	}

	selected, fee, changeScript, err := selectFeeInputFanoutInputs(
		ctx, signer, utxos, outputs, outputAmount, feeRate,
	)
	if err != nil {
		return nil, nil, err
	}

	tx := wire.NewMsgTx(feeInputFanoutTxVersion)
	for _, utxo := range selected {
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: utxo.Outpoint,
			Sequence:         wire.MaxTxInSequenceNum,
		})
	}

	var watchScript []byte
	for idx := 0; idx < outputs; idx++ {
		pkScript, err := signer.NewWalletPkScript(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("fanout pkscript: %w", err)
		}

		if len(pkScript) == 0 {
			return nil, nil, fmt.Errorf("wallet returned empty " +
				"pkscript")
		}

		if idx == 0 {
			watchScript = append([]byte(nil), pkScript...)
		}
		tx.AddTxOut(&wire.TxOut{
			Value:    int64(outputAmount),
			PkScript: pkScript,
		})
	}

	change := selectedTotal(selected) -
		btcutil.Amount(outputs)*outputAmount - fee
	if change >= txconfirm.DustLimit {
		tx.AddTxOut(&wire.TxOut{
			Value:    int64(change),
			PkScript: changeScript,
		})
	}

	finalTx, err := signFeeInputFanoutTx(ctx, signer, tx, selected)
	if err != nil {
		return nil, nil, err
	}

	return finalTx, watchScript, nil
}

// selectFeeInputFanoutInputs selects wallet inputs and estimates the fee for
// the requested fanout shape.
func selectFeeInputFanoutInputs(ctx context.Context, signer txconfirm.Wallet,
	utxos []*wallet.Utxo, outputs int, outputAmount btcutil.Amount,
	feeRate int64) ([]*wallet.Utxo, btcutil.Amount, []byte, error) {

	if feeRate <= 0 {
		return nil, 0, nil, fmt.Errorf("fee rate must be positive")
	}

	candidates := append([]*wallet.Utxo(nil), utxos...)
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i] == nil {
			return false
		}
		if candidates[j] == nil {
			return true
		}

		return candidates[i].Amount > candidates[j].Amount
	})

	changeScript, err := signer.NewWalletPkScript(ctx)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("fanout change pkscript: %w",
			err)
	}

	var selected []*wallet.Utxo
	for _, utxo := range candidates {
		if utxo == nil {
			continue
		}

		selected = append(selected, utxo)
		feeNoChange := btcutil.Amount(
			estimateFeeInputFanoutVSize(
				selected, outputs, nil,
			),
		) * btcutil.Amount(feeRate)
		neededNoChange := btcutil.Amount(outputs)*outputAmount +
			feeNoChange
		total := selectedTotal(selected)
		if total == neededNoChange ||
			total-neededNoChange < txconfirm.DustLimit {

			if total >= neededNoChange {
				return selected, feeNoChange, changeScript, nil
			}
		}

		feeWithChange := btcutil.Amount(
			estimateFeeInputFanoutVSize(
				selected, outputs, changeScript,
			),
		) * btcutil.Amount(feeRate)
		neededWithChange := btcutil.Amount(outputs)*outputAmount +
			feeWithChange + txconfirm.DustLimit
		if total >= neededWithChange {
			return selected, feeWithChange, changeScript, nil
		}
	}

	return nil, 0, nil, fmt.Errorf("insufficient wallet balance for " +
		"fee-input fanout")
}

// selectedTotal sums selected wallet input amounts.
func selectedTotal(utxos []*wallet.Utxo) btcutil.Amount {
	var total btcutil.Amount
	for _, utxo := range utxos {
		if utxo != nil {
			total += utxo.Amount
		}
	}

	return total
}

// estimateFeeInputFanoutVSize estimates the fanout transaction vsize.
func estimateFeeInputFanoutVSize(inputs []*wallet.Utxo, outputs int,
	changeScript []byte) int64 {

	var est input.TxWeightEstimator
	if len(inputs) == 0 {
		est.AddTaprootKeySpendInput(txscript.SigHashDefault)
	} else {
		for _, utxo := range inputs {
			if utxo == nil {
				continue
			}

			addFeeInputFanoutInput(&est, utxo.PkScript)
		}
	}
	for i := 0; i < outputs; i++ {
		est.AddP2TROutput()
	}
	if len(changeScript) > 0 {
		est.AddOutput(changeScript)
	}

	return int64(est.VSize())
}

// addFeeInputFanoutInput adds one wallet input to the weight estimator.
func addFeeInputFanoutInput(est *input.TxWeightEstimator, pkScript []byte) {
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

// signFeeInputFanoutTx signs the selected wallet inputs through the wallet's
// PSBT finalizer.
func signFeeInputFanoutTx(ctx context.Context, signer txconfirm.Wallet,
	tx *wire.MsgTx, utxos []*wallet.Utxo) (*wire.MsgTx, error) {

	inputs := make([]*wire.OutPoint, 0, len(tx.TxIn))
	sequences := make([]uint32, 0, len(tx.TxIn))
	witnessByOutpoint := make(map[wire.OutPoint]*wire.TxOut, len(utxos))
	for _, utxo := range utxos {
		if utxo == nil {
			continue
		}

		witnessByOutpoint[utxo.Outpoint] = &wire.TxOut{
			Value:    int64(utxo.Amount),
			PkScript: utxo.PkScript,
		}
	}

	for _, txIn := range tx.TxIn {
		inputs = append(inputs, &txIn.PreviousOutPoint)
		sequences = append(sequences, txIn.Sequence)
	}

	packet, err := psbt.New(
		inputs, tx.TxOut, tx.Version, tx.LockTime, sequences,
	)
	if err != nil {
		return nil, fmt.Errorf("create fanout PSBT: %w", err)
	}

	for idx, txIn := range tx.TxIn {
		witness, ok := witnessByOutpoint[txIn.PreviousOutPoint]
		if !ok {
			return nil, fmt.Errorf("missing witness UTXO for %s",
				txIn.PreviousOutPoint)
		}

		packet.Inputs[idx].WitnessUtxo = witness
	}

	var buf bytes.Buffer
	if err := packet.Serialize(&buf); err != nil {
		return nil, fmt.Errorf("serialize fanout PSBT: %w", err)
	}

	finalTx, err := signer.FinalizePsbt(ctx, buf.Bytes())
	if err != nil {
		return nil, err
	}

	if err := verifyFeeInputFanoutOutputs(tx, finalTx); err != nil {
		return nil, err
	}

	return finalTx, nil
}

// verifyFeeInputFanoutOutputs asserts that wallet finalization did not rewrite
// the fanout outputs.
func verifyFeeInputFanoutOutputs(expected, actual *wire.MsgTx) error {
	if expected == nil || actual == nil {
		return fmt.Errorf("fanout tx missing")
	}

	if len(expected.TxOut) != len(actual.TxOut) {
		return fmt.Errorf("fanout output count changed")
	}

	for idx := range expected.TxOut {
		want := expected.TxOut[idx]
		got := actual.TxOut[idx]
		if want.Value != got.Value ||
			!bytes.Equal(want.PkScript, got.PkScript) {
			return fmt.Errorf("fanout output %d changed", idx)
		}
	}

	return nil
}
