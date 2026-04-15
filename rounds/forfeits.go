package rounds

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/tx"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// SpentVTXO captures a forfeited VTXO outpoint and its completed forfeit
// transaction.
type SpentVTXO struct {
	// VTXOOutpoint identifies the forfeited VTXO.
	VTXOOutpoint wire.OutPoint

	// ForfeitInfo records how the VTXO was forfeited.
	ForfeitInfo *ForfeitInfo
}

// completeForfeitTxs completes forfeit transactions by adding the server's
// signatures for both the VTXO input (collaborative path) and the connector
// input (operator-only keyspend). Returns spent VTXO information with
// completed forfeit transactions.
func completeForfeitTxs(forfeitTxSigs []*types.ForfeitTxSig,
	reg *ClientRegistration,
	connectorAssignments map[wire.OutPoint]*ConnectorLeafAssignment,
	walletCtrl WalletController, operatorKey keychain.KeyDescriptor,
	vtxoExitDelay uint32,
	roundID RoundID) ([]*SpentVTXO, error) {

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
		if forfeitTxSig.SpendPath == nil {
			return nil, fmt.Errorf(
				"forfeit spend path cannot be nil",
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
			assignment.LeafOutput, walletCtrl,
			operatorKey, vtxoExitDelay, forfeitTxSig.SpendPath,
		); err != nil {
			return nil, fmt.Errorf("failed to sign VTXO input for "+
				"%v: %w", vtxoOutpoint, err)
		}

		vtxoOutput := &wire.TxOut{
			Value:    int64(forfeitInput.VTXO.Descriptor.Amount),
			PkScript: forfeitInput.VTXO.Descriptor.PkScript,
		}
		if err := signForfeitConnectorInput(
			ftx, vtxoOutput, assignment.LeafOutput,
			walletCtrl, operatorKey,
		); err != nil {
			return nil, fmt.Errorf("failed to sign connector "+
				"input for %v: %w", vtxoOutpoint, err)
		}

		connOutIdx := assignment.ConnectorOutputIndex
		spentVTXOs = append(spentVTXOs, &SpentVTXO{
			VTXOOutpoint: vtxoOutpoint,
			ForfeitInfo: &ForfeitInfo{
				RoundID:              roundID,
				ConnectorOutputIndex: connOutIdx,
				LeafIndex:            assignment.LeafIndex,
				ForfeitTx:            ftx,
			},
		})
	}

	return spentVTXOs, nil
}

