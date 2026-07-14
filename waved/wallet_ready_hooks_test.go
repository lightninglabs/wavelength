package waved

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRunWalletReadyHooksRunsInOrder verifies optional subservers can depend
// on deterministic wallet-ready hook ordering.
func TestRunWalletReadyHooksRunsInOrder(t *testing.T) {
	t.Parallel()

	var calls []int
	s := &Server{
		cfg: &Config{
			WalletReadyHooks: []WalletReadyHook{
				func(context.Context) error {
					calls = append(calls, 1)

					return nil
				},
				nil,
				func(context.Context) error {
					calls = append(calls, 2)

					return nil
				},
			},
		},
	}

	require.NoError(t, s.runWalletReadyHooks(t.Context()))
	require.Equal(t, []int{1, 2}, calls)
}

// TestRunWalletReadyHooksPropagatesError verifies startup fails closed when a
// wallet-ready hook cannot start its post-unlock background work.
func TestRunWalletReadyHooksPropagatesError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("resume failed")
	s := &Server{
		cfg: &Config{
			WalletReadyHooks: []WalletReadyHook{
				func(context.Context) error {
					return wantErr
				},
			},
		},
	}

	err := s.runWalletReadyHooks(t.Context())
	require.ErrorIs(t, err, wantErr)
	require.ErrorContains(t, err, "wallet-ready hook")
}
