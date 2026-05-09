package fraud

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	arktxlib "github.com/lightninglabs/darepo-client/lib/tx"
	"github.com/lightninglabs/darepo-client/lib/tx/arktx"
	"github.com/lightninglabs/darepo-client/lib/tx/checkpoint"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// CheckpointSweepInfo is the narrow persisted checkpoint projection needed to
// reconstruct and sign the operator timeout sweep.
type CheckpointSweepInfo struct {
	// InputOutpoint is the original spent VTXO input.
	InputOutpoint wire.OutPoint

	// CheckpointTx is the finalized checkpoint transaction.
	CheckpointTx *wire.MsgTx

	// CheckpointOutputIndex is the swept checkpoint output. Step 1 only
	// supports output 0.
	CheckpointOutputIndex uint32

	// CheckpointOutput is the value and pkScript of the checkpoint output.
	CheckpointOutput *wire.TxOut

	// TapTreeEncoded is the checkpoint output tap tree metadata persisted
	// in the finalized checkpoint PSBT.
	TapTreeEncoded []byte
}

// CheckpointSweepRequest contains all data needed to build a timeout sweep.
type CheckpointSweepRequest struct {
	// Info is the persisted checkpoint projection (input outpoint,
	// finalized checkpoint tx, output 0 details, tap tree blob).
	Info *CheckpointSweepInfo

	// Policy is the operator's checkpoint policy (operator key, CSV
	// delay) used to rebuild the timeout leaf and its control block.
	Policy arkscript.CheckpointPolicy

	// OperatorKey is the operator key descriptor used to sign the
	// timeout leaf.
	OperatorKey keychain.KeyDescriptor

	// Signer signs the checkpoint timeout sweep input.
	Signer input.Signer

	// SweepPkScript is the destination pkScript the swept value is sent
	// to (a fresh server-controlled wallet output).
	SweepPkScript []byte
}

// ForfeitSweepRequest contains all data needed to sweep a confirmed forfeit
// penalty output into the operator wallet.
type ForfeitSweepRequest struct {
	// ForfeitTx is the confirmed stored forfeit transaction. Output 0 is
	// the penalty output swept by this request.
	ForfeitTx *wire.MsgTx

	// ForfeitOutpoint is the forfeited VTXO spent by ForfeitTx input 0.
	ForfeitOutpoint wire.OutPoint

	// OperatorKey is the operator key descriptor used to sign the BIP86
	// keyspend of the penalty output.
	OperatorKey keychain.KeyDescriptor

	// Signer signs the forfeit penalty sweep input.
	Signer input.Signer

	// SweepPkScript is the destination pkScript the swept value is sent to.
	SweepPkScript []byte
}

// BuildCheckpointTimeoutSweep builds and validates the operator CSV timeout
// sweep for checkpoint output 0.
func BuildCheckpointTimeoutSweep(_ context.Context,
	req *CheckpointSweepRequest) (*wire.MsgTx, error) {

	if req == nil {
		return nil, fmt.Errorf("sweep request is nil")
	}
	if req.Info == nil {
		return nil, fmt.Errorf("checkpoint sweep info is nil")
	}
	if req.Policy.OperatorKey == nil {
		return nil, fmt.Errorf("checkpoint operator key is nil")
	}
	if req.OperatorKey.PubKey == nil {
		return nil, fmt.Errorf("operator key descriptor missing pubkey")
	}
	if !req.OperatorKey.PubKey.IsEqual(req.Policy.OperatorKey) {
		return nil, fmt.Errorf("operator signing key does not match " +
			"checkpoint policy")
	}
	if req.Signer == nil {
		return nil, fmt.Errorf("checkpoint sweep signer is nil")
	}
	if len(req.SweepPkScript) == 0 {
		return nil, fmt.Errorf("checkpoint sweep pkScript is empty")
	}

	info := req.Info
	if err := validateSweepInfo(info); err != nil {
		return nil, err
	}

	spendInfo, err := checkpointTimeoutSpendInfo(
		info, req.Policy,
	)
	if err != nil {
		return nil, err
	}

	if err := (&arkscript.SpendPath{
		SpendInfo: spendInfo,
	}).VerifyBindsToPkScript(info.CheckpointOutput.PkScript); err != nil {
		return nil, fmt.Errorf("checkpoint timeout control block: %w",
			err)
	}

	checkpointTxid := info.CheckpointTx.TxHash()
	checkpointOutpoint := wire.OutPoint{
		Hash:  checkpointTxid,
		Index: info.CheckpointOutputIndex,
	}

	sweepTx := wire.NewMsgTx(arktx.TxVersion)
	sweepTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: checkpointOutpoint,
		Sequence:         req.Policy.CSVDelay,
	})
	sweepTx.AddTxOut(&wire.TxOut{
		Value:    info.CheckpointOutput.Value,
		PkScript: append([]byte(nil), req.SweepPkScript...),
	})
	sweepTx.AddTxOut(arkscript.AnchorOutput())

	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		info.CheckpointOutput.PkScript, info.CheckpointOutput.Value,
	)
	sigHashes := txscript.NewTxSigHashes(sweepTx, prevFetcher)
	signDesc := spendInfo.BuildSignDescriptor(
		req.OperatorKey, info.CheckpointOutput, sigHashes, prevFetcher,
		0,
	)

	witness, err := arkscript.VTXOTimeoutSpendWitness(
		req.Signer, signDesc, sweepTx,
	)
	if err != nil {
		return nil, fmt.Errorf("sign checkpoint sweep: %w", err)
	}
	sweepTx.TxIn[0].Witness = witness

	if err := validateCheckpointSweepTx(
		sweepTx, checkpointOutpoint, info.CheckpointOutput,
		req.Policy.CSVDelay, spendInfo,
	); err != nil {
		return nil, err
	}

	return sweepTx, nil
}

