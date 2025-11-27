package tx

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/closure"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

const (
	// ForfeitVTXOInputIndex is the index of the VTXO input in the
	// forfeit tx.
	ForfeitVTXOInputIndex = 0

	// ForfeitConnectorInputIndex is the index of the connector input.
	ForfeitConnectorInputIndex = 1
)

// VTXOSpendContext describes the VTXO being spent.
type VTXOSpendContext struct {
	// Outpoint is the outpoint of the VTXO output being spent.
	Outpoint wire.OutPoint

	// Output is the transaction output containing the VTXO script and
	// amount.
	Output *wire.TxOut

	// VtxoScript contains the closure-based tapscript for the VTXO,
	// including all script paths (exit and forfeit closures).
	VtxoScript *closure.TapscriptsVtxoScript
}

// ConnectorSpendContext describes the connector input being spent.
type ConnectorSpendContext struct {
	// Outpoint is the outpoint of the connector output being spent.
	Outpoint wire.OutPoint

	// Output is the transaction output containing the connector script and
	// amount (typically dust).
	Output *wire.TxOut
}

// BuildForfeitTx creates a 2-input (VTXO + connector), 2-output (penalty +
// anchor) transaction.
func BuildForfeitTx(vtxoOutpoint *wire.OutPoint, vtxoAmount btcutil.Amount,
	connectorOutpoint *wire.OutPoint,
	serverForfeitScript []byte) (*wire.MsgTx, error) {

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
	}

	tx := wire.NewMsgTx(3)

	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: *vtxoOutpoint,
		Sequence:         wire.MaxTxInSequenceNum,
	})

	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: *connectorOutpoint,
		Sequence:         wire.MaxTxInSequenceNum,
	})

	tx.AddTxOut(&wire.TxOut{
		Value:    int64(vtxoAmount),
		PkScript: serverForfeitScript,
	})

	tx.AddTxOut(scripts.AnchorOutput())

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
// collaborative (forfeit) VTXO spend.
func NewVTXOCollabSignDescriptor(vtxo *VTXOSpendContext,
	keyDesc keychain.KeyDescriptor, inputIndex int,
	sigHashes *txscript.TxSigHashes,
	prevFetcher txscript.PrevOutputFetcher) (*input.SignDescriptor,
	*scripts.VTXOSpendData, error) {

	if vtxo == nil || vtxo.VtxoScript == nil {
		return nil, nil, fmt.Errorf("vtxo script must be provided")
	}

	spendInfo, err := scripts.VtxoCollabSpendInfo(vtxo.VtxoScript)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to derive collaborative "+
			"spend info: %w", err)
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
