package unroll

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/lib/recovery"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/vtxo"
)

const (
	// estimatedSweepVBytes is a conservative virtual-size estimate for the
	// timeout-path sweep spend.
	estimatedSweepVBytes = 200

	// defaultMaxSweepFeeRateSatPerVByte clamps pathological fee estimates.
	defaultMaxSweepFeeRateSatPerVByte int64 = 100
)

// estimateSweepFeeRate returns the current fee rate in sat/vbyte, clamped to a
// sane maximum.
func estimateSweepFeeRate(ctx context.Context,
	chainSource actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	], maxFeeRate int64) (int64, error) {

	if maxFeeRate <= 0 {
		maxFeeRate = defaultMaxSweepFeeRateSatPerVByte
	}

	resp, err := chainSource.Ask(
		ctx, &chainsource.FeeEstimateRequest{TargetConf: 6},
	).Await(ctx).Unpack()
	if err != nil {
		return 0, err
	}

	feeResp, ok := resp.(*chainsource.FeeEstimateResponse)
	if !ok {
		return 0, fmt.Errorf("unexpected fee response %T", resp)
	}

	feeRate := int64(feeResp.SatPerVByte)
	if feeRate <= 0 {
		return 0, fmt.Errorf("fee rate must be positive")
	}

	if feeRate > maxFeeRate {
		feeRate = maxFeeRate
	}

	return feeRate, nil
}

// buildSweepTx constructs and signs the final timeout sweep for the target.
func buildSweepTx(ctx context.Context, wallet SweepWallet,
	chainSource actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	], proof *recovery.Proof,
	desc *vtxo.Descriptor, maxFeeRate int64) (*wire.MsgTx, error) {

	if wallet == nil {
		return nil, fmt.Errorf("sweep wallet must be provided")
	}

	if chainSource == nil {
		return nil, fmt.Errorf("chain source must be provided")
	}

	if proof == nil {
		return nil, fmt.Errorf("proof must be provided")
	}

	if desc == nil {
		return nil, fmt.Errorf("descriptor must be provided")
	}

	targetOutput, err := proof.TargetOutput()
	if err != nil {
		return nil, err
	}

	feeRate, err := estimateSweepFeeRate(ctx, chainSource, maxFeeRate)
	if err != nil {
		return nil, fmt.Errorf("estimate fee: %w", err)
	}

	sweepPkScript, err := wallet.NewWalletPkScript(ctx)
	if err != nil {
		return nil, fmt.Errorf("sweep pkscript: %w", err)
	}

	if len(sweepPkScript) == 0 {
		return nil, fmt.Errorf("wallet returned empty pkscript")
	}

	sweepTx := wire.NewMsgTx(2)
	sweepTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: proof.TargetOutpoint(),
		Sequence:         uint32(desc.RelativeExpiry),
	})

	inputValue := btcutil.Amount(targetOutput.Value)
	fee := btcutil.Amount(feeRate * estimatedSweepVBytes)
	sweepValue := inputValue - fee
	if sweepValue <= 0 {
		return nil, fmt.Errorf("sweep value %d not positive after fee %d",
			sweepValue, fee)
	}

	sweepTx.AddTxOut(&wire.TxOut{
		Value:    int64(sweepValue),
		PkScript: append([]byte(nil), sweepPkScript...),
	})

	if desc.TapScript == nil {
		return nil, fmt.Errorf("descriptor missing TapScript")
	}

	spendInfo, err := scripts.NewVTXOSpendInfo(
		desc.TapScript, scripts.VTXOTimeoutPathLeaf,
	)
	if err != nil {
		return nil, fmt.Errorf("timeout spend info: %w", err)
	}

	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		targetOutput.PkScript, targetOutput.Value,
	)
	sigHashes := txscript.NewTxSigHashes(sweepTx, prevFetcher)
	signDesc := scripts.VTXOSignDesc(
		desc.OwnerKey, targetOutput, sigHashes, prevFetcher, 0, spendInfo,
	)

	witness, err := scripts.VTXOTimeoutSpendWitness(
		wallet, signDesc, sweepTx,
	)
	if err != nil {
		return nil, fmt.Errorf("timeout witness: %w", err)
	}

	sweepTx.TxIn[0].Witness = witness

	return sweepTx, nil
}