// BuildForfeitOutputSweep builds and validates the operator keyspend that
// moves a confirmed forfeit penalty output into a wallet-recognized script.
func BuildForfeitOutputSweep(_ context.Context,
	req *ForfeitSweepRequest) (*wire.MsgTx, error) {

	if req == nil {
		return nil, fmt.Errorf("forfeit sweep request is nil")
	}
	if req.OperatorKey.PubKey == nil {
		return nil, fmt.Errorf("operator key descriptor missing pubkey")
	}
	if req.Signer == nil {
		return nil, fmt.Errorf("forfeit sweep signer is nil")
	}
	if len(req.SweepPkScript) == 0 {
		return nil, fmt.Errorf("forfeit sweep pkScript is empty")
	}
	if err := validateForfeitSweepTx(req.ForfeitTx); err != nil {
		return nil, err
	}
	if req.ForfeitTx.TxIn[0].PreviousOutPoint != req.ForfeitOutpoint {
		return nil, fmt.Errorf("forfeit tx input spends %s, want %s",
			req.ForfeitTx.TxIn[0].PreviousOutPoint,
			req.ForfeitOutpoint)
	}

	forfeitOutput := req.ForfeitTx.TxOut[arktxlib.ForfeitPenaltyOutputIndex]
	if err := validateForfeitOutputScript(
		forfeitOutput.PkScript, req.OperatorKey,
	); err != nil {
		return nil, err
	}

	forfeitTxid := req.ForfeitTx.TxHash()
	forfeitOutpoint := wire.OutPoint{
		Hash:  forfeitTxid,
		Index: arktxlib.ForfeitPenaltyOutputIndex,
	}

	sweepTx := wire.NewMsgTx(arktx.TxVersion)
	sweepTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: forfeitOutpoint,
		Sequence:         wire.MaxTxInSequenceNum,
	})
	sweepTx.AddTxOut(&wire.TxOut{
		Value:    forfeitOutput.Value,
		PkScript: append([]byte(nil), req.SweepPkScript...),
	})
	sweepTx.AddTxOut(arkscript.AnchorOutput())

	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		forfeitOutput.PkScript, forfeitOutput.Value,
	)
	sigHashes := txscript.NewTxSigHashes(sweepTx, prevFetcher)
	signDesc := &input.SignDescriptor{
		KeyDesc:           req.OperatorKey,
		TapTweak:          []byte{},
		SignMethod:        input.TaprootKeySpendBIP0086SignMethod,
		Output:            forfeitOutput,
		HashType:          txscript.SigHashDefault,
		SigHashes:         sigHashes,
		PrevOutputFetcher: prevFetcher,
		InputIndex:        0,
	}

	sig, err := req.Signer.SignOutputRaw(sweepTx, signDesc)
	if err != nil {
		return nil, fmt.Errorf("sign forfeit sweep: %w", err)
	}
	sweepTx.TxIn[0].Witness = wire.TxWitness{sig.Serialize()}

	if err := validateForfeitSweep(
		sweepTx, forfeitOutpoint, forfeitOutput,
	); err != nil {
		return nil, err
	}

	return sweepTx, nil
}

