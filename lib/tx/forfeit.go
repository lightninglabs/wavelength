package tx

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

const (
	// ForfeitVTXOInputIndex is the index of the VTXO input in the
	// forfeit tx.
	ForfeitVTXOInputIndex = 0

	// ForfeitConnectorInputIndex is the index of the connector input.
	ForfeitConnectorInputIndex = 1

	// ForfeitPenaltyOutputIndex is the index of the penalty output that
	// pays to the server's forfeit address.
	ForfeitPenaltyOutputIndex = 0

	// ForfeitAnchorOutputIndex is the index of the ephemeral anchor output
	// for fee bumping via CPFP.
	ForfeitAnchorOutputIndex = 1
)

// VTXOSpendContext describes the VTXO being spent.
type VTXOSpendContext struct {
	// Outpoint is the outpoint of the VTXO output being spent.
	Outpoint wire.OutPoint

	// Output is the transaction output containing the VTXO script and
	// amount.
	Output *wire.TxOut

	// TapScript contains the taproot script details for the VTXO,
	// including the internal key and all script paths.
	TapScript *waddrmgr.Tapscript
}

// ConnectorSpendContext describes the connector input being spent.
type ConnectorSpendContext struct {
	// Outpoint is the outpoint of the connector output being spent.
	Outpoint wire.OutPoint

	// Output is the transaction output containing the connector script and
	// amount (typically dust).
	Output *wire.TxOut
}

// ForfeitTxContext describes the tx-level requirements for a specific VTXO
// spend path used by a forfeit transaction.
type ForfeitTxContext struct {
	// VTXOSequence is the nSequence value for the VTXO input.
	VTXOSequence uint32

	// LockTime is the transaction nLockTime required by the chosen leaf.
	LockTime uint32
}

// BuildForfeitTx creates a 2-input (VTXO + connector), 2-output (penalty +
// anchor) transaction.
func BuildForfeitTx(vtxoOutpoint *wire.OutPoint, vtxoAmount btcutil.Amount,
	connectorOutpoint *wire.OutPoint, connectorAmount btcutil.Amount,
	serverForfeitScript []byte) (*wire.MsgTx, error) {

	return BuildForfeitTxWithContext(
		vtxoOutpoint, vtxoAmount, connectorOutpoint, connectorAmount,
		serverForfeitScript, ForfeitTxContext{
			VTXOSequence: wire.MaxTxInSequenceNum,
		},
	)
}

// BuildForfeitTxWithContext creates a forfeit transaction using the supplied
// tx-level requirements for the VTXO input spend path.
func BuildForfeitTxWithContext(vtxoOutpoint *wire.OutPoint,
	vtxoAmount btcutil.Amount, connectorOutpoint *wire.OutPoint,
	connectorAmount btcutil.Amount, serverForfeitScript []byte,
	ctx ForfeitTxContext) (*wire.MsgTx, error) {

	switch {
	case vtxoOutpoint == nil:
		return nil, fmt.Errorf("vtxo outpoint cannot be nil")

	case connectorOutpoint == nil:
		return nil, fmt.Errorf("connector outpoint cannot be nil")

	case len(serverForfeitScript) == 0:
		return nil, fmt.Errorf("server forfeit script cannot be empty")

	case vtxoAmount <= 0:
		return nil, fmt.Errorf("vtxo amount must be positive, got %d",
			vtxoAmount)

	case connectorAmount < 0:
		return nil, fmt.Errorf("connector amount must be "+
			"non-negative, got %d", connectorAmount)
	}

	tx := wire.NewMsgTx(3)
	tx.LockTime = ctx.LockTime

	vtxoSequence := ctx.VTXOSequence
	if vtxoSequence == 0 {
		vtxoSequence = wire.MaxTxInSequenceNum
	}

	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: *vtxoOutpoint,
		Sequence:         vtxoSequence,
	})

	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: *connectorOutpoint,
		Sequence:         wire.MaxTxInSequenceNum,
	})

	tx.AddTxOut(&wire.TxOut{
		Value:    int64(vtxoAmount + connectorAmount),
		PkScript: serverForfeitScript,
	})

	tx.AddTxOut(arkscript.AnchorOutput())

	return tx, nil
}

