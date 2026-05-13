package oor

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	checkpointtx "github.com/lightninglabs/darepo-client/lib/tx/checkpoint"
	"github.com/lightninglabs/darepo/db"
	"github.com/lightninglabs/darepo/db/sqlc"
)

// expectedCheckpointTapLeaves is the number of leaves the canonical OOR
// checkpoint tap tree commits to: the operator unilateral CSV timeout leaf and
// the collaborative owner leaf.
const expectedCheckpointTapLeaves = 2

// CheckpointSweepInfo is the narrow OOR persistence projection needed by the
// fraud responder to reconstruct a checkpoint timeout sweep.
type CheckpointSweepInfo struct {
	// InputOutpoint is the original OOR-spent VTXO input.
	InputOutpoint wire.OutPoint

	// CheckpointTx is the finalized checkpoint transaction.
	CheckpointTx *wire.MsgTx

	// CheckpointOutputIndex is the checkpoint output to sweep.
	CheckpointOutputIndex uint32

	// CheckpointOutput is the output value and pkScript for output 0.
	CheckpointOutput *wire.TxOut

	// TapTreeEncoded is the PSBT output tap tree metadata for output 0.
	TapTreeEncoded []byte
}

// LoadCheckpointSweepInfoByInput returns the finalized checkpoint output
// metadata needed to build the operator timeout sweep.
func (s *DBSessionStore) LoadCheckpointSweepInfoByInput(ctx context.Context,
	input wire.OutPoint) (*CheckpointSweepInfo, bool, error) {

	var info *CheckpointSweepInfo

	err := s.tx.ExecTx(ctx, db.ReadTxOption(),
		func(q *sqlc.Queries) error {
			params := sqlc.GetBroadcastableOORCheckpointByInputParams{ //nolint:ll
				InputTxid: input.Hash[:],
				InputVout: int32(input.Index),
			}

			checkpointPSBT, err := q.GetBroadcastableOORCheckpointByInput( //nolint:ll
				ctx, params,
			)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}

			loaded, err := sweepInfoFromCheckpointPSBT(
				input, checkpointPSBT,
			)
			if err != nil {
				return err
			}

			info = loaded

			return nil
		},
	)
	if err != nil {
		return nil, false, err
	}
	if info == nil {
		return nil, false, nil
	}

	return info, true, nil
}

// sweepInfoFromCheckpointPSBT extracts output 0 and tap tree metadata.
func sweepInfoFromCheckpointPSBT(input wire.OutPoint,
	checkpointPSBT []byte) (*CheckpointSweepInfo, error) {

	pkt, err := deserializePSBT(checkpointPSBT)
	if err != nil {
		return nil, err
	}
	if pkt == nil || pkt.UnsignedTx == nil {
		return nil, fmt.Errorf("checkpoint psbt missing unsigned tx")
	}
	if len(pkt.UnsignedTx.TxOut) == 0 {
		return nil, fmt.Errorf("checkpoint tx has no outputs")
	}
	if len(pkt.Outputs) == 0 {
		return nil, fmt.Errorf("checkpoint psbt has no outputs")
	}

	checkpointTx, err := extractCheckpointTx(pkt)
	if err != nil {
		return nil, fmt.Errorf("extract checkpoint tx for %s: %w",
			input, err)
	}

	output := pkt.UnsignedTx.TxOut[0]
	tapTree := pkt.Outputs[0].TaprootTapTree
	if len(tapTree) == 0 {
		return nil, fmt.Errorf("checkpoint output 0 missing tap tree")
	}

	// PSBT output metadata (including TaprootTapTree) is not committed to
	// by the on-chain txid and is not protected by the checkpoint
	// signatures. Before exposing the encoded tree as authoritative sweep
	// material, prove it commits to the actual checkpoint output script.
	// Without this check, a poisoned tap tree would only surface much
	// later inside the fraud sweep builder, after the binding check on
	// the rebuilt control block fails, preventing timely fraud recovery.
	err = verifyCheckpointTapTreeBindsToPkScript(tapTree, output.PkScript)
	if err != nil {
		return nil, fmt.Errorf("checkpoint output 0 tap tree: %w", err)
	}

	return &CheckpointSweepInfo{
		InputOutpoint:         input,
		CheckpointTx:          checkpointTx,
		CheckpointOutputIndex: 0,
		CheckpointOutput:      cloneTxOut(output),
		TapTreeEncoded:        bytes.Clone(tapTree),
	}, nil
}

// verifyCheckpointTapTreeBindsToPkScript checks that the PSBT output tap-tree
// blob decodes to a canonical two-leaf OOR checkpoint tree whose taproot
// output key (computed against the Ark NUMS internal key) yields exactly the
// supplied pkScript. PSBT output metadata is unauthenticated, so without this
// binding any later sweep that reads the tree as authoritative would happily
// derive a control block for an unrelated taproot output.
func verifyCheckpointTapTreeBindsToPkScript(encoded, pkScript []byte) error {
	if len(pkScript) == 0 {
		return fmt.Errorf("checkpoint output pkScript is empty")
	}

	leafScripts, err := checkpointtx.DecodeTapTree(encoded)
	if err != nil {
		return fmt.Errorf("decode tap tree: %w", err)
	}

	// The v0 OOR checkpoint tap tree is fixed shape: exactly the operator
	// CSV timeout leaf and the collaborative owner leaf. Trees of any
	// other shape cannot have produced the on-chain pkScript via the
	// canonical CheckpointTapScript builder.
	if len(leafScripts) != expectedCheckpointTapLeaves {
		return fmt.Errorf("tap tree has %d leaves, want %d",
			len(leafScripts), expectedCheckpointTapLeaves)
	}

	leaves := make([]txscript.TapLeaf, 0, len(leafScripts))
	for _, script := range leafScripts {
		if len(script) == 0 {
			return fmt.Errorf("tap tree leaf script is empty")
		}

		leaves = append(leaves, txscript.NewBaseTapLeaf(script))
	}

	tree := txscript.AssembleTaprootScriptTree(leaves...)
	rootHash := tree.RootNode.TapHash()
	outputKey := txscript.ComputeTaprootOutputKey(
		&arkscript.ARKNUMSKey, rootHash[:],
	)

	expectedPkScript, err := txscript.PayToTaprootScript(outputKey)
	if err != nil {
		return fmt.Errorf("derive p2tr from tap tree: %w", err)
	}

	if !bytes.Equal(expectedPkScript, pkScript) {
		return fmt.Errorf("tap tree does not commit to checkpoint "+
			"output pkScript: got %x want %x", expectedPkScript,
			pkScript)
	}

	return nil
}

// cloneTxOut copies a transaction output and its script bytes.
func cloneTxOut(out *wire.TxOut) *wire.TxOut {
	if out == nil {
		return nil
	}

	return &wire.TxOut{
		Value:    out.Value,
		PkScript: bytes.Clone(out.PkScript),
	}
}
