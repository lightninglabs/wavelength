package waved

import (
	"context"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/unroll"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ExportExits exports a bounded set of distinct targets, reusing witness
// validation for shared ancestry. Failure returns no partial result. Like a
// single export, this does not reserve targets or freeze cooperative activity.
func (r *RPCServer) ExportExits(ctx context.Context,
	requests []*ExitExportRequest) ([]*ExitExport, error) {

	if len(requests) == 0 || len(requests) > 100 {
		return nil, status.Error(
			codes.InvalidArgument,
			"provide between 1 and 100 exit targets",
		)
	}
	seen := make(map[wire.OutPoint]bool)
	for _, req := range requests {
		if req == nil {
			return nil, status.Error(
				codes.InvalidArgument, "exit target is nil",
			)
		}
		outpoint, err := parseOutpointString(req.Outpoint)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"invalid outpoint: %v", err)
		}
		if seen[outpoint] {
			return nil, status.Errorf(codes.InvalidArgument,
				"duplicate exit target %s", outpoint)
		}
		seen[outpoint] = true
	}
	var builder unroll.SignedAncestryBuilder
	exports := make([]*ExitExport, 0, len(requests))
	for _, req := range requests {
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		exported, err := r.exportExit(ctx, req, &builder)
		if err != nil {
			return nil, err
		}
		exports = append(exports, exported)
	}

	return exports, nil
}
