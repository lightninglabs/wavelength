package rounds

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/lib/tx"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightningnetwork/lnd/input"
)

// SpentVTXO captures a forfeited VTXO outpoint and its completed forfeit
// transaction.
type SpentVTXO struct {
	// VTXOOutpoint identifies the forfeited VTXO.
	VTXOOutpoint wire.OutPoint

	// ForfeitTx is the completed forfeit transaction.
	ForfeitTx *wire.MsgTx
}

// completeForfeitTxs completes forfeit transactions by adding the server's
// signatures for both the VTXO input (collaborative path) and the connector
// input (operator-only keyspend). Returns spent VTXO information with
// completed forfeit transactions.
func completeForfeitTxs(forfeitTxSigs []*types.ForfeitTxSig,
	reg *ClientRegistration,
	connectorAssignments map[wire.OutPoint]*ConnectorLeafAssignment,
	env *Environment) ([]*SpentVTXO, error) {

	forfeitInputs := make(map[wire.OutPoint]*ForfeitInput)
	for _, input := range reg.ForfeitInputs {
		if input.Outpoint == nil {
			return nil, fmt.Errorf("forfeit outpoint cannot be nil")
		}

		forfeitInputs[*input.Outpoint] = input
	}

	spentVTXOs := make([]*SpentVTXO, 0, len(forfeitTxSigs))

	for _, forfeitTxSig := range forfeitTxSigs {
		if forfeitTxSig.UnsignedTx == nil {
			return nil, fmt.Errorf("forfeit tx cannot be nil")
		}

		if forfeitTxSig.ClientVTXOSig == nil {
			return nil, fmt.Errorf(
				"client VTXO signature cannot be nil",
			)
		}

		ftx := forfeitTxSig.UnsignedTx
		if len(ftx.TxIn) != 2 {
			return nil, fmt.Errorf(
				"forfeit tx must have 2 inputs, got %d",
				len(ftx.TxIn),
			)
		}

		vtxoOutpoint :=
			ftx.TxIn[tx.ForfeitVTXOInputIndex].PreviousOutPoint
		forfeitInput, exists := forfeitInputs[vtxoOutpoint]
		if !exists {
			return nil, fmt.Errorf("forfeit input not found for "+
				"VTXO %v", vtxoOutpoint)
		}

		assignment, exists := connectorAssignments[vtxoOutpoint]
		if !exists {
			return nil, fmt.Errorf("no connector assignment for "+
				"VTXO %v", vtxoOutpoint)
		}

		if assignment.LeafOutput == nil {
			return nil, fmt.Errorf("connector leaf output missing "+
				"for VTXO %v", vtxoOutpoint)
		}

		connectorInput := ftx.TxIn[tx.ForfeitConnectorInputIndex]
		if connectorInput.PreviousOutPoint != assignment.LeafOutpoint {
			return nil, fmt.Errorf("forfeit tx for VTXO %v "+
				"references wrong connector leaf: expected "+
				"%v, got %v", vtxoOutpoint,
				assignment.LeafOutpoint,
				connectorInput.PreviousOutPoint)
		}

		if err := signForfeitVTXOInput(
			ftx, forfeitInput, forfeitTxSig.ClientVTXOSig,
			assignment.LeafOutput, env,
		); err != nil {
			return nil, fmt.Errorf("failed to sign VTXO input for "+
				"%v: %w", vtxoOutpoint, err)
		}

		vtxoOutput := &wire.TxOut{
			Value:    int64(forfeitInput.VTXO.Descriptor.Amount),
			PkScript: forfeitInput.VTXO.Descriptor.PkScript,
		}
		if err := signForfeitConnectorInput(
			ftx, vtxoOutput, assignment.LeafOutput, env,
		); err != nil {
			return nil, fmt.Errorf("failed to sign connector "+
				"input for %v: %w", vtxoOutpoint, err)
		}

		spentVTXOs = append(spentVTXOs, &SpentVTXO{
			VTXOOutpoint: vtxoOutpoint,
			ForfeitTx:    ftx,
		})
	}

	return spentVTXOs, nil
}