// NewForfeitPrevOutFetcher builds a prev-output fetcher for the two inputs.
func NewForfeitPrevOutFetcher(vtxo *VTXOSpendContext,
	connector *ConnectorSpendContext) (txscript.PrevOutputFetcher, error) {

	switch {
	case vtxo == nil || vtxo.Output == nil:
		return nil, fmt.Errorf("vtxo context must be provided")

	case connector == nil || connector.Output == nil:
		return nil, fmt.Errorf("connector context must be provided")
	}

	return txscript.NewMultiPrevOutFetcher(map[wire.OutPoint]*wire.TxOut{
		vtxo.Outpoint:      vtxo.Output,
		connector.Outpoint: connector.Output,
	}), nil
}

// NewVTXOCollabSignDescriptor returns the sign descriptor + spend info for a
// collaborative VTXO spend.
func NewVTXOCollabSignDescriptor(vtxo *VTXOSpendContext,
	keyDesc keychain.KeyDescriptor, inputIndex int,
	sigHashes *txscript.TxSigHashes,
	prevFetcher txscript.PrevOutputFetcher) (*input.SignDescriptor,
	*arkscript.SpendInfo, error) {

	if vtxo == nil || vtxo.TapScript == nil {
		return nil, nil, fmt.Errorf("vtxo tapscript must be provided")
	}

	// Derive the collaborative spend info from the tapscript. The
	// collab leaf is always at index 0 in the VTXO tap tree.
	const collabLeafIndex = 0

	if len(vtxo.TapScript.Leaves) <= collabLeafIndex {
		return nil, nil, fmt.Errorf("tapscript has no collab leaf")
	}

	targetLeaf := vtxo.TapScript.Leaves[collabLeafIndex]

	tapTree := txscript.AssembleTaprootScriptTree(
		vtxo.TapScript.Leaves...,
	)
	if len(tapTree.LeafMerkleProofs) <= collabLeafIndex {
		return nil, nil, fmt.Errorf("missing taproot proof for vtxo " +
			"collab leaf")
	}

	controlBlock := tapTree.LeafMerkleProofs[collabLeafIndex].
		ToControlBlock(
			&arkscript.ARKNUMSKey,
		)
	ctrlBytes, err := controlBlock.ToBytes()
	if err != nil {
		return nil, nil, fmt.Errorf("unable to encode control "+
			"block: %w", err)
	}

	spendInfo := &arkscript.SpendInfo{
		WitnessScript: targetLeaf.Script,
		ControlBlock:  ctrlBytes,
	}

	signDesc := &input.SignDescriptor{
		KeyDesc:           keyDesc,
		WitnessScript:     spendInfo.WitnessScript,
		Output:            vtxo.Output,
		HashType:          txscript.SigHashDefault,
		InputIndex:        inputIndex,
		SignMethod:        input.TaprootScriptSpendSignMethod,
		SigHashes:         sigHashes,
		PrevOutputFetcher: prevFetcher,
		ControlBlock:      spendInfo.ControlBlock,
	}

	return signDesc, spendInfo, nil
}

// ForfeitTxParams contains the expected parameters for validating a forfeit
// transaction structure. This is used to verify that a forfeit tx built by a
// VTXO actor matches the expected structure before submitting to the server.
type ForfeitTxParams struct {
	// VTXOOutpoint is the expected VTXO input outpoint.
	VTXOOutpoint wire.OutPoint

	// ConnectorOutpoint is the expected connector input outpoint.
	ConnectorOutpoint wire.OutPoint

	// ServerForfeitScript is the expected penalty output script.
	ServerForfeitScript []byte

	// ExpectedAmount is the expected value of the penalty output. If zero,
	// the amount check is skipped (useful when the caller doesn't know the
	// exact amount but wants to validate structure).
	ExpectedAmount btcutil.Amount

	// ExpectedSequence is the expected nSequence for the VTXO
	// input. Zero means the default final sequence is expected.
	ExpectedSequence uint32

	// ExpectedLockTime is the expected transaction nLockTime.
	ExpectedLockTime uint32
}

