package swaps

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightninglabs/wavelength/swaprpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GetSwapRecoveryInfo supplies an authenticated test audience for SDK scans.
func (c *testSwapServerConn) GetSwapRecoveryInfo(context.Context) (
	*swaprpc.GetSwapRecoveryInfoResponse, error) {

	_, key := btcec.PrivKeyFromBytes([]byte{2})

	return &swaprpc.GetSwapRecoveryInfoResponse{
		ServerIdentityPubkey: key.SerializeCompressed(),
		NetworkGenesisHash: append(
			[]byte(nil),
			chaincfg.RegressionNetParams.GenesisHash[:]...,
		),
	}, nil
}

// TestRestoreQueuedCancellation bounds wallet recovery even when another
// caller owns the scanner. Cancelling must not wait for that scan's RPCs.
func TestRestoreQueuedCancellation(t *testing.T) {
	t.Parallel()
	client, _, _, _ := newRefundRecoveryFixture(t)
	client.restoreGate <- struct{}{}
	defer func() {
		<-client.restoreGate
	}()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err := client.RecoverSwapserverVHTLCs(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, result.RecoveredVHTLCs)
}

// TestRestoreRejectsEmptyContinuation bounds malformed servers that never
// supply candidates but keep returning fresh cursors and requesting signatures.
func TestRestoreRejectsEmptyContinuation(t *testing.T) {
	t.Parallel()
	client, daemon, base, _ := newRefundRecoveryFixture(t)
	server := &recoveryPagedServer{
		testSwapServerConn: base,
		pages: []*swaprpc.ListRecoverableSwapsResponse{
			{
				NextPageToken: []byte{
					1,
				},
			},
		},
	}
	client.server = server
	result, err := client.RecoverSwapserverVHTLCs(t.Context())
	require.ErrorContains(t, err, "empty recovery page")
	require.Zero(t, result.RecoveredVHTLCs)
	require.Len(t, server.requested, 1)
	require.Zero(t, daemon.armRecoveryCalls)
}

// TestRestoreCancellationPreservesRowErrors retains actionable earlier failures
// even when a later candidate observes cancellation partway through its lookup.
func TestRestoreCancellationPreservesRowErrors(t *testing.T) {
	t.Parallel()
	client, daemon, server, row := newRefundRecoveryFixture(t)
	server.recoverableRows = []*swaprpc.RecoverableSwap{row, row}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	lookupErr := status.Error(codes.Unavailable, "earlier lookup failure")
	daemon.liveLookupHook = func(call int) (*VTXOInfo, error) {
		if call == 1 {
			return nil, lookupErr
		}
		cancel()

		return nil, context.Canceled
	}
	result, err := client.RecoverSwapserverVHTLCs(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorContains(t, err, "earlier lookup failure")
	require.Zero(t, result.RecoveredVHTLCs)
}

// ListRecoverableSwapsPage adapts existing one-page fixtures without changing
// the production scanner's bounded authenticated transport contract.
func (c *testSwapServerConn) ListRecoverableSwapsPage(ctx context.Context,
	proof *swaprpc.SwapOwnerProof, _ uint32, _ []byte) (
	*swaprpc.ListRecoverableSwapsResponse, error) {

	rows, err := c.ListRecoverableSwaps(ctx, proof)

	return &swaprpc.ListRecoverableSwapsResponse{Swaps: rows}, err
}

// recoveryPagedServer scripts continuations and audience corruption without
// introducing an unbounded list into the production connection interface.
type recoveryPagedServer struct {
	*testSwapServerConn
	pages     []*swaprpc.ListRecoverableSwapsResponse
	pageErr   error
	info      *swaprpc.GetSwapRecoveryInfoResponse
	requested [][]byte
}

// GetSwapRecoveryInfo can advertise the wrong network for rejection tests.
func (s *recoveryPagedServer) GetSwapRecoveryInfo(ctx context.Context) (
	*swaprpc.GetSwapRecoveryInfoResponse, error) {

	if s.info != nil {
		return s.info, nil
	}

	return s.testSwapServerConn.GetSwapRecoveryInfo(ctx)
}

// ListRecoverableSwapsPage records the exact signed cursor for every page.
func (s *recoveryPagedServer) ListRecoverableSwapsPage(_ context.Context,
	_ *swaprpc.SwapOwnerProof, _ uint32, token []byte) (
	*swaprpc.ListRecoverableSwapsResponse, error) {

	s.requested = append(s.requested, append([]byte(nil), token...))
	if len(s.pages) == 0 {
		return nil, s.pageErr
	}
	page := s.pages[0]
	s.pages = s.pages[1:]

	return page, nil
}

// TestRestoreScansBoundedPages ensures live rows beyond the first page are
// restored and repeated/malicious continuations cannot loop indefinitely.
func TestRestoreScansBoundedPages(t *testing.T) {
	t.Parallel()
	for _, repeated := range []bool{false, true} {
		name := "complete"
		if repeated {
			name = "repeated token"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client, daemon, base, row := newRefundRecoveryFixture(t)
			last := &swaprpc.ListRecoverableSwapsResponse{
				Swaps: []*swaprpc.RecoverableSwap{
					row,
				},
			}
			if repeated {
				last.NextPageToken = []byte{1}
			}
			first := &swaprpc.ListRecoverableSwapsResponse{
				Swaps: []*swaprpc.RecoverableSwap{
					{},
				},
				NextPageToken: []byte{
					1,
				},
			}
			server := &recoveryPagedServer{
				testSwapServerConn: base,
				pages: []*swaprpc.ListRecoverableSwapsResponse{
					first, last,
				},
			}
			client.server = server
			result, err := client.RecoverSwapserverVHTLCs(
				t.Context(),
			)
			if repeated {
				require.ErrorContains(
					t, err, "repeated recovery page token",
				)
			} else {
				require.NoError(t, err)
			}
			require.EqualValues(t, 1, result.RecoveredVHTLCRefunds)
			require.Len(t, server.requested, 2)
			require.Equal(t, []byte{1}, server.requested[1])
			require.Equal(t, 1, daemon.armRecoveryCalls)
		})
	}
}

// TestRestoreRejectsWrongNetwork refuses to sign for a different Bitcoin chain.
func TestRestoreRejectsWrongNetwork(t *testing.T) {
	t.Parallel()
	client, daemon, base, _ := newRefundRecoveryFixture(t)
	info, err := base.GetSwapRecoveryInfo(t.Context())
	require.NoError(t, err)
	info.NetworkGenesisHash = chaincfg.MainNetParams.GenesisHash[:]
	server := &recoveryPagedServer{testSwapServerConn: base, info: info}
	client.server = server
	_, err = client.RecoverSwapserverVHTLCs(t.Context())
	require.ErrorContains(t, err, "network mismatch")
	require.Empty(t, server.requested)
	require.Zero(t, daemon.armRecoveryCalls)
}