// validateSweepInfo checks the persisted checkpoint projection shape.
func validateSweepInfo(info *CheckpointSweepInfo) error {
	switch {
	case info == nil:
		return fmt.Errorf("checkpoint sweep info is nil")

	case info.CheckpointTx == nil:
		return fmt.Errorf("checkpoint tx is nil")

	case info.CheckpointOutputIndex != 0:
		return fmt.Errorf("checkpoint output index %d, want 0",
			info.CheckpointOutputIndex)

	case len(info.CheckpointTx.TxOut) == 0:
		return fmt.Errorf("checkpoint tx has no outputs")

	case info.CheckpointOutput == nil:
		return fmt.Errorf("checkpoint output is nil")

	case !txOutEqual(info.CheckpointOutput, info.CheckpointTx.TxOut[0]):
		return fmt.Errorf(
			"checkpoint output does not match tx output 0",
		)

	case info.CheckpointOutput.Value <= 0:
		return fmt.Errorf("checkpoint output value must be positive")

	case len(info.CheckpointOutput.PkScript) == 0:
		return fmt.Errorf("checkpoint output pkScript is empty")

	case len(info.TapTreeEncoded) == 0:
		return fmt.Errorf("checkpoint tap tree is empty")
	}

	return nil
}

// validateForfeitSweepTx checks the confirmed forfeit tx shape.
func validateForfeitSweepTx(forfeitTx *wire.MsgTx) error {
	switch {
	case forfeitTx == nil:
		return fmt.Errorf("forfeit tx is nil")

	case len(forfeitTx.TxIn) == 0:
		return fmt.Errorf("forfeit tx has no inputs")

	case len(forfeitTx.TxOut) <= arktxlib.ForfeitPenaltyOutputIndex:
		return fmt.Errorf("forfeit tx has no penalty output")

	case forfeitTx.TxOut[arktxlib.ForfeitPenaltyOutputIndex] == nil:
		return fmt.Errorf("forfeit penalty output is nil")

	case forfeitTx.TxOut[arktxlib.ForfeitPenaltyOutputIndex].Value <= 0:
		return fmt.Errorf("forfeit penalty output value must be " +
			"positive")

	case len(forfeitTx.TxOut[arktxlib.ForfeitPenaltyOutputIndex].
		PkScript) == 0:

		return fmt.Errorf("forfeit penalty output pkScript is empty")
	}

	return nil
}

// validateForfeitOutputScript checks that output 0 is the operator's BIP86
// taproot output before the fraud actor signs it.
func validateForfeitOutputScript(pkScript []byte,
	operatorKey keychain.KeyDescriptor) error {

	expected, err := txscript.PayToTaprootScript(
		txscript.ComputeTaprootKeyNoScript(operatorKey.PubKey),
	)
	if err != nil {
		return fmt.Errorf("build operator taproot script: %w", err)
	}
	if !bytes.Equal(pkScript, expected) {
		return fmt.Errorf("forfeit penalty output is not operator " +
			"BIP86")
	}

	return nil
}

// checkpointTimeoutSpendInfo reconstructs the operator timeout leaf and proof.
func checkpointTimeoutSpendInfo(info *CheckpointSweepInfo,
	policy arkscript.CheckpointPolicy) (*arkscript.SpendInfo, error) {

	timeoutLeaf, err := arkscript.UnilateralCSVTimeoutTapLeaf(
		policy.OperatorKey, policy.CSVDelay,
	)
	if err != nil {
		return nil, fmt.Errorf("build timeout leaf: %w", err)
	}

	leaves, err := checkpoint.DecodeTapTree(info.TapTreeEncoded)
	if err != nil {
		return nil, fmt.Errorf("decode checkpoint tap tree: %w", err)
	}

	var ownerLeaf []byte
	for _, leaf := range leaves {
		if bytes.Equal(leaf, timeoutLeaf.Script) {
			continue
		}

		if len(ownerLeaf) > 0 {
			return nil, fmt.Errorf(
				"checkpoint tap tree has multiple " +
					"non-timeout leaves",
			)
		}

		ownerLeaf = leaf
	}
	if len(ownerLeaf) == 0 {
		return nil, fmt.Errorf("checkpoint owner leaf not found")
	}

	tapscript, err := arkscript.CheckpointTapScript(policy, ownerLeaf)
	if err != nil {
		return nil, fmt.Errorf("rebuild checkpoint tap tree: %w", err)
	}

	tapTree := txscript.AssembleTaprootScriptTree(tapscript.Leaves...)
	if len(tapTree.LeafMerkleProofs) == 0 {
		return nil, fmt.Errorf("checkpoint timeout proof missing")
	}

	controlBlock := tapTree.LeafMerkleProofs[0].ToControlBlock(
		&arkscript.ARKNUMSKey,
	)
	controlBytes, err := controlBlock.ToBytes()
	if err != nil {
		return nil, fmt.Errorf("serialize timeout control block: %w",
			err)
	}

	return &arkscript.SpendInfo{
		WitnessScript: timeoutLeaf.Script,
		ControlBlock:  controlBytes,
	}, nil
}

