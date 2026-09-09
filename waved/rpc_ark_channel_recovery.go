package waved

import (
	"context"

	"github.com/lightninglabs/wavelength/lnruntime"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ExportOORRecoveryPackage returns the immutable local lineage for one exact
// output created by this daemon. It does not install ownership or watches.
func (r *RPCServer) ExportOORRecoveryPackage(ctx context.Context,
	req *waverpc.ExportOORRecoveryPackageRequest) (
	*waverpc.ExportOORRecoveryPackageResponse, error) {

	if r == nil || r.server == nil || r.server.vtxoStore == nil {
		return nil, status.Error(
			codes.Unavailable,
			"OOR recovery exporter is unavailable",
		)
	}
	source, err := lnruntime.OORRecoverySourceFromRPC(
		req.GetSource(),
	)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	packages := r.newLocalOORArtifactStore()
	if packages == nil {
		return nil, status.Error(
			codes.Unavailable, "OOR artifact store is unavailable",
		)
	}
	terms, err := r.server.fetchOperatorTerms(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "fetch Ark "+
			"operator terms: %v", err)
	}
	exporter := &arkChannelRecoveryArchive{
		vtxos: r.server.vtxoStore, packages: packages,
	}
	recovery, err := exporter.ExportOORRecoveryPackage(
		ctx, source, terms.PubKey,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "export OOR "+
			"recovery package: %v", err)
	}
	message, err := lnruntime.ChannelRecoveryToRPC(recovery)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encode OOR "+
			"recovery package: %v", err)
	}

	return &waverpc.ExportOORRecoveryPackageResponse{
		Recovery: message,
	}, nil
}
