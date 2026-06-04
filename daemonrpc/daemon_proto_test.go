package daemonrpc

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
)

// requireProtoRoundTrip marshals original, unmarshals into a fresh value of
// the same type, and asserts the decoded message equals the original. It
// guards against stale generated descriptors dropping fields on the wire,
// covering all current and future fields without per-field assertions.
func requireProtoRoundTrip(t *testing.T, original proto.Message) {
	t.Helper()

	payload, err := proto.Marshal(original)
	require.NoError(t, err)

	decoded := original.ProtoReflect().New().Interface()
	require.NoError(t, proto.Unmarshal(payload, decoded))
	require.True(t, proto.Equal(original, decoded))
}

// TestDaemonProtoRoundTrip guards the response and recovery control-plane
// messages that swap SDKs and swap servers rely on across process
// boundaries against stale generated descriptors dropping fields.
func TestDaemonProtoRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		message proto.Message
	}{
		{
			name: "send vtxo response",
			message: &SendVTXOResponse{
				Status:          "submitted",
				TotalAmountSat:  40_000,
				ChangeAmountSat: 59_000,
				SelectedCount:   1,
			},
		},
		{
			name: "vhtlc recovery status",
			message: &VHTLCRecoveryStatus{
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
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			requireProtoRoundTrip(t, tc.message)
		})
	}
}
