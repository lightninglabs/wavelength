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

	proof := exported.Ancestry.Proof
	response := &wavewalletrpc.ExportExitResponse{
		FormatVersion:  2,
		Network:        exported.Network,
		GenesisHash:    exported.GenesisHash,
		TargetOutpoint: proof.TargetOutpoint().String(),
		CsvDelay:       proof.CSVDelay(),
	}
	for _, input := range exported.Ancestry.ExternalInputs {
		response.ExternalInputs = append(
			response.ExternalInputs,
			&wavewalletrpc.ExitAncestryInput{
				Outpoint:  input.Outpoint.String(),
				AmountSat: input.Output.Value,
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
