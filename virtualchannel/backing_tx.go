package virtualchannel

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

// BuildBackingTx builds the deterministic VTXO-to-channel-point transaction.
// The returned transaction is unsigned; negotiation must add the collaborative
// VTXO spend witnesses before the registration is persisted.
func BuildBackingTx(backingVTXOs []BackingVTXO, fundingOutput *wire.TxOut) (
	*wire.MsgTx, btcutil.Amount, error) {

	if len(backingVTXOs) != 1 {
		return nil, 0, fmt.Errorf("exactly one backing VTXO is " +
			"required")
	}
	if fundingOutput == nil {
		return nil, 0, fmt.Errorf("funding output is nil")
	}
	if fundingOutput.Value <= 0 {
		return nil, 0, fmt.Errorf("funding output value must be " +
			"positive")
	}
	if fundingOutput.Value > int64(btcutil.MaxSatoshi) {
		return nil, 0, fmt.Errorf("funding output exceeds Bitcoin " +
			"money supply")
	}
	if len(fundingOutput.PkScript) == 0 {
		return nil, 0, fmt.Errorf("funding output script is empty")
	}

	tx := wire.NewMsgTx(2)
	var total btcutil.Amount
	for _, input := range backingVTXOs {
		if input.Amount <= 0 {
			return nil, 0, fmt.Errorf("backing VTXO %s amount "+
				"must be positive", input.OutPoint)
		}
		if input.Amount > btcutil.MaxSatoshi {
			return nil, 0, fmt.Errorf("backing VTXO %s exceeds "+
				"Bitcoin money supply", input.OutPoint)
		}

		total += input.Amount
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: input.OutPoint,
			Sequence:         wire.MaxTxInSequenceNum,
		})
	}

	outputValue := btcutil.Amount(fundingOutput.Value)
	if total < outputValue {
		return nil, 0, fmt.Errorf("backing total %d sats below "+
			"funding output %d sats", total, outputValue)
	}

	tx.AddTxOut(&wire.TxOut{
		Value:    fundingOutput.Value,
		PkScript: append([]byte(nil), fundingOutput.PkScript...),
	})

	return tx, total - outputValue, nil
}

// ValidateBackingTemplate verifies that the backing transaction spends the
// bound VTXO into the exact funding output negotiated with lnd. It deliberately
// does not require a witness so callers can validate before signing.
func ValidateBackingTemplate(reg Registration, backingTx *wire.MsgTx) error {
	if backingTx == nil {
		return fmt.Errorf("virtual channel backing tx is nil")
	}
	if len(reg.BackingVTXOs) != 1 {
		return fmt.Errorf("virtual channel requires exactly one " +
			"backing VTXO")
	}
	if len(backingTx.TxIn) != 1 {
		return fmt.Errorf("backing tx requires exactly one input")
	}
	if len(backingTx.TxOut) != 1 {
		return fmt.Errorf("backing tx requires exactly one output")
	}

	backing := reg.BackingVTXOs[0]
	if backing.Amount <= 0 || len(backing.PkScript) == 0 {
		return fmt.Errorf("bound VTXO descriptor is incomplete")
	}
	if backingTx.TxIn[0].PreviousOutPoint != backing.OutPoint {
		return fmt.Errorf("backing tx input does not match bound VTXO")
	}
	if backingTx.TxHash() != reg.ChannelPoint.Hash {
		return fmt.Errorf("backing txid does not match channel point")
	}
	if reg.ChannelPoint.Index >= uint32(len(backingTx.TxOut)) {
		return fmt.Errorf("channel point output index is out of range")
	}

	packet, err := psbt.NewFromRawBytes(
		bytes.NewReader(reg.FundingPsbt), false,
	)
	if err != nil {
		return fmt.Errorf("parse funding PSBT: %w", err)
	}
	fundingOutput, outputIndex, err := fundingOutputFromPSBT(
		packet, int64(reg.Capacity),
	)
	if err != nil {
		return err
	}
	if outputIndex != reg.ChannelPoint.Index {
		return fmt.Errorf("funding PSBT output does not match " +
			"channel point")
	}
	actualOutput := backingTx.TxOut[reg.ChannelPoint.Index]
	if actualOutput.Value != fundingOutput.Value ||
		!bytes.Equal(actualOutput.PkScript, fundingOutput.PkScript) {
		return fmt.Errorf("backing tx output does not match lnd " +
			"funding output")
	}

	return nil
}

// ValidateBackingProof verifies the complete artifact that arms a virtual
// channel. V1 deliberately permits exactly one VTXO and one funding output.
func ValidateBackingProof(reg Registration, signedTx *wire.MsgTx) error {
	if err := ValidateBackingTemplate(reg, signedTx); err != nil {
		return err
	}

	backing := reg.BackingVTXOs[0]
	if len(signedTx.TxIn[0].Witness) == 0 {
		return fmt.Errorf("backing tx input has no witness")
	}

	prevOut := &wire.TxOut{
		Value:    int64(backing.Amount),
		PkScript: bytes.Clone(backing.PkScript),
	}
	prevFetcher := txscript.NewMultiPrevOutFetcher(
		map[wire.OutPoint]*wire.TxOut{
			backing.OutPoint: prevOut,
		},
	)
	sigHashes := txscript.NewTxSigHashes(signedTx, prevFetcher)
	engine, err := txscript.NewEngine(
		prevOut.PkScript, signedTx, 0, txscript.StandardVerifyFlags,
		nil, sigHashes, prevOut.Value, prevFetcher,
	)
	if err != nil {
		return fmt.Errorf("build backing script engine: %w", err)
	}
	if err := engine.Execute(); err != nil {
		return fmt.Errorf("verify backing VTXO witness: %w", err)
	}

	return nil
}
