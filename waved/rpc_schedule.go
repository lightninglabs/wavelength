package waved

import (
	"context"

	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GetBatchSchedule queries current operator time and slot opportunities. An
// absent schedule means the connected operator does not publish the capability.
func (r *RPCServer) GetBatchSchedule(ctx context.Context,
	_ *waverpc.GetBatchScheduleRequest) (*waverpc.GetBatchScheduleResponse,
	error) {

	client := r.server.operatorArkClient()
	if client == nil {
		return nil, status.Error(
			codes.Unavailable,
			"operator connection not initialized",
		)
	}
	info, err := client.GetInfo(ctx, &arkrpc.GetInfoRequest{
		SupportedArkVersions: []uint32{r.server.arkProtocolVersion},
	})
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "query operator "+
			"schedule: %v", err)
	}

	return &waverpc.GetBatchScheduleResponse{
		Schedule: info.BatchSchedule,
	}, nil
}
