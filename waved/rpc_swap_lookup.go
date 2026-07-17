package waved

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/indexer"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GetIndexedVTXOByPkScript queries the authoritative indexer for the first
// VTXO matching the given script and status filter.
func (r *RPCServer) GetIndexedVTXOByPkScript(ctx context.Context,
	req *waverpc.GetIndexedVTXOByPkScriptRequest) (
	*waverpc.GetIndexedVTXOByPkScriptResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	if req == nil || len(req.PkScript) == 0 {
		return nil, status.Error(
			codes.InvalidArgument, "missing pk_script",
		)
	}

	if r.server.indexer == nil {
		return nil, status.Error(
			codes.Internal, "indexer client not initialized",
		)
	}

	statusFilter := make([]arkrpc.VTXOStatus, 0, len(req.StatusFilter))
	for i := range req.StatusFilter {
		st, err := daemonStatusToIndexerStatus(req.StatusFilter[i])
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"invalid status filter: %v", err)
		}

		statusFilter = append(statusFilter, st)
	}

	resp, err := r.server.indexer.ListVTXOsByScriptsTaproot(
		ctx,
		[]indexer.TaprootScriptScope{{
			PkScript: append([]byte(nil), req.PkScript...),
		}},
		nil, 1, statusFilter,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "indexer query "+
			"failed: %v", err)
	}

	vtxos := vtxo.FlattenListVTXOsByScriptsResponse(resp)
	if len(vtxos) == 0 {
		return &waverpc.GetIndexedVTXOByPkScriptResponse{}, nil
	}

	currentHeight, heightErr := r.currentBlockHeight(ctx)
	if heightErr != nil {
		r.server.log.WarnS(ctx, "Unable to fetch block height for "+
			"indexed VTXO expiry info", heightErr)
	}

	vtxo, err := indexedVTXOToProto(
		vtxos[0], currentHeight, r.server.vtxoExpiryConfig(),
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "convert indexed "+
			"vtxo: %v", err)
	}

	return &waverpc.GetIndexedVTXOByPkScriptResponse{
		Vtxo: vtxo,
	}, nil
}

// GetIndexedOORSessionByTxid queries the authoritative indexer for one OOR
// session using a spent script proof and deterministic session txid.
func (r *RPCServer) GetIndexedOORSessionByTxid(ctx context.Context,
	req *waverpc.GetIndexedOORSessionByTxidRequest) (
	*waverpc.GetIndexedOORSessionByTxidResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	switch {
	case req == nil:
		return nil, status.Error(
			codes.InvalidArgument, "request is required",
		)

	case len(req.PkScript) == 0:
		return nil, status.Error(
			codes.InvalidArgument, "missing pk_script",
		)

	case len(req.SessionTxid) != chainhash.HashSize:
		return nil, status.Error(
			codes.InvalidArgument, "session_txid must be 32 bytes",
		)
	}

	if r.server.indexer == nil {
		return nil, status.Error(
			codes.Internal, "indexer client not initialized",
		)
	}

	resp, err := r.server.indexer.GetOORSessionByTxidTaproot(
		ctx, req.PkScript, req.SessionTxid,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "indexer query "+
			"failed: %v", err)
	}

	return &waverpc.GetIndexedOORSessionByTxidResponse{
		ArkPsbt: append([]byte(nil), resp.GetArkPsbt()...),
		CheckpointPsbts: deepCopyByteSlices(
			resp.GetCheckpointPsbts(),
		),
	}, nil
}

// daemonStatusToIndexerStatus converts waverpc VTXO status enums to their
// arkrpc counterparts.
func daemonStatusToIndexerStatus(status waverpc.VTXOStatus) (arkrpc.VTXOStatus,
	error) {

	switch status {
	case waverpc.VTXOStatus_VTXO_STATUS_UNSPECIFIED:
		return arkrpc.VTXOStatus_VTXO_STATUS_UNSPECIFIED, nil

	case waverpc.VTXOStatus_VTXO_STATUS_LIVE:
		return arkrpc.VTXOStatus_VTXO_STATUS_LIVE, nil

	case waverpc.VTXOStatus_VTXO_STATUS_PENDING_FORFEIT:
		return arkrpc.VTXOStatus_VTXO_STATUS_PENDING_FORFEIT, nil

	case waverpc.VTXOStatus_VTXO_STATUS_FORFEITING:
		return arkrpc.VTXOStatus_VTXO_STATUS_FORFEITING, nil

	case waverpc.VTXOStatus_VTXO_STATUS_FORFEITED:
		return arkrpc.VTXOStatus_VTXO_STATUS_FORFEITED, nil

	case waverpc.VTXOStatus_VTXO_STATUS_SPENT:
		return arkrpc.VTXOStatus_VTXO_STATUS_SPENT, nil

	case waverpc.VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT:
		return arkrpc.VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT, nil

	case waverpc.VTXOStatus_VTXO_STATUS_FAILED:
		return arkrpc.VTXOStatus_VTXO_STATUS_FAILED, nil

	case waverpc.VTXOStatus_VTXO_STATUS_SPENDING:
		return 0, fmt.Errorf("indexer does not expose spending status")

	default:
		return 0, fmt.Errorf("unknown VTXO status: %v", status)
	}
}

