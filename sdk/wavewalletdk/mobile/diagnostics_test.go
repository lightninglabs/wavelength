//go:build mobile && wavewalletrpc && swapruntime

package mobile

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// diagnosticInventoryRPC models the lightweight list/detail expiry contract.
type diagnosticInventoryRPC struct {
	waverpc.DaemonServiceClient
	seen      []int32
	detailErr error
}

// ListVTXOs returns no expiry info, matching the actual lightweight RPC.
func (r *diagnosticInventoryRPC) ListVTXOs(context.Context,
	*waverpc.ListVTXOsRequest, ...grpc.CallOption) (
	*waverpc.ListVTXOsResponse, error) {

	return &waverpc.ListVTXOsResponse{Vtxos: []*waverpc.VTXO{
		{
			Outpoint: "due",
		}, {
			Outpoint: "safe",
		}, {
			Outpoint: "expired",
		}, {
			Outpoint: "unknown",
		}, {},
	}}, nil
}

// GetVTXOExpiryInfo returns the authoritative state and tracks shared tip use.
func (r *diagnosticInventoryRPC) GetVTXOExpiryInfo(_ context.Context,
	req *waverpc.GetVTXOExpiryInfoRequest, _ ...grpc.CallOption) (
	*waverpc.GetVTXOExpiryInfoResponse, error) {

	if r.detailErr != nil {
		return nil, r.detailErr
	}
	r.seen = append(r.seen, req.GetCurrentHeight())
	due := waverpc.VTXOExpiryStatus_VTXO_EXPIRY_STATUS_NEEDS_REFRESH
	statuses := map[string]waverpc.VTXOExpiryStatus{
		"due":     due,
		"safe":    waverpc.VTXOExpiryStatus_VTXO_EXPIRY_STATUS_SAFE,
		"expired": waverpc.VTXOExpiryStatus_VTXO_EXPIRY_STATUS_EXPIRED,
		"unknown": waverpc.VTXOExpiryStatus_VTXO_EXPIRY_STATUS_UNKNOWN,
	}

	return &waverpc.GetVTXOExpiryInfoResponse{Found: true,
		ExpiryInfo: &waverpc.VTXOExpiryInfo{
			Status:        statuses[req.GetOutpoint()],
			CurrentHeight: 100,
		}}, nil
}

// TestDiagnosticInventoryLoadsExpiry prevents an always-empty refresh filter.
func TestDiagnosticInventoryLoadsExpiry(t *testing.T) {
	rpc := &diagnosticInventoryRPC{}
	result, err := diagnosticVTXOs(t.Context(), rpc, nil)
	require.NoError(t, err)
	require.Equal(t, []int32{0, 100, 100, 100}, rpc.seen)
	require.Equal(
		t, waverpc.VTXOExpiryStatus_VTXO_EXPIRY_STATUS_NEEDS_REFRESH,
		result.Vtxos[0].ExpiryInfo.Status,
	)
	require.Equal(
		t, waverpc.VTXOExpiryStatus_VTXO_EXPIRY_STATUS_SAFE,
		result.Vtxos[1].ExpiryInfo.Status,
	)
	require.Equal(
		t, waverpc.VTXOExpiryStatus_VTXO_EXPIRY_STATUS_EXPIRED,
		result.Vtxos[2].ExpiryInfo.Status,
	)
	require.Equal(
		t, waverpc.VTXOExpiryStatus_VTXO_EXPIRY_STATUS_UNKNOWN,
		result.Vtxos[3].ExpiryInfo.Status,
	)
	require.Nil(t, result.Vtxos[4].ExpiryInfo)
	rpc.detailErr = errors.New("expiry unavailable")
	_, err = diagnosticVTXOs(t.Context(), rpc, nil)
	require.ErrorIs(t, err, rpc.detailErr)
}

// TestDiagnosticJSONPreservesPublicValuesAndUnknownAmounts checks the wire
// adapter without starting a wallet or replacing an absent amount with zero.
func TestDiagnosticJSONPreservesPublicValuesAndUnknownAmounts(t *testing.T) {
	response := &waverpc.ListOORSessionsResponse{
		Sessions: []*waverpc.OORSessionInfo{
			{
				SessionId: "known",
			}, {
				SessionId: "unknown",
			},
		},
	}

	raw, err := marshalDiagnostic(response, map[string]int64{"known": 3000})
	require.NoError(t, err)
	var decoded struct {
		Sessions []map[string]json.RawMessage `json:"sessions"`
	}
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.JSONEq(t, `"3000"`, string(decoded.Sessions[0]["amount_sat"]))
	require.NotContains(t, decoded.Sessions[1], "amount_sat")
	raw, err = marshalDiagnostic(
		&waverpc.ListOORSessionsResponse{}, map[string]int64{},
	)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"sessions":[]`)
}
