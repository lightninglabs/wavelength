package waverpc

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

//nolint:ll // Generated protobuf enum names are intentionally long.
const (
	testRecoveryDirection = VHTLCRecoveryDirection_VHTLC_RECOVERY_DIRECTION_RECEIVE
	testRecoveryAction    = VHTLCRecoveryAction_VHTLC_RECOVERY_ACTION_CLAIM
	testRecoveryState     = VHTLCRecoveryState_VHTLC_RECOVERY_STATE_UNROLL_STARTED
	testUnrollStatus      = UnrollJobStatus_UNROLL_JOB_STATUS_MATERIALIZING
	testExpiryStatus      = VTXOExpiryStatus_VTXO_EXPIRY_STATUS_NEEDS_REFRESH
)

// TestSendVTXOResponseProtoRoundTrip guards against stale generated
// descriptors dropping newly added response fields on the wire.
func TestSendVTXOResponseProtoRoundTrip(t *testing.T) {
	t.Parallel()

	original := &SendVTXOResponse{
		Status:          "submitted",
		TotalAmountSat:  40_000,
		ChangeAmountSat: 59_000,
		SelectedCount:   1,
	}

	payload, err := proto.Marshal(original)
	require.NoError(t, err)

	var decoded SendVTXOResponse
	require.NoError(t, proto.Unmarshal(payload, &decoded))
	require.Equal(t, original.Status, decoded.Status)
	require.Equal(t, original.TotalAmountSat, decoded.TotalAmountSat)
	require.Equal(t, original.ChangeAmountSat, decoded.ChangeAmountSat)
	require.Equal(t, original.SelectedCount, decoded.SelectedCount)
}

// TestTaprootAssetInputOutpointProtoRoundTrip guards the managed VTXO selector
// that replaces the custom-input bypass for asset OOR transfers.
func TestTaprootAssetInputOutpointProtoRoundTrip(t *testing.T) {
	t.Parallel()

	original := &TaprootAssetOORIntent{
		InputVtxoOutpoint: "0000000000000000000000000000000000000000" +
			"000000000000000000000000:1",
	}

	payload, err := proto.Marshal(original)
	require.NoError(t, err)

	var decoded TaprootAssetOORIntent
	require.NoError(t, proto.Unmarshal(payload, &decoded))
	require.Equal(
		t, original.GetInputVtxoOutpoint(),
		decoded.GetInputVtxoOutpoint(),
	)
}

// TestVTXOExpiryInfoProtoRoundTrip guards the expiry posture fields that swap
// services use to decide whether a VTXO is safe to build a swap around.
func TestVTXOExpiryInfoProtoRoundTrip(t *testing.T) {
	t.Parallel()

	original := &GetVTXOExpiryInfoResponse{
		Found: true,
		ExpiryInfo: &VTXOExpiryInfo{
			Status:                  testExpiryStatus,
			CurrentHeight:           784,
			BatchExpiry:             1000,
			BlocksRemaining:         216,
			RefreshThresholdBlocks:  234,
			CriticalThresholdBlocks: 162,
			RelativeExpiry:          144,
			MaxTreeDepth:            3,
			ChainDepth:              2,
		},
		Vtxo: &VTXO{
			Outpoint:       "00:1",
			AmountSat:      42_000,
			Status:         VTXOStatus_VTXO_STATUS_LIVE,
			BatchExpiry:    1000,
			RelativeExpiry: 144,
			ExpiryInfo: &VTXOExpiryInfo{
				Status:          testExpiryStatus,
				BlocksRemaining: 216,
			},
		},
	}

	payload, err := proto.Marshal(original)
	require.NoError(t, err)

	var decoded GetVTXOExpiryInfoResponse
	require.NoError(t, proto.Unmarshal(payload, &decoded))
	require.True(t, decoded.GetFound())
	require.Equal(t, testExpiryStatus, decoded.GetExpiryInfo().GetStatus())
	require.Equal(t, int32(784), decoded.GetExpiryInfo().GetCurrentHeight())
	require.Equal(t, int32(1000), decoded.GetExpiryInfo().GetBatchExpiry())
	require.Equal(
		t, int32(216), decoded.GetExpiryInfo().GetBlocksRemaining(),
	)
	require.Equal(
		t, int32(234),
		decoded.GetExpiryInfo().GetRefreshThresholdBlocks(),
	)
	require.Equal(
		t, int32(162),
		decoded.GetExpiryInfo().GetCriticalThresholdBlocks(),
	)
	require.Equal(
		t, uint32(144), decoded.GetExpiryInfo().GetRelativeExpiry(),
	)
	require.Equal(t, uint32(3), decoded.GetExpiryInfo().GetMaxTreeDepth())
	require.Equal(t, uint32(2), decoded.GetExpiryInfo().GetChainDepth())
	require.Equal(t, "00:1", decoded.GetVtxo().GetOutpoint())
	require.Equal(
		t, testExpiryStatus,
		decoded.GetVtxo().GetExpiryInfo().GetStatus(),
	)
}