// ValidateForfeitTx verifies that the forfeit transaction has the expected
// structure before signing. This validation is critical because once a VTXO
// actor signs a forfeit tx, they authorize the operator to claim their funds
// if they attempt to exit unilaterally. By validating the structure, we ensure
// the operator cannot construct a malformed transaction that violates the
// protocol rules.
//
// The function checks:
//   - Exactly 2 inputs: VTXO at index 0, connector at index 1
//   - Exactly 2 outputs: penalty at index 0, P2A anchor at index 1
//   - Inputs match expected outpoints
//   - Penalty output pays to server's forfeit script
//   - Anchor output is standard P2A with zero value
func ValidateForfeitTx(forfeitTx *wire.MsgTx, params ForfeitTxParams) error {
	if forfeitTx == nil {
		return fmt.Errorf("forfeit tx is nil")
	}

	// Forfeit tx must have exactly 2 inputs.
	if len(forfeitTx.TxIn) != 2 {
		return fmt.Errorf("forfeit tx has %d inputs, expected 2",
			len(forfeitTx.TxIn))
	}

	// Verify input 0 is the VTXO.
	vtxoIn := forfeitTx.TxIn[ForfeitVTXOInputIndex]
	if vtxoIn.PreviousOutPoint != params.VTXOOutpoint {
		return fmt.Errorf("forfeit tx input %d is %s, expected VTXO %s",
			ForfeitVTXOInputIndex, vtxoIn.PreviousOutPoint,
			params.VTXOOutpoint)
	}

	expectedSequence := params.ExpectedSequence
	if expectedSequence == 0 {
		expectedSequence = wire.MaxTxInSequenceNum
	}
	if vtxoIn.Sequence != expectedSequence {
		return fmt.Errorf("forfeit tx input %d sequence is %d, "+
			"expected %d", ForfeitVTXOInputIndex, vtxoIn.Sequence,
			expectedSequence)
	}

	// Verify input 1 is the connector.
	connectorIn := forfeitTx.TxIn[ForfeitConnectorInputIndex]
	if connectorIn.PreviousOutPoint != params.ConnectorOutpoint {
		return fmt.Errorf("forfeit tx input %d is %s, expected "+
			"connector %s", ForfeitConnectorInputIndex,
			connectorIn.PreviousOutPoint, params.ConnectorOutpoint)
	}

	// Forfeit tx must have exactly 2 outputs (penalty + anchor).
	if len(forfeitTx.TxOut) != 2 {
		return fmt.Errorf("forfeit tx has %d outputs, expected 2",
			len(forfeitTx.TxOut))
	}

	// Verify penalty output pays to server's forfeit address.
	penaltyOut := forfeitTx.TxOut[ForfeitPenaltyOutputIndex]
	if !bytes.Equal(penaltyOut.PkScript, params.ServerForfeitScript) {
		return fmt.Errorf("forfeit tx output %d does not pay to "+
			"server forfeit script", ForfeitPenaltyOutputIndex)
	}

	// Optionally verify the penalty output amount.
	if params.ExpectedAmount > 0 {
		if btcutil.Amount(penaltyOut.Value) != params.ExpectedAmount {
			return fmt.Errorf("forfeit tx penalty output has "+
				"amount %d, expected %d", penaltyOut.Value,
				params.ExpectedAmount)
		}
	}

	if forfeitTx.LockTime != params.ExpectedLockTime {
		return fmt.Errorf("forfeit tx locktime is %d, expected %d",
			forfeitTx.LockTime, params.ExpectedLockTime)
	}

	// Verify anchor output is a standard P2A with zero value.
	anchorOut := forfeitTx.TxOut[ForfeitAnchorOutputIndex]
	if !bytes.Equal(anchorOut.PkScript, arkscript.AnchorPkScript) {
		return fmt.Errorf("forfeit tx output %d is not a P2A anchor",
			ForfeitAnchorOutputIndex)
	}
	if anchorOut.Value != 0 {
		return fmt.Errorf("forfeit tx anchor output has non-zero "+
			"value: %d", anchorOut.Value)
	}

	return nil
}
