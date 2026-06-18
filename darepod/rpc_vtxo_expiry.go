package darepod

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/indexer"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightninglabs/lndclient"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GetVTXOExpiryInfo returns the daemon's authoritative expiry posture for a
// VTXO identified either by local outpoint or indexed pkScript.
func (r *RPCServer) GetVTXOExpiryInfo(ctx context.Context,
	req *daemonrpc.GetVTXOExpiryInfoRequest) (
	*daemonrpc.GetVTXOExpiryInfoResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	if req == nil {
		return nil, status.Error(
			codes.InvalidArgument, "request is required",
		)
	}

	currentHeight := req.GetCurrentHeight()
	if currentHeight == 0 {
		height, err := r.currentBlockHeight(ctx)
		if err != nil {
			return nil, err
		}

		currentHeight = height
	}

	switch target := req.GetTarget().(type) {
	case *daemonrpc.GetVTXOExpiryInfoRequest_Outpoint:
		return r.getLocalVTXOExpiryInfo(
			ctx, target.Outpoint, currentHeight,
		)

	case *daemonrpc.GetVTXOExpiryInfoRequest_PkScript:
		return r.getIndexedVTXOExpiryInfo(
			ctx, target.PkScript, req.GetStatusFilter(),
			currentHeight,
		)

	default:
		return nil, status.Error(
			codes.InvalidArgument,
			"outpoint or pk_script is required",
		)
	}
}

// getLocalVTXOExpiryInfo resolves a locally persisted VTXO by outpoint and
// classifies it with the descriptor's full ancestry metadata.
func (r *RPCServer) getLocalVTXOExpiryInfo(ctx context.Context, outpoint string,
	currentHeight int32) (*daemonrpc.GetVTXOExpiryInfoResponse, error) {

	if outpoint == "" {
		return nil, status.Error(
			codes.InvalidArgument, "missing outpoint",
		)
	}
	if r.server.vtxoStore == nil {
		return nil, status.Error(
			codes.Internal, "vtxo store not initialized",
		)
	}

	op, err := parseOutpointString(outpoint)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid "+
			"outpoint: %v", err)
	}

	desc, err := r.server.vtxoStore.GetVTXO(ctx, op)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &daemonrpc.GetVTXOExpiryInfoResponse{}, nil
		}

		return nil, status.Errorf(codes.Internal, "get vtxo: %v", err)
	}

	protoVTXO := descriptorToProto(desc)
	protoVTXO.ExpiryInfo = expiryInfoFromDescriptor(
		desc, currentHeight,
	)

	return &daemonrpc.GetVTXOExpiryInfoResponse{
		Found:      true,
		ExpiryInfo: protoVTXO.GetExpiryInfo(),
		Vtxo:       protoVTXO,
	}, nil
}

// getIndexedVTXOExpiryInfo resolves one indexed VTXO by pkScript and
// classifies it with the metadata returned by the authoritative indexer.
func (r *RPCServer) getIndexedVTXOExpiryInfo(ctx context.Context,
	pkScript []byte, filters []daemonrpc.VTXOStatus, currentHeight int32) (
	*daemonrpc.GetVTXOExpiryInfoResponse, error) {

	if len(pkScript) == 0 {
		return nil, status.Error(
			codes.InvalidArgument, "missing pk_script",
		)
	}
	if r.server.indexer == nil {
		return nil, status.Error(
			codes.Internal, "indexer client not initialized",
		)
	}

	statusFilter := make([]arkrpc.VTXOStatus, 0, len(filters))
	for i := range filters {
		st, err := daemonStatusToIndexerStatus(filters[i])
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"invalid status filter: %v", err)
		}

		statusFilter = append(statusFilter, st)
	}

	resp, err := r.server.indexer.ListVTXOsByScriptsTaproot(
		ctx,
		[]indexer.TaprootScriptScope{{
			PkScript: append([]byte(nil), pkScript...),
		}},
		nil, 1, statusFilter,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "indexer query "+
			"failed: %v", err)
	}

	vtxos := vtxo.FlattenListVTXOsByScriptsResponse(resp)
	if len(vtxos) == 0 {
		return &daemonrpc.GetVTXOExpiryInfoResponse{}, nil
	}

	protoVTXO, err := indexedVTXOToProto(vtxos[0], currentHeight)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "convert indexed "+
			"vtxo: %v", err)
	}

	return &daemonrpc.GetVTXOExpiryInfoResponse{
		Found:      true,
		ExpiryInfo: protoVTXO.GetExpiryInfo(),
		Vtxo:       protoVTXO,
	}, nil
}

