package unroll

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/recovery"
	"github.com/lightninglabs/wavelength/lib/tx/arktx"
	"github.com/lightninglabs/wavelength/vtxo"
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

// StandardVTXOExitSpendPolicy builds the normal timeout-path sweep used by
// standard Ark VTXOs.
type StandardVTXOExitSpendPolicy struct {
	desc *vtxo.Descriptor
}

// standardExitSpendPolicyResolver resolves the built-in standard VTXO timeout
// policy used when no custom resolver is configured.
type standardExitSpendPolicyResolver struct{}

// SupportsKind reports the only kind the built-in resolver covers.
func (r standardExitSpendPolicyResolver) SupportsKind(
	kind ExitPolicyKind) bool {

	return kind == "" || kind == StandardVTXOTimeoutExitPolicyKind
}

// ResolveExitSpendPolicy reconstructs the built-in standard exit policy.
func (r standardExitSpendPolicyResolver) ResolveExitSpendPolicy(
	ctx context.Context, req ExitSpendPolicyRequest) (ExitSpendPolicy,
	error) {

	// Unused today; kept on the interface for future resolvers that need
	// cancellable store lookups.
	_ = ctx

	if req.Kind != StandardVTXOTimeoutExitPolicyKind {
		return nil, fmt.Errorf("unknown exit policy kind: %s", req.Kind)
	}

	if req.Ref != "" {
		return nil, fmt.Errorf("standard exit policy ref must be empty")
	}

	return NewStandardVTXOExitSpendPolicy(req.StandardDescriptor), nil
}

// NewStandardVTXOExitSpendPolicy creates the built-in standard VTXO timeout
// exit policy.
func NewStandardVTXOExitSpendPolicy(
	desc *vtxo.Descriptor) *StandardVTXOExitSpendPolicy {

	return &StandardVTXOExitSpendPolicy{desc: desc}
}

// Kind returns the durable policy kind.
func (p *StandardVTXOExitSpendPolicy) Kind() ExitPolicyKind {
	return StandardVTXOTimeoutExitPolicyKind
}

// CSVDelay returns the descriptor relative expiry used by the timeout path.
// Standard VTXO descriptors store this as the raw block delay. The on-wire
// sequence is derived from this via blockchain.LockTimeToSequence so the BIP-68
// height-mode flags are explicit.
func (p *StandardVTXOExitSpendPolicy) CSVDelay() uint32 {
	if p == nil || p.desc == nil {
		return 0
	}

	return p.desc.RelativeExpiry
}

// RequiredLockTime returns zero. The standard timeout-path sweep has no
// absolute locktime gate.
func (p *StandardVTXOExitSpendPolicy) RequiredLockTime() uint32 {
	return 0
}

// ValidateTarget checks that a target output was materialized and matches the
// descriptor's pkScript. The pkScript comparison mirrors the vHTLC policy's
// invariant so a misrouted exit-policy kind cannot silently produce a sweep
// against the wrong taproot output; on-chain script verification would catch
// the broken witness later, but the recovery row would loop in retries with
// no operator-visible signal pointing at the policy mismatch.
func (p *StandardVTXOExitSpendPolicy) ValidateTarget(target *wire.TxOut) error {
	switch {
	case p == nil:
		return fmt.Errorf("standard exit policy must be provided")

	case p.desc == nil:
		return fmt.Errorf("descriptor must be provided")

	case target == nil:
		return fmt.Errorf("target output must be provided")
	}

	if target.Value <= 0 {
		return fmt.Errorf("target output value must be positive")
	}

	// The descriptor's PkScript is the authoritative output script for
	// the wrapped VTXO; refuse to sweep a materialized output whose
	// pkScript does not match it.
	if len(p.desc.PkScript) > 0 &&
		!bytes.Equal(target.PkScript, p.desc.PkScript) {
		return fmt.Errorf("target output pkscript does not match " +
			"descriptor pkscript")
	}

	return nil
}

