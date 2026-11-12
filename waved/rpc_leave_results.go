package waved

import (
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/lightninglabs/wavelength/waverpc"
)

// leaveVTXOsResponse preserves definitive per-input failures alongside queued
// inputs. Wallet registration errors return separately and cannot be converted
// into per-input failure: an intent may already have been accepted.
func leaveVTXOsResponse(targets []wire.OutPoint,
	response *wallet.LeaveVTXOsResponse) *waverpc.LeaveVTXOsResponse {

	result := &waverpc.LeaveVTXOsResponse{
		Status:          "queued",
		QueuedOutpoints: make([]string, 0, response.LeavingCount),
		Errors:          make(map[string]string, len(response.Errors)),
	}
	for op, err := range response.Errors {
		result.Errors[op.String()] = err.Error()
	}
	for _, op := range targets {
		if _, failed := response.Errors[op]; !failed {
			result.QueuedOutpoints = append(
				result.QueuedOutpoints, op.String(),
			)
		}
	}

	return result
}
