package lib

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

const (
	ForfeitVTXOInputIndex      = 0
	ForfeitConnectorInputIndex = 1
)

// ForfeitRequest represents a request to forfeit a VTXO.
type ForfeitRequest struct {
	// VTXOOutpoint is the outpoint of the VTXO to forfeit.
	VTXOOutpoint *wire.OutPoint
}

// BuildForfeitTx creates a version 3 forfeit transaction. A forfeit tx has:
// two inputs:
//   - The vtxo being forfeited
//   - a connector output.
//
// two outputs:
//   - the forfeited amount to the server's forfeit script.
//   - an ephemeral anchor output.
func BuildForfeitTx(vtxoOutpoint *wire.OutPoint,
	vtxoAmount btcutil.Amount,
	connectorOutpoint *wire.OutPoint,
	serverForfeitScript []byte) (*wire.MsgTx, error) {

	// Create version 3 transaction for ephemeral anchors.
	tx := wire.NewMsgTx(3)

	// Add VTXO input (first input).
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: *vtxoOutpoint,
		Sequence:         wire.MaxTxInSequenceNum,
	})

	// Add connector input (second input).
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: *connectorOutpoint,
		Sequence:         wire.MaxTxInSequenceNum,
	})

	// Add the forfeit output.
	tx.AddTxOut(&wire.TxOut{
		Value:    int64(vtxoAmount),
		PkScript: serverForfeitScript,
	})

	// Add Anchor output.
	tx.AddTxOut(AnchorOutput())

	return tx, nil
}

type VTXOSpendContext struct {
	// Outpoint is the outpoint of the VTXO. This locates the vtxo being
	// spent.
	Outpoint *wire.OutPoint

	// Output is the VTXO output. This defines the amount and pkScript of
	// the VTXO being spent.
	Output *wire.TxOut

	// TapScript is the tapscript defines the spend paths of the VTXO.
	TapScript *waddrmgr.Tapscript
}

// ForfeitTxVTXOSignDescriptor builds the sign descriptor for signing the VTXO
// input of a forfeit transaction. This involves building the appropriate
// witness to spend the collaborative tapscript of the VTXO.
func ForfeitTxVTXOSignDescriptor(clientKeyDesc keychain.KeyDescriptor,
	forfeitTx *wire.MsgTx,
	connector *Connector, vtxo *VTXOSpendContext) (*input.SignDescriptor,
	error) {

	// Assemble the taproot script tree.
	tapTree := txscript.AssembleTaprootScriptTree(vtxo.TapScript.Leaves...)
	if len(tapTree.LeafMerkleProofs) != 2 {
		return nil, fmt.Errorf("expected 2 leaves in taproot tree, "+
			"got %d", len(tapTree.LeafMerkleProofs))
	}

	// Get collaborative script.
	collabLeafProof := tapTree.LeafMerkleProofs[VTXOCollabPathLeafIndex]
	collabScript := vtxo.TapScript.Leaves[VTXOCollabPathLeafIndex]

	// Create control block for script path spend.
	controlBlock := collabLeafProof.ToControlBlock(&ARKNUMSKey)
	controlBlockBytes, err := controlBlock.ToBytes()
	if err != nil {
		return nil, err
	}

	// Create prevout map for signature hash calculation.
	prevOutFetcher := txscript.NewMultiPrevOutFetcher(
		map[wire.OutPoint]*wire.TxOut{
			*vtxo.Outpoint:      vtxo.Output,
			*connector.Outpoint: connector.Output,
		},
	)
	sigHashes := txscript.NewTxSigHashes(forfeitTx, prevOutFetcher)

	// Create sign descriptor
	return &input.SignDescriptor{
		KeyDesc:           clientKeyDesc,
		WitnessScript:     collabScript.Script,
		Output:            vtxo.Output,
		HashType:          txscript.SigHashDefault,
		InputIndex:        ForfeitVTXOInputIndex,
		SignMethod:        input.TaprootScriptSpendSignMethod,
		SigHashes:         sigHashes,
		PrevOutputFetcher: prevOutFetcher,
		ControlBlock:      controlBlockBytes,
	}, nil
}
