package unroll

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/recovery"
	"github.com/lightninglabs/darepo-client/lib/tx/arktx"
	"github.com/lightninglabs/darepo-client/vtxo"
)

const (
	// estimatedSweepVBytes is a conservative virtual-size estimate for the
	// timeout-path sweep spend.
	estimatedSweepVBytes = 200

	// defaultSweepFallbackFeeRateSatPerVByte is used when fee estimation is
	// temporarily unavailable on regtest or a cold backend.
	defaultSweepFallbackFeeRateSatPerVByte int64 = 2

	// defaultMaxSweepFeeRateSatPerVByte clamps pathological fee estimates.
	defaultMaxSweepFeeRateSatPerVByte int64 = 100
)

// estimateSweepFeeRate asks chainsource for a 6-block fee estimate and
// clamps it to a sane range.
//
// Three failure modes are handled:
//
//   - Ask error (chainsource unavailable or estimator cold): fall back
//     to a small fixed rate so regtest / fresh-sync daemons can still
//     produce a plausible sweep. This fallback is clamped so it never
//     exceeds maxFeeRate.
//
//   - Non-positive estimate: reject with an error. A zero-rate sweep
//     would be rejected by every node, so pretending is worse than
//     failing fast.
//
//   - Estimate above the cap (fee-spike, bad estimator signal):
//     clamp to maxFeeRate. The cap exists to protect against a
//     pathological backend returning e.g. 10000 sat/vB and burning
//     the entire sweep value in miner fees.
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
		fallbackFeeRate := defaultSweepFallbackFeeRateSatPerVByte
		if fallbackFeeRate > maxFeeRate {
			fallbackFeeRate = maxFeeRate
		}

		return fallbackFeeRate, nil
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

// buildSweepTx constructs and signs the final timeout-path sweep.
//
// Structure of the produced transaction:
//
//   - Version 3 (TRUC). CSV-relative timelocks work for any version >=2;
//     v3 is required because the shared txconfirm CPFP broadcaster gates
//     parent tx submission on BIP-431 (TRUC) semantics.
//   - One input spending proof.TargetOutpoint with Sequence set to the
//     descriptor's RelativeExpiry. That sequence value is what arms the
//     CSV check on chain; spending earlier is consensus-invalid, so the
//     actor must wait for the CSV to mature (AwaitingCSV) before
//     submitting.
//   - One P2TR output paying to a fresh wallet pkScript. Value =
//     inputValue - fee. A non-positive sweep value fails construction
//     outright since publishing a tx with value <= 0 is nonsensical.
//
// Signing uses the taproot timeout-path leaf (leaf index 1 in the
// standard VTXO tap tree). The leaf script is rebuilt from the
// descriptor's policy keys and CSV delay via arkscript, and the
// resulting witness is stored on TxIn[0].
//
// This function is deliberately pure: every IO boundary (fee estimate,
// wallet pkScript, wallet signing) is threaded in through parameters so
// buildSweepTx is directly test-friendly with injected fakes.
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

	// Resolve the target's TxOut (value + pkScript). This is the
	// source of truth for fee math and signing — the VTXO descriptor
	// stores a redundant PkScript but the proof-derived output is
	// authoritative.
	targetOutput, err := proof.TargetOutput()
	if err != nil {
		return nil, err
	}

	feeRate, err := estimateSweepFeeRate(ctx, chainSource, maxFeeRate)
	if err != nil {
		return nil, fmt.Errorf("estimate fee: %w", err)
	}

	// Ask the wallet for a fresh P2TR address. Every sweep attempt in
	// a single actor lifetime reuses b.sweepTx (and therefore this
	// pkScript) so we only burn one BIP32 address per VTXO — caller
	// (startSweep) is responsible for that reuse.
	sweepPkScript, err := wallet.NewWalletPkScript(ctx)
	if err != nil {
		return nil, fmt.Errorf("sweep pkscript: %w", err)
	}

	if len(sweepPkScript) == 0 {
		return nil, fmt.Errorf("wallet returned empty pkscript")
	}

	// Version 3 (TRUC) — CSV works for any v>=2, but the shared
	// txconfirm CPFP broadcaster rejects non-v3 parents because the
	// anchor-detection heuristic and package-relay strategy assume
	// BIP-431 semantics. Sequence = desc.RelativeExpiry is what tells
	// consensus "this input is only valid at least RelativeExpiry
	// blocks after the target confirmed" — the same CSV the planner
	// is tracking off-chain.
	sweepTx := wire.NewMsgTx(arktx.TxVersion)
	sweepTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: proof.TargetOutpoint(),
		Sequence:         desc.RelativeExpiry,
	})

	// Fee math is deliberately simple: a conservative static vsize
	// estimate (taproot key-path + timeout leaf spend fits well under
	// the estimatedSweepVBytes budget) times the clamped fee rate.
	// The dust check below handles the pathological case of a VTXO
	// whose value is below the sweep fee at current rates — in that
	// case the unroll cannot produce a viable spend and must fail.
	inputValue := btcutil.Amount(targetOutput.Value)
	fee := btcutil.Amount(feeRate * estimatedSweepVBytes)
	sweepValue := inputValue - fee
	if sweepValue <= 0 {
		return nil, fmt.Errorf(
			"sweep value %d not positive after fee %d",
			sweepValue, fee)
	}

	sweepTx.AddTxOut(&wire.TxOut{
		Value: int64(sweepValue),

		// Defensive copy: sweepPkScript came from the wallet and
		// the returned tx lives on past this call.
		PkScript: append([]byte(nil), sweepPkScript...),
	})

	if desc.ClientKey.PubKey == nil {
		return nil, fmt.Errorf("descriptor missing ClientKey pubkey")
	}
	if desc.OperatorKey == nil {
		return nil, fmt.Errorf("descriptor missing OperatorKey")
	}

	// Derive the timeout-path spend info from the policy keys. The
	// legacy leaf index 1 maps to the CSV-gated exit leaf in the
	// standard VTXO tap tree.
	spendInfo, err := arkscript.NewVTXOSpendInfoFromPolicy(
		desc.ClientKey.PubKey, desc.OperatorKey,
		desc.RelativeExpiry, 1,
	)
	if err != nil {
		return nil, fmt.Errorf("timeout spend info: %w", err)
	}

	// BuildSignDescriptor wires together everything the taproot
	// signer needs: the client key to sign with, the prevout being
	// spent (value + pkScript), pre-computed sighashes for the new
	// tx, and the input index (0 — we only have one input).
	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		targetOutput.PkScript, targetOutput.Value,
	)
	sigHashes := txscript.NewTxSigHashes(sweepTx, prevFetcher)
	signDesc := spendInfo.BuildSignDescriptor(
		desc.ClientKey, targetOutput, sigHashes, prevFetcher, 0,
	)

	// Sign and assemble the taproot timeout-path witness. This is
	// the only IO path that crosses into the wallet — from here on
	// the sweep tx is fully signed and byte-stable, which is what
	// lets startSweep persist its bytes before broadcasting.
	witness, err := arkscript.VTXOTimeoutSpendWitness(
		wallet, signDesc, sweepTx,
	)
	if err != nil {
		return nil, fmt.Errorf("timeout witness: %w", err)
	}

	sweepTx.TxIn[0].Witness = witness

	return sweepTx, nil
}
