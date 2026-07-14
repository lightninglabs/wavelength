//go:build systest

package systest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/vhtlcrecovery"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

//nolint:ll // Generated daemon RPC enum names are intentionally long.
const (
	systestRecoveryDirectionReceive = waverpc.VHTLCRecoveryDirection_VHTLC_RECOVERY_DIRECTION_RECEIVE
	systestRecoveryActionClaim      = waverpc.VHTLCRecoveryAction_VHTLC_RECOVERY_ACTION_CLAIM
	systestRecoveryStateArmed       = waverpc.VHTLCRecoveryState_VHTLC_RECOVERY_STATE_ARMED
	systestRecoveryStateCancelled   = waverpc.VHTLCRecoveryState_VHTLC_RECOVERY_STATE_CANCELLED
)

// TestVHTLCRecoveryRPCEndToEnd verifies that a full daemon can arm, read, and
// cancel a vHTLC recovery job through the public daemon RPC surface. The test
// intentionally stays at the recovery-control layer: unroll already owns the
// later materialization path, while this PR wires swap sessions to these RPCs.
func TestVHTLCRecoveryRPCEndToEnd(t *testing.T) {
	ParallelN(t)

	fixture := newDirectedSendFixture(t)
	armReq := testVHTLCRecoveryArmRequest(t, "recovery-rpc-request")

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	armResp, err := fixture.client.ArmVHTLCRecovery(ctx, armReq)
	require.NoError(t, err)
	require.True(t, armResp.GetCreated())
	require.NotEmpty(t, armResp.GetRecoveryId())
	requireVHTLCRecoveryStatusMatchesArm(
		t, armReq, armResp.GetRecoveryId(), armResp.GetStatus(),
	)

	retryResp, err := fixture.client.ArmVHTLCRecovery(ctx, armReq)
	require.NoError(t, err)
	require.False(t, retryResp.GetCreated())
	require.Equal(t, armResp.GetRecoveryId(), retryResp.GetRecoveryId())
	requireVHTLCRecoveryStatusMatchesArm(
		t, armReq, armResp.GetRecoveryId(), retryResp.GetStatus(),
	)

	conflictReq := protoCloneArmRequest(armReq)
	conflictReq.VtxoAmountSat++
	_, err = fixture.client.ArmVHTLCRecovery(ctx, conflictReq)
	require.Error(t, err)
	require.Equal(t, codes.AlreadyExists, status.Code(err))

	statusResp, err := fixture.client.GetVHTLCRecoveryStatus(
		ctx, &waverpc.GetVHTLCRecoveryStatusRequest{
			RecoveryId: armResp.GetRecoveryId(),
		},
	)
	require.NoError(t, err)
	require.True(t, statusResp.GetFound())
	requireVHTLCRecoveryStatusMatchesArm(
		t, armReq, armResp.GetRecoveryId(), statusResp.GetStatus(),
	)

	cooperativeTxid := chainhash.HashH(
		[]byte(t.Name() + "-cooperative-settlement"),
	).String()
	cancelResp, err := fixture.client.CancelVHTLCRecovery(
		ctx, &waverpc.CancelVHTLCRecoveryRequest{
			RecoveryId:      armResp.GetRecoveryId(),
			Reason:          "systest cooperative settlement won",
			CooperativeTxid: cooperativeTxid,
		},
	)
	require.NoError(t, err)

	cancelled := cancelResp.GetStatus()
	require.Equal(t, armResp.GetRecoveryId(), cancelled.GetRecoveryId())
	require.Equal(t, systestRecoveryStateCancelled, cancelled.GetState())
	require.Equal(
		t, "systest cooperative settlement won",
		cancelled.GetCancelReason(),
	)
	require.Equal(t, cooperativeTxid, cancelled.GetCooperativeTxid())
	require.NotZero(t, cancelled.GetTerminalAtUnix())
}

