package waved

import (
	"context"
	"errors"
	"testing"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// resolvedSignal returns a readiness signal that has already resolved, for
// tests that hand the server a wallet that is ready, or a running wallet
// actor.
func resolvedSignal() chan struct{} {
	ready := make(chan struct{})
	close(ready)

	return ready
}

// spawnIdleWalletActor registers a wallet actor for tests that only check
// whether the RPC layer may reach it; they never send it a message.
func spawnIdleWalletActor(
	t *testing.T) actor.ActorRef[wallet.WalletMsg, wallet.WalletResp] {

	t.Helper()

	system := actor.NewActorSystem()
	t.Cleanup(func() {
		require.NoError(t, system.Shutdown(context.Background()))
	})
	key := actor.NewServiceKey[wallet.WalletMsg, wallet.WalletResp](
		"idle-wallet",
	)

	return key.Spawn(system, "idle-wallet", &batchLeaveWallet{})
}

// TestWalletActorWaitsForWalletServices pins that the RPC layer reaches the
// wallet actor only after the wallet-dependent services resolved. The ref is
// written by the startup goroutine; until the services signal resolves, a
// handler must not read it and must report a retryable condition.
func TestWalletActorWaitsForWalletServices(t *testing.T) {
	t.Parallel()

	ref := spawnIdleWalletActor(t)

	tests := []struct {
		name string

		// resolve resolves the services signal with this error; nil
		// leaves it unresolved.
		resolve fn.Option[error]

		walletRef fn.Option[actor.ActorRef[
			wallet.WalletMsg, wallet.WalletResp,
		]]

		wantRef bool
	}{{
		name:      "services still starting",
		walletRef: fn.Some(ref),
	}, {
		name:      "services failed",
		resolve:   fn.Some(errors.New("mailbox unreachable")),
		walletRef: fn.Some(ref),
	}, {
		name:    "services ready without an actor",
		resolve: fn.Some[error](nil),
	}, {
		name:      "services ready",
		resolve:   fn.Some[error](nil),
		walletRef: fn.Some(ref),
		wantRef:   true,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server := &Server{
				walletServicesReady: make(chan struct{}),
				walletRef:           tc.walletRef,
			}
			tc.resolve.WhenSome(server.markWalletServicesReady)
			rpc := NewRPCServer(server)

			got, err := rpc.walletActor()
			if tc.wantRef {
				require.NoError(t, err)
				require.Equal(t, ref, got)

				return
			}

			require.Equal(t, codes.Unavailable, status.Code(err))
			require.Nil(t, got)
		})
	}
}

// TestNewAddressBeforeWalletServicesIsRetryable pins the window between an
// unlocked wallet and its running wallet actor: the wallet already reports
// ready, so a caller asks for an address, and must get a retryable code
// rather than an internal error it cannot tell from a real failure.
func TestNewAddressBeforeWalletServicesIsRetryable(t *testing.T) {
	t.Parallel()

	rpc := NewRPCServer(&Server{
		walletReady:         resolvedSignal(),
		walletServicesReady: make(chan struct{}),
	})

	_, err := rpc.NewAddress(
		context.Background(), &waverpc.NewAddressRequest{},
	)
	require.Equal(t, codes.Unavailable, status.Code(err))
}

// TestWaitForWalletServices pins what InitWallet and UnlockWallet return once
// the wallet itself is open: success only after the wallet-dependent services
// started, a retryable code naming what already succeeded when they failed,
// the context's code when the caller gave up first, and no wait at all for a
// wallet that is still syncing.
func TestWaitForWalletServices(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string

		// walletReady is false for a wallet that is still syncing.
		walletReady bool

		// resolve resolves the services signal with this error; None
		// leaves it unresolved.
		resolve fn.Option[error]

		// cancel cancels the caller's context before waiting.
		cancel bool

		wantCode codes.Code
	}{{
		name:        "services ready",
		walletReady: true,
		resolve:     fn.Some[error](nil),
		wantCode:    codes.OK,
	}, {
		name:        "services failed",
		walletReady: true,
		resolve:     fn.Some(errors.New("mailbox unreachable")),
		wantCode:    codes.Unavailable,
	}, {
		name:        "caller gave up",
		walletReady: true,
		cancel:      true,
		wantCode:    codes.Canceled,
	}, {
		name:     "wallet still syncing",
		wantCode: codes.OK,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server := &Server{
				walletReady:         make(chan struct{}),
				walletServicesReady: make(chan struct{}),
			}
			if tc.walletReady {
				close(server.walletReady)
			}
			tc.resolve.WhenSome(server.markWalletServicesReady)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancel {
				cancel()
			}

			err := NewRPCServer(server).waitForWalletServices(
				ctx, "wallet unlocked",
			)
			require.Equal(t, tc.wantCode, status.Code(err))
			if err != nil {
				require.Contains(
					t, err.Error(),
					"wallet unlocked",
				)
			}
		})
	}
}