// signForfeitVTXOInput adds the server's signature to the VTXO input in a
// forfeit transaction. The VTXO is spent via the collaborative tapscript path
// which requires both client and operator signatures.
func signForfeitVTXOInput(ftx *wire.MsgTx, forfeitInput *ForfeitInput,
	clientSig *schnorr.Signature, connectorOutput *wire.TxOut,
	env *Environment) error {

	if forfeitInput == nil || forfeitInput.VTXO == nil {
		return fmt.Errorf("forfeit VTXO must be provided")
	}

	vtxo := forfeitInput.VTXO
	vtxoOutpoint := *forfeitInput.Outpoint

	vtxoOutput := &wire.TxOut{
		Value:    int64(vtxo.Descriptor.Amount),
		PkScript: vtxo.Descriptor.PkScript,
	}

	connectorOutpoint :=
		ftx.TxIn[tx.ForfeitConnectorInputIndex].PreviousOutPoint

	vtxoTapScript, err := scripts.VTXOTapScript(
		vtxo.Descriptor.CoSignerKey, env.Terms.OperatorKey.PubKey,
		env.Terms.VTXOExitDelay,
	)
	if err != nil {
		return fmt.Errorf("failed to reconstruct VTXO tapscript: %w",
			err)
	}

	vtxoCtx := &tx.VTXOSpendContext{
		Outpoint:  vtxoOutpoint,
		Output:    vtxoOutput,
		TapScript: vtxoTapScript,
	}

	connectorCtx := &tx.ConnectorSpendContext{
		Outpoint: connectorOutpoint,
		Output:   connectorOutput,
	}

	prevOutFetcher, err := tx.NewForfeitPrevOutFetcher(
		vtxoCtx, connectorCtx,
	)
	if err != nil {
		return fmt.Errorf("failed to create prev out fetcher: %w",
			err)
	}

	sigHashes := txscript.NewTxSigHashes(ftx, prevOutFetcher)

	signDesc, spendInfo, err := tx.NewVTXOCollabSignDescriptor(
		vtxoCtx, env.Terms.OperatorKey,
		tx.ForfeitVTXOInputIndex, sigHashes, prevOutFetcher,
	)
	if err != nil {
		return fmt.Errorf("failed to create VTXO sign descriptor: %w",
			err)
	}

	serverSig, err := env.WalletController.SignOutputRaw(
		ftx, signDesc,
	)
	if err != nil {
		return fmt.Errorf("failed to sign VTXO input: %w", err)
	}

	witness, err := scripts.VTXOCollabSpendWitness(
		clientSig, serverSig, spendInfo,
	)
	if err != nil {
		return fmt.Errorf("failed to build witness: %w", err)
	}

	ftx.TxIn[tx.ForfeitVTXOInputIndex].Witness = witness

	return nil
}

// signForfeitConnectorInput signs the connector input in a forfeit
// transaction. The connector is spent via keyspend (operator-only). This
// assumes the VTXO input witness has already been set.
func signForfeitConnectorInput(ftx *wire.MsgTx, vtxoOutput *wire.TxOut,
	connectorLeafOutput *wire.TxOut, env *Environment) error {

	vtxoOutpoint :=
		ftx.TxIn[tx.ForfeitVTXOInputIndex].PreviousOutPoint
	connectorOutpoint :=
		ftx.TxIn[tx.ForfeitConnectorInputIndex].PreviousOutPoint

	prevOutFetcher := txscript.NewMultiPrevOutFetcher(
		map[wire.OutPoint]*wire.TxOut{
			vtxoOutpoint:      vtxoOutput,
			connectorOutpoint: connectorLeafOutput,
		},
	)
	sigHashes := txscript.NewTxSigHashes(ftx, prevOutFetcher)

	connectorSignDesc := &input.SignDescriptor{
		KeyDesc:           env.Terms.OperatorKey,
		Output:            connectorLeafOutput,
		HashType:          txscript.SigHashDefault,
		InputIndex:        tx.ForfeitConnectorInputIndex,
		SignMethod:        input.TaprootKeySpendBIP0086SignMethod,
		SigHashes:         sigHashes,
		PrevOutputFetcher: prevOutFetcher,
		TapTweak:          []byte{},
	}

	connectorSig, err := env.WalletController.SignOutputRaw(
		ftx, connectorSignDesc,
	)
	if err != nil {
		return fmt.Errorf("failed to sign connector input: %w", err)
	}

	ftx.TxIn[tx.ForfeitConnectorInputIndex].Witness = wire.TxWitness{
		connectorSig.Serialize(),
	}

	return nil
}