// currentBlockHeight returns the daemon's best chain height from the active
// chain backend.
func (r *RPCServer) currentBlockHeight(ctx context.Context) (int32, error) {
	var (
		height int32
		err    error
		found  bool
	)

	r.server.lnd.WhenSome(func(lndSvc *lndclient.GrpcLndServices) {
		_, bestHeight, bestErr := lndSvc.ChainKit.GetBestBlock(ctx)
		if bestErr != nil {
			err = fmt.Errorf("fetch lnd best block: %w", bestErr)

			return
		}

		height = bestHeight
		found = true
	})
	if err != nil {
		return 0, status.Errorf(codes.Unavailable, "fetch block "+
			"height: %v", err)
	}
	if found {
		return height, nil
	}

	if r.server.chainBackend == nil {
		return 0, status.Error(
			codes.Unavailable, "chain backend not initialized",
		)
	}

	height, _, err = r.server.chainBackend.BestBlock(ctx)
	if err != nil {
		return 0, status.Errorf(codes.Unavailable, "fetch block "+
			"height: %v", err)
	}

	return height, nil
}

// expiryInfoFromDescriptor classifies a locally stored VTXO descriptor at the
// supplied chain height.
func expiryInfoFromDescriptor(desc *vtxo.Descriptor,
	currentHeight int32) *daemonrpc.VTXOExpiryInfo {

	if desc == nil {
		return nil
	}

	return expiryInfoFromTiming(
		desc.BatchExpiry, desc.RelativeExpiry,
		uint32(
			desc.MaxTreeDepth(),
		),
		uint32(desc.ChainDepth),
		currentHeight,
	)
}

// expiryInfoFromIndexedVTXO classifies an indexer VTXO at the supplied chain
// height using max tree depth across its ancestry paths.
func expiryInfoFromIndexedVTXO(indexed *arkrpc.VTXO,
	currentHeight int32) *daemonrpc.VTXOExpiryInfo {

	if indexed == nil {
		return nil
	}

	var maxTreeDepth uint32
	for _, ancestry := range indexed.GetAncestryPaths() {
		if ancestry.GetTreeDepth() > maxTreeDepth {
			maxTreeDepth = ancestry.GetTreeDepth()
		}
	}

	return expiryInfoFromTiming(
		indexed.GetBatchExpiryHeight(), indexed.GetRelativeExpiry(),
		maxTreeDepth, indexed.GetChainDepth(), currentHeight,
	)
}

// expiryInfoFromTiming classifies one VTXO from the timing inputs used by the
// wallet expiry policy.
func expiryInfoFromTiming(batchExpiry int32, relativeExpiry, maxTreeDepth,
	chainDepth uint32, currentHeight int32) *daemonrpc.VTXOExpiryInfo {

	info := &daemonrpc.VTXOExpiryInfo{
		CurrentHeight:  currentHeight,
		BatchExpiry:    batchExpiry,
		RelativeExpiry: relativeExpiry,
		MaxTreeDepth:   maxTreeDepth,
		ChainDepth:     chainDepth,
		Status: daemonrpc.
			VTXOExpiryStatus_VTXO_EXPIRY_STATUS_UNKNOWN,
	}

	if batchExpiry <= 0 || currentHeight <= 0 {
		return info
	}

	desc := &vtxo.Descriptor{
		BatchExpiry:    batchExpiry,
		RelativeExpiry: relativeExpiry,
		Ancestry: []vtxo.Ancestry{{
			TreeDepth: maxTreeDepth,
		}},
	}
	cfg := vtxo.DefaultExpiryConfig()

	info.BlocksRemaining = vtxo.BlocksUntilExpiry(desc, currentHeight)
	info.CriticalThresholdBlocks = cfg.CalculateCriticalThreshold(desc)
	info.RefreshThresholdBlocks = cfg.CalculateRefreshThreshold(desc)
	info.Status = expiryStatusToProto(
		cfg.CheckExpiry(desc, currentHeight),
	)

	return info
}

// expiryStatusToProto maps the wallet expiry enum onto the public daemon RPC
// enum.
func expiryStatusToProto(status vtxo.ExpiryStatus) daemonrpc.VTXOExpiryStatus {
	switch status {
	case vtxo.ExpiryStatusSafe:
		return daemonrpc.VTXOExpiryStatus_VTXO_EXPIRY_STATUS_SAFE

	case vtxo.ExpiryStatusNeedsRefresh:
		return daemonrpc.
			VTXOExpiryStatus_VTXO_EXPIRY_STATUS_NEEDS_REFRESH

	case vtxo.ExpiryStatusCritical:
		return daemonrpc.VTXOExpiryStatus_VTXO_EXPIRY_STATUS_CRITICAL

	case vtxo.ExpiryStatusExpired:
		return daemonrpc.VTXOExpiryStatus_VTXO_EXPIRY_STATUS_EXPIRED

	default:
		return daemonrpc.VTXOExpiryStatus_VTXO_EXPIRY_STATUS_UNKNOWN
	}
}
