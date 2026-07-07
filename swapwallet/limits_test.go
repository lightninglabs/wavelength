//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/stretchr/testify/require"
)

// TestCheckReceiveLimits exercises the Recv pre-flight check against
// the operator-advertised per-VTXO maximum and total balance cap.
func TestCheckReceiveLimits(t *testing.T) {
	t.Parallel()

	const (
		maxVTXO = uint64(10_000_000)
		maxBal  = uint64(10_000_000)
	)

	serverInfo := func(maxV, maxB uint64) *daemonrpc.GetInfoResponse {
		return &daemonrpc.GetInfoResponse{
			ServerInfo: &daemonrpc.ServerInfo{
				MaxVtxoAmount:  maxV,
				MaxUserBalance: maxB,
			},
		}
	}

	errRPC := errors.New("rpc unavailable")

	tests := []struct {
		name       string
		info       *daemonrpc.GetInfoResponse
		infoErr    error
		balance    *daemonrpc.GetBalanceResponse
		balanceErr error
		amt        btcutil.Amount
		wantErr    error
	}{{
		// A daemon that has not fetched operator terms yet skips
		// the checks rather than failing closed.
		name: "no server info",
		info: &daemonrpc.GetInfoResponse{},
		amt:  1_000_000_000,
	}, {
		// Zero-valued limits mean the operator advertises no caps.
		name:    "limits disabled",
		info:    serverInfo(0, 0),
		balance: &daemonrpc.GetBalanceResponse{},
		amt:     1_000_000_000,
	}, {
		name:    "within limits",
		info:    serverInfo(maxVTXO, maxBal),
		balance: &daemonrpc.GetBalanceResponse{},
		amt:     5_000_000,
	}, {
		// A Lightning receive settles into a single VTXO, so the
		// invoice amount must fit under the per-VTXO maximum.
		name:    "exceeds per-vtxo max",
		info:    serverInfo(maxVTXO, maxBal),
		amt:     btcutil.Amount(maxVTXO) + 1,
		wantErr: ErrAmountExceedsVTXOLimit,
	}, {
		// Live VTXOs plus all boarding inflight count toward the
		// balance cap.
		name: "exceeds balance cap",
		info: serverInfo(maxVTXO, maxBal),
		balance: &daemonrpc.GetBalanceResponse{
			VtxoBalanceSat:         4_000_000,
			BoardingConfirmedSat:   2_000_000,
			BoardingUnconfirmedSat: 1_000_000,
			BoardingAdoptedSat:     1_000_000,
		},
		amt:     2_000_001,
		wantErr: ErrBalanceLimitExceeded,
	}, {
		// Exactly filling the cap is allowed.
		name: "exactly at cap",
		info: serverInfo(maxVTXO, maxBal),
		balance: &daemonrpc.GetBalanceResponse{
			VtxoBalanceSat: 4_000_000,
		},
		amt: 6_000_000,
	}, {
		// A transient GetInfo failure fails OPEN: the pre-flight is
		// advisory and the operator re-validates server-side, so an
		// otherwise-oversized receive is admitted rather than blocked.
		name:    "getinfo error fails open",
		infoErr: errRPC,
		amt:     1_000_000_000,
	}, {
		// A transient GetBalance failure skips only the balance cap;
		// the per-VTXO check already passed, and the receive proceeds.
		name:       "getbalance error fails open",
		info:       serverInfo(maxVTXO, maxBal),
		balanceErr: errRPC,
		amt:        6_000_000,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rpc := &fakeRPCServer{
				getInfoResp:    tc.info,
				getInfoErr:     tc.infoErr,
				getBalanceResp: tc.balance,
				getBalanceErr:  tc.balanceErr,
			}

			err := checkReceiveLimits(
				context.Background(), rpc, btclog.Disabled,
				tc.amt,
			)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)

				return
			}

			require.NoError(t, err)
		})
	}
}
