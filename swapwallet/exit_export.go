//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"bytes"
	"context"
	"encoding/hex"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/lightninglabs/wavelength/waved"
)

// exportExit projects verified local ancestry into a portable, versioned
// bundle without requiring the swap runtime or a funded backing wallet.
func (s *Service) exportExit(ctx context.Context,
	req *wavewalletrpc.ExportExitRequest) (
	*wavewalletrpc.ExportExitResponse, error) {

	if s.deps == nil || s.deps.RPCServer == nil {
		return nil, statusSwapBackendUnavailable()
	}
	exported, err := s.deps.RPCServer.ExportExit(
		ctx, &waved.ExitExportRequest{
			Outpoint:     req.GetOutpoint(),
			SweepAddress: req.GetSweepAddress(),
		},
	)
	if err != nil {
		return nil, err
	}

	return marshalExitExport(exported)
}

// marshalExitExport preserves exact signed bytes and public sweep evidence.
func marshalExitExport(exported *waved.ExitExport) (
	*wavewalletrpc.ExportExitResponse, error) {

	proof := exported.Ancestry.Proof
	response := &wavewalletrpc.ExportExitResponse{
		FormatVersion:     2,
		Network:           exported.Network,
		GenesisHash:       exported.GenesisHash,
		TargetOutpoint:    proof.TargetOutpoint().String(),
		CsvDelay:          proof.CSVDelay(),
		BatchExpiryHeight: exported.BatchExpiryHeight,
	}
	for _, input := range exported.Ancestry.ExternalInputs {
		var sweep *wavewalletrpc.ExitCommitmentSweep
		evidence := exported.CommitmentSweeps[input.Outpoint]
		if evidence != nil {
			sweep = &wavewalletrpc.ExitCommitmentSweep{
				DelayBlocks: evidence.DelayBlocks,
				LeafScriptHex: hex.EncodeToString(
					evidence.LeafScript,
				),
				ControlBlockHex: hex.EncodeToString(
					evidence.ControlBlock,
				),
			}
		}
		response.ExternalInputs = append(
			response.ExternalInputs,
			&wavewalletrpc.ExitAncestryInput{
				Outpoint:        input.Outpoint.String(),
				CommitmentSweep: sweep,
				AmountSat:       input.Output.Value,
				PkScriptHex: hex.EncodeToString(
					input.Output.PkScript,
				),
			},
		)
	}

	// Layer order puts every dependency before its children; each layer
	// and its parent list are already deterministically sorted by Proof.
	for _, layer := range proof.Layers() {
		for _, txid := range layer {
			node, _ := proof.Node(txid)
			var raw bytes.Buffer
			if err := node.Tx.Serialize(&raw); err != nil {
				return nil, err
			}
			transaction := &wavewalletrpc.ExitAncestryTransaction{
				Txid:     txid.String(),
				Kind:     node.Kind.String(),
				RawTxHex: hex.EncodeToString(raw.Bytes()),
			}
			parents, err := proof.ParentTxids(txid)
			if err != nil {
				return nil, err
			}
			for _, parent := range parents {
				transaction.ParentTxids = append(
					transaction.ParentTxids,
					parent.String(),
				)
			}
			anchor, ok, err := node.AnchorOutpoint()
			if err != nil {
				return nil, err
			}
			if ok {
				transaction.AnchorOutpoint = anchor.String()
			}
			response.Transactions = append(
				response.Transactions, transaction,
			)
		}
	}

	if sweep := exported.FinalSweep; sweep != nil {
		var raw bytes.Buffer
		if err := sweep.Serialize(&raw); err != nil {
			return nil, err
		}
		response.FinalSweep = &wavewalletrpc.ExitFinalSweep{
			Txid:     sweep.TxHash().String(),
			RawTxHex: hex.EncodeToString(raw.Bytes()),
			AnchorOutpoint: wire.OutPoint{
				Hash: sweep.TxHash(), Index: 1,
			}.String(),
			DestinationAddress: exported.SweepAddress,
			AmountSat:          sweep.TxOut[0].Value,
		}
	}

	return response, nil
}
