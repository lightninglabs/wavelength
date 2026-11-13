package swaps

import (
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightninglabs/wavelength/swaprpc"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newRefundRecoveryFixture builds one owned live refund row for review
// regressions.
func newRefundRecoveryFixture(t *testing.T) (*SwapClient, *testDaemonConn,
	*testSwapServerConn, *swaprpc.RecoverableSwap) {

	t.Helper()
	owner, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	other, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	row := recoverableSwapForTest(
		t,
		swaprpc.RecoverableSwapDirection_RECOVERABLE_SWAP_DIRECTION_IN,
		lntypes.Hash(
			testHash(171),
		),
		owner.PubKey(),
		other.PubKey(),
		other.PubKey(),
		nil,
	)
	daemon := &testDaemonConn{
		identityPriv: owner, identityKey: owner.PubKey(),
		receiveInfo: &ReceiveInfo{
			PkScript: []byte{
				0x51,
			},
			PubKeyXOnly: schnorr.SerializePubKey(owner.PubKey()),
		},
		liveByPkScript: map[string]*VTXOInfo{
			hex.EncodeToString(row.GetVhtlcPkScript()): {
				Outpoint: "owned:0", AmountSat: 42_000,
			},
		},
	}
	server := &testSwapServerConn{
		recoverableRows: []*swaprpc.RecoverableSwap{
			row,
		},
	}

	return newRecoveryClient(t, server, daemon), daemon, server, row
}

// newRecoveryClient gives restore tests durable storage so they can resume the
// reconstructed settlement workers instead of merely counting dormant jobs.
func newRecoveryClient(t *testing.T, server SwapServerConn,
	daemon DaemonConn) *SwapClient {

	t.Helper()

	client := NewSwapClientWithStore(
		server, daemon, nil, nil, newTestSwapStore(t),
	)
	client.SetChainParams(&chaincfg.RegressionNetParams)

	return client
}

// TestRecoveryContinuesAfterIndexerFailure preserves successful row results and
// reports an aggregate retryable error instead of stopping at the first row.
func TestRecoveryContinuesAfterIndexerFailure(t *testing.T) {
	t.Parallel()

	client, daemon, server, row := newRefundRecoveryFixture(t)
	server.recoverableRows = append(server.recoverableRows, row)
	daemon.liveLookupHook = func(call int) (*VTXOInfo, error) {
		if call == 1 {
			return nil, status.Error(
				codes.Unavailable, "injected indexer failure",
			)
		}

		return &VTXOInfo{Outpoint: "owned:0", AmountSat: 42_000}, nil
	}
	result, err := client.RecoverSwapserverVHTLCs(t.Context())
	require.ErrorContains(t, err, "injected indexer failure")
	require.EqualValues(t, 1, result.RecoveredVHTLCRefunds)
	require.Equal(t, 2, daemon.liveLookupCalls)
	require.Equal(t, 1, daemon.armRecoveryCalls)
}

// TestRecoverySupportsLegacySwapserver treats an absent discovery API as an
// optional capability, without hiding other discovery failures.
func TestRecoverySupportsLegacySwapserver(t *testing.T) {
	t.Parallel()

	client, _, server, _ := newRefundRecoveryFixture(t)
	server.recoverableErr = status.Error(codes.Unimplemented, "old server")
	result, err := client.RecoverSwapserverVHTLCs(t.Context())
	require.NoError(t, err)
	require.Zero(t, result.RecoveredVHTLCs)
}

// TestRecoveryRejectsForeignParticipants rejects internally valid scripts that
// cannot be signed by this wallet, before allocating destinations or jobs.
func TestRecoveryRejectsForeignParticipants(t *testing.T) {
	t.Parallel()

	for _, direction := range []swaprpc.RecoverableSwapDirection{
		swaprpc.RecoverableSwapDirection_RECOVERABLE_SWAP_DIRECTION_IN,
		swaprpc.RecoverableSwapDirection_RECOVERABLE_SWAP_DIRECTION_OUT,
	} {
		client, daemon, server, _ := newRefundRecoveryFixture(t)
		foreign, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		server.recoverableRows = []*swaprpc.RecoverableSwap{
			recoverableSwapForTest(
				t, direction,
				lntypes.Hash(
					testHash(172),
				),
				foreign.PubKey(),
				foreign.PubKey(),
				foreign.PubKey(),
				nil,
			),
		}
		result, err := client.RecoverSwapserverVHTLCs(t.Context())
		require.NoError(t, err)
		require.Zero(t, result.RecoveredVHTLCs)
		require.Zero(t, daemon.liveLookupCalls)
		require.Zero(t, daemon.receiveAllocCalls)
		require.Zero(t, daemon.armRecoveryCalls)
	}
}

// TestRecoveredClaimRespectsPolicy covers default opt-out, grace, deadline
// pressure, explicit opt-in, and durable local session ownership on retries.
func TestRecoveredClaimRespectsPolicy(t *testing.T) {
	t.Parallel()
	direction := swaprpc.
		RecoverableSwapDirection_RECOVERABLE_SWAP_DIRECTION_OUT

	for _, test := range []struct {
		name         string
		auto         bool
		grace        time.Duration
		height       uint32
		pending      bool
		existing     bool
		wantEscalate int
	}{
		{
			name: "default off",
		},
		{
			name:         "explicit opt in",
			auto:         true,
			wantEscalate: 1,
		},
		{
			name:  "grace",
			auto:  true,
			grace: time.Hour,
		},
		{
			name:         "latest start margin",
			auto:         true,
			grace:        time.Hour,
			height:       50,
			wantEscalate: 1,
		},
		{
			name:   "closed claim window",
			auto:   true,
			height: 144,
		},
		{
			name:    "local new row",
			auto:    true,
			pending: true,
		},
		{
			name:     "local armed row",
			auto:     true,
			pending:  true,
			existing: true,
		},
		{
			name:     "existing off",
			existing: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, daemon, server, inRow :=
				newRefundRecoveryFixture(t)
			preimage, params, err := newOutSwapPreimage(
				t.Context(), daemon,
			)
			require.NoError(t, err)
			other, err := btcec.ParsePubKey(
				inRow.GetReceiverPubkey(),
			)
			require.NoError(t, err)
			row := recoverableSwapForTest(
				t, direction, preimage.Hash(), other,
				daemon.identityKey, other, params,
			)
			live := &VTXOInfo{
				Outpoint:  "claim:0",
				AmountSat: 42_000,
			}
			daemon.liveByPkScript[hex.EncodeToString(
				row.GetVhtlcPkScript(),
			)] = live
			daemon.blockHeight = test.height
			server.recoverableRows = []*swaprpc.RecoverableSwap{row}
			client.SetRecoveryPolicy(RecoveryPolicy{
				AutoEscalate:                  test.auto,
				CooperativeFailureGracePeriod: test.grace,
			})
			if test.existing {
				stored := recoverableStatusForTest(
					row, preimage.Hash(), live, "existing",
					recoveryDirectionReceive,
					recoveryActionClaim,
				)
				response := new(
					waverpc.ListVHTLCRecoveriesResponse,
				)
				response.Statuses = append(
					response.Statuses, stored,
				)
				daemon.listRecoveriesResp = response
			}
			if test.pending {
				client.store = newTestSwapStore(t)
				session := &ReceiveSession{
					client:      client,
					PaymentHash: preimage.Hash(),
					Preimage:    preimage,
					state:       ReceiveStateVHTLCFunded,
				}
				require.NoError(t, session.persist(t.Context()))
			}
			result, err := client.RecoverSwapserverVHTLCs(
				t.Context(),
			)
			require.NoError(t, err)
			require.Zero(t, daemon.escalateCalls)
			if test.pending {
				require.Zero(t, result.RecoveredVHTLCClaims)

				return
			}
			require.EqualValues(t, 1, result.RecoveredVHTLCClaims)
			session, err := client.ResumeReceiveViaLightning(
				t.Context(), preimage.Hash(),
			)
			require.NoError(t, err)
			require.Equal(
				t, ReceiveStateVHTLCFunded, session.State(),
			)
			require.NoError(
				t,
				session.maybeEscalateReceiveClaimRecovery(
					t.Context(),
					errors.New("cooperative claim "+
						"unavailable"),
				),
			)
			require.Equal(
				t, test.wantEscalate, daemon.escalateCalls,
			)
		})
	}
}
