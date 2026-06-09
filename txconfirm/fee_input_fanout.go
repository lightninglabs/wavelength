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
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/walletcore"
	"github.com/lightningnetwork/lnd/input"
)

const (
	feeInputFanoutTxVersion  int32 = 2
	minFeeInputFanoutOutputs       = 3
)

type feeInputDemand struct {
	parentTxid chainhash.Hash
	minAmount  btcutil.Amount
}

type pendingFeeInputFanout struct {
	txid        chainhash.Hash
	watchScript []byte
	assignments map[chainhash.Hash][]wire.OutPoint
}

func (b *CPFPBroadcaster) ensureFeeInputSupply(ctx context.Context,
	demands []feeInputDemand, feeRate int64) (*pendingFeeInputFanout,
	error) {

	if b == nil || b.cfg.Wallet == nil || len(demands) == 0 {
		return nil, nil
	}

	if b.pendingFanout != nil {
		return b.pendingFanout, nil
	}

	blocked := b.blockedFeeInputDemands(demands)
	if len(blocked) == 0 {
		return nil, nil
	}

	utxos, err := b.cfg.Wallet.ListUnspent(ctx, 1, 9999999)
	if err != nil {
		return nil, fmt.Errorf("list fanout supply: %w", err)
	}

	available := fanoutAvailableFeeInputs(utxos, b.reservedOutpoints())
	blocked = demandsStillMissingSupply(blocked, available)
	if len(blocked) == 0 {
		return nil, nil
	}

	fanoutDemands := blocked
	fundedTx, err := b.buildFeeInputFanoutTx(
		ctx, blocked, len(blocked), feeRate,
	)
	if err != nil {
		reservedInputs := b.reservedFeeInputsForFanout(demands)
		if len(reservedInputs) == 0 {
			return nil, err
		}

		fundedTx, err = b.buildReservedInputFanoutTx(
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
	assignments, err := b.reservePredictedFanoutOutputs(
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
			b.log.WarnS(ctx, "Fanout input lease failed; relying "+
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
		b.releasePredictedFanoutOutputs(assignments)

		return nil, fmt.Errorf("broadcast fanout: %w", err)
	}

	broadcastResp, ok := resp.(*chainsource.BroadcastTxResponse)
	if !ok {
		b.releasePredictedFanoutOutputs(assignments)

		return nil, fmt.Errorf("unexpected fanout response %T", resp)
	}

	pending := &pendingFeeInputFanout{
		txid:        broadcastResp.Txid,
		watchScript: watchScript,
		assignments: assignments,
	}
	b.consumeFanoutInputs(fundedTx.TxIn)
	b.pendingFanout = pending

	return pending, nil
}

func (b *CPFPBroadcaster) blockedFeeInputDemands(
	demands []feeInputDemand) []feeInputDemand {

	var blocked []feeInputDemand
	for _, demand := range demands {
		if demand.minAmount <= 0 {
			continue
		}

		if b.selectReservedFeeInput(
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

func (b *CPFPBroadcaster) reservedOutpoints() map[wire.OutPoint]struct{} {
	reserved := make(map[wire.OutPoint]struct{})
	for _, state := range b.parentStates {
		for op := range state.UsedFeeOutpoints {
			reserved[op] = struct{}{}
		}
		for op := range state.PredictedFeeInputs {
			reserved[op] = struct{}{}
		}
	}

	return reserved
}

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

func (b *CPFPBroadcaster) buildFeeInputFanoutTx(ctx context.Context,
	demands []feeInputDemand, minOutputs int, feeRate int64) (*wire.MsgTx,
	error) {

	outputs, err := b.fanoutOutputs(ctx, demands, minOutputs)
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

	fundedTx, err := b.cfg.Wallet.FundPsbt(
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

func (b *CPFPBroadcaster) fanoutOutputs(ctx context.Context,
	demands []feeInputDemand, minOutputs int) ([]*wire.TxOut, error) {

	outputCount := max(len(demands), minOutputs)
	outputs := make([]*wire.TxOut, 0, outputCount)
	var extraAmount btcutil.Amount
	for _, demand := range demands {
		if demand.minAmount > extraAmount {
			extraAmount = demand.minAmount
		}

		pkScript, err := b.deriveChangePkScript(ctx)
		if err != nil {
			return nil, fmt.Errorf("fanout pkscript: %w", err)
		}

		outputs = append(outputs, &wire.TxOut{
			Value:    int64(demand.minAmount),
			PkScript: pkScript,
		})
	}
	for len(outputs) < outputCount {
		pkScript, err := b.deriveChangePkScript(ctx)
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

func (b *CPFPBroadcaster) reservedFeeInputsForFanout(
	demands []feeInputDemand) []*FeeInput {

	var inputs []*FeeInput
	seen := make(map[wire.OutPoint]struct{})
	for _, demand := range demands {
		state := b.parentStates[demand.parentTxid]
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

func (b *CPFPBroadcaster) buildReservedInputFanoutTx(ctx context.Context,
	demands []feeInputDemand, inputs []*FeeInput, minOutputs int,
	feeRate int64) (*wire.MsgTx, error) {

	outputs, err := b.fanoutOutputs(ctx, demands, minOutputs)
	if err != nil {
		return nil, err
	}

	changePkScript, err := b.deriveChangePkScript(ctx)
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

	fundedTx, err := b.cfg.Wallet.FinalizePsbt(ctx, buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("finalize reserved fanout PSBT: %w", err)
	}
	if err := verifyFanoutOutputs(outputs, fundedTx); err != nil {
		return nil, err
	}

	return fundedTx, nil
}

func (b *CPFPBroadcaster) consumeFanoutInputs(inputs []*wire.TxIn) {
	for _, txIn := range inputs {
		op := txIn.PreviousOutPoint
		for _, state := range b.parentStates {
			delete(state.UsedFeeOutpoints, op)
			delete(state.UsedFeeInputs, op)
		}
	}
}

func (b *CPFPBroadcaster) reservePredictedFanoutOutputs(txid chainhash.Hash,
	tx *wire.MsgTx, demands []feeInputDemand) (
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

			state := b.parentState(demand.parentTxid)
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
			b.releasePredictedFanoutOutputs(assignments)

			return nil, fmt.Errorf("fanout missing output for %s",
				demand.parentTxid)
		}
	}

	return assignments, nil
}

func (b *CPFPBroadcaster) releasePredictedFanoutOutputs(
	assignments map[chainhash.Hash][]wire.OutPoint) {

	for parent, outpoints := range assignments {
		state := b.parentStates[parent]
		if state == nil {
			continue
		}

		for _, op := range outpoints {
			delete(state.PredictedFeeInputs, op)
		}
	}
}

func (b *CPFPBroadcaster) PromoteConfirmedFanout(txid chainhash.Hash) {
	if b.pendingFanout == nil || b.pendingFanout.txid != txid {
		return
	}

	for parent, outpoints := range b.pendingFanout.assignments {
		state := b.parentState(parent)
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

	b.pendingFanout = nil
}

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

func cloneTxOut(txOut *wire.TxOut) *wire.TxOut {
	if txOut == nil {
		return nil
	}

	return &wire.TxOut{
		Value:    txOut.Value,
		PkScript: append([]byte(nil), txOut.PkScript...),
	}
}

func estimateFanoutVSize(inputs []*walletcore.Utxo, outputs int,
	changeScript []byte) int64 {

	var est input.TxWeightEstimator
	if len(inputs) == 0 {
		est.AddTaprootKeySpendInput(txscript.SigHashDefault)
	} else {
		for _, utxo := range inputs {
			if utxo == nil {
				continue
			}

			addFanoutInput(&est, utxo.PkScript)
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