// TestVHTLCRecoveryStatusProtoRoundTrip guards the recovery control-plane
// fields that swap SDKs and swap servers rely on across process boundaries.
func TestVHTLCRecoveryStatusProtoRoundTrip(t *testing.T) {
	t.Parallel()

	original := &VHTLCRecoveryStatus{
		RecoveryId:      "recovery-1",
		RequestId:       "request-1",
		SwapId:          []byte("swap-1"),
		Direction:       testRecoveryDirection,
		Action:          testRecoveryAction,
		State:           testRecoveryState,
		VtxoOutpoint:    "00:1",
		VtxoAmountSat:   50_000,
		ExitPolicyKind:  "vhtlc_claim",
		UnrollFound:     true,
		UnrollStatus:    testUnrollStatus,
		SweepTxid:       "sweep",
		ExitTxid:        "exit",
		CooperativeTxid: "coop",
		LastError:       "last error",
		CancelReason:    "cooperative completed",
		CreatedAtUnix:   1,
		UpdatedAtUnix:   2,
		ArmedAtUnix:     3,
		EscalatedAtUnix: 4,
		TerminalAtUnix:  5,
	}

	payload, err := proto.Marshal(original)
	require.NoError(t, err)

	var decoded VHTLCRecoveryStatus
	require.NoError(t, proto.Unmarshal(payload, &decoded))
	require.Equal(t, original.RecoveryId, decoded.RecoveryId)
	require.Equal(t, original.RequestId, decoded.RequestId)
	require.Equal(t, original.SwapId, decoded.SwapId)
	require.Equal(t, original.Direction, decoded.Direction)
	require.Equal(t, original.Action, decoded.Action)
	require.Equal(t, original.State, decoded.State)
	require.Equal(t, original.VtxoOutpoint, decoded.VtxoOutpoint)
	require.Equal(t, original.VtxoAmountSat, decoded.VtxoAmountSat)
	require.Equal(t, original.ExitPolicyKind, decoded.ExitPolicyKind)
	require.Equal(t, original.UnrollFound, decoded.UnrollFound)
	require.Equal(t, original.UnrollStatus, decoded.UnrollStatus)
	require.Equal(t, original.SweepTxid, decoded.SweepTxid)
	require.Equal(t, original.ExitTxid, decoded.ExitTxid)
	require.Equal(t, original.CooperativeTxid, decoded.CooperativeTxid)
	require.Equal(t, original.LastError, decoded.LastError)
	require.Equal(t, original.CancelReason, decoded.CancelReason)
	require.Equal(t, original.CreatedAtUnix, decoded.CreatedAtUnix)
	require.Equal(t, original.UpdatedAtUnix, decoded.UpdatedAtUnix)
	require.Equal(t, original.ArmedAtUnix, decoded.ArmedAtUnix)
	require.Equal(t, original.EscalatedAtUnix, decoded.EscalatedAtUnix)
	require.Equal(t, original.TerminalAtUnix, decoded.TerminalAtUnix)
}