// signForfeitVTXOInput adds the server's signature to the VTXO input in a
// forfeit transaction. The VTXO is spent via the collaborative tapscript path
// which requires both client and operator signatures.
func signForfeitVTXOInput(ftx *wire.MsgTx, forfeitInput *ForfeitInput,
	clientSig *schnorr.Signature, connectorOutput *wire.TxOut,
	walletCtrl WalletController,
	operatorKey keychain.KeyDescriptor,
	vtxoExitDelay uint32,
	spendPath *arkscript.SpendPath) error {

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

	if spendPath == nil {
		return fmt.Errorf("forfeit spend path must be provided")
	}
	if err := spendPath.Validate(); err != nil {
		return fmt.Errorf("invalid forfeit spend path: %w", err)
	}

	// Defense in depth: re-check at sign time that the spend path
	// leaf actually references the operator key via AST. The
	// validator already rejected non-operator-backed paths before
	// any client signature was accepted, but re-running the check
	// here means a direct caller of signForfeitVTXOInput that skips
	// validation cannot get the operator to sign an unrelated
	// script. See also oor/checkpoint_cosign.go's AST check.
	if err := ensureForfeitSpendPathCommitsOperator(
		vtxo, spendPath, operatorKey.PubKey,
	); err != nil {
		return err
	}

	vtxoCtx := &tx.VTXOSpendContext{
		Outpoint: vtxoOutpoint,
		Output:   vtxoOutput,
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

	signDesc := spendPath.BuildSignDescriptor(
		operatorKey, vtxoOutput, sigHashes, prevOutFetcher,
		tx.ForfeitVTXOInputIndex,
	)

	serverSig, err := walletCtrl.SignOutputRaw(ftx, signDesc)
	if err != nil {
		return fmt.Errorf("failed to sign VTXO input: %w", err)
	}

	witness, err := spendPath.Witness(
		arkscript.MaybeAppendSighash(serverSig, signDesc.HashType),
		arkscript.MaybeAppendSighash(clientSig, signDesc.HashType),
	)
	if err != nil {
		return fmt.Errorf("failed to build custom witness: %w", err)
	}

	ftx.TxIn[tx.ForfeitVTXOInputIndex].Witness = witness

	err = verifyCompletedForfeitVTXOInput(
		ftx, vtxoOutput, prevOutFetcher, sigHashes,
	)
	if err != nil {
		return err
	}

	return nil
}

// verifyCompletedForfeitVTXOInput runs the completed VTXO input through the
// script engine to ensure the assembled forfeit witness is actually valid for
// the chosen tapscript path and tx-level fields.
func verifyCompletedForfeitVTXOInput(ftx *wire.MsgTx, vtxoOutput *wire.TxOut,
	prevOutFetcher txscript.PrevOutputFetcher,
	sigHashes *txscript.TxSigHashes) error {

	engine, err := txscript.NewEngine(
		vtxoOutput.PkScript, ftx, tx.ForfeitVTXOInputIndex,
		txscript.StandardVerifyFlags, nil, sigHashes,
		vtxoOutput.Value, prevOutFetcher,
	)
	if err != nil {
		return fmt.Errorf("create forfeit VTXO engine: %w", err)
	}

	err = engine.Execute()
	if err != nil {
		return fmt.Errorf("verify completed forfeit VTXO input: %w",
			err)
	}

	return nil
}

// signForfeitConnectorInput signs the connector input in a forfeit
// transaction. The connector is spent via keyspend (operator-only). This
// assumes the VTXO input witness has already been set.
func signForfeitConnectorInput(ftx *wire.MsgTx, vtxoOutput *wire.TxOut,
	connectorLeafOutput *wire.TxOut,
	walletCtrl WalletController,
	operatorKey keychain.KeyDescriptor) error {

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
		KeyDesc:           operatorKey,
		Output:            connectorLeafOutput,
		HashType:          txscript.SigHashDefault,
		InputIndex:        tx.ForfeitConnectorInputIndex,
		SignMethod:        input.TaprootKeySpendBIP0086SignMethod,
		SigHashes:         sigHashes,
		PrevOutputFetcher: prevOutFetcher,
		TapTweak:          []byte{},
	}

	connectorSig, err := walletCtrl.SignOutputRaw(
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

// buildConnectorTreeFromDescriptor reconstructs a connector tree from the
// commitment transaction and a stored descriptor.
func buildConnectorTreeFromDescriptor(commitmentTx *wire.MsgTx,
	desc *ConnectorTreeDescriptor, operatorKey *btcec.PublicKey,
	radix int) (*tree.Tree, error) {

	if commitmentTx == nil {
		return nil, fmt.Errorf("commitment tx cannot be nil")
	}

	if desc == nil {
		return nil, fmt.Errorf("connector descriptor cannot be nil")
	}

	if desc.OutputIndex < 0 || desc.OutputIndex >= len(commitmentTx.TxOut) {
		return nil, fmt.Errorf("connector output index out of bounds")
	}

	if desc.NumLeaves <= 0 {
		return nil, fmt.Errorf("connector num leaves must be > 0")
	}

	if operatorKey == nil {
		return nil, fmt.Errorf("operator key cannot be nil")
	}

	output := commitmentTx.TxOut[desc.OutputIndex]
	if output == nil {
		return nil, fmt.Errorf("connector output cannot be nil")
	}

	if output.Value%int64(desc.NumLeaves) != 0 {
		return nil, fmt.Errorf("connector output value does not " +
			"divide into leaves")
	}

	leafAmount := btcutil.Amount(
		output.Value / int64(desc.NumLeaves),
	)

	connectorDesc := tree.ConnectorDescriptor{
		PkScript:  output.PkScript,
		NumLeaves: desc.NumLeaves,
		Amount:    leafAmount,
	}

	connectorOutpoint := wire.OutPoint{
		Hash:  commitmentTx.TxHash(),
		Index: uint32(desc.OutputIndex),
	}

	return tree.BuildConnectorTree(
		connectorOutpoint, output, connectorDesc, operatorKey, radix,
	)
}
