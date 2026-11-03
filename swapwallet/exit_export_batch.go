//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/lightninglabs/wavelength/waved"
	"google.golang.org/protobuf/proto"
)

// exportExits retains target-specific payouts and deadlines while emitting
// shared transactions and commitment outputs only once.
func (s *Service) exportExits(ctx context.Context,
	req *wavewalletrpc.ExportExitsRequest) (
	*wavewalletrpc.ExportExitsResponse, error) {

	if s.deps == nil || s.deps.RPCServer == nil {
		return nil, statusSwapBackendUnavailable()
	}
	requests := make([]*waved.ExitExportRequest, 0, len(req.GetExits()))
	for _, target := range req.GetExits() {
		requests = append(requests, &waved.ExitExportRequest{
			Outpoint:     target.GetOutpoint(),
			SweepAddress: target.GetSweepAddress(),
		})
	}
	exports, err := s.deps.RPCServer.ExportExits(ctx, requests)
	if err != nil {
		return nil, err
	}
	bundles := make([]*wavewalletrpc.ExportExitResponse, 0, len(exports))
	for _, exported := range exports {
		bundle, err := marshalExitExport(exported)
		if err != nil {
			return nil, err
		}
		bundles = append(bundles, bundle)
	}

	return mergeExitExports(bundles)
}

// mergeExitExports rejects conflicting shared material and orders the union
// by dependency depth, then raw transaction hash, independently of request
// order. It only accepts individually verified, topologically sorted bundles.
func mergeExitExports(bundles []*wavewalletrpc.ExportExitResponse) (
	*wavewalletrpc.ExportExitsResponse, error) {

	result := &wavewalletrpc.ExportExitsResponse{FormatVersion: 2}
	transactions := make(map[string]*wavewalletrpc.ExitAncestryTransaction)
	inputs := make(map[string]*wavewalletrpc.ExitAncestryInput)
	depths := make(map[string]int)
	hashes := make(map[string]chainhash.Hash)
	outpoints := make(map[string]*wire.OutPoint)
	for i, bundle := range bundles {
		if i == 0 {
			result.Network = bundle.Network
			result.GenesisHash = bundle.GenesisHash
		}
		if bundle.Network != result.Network ||
			bundle.GenesisHash != result.GenesisHash {
			return nil, fmt.Errorf("exit bundles belong to " +
				"different chains")
		}
		result.Targets = append(
			result.Targets, &wavewalletrpc.ExitExportTarget{
				TargetOutpoint:    bundle.TargetOutpoint,
				CsvDelay:          bundle.CsvDelay,
				BatchExpiryHeight: bundle.BatchExpiryHeight,
				FinalSweep:        bundle.FinalSweep,
			},
		)
		for _, tx := range bundle.Transactions {
			previous := transactions[tx.Txid]
			if previous != nil {
				if !proto.Equal(previous, tx) {
					return nil, fmt.Errorf("conflicting "+
						"transaction %s", tx.Txid)
				}

				continue
			}
			hash, err := chainhash.NewHashFromStr(tx.Txid)
			if err != nil {
				return nil, err
			}
			hashes[tx.Txid] = *hash
			depth := 0
			for _, parent := range tx.ParentTxids {
				if transactions[parent] == nil {
					return nil, fmt.Errorf("missing "+
						"parent %s", parent)
				}
				depth = max(depth, depths[parent]+1)
			}
			depths[tx.Txid] = depth
			transactions[tx.Txid] = tx
			result.Transactions = append(result.Transactions, tx)
		}
		for _, input := range bundle.ExternalInputs {
			previous := inputs[input.Outpoint]
			if previous != nil {
				if err := mergeExitInput(
					previous, input,
				); err != nil {
					return nil, err
				}

				continue
			}
			outpoint, err := wire.NewOutPointFromString(
				input.Outpoint,
			)
			if err != nil {
				return nil, err
			}
			outpoints[input.Outpoint] = outpoint
			inputs[input.Outpoint] = input
			result.ExternalInputs = append(
				result.ExternalInputs, input,
			)
		}
	}
	slices.SortFunc(result.Transactions, func(
		a, b *wavewalletrpc.ExitAncestryTransaction) int {

		if order := cmp.Compare(
			depths[a.Txid], depths[b.Txid],
		); order != 0 {
			return order
		}
		first, second := hashes[a.Txid], hashes[b.Txid]

		return bytes.Compare(first[:], second[:])
	})
	// Each individual bundle already orders inputs by raw hash/index.
	// Comparing their fixed-width reversed hashes preserves that
	// convention.
	slices.SortFunc(result.ExternalInputs, func(
		a, b *wavewalletrpc.ExitAncestryInput) int {

		first := outpoints[a.Outpoint]
		second := outpoints[b.Outpoint]
		if order := bytes.Compare(
			first.Hash[:], second.Hash[:],
		); order != 0 {
			return order
		}

		return cmp.Compare(first.Index, second.Index)
	})

	return result, nil
}

// mergeExitInput combines optional public evidence only when both exported
// references identify exactly the same commitment output and sweep path.
func mergeExitInput(previous, input *wavewalletrpc.ExitAncestryInput) error {
	conflicting := previous.AmountSat != input.AmountSat ||
		previous.PkScriptHex != input.PkScriptHex
	if previous.CommitmentSweep != nil && input.CommitmentSweep != nil {
		conflicting = conflicting || !proto.Equal(
			previous.CommitmentSweep, input.CommitmentSweep,
		)
	}
	if conflicting {
		return fmt.Errorf("conflicting external input %s",
			input.Outpoint)
	}
	if previous.CommitmentSweep == nil {
		previous.CommitmentSweep = input.CommitmentSweep
	}

	return nil
}
