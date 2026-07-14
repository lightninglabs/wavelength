package waved

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/indexer"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/lightningnetwork/lnd/keychain"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	customRefreshMetadataPageSize uint32 = 128

	customRefreshAmountMismatch = "custom refresh input %s amount_sat " +
		"does not match indexed VTXO"
	customRefreshPkScriptMismatch = "custom refresh input %s pk_script " +
		"does not match indexed VTXO"
)

func (r *RPCServer) enrichCustomRefreshInputs(ctx context.Context,
	inputs []wallet.CustomRefreshInput, clientKey keychain.KeyDescriptor,
	operatorKey *btcec.PublicKey) error {

	if r.server.indexer == nil {
		return status.Error(
			codes.Internal, "indexer client not initialized",
		)
	}

	for i := range inputs {
		inputs[i].ClientKey = clientKey
		inputs[i].OperatorKey = operatorKey

		meta, err := r.resolveCustomRefreshMetadata(ctx, inputs[i])
		if err != nil {
			return err
		}

		inputs[i].RoundID = meta.RoundID
		inputs[i].CommitmentTxID = meta.CommitmentTxID
		inputs[i].BatchExpiry = meta.BatchExpiry
		inputs[i].ChainDepth = meta.ChainDepth
		inputs[i].CreatedHeight = meta.CreatedHeight
		inputs[i].Ancestry = meta.Ancestry
	}

	return nil
}

func (r *RPCServer) resolveCustomRefreshMetadata(ctx context.Context,
	input wallet.CustomRefreshInput) (customRefreshMetadata, error) {

	var cursor []byte
	statusFilter := []arkrpc.VTXOStatus{
		arkrpc.VTXOStatus_VTXO_STATUS_LIVE,
	}

	for {
		resp, err := r.server.indexer.ListVTXOsByScriptsTaproot(
			ctx,
			[]indexer.TaprootScriptScope{{
				PkScript: append(
					[]byte(nil), input.PkScript...,
				),
			}},
			cursor, customRefreshMetadataPageSize, statusFilter,
		)
		if err != nil {
			return customRefreshMetadata{}, status.Errorf(
				codes.Internal, "custom refresh metadata "+
					"query failed: %v", err)
		}

		candidates := vtxo.FlattenListVTXOsByScriptsResponse(resp)
		for _, candidate := range candidates {
			op, ok := indexerVTXOOutpoint(candidate)
			if !ok || op != input.Outpoint {
				continue
			}

			if candidate.GetValueSat() != uint64(input.Amount) {
				return customRefreshMetadata{}, status.Errorf(
					codes.InvalidArgument,
					customRefreshAmountMismatch,
					input.Outpoint)
			}
			if !bytes.Equal(
				candidate.GetPkScript(), input.PkScript,
			) {
				return customRefreshMetadata{}, status.Errorf(
					codes.InvalidArgument,
					customRefreshPkScriptMismatch,
					input.Outpoint)
			}

			meta, err := customRefreshMetadataFromRPC(candidate)
			if err != nil {
				return customRefreshMetadata{}, status.Errorf(
					codes.Internal, "custom refresh "+
						"indexed metadata for %s: %v",
					input.Outpoint, err)
			}

			return meta, nil
		}

		nextCursor := resp.GetNextCursor()
		if len(nextCursor) == 0 {
			break
		}
		cursor = append(cursor[:0], nextCursor...)
	}

	return customRefreshMetadata{}, status.Errorf(codes.FailedPrecondition,
		"custom refresh input %s not found in live indexer inventory",
		input.Outpoint)
}

type customRefreshMetadata struct {
	RoundID        string
	CommitmentTxID chainhash.Hash
	BatchExpiry    int32
	ChainDepth     int
	CreatedHeight  int32
	Ancestry       []vtxo.Ancestry
}

func customRefreshMetadataFromRPC(candidate *arkrpc.VTXO) (
	customRefreshMetadata, error) {

	if candidate == nil {
		return customRefreshMetadata{}, fmt.Errorf("indexer vtxo " +
			"must be provided")
	}

	if candidate.GetRoundId() == "" {
		return customRefreshMetadata{}, fmt.Errorf("indexer vtxo " +
			"missing round id")
	}

	if len(candidate.GetCommitmentTxid()) != chainhash.HashSize {
		return customRefreshMetadata{}, fmt.Errorf("indexer vtxo " +
			"missing commitment txid")
	}

	ancestry, err := vtxo.AncestryFromRPC(candidate.GetAncestryPaths())
	if err != nil {
		return customRefreshMetadata{}, fmt.Errorf("convert ancestry "+
			"paths: %w", err)
	}

	var commitmentTxID chainhash.Hash
	copy(commitmentTxID[:], candidate.GetCommitmentTxid())

	return customRefreshMetadata{
		RoundID:        candidate.GetRoundId(),
		CommitmentTxID: commitmentTxID,
		BatchExpiry:    candidate.GetBatchExpiryHeight(),
		ChainDepth:     int(candidate.GetChainDepth()),
		CreatedHeight:  candidate.GetCreatedHeight(),
		Ancestry:       ancestry,
	}, nil
}

func indexerVTXOOutpoint(candidate *arkrpc.VTXO) (wire.OutPoint, bool) {
	if candidate == nil || candidate.GetOutpoint() == nil {
		return wire.OutPoint{}, false
	}

	op := candidate.GetOutpoint()
	if len(op.GetTxid()) != chainhash.HashSize {
		return wire.OutPoint{}, false
	}

	var outpoint wire.OutPoint
	copy(outpoint.Hash[:], op.GetTxid())
	outpoint.Index = op.GetVout()

	return outpoint, true
}