// indexedVTXOToProto converts an authoritative indexer VTXO into the daemon's
// simplified VTXO response shape.
func indexedVTXOToProto(vtxo *arkrpc.VTXO, currentHeight int32,
	cfg *vtxo.ExpiryConfig) (*waverpc.VTXO, error) {

	if vtxo == nil {
		return nil, fmt.Errorf("vtxo must be provided")
	}

	outpoint := vtxo.GetOutpoint()
	if outpoint == nil {
		return nil, fmt.Errorf("indexer vtxo missing outpoint")
	}

	txid, err := chainhash.NewHash(outpoint.GetTxid())
	if err != nil {
		return nil, fmt.Errorf("parse outpoint txid: %w", err)
	}

	status, err := indexerStatusToDaemonStatus(vtxo.GetStatus())
	if err != nil {
		return nil, err
	}

	commitmentTxid := ""
	if len(vtxo.GetCommitmentTxid()) > 0 {
		commitmentHash, err := chainhash.NewHash(
			vtxo.GetCommitmentTxid(),
		)
		if err != nil {
			return nil, fmt.Errorf("parse commitment txid: %w", err)
		}

		commitmentTxid = commitmentHash.String()
	}

	spentByTxid := ""
	if len(vtxo.GetSpentByTxid()) > 0 {
		spentHash, err := chainhash.NewHash(vtxo.GetSpentByTxid())
		if err != nil {
			return nil, fmt.Errorf("parse spent_by_txid: %w", err)
		}

		spentByTxid = spentHash.String()
	}

	return &waverpc.VTXO{
		Outpoint:       fmt.Sprintf("%s:%d", txid, outpoint.GetVout()),
		AmountSat:      int64(vtxo.GetValueSat()),
		Status:         status,
		BatchExpiry:    vtxo.GetBatchExpiryHeight(),
		RoundId:        vtxo.GetRoundId(),
		CreatedHeight:  vtxo.GetCreatedHeight(),
		RelativeExpiry: vtxo.GetRelativeExpiry(),
		PkScript:       fmt.Sprintf("%x", vtxo.GetPkScript()),
		CommitmentTxid: commitmentTxid,
		ChainDepth:     vtxo.GetChainDepth(),
		OorFinalCheckpointPsbts: deepCopyByteSlices(
			vtxo.GetOorFinalCheckpointPsbts(),
		),
		SpentByTxid: spentByTxid,
		ExpiryInfo: expiryInfoFromIndexedVTXO(
			vtxo, currentHeight, cfg,
		),
	}, nil
}

// indexerStatusToDaemonStatus converts arkrpc VTXO status enums to waverpc.
func indexerStatusToDaemonStatus(status arkrpc.VTXOStatus) (waverpc.VTXOStatus,
	error) {

	switch status {
	case arkrpc.VTXOStatus_VTXO_STATUS_UNSPECIFIED:
		return waverpc.VTXOStatus_VTXO_STATUS_UNSPECIFIED, nil

	case arkrpc.VTXOStatus_VTXO_STATUS_LIVE:
		return waverpc.VTXOStatus_VTXO_STATUS_LIVE, nil

	case arkrpc.VTXOStatus_VTXO_STATUS_PENDING_FORFEIT:
		return waverpc.VTXOStatus_VTXO_STATUS_PENDING_FORFEIT, nil

	case arkrpc.VTXOStatus_VTXO_STATUS_FORFEITING:
		return waverpc.VTXOStatus_VTXO_STATUS_FORFEITING, nil

	case arkrpc.VTXOStatus_VTXO_STATUS_FORFEITED:
		return waverpc.VTXOStatus_VTXO_STATUS_FORFEITED, nil

	case arkrpc.VTXOStatus_VTXO_STATUS_SPENT:
		return waverpc.VTXOStatus_VTXO_STATUS_SPENT, nil

	case arkrpc.VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT:
		return waverpc.VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT, nil

	case arkrpc.VTXOStatus_VTXO_STATUS_FAILED:
		return waverpc.VTXOStatus_VTXO_STATUS_FAILED, nil

	default:
		return 0, fmt.Errorf("unknown indexer VTXO status: %v", status)
	}
}

// deepCopyByteSlices clones each byte slice in src.
func deepCopyByteSlices(src [][]byte) [][]byte {
	out := make([][]byte, 0, len(src))
	for i := range src {
		out = append(out, append([]byte(nil), src[i]...))
	}

	return out
}
