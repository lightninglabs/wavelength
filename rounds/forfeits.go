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
	vtxoExitDelay uint32, roundID RoundID) ([]*SpentVTXO, error) {

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
			return nil, fmt.Errorf("client VTXO signature cannot " +
				"be nil")
		}
		if forfeitTxSig.SpendPath == nil {
			return nil, fmt.Errorf("forfeit spend path cannot be " +
				"nil")
		}

		ftx := forfeitTxSig.UnsignedTx
		if len(ftx.TxIn) != 2 {
			return nil, fmt.Errorf("forfeit tx must have 2 "+
				"inputs, got %d", len(ftx.TxIn))
		}

		// Clear any witness state on the forfeit tx before signing.
		// We sign the VTXO and connector inputs against the same
		// *wire.MsgTx, and lndclient's encodeTx serializes the witness
		// when it ships the tx to a remote-signer LND. The watch-only
		// LND then can't wrap the tx in a fresh PSBT
		// (psbt.NewFromUnsignedTx rejects witness-bearing inputs) and
		// silently returns no signature. Mirrors the per-pass clear in
		// batchsweeper/sweep.go's signSweepInputs.
		for i := range ftx.TxIn {
			ftx.TxIn[i].Witness = nil
		}

		vtxoOutpoint :=
			ftx.TxIn[tx.ForfeitVTXOInputIndex].PreviousOutPoint
		forfeitInput, exists := forfeitInputs[vtxoOutpoint]
		if !exists {
			return nil, fmt.Errorf("forfeit input not found "+
				"for VTXO %v", vtxoOutpoint)
		}

		assignment, exists := connectorAssignments[vtxoOutpoint]
		if !exists {
			return nil, fmt.Errorf("no connector assignment "+
				"for VTXO %v", vtxoOutpoint)
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

		// Sign the VTXO input first. We collect the witness stack and
		// the intermediate state needed for script-engine verification
		// but do NOT attach the witness to ftx yet — see the
		// witness-clear comment above for why we keep ftx witness-free
		// until every SignOutputRaw call has returned.
		vtxoSign, err := signForfeitVTXOInput(
			ftx, forfeitInput, forfeitTxSig.ClientVTXOSig,
			assignment.LeafOutput, walletCtrl, operatorKey,
			vtxoExitDelay, forfeitTxSig.SpendPath,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to sign VTXO input for "+
				"%v: %w", vtxoOutpoint, err)
		}

		// Sign the connector input while ftx is still witness-free.
		connectorWitness, err := signForfeitConnectorInput(
			ftx, vtxoSign.VTXOOutput, assignment.LeafOutput,
			walletCtrl, operatorKey,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to sign connector "+
				"input for %v: %w", vtxoOutpoint, err)
		}

		// Both signing calls have returned. Attach the witnesses now,
		// then run script-engine verification on the fully-signed tx.
		ftx.TxIn[tx.ForfeitVTXOInputIndex].Witness =
			vtxoSign.Witness
		ftx.TxIn[tx.ForfeitConnectorInputIndex].Witness =
			connectorWitness

		err = verifyCompletedForfeitVTXOInput(
			ftx, vtxoSign.VTXOOutput, vtxoSign.PrevOutFetcher,
			vtxoSign.SigHashes,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to verify forfeit VTXO "+
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

// forfeitVTXOSignResult bundles the outputs of signForfeitVTXOInput: the
// completed VTXO witness stack plus the intermediate state needed to run
// script-engine verification on the assembled forfeit tx after both
// witnesses have been attached.
type forfeitVTXOSignResult struct {
	// Witness is the completed witness stack for the VTXO input
	// (collaborative tapscript path: server sig + client sig +
	// witness script + control block).
	Witness wire.TxWitness

	// VTXOOutput is the prev-out for the VTXO input, threaded back
	// to the caller so the connector sign call and the script-engine
	// verifier can reuse it without rebuilding.
	VTXOOutput *wire.TxOut

	// PrevOutFetcher and SigHashes are the BIP-341 sighash midstate
	// used during signing; the caller reuses them for script-engine
	// verification of the completed VTXO input.
	PrevOutFetcher txscript.PrevOutputFetcher
	SigHashes      *txscript.TxSigHashes
}

// signForfeitVTXOInput produces the server-side witness for the VTXO input
// of a forfeit transaction. The VTXO is spent via the collaborative
// tapscript path, which requires both client and operator signatures.
//
// The witness is returned to the caller rather than attached to ftx, so
// that ftx remains witness-free across subsequent SignOutputRaw calls
// against the same tx. lndclient's encodeTx serializes any attached
// witness when shipping to a remote-signer LND, where
// psbt.NewFromUnsignedTx then rejects the witness-bearing tx and silently
// drops the next signature. The same pattern is enforced in
// batchsweeper/sweep.go's signSweepInputs.
func signForfeitVTXOInput(ftx *wire.MsgTx, forfeitInput *ForfeitInput,
	clientSig *schnorr.Signature, connectorOutput *wire.TxOut,
	walletCtrl WalletController, operatorKey keychain.KeyDescriptor,
	vtxoExitDelay uint32,
	spendPath *arkscript.SpendPath) (*forfeitVTXOSignResult, error) {

	if forfeitInput == nil || forfeitInput.VTXO == nil {
		return nil, fmt.Errorf("forfeit VTXO must be provided")
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
		return nil, fmt.Errorf("forfeit spend path must be provided")
	}
	if err := spendPath.Validate(); err != nil {
		return nil, fmt.Errorf("invalid forfeit spend path: %w", err)
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
		return nil, err
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
		return nil, fmt.Errorf("failed to create prev out fetcher: %w",
			err)
	}

	sigHashes := txscript.NewTxSigHashes(ftx, prevOutFetcher)

	signDesc := spendPath.BuildSignDescriptor(
		operatorKey, vtxoOutput, sigHashes, prevOutFetcher,
		tx.ForfeitVTXOInputIndex,
	)

	serverSig, err := walletCtrl.SignOutputRaw(ftx, signDesc)
	if err != nil {
		return nil, fmt.Errorf("failed to sign VTXO input: %w", err)
	}

	witness, err := spendPath.Witness(
		arkscript.MaybeAppendSighash(serverSig, signDesc.HashType),
		arkscript.MaybeAppendSighash(clientSig, signDesc.HashType),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to build custom witness: %w",
			err)
	}

	return &forfeitVTXOSignResult{
		Witness:        witness,
		VTXOOutput:     vtxoOutput,
		PrevOutFetcher: prevOutFetcher,
		SigHashes:      sigHashes,
	}, nil
}

