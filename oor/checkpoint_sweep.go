package oor

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo/db"
	"github.com/lightninglabs/darepo/db/sqlc"
)

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

	return &CheckpointSweepInfo{
		InputOutpoint:         input,
		CheckpointTx:          checkpointTx,
		CheckpointOutputIndex: 0,
		CheckpointOutput:      cloneTxOut(output),
		TapTreeEncoded:        bytes.Clone(tapTree),
	}, nil
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
