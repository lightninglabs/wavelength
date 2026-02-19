package chainresolver

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

const (
	// estimatedChildVsize is a conservative estimate of the CPFP
	// child transaction's virtual size in vbytes. This covers:
	// ~41 vB for the P2A anchor input, ~58 vB for one P2TR wallet
	// input, ~43 vB for one change output, and ~58 vB for tx
	// overhead. Used to inflate the fee rate so the child pays
	// enough fee to cover both parent and child weight.
	estimatedChildVsize = 200

	// cpfpTxVersion is the transaction version used for CPFP child
	// transactions. Version 3 enables opt-in topologically restricted
	// until confirmation (TRUC) policy, which is required for
	// ephemeral anchor spending.
	cpfpTxVersion = 3

	// cpfpMinConfs is the minimum number of confirmations required
	// for wallet UTXOs used to fund CPFP child transactions.
	cpfpMinConfs = 1
)

// findAnchorOutput scans the outputs of a transaction for a P2A
// ephemeral anchor output (BIP 431). Returns the output index if
// found, or -1 if no anchor output is present.
func findAnchorOutput(tx *wire.MsgTx) int {
	if tx == nil {
		return -1
	}

	for i, out := range tx.TxOut {
		if bytes.Equal(out.PkScript, scripts.AnchorPkScript) {
			return i
		}
	}

	return -1
}

// buildCPFPChildPSBT creates a PSBT template for a CPFP child
// transaction that spends the P2A anchor output of the given parent
// transaction. The PSBT has a single input (the anchor, marked as
// external with WitnessUtxo set) and no outputs. FundPsbt will add
// wallet UTXOs and a change output during coin selection.
func buildCPFPChildPSBT(parentTx *wire.MsgTx,
	anchorIdx int) (*psbt.Packet, error) {

	if parentTx == nil {
		return nil, fmt.Errorf("parent tx is nil")
	}

	if anchorIdx < 0 || anchorIdx >= len(parentTx.TxOut) {
		return nil, fmt.Errorf(
			"anchor index %d out of range [0, %d)",
			anchorIdx, len(parentTx.TxOut),
		)
	}

	parentTxid := parentTx.TxHash()

	// Build the unsigned child transaction with the anchor as its
	// only input.
	anchorOutpoint := wire.OutPoint{
		Hash:  parentTxid,
		Index: uint32(anchorIdx),
	}

	txIn := []*wire.OutPoint{&anchorOutpoint}
	sequences := []uint32{wire.MaxTxInSequenceNum}

	// Create the PSBT with no outputs. FundPsbt will add wallet
	// inputs and a change output during coin selection.
	pkt, err := psbt.New(
		txIn, nil, cpfpTxVersion, 0, sequences,
	)
	if err != nil {
		return nil, fmt.Errorf("create PSBT: %w", err)
	}

	// Mark the anchor input as external by setting its WitnessUtxo.
	// This tells FundPsbt that this input exists but has 0 value,
	// so the wallet must fund the entire fee from its own UTXOs.
	pkt.Inputs[0].WitnessUtxo = scripts.AnchorOutput()

	return pkt, nil
}

// finalizeAnchorInput sets the FinalScriptWitness for a P2A anchor
// input to an empty witness stack. P2A outputs are segwit v1 with a
// 2-byte witness program (0x4e73), which has no consensus spending
// rules beyond the segwit version check. The witness must be empty
// (zero items). This must be called before FinalizePsbt so the
// wallet knows to skip this input during signing.
func finalizeAnchorInput(pkt *psbt.Packet, anchorInputIdx int) error {
	if pkt == nil {
		return fmt.Errorf("PSBT is nil")
	}

	if anchorInputIdx < 0 || anchorInputIdx >= len(pkt.Inputs) {
		return fmt.Errorf(
			"anchor input index %d out of range [0, %d)",
			anchorInputIdx, len(pkt.Inputs),
		)
	}

	// An empty witness stack serializes as a single byte: varint 0
	// (0x00) indicating zero witness items.
	pkt.Inputs[anchorInputIdx].FinalScriptWitness = []byte{0x00}

	return nil
}

// computeAdjustedFeeRate inflates the target fee rate so that the
// CPFP child pays enough fee to cover both its own weight and the
// parent's weight. The effective package fee rate will meet the
// target because:
//
//	child_fee = adjustedRate * childVsize
//	          = targetRate * (parentVsize + estChildVsize) / estChildVsize * childVsize
//	          ≈ targetRate * (parentVsize + childVsize)
//
// This slightly overpays when the estimated child vsize differs from
// the actual value, but the miner benefits and the overpayment is
// bounded.
func computeAdjustedFeeRate(parentTx *wire.MsgTx,
	targetSatPerVByte btcutil.Amount) chainfee.SatPerKWeight {

	parentWeight := parentTx.SerializeSize()
	parentVsize := (parentWeight + 3) / 4

	// Inflate the fee rate so the child covers the parent weight.
	adjustedSatPerVByte := targetSatPerVByte *
		btcutil.Amount(parentVsize+estimatedChildVsize) /
		btcutil.Amount(estimatedChildVsize)

	// Ensure a minimum of 1 sat/vB to avoid zero-fee children.
	if adjustedSatPerVByte < 1 {
		adjustedSatPerVByte = 1
	}

	// Convert sat/vB to sat/kW for the wallet's FundPsbt method.
	satPerKVByte := chainfee.SatPerKVByte(
		adjustedSatPerVByte * 1000,
	)

	return satPerKVByte.FeePerKWeight()
}

// buildCPFPChild constructs a signed CPFP child transaction that
// spends the P2A anchor output of parentTx and pays enough fee to
// cover the entire parent+child package at the given fee rate. The
// wallet handles coin selection (adding UTXOs) and signing.
func buildCPFPChild(ctx context.Context, wallet CPFPWallet,
	parentTx *wire.MsgTx,
	feeRateSatPerVByte btcutil.Amount) (*wire.MsgTx, error) {

	// Locate the P2A anchor output in the parent.
	anchorIdx := findAnchorOutput(parentTx)
	if anchorIdx < 0 {
		return nil, fmt.Errorf("parent tx has no P2A anchor output")
	}

	// Build the PSBT template with the anchor as an external input.
	pkt, err := buildCPFPChildPSBT(parentTx, anchorIdx)
	if err != nil {
		return nil, fmt.Errorf("build CPFP child PSBT: %w", err)
	}

	// Compute the inflated fee rate that covers both parent and
	// child weight.
	adjustedRate := computeAdjustedFeeRate(
		parentTx, feeRateSatPerVByte,
	)

	// Fund the child with wallet UTXOs. The wallet adds inputs to
	// cover the fee and a change output for the remainder.
	_, err = wallet.FundPsbt(
		ctx, pkt, cpfpMinConfs, adjustedRate, "",
	)
	if err != nil {
		return nil, fmt.Errorf("fund CPFP child: %w", err)
	}

	// Pre-finalize the P2A anchor input with an empty witness.
	// This must happen before FinalizePsbt so the wallet knows to
	// skip this input when signing.
	if err := finalizeAnchorInput(pkt, 0); err != nil {
		return nil, fmt.Errorf("finalize anchor input: %w", err)
	}

	// Sign all wallet-controlled inputs and extract the final tx.
	childTx, err := wallet.FinalizePsbt(ctx, pkt)
	if err != nil {
		return nil, fmt.Errorf("finalize CPFP child: %w", err)
	}

	return childTx, nil
}
