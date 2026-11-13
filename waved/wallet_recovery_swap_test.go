package waved

import (
	"context"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type failingRecoveryBackend struct {
	SwapBackend
	err     error
	partial *SwapRecoveryResult
}

// RecoverVHTLCs models an unavailable or older server after core wallet
// restore.
func (b *failingRecoveryBackend) RecoverVHTLCs(ctx context.Context) (
	*SwapRecoveryResult, error) {

	if _, ok := ctx.Deadline(); !ok {
		return nil, status.Error(
			codes.Internal, "missing recovery deadline",
		)
	}

	return b.partial, b.err
}

// TestOptionalSwapRecoveryPreservesResults checks core and partial counters
// survive backend outages and old servers without discovery support.
func TestOptionalSwapRecoveryPreservesResults(t *testing.T) {
	t.Parallel()

	for _, code := range []codes.Code{
		codes.Unavailable,
		codes.Unimplemented,
	} {
		backend := &failingRecoveryBackend{
			err: status.Error(code, "injected swap failure"),
			partial: &SwapRecoveryResult{
				RecoveredVHTLCs: 1,
			},
		}
		rpc := &RPCServer{server: &Server{
			cfg: &Config{
				Swap: &SwapConfig{
					Backend: backend,
				},
			},
			log: btclog.Disabled,
		}}
		result := &WalletRecoveryResult{VTXOs: 3, OOREvents: 2}
		rpc.recoverSwapVHTLCs(t.Context(), result)
		require.EqualValues(t, 3, result.VTXOs)
		require.EqualValues(t, 2, result.OOREvents)
		require.EqualValues(t, 1, result.VHTLCs)
	}
}