// verifyCompletedForfeitVTXOInput runs the completed VTXO input through the
// script engine to ensure the assembled forfeit witness is actually valid for
// the chosen tapscript path and tx-level fields.
func verifyCompletedForfeitVTXOInput(ftx *wire.MsgTx, vtxoOutput *wire.TxOut,
	prevOutFetcher txscript.PrevOutputFetcher,
	sigHashes *txscript.TxSigHashes) error {

	engine, err := txscript.NewEngine(
		vtxoOutput.PkScript, ftx, tx.ForfeitVTXOInputIndex,
		txscript.StandardVerifyFlags, nil, sigHashes, vtxoOutput.Value,
		prevOutFetcher,
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

// signForfeitConnectorInput produces the server-side witness for the
// connector input of a forfeit transaction. The connector is spent via
// keyspend (operator-only).
//
// Like signForfeitVTXOInput, the witness is returned rather than attached
// to ftx, so that ftx stays witness-free for any further SignOutputRaw
// calls. The caller (completeForfeitTxs) attaches both witnesses after
// signing for both inputs has completed.
func signForfeitConnectorInput(ftx *wire.MsgTx, vtxoOutput *wire.TxOut,
	connectorLeafOutput *wire.TxOut, walletCtrl WalletController,
	operatorKey keychain.KeyDescriptor) (wire.TxWitness, error) {

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
		return nil, fmt.Errorf("failed to sign connector input: %w",
			err)
	}

	return wire.TxWitness{connectorSig.Serialize()}, nil
}

// BuildConnectorTreeFromDescriptor reconstructs a connector tree from the
// commitment transaction and a stored descriptor.
func BuildConnectorTreeFromDescriptor(commitmentTx *wire.MsgTx,
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
