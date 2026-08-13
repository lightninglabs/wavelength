package waved

import (
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/lnruntime"
	"github.com/lightninglabs/wavelength/waverpc"
)

// ExportOORRecoveryPackage returns the immutable local lineage for one exact
// output created by this daemon. It does not install ownership or watches.
func (r *RPCServer) ExportOORRecoveryPackage(ctx context.Context,
	req *waverpc.ExportOORRecoveryPackageRequest) (
	*waverpc.ExportOORRecoveryPackageResponse, error) {

	if r == nil || r.server == nil || r.server.vtxoStore == nil {
		return nil, fmt.Errorf("OOR recovery exporter is unavailable")
	}
	source, err := lnruntime.ReceiveClaimRecoverySourceFromRPC(
		req.GetSource(),
	)
	if err != nil {
		return nil, err
	}
	packages := r.newLocalOORArtifactStore()
	if packages == nil {
		return nil, fmt.Errorf("OOR artifact store is unavailable")
	}
	terms, err := r.server.fetchOperatorTerms(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch Ark operator terms: %w", err)
	}
	exporter := &arkChannelRecoveryArchive{
		vtxos: r.server.vtxoStore, packages: packages,
	}
	recovery, err := exporter.ExportOORRecoveryPackage(
		ctx, source, terms.PubKey,
	)
	if err != nil {
		return nil, err
	}
	message, err := lnruntime.ChannelRecoveryToRPC(recovery)
	if err != nil {
		return nil, err
	}

	return &waverpc.ExportOORRecoveryPackageResponse{
		Recovery: message,
	}, nil
}
