package oor

import (
	"context"
	"log/slog"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btclog/v2"
)

// logFinalizeCheckpointSummary emits a compact summary of the first input
// metadata for each finalized checkpoint PSBT.
func logFinalizeCheckpointSummary(ctx context.Context, log btclog.Logger,
	msg string, checkpoints []*psbt.Packet) {

	if log == nil {
		return
	}

	for i := range checkpoints {
		checkpoint := checkpoints[i]
		if checkpoint == nil || len(checkpoint.Inputs) == 0 {
			log.DebugS(
				ctx, msg, slog.Int("checkpoint_index", i),
				slog.Bool("nil_checkpoint", checkpoint == nil),
			)

			continue
		}

		in := checkpoint.Inputs[0]
		log.DebugS(
			ctx, msg, slog.Int("checkpoint_index", i),
			slog.Int(
				"final_witness_len", len(in.FinalScriptWitness),
			),
			slog.Int(
				"taproot_sig_count",
				len(in.TaprootScriptSpendSig),
			),
			slog.Int(
				"taproot_leaf_count", len(in.TaprootLeafScript),
			),
			slog.Int("unknown_count", len(in.Unknowns)),
		)
	}
}
