package waved

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/lib/recovery"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/oor"
	"github.com/lightninglabs/wavelength/unroll"
)

// validatePreparedChannelSource bounds the negotiated exit before committing
// wallet inputs. Witnesses are installed later; signatures cannot change
// these transactions' outpoints, sequences or lock times.
func (s *Server) validatePreparedChannelSource(ctx context.Context,
	terms arkchannel.Terms, source arkchannel.VTXOBinding,
	prepared *oor.Prepared) error {

	height, err := s.channelSourceAdmissionHeight(
		ctx, terms, prepared.TransferInputs,
	)
	if err != nil {
		return err
	}
	if s.channelFundingProofs == nil {
		return fmt.Errorf("channel funding proof assembler unavailable")
	}
	proof, err := projectChannelFundingProof(
		ctx, s.channelFundingProofs, source.OutPoint,
		terms.VTXO.ChannelDelay, prepared,
	)
	if err != nil {
		return err
	}
	if err := validateArkChannelRecoveryWindow(
		proof, terms.VTXO.FunderDelay, height,
	); err != nil {
		return fmt.Errorf("%w: %w", arkchannel.ErrSourceInadmissible,
			err)
	}

	return nil
}

// projectChannelFundingProof extends the wallet inputs' existing recovery
// proofs with the exact prepared checkpoints and the policy output. It never
// stores an unsigned projection as exit material.
func projectChannelFundingProof(ctx context.Context,
	assembler unroll.ProofAssembler, target wire.OutPoint, delay uint32,
	prepared *oor.Prepared) (*recovery.Proof, error) {

	pkg, err := oortx.ValidateSubmitPackage(
		prepared.ArkPSBT, prepared.CheckpointPSBTs,
	)
	if err != nil {
		return nil, fmt.Errorf("invalid prepared channel transfer: %w",
			err)
	}
	if pkg.ArkTxid != target.Hash {
		return nil, fmt.Errorf("prepared transfer does not create " +
			"channel source")
	}
	selected := make(
		map[wire.OutPoint]struct{}, len(prepared.TransferInputs),
	)
	for _, input := range prepared.TransferInputs {
		if input.VTXO == nil {
			return nil, fmt.Errorf("missing channel funding input")
		}
		selected[input.VTXO.Outpoint] = struct{}{}
	}
	nodes := make(map[chainhash.Hash]*recovery.Node)
	for _, checkpoint := range prepared.CheckpointPSBTs {
		tx := checkpoint.UnsignedTx
		for _, in := range tx.TxIn {
			out := in.PreviousOutPoint
			if _, ok := selected[out]; !ok {
				return nil, fmt.Errorf("unexpected channel "+
					"funding input %s", out)
			}
			delete(selected, out)
			proof, err := assembler.EnsureProof(ctx, out)
			if err != nil {
				return nil, fmt.Errorf("resolve funding input "+
					"%s: %w", out, err)
			}
			for _, layer := range proof.Layers() {
				for _, id := range layer {
					nodes[id], _ = proof.Node(id)
				}
			}
		}
		nodes[tx.TxHash()] = &recovery.Node{
			Kind: recovery.NodeKindCheckpoint,
			Tx:   tx,
		}
	}
	if len(selected) != 0 {
		return nil, fmt.Errorf("prepared checkpoints omit channel " +
			"funding inputs")
	}
	nodes[target.Hash] = &recovery.Node{
		Kind: recovery.NodeKindArk,
		Tx:   prepared.ArkPSBT.UnsignedTx,
	}

	projected, err := recovery.NewProof(
		target, delay, slices.Collect(maps.Values(nodes))...,
	)
	if err != nil {

		// The complete set of immutable inputs was available. Retrying
		// this same transfer cannot change a structural proof limit.
		return nil, fmt.Errorf("%w: %w",
			arkchannel.ErrSourceInadmissible, err)
	}

	return projected, nil
}