// testVHTLCRecoveryArmRequest returns a complete, deterministic recovery arm
// request with fake but structurally valid keys and outpoints.
func testVHTLCRecoveryArmRequest(t *testing.T,
	requestID string) *waverpc.ArmVHTLCRecoveryRequest {

	t.Helper()

	senderPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	receiverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	outpointHash := chainhash.HashH([]byte(t.Name() + "-vhtlc"))
	preimageHash := chainhash.HashH([]byte(t.Name() + "-preimage"))

	return &waverpc.ArmVHTLCRecoveryRequest{
		RequestId: requestID,
		SwapId:    []byte(t.Name() + "-swap"),
		Direction: systestRecoveryDirectionReceive,
		Action:    systestRecoveryActionClaim,
		VtxoOutpoint: fmt.Sprintf(
			"%s:%d", outpointHash.String(), 0,
		),
		VtxoAmountSat: 21_000,
		SenderPubkey: senderPriv.
			PubKey().
			SerializeCompressed(),
		ReceiverPubkey: receiverPriv.
			PubKey().
			SerializeCompressed(),
		ServerPubkey: serverPriv.
			PubKey().
			SerializeCompressed(),
		RefundLocktime: 500,
		PreimageHash:   preimageHash[:],
		SignerKeyFamily: int32(
			keychain.KeyFamilyMultiSig,
		),
		SignerKeyIndex: 7,
		DestinationScript: []byte{
			0x51,
		},
		MaxFeeRateSatPerKw:                   5_000,
		UnilateralClaimDelay:                 1,
		UnilateralRefundDelay:                2,
		UnilateralRefundWithoutReceiverDelay: 3,
	}
}

// requireVHTLCRecoveryStatusMatchesArm checks that the daemon persisted and
// returned the public recovery fields that the swap SDK/server retry around.
func requireVHTLCRecoveryStatusMatchesArm(t *testing.T,
	req *waverpc.ArmVHTLCRecoveryRequest, recoveryID string,
	status *waverpc.VHTLCRecoveryStatus) {

	t.Helper()

	require.NotNil(t, status)
	require.Equal(t, recoveryID, status.GetRecoveryId())
	require.Equal(t, req.GetRequestId(), status.GetRequestId())
	require.Equal(t, req.GetSwapId(), status.GetSwapId())
	require.Equal(t, req.GetDirection(), status.GetDirection())
	require.Equal(t, req.GetAction(), status.GetAction())
	require.Equal(t, systestRecoveryStateArmed, status.GetState())
	require.Equal(t, req.GetVtxoOutpoint(), status.GetVtxoOutpoint())
	require.Equal(t, req.GetVtxoAmountSat(), status.GetVtxoAmountSat())
	require.Equal(
		t, vhtlcrecovery.ExitPolicyKindClaim,
		status.GetExitPolicyKind(),
	)
	require.False(t, status.GetUnrollFound())
	require.NotZero(t, status.GetCreatedAtUnix())
	require.NotZero(t, status.GetUpdatedAtUnix())
	require.NotZero(t, status.GetArmedAtUnix())
	require.Zero(t, status.GetTerminalAtUnix())
}

// protoCloneArmRequest copies the request fields that participate in the
// recovery idempotency contract.
func protoCloneArmRequest(
	req *waverpc.ArmVHTLCRecoveryRequest,
) *waverpc.ArmVHTLCRecoveryRequest {

	refundNoReceiverDelay := req.UnilateralRefundWithoutReceiverDelay

	return &waverpc.ArmVHTLCRecoveryRequest{
		RequestId: req.RequestId,
		SwapId: append(
			[]byte(nil), req.SwapId...,
		),
		Direction:     req.Direction,
		Action:        req.Action,
		VtxoOutpoint:  req.VtxoOutpoint,
		VtxoAmountSat: req.VtxoAmountSat,
		SenderPubkey: append(
			[]byte(nil), req.SenderPubkey...,
		),
		ReceiverPubkey: append(
			[]byte(nil), req.ReceiverPubkey...,
		),
		ServerPubkey: append(
			[]byte(nil), req.ServerPubkey...,
		),
		RefundLocktime:                       req.RefundLocktime,
		UnilateralClaimDelay:                 req.UnilateralClaimDelay,
		UnilateralRefundDelay:                req.UnilateralRefundDelay,
		UnilateralRefundWithoutReceiverDelay: refundNoReceiverDelay,
		PreimageHash: append(
			[]byte(nil), req.PreimageHash...,
		),
		SignerKeyFamily: req.SignerKeyFamily,
		SignerKeyIndex:  req.SignerKeyIndex,
		DestinationScript: append(
			[]byte(nil), req.DestinationScript...,
		),
		MaxFeeRateSatPerKw: req.MaxFeeRateSatPerKw,
	}
}
