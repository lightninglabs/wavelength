package swaps

import (
	"context"

	"github.com/lightninglabs/wavelength/swaprpc"
)

// RecoverableSwapPageServerConn is an additive recovery transport capability.
// Callers validate the advertised network and sign each exact page request.
type RecoverableSwapPageServerConn interface {
	// GetSwapRecoveryInfo returns the TLS-authenticated signing audience.
	GetSwapRecoveryInfo(context.Context) (
		*swaprpc.GetSwapRecoveryInfoResponse, error)

	// ListRecoverableSwapsPage returns at most pageSize candidates and an
	// opaque continuation token. Zero selects the server default of 100.
	ListRecoverableSwapsPage(context.Context, *swaprpc.SwapOwnerProof,
		uint32, []byte) (*swaprpc.ListRecoverableSwapsResponse, error)
}