// BuildSpendTx builds and signs the standard timeout-path sweep.
//
// The produced transaction is version 3 (TRUC). CSV-relative timelocks work
// for any version >=2, but the shared txconfirm CPFP broadcaster gates parent
// tx submission on BIP-431 semantics. The transaction has one input spending
// req.TargetOutpoint with Sequence set to the descriptor's RelativeExpiry. That
// field is a raw block count, and for BIP-68 height-mode block delays accepted
// by Ark descriptors the raw count is the encoded sequence value as well.
// Signing uses the taproot timeout-path leaf rebuilt from the descriptor's
// policy keys and CSV delay.
func (p *StandardVTXOExitSpendPolicy) BuildSpendTx(ctx context.Context,
	req ExitSpendRequest) (*wire.MsgTx, error) {

	// Unused by the standard policy; future policies may use it for
	// cancellable signer or store work.
	_ = ctx

	if err := p.ValidateTarget(req.TargetOutput); err != nil {
		return nil, err
	}

	if req.Signer == nil {
		return nil, fmt.Errorf("signer must be provided")
	}

	if len(req.DestinationPkScript) == 0 {
		return nil, fmt.Errorf("destination pkscript must be provided")
	}

	if req.FeeRateSatPerVByte <= 0 {
		return nil, fmt.Errorf("fee rate must be positive")
	}

	desc := p.desc
	if desc.ClientKey.PubKey == nil {
		return nil, fmt.Errorf("descriptor missing ClientKey pubkey")
	}
	if desc.OperatorKey == nil {
		return nil, fmt.Errorf("descriptor missing OperatorKey")
	}

	// Encode the BIP-68 height-mode sequence explicitly: for the small
	// height-mode delays standard VTXOs carry today the raw count and the
	// LockTimeToSequence result are numerically identical, but routing
	// through LockTimeToSequence locks in the disable-bit and type-flag
	// behaviour so future time-mode or larger-count delays cannot silently
	// flip the sequence into a non-relative value.
	sweepTx := wire.NewMsgTx(arktx.TxVersion)
	sweepTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: req.TargetOutpoint,
		Sequence: blockchain.LockTimeToSequence(
			false, p.CSVDelay(),
		),
	})

	inputValue := btcutil.Amount(req.TargetOutput.Value)
	fee := btcutil.Amount(req.FeeRateSatPerVByte * estimatedSweepVBytes)
	sweepValue := inputValue - fee
	if sweepValue <= 0 {
		return nil, fmt.Errorf("sweep value %d not positive "+
			"after fee %d", sweepValue, fee)
	}

	sweepTx.AddTxOut(&wire.TxOut{
		Value:    int64(sweepValue),
		PkScript: append([]byte(nil), req.DestinationPkScript...),
	})

	spendInfo, err := arkscript.NewVTXOSpendInfoFromPolicy(
		desc.ClientKey.PubKey, desc.OperatorKey, p.CSVDelay(), 1,
	)
	if err != nil {
		return nil, fmt.Errorf("timeout spend info: %w", err)
	}

	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		req.TargetOutput.PkScript, req.TargetOutput.Value,
	)
	sigHashes := txscript.NewTxSigHashes(sweepTx, prevFetcher)
	signDesc := spendInfo.BuildSignDescriptor(
		desc.ClientKey, req.TargetOutput, sigHashes, prevFetcher, 0,
	)

	witness, err := arkscript.VTXOTimeoutSpendWitness(
		req.Signer, signDesc, sweepTx,
	)
	if err != nil {
		return nil, fmt.Errorf("timeout witness: %w", err)
	}

	sweepTx.TxIn[0].Witness = witness

	return sweepTx, nil
}

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

// buildSweepTx resolves the common IO inputs for the final exit spend and
// delegates transaction construction to policy.BuildSpendTx.
func buildSweepTx(ctx context.Context, wallet SweepWallet,
	chainSource actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	], proof *recovery.Proof,
	desc *vtxo.Descriptor, maxFeeRate int64, currentHeight int32,
	policy ExitSpendPolicy) (*wire.MsgTx, error) {

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

	if policy == nil {
		return nil, fmt.Errorf("exit spend policy must be provided")
	}

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

	return policy.BuildSpendTx(ctx, ExitSpendRequest{
		TargetOutpoint:      proof.TargetOutpoint(),
		TargetOutput:        targetOutput,
		DestinationPkScript: sweepPkScript,
		FeeRateSatPerVByte:  feeRate,
		CurrentHeight:       currentHeight,
		Signer:              wallet,
	})
}