// validateCheckpointSweepTx enforces local invariants before txconfirm.
func validateCheckpointSweepTx(sweepTx *wire.MsgTx,
	checkpointOutpoint wire.OutPoint, checkpointOutput *wire.TxOut,
	csvDelay uint32, spendInfo *arkscript.SpendInfo) error {

	switch {
	case len(sweepTx.TxIn) != 1:
		return fmt.Errorf("checkpoint sweep has %d inputs, want 1",
			len(sweepTx.TxIn))

	case sweepTx.TxIn[0].PreviousOutPoint != checkpointOutpoint:
		return fmt.Errorf("checkpoint sweep input spends %s, want %s",
			sweepTx.TxIn[0].PreviousOutPoint, checkpointOutpoint)

	case sweepTx.TxIn[0].Sequence != csvDelay:
		return fmt.Errorf("checkpoint sweep sequence %d, want %d",
			sweepTx.TxIn[0].Sequence, csvDelay)

	case len(sweepTx.TxIn[0].Witness) != 3:
		return fmt.Errorf(
			"checkpoint sweep witness has %d items, want 3",
			len(sweepTx.TxIn[0].Witness),
		)

	case !bytes.Equal(sweepTx.TxIn[0].Witness[1], spendInfo.WitnessScript):
		return fmt.Errorf("checkpoint sweep witness script mismatch")

	case !bytes.Equal(sweepTx.TxIn[0].Witness[2], spendInfo.ControlBlock):
		return fmt.Errorf("checkpoint sweep control block mismatch")

	case len(sweepTx.TxOut) != 2:
		return fmt.Errorf("checkpoint sweep has %d outputs, want 2",
			len(sweepTx.TxOut))

	case arktx.IsAnchorOutput(sweepTx.TxOut[0]):
		return fmt.Errorf("checkpoint sweep output 0 is anchor")

	case !arktx.IsAnchorOutput(sweepTx.TxOut[1]):
		return fmt.Errorf("checkpoint sweep output 1 is not anchor")
	}

	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		checkpointOutput.PkScript, checkpointOutput.Value,
	)
	sigHashes := txscript.NewTxSigHashes(sweepTx, prevFetcher)
	engine, err := txscript.NewEngine(
		checkpointOutput.PkScript, sweepTx, 0,
		txscript.StandardVerifyFlags, nil, sigHashes,
		checkpointOutput.Value, prevFetcher,
	)
	if err != nil {
		return fmt.Errorf("create checkpoint sweep script engine: %w",
			err)
	}
	if err := engine.Execute(); err != nil {
		return fmt.Errorf("checkpoint sweep script execution: %w", err)
	}

	return nil
}

// validateForfeitSweep enforces local invariants before txconfirm.
func validateForfeitSweep(sweepTx *wire.MsgTx,
	forfeitOutpoint wire.OutPoint, forfeitOutput *wire.TxOut) error {

	switch {
	case len(sweepTx.TxIn) != 1:
		return fmt.Errorf("forfeit sweep has %d inputs, want 1",
			len(sweepTx.TxIn))

	case sweepTx.TxIn[0].PreviousOutPoint != forfeitOutpoint:
		return fmt.Errorf("forfeit sweep input spends %s, want %s",
			sweepTx.TxIn[0].PreviousOutPoint, forfeitOutpoint)

	case sweepTx.TxIn[0].Sequence != wire.MaxTxInSequenceNum:
		return fmt.Errorf("forfeit sweep sequence %d, want %d",
			sweepTx.TxIn[0].Sequence, wire.MaxTxInSequenceNum)

	case len(sweepTx.TxIn[0].Witness) != 1:
		return fmt.Errorf("forfeit sweep witness has %d items, want 1",
			len(sweepTx.TxIn[0].Witness))

	case len(sweepTx.TxOut) != 2:
		return fmt.Errorf("forfeit sweep has %d outputs, want 2",
			len(sweepTx.TxOut))

	case arktx.IsAnchorOutput(sweepTx.TxOut[0]):
		return fmt.Errorf("forfeit sweep output 0 is anchor")

	case !arktx.IsAnchorOutput(sweepTx.TxOut[1]):
		return fmt.Errorf("forfeit sweep output 1 is not anchor")
	}

	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		forfeitOutput.PkScript, forfeitOutput.Value,
	)
	sigHashes := txscript.NewTxSigHashes(sweepTx, prevFetcher)
	engine, err := txscript.NewEngine(
		forfeitOutput.PkScript, sweepTx, 0,
		txscript.StandardVerifyFlags, nil, sigHashes,
		forfeitOutput.Value, prevFetcher,
	)
	if err != nil {
		return fmt.Errorf("create forfeit sweep script engine: %w", err)
	}
	if err := engine.Execute(); err != nil {
		return fmt.Errorf("forfeit sweep script execution: %w", err)
	}

	return nil
}

// txOutEqual compares wire outputs without requiring pointer identity.
func txOutEqual(a, b *wire.TxOut) bool {
	if a == nil || b == nil {
		return a == b
	}

	return a.Value == b.Value && bytes.Equal(a.PkScript, b.PkScript)
}
