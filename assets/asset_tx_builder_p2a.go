package assets

import (
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// -----------------------------------------------------------------------------
// P2A (Pay-to-Anchor) Infrastructure
//
// This file implements zero-value ephemeral anchor outputs used for CPFP
// (Child-Pays-For-Parent) fee bumping. P2A anchors allow broadcasting
// transactions with zero fees and then having a separate child transaction
// pay for the entire package.
//
// Key components:
// - payToAnchorPkScript: The OP_1 <0x4e73> witness v1 program
// - BuildAnchorChild: Builds CPFP child spending all builder anchors
// - adjustChangeForAnchors: Adjusts change to meet target fee rate
// -----------------------------------------------------------------------------

// payToAnchorPkScript returns the standard P2A witness program.
func payToAnchorPkScript() []byte {
	// Bitcoin Core recognises OP_1 <0x4e73> as the standard pay-to-anchor
	// witness program. It is still anyone-can-spend, but unlike the taproot
	// OP_TRUE variant it results in a keyless v1 witness program. Using it
	// keeps the anchor weight minimal and matches the policy checks
	// enforced for ephemeral packages.
	return []byte{
		txscript.OP_1, txscript.OP_DATA_2, 0x4e, 0x73,
	}
}

// BuildAnchorChild assembles a CPFP child transaction that spends every zero
// value BTC anchor associated with the builder.
func (b *AssetTxBuilder) BuildAnchorChild(ctx context.Context,
	wallet AnchorFundingWallet, opts AnchorChildOptions) (*psbt.Packet,
	*wire.MsgTx, error) {

	// Sanity-check the inputs: we need a wallet to fund the CPFP child, a
	// change destination for the wallet to target, and an anchor PSBT that
	// has already been compiled/committed so we can reference the
	// zero-value outputs.
	if wallet == nil {
		return nil, nil, errors.New("funding wallet missing")
	}
	if opts.ChangeAddress == nil {
		return nil, nil, errors.New("change address missing")
	}
	if opts.FeeRate <= 0 {
		return nil, nil, errors.New("fee rate must be greater than " +
			"zero")
	}
	if b.anchorPsbt == nil {
		return nil, nil, errors.New("anchor psbt not available")
	}
	if b.ephemeralAnchorPlan == nil {
		return nil, nil, errors.New("no ephemeral anchor configured")
	}

	parentTx := b.anchorPsbt.UnsignedTx
	parentHash := parentTx.TxHash()

	// Start with an empty v3 transaction that only contains a change
	// output. The wallet will populate inputs that supply the actual fees.
	child := &psbt.Packet{UnsignedTx: wire.NewMsgTx(3)}
	changeScript, err := txscript.PayToAddrScript(opts.ChangeAddress)
	if err != nil {
		return nil, nil, fmt.Errorf("change address script: %w", err)
	}
	child.UnsignedTx.AddTxOut(wire.NewTxOut(1, changeScript))
	child.Outputs = append(child.Outputs, psbt.POutput{})

	changeIndex := len(child.Outputs) - 1
	funded, err := wallet.FundPsbt(ctx, child, changeIndex, opts.FeeRate)
	if err != nil {
		return nil, nil, fmt.Errorf("fund child psbt: %w", err)
	}
	funded.UnsignedTx.Version = 3
	for i := range funded.UnsignedTx.TxIn {
		funded.UnsignedTx.TxIn[i].Sequence = wire.MaxTxInSequenceNum - 2
	}

	if changeIndex < 0 || changeIndex >= len(funded.UnsignedTx.TxOut) {
		return nil, nil, fmt.Errorf("wallet returned invalid change "+
			"index %d", changeIndex)
	}
	funded.UnsignedTx.TxOut[changeIndex].PkScript = append(
		[]byte(nil), changeScript...,
	)

	if err := adjustChangeForAnchor(
		funded, parentTx, b.ephemeralAnchorPlan, changeIndex,
		opts.FeeRate,
	); err != nil {
		return nil, nil, err
	}

	walletInputCount := len(funded.Inputs)

	// Import the anchor as a PSBT input so the wallet's signatures will
	// commit to it and so we can attach the appropriate witness data later.
	anchorOut := parentTx.TxOut[b.ephemeralAnchorPlan.OutputIndex]
	prev := wire.OutPoint{
		Hash:  parentHash,
		Index: uint32(b.ephemeralAnchorPlan.OutputIndex),
	}

	addTRUCInput(funded.UnsignedTx, prev)
	funded.Inputs = append(
		funded.Inputs, buildEphemeralAnchorInput(anchorOut),
	)

	// Ask the wallet to sign its inputs. Anchor inputs have no wallet key
	// material, so we will populate their witnesses by hand below.
	signed, err := wallet.SignPsbt(ctx, funded)
	if err != nil {
		return nil, nil, fmt.Errorf("sign child psbt: %w", err)
	}

	for i := 0; i < walletInputCount; i++ {
		if len(signed.Inputs[i].FinalScriptWitness) == 0 &&
			len(signed.Inputs[i].TaprootKeySpendSig) > 0 {

			sig := append(
				[]byte(nil),
				signed.Inputs[i].TaprootKeySpendSig...,
			)
			if err := applyWitness(
				signed, i, wire.TxWitness{sig},
			); err != nil {
				return nil, nil, fmt.Errorf("serialize wallet "+
					"witness: %w", err)
			}
		}
	}

	// P2A anchor requires an empty witness.
	emptyWitness := wire.TxWitness{}
	anchorIdx := walletInputCount
	if err := applyWitness(signed, anchorIdx, emptyWitness); err != nil {
		return nil, nil, fmt.Errorf("serialize anchor witness: %w", err)
	}

	// Finalize and extract the signed transaction. At this point every
	// input has a witness, so we can compute the fee and hand the package
	// to the caller.
	if err := psbt.MaybeFinalizeAll(signed); err != nil {
		return nil, nil, fmt.Errorf("finalize child psbt: %w", err)
	}

	childTx, err := psbt.Extract(signed)
	if err != nil {
		return nil, nil, fmt.Errorf("extract child tx: %w", err)
	}

	var totalInputs, totalOutputs btcutil.Amount
	for i, in := range signed.Inputs {
		switch {
		case in.WitnessUtxo != nil:
			totalInputs += btcutil.Amount(in.WitnessUtxo.Value)

		case in.NonWitnessUtxo != nil:
			prevIdx :=
				signed.UnsignedTx.TxIn[i].PreviousOutPoint.Index
			totalInputs += btcutil.Amount(
				in.NonWitnessUtxo.TxOut[prevIdx].Value,
			)

		default:
			return nil, nil, fmt.Errorf("child input %d missing "+
				"utxo data", i)
		}
	}
	for _, txOut := range childTx.TxOut {
		totalOutputs += btcutil.Amount(txOut.Value)
	}
	if totalInputs <= totalOutputs {
		return nil, nil, errors.New("child tx has non-positive fee")
	}

	return signed, childTx, nil
}

// buildEphemeralAnchorInput creates a psbt.PInput for spending a P2A anchor.
func buildEphemeralAnchorInput(anchorOut *wire.TxOut) psbt.PInput {
	return psbt.PInput{
		WitnessUtxo: &wire.TxOut{
			Value:    anchorOut.Value,
			PkScript: append([]byte(nil), anchorOut.PkScript...),
		},
	}
}

// adjustChangeForAnchor adjusts the change output value to account for the
// weight of the anchor input that will be added to the CPFP child transaction.
func adjustChangeForAnchor(packet *psbt.Packet, parentTx *wire.MsgTx,
	plan *EphemeralAnchorPlan, changeIndex int,
	feeRate chainfee.SatPerKWeight) error {

	probe, err := clonePsbt(packet)
	if err != nil {
		return fmt.Errorf("clone psbt: %w", err)
	}

	if plan.OutputIndex < 0 || plan.OutputIndex >= len(parentTx.TxOut) {
		return fmt.Errorf("anchor output index %d invalid",
			plan.OutputIndex)
	}

	parentHash := parentTx.TxHash()
	anchorOut := parentTx.TxOut[plan.OutputIndex]
	prev := wire.OutPoint{
		Hash:  parentHash,
		Index: uint32(plan.OutputIndex),
	}

	addProbeInput(probe.UnsignedTx, prev)
	probe.Inputs = append(
		probe.Inputs, buildEphemeralAnchorInput(anchorOut),
	)

	var totalInputs, totalOutputs btcutil.Amount
	for i, in := range probe.Inputs {
		switch {
		case in.WitnessUtxo != nil:
			totalInputs += btcutil.Amount(in.WitnessUtxo.Value)

		case in.NonWitnessUtxo != nil:
			prevIdx :=
				probe.UnsignedTx.TxIn[i].PreviousOutPoint.Index
			totalInputs += btcutil.Amount(
				in.NonWitnessUtxo.TxOut[prevIdx].Value,
			)

		default:
			return fmt.Errorf("child input %d missing utxo data", i)
		}
	}

	for _, txOut := range probe.UnsignedTx.TxOut {
		totalOutputs += btcutil.Amount(txOut.Value)
	}

	weight := blockchain.GetTransactionWeight(
		btcutil.NewTx(probe.UnsignedTx),
	)
	requiredFee := feeRate.FeeForWeight(lntypes.WeightUnit(weight))
	currentFee := totalInputs - totalOutputs
	if currentFee < requiredFee {
		delta := requiredFee - currentFee
		changeValue := btcutil.Amount(
			packet.UnsignedTx.TxOut[changeIndex].Value,
		)

		if changeValue <= delta {
			return errors.New("insufficient change to reach " +
				"target fee")
		}

		packet.UnsignedTx.TxOut[changeIndex].Value -= int64(delta)
	}

	return nil
}
