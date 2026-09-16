package waved

import (
	"context"
	"testing"

	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestGetBatchScheduleRefreshes keeps discovery tied to current operator time.
func TestGetBatchScheduleRefreshes(t *testing.T) {
	client := &stubArkServiceClient{resp: &arkrpc.GetInfoResponse{
		BatchSchedule: &arkrpc.BatchScheduleInfo{
			Version: 2, NextSlotIndex: 3, ServerTimeUnix: 100,
			AdmissionEnabled: true,
		},
	}}
	rpc := &RPCServer{server: &Server{
		arkClient: client, arkProtocolVersion: 1,
	}}
	ctx := context.Background()
	resp, err := rpc.GetBatchSchedule(
		ctx, &waverpc.GetBatchScheduleRequest{},
	)
	require.NoError(t, err)
	require.Equal(t, uint64(3), resp.Schedule.NextSlotIndex)
	client.resp = &arkrpc.GetInfoResponse{
		BatchSchedule: &arkrpc.BatchScheduleInfo{
			Version: 2, NextSlotIndex: 4, ServerTimeUnix: 200,
		},
	}
	resp, err = rpc.GetBatchSchedule(
		ctx, &waverpc.GetBatchScheduleRequest{},
	)
	require.NoError(t, err)
	require.Equal(t, uint64(4), resp.Schedule.NextSlotIndex)
	require.Equal(t, int64(200), resp.Schedule.ServerTimeUnix)
	require.False(t, resp.Schedule.AdmissionEnabled)
	require.Equal(t, 2, client.calls)

	client.resp = &arkrpc.GetInfoResponse{}
	resp, err = rpc.GetBatchSchedule(
		ctx, &waverpc.GetBatchScheduleRequest{},
	)
	require.NoError(t, err)
	require.Nil(t, resp.Schedule)
	client.err = context.DeadlineExceeded
	_, err = rpc.GetBatchSchedule(ctx, &waverpc.GetBatchScheduleRequest{})
	require.Equal(t, codes.Unavailable, status.Code(err))
}
